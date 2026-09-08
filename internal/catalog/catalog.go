// Package catalog writes a column-grain record of what a run produced: one
// row per output column, carrying the field comment and the type decision
// alongside the names on both sides of the conversion.
//
// It exists because the metadata qvd2parquet attaches to a column survives
// only in Arrow's ARROW:schema entry, which the Arrow readers decode and the
// query engines do not. Dremio in particular has no column description field
// at all, so a comment reaches a reader there only as data. A catalog table is
// that data: queryable, joinable against INFORMATION_SCHEMA, and durable in a
// way a catalog-side annotation is not.
package catalog

import (
	"fmt"
	"sync"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/ralforion/qvd2parquet/internal/parquetwrite"
)

// SourceQVD and SourceParquet say where a row's facts came from. A row read
// back from a finished Parquet file knows the column's name, type and comment
// but nothing about the QVD behind it, so the two are distinguished rather
// than left to be inferred from which fields happen to be empty.
const (
	SourceQVD     = "qvd"
	SourceParquet = "parquet"
)

// Row is one output column.
//
// Every field is written on every row, including the empty ones, for the same
// reason the JSON log writes its empty fields: a column absent whenever no row
// happened to carry it turns `where comment <> ”` into a binding error on
// exactly the run where nothing was commented. Only Symbols is nullable, and
// only because zero symbols is a real value that a scan cannot distinguish
// from not knowing.
type Row struct {
	RunAt       time.Time
	ToolVersion string
	Source      string

	SourceFile  string
	SourceTable string
	SourceRows  int64
	OutputFile  string

	Ordinal      int32
	ColumnName   string
	SourceColumn string
	Comment      string

	QlikType    string
	ParquetType string
	Nullable    bool
	Symbols     int64
	HasSymbols  bool
	ValueRange  string
	Strategy    string
	Note        string
}

// Schema is the catalog table's Arrow schema. It is fixed: a reader that
// promotes the output as a dataset should not have to rediscover the columns
// after every run.
var Schema = arrow.NewSchema([]arrow.Field{
	{Name: "run_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}},
	{Name: "tool_version", Type: arrow.BinaryTypes.String},
	{Name: "source", Type: arrow.BinaryTypes.String},
	{Name: "source_file", Type: arrow.BinaryTypes.String},
	{Name: "source_table", Type: arrow.BinaryTypes.String},
	{Name: "source_rows", Type: arrow.PrimitiveTypes.Int64},
	{Name: "output_file", Type: arrow.BinaryTypes.String},
	{Name: "ordinal", Type: arrow.PrimitiveTypes.Int32},
	{Name: "column_name", Type: arrow.BinaryTypes.String},
	{Name: "source_column", Type: arrow.BinaryTypes.String},
	{Name: "comment", Type: arrow.BinaryTypes.String},
	{Name: "qlik_type", Type: arrow.BinaryTypes.String},
	{Name: "parquet_type", Type: arrow.BinaryTypes.String},
	{Name: "nullable", Type: arrow.FixedWidthTypes.Boolean},
	{Name: "symbols", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	{Name: "value_range", Type: arrow.BinaryTypes.String},
	{Name: "strategy", Type: arrow.BinaryTypes.String},
	{Name: "note", Type: arrow.BinaryTypes.String},
}, nil)

// Writer accumulates rows across a run and writes them as one Parquet file.
//
// Rows are held in memory rather than streamed: a batch of 200 files at 200
// columns each is 40,000 rows of mostly short strings, and buffering means a
// run that fails partway leaves no half-written catalog claiming to describe
// the whole folder.
type Writer struct {
	mu      sync.Mutex
	rows    []Row
	path    string
	force   bool
	started bool
	runAt   time.Time
	version string
}

// NewWriter prepares a catalog for one run. The timestamp is taken once here,
// so every row of a batch shares it and a run is a single value to group by.
func NewWriter(path, toolVersion string, force bool) (*Writer, error) {
	// Fail on an existing file now rather than after converting the folder.
	if err := parquetwrite.CheckOutput(path, force); err != nil {
		return nil, err
	}
	return &Writer{path: path, force: force, runAt: time.Now().UTC(), version: toolVersion}, nil
}

// Begin marks the run as started, which is what licenses Close to write.
//
// A catalog describes a run. Between opening the writer and starting the
// conversion the caller may still fail -- another output cannot be created,
// some other guard refuses -- and a catalog written then would replace a real
// one with the record of a run that never happened. Nothing about the ordering
// of the caller's setup steps prevents that; only asking the run to say it
// began does.
//
// This is deliberately not the same thing as converting something. A run that
// began and converted nothing does write its catalog, empty, so that a
// scheduled job can tell it apart from a run that never started.
func (w *Writer) Begin() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started = true
}

// Started reports whether Begin was called.
func (w *Writer) Started() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.started
}

