package convert

import (
	"bytes"
	"context"
	"fmt"
	"math/bits"
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
	// dictionary that cannot fit it is never kept whole, so such a column is
	// either settled plain or worth measuring.
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
	// A column an explicit rule already names is not measured at all. A rule
	// is a decision, so measuring it would only produce a recommendation
	// nothing may act on, which reads as the tool arguing with itself.
	pinned, err := ResolveEncodings(opts.Encodings.Rules, rs, f)
	if err != nil {
		return nil, err
	}
	candidates := encodingCandidates(rs, f, int64(opts.RowGroupRows), opts.EmptyStringAsNull, pinned.ByColumn)
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
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w while measuring encodings", ErrCanceled)
		}
		trial, err := measureColumn(sub, records, i, f, base, opts.EmptyStringAsNull, sampled)
		if err != nil {
			return nil, err
		}
		if trial.Encoding != "" {
			out = append(out, trial)
		}
	}
	return out, nil
}

// measureColumn measures one column's candidates against the way it would be
// written today, and returns the best of them.
func measureColumn(sub *ResolvedSchema, records []arrow.Record, i int, f *qvd.File,
	base parquetwrite.Options, emptyAsNull bool, sampled int64) (EncodingTrial, error) {

	c := &sub.Columns[i]
	colRecords, schema := sliceColumn(sub, records, i)
	// Each sliced record retains the column it was built from, so they have
	// to be released whichever way this returns.
	defer func() {
		for _, r := range colRecords {
			r.Release()
		}
	}()

	// The baseline is the column as a conversion would write it: plain where
	// the symbol table ruled the dictionary out, the writer's default where
	// it could not.
	baseOpts := base
	baseOpts.ColumnEncodings = writerEncodings(sub, f, base.RowGroupRows, emptyAsNull, nil)
	baseline, err := measure(schema, colRecords, baseOpts)
	if err != nil {
		return EncodingTrial{}, err
	}
	best := EncodingTrial{Column: c.Name, Original: originalName(f, c),
		Baseline: baseline, Candidate: baseline, SampledRows: sampled}
	for _, enc := range candidateEncodings(c.ArrowType) {
		trialOpts := base
		trialOpts.ColumnEncodings = map[string]parquetwrite.Encoding{c.Name: enc}
		size, err := measure(schema, colRecords, trialOpts)
		if err != nil {
			return EncodingTrial{}, err
		}
		if size < best.Candidate || best.Encoding == "" {
			best.Candidate, best.Encoding = size, enc
		}
	}
	return best, nil
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

// encodingCandidates picks the columns worth measuring: those the symbol
// table writes plain, and those it could not settle whose dictionary would
// not fit the page, since a column that keeps its whole dictionary is
// already encoded about as well as it can be.
func encodingCandidates(rs *ResolvedSchema, f *qvd.File, rowGroupRows int64, emptyAsNull bool, pinned map[string]parquetwrite.Encoding) []int {
	var out []int
	for i := range rs.Columns {
		c := &rs.Columns[i]
		if _, decided := pinned[c.Name]; decided {
			continue
		}
		if len(candidateEncodings(c.ArrowType)) == 0 {
			continue
		}
		enc, settled := dictionaryChoice(c, f, rowGroupRows, emptyAsNull)
		dictBytes, _, ok := dictionarySize(c, f)
		if (settled && enc == parquetwrite.EncodingPlain) || (!settled && ok && dictBytes > dictionaryOverflowBytes) {
			out = append(out, i)
		}
	}
	return out
}

// dictionarySize is what a column's dictionary would hold if every symbol
// appeared: the page the writer would build, in bytes, and the bytes one
// value takes written plain. It reports false for a column it cannot judge.
func dictionarySize(c *ResolvedColumn, f *qvd.File) (dictBytes, perValue int64, ok bool) {
	if c.SourceIndex < 0 || c.SourceIndex >= len(f.Profiles) {
		return 0, 0, false
	}
	p := f.Profiles[c.SourceIndex]
	if p == nil || p.Symbols == 0 {
		return 0, 0, false
	}
	switch {
	case isByteArrayType(c.ArrowType):
		// Exact: every symbol's text once, each behind a four byte length.
		// A text column whose symbols carry no text, numbers rendered under
		// --mixed=text, has nothing to add up and is bounded at the twenty
		// characters a rendered number can reach.
		text := p.TextBytes
		if text == 0 {
			text = 20 * p.Symbols
		}
		dictBytes = text + 4*p.Symbols
		return dictBytes, dictBytes / p.Symbols, true
	}
	if w := physicalWidth(c.ArrowType); w > 0 {
		return w * p.Symbols, w, true
	}
	return 0, 0, false
}

// physicalWidth is the bytes one value of a fixed-width type takes on a
// Parquet page, which is what a dictionary entry and a plain value both
// cost, or zero for a type this does not size. A decimal is written as a
// fixed-length byte array just wide enough for its precision, three bytes
// for a five digit amount, not the sixteen it takes in memory.
func physicalWidth(t arrow.DataType) int64 {
	switch t.ID() {
	case arrow.INT32, arrow.DATE32, arrow.TIME32:
		return 4
	case arrow.INT64, arrow.TIME64, arrow.TIMESTAMP, arrow.FLOAT64:
		return 8
	case arrow.DECIMAL128:
		return int64(pqarrow.DecimalSize(t.(*arrow.Decimal128Type).Precision))
	}
	return 0
}

// dictionaryChoice decides from the symbol table whether a column keeps its
// dictionary, where the symbol table can settle it, and reports false where
// only the row order could.
//
// A dictionary is built per row group, and what it saves depends on which
// values each row group repeats, which the symbol table cannot see: a
// hundred thousand keys each repeated five times cost a row group a fifth
// of its rows in dictionary if the repeats sit together and nearly all of
// them if they are spread, and a repeat of a short value saves less than a
// repeat of a long one. So the choice is made only where it comes out the
// same either way. A column keeps its dictionary, and is named so the writer
// does not judge otherwise from its first rows, when the dictionary fits the
// page and beats plain even at its worst: every symbol in every row group,
// and every repeat the shortest value the column has. A column is written
// plain when a dictionary cannot pay even at its best: every repeat of a
// value in the same row group, and every repeat the longest value, which is
// the composite key. In between, the writer's default stands, a dictionary
// that falls back to plain where it overflows, and --encoding auto is the
// measurement.
//
// A column that can hold nulls, an empty string written as null included,
// is never settled on a dictionary. A null row costs plain nothing and a
// dictionary nothing, so a row group of distinct values padded with nulls
// pays the indices for no saving at all.
func dictionaryChoice(c *ResolvedColumn, f *qvd.File, rowGroupRows int64, emptyAsNull bool) (parquetwrite.Encoding, bool) {
	dictBytes, perValue, ok := dictionarySize(c, f)
	if !ok {
		return "", false
	}
	p := f.Profiles[c.SourceIndex]
	if emptyAsNull {
		p = p.WithEmptyStringsAsNulls()
	}
	symbols := p.Symbols
	rows := f.NoOfRecords
	if rows <= 0 {
		return "", false
	}
	rowsPerGroup := rows
	if rowGroupRows > 0 && rowGroupRows < rows {
		rowsPerGroup = rowGroupRows
	}
	// What a repeated row saves written plain: the value it repeats, at the
	// column's shortest and at its longest. Fixed-width types have one width.
	shortest, longest := perValue, perValue
	if isByteArrayType(c.ArrowType) {
		shortest, longest = int64(4+p.MinTextLen), int64(4+p.MaxTextLen)
		if p.TextBytes == 0 {
			// Numbers rendered as text: one to twenty characters.
			shortest, longest = 5, 24
		}
	}
	// Indices are per row group too, bit-packed at the width the group's
	// distinct count needs.
	indexBytes := func(distinct int64) int64 {
		return rowsPerGroup * int64(bits.Len64(uint64(distinct-1))) / 8
	}

	// The dictionary at its worst: every symbol in every row group, capped
	// at the rows the group has, and every repeated row the shortest value.
	worstDistinct := symbols
	if worstDistinct > rowsPerGroup {
		worstDistinct = rowsPerGroup
	}
	if p.Nulls == 0 && dictBytes <= dictionaryOverflowBytes &&
		indexBytes(worstDistinct) < (rowsPerGroup-worstDistinct)*shortest {
		return parquetwrite.EncodingDictionary, true
	}
	// The dictionary at its best: every repeat of a value sits in the same
	// row group, so each group holds its share of the symbols and no more,
	// and every repeated row is the longest value.
	bestDistinct := rowsPerGroup * symbols / rows
	if bestDistinct < 1 {
		bestDistinct = 1
	}
	if (rowsPerGroup-bestDistinct)*longest <= indexBytes(bestDistinct) {
		return parquetwrite.EncodingPlain, true
	}
	return "", false
}

// writerEncodings is what the writer is told per column: the symbol table's
// choice of dictionary or plain for every column it can settle, and whatever
// a rule or a measurement pinned, which wins. Naming the dictionary rather
// than leaving the default matters too: on an uncompressed column the writer
// would otherwise still make its own first-batch judgement and drop it. A
// column the symbol table cannot settle is not named, and the writer's
// default stands.
func writerEncodings(rs *ResolvedSchema, f *qvd.File, rowGroupRows int64, emptyAsNull bool, pinned map[string]parquetwrite.Encoding) map[string]parquetwrite.Encoding {
	out := make(map[string]parquetwrite.Encoding, len(rs.Columns))
	for i := range rs.Columns {
		c := &rs.Columns[i]
		if enc, ok := dictionaryChoice(c, f, rowGroupRows, emptyAsNull); ok {
			out[c.Name] = enc
		}
	}
	for name, enc := range pinned {
		out[name] = enc
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

// trialChunks places the sample windows: head, middle and tail on a file large
// enough for three that do not overlap, and otherwise a single window over
// every row.
//
// Sampling the whole file below that size costs no more than three windows
// would, since three windows are 300k rows either way, and it closes the gap
// where a file of 150k rows would have been judged on its first 100k alone.
// Row order is the thing being measured, so a sorted head and a shuffled tail
// must not be able to answer for each other.
func trialChunks(f *qvd.File) []DecodeChunk {
	rows := f.NoOfRecords
	if rows <= 0 {
		return nil
	}
	window := int64(trialWindowRows)
	if rows <= window*trialWindows {
		return []DecodeChunk{{RowCount: int(rows), ByteOffset: f.RecordStart}}
	}
	starts := []int64{0, rows/2 - window/2, rows - window}
	out := make([]DecodeChunk, 0, len(starts))
	for i, start := range starts {
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
	w, err := conv.newWorker()
	if err != nil {
		return nil, 0, err
	}
	defer w.release()

	var records []arrow.Record
	var sampled int64
	for _, ch := range windows {
		if ctx.Err() != nil {
			for _, r := range records {
				r.Release()
			}
			// Wrapped the way every other cancellable path wraps it, so the
			// CLI reports exit 7 rather than reading it as an input failure.
			return nil, 0, fmt.Errorf("%w while measuring encodings", ErrCanceled)
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

	adopted, rejected := enc.AdoptTrials(trials)
	logf("encoding: measured %d column(s) on %s sampled rows in %s: %d adopted, %d left as they are",
		len(trials), withThousands(trials[0].SampledRows),
		time.Since(start).Round(time.Millisecond), adopted, rejected)
	for _, t := range trials {
		if enc.Adopted(t) {
			logf("encoding: %s", t.Line())
		}
	}
	return nil
}
