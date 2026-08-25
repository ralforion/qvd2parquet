package convert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// ErrInput marks an input read/decode failure (CLI exit code 4).
var ErrInput = errors.New("input error")

// DecodeChunk is one contiguous range of records assigned to a worker.
type DecodeChunk struct {
	Index      int64
	StartRow   int64
	RowCount   int
	ByteOffset int64
}

// DecodeResult is a finished chunk handed to the writer goroutine.
type DecodeResult struct {
	Chunk   DecodeChunk
	Record  arrow.Record
	Metrics *Metrics
}

// RecordSink consumes finished chunks. Implementations are called from a
// single goroutine and need not be safe for concurrent use.
type RecordSink interface {
	Write(arrow.Record) error
}

// Chunks splits the record area into contiguous work items of at most
// batchRows rows each.
func Chunks(rows int64, batchRows int, recordSize int, recordStart int64) []DecodeChunk {
	if rows <= 0 || batchRows <= 0 {
		return nil
	}
	n := (rows + int64(batchRows) - 1) / int64(batchRows)
	out := make([]DecodeChunk, 0, n)
	for i := int64(0); i < n; i++ {
		start := i * int64(batchRows)
		count := int64(batchRows)
		if start+count > rows {
			count = rows - start
		}
		out = append(out, DecodeChunk{
			Index:      i,
			StartRow:   start,
			RowCount:   int(count),
			ByteOffset: recordStart + start*int64(recordSize),
		})
	}
	return out
}

// MinDefaultWorkers is the floor under the automatic worker count, so a small
// machine still decodes in parallel.
const MinDefaultWorkers = 2

// DefaultWorkers is the worker count --workers=0 resolves to: one per two
// CPUs, never fewer than MinDefaultWorkers.
//
// It is deliberately below runtime.NumCPU(). Decoding itself scales close to
// linearly, but it is only half the pipeline -- the Parquet writer is a single
// goroutine -- and every worker costs its share of in-flight Arrow memory,
// which on a wide file is roughly workers * batch-rows * columns * 16 bytes
// and dominates resident size. Half the CPUs keeps most of the decode
// throughput at half the batches in flight.
//
// The divisor applies to logical CPUs, so on a hyper-threaded machine this is
// about one worker per physical core.
func DefaultWorkers() int {
	n := runtime.NumCPU() / 2
	if n < MinDefaultWorkers {
		n = MinDefaultWorkers
	}
	return n
}

// WorkerCount resolves the --workers option.
func WorkerCount(requested int, chunks int) int {
	n := requested
	if n <= 0 {
		n = DefaultWorkers()
	}
	if chunks > 0 && n > chunks {
		n = chunks
	}
	if n < 1 {
		n = 1
	}
	return n
}

// ProgressFunc is called with the cumulative number of rows written.
type ProgressFunc func(rows int64)

// ReorderWindowFactor sets how many chunks may be outstanding per worker
// before the feeder stops issuing work. Records are written in chunk order, so
// a chunk that finishes early waits in a reorder buffer for its predecessors;
// the window is what bounds that buffer. Two per worker lets a worker pick up
// its next chunk while its last one waits, without letting a straggler pull an
// unbounded number of finished records into memory behind it.
const ReorderWindowFactor = 2