// Path is where the catalog will be written.
func (w *Writer) Path() string {
	if w == nil {
		return ""
	}
	return w.path
}

// RunAt is the run's timestamp, stamped on every row.
func (w *Writer) RunAt() time.Time {
	if w == nil {
		return time.Time{}
	}
	return w.runAt
}

// Add records the columns of one file. It is safe to call from the several
// goroutines a --file-workers batch runs.
func (w *Writer) Add(rows []Row) {
	if w == nil || len(rows) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range rows {
		r.RunAt = w.runAt
		r.ToolVersion = w.version
		w.rows = append(w.rows, r)
	}
}

// Len is how many rows have been recorded.
func (w *Writer) Len() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.rows)
}

// Rows returns a copy of what has been recorded so far, so a caller can assert
// on the run's columns without reading the file back.
func (w *Writer) Rows() []Row {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Row(nil), w.rows...)
}

// Close writes the catalog. A run that recorded no columns still writes the
// file, empty, so a scheduled job can tell "converted nothing" from "did not
// run" without a special case.
//
// A writer that was never begun writes nothing at all and leaves whatever is
// at its path untouched, since there was no run to describe.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started {
		return nil
	}

	rec, err := buildRecord(w.rows)
	if err != nil {
		return err
	}
	defer rec.Release()

	codec, err := parquetwrite.ParseCompression("zstd")
	if err != nil {
		return err
	}
	pw, err := parquetwrite.Create(w.path, Schema, parquetwrite.Options{
		Compression:  codec,
		RowGroupRows: int64(len(w.rows)) + 1,
	}, w.force)
	if err != nil {
		return err
	}
	// The writer builds a temporary file and renames it into place, so every
	// path out of here that is not a successful commit has to abort. The
	// rename is one of them: it fails when the destination cannot be replaced,
	// and returning that error alone left the temporary file sitting next to
	// the path the run had just reported it could not write.
	committed := false
	defer func() {
		if !committed {
			_ = pw.Abort()
		}
	}()

	if err := pw.Write(rec); err != nil {
		return err
	}
	if err := pw.Close(); err != nil {
		return err
	}
	if err := pw.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// buildRecord turns the accumulated rows into a single Arrow record.
func buildRecord(rows []Row) (arrow.Record, error) {
	b := array.NewRecordBuilder(memory.DefaultAllocator, Schema)
	defer b.Release()

	if n := len(Schema.Fields()); n != 18 {
		return nil, fmt.Errorf("catalog schema has %d fields, builders expect 18", n)
	}
	var (
		runAt     = b.Field(0).(*array.TimestampBuilder)
		version   = b.Field(1).(*array.StringBuilder)
		source    = b.Field(2).(*array.StringBuilder)
		srcFile   = b.Field(3).(*array.StringBuilder)
		srcTable  = b.Field(4).(*array.StringBuilder)
		srcRows   = b.Field(5).(*array.Int64Builder)
		outFile   = b.Field(6).(*array.StringBuilder)
		ordinal   = b.Field(7).(*array.Int32Builder)
		colName   = b.Field(8).(*array.StringBuilder)
		srcColumn = b.Field(9).(*array.StringBuilder)
		comment   = b.Field(10).(*array.StringBuilder)
		qlikType  = b.Field(11).(*array.StringBuilder)
		pqType    = b.Field(12).(*array.StringBuilder)
		nullable  = b.Field(13).(*array.BooleanBuilder)
		symbols   = b.Field(14).(*array.Int64Builder)
		valRange  = b.Field(15).(*array.StringBuilder)
		strategy  = b.Field(16).(*array.StringBuilder)
		note      = b.Field(17).(*array.StringBuilder)
	)

	for _, r := range rows {
		runAt.Append(arrow.Timestamp(r.RunAt.UnixMicro()))
		version.Append(r.ToolVersion)
		source.Append(r.Source)
		srcFile.Append(r.SourceFile)
		srcTable.Append(r.SourceTable)
		srcRows.Append(r.SourceRows)
		outFile.Append(r.OutputFile)
		ordinal.Append(r.Ordinal)
		colName.Append(r.ColumnName)
		srcColumn.Append(r.SourceColumn)
		comment.Append(r.Comment)
		qlikType.Append(r.QlikType)
		pqType.Append(r.ParquetType)
		nullable.Append(r.Nullable)
		if r.HasSymbols {
			symbols.Append(r.Symbols)
		} else {
			symbols.AppendNull()
		}
		valRange.Append(r.ValueRange)
		strategy.Append(r.Strategy)
		note.Append(r.Note)
	}
	return b.NewRecord(), nil
}
