package convert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

// RunQualityGate validates the written Parquet file against the metrics
// collected while converting. parquetPath is read directly, so the caller can
// validate a temporary file before renaming it into place.
func RunQualityGate(inputPath, outputPath, parquetPath string, rs *ResolvedSchema,
	source *Metrics, opts *Options) (*QualityReport, error) {

	rep := &QualityReport{
		Input:      inputPath,
		Output:     outputPath,
		Mode:       opts.Quality.String(),
		Passed:     true,
		RowsSource: source.Rows,
	}

	pqMetrics, pqRows, pqSchema, err := readParquetMetrics(parquetPath, rs, opts)
	if err != nil {
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
func readParquetMetrics(path string, rs *ResolvedSchema, opts *Options) (*Metrics, int64, *arrow.Schema, error) {
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

	mem := memory.NewGoAllocator()
	arrowRdr, err := pqarrow.NewFileReader(rdr, pqarrow.ArrowReadProperties{BatchSize: int64(opts.BatchRows)}, mem)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read Parquet output %s: %w", path, err)
	}
	schema, err := arrowRdr.Schema()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read Parquet schema from %s: %w", path, err)
	}

	metrics := NewMetrics(rs)
	rr, err := arrowRdr.GetRecordReader(context.Background(), nil, nil)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read Parquet records from %s: %w", path, err)
	}
	defer rr.Release()

	hash := opts.Quality == QualityFull
	var rows int64
	for {
		rec, err := rr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Do not treat a read failure as end-of-data: that would compare
			// partial metrics and could pass a corrupt file.
			return nil, 0, nil, fmt.Errorf("read Parquet records from %s: %w", path, err)
		}
		if rec == nil {
			break
		}
		rows += rec.NumRows()
		if err := observeRecord(metrics, rec, rs, hash); err != nil {
			rec.Release()
			return nil, 0, nil, err
		}
		rec.Release()
	}
	metrics.Rows = rows
	return metrics, rows, schema, nil
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
