package convert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

// RunQualityGate validates the written Parquet file against the metrics
// collected while converting. parquetPath is read directly, so the caller can
// validate a temporary file before renaming it into place.
// logf may be nil. When it is not, and --progress is enabled, the read-back
// reports rows verified as it goes: on a wide file the gate runs for minutes
// with nothing else to show for itself, which reads as a hang.
func RunQualityGate(ctx context.Context, inputPath, outputPath, parquetPath string,
	rs *ResolvedSchema, source *Metrics, opts *Options, logf Logf) (*QualityReport, error) {

	rep := &QualityReport{
		Input:      inputPath,
		Output:     outputPath,
		Mode:       opts.Quality.String(),
		Passed:     true,
		RowsSource: source.Rows,
	}

	pqMetrics, pqRows, pqSchema, err := readParquetMetrics(ctx, parquetPath, rs, opts, logf)
	if err != nil {
		// A stopped run is not a failed check. Folding it into the report
		// would tell the user their output did not match its input, when the
		// truth is that nobody finished looking.
		if errors.Is(err, ErrCanceled) {
			return nil, err
		}
		rep.Passed = false
		rep.Errors = append(rep.Errors, err.Error())
		return rep, nil
	}
	rep.RowsParquet = pqRows

	if pqRows != source.Rows {
		rep.Passed = false
		rep.Errors = append(rep.Errors,
			fmt.Sprintf("row count differs: source %d, Parquet %d", source.Rows, pqRows))
	}
	if err := compareSchemas(rs.Arrow, pqSchema); err != nil {
		rep.Passed = false
		rep.Errors = append(rep.Errors, err.Error())
		// Column-level comparison is meaningless once the schema diverges.
		return rep, nil
	}

	for i := range source.Columns {
		sm, pm := source.Columns[i], pqMetrics.Columns[i]
		cmp := ColumnComparison{
			Name:    sm.Name,
			Type:    sm.Type,
			Source:  sm.Stats(),
			Parquet: pm.Stats(),
			Passed:  true,
		}
		cmp.Errors = compareColumn(sm, pm, opts)
		if len(cmp.Errors) > 0 {
			cmp.Passed = false
			rep.Passed = false
		}
		rep.Columns = append(rep.Columns, cmp)
	}
	return rep, nil
}

// compareColumn applies the checks enabled by the configured quality mode.
func compareColumn(src, pq *ColumnMetrics, opts *Options) []string {
	var errs []string

	// basic: null counts.
	if src.Nulls != pq.Nulls {
		errs = append(errs, fmt.Sprintf("null count differs: source %d, Parquet %d", src.Nulls, pq.Nulls))
	}
	if src.NonNulls != pq.NonNulls {
		errs = append(errs, fmt.Sprintf("non-null count differs: source %d, Parquet %d", src.NonNulls, pq.NonNulls))
	}
	if opts.Quality == QualityBasic {
		return errs
	}

	// numeric: aggregates.
	ss, ps := src.Stats(), pq.Stats()
	switch src.Strategy {
	case StrategyInt64, StrategyDate32, StrategyTimestampMicros, StrategyTimeMillis, StrategyDecimal:
		// Integers and decimals compare exactly.
		for _, m := range []struct{ name, a, b string }{
			{"sum", ss.Sum, ps.Sum}, {"min", ss.Min, ps.Min}, {"max", ss.Max, ps.Max},
		} {
			if m.a != m.b {
				errs = append(errs, fmt.Sprintf("%s differs: source %s, Parquet %s", m.name, m.a, m.b))
			}
		}
	case StrategyFloat64:
		rel, abs := opts.QualityRelTolerance, opts.QualityAbsTolerance
		if !floatsClose(ss.sumF, ps.sumF, rel, abs) {
			errs = append(errs, fmt.Sprintf("sum differs beyond tolerance: source %s, Parquet %s", ss.Sum, ps.Sum))
		}
		if !floatsClose(ss.sumSqF, ps.sumSqF, rel, abs) {
			errs = append(errs, fmt.Sprintf("sum of squares differs beyond tolerance: source %s, Parquet %s", ss.SumSq, ps.SumSq))
		}
		// Min and max are values that round-trip exactly through Parquet.
		if ss.Min != ps.Min {
			errs = append(errs, fmt.Sprintf("min differs: source %s, Parquet %s", ss.Min, ps.Min))
		}
		if ss.Max != ps.Max {
			errs = append(errs, fmt.Sprintf("max differs: source %s, Parquet %s", ss.Max, ps.Max))
		}
	}
	if opts.Quality != QualityFull {
		return errs
	}

	// full: order-independent value fingerprints.
	if ss.Hash != ps.Hash {
		errs = append(errs, fmt.Sprintf("value fingerprint differs: source %s, Parquet %s",
			shortHash(ss.Hash), shortHash(ps.Hash)))
	}
	return errs
}

