package convert

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ralforion/qvd2parquet/internal/parquetwrite"
	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// Stats summarizes a finished conversion.
type Stats struct {
	Rows        int64
	Columns     int
	OutputBytes int64
	Elapsed     time.Duration
	SymbolsRead int64
	// DecimalsNearLimit names decimal columns whose widest observed value
	// already fills most of the type's range, so a later load carrying a
	// larger one would not convert.
	DecimalsNearLimit []string
	// ExcludeNoMatch holds the --exclude patterns that matched no field in
	// this file, and Renames records what --field-regex did to the fields
	// that were kept. Both describe silent outcomes: a pattern that drops
	// nothing and an expression that renames nothing look exactly like ones
	// that worked.
	ExcludeNoMatch []string
	Renames        RenameSummary
	// Encodings names the columns written with a pinned or measured encoding,
	// as "NAME=encoding".
	Encodings []string
}

// maxNamedFields caps how many field names a single log line will list. A run
// where the expression matched nothing would otherwise print every field on a
// 213 column extract.
const maxNamedFields = 5

// RowsPerSecond is the overall conversion throughput.
func (s Stats) RowsPerSecond() float64 {
	if s.Elapsed <= 0 {
		return 0
	}
	return float64(s.Rows) / s.Elapsed.Seconds()
}

// Logf receives human-readable progress lines. It may be nil.
type Logf func(format string, args ...any)

