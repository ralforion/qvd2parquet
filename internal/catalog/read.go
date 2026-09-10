package catalog

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

// ReadFile reads a catalog written by an earlier run back into rows.
//
// It is strict about the schema. A path that holds some other Parquet file is
// a mistake worth reporting, not a catalog to be merged into: silently
// treating it as empty would replace whatever is there with this run alone,
// which is the one outcome an additive catalog exists to prevent.
func ReadFile(path string) ([]Row, error) {
	// The file is opened here rather than by file.OpenParquetFile because that
	// leaves the descriptor open when it fails to read the footer, and this is
	// a path that fails routinely: --catalog-out pointed at something that is
	// not a catalog is a setup mistake to report, not an exceptional event. On
	// Windows an open handle blocks the file from being removed or replaced,
	// which is how it surfaced -- a test could not clean up its own directory.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog %s: %w", path, err)
	}
	defer f.Close()

	rdr, err := file.NewParquetReader(f)
	if err != nil {
		return nil, fmt.Errorf("read catalog %s: %w", path, err)
	}
	defer rdr.Close()

	fr, err := pqarrow.NewFileReader(rdr, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
	if err != nil {
		return nil, fmt.Errorf("read catalog %s: %w", path, err)
	}
	tbl, err := fr.ReadTable(context.Background())
	if err != nil {
		return nil, fmt.Errorf("read catalog %s: %w", path, err)
	}
	defer tbl.Release()

	if err := checkSchema(path, tbl.Schema()); err != nil {
		return nil, err
	}

	rows := make([]Row, 0, tbl.NumRows())
	tr := array.NewTableReader(tbl, 4096)
	defer tr.Release()
	for tr.Next() {
		rec := tr.Record()
		batch, err := rowsFromRecord(path, rec)
		if err != nil {
			return nil, err
		}
		rows = append(rows, batch...)
	}
	if err := tr.Err(); err != nil {
		return nil, fmt.Errorf("read catalog %s: %w", path, err)
	}
	return rows, nil
}

// checkSchema requires every field the catalog writes, by name and type. A
// file carrying extra fields is accepted: a later version adding a column
// should not strand a catalog an earlier one has to read.
func checkSchema(path string, s *arrow.Schema) error {
	for _, want := range Schema.Fields() {
		got, ok := fieldByName(s, want.Name)
		if !ok {
			return fmt.Errorf("read catalog %s: not a catalog: no %q column", path, want.Name)
		}
		if !arrow.TypeEqual(got.Type, want.Type) {
			return fmt.Errorf("read catalog %s: column %q is %s, want %s",
				path, want.Name, got.Type, want.Type)
		}
	}
	return nil
}

func fieldByName(s *arrow.Schema, name string) (arrow.Field, bool) {
	for _, f := range s.Fields() {
		if f.Name == name {
			return f, true
		}
	}
	return arrow.Field{}, false
}

// rowsFromRecord decodes one batch. Every column but symbols is written
// non-null, but a file another tool produced may still hold nulls, so a null
// is read as the zero value rather than refused.
func rowsFromRecord(path string, rec arrow.Record) ([]Row, error) {
	col := func(name string) (arrow.Array, error) {
		for i, f := range rec.Schema().Fields() {
			if f.Name == name {
				return rec.Column(i), nil
			}
		}
		return nil, fmt.Errorf("read catalog %s: no %q column", path, name)
	}
	str := func(name string) (*array.String, error) {
		a, err := col(name)
		if err != nil {
			return nil, err
		}
		s, ok := a.(*array.String)
		if !ok {
			return nil, fmt.Errorf("read catalog %s: column %q is not a string", path, name)
		}
		return s, nil
	}

	var (
		runAt                                    *array.Timestamp
		srcRows                                  *array.Int64
		ordinal                                  *array.Int32
		nullable                                 *array.Boolean
		symbols                                  *array.Int64
		version, source, srcFile, srcTable       *array.String
		outFile, colName, srcColumn, comment     *array.String
		qlikType, pqType, valRange, strat, notes *array.String
	)
	var err error
	if a, e := col("run_at"); e != nil {
		return nil, e
	} else if runAt, _ = a.(*array.Timestamp); runAt == nil {
		return nil, fmt.Errorf("read catalog %s: column %q is not a timestamp", path, "run_at")
	}
	if a, e := col("source_rows"); e != nil {
		return nil, e
	} else if srcRows, _ = a.(*array.Int64); srcRows == nil {
		return nil, fmt.Errorf("read catalog %s: column %q is not an int64", path, "source_rows")
	}
	if a, e := col("ordinal"); e != nil {
		return nil, e
	} else if ordinal, _ = a.(*array.Int32); ordinal == nil {
		return nil, fmt.Errorf("read catalog %s: column %q is not an int32", path, "ordinal")
	}
	if a, e := col("nullable"); e != nil {
		return nil, e
	} else if nullable, _ = a.(*array.Boolean); nullable == nil {
		return nil, fmt.Errorf("read catalog %s: column %q is not a boolean", path, "nullable")
	}
	if a, e := col("symbols"); e != nil {
		return nil, e
	} else if symbols, _ = a.(*array.Int64); symbols == nil {
		return nil, fmt.Errorf("read catalog %s: column %q is not an int64", path, "symbols")
	}
	for _, f := range []struct {
		name string
		dst  **array.String
	}{
		{"tool_version", &version}, {"source", &source}, {"source_file", &srcFile},
		{"source_table", &srcTable}, {"output_file", &outFile}, {"column_name", &colName},
		{"source_column", &srcColumn}, {"comment", &comment}, {"qlik_type", &qlikType},
		{"parquet_type", &pqType}, {"value_range", &valRange}, {"strategy", &strat},
		{"note", &notes},
	} {
		if *f.dst, err = str(f.name); err != nil {
			return nil, err
		}
	}

	text := func(a *array.String, i int) string {
		if a.IsNull(i) {
			return ""
		}
		return a.Value(i)
	}

	out := make([]Row, 0, rec.NumRows())
	for i := 0; i < int(rec.NumRows()); i++ {
		r := Row{
			ToolVersion:  text(version, i),
			Source:       text(source, i),
			SourceFile:   text(srcFile, i),
			SourceTable:  text(srcTable, i),
			OutputFile:   text(outFile, i),
			ColumnName:   text(colName, i),
			SourceColumn: text(srcColumn, i),
			Comment:      text(comment, i),
			QlikType:     text(qlikType, i),
			ParquetType:  text(pqType, i),
			ValueRange:   text(valRange, i),
			Strategy:     text(strat, i),
			Note:         text(notes, i),
		}
		if !runAt.IsNull(i) {
			r.RunAt = time.UnixMicro(int64(runAt.Value(i))).UTC()
		}
		if !srcRows.IsNull(i) {
			r.SourceRows = srcRows.Value(i)
		}
		if !ordinal.IsNull(i) {
			r.Ordinal = ordinal.Value(i)
		}
		if !nullable.IsNull(i) {
			r.Nullable = nullable.Value(i)
		}
		if !symbols.IsNull(i) {
			r.Symbols, r.HasSymbols = symbols.Value(i), true
		}
		out = append(out, r)
	}
	return out, nil
}