func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16] + "..."
	}
	if h == "" {
		return "(none)"
	}
	return h
}

// compareSchemas checks that the Parquet file's Arrow schema matches what was
// resolved, field by field.
func compareSchemas(want, got *arrow.Schema) error {
	if len(want.Fields()) != len(got.Fields()) {
		return fmt.Errorf("column count differs: schema has %d, Parquet has %d",
			len(want.Fields()), len(got.Fields()))
	}
	for i, wf := range want.Fields() {
		gf := got.Field(i)
		if wf.Name != gf.Name {
			return fmt.Errorf("column %d name differs: schema %q, Parquet %q", i, wf.Name, gf.Name)
		}
		if !arrow.TypeEqual(wf.Type, gf.Type) {
			return fmt.Errorf("column %q type differs: schema %s, Parquet %s", wf.Name, wf.Type, gf.Type)
		}
	}
	return nil
}

// readParquetMetrics reopens the written Parquet file and recomputes the same
// metrics from its data, never reusing the in-memory batches from the writer.
//
// Row groups are read in parallel. They are independent, and both the metrics
// and the full mode's fingerprint merge in any order -- that is what lets
// parallel decoding validate without reordering -- so the gate can use the
// machine the same way the conversion does. Each worker opens its own handle,
// for the reason decode workers do: Windows serializes concurrent ReadAt on a
// shared one.
func readParquetMetrics(ctx context.Context, path string, rs *ResolvedSchema, opts *Options, logf Logf) (*Metrics, int64, *arrow.Schema, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("open Parquet output %s: %w", path, err)
	}
	defer f.Close()

	rdr, err := file.NewParquetReader(f)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("open Parquet output %s: %w", path, err)
	}
	defer rdr.Close()

	arrowRdr, err := pqarrow.NewFileReader(rdr,
		pqarrow.ArrowReadProperties{BatchSize: int64(opts.BatchRows)}, memory.NewGoAllocator())
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read Parquet output %s: %w", path, err)
	}
	schema, err := arrowRdr.Schema()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read Parquet schema from %s: %w", path, err)
	}

	groups := rdr.NumRowGroups()
	totalRows := rdr.NumRows()
	if groups == 0 {
		return NewMetrics(rs), 0, schema, nil
	}

	// Contiguous blocks rather than round robin, so each worker reads forwards
	// through its own span of the file.
	workers := WorkerCount(opts.Workers, groups)
	per := (groups + workers - 1) / workers
	var assignments [][]int
	for lo := 0; lo < groups; lo += per {
		hi := lo + per
		if hi > groups {
			hi = groups
		}
		block := make([]int, 0, hi-lo)
		for g := lo; g < hi; g++ {
			block = append(block, g)
		}
		assignments = append(assignments, block)
	}

	// Progress is aggregated across workers, so it counts the whole gate
	// rather than whichever worker happens to report.
	hash := opts.Quality == QualityFull
	gateStart := time.Now()
	var progMu sync.Mutex
	var seen, nextProgress int64 = 0, opts.ProgressEvery
	report := func(n int64) {
		if logf == nil || opts.ProgressEvery <= 0 {
			return
		}
		progMu.Lock()
		defer progMu.Unlock()
		seen += n
		if seen < nextProgress {
			return
		}
		el := time.Since(gateStart)
		logf("quality gate %s: verified %d/%d rows in %s (%.0f rows/s)",
			opts.Quality, seen, totalRows, el.Round(time.Millisecond),
			float64(seen)/el.Seconds())
		for seen >= nextProgress {
			nextProgress += opts.ProgressEvery
		}
	}

	partials := make([]*Metrics, len(assignments))
	counts := make([]int64, len(assignments))
	errs := make([]error, len(assignments))
	var wg sync.WaitGroup
	for i, block := range assignments {
		wg.Add(1)
		go func(i int, block []int) {
			defer wg.Done()
			partials[i], counts[i], errs[i] = readRowGroups(ctx, path, block, rs, opts, hash, report)
		}(i, block)
	}
	wg.Wait()

	metrics := NewMetrics(rs)
	var rows int64
	for i := range assignments {
		if errs[i] != nil {
			return nil, 0, nil, errs[i]
		}
		// Merged in worker order, so a report does not depend on which
		// goroutine finished first.
		metrics.Merge(partials[i])
		rows += counts[i]
	}
	metrics.Rows = rows
	return metrics, rows, schema, nil
}

