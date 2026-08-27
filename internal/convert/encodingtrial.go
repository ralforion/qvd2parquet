package convert

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/ralforion/qvd2parquet/internal/parquetwrite"
	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// Choosing an encoding is a measurement, not a rule of thumb. Whether
// delta_byte_array pays depends on the order the rows arrive in, since it
// stores each value against the one before it, and no property of the symbol
// table reveals that order. So a trial writes sampled rows through the real
// writer twice, once as the run would today and once with the candidate, and
// compares what came out.
const (
	// trialWindowRows is how many consecutive rows one window covers. At this
	// size the measured ratio lands within about a point of the whole file,
	// and it converges from above, so a sample understates a win rather than
	// overselling it. Below roughly 20k rows the compressor has too little
	// context and the estimate turns conservative enough to hide a real gain.
	trialWindowRows = 100_000
	// trialWindows samples the head, the middle and the tail, because row
	// order is what the measurement turns on and a file need not be uniform.
	trialWindows = 3
	// trialThreshold is how much smaller a candidate has to be before it is
	// worth recommending. A few percent is not worth changing a file for, and
	// it is inside the error a sample carries.
	trialThreshold = 0.8
	// dictionaryOverflowBytes is the writer's dictionary page size limit. A
	// column whose values would exceed it falls back to plain, and only such
	// a column has anything to gain here.
	dictionaryOverflowBytes = 1024 * 1024
	// trialGroupColumns bounds how many columns are held in memory at once.
	// A wide SAP extract can have dozens of candidates, and sampling them all
	// together would hold 300k rows of every one: hundreds of megabytes to
	// answer a question about file size. Groups cost a re-read of the sample
	// windows instead, which is sequential and usually still in the page
	// cache.
	trialGroupColumns = 8
)

// EncodingTrial is one column measured against one candidate encoding.
type EncodingTrial struct {
	Column   string
	Original string
	Encoding parquetwrite.Encoding
	// Baseline and Candidate are the compressed bytes the column chunk
	// occupied, written as the run would today and with the candidate. The
	// column chunk rather than the file, so a sample's footer does not
	// distort the comparison.
	Baseline    int64
	Candidate   int64
	SampledRows int64
}

// Ratio is the candidate's size as a share of the current one. Below 1 the
// candidate is smaller.
func (t EncodingTrial) Ratio() float64 {
	if t.Baseline <= 0 {
		return 1
	}
	return float64(t.Candidate) / float64(t.Baseline)
}

// Worthwhile reports whether the measurement justifies changing the file.
func (t EncodingTrial) Worthwhile() bool { return t.Ratio() <= trialThreshold }

// Line describes the trial for a log or report.
func (t EncodingTrial) Line() string {
	return fmt.Sprintf("%s  %s, measured %.0f%% of current size on %s sampled rows",
		t.Column, t.Encoding, t.Ratio()*100, withThousands(t.SampledRows))
}

// TrialEncodings measures candidate encodings for the columns that can gain
// from one, and returns the best candidate per column, worthwhile or not.
//
// It is the only part of the tool that reads records without converting them.
// The cost is bounded by the windows: on a 232 byte record that is about 23
// MiB per window read, plus a few tens of milliseconds per trial write.
func TrialEncodings(ctx context.Context, f *qvd.File, rs *ResolvedSchema, opts *Options) ([]EncodingTrial, error) {
	candidates := encodingCandidates(rs, f)
	if len(candidates) == 0 || f.NoOfRecords == 0 {
		return nil, nil
	}
	windows := trialChunks(f)
	if len(windows) == 0 {
		return nil, nil
	}

	codec, err := parquetwrite.ParseCompression(opts.Compression)
	if err != nil {
		return nil, err
	}
	base := parquetwrite.Options{Compression: codec, RowGroupRows: int64(opts.RowGroupRows)}

	var out []EncodingTrial
	for start := 0; start < len(candidates); start += trialGroupColumns {
		end := start + trialGroupColumns
		if end > len(candidates) {
			end = len(candidates)
		}
		group, err := trialGroup(ctx, f, rs, opts, base, candidates[start:end], windows)
		if err != nil {
			return nil, err
		}
		out = append(out, group...)
	}
	// Best saving first: on a wide file the list is what a reader scans.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ratio() < out[j].Ratio() })
	return out, nil
}