// Run performs the full QVD to Parquet conversion.
func Run(ctx context.Context, inputPath, outputPath string, opts *Options, logf Logf) (*Stats, *QualityReport, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := opts.Validate(); err != nil {
		return nil, nil, err
	}
	start := time.Now()

	f, err := qvd.Open(inputPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	if err := f.SelectColumns(opts.Columns); err != nil {
		return nil, nil, err
	}
	dropped, unmatchedExcludes, err := f.ExcludeColumns(opts.Exclude)
	if err != nil {
		return nil, nil, err
	}
	if len(dropped) > 0 {
		logf("excluded %d column(s) by pattern: %s", len(dropped), strings.Join(dropped, ", "))
	}
	if len(unmatchedExcludes) > 0 {
		logf("note: --exclude %s matched no field; patterns are wildcards over the "+
			"original QVD names, before --field-regex renames anything",
			quotedList(unmatchedExcludes))
	}
	logf("%s: table %q, %d rows, %d bytes/record, %d of %d columns selected",
		inputPath, f.Header.TableName, f.NoOfRecords, f.RecordByteSize,
		len(f.SelectedColumns()), len(f.Columns))

	symStart := time.Now()
	if err := f.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		return nil, nil, err
	}
	var symbolsRead int64
	for _, p := range f.Profiles {
		if p != nil {
			symbolsRead += p.Symbols
		}
	}
	logf("read %d symbols in %s; records start at offset %d",
		symbolsRead, time.Since(symStart).Round(time.Millisecond), f.RecordStart)

	var override *SchemaOverride
	if opts.SchemaOverridePath != "" {
		if override, err = LoadSchemaOverride(opts.SchemaOverridePath); err != nil {
			return nil, nil, err
		}
	}

	rs, err := ResolveSchema(f, opts, override)
	if err != nil {
		return nil, nil, err
	}
	for _, n := range rs.Notes {
		logf("schema: %s", n)
	}
	if line := rs.Renames.Line(maxNamedFields); line != "" {
		logf("field-regex: %s", line)
	}

	// Batch size depends on the resolved column count, so it can only be
	// settled here. Work from a copy: the caller's Options are shared across
	// files in a batch run, where each file resolves its own width.
	var rounded, nonFinite int64
	for _, c := range rs.Columns {
		rounded += c.DecimalRounded
		nonFinite += c.NonFiniteNulls
	}
	if rounded > 0 {
		logf("note: %d decimal value(s) were rounded to their column's scale; "+
			"pass --decimal-strict to fail instead", rounded)
	}
	if nonFinite > 0 {
		logf("note: %d value(s) are NaN or infinite and were written as null", nonFinite)
	}
	if opts.EmptyStringAsNull {
		// Count per output column what that column will actually null. A
		// dual's text column nulls empty display strings, including those on
		// duals; every other column nulls only symbols that are nothing but an
		// empty string, since a dual's number survives its blank text.
		var empty int64
		for _, c := range rs.Columns {
			p := f.Profiles[c.SourceIndex]
			if p == nil {
				continue
			}
			if c.Strategy == StrategyDualText {
				empty += p.EmptyText
			} else {
				empty += p.EmptyStrings
			}
		}
		if empty > 0 {
			logf("note: %d empty string symbol(s) written as null; "+
				"pass --empty-as-null=false to keep them as empty strings", empty)
		}
	}

	if opts.SchemaReportPath != "" {
		if err := WriteSchemaReport(opts.SchemaReportPath, inputPath, f, rs, opts); err != nil {
			return nil, nil, err
		}
		logf("wrote schema report to %s", opts.SchemaReportPath)
	}

	conv, err := NewConverter(f, rs, opts)
	if err != nil {
		return nil, nil, err
	}
	// NewConverter resolves an automatic batch size against this file's width.
	// Carry it in a copy so everything downstream -- the quality gate reads
	// the output back a batch at a time -- sizes itself the same way. A copy,
	// because a batch run shares one Options across files of differing width.
	if opts.BatchRows != conv.BatchRows {
		logf("batch: %d rows over %d columns (~%.1fM cells per batch), %d rows per row group",
			conv.BatchRows, len(rs.Columns),
			float64(conv.BatchRows*len(rs.Columns))/1e6, opts.RowGroupRows)
		sized := *opts
		sized.BatchRows = conv.BatchRows
		opts = &sized
	}

	codec, err := parquetwrite.ParseCompression(opts.Compression)
	if err != nil {
		return nil, nil, err
	}
	enc, err := ResolveEncodings(opts.Encodings.Rules, rs, f)
	if err != nil {
		return nil, nil, err
	}
	// Pins are reported before any measurement, since a measurement that
	// follows must leave them alone. What it adopts reports itself.
	if len(enc.Pinned) > 0 {
		logf("encoding: %s", strings.Join(enc.Pinned, ", "))
	}
	if opts.Encodings.Auto {
		if err := applyMeasuredEncodings(ctx, f, rs, opts, enc, logf); err != nil {
			return nil, nil, err
		}
	}
	if len(enc.Unmatched) > 0 {
		logf("note: --encoding %s matched no column; patterns are wildcards over "+
			"the output column names and the original QVD names",
			quotedList(enc.Unmatched))
	}
	w, err := parquetwrite.Create(outputPath, rs.Arrow, parquetwrite.Options{
		Compression:     codec,
		RowGroupRows:    int64(opts.RowGroupRows),
		ColumnEncodings: enc.ByColumn,
	}, opts.Force)
	if err != nil {
		return nil, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			// Say so rather than leaving a partial Parquet file sitting next
			// to the real output with nobody aware of it.
			if err := w.Abort(); err != nil {
				logf("warning: %v", err)
			}
		}
	}()

	decodeStart := time.Now()
	// The row total is known from the header before a single record is read,
	// so every progress line can carry the share done and the time left.
	prog := newProgressETA(f.NoOfRecords, decodeStart)
	metrics, err := conv.Run(ctx, w, func(rows int64) {
		logf("converted %s", prog.Report(rows, time.Now()))
	})
	if err != nil {
		return nil, nil, err
	}
	// Reported unconditionally, the way the quality gate reports itself. It
	// used to arrive only as the last --progress line, so --progress 0 left
	// the conversion as the one phase that never said how long it took.
	decodeElapsed := time.Since(decodeStart)
	logf("conversion finished in %s: %d rows (%.0f rows/s)",
		decodeElapsed.Round(time.Millisecond), metrics.Rows,
		float64(metrics.Rows)/decodeElapsed.Seconds())
	if err := w.Close(); err != nil {
		return nil, nil, err
	}

	// Validate the temporary file, so a failed gate never leaves a
	// final-looking output in place.
	var report *QualityReport
	if opts.Quality != QualityNone {
		gateStart := time.Now()
		report, err = RunQualityGate(ctx, inputPath, outputPath, w.TempPath(), rs, metrics, opts, logf)
		if err != nil {
			return nil, nil, err
		}
		logf("quality gate %s finished in %s: %s",
			opts.Quality, time.Since(gateStart).Round(time.Millisecond), passFail(report.Passed))
		if opts.QualityReportPath != "" {
			if err := report.Write(opts.QualityReportPath); err != nil {
				return nil, nil, err
			}
			logf("wrote quality report to %s", opts.QualityReportPath)
		}
		if err := report.Err(); err != nil {
			return nil, report, err
		}
	}

	if err := w.Commit(); err != nil {
		return nil, report, err
	}
	committed = true

	st := &Stats{
		Rows:              metrics.Rows,
		Columns:           len(rs.Columns),
		Elapsed:           time.Since(start),
		SymbolsRead:       symbolsRead,
		DecimalsNearLimit: decimalsNearLimit(rs, f),
		ExcludeNoMatch:    unmatchedExcludes,
		Renames:           rs.Renames,
		Encodings:         enc.Pinned,
	}
	if fi, err := os.Stat(outputPath); err == nil {
		st.OutputBytes = fi.Size()
	}
	return st, report, nil
}

