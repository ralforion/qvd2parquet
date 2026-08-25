// Package parquetwrite wraps the Arrow/Parquet file writer with the output
// safety behaviour the converter needs: write to a temporary file, validate,
// then rename into place.
package parquetwrite

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

// ErrOutput marks an output/write failure (CLI exit code 5).
var ErrOutput = errors.New("output error")

// ParseCompression maps the --compression flag value.
func ParseCompression(s string) (compress.Compression, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "zstd":
		return compress.Codecs.Zstd, nil
	case "snappy":
		return compress.Codecs.Snappy, nil
	case "gzip":
		return compress.Codecs.Gzip, nil
	case "uncompressed", "none":
		return compress.Codecs.Uncompressed, nil
	}
	return 0, fmt.Errorf("invalid --compression %q: want zstd|snappy|gzip|uncompressed", s)
}

// Options configures the Parquet writer.
type Options struct {
	Compression compress.Compression
	// RowGroupRows caps how many rows go into one row group.
	RowGroupRows int64
}

// Writer writes Arrow records to a Parquet file through a temporary path.
type Writer struct {
	finalPath string
	tmpPath   string
	file      *os.File
	fw        *pqarrow.FileWriter
	rows      int64
	closed    bool
	renamed   bool
}

// Create opens a temporary Parquet output next to finalPath. If force is false
// and finalPath already exists, it fails without touching anything.
func Create(finalPath string, schema *arrow.Schema, opts Options, force bool) (*Writer, error) {
	if _, err := os.Stat(finalPath); err == nil && !force {
		return nil, fmt.Errorf("%w: %s already exists; pass --force to overwrite", ErrOutput, finalPath)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: stat %s: %v", ErrOutput, finalPath, err)
	}

	tmpPath := fmt.Sprintf("%s.tmp-%d", finalPath, os.Getpid())
	if err := os.MkdirAll(filepath.Dir(tmpPath), 0o755); err != nil {
		return nil, fmt.Errorf("%w: create output directory: %v", ErrOutput, err)
	}
	f, err := os.Create(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("%w: create %s: %v", ErrOutput, tmpPath, err)
	}

	props := []parquet.WriterProperty{
		parquet.WithCompression(opts.Compression),
		parquet.WithDictionaryDefault(true),
		parquet.WithStats(true),
		parquet.WithDataPageVersion(parquet.DataPageV2),
	}
	if opts.RowGroupRows > 0 {
		props = append(props, parquet.WithMaxRowGroupLength(opts.RowGroupRows))
	}
	wp := parquet.NewWriterProperties(props...)
	// StoreSchema writes the Arrow schema into the Parquet footer so readers
	// round-trip types such as time32[ms] faithfully.
	ap := pqarrow.NewArrowWriterProperties(pqarrow.WithStoreSchema())

	fw, err := pqarrow.NewFileWriter(schema, f, wp, ap)
	if err != nil {
		f.Close()
		removeTemp(tmpPath)
		return nil, fmt.Errorf("%w: create Parquet writer: %v", ErrOutput, err)
	}
	return &Writer{finalPath: finalPath, tmpPath: tmpPath, file: f, fw: fw}, nil
}

// TempPath is the path currently being written, which the quality gate reads
// before the file is renamed into place.
func (w *Writer) TempPath() string { return w.tmpPath }

// Rows is the number of rows written so far.
func (w *Writer) Rows() int64 { return w.rows }

// Write appends one Arrow record as a row group.
func (w *Writer) Write(rec arrow.Record) error {
	if rec.NumRows() == 0 {
		return nil
	}
	if err := w.fw.WriteBuffered(rec); err != nil {
		return fmt.Errorf("%w: write row group: %v", ErrOutput, err)
	}
	w.rows += rec.NumRows()
	return nil
}

// Close finalizes the Parquet footer. The file stays at its temporary path
// until Commit is called.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	// pqarrow.FileWriter.Close writes the footer and closes the underlying
	// file, so the durability sync needs a fresh handle.
	if err := w.fw.Close(); err != nil {
		w.file.Close()
		return fmt.Errorf("%w: close Parquet writer: %v", ErrOutput, err)
	}
	w.file.Close()

	f, err := os.OpenFile(w.tmpPath, os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("%w: reopen %s to sync: %v", ErrOutput, w.tmpPath, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("%w: sync %s: %v", ErrOutput, w.tmpPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%w: close %s: %v", ErrOutput, w.tmpPath, err)
	}
	return nil
}

// Commit renames the finished temporary file onto the final output path.
func (w *Writer) Commit() error {
	if !w.closed {
		return errors.New("parquetwrite: Commit called before Close")
	}
	if err := os.Rename(w.tmpPath, w.finalPath); err != nil {
		return fmt.Errorf("%w: rename %s to %s: %v", ErrOutput, w.tmpPath, w.finalPath, err)
	}
	w.renamed = true
	return nil
}

// Abort closes and removes the temporary output. It is safe to call after a
// successful Commit, where it does nothing.
//
// It reports a removal it could not make. The temporary file is a partial
// Parquet file, and leaving one next to the real output without a word is
// worse than the failure itself: the name is the only thing marking it as
// unfinished.
func (w *Writer) Abort() error {
	if w.renamed {
		return nil
	}
	if !w.closed {
		w.closed = true
		// Both are best-effort: the file is about to be deleted, and a footer
		// that failed to write does not matter. Closing is what releases the
		// handle so the delete can succeed at all, which is the point.
		w.fw.Close()
		w.file.Close()
	}
	return removeTemp(w.tmpPath)
}

// removeTemp deletes a temporary output, retrying briefly. Windows refuses to
// delete a file while any handle on it is open, and a virus scanner or the
// search indexer routinely holds one for a moment on a file just written, so
// the first attempt can fail on a file that is about to be perfectly
// deletable.
func removeTemp(path string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 40 * time.Millisecond)
	}
	return fmt.Errorf("%w: could not remove the partial output %s: %v", ErrOutput, path, err)
}

// DefaultAllocator is the Arrow allocator used for builders and batches.
var DefaultAllocator = memory.NewGoAllocator()