// readRowGroups computes metrics over one worker's span of row groups, through
// a handle of its own.
func readRowGroups(ctx context.Context, path string, groups []int, rs *ResolvedSchema,
	opts *Options, hash bool, report func(int64)) (*Metrics, int64, error) {

	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open Parquet output %s: %w", path, err)
	}
	defer f.Close()

	rdr, err := file.NewParquetReader(f)
	if err != nil {
		return nil, 0, fmt.Errorf("open Parquet output %s: %w", path, err)
	}
	defer rdr.Close()

	arrowRdr, err := pqarrow.NewFileReader(rdr,
		pqarrow.ArrowReadProperties{BatchSize: int64(opts.BatchRows)}, memory.NewGoAllocator())
	if err != nil {
		return nil, 0, fmt.Errorf("read Parquet output %s: %w", path, err)
	}
	rr, err := arrowRdr.GetRecordReader(ctx, nil, groups)
	if err != nil {
		return nil, 0, fmt.Errorf("read Parquet records from %s: %w", path, err)
	}
	defer rr.Release()

	metrics := NewMetrics(rs)
	var rows int64
	for {
		// Ctrl-C during the gate must stop it. On a wide file the read-back
		// runs for minutes, and a signal that appears to do nothing at all is
		// worse than no signal handling.
		if err := ctx.Err(); err != nil {
			return nil, 0, fmt.Errorf("%w while verifying the output", ErrCanceled)
		}
		rec, err := rr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Do not treat a read failure as end-of-data: that would compare
			// partial metrics and could pass a corrupt file.
			return nil, 0, fmt.Errorf("read Parquet records from %s: %w", path, err)
		}
		if rec == nil {
			break
		}
		// Read the count before releasing: the record's buffers are gone
		// afterwards.
		n := rec.NumRows()
		rows += n
		if err := observeRecord(metrics, rec, rs, hash); err != nil {
			rec.Release()
			return nil, 0, err
		}
		rec.Release()
		report(n)
	}
	metrics.Rows = rows
	return metrics, rows, nil
}

// observeRecord folds one Arrow record read back from Parquet into the metrics,
// canonicalizing values exactly as the source side did.
func observeRecord(m *Metrics, rec arrow.Record, rs *ResolvedSchema, hash bool) error {
	n := int(rec.NumRows())
	for ci := range rs.Columns {
		col := rec.Column(ci)
		cm := m.Columns[ci]
		strategy := rs.Columns[ci].Strategy
		for r := 0; r < n; r++ {
			v, err := arrowValue(col, r, strategy)
			if err != nil {
				return fmt.Errorf("column %q row %d: %w", rs.Columns[ci].Name, r, err)
			}
			cm.Observe(v, hash)
		}
	}
	return nil
}

// arrowValue extracts one cell from a Parquet-read array into the same Value
// shape the converter produced.
func arrowValue(col arrow.Array, row int, strategy ValueStrategy) (Value, error) {
	if col.IsNull(row) {
		return Value{Null: true}, nil
	}
	switch a := col.(type) {
	case *array.String:
		return Value{Str: a.Value(row)}, nil
	case *array.LargeString:
		return Value{Str: a.Value(row)}, nil
	case *array.Int64:
		return Value{Int: a.Value(row)}, nil
	case *array.Int32:
		return Value{Int: int64(a.Value(row))}, nil
	case *array.Float64:
		return Value{Float: a.Value(row)}, nil
	case *array.Date32:
		return Value{Int: int64(a.Value(row))}, nil
	case *array.Timestamp:
		return Value{Int: int64(a.Value(row))}, nil
	case *array.Time32:
		return Value{Int: int64(a.Value(row))}, nil
	case *array.Decimal128:
		return Value{Scaled: a.Value(row).BigInt()}, nil
	}
	return Value{}, fmt.Errorf("unsupported Parquet array type %s for %v", col.DataType(), strategy)
}