func passFail(ok bool) string {
	if ok {
		return "passed"
	}
	return "FAILED"
}

// SchemaReport is the --schema-report document.
type SchemaReport struct {
	Input          string `json:"input"`
	TableName      string `json:"tableName"`
	Rows           int64  `json:"rows"`
	RecordByteSize int    `json:"recordByteSize"`
	// FieldRegex is present only when --field-regex was given, and says which
	// of the selected fields it left alone.
	FieldRegex *FieldRegexReport    `json:"fieldRegex,omitempty"`
	Columns    []SchemaReportColumn `json:"columns"`
}

// FieldRegexReport summarises a --field-regex run over one file. Unlike the
// log record, which is one line per file, this can afford every name.
type FieldRegexReport struct {
	Fields    int      `json:"fields"`
	Renamed   int      `json:"renamed"`
	Unchanged []string `json:"unchanged"`
}

// SchemaReportColumn explains one output column's type decision.
type SchemaReportColumn struct {
	Name         string             `json:"name"`
	SourceColumn string             `json:"sourceColumn"`
	Comment      string             `json:"comment,omitempty"`
	QlikType     string             `json:"qlikType"`
	BitOffset    int                `json:"bitOffset"`
	BitWidth     int                `json:"bitWidth"`
	Bias         int64              `json:"bias"`
	Symbols      int64              `json:"symbols"`
	Profile      *qvd.ColumnProfile `json:"profile,omitempty"`
	ResolvedType string             `json:"resolvedType"`
	Strategy     string             `json:"strategy"`
	Nullable     bool               `json:"nullable"`
	// Range is the column's observed low and high values rendered in the type
	// it is written as. The profile above holds the same span as the raw Qlik
	// payload, where a date is a serial day number: 411241 rather than
	// 3025-12-08.
	Range string `json:"range,omitempty"`
	// Encoding is set only when --encoding pins this column; otherwise the
	// writer's default applies and there is nothing to record.
	Encoding string         `json:"encoding,omitempty"`
	Decimal  *DecimalReport `json:"decimal,omitempty"`
	Note     string         `json:"note"`
}