// trialGroup measures one group of columns, holding only that group's sampled
// rows in memory.
func trialGroup(ctx context.Context, f *qvd.File, rs *ResolvedSchema, opts *Options,
	base parquetwrite.Options, columns []int, windows []DecodeChunk) ([]EncodingTrial, error) {

	sub := subsetSchema(rs, columns)
	records, sampled, err := decodeWindows(ctx, f, sub, opts, windows)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, r := range records {
			r.Release()
		}
	}()

	var out []EncodingTrial
	for i := range sub.Columns {
		c := &sub.Columns[i]
		colRecords, schema := sliceColumn(sub, records, i)
		baseline, err := measure(schema, colRecords, base)
		if err != nil {
			return nil, err
		}
		best := EncodingTrial{Column: c.Name, Original: originalName(f, c),
			Baseline: baseline, Candidate: baseline, SampledRows: sampled}
		for _, enc := range candidateEncodings(c.ArrowType) {
			trialOpts := base
			trialOpts.ColumnEncodings = map[string]parquetwrite.Encoding{c.Name: enc}
			size, err := measure(schema, colRecords, trialOpts)
			if err != nil {
				return nil, err
			}
			if size < best.Candidate || best.Encoding == "" {
				best.Candidate, best.Encoding = size, enc
			}
		}
		if best.Encoding != "" {
			out = append(out, best)
		}
	}
	return out, nil
}

// WorthwhileTrials keeps the measurements that justify acting on them.
func WorthwhileTrials(trials []EncodingTrial) []EncodingTrial {
	var out []EncodingTrial
	for _, t := range trials {
		if t.Worthwhile() {
			out = append(out, t)
		}
	}
	return out
}

// encodingCandidates picks the columns worth measuring: those whose values
// would overflow the dictionary page, since a column that fits its dictionary
// is already encoded about as well as it can be.
func encodingCandidates(rs *ResolvedSchema, f *qvd.File) []int {
	var out []int
	for i := range rs.Columns {
		c := &rs.Columns[i]
		if c.SourceIndex < 0 || c.SourceIndex >= len(f.Profiles) {
			continue
		}
		p := f.Profiles[c.SourceIndex]
		if p == nil || p.Symbols == 0 {
			continue
		}
		var estimate int64
		switch {
		case isByteArrayType(c.ArrowType):
			// An upper bound: every symbol at its column's longest. The trial
			// itself is the real evidence, so an estimate that lets one extra
			// column through costs a measurement, not a wrong answer.
			estimate = p.Symbols * int64(p.MaxTextLen+4)
		case isIntegerBackedType(c.ArrowType):
			estimate = p.Symbols * 8
		default:
			continue
		}
		if estimate > dictionaryOverflowBytes {
			out = append(out, i)
		}
	}
	return out
}

// candidateEncodings lists what is worth trying for a type.
func candidateEncodings(t arrow.DataType) []parquetwrite.Encoding {
	switch {
	case isByteArrayType(t):
		return []parquetwrite.Encoding{
			parquetwrite.EncodingDeltaByteArray,
			parquetwrite.EncodingDeltaLengthByteArray,
		}
	case isIntegerBackedType(t):
		return []parquetwrite.Encoding{parquetwrite.EncodingDeltaBinaryPacked}
	}
	return nil
}

// trialChunks places the sample windows: head, middle and tail, or fewer when
// the file is too small to hold three that do not overlap.
func trialChunks(f *qvd.File) []DecodeChunk {
	rows := f.NoOfRecords
	if rows <= 0 {
		return nil
	}
	window := int64(trialWindowRows)
	if window > rows {
		window = rows
	}
	starts := []int64{0}
	if rows >= window*trialWindows {
		starts = []int64{0, rows/2 - window/2, rows - window}
	}
	out := make([]DecodeChunk, 0, len(starts))
	for i, start := range starts {
		if start < 0 {
			start = 0
		}
		out = append(out, DecodeChunk{
			Index:      int64(i),
			StartRow:   start,
			RowCount:   int(window),
			ByteOffset: f.RecordStart + start*int64(f.RecordByteSize),
		})
	}
	return out
}

// subsetSchema builds a schema holding only the named columns, so the sample
// is decoded once for every candidate rather than once per candidate.
func subsetSchema(rs *ResolvedSchema, columns []int) *ResolvedSchema {
	sub := &ResolvedSchema{}
	fields := make([]arrow.Field, 0, len(columns))
	for _, i := range columns {
		sub.Columns = append(sub.Columns, rs.Columns[i])
		fields = append(fields, rs.Arrow.Field(i))
	}
	sub.Arrow = arrow.NewSchema(fields, nil)
	return sub
}

