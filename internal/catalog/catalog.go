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
	"errors"
	"fmt"
	"os"
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

	// stored is the catalog already at path, read once when the writer opens
	// so a run cannot spend an hour converting and then discover the file it
	// has to merge into is unreadable. merging says it was there to read.
	stored  []Row
	merging bool

	// canon memoizes the canonical form of each output path a run records, so
	// a batch of two hundred files at two hundred columns resolves two hundred
	// paths rather than forty thousand.
	canon map[string]string
}

// NewWriter prepares a catalog for one run. The timestamp is taken once here,
// so every row of a batch shares it and a run is a single value to group by.
//
// A catalog already at the path is read and merged into, rather than refused.
// A catalog describes tables, and a run describes the tables it converted:
// refusing the second one would mean a nightly job over a subset either fails
// or, with --force, replaces the record of two hundred tables with the record
// of the one it touched. --force still means replace, for the run that wants
// to start the catalog over.
//
// Reading here rather than at Close is deliberate. An unreadable catalog is a
// setup mistake, and the run should learn about it before it converts a folder
// rather than after.
func NewWriter(path, toolVersion string, force bool) (*Writer, error) {
	w := &Writer{path: path, force: force, runAt: time.Now().UTC(), version: toolVersion}
	if force {
		return w, nil
	}
	switch _, err := os.Stat(path); {
	case err == nil:
		stored, err := ReadFile(path)
		if err != nil {
			return nil, err
		}
		w.stored, w.merging = stored, true
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("stat catalog %s: %w", path, err)
	}
	return w, nil
}

// Merging reports whether a catalog was already at the path and will be
// merged into.
func (w *Writer) Merging() bool {
	if w == nil {
		return false
	}
	return w.merging
}

// StoredLen is how many rows the catalog held before this run.
func (w *Writer) StoredLen() int {
	if w == nil {
		return 0
	}
	return len(w.stored)
}

// Begin marks that an input has been accounted for, which is what licenses
// Close to write.
//
// A catalog describes files. Until at least one input has been converted,
// skipped or scanned there is nothing to describe, and writing then would
// replace a real catalog with the record of a run that got nowhere: a missing
// input, an output that could not be created, a setup step that failed after
// the writer was opened. No amount of care about the order of the caller's
// setup steps prevents that, because the next thing able to fail always lands
// somewhere new. Only counting what was actually accounted for does.
//
// Add calls this, so the ordinary paths need not. It is separate because a
// file can legitimately account for no columns -- every one excluded by
// --columns, say -- and that is still a file the catalog has described.
func (w *Writer) Begin() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started = true
}

// Started reports whether any input has been accounted for.
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
//
// Adding an empty slice is meaningful: it says a file was accounted for and
// contributed no columns, which is not the same as never having looked.
func (w *Writer) Add(rows []Row) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started = true
	for _, r := range rows {
		r.RunAt = w.runAt
		r.ToolVersion = w.version
		r.OutputFile = w.canonical(r.OutputFile)
		w.rows = append(w.rows, r)
	}
}

// canonical resolves an output path to the one spelling that identifies the
// file from any working directory.
//
// It is stored that way, not merely compared that way, because comparing is
// not enough. A row recorded as "out/orders.parquet" says nothing outside the
// directory the run was launched from, and a later run resolving it from
// somewhere else gets a path to a file that was never there -- indistinguishable
// from a path to a real, different file. Two runs of the same job from
// different directories would then either miss each other or, worse, be
// reconciled on the strength of a shared file name, which is no evidence of
// identity at all: original/orders.parquet and other/orders.parquet are two
// files.
//
// Rows written before this carry whatever spelling they were given. They still
// match a later run launched from the same directory, and where they do not
// the run records the table again rather than claiming the wrong one.
//
// Called under the writer's lock, which is also what guards the memo.
func (w *Writer) canonical(path string) string {
	if path == "" {
		return ""
	}
	if c, ok := w.canon[path]; ok {
		return c
	}
	c := canonicalOutput(path)
	if w.canon == nil {
		w.canon = map[string]string{}
	}
	w.canon[path] = c
	return c
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
// A writer that accounted for no input at all writes nothing and leaves
// whatever is at its path untouched, since there was nothing to describe.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started {
		return nil
	}

	// A run's rows win over the stored ones for the columns they describe, so
	// a changed type is updated; every other stored row is carried across.
	rows := w.rows
	if w.merging {
		rows = Merge(w.stored, w.rows)
	}

	rec, err := buildRecord(rows)
	if err != nil {
		return err
	}
	defer rec.Release()

	codec, err := parquetwrite.ParseCompression("zstd")
	if err != nil {
		return err
	}
	// The merged file replaces the one it was read from. That is not the
	// overwrite --force guards: the existing rows are in the record about to
	// be written, and the writer renames a temporary into place, so the
	// catalog on disk is intact until the new one is complete.
	pw, err := parquetwrite.Create(w.path, Schema, parquetwrite.Options{
		Compression:  codec,
		RowGroupRows: int64(len(rows)) + 1,
	}, w.force || w.merging)
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