// DecimalReport documents how a decimal column was resolved.
type DecimalReport struct {
	Precision   int32 `json:"precision"`
	Scale       int32 `json:"scale"`
	FromText    bool  `json:"fromDisplayStrings"`
	FromNumeric bool  `json:"fromNumericPayloads"`
	// Rounded counts values that did not fit the scale and were rounded to it.
	Rounded int64 `json:"roundedValues,omitempty"`
	// NonFinite counts NaN and infinite values written as null.
	NonFinite int64 `json:"nonFiniteNulls,omitempty"`
	// Limit is the largest magnitude the type holds, and Used is the fraction
	// of it the widest observed value occupies. Precision is inferred from the
	// values, so the data always fits by construction; what varies is how much
	// room a later load has before it does not.
	Limit string  `json:"limit,omitempty"`
	Used  float64 `json:"usedFraction,omitempty"`
}

// WriteSchemaReport saves the inferred schema and profiles as JSON.
func WriteSchemaReport(path, inputPath string, f *qvd.File, rs *ResolvedSchema, opts *Options) error {
	// Resolution errors are reported by the conversion itself, which fails on
	// them; a report should still describe everything it can.
	pinned := map[string]parquetwrite.Encoding{}
	if enc, err := ResolveEncodings(opts.Encodings.Rules, rs, f); err == nil {
		pinned = enc.ByColumn
	}
	rep := SchemaReport{
		Input:          inputPath,
		TableName:      f.Header.TableName,
		Rows:           f.NoOfRecords,
		RecordByteSize: f.RecordByteSize,
	}
	if rs.Renames.Fields > 0 {
		rep.FieldRegex = &FieldRegexReport{
			Fields:    rs.Renames.Fields,
			Renamed:   rs.Renames.Renamed,
			Unchanged: append([]string{}, rs.Renames.Unchanged...),
		}
	}
	// Notes are produced per source column; map them onto output columns.
	noteBySource := map[int]string{}
	ni := 0
	seen := map[int]bool{}
	for _, c := range rs.Columns {
		if !seen[c.SourceIndex] {
			seen[c.SourceIndex] = true
			if ni < len(rs.Notes) {
				noteBySource[c.SourceIndex] = rs.Notes[ni]
				ni++
			}
		}
	}

	for i := range rs.Columns {
		c := &rs.Columns[i]
		src := f.Columns[c.SourceIndex]
		rc := SchemaReportColumn{
			Name:         c.Name,
			SourceColumn: src.Name,
			Comment:      c.Comment,
			QlikType:     src.QlikType.String(),
			BitOffset:    src.BitOffset,
			BitWidth:     src.BitWidth,
			Bias:         src.Bias,
			Symbols:      src.SymbolCount,
			Profile:      f.Profiles[c.SourceIndex],
			ResolvedType: c.ArrowType.String(),
			Strategy:     c.Strategy.String(),
			Nullable:     c.Nullable,
			Range:        ValueRange(c, f.Profiles[c.SourceIndex], opts),
			Encoding:     string(pinned[c.Name]),
			Note:         noteBySource[c.SourceIndex],
		}
		if c.Strategy == StrategyDecimal {
			rc.Decimal = &DecimalReport{
				Precision:   c.Decimal.Precision,
				Scale:       c.Decimal.Scale,
				FromText:    c.DecimalFromText,
				FromNumeric: c.DecimalFromNumeric,
				Rounded:     c.DecimalRounded,
				NonFinite:   c.NonFiniteNulls,
				Limit:       scaledText(scaledLimit(c.Decimal.Precision), c.Decimal.Scale),
				Used:        DecimalHeadroom(c, f.Profiles[c.SourceIndex]),
			}
		}
		rep.Columns = append(rep.Columns, rc)
	}

	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encode schema report: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write schema report %s: %w", path, err)
	}
	return nil
}

// quotedList renders patterns for a message, so an empty or space-carrying
// one is visible rather than vanishing into the sentence.
func quotedList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = strconv.Quote(s)
	}
	return strings.Join(quoted, ", ")
}