// decodeWindows converts the sampled rows exactly as a conversion would.
func decodeWindows(ctx context.Context, f *qvd.File, sub *ResolvedSchema, opts *Options, windows []DecodeChunk) ([]arrow.Record, int64, error) {
	// The quality gate's hashing is pure cost here, and a trial writes
	// nothing that is kept, so it is turned off for the sample.
	sampleOpts := *opts
	sampleOpts.Quality = QualityNone
	sampleOpts.BatchRows = windows[0].RowCount

	conv, err := NewConverter(f, sub, &sampleOpts)
	if err != nil {
		return nil, 0, err
	}
	w := conv.newWorker()
	defer w.release()

	var records []arrow.Record
	var sampled int64
	for _, ch := range windows {
		if err := ctx.Err(); err != nil {
			for _, r := range records {
				r.Release()
			}
			return nil, 0, err
		}
		res, err := w.decodeChunk(ch)
		if err != nil {
			for _, r := range records {
				r.Release()
			}
			return nil, 0, err
		}
		records = append(records, res.Record)
		sampled += int64(ch.RowCount)
	}
	return records, sampled, nil
}

// sliceColumn narrows the sampled records to one column, so each measurement
// sees exactly the column chunk it is about.
func sliceColumn(sub *ResolvedSchema, records []arrow.Record, col int) ([]arrow.Record, *arrow.Schema) {
	schema := arrow.NewSchema([]arrow.Field{sub.Arrow.Field(col)}, nil)
	out := make([]arrow.Record, 0, len(records))
	for _, r := range records {
		out = append(out, array.NewRecord(schema, []arrow.Array{r.Column(col)}, r.NumRows()))
	}
	return out, schema
}

// measure writes the records to memory and reports the compressed size of the
// column chunk, which is the number the choice turns on.
func measure(schema *arrow.Schema, records []arrow.Record, opts parquetwrite.Options) (int64, error) {
	var buf bytes.Buffer
	fw, err := pqarrow.NewFileWriter(schema, &buf, parquetwrite.Properties(opts),
		pqarrow.NewArrowWriterProperties(pqarrow.WithStoreSchema()))
	if err != nil {
		return 0, fmt.Errorf("encoding trial: create writer: %w", err)
	}
	for _, r := range records {
		if err := fw.Write(r); err != nil {
			return 0, fmt.Errorf("encoding trial: write sample: %w", err)
		}
	}
	if err := fw.Close(); err != nil {
		return 0, fmt.Errorf("encoding trial: close sample: %w", err)
	}

	rdr, err := file.NewParquetReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return 0, fmt.Errorf("encoding trial: read sample back: %w", err)
	}
	defer rdr.Close()
	var total int64
	for rg := 0; rg < rdr.NumRowGroups(); rg++ {
		chunk, err := rdr.MetaData().RowGroup(rg).ColumnChunk(0)
		if err != nil {
			return 0, fmt.Errorf("encoding trial: read column chunk: %w", err)
		}
		total += chunk.TotalCompressedSize()
	}
	return total, nil
}

// originalName is the QVD field a resolved column came from.
func originalName(f *qvd.File, c *ResolvedColumn) string {
	if c.SourceIndex >= 0 && c.SourceIndex < len(f.Columns) {
		return f.Columns[c.SourceIndex].Name
	}
	return c.Name
}

// applyMeasuredEncodings runs the trial and adopts what it recommends, leaving
// any column an explicit rule already named alone: a rule is a decision, and a
// measurement should not overrule it.
func applyMeasuredEncodings(ctx context.Context, f *qvd.File, rs *ResolvedSchema, opts *Options, enc *ResolvedEncodings, logf Logf) error {
	start := time.Now()
	trials, err := TrialEncodings(ctx, f, rs, opts)
	if err != nil {
		return err
	}
	if len(trials) == 0 {
		logf("encoding: no column is large enough for the encoding to matter")
		return nil
	}

	var adopted, rejected int
	for _, t := range trials {
		if _, pinned := enc.ByColumn[t.Column]; pinned {
			continue
		}
		if !t.Worthwhile() {
			rejected++
			continue
		}
		enc.ByColumn[t.Column] = t.Encoding
		enc.Pinned = append(enc.Pinned, fmt.Sprintf("%s=%s", t.Column, t.Encoding))
		enc.Measured = append(enc.Measured, t)
		adopted++
	}
	logf("encoding: measured %d column(s) on %s sampled rows in %s: %d adopted, %d left as they are",
		len(trials), withThousands(trials[0].SampledRows),
		time.Since(start).Round(time.Millisecond), adopted, rejected)
	for _, t := range enc.Measured {
		logf("encoding: %s", t.Line())
	}
	return nil
}