// Run decodes every record in parallel and streams the resulting Arrow records
// into sink in chunk order, so the Parquet file holds the QVD's rows in their
// original order. Decoding still runs in parallel; only the handoff to the
// writer is sequenced.
func (c *Converter) Run(ctx context.Context, sink RecordSink, progress ProgressFunc) (*Metrics, error) {
	f := c.File
	chunks := Chunks(f.NoOfRecords, c.BatchRows, f.RecordByteSize, f.RecordStart)
	total := NewMetrics(c.Schema)

	if len(chunks) == 0 {
		return total, nil
	}

	workers := WorkerCount(c.Options.Workers, len(chunks))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	work := make(chan DecodeChunk)
	results := make(chan DecodeResult, workers)

	// The writer emits chunks in order, so one that finishes ahead of its
	// predecessors waits in pending. window bounds how far ahead the decoders
	// may run, and so bounds pending: the feeder takes a slot per chunk and
	// the writer returns one per record written.
	//
	// Without it, a slow chunk 0 would let every other worker race ahead and
	// pile finished records into pending without limit; and a writer that
	// simply stopped draining results while it waited would fill that channel
	// and deadlock chunk 0 when it finally tried to hand its record over.
	window := workers * ReorderWindowFactor
	if window > len(chunks) {
		window = len(chunks)
	}
	slots := make(chan struct{}, window)

	var wg sync.WaitGroup
	// firstErr is written by whichever worker fails first and read by the
	// writer loop below, so both sides must hold errMu.
	var errMu sync.Mutex
	var firstErr error
	fail := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errMu.Unlock()
	}
	failed := func() bool {
		errMu.Lock()
		defer errMu.Unlock()
		return firstErr != nil
	}

	// Feeder.
	go func() {
		defer close(work)
		for _, ch := range chunks {
			// Take a window slot first: this is the backpressure that keeps
			// the reorder buffer bounded.
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case work <- ch:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Workers.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := c.newWorker()
			defer w.release()
			for ch := range work {
				if ctx.Err() != nil {
					return
				}
				res, err := w.decodeChunk(ch)
				if err != nil {
					fail(err)
					return
				}
				if c.onDecoded != nil {
					c.onDecoded(ch)
				}
				select {
				case results <- res:
				case <-ctx.Done():
					res.Record.Release()
					return
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	// Single writer goroutine: this goroutine. The Parquet writer is not safe
	// for concurrent use, so only decoding runs in parallel.
	//
	// Results arrive in completion order and are written in chunk order. A
	// chunk that arrives early waits in pending until its predecessors have
	// been written; the feeder's window bounds how many can be waiting.
	var written, lastReported int64
	nextProgress := c.Options.ProgressEvery
	pending := make(map[int64]DecodeResult, window)
	nextChunk := int64(0)
	c.maxPending = 0

	writeOne := func(res DecodeResult) {
		if !failed() {
			if err := sink.Write(res.Record); err != nil {
				fail(err)
			} else {
				total.Merge(res.Metrics)
				written += res.Record.NumRows()
				if progress != nil && c.Options.ProgressEvery > 0 && written >= nextProgress {
					progress(written)
					lastReported = written
					for written >= nextProgress {
						nextProgress += c.Options.ProgressEvery
					}
				}
			}
		}
		res.Record.Release()
		// Return the window slot only once the record is gone, so the feeder
		// cannot issue a replacement while this one is still held.
		<-slots
	}

	for res := range results {
		pending[res.Chunk.Index] = res
		if len(pending) > c.maxPending {
			c.maxPending = len(pending)
		}
		for {
			res, ok := pending[nextChunk]
			if !ok {
				break
			}
			delete(pending, nextChunk)
			nextChunk++
			writeOne(res)
		}
	}
	// A cancelled or failed run can leave chunks that will never be written,
	// because the chunk before them never arrived. Their records still hold
	// Arrow buffers.
	for _, res := range pending {
		res.Record.Release()
	}

	errMu.Lock()
	runErr := firstErr
	errMu.Unlock()
	if runErr != nil {
		return nil, runErr
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return nil, err
	}
	if written != f.NoOfRecords {
		return nil, fmt.Errorf("%w: wrote %d rows but the header declares %d", ErrInput, written, f.NoOfRecords)
	}
	if progress != nil && c.Options.ProgressEvery > 0 && written != lastReported {
		progress(written)
	}
	return total, nil
}

// worker holds the per-goroutine state: its own Arrow builders, record buffer
// and scratch slices. Nothing here is shared between goroutines.
type worker struct {
	c    *Converter
	file *os.File
	// own is this worker's private handle on the input, closed on release.
	// It is nil when the worker shares the File's handle instead.
	own    *os.File
	batch  *Batch
	raw    []byte
	symIdx []int64
	hash   bool
	mem    memory.Allocator
}

// openWorkerFile opens a private handle on the QVD for one decode worker. It
// returns nil when the worker should fall back to sharing the File's handle.
//
// Windows serializes concurrent ReadAt on a single *os.File: internal/poll's
// Pread takes that descriptor's read and write locks and moves the shared file
// pointer, so every worker queues on one mutex for every chunk. Unix pread
// needs no such lock. A private handle is a separate kernel file object with
// its own pointer, which is why this reopens the path -- duplicating the
// handle would share the pointer the lock exists to protect.
func openWorkerFile(qf *qvd.File) *os.File {
	f, err := os.Open(qf.Path)
	if err != nil {
		return nil
	}
	// The path can name a different file than the header was read from if the
	// input was replaced mid-run. Decoding records from a file nobody
	// validated would be worse than losing the parallel read, so share the
	// original handle instead.
	mine, errMine := f.Stat()
	orig, errOrig := qf.FileHandle().Stat()
	if errMine != nil || errOrig != nil || !os.SameFile(mine, orig) {
		f.Close()
		return nil
	}
	return f
}

func (c *Converter) newWorker() *worker {
	mem := memory.NewGoAllocator()
	w := &worker{
		c:      c,
		file:   c.File.FileHandle(),
		batch:  c.NewBatch(mem, c.BatchRows),
		raw:    make([]byte, c.BatchRows*c.File.RecordByteSize),
		symIdx: make([]int64, len(c.File.Columns)),
		hash:   c.Options.Quality == QualityFull,
		mem:    mem,
	}
	if own := openWorkerFile(c.File); own != nil {
		w.own, w.file = own, own
	}
	return w
}

func (w *worker) release() {
	w.batch.Release()
	if w.own != nil {
		w.own.Close()
	}
}

// decodeChunk reads one contiguous byte range and converts it into an Arrow
// record plus chunk-local quality metrics.
func (w *worker) decodeChunk(ch DecodeChunk) (DecodeResult, error) {
	f := w.c.File
	size := ch.RowCount * f.RecordByteSize
	buf := w.raw[:size]
	// ReadAt reports a short read as io.EOF, so check the byte count too: the
	// file can be truncated or replaced after the up-front size check.
	n, err := w.file.ReadAt(buf, ch.ByteOffset)
	if err != nil && !(errors.Is(err, io.EOF) && n == size) {
		return DecodeResult{}, fmt.Errorf("%w: read rows %d..%d at offset %d: read %d of %d bytes: %v",
			ErrInput, ch.StartRow, ch.StartRow+int64(ch.RowCount), ch.ByteOffset, n, size, err)
	}
	if n != size {
		return DecodeResult{}, fmt.Errorf("%w: read rows %d..%d at offset %d: got %d of %d bytes; "+
			"the input was truncated or modified during conversion",
			ErrInput, ch.StartRow, ch.StartRow+int64(ch.RowCount), ch.ByteOffset, n, size)
	}

	metrics := NewMetrics(w.c.Schema)
	cols := f.Columns
	outCols := w.c.Schema.Columns

	for r := 0; r < ch.RowCount; r++ {
		rec := buf[r*f.RecordByteSize : (r+1)*f.RecordByteSize]
		if err := qvd.DecodeRecord(rec, cols, w.symIdx); err != nil {
			return DecodeResult{}, fmt.Errorf("%w: row %d: %v", ErrInput, ch.StartRow+int64(r), err)
		}
		for ci := range outCols {
			src := outCols[ci].SourceIndex
			sym, err := f.Symbol(src, w.symIdx[src])
			if err != nil {
				return DecodeResult{}, fmt.Errorf("%w: row %d: %v", ErrInput, ch.StartRow+int64(r), err)
			}
			v, err := w.c.ConvertAt(ci, w.symIdx[src], sym)
			if err != nil {
				return DecodeResult{}, fmt.Errorf("%w: row %d: %v", ErrInput, ch.StartRow+int64(r), err)
			}
			w.batch.Append(ci, v)
			metrics.Columns[ci].Observe(v, w.hash)
		}
		w.batch.EndRow()
	}
	metrics.Rows = int64(ch.RowCount)

	return DecodeResult{Chunk: ch, Record: w.batch.NewRecord(), Metrics: metrics}, nil
}
