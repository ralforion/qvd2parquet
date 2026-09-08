package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/ralforion/qvd2parquet/internal/parquetwrite"
)

// writeCommented writes a small Parquet file carrying the same field metadata
// the converter attaches, which is what a scan has to recover.
func writeCommented(t *testing.T, path string) {
	t.Helper()
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "Id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "KBETR", Type: arrow.BinaryTypes.String, Nullable: true,
			Metadata: arrow.MetadataFrom(map[string]string{
				"comment":   "Betrag",
				"qvd.field": "A057-||-KBETR-||-Betrag",
			})},
	}, nil)

	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()
	b.Field(0).(*array.Int64Builder).Append(1)
	b.Field(1).(*array.StringBuilder).Append("x")
	rec := b.NewRecord()
	defer rec.Release()

	codec, err := parquetwrite.ParseCompression("snappy")
	if err != nil {
		t.Fatalf("compression: %v", err)
	}
	w, err := parquetwrite.Create(path, schema, parquetwrite.Options{Compression: codec}, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := w.Write(rec); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestScanRecoversComments is the whole reason the scan exists: a conversion
// run without --catalog-out is not lost, because the comment is in the file.
func TestScanRecoversComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a057.parquet")
	writeCommented(t, path)

	rows, err := ScanFile(path)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[1].Comment != "Betrag" {
		t.Errorf("comment = %q, want %q", rows[1].Comment, "Betrag")
	}
	if rows[1].SourceColumn != "A057-||-KBETR-||-Betrag" {
		t.Errorf("source column = %q, want the original QVD name", rows[1].SourceColumn)
	}
	if rows[1].Source != SourceParquet {
		t.Errorf("source = %q, want %q", rows[1].Source, SourceParquet)
	}
	// A scan cannot know the symbol count, and saying zero would be a lie a
	// query could not distinguish from a genuinely empty column.
	if rows[1].HasSymbols {
		t.Error("a scanned row claims to know its symbol count")
	}
	// A column with no metadata of its own still reports a source column,
	// because the writer records the original name only when it differs.
	if rows[0].SourceColumn != "Id" {
		t.Errorf("uncommented source column = %q, want %q", rows[0].SourceColumn, "Id")
	}
}

// TestScanIgnoresForeignParquet checks that a file written by something else
// scans cleanly rather than failing. Most Parquet in the world carries no
// field metadata at all.
func TestScanIgnoresForeignParquet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.parquet")
	schema := arrow.NewSchema([]arrow.Field{{Name: "a", Type: arrow.PrimitiveTypes.Int64}}, nil)
	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()
	b.Field(0).(*array.Int64Builder).Append(7)
	rec := b.NewRecord()
	defer rec.Release()

	codec, _ := parquetwrite.ParseCompression("snappy")
	w, err := parquetwrite.Create(path, schema, parquetwrite.Options{Compression: codec}, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := w.Write(rec); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rows, err := ScanFile(path)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(rows) != 1 || rows[0].Comment != "" {
		t.Fatalf("got %+v, want one row with no comment", rows)
	}
}

// TestWriterRoundTrip proves the catalog is readable as a table, which is the
// only thing a query engine will ever do with it.
func TestWriterRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.parquet")

	w, err := NewWriter(path, "test", false)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	w.Add([]Row{{
		Source: SourceQVD, SourceFile: "A057.qvd", SourceTable: "A057",
		Ordinal: 1, ColumnName: "KBETR", Comment: "Betrag",
		QlikType: "REAL", ParquetType: "decimal(4, 2)",
		Symbols: 12, HasSymbols: true,
	}})
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reading the schema back is enough to prove the table is well formed and
	// carries every column, which is what a promoted dataset depends on.
	cols, err := ScanFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(cols) != len(Schema.Fields()) {
		t.Fatalf("catalog has %d columns, schema declares %d", len(cols), len(Schema.Fields()))
	}
	if w.Rows()[0].ToolVersion != "test" {
		t.Errorf("tool version = %q, want %q", w.Rows()[0].ToolVersion, "test")
	}
	if w.Rows()[0].RunAt.IsZero() {
		t.Error("row carries no run timestamp")
	}
}

// TestEmptyRunStillWritesACatalog keeps "converted nothing" distinguishable
// from "did not run" for a scheduled job.
func TestEmptyRunStillWritesACatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.parquet")
	w, err := NewWriter(path, "test", false)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no catalog written: %v", err)
	}
}

// TestNewWriterRefusesAnExistingFile matches every other output the tool
// writes: nothing is replaced without --force.
func TestNewWriterRefusesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.parquet")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(path, "test", false); err == nil {
		t.Fatal("expected a refusal without --force")
	}
	if _, err := NewWriter(path, "test", true); err != nil {
		t.Fatalf("--force should allow it: %v", err)
	}
}

// TestFindParquet checks the directory expansion, including that it leaves
// everything that is not Parquet alone.
func TestFindParquet(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCommented(t, filepath.Join(dir, "a.parquet"))
	writeCommented(t, filepath.Join(nested, "b.parquet"))
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	flat, err := FindParquet([]string{dir}, false)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(flat) != 1 {
		t.Errorf("non-recursive found %v, want only the top-level file", flat)
	}

	deep, err := FindParquet([]string{dir}, true)
	if err != nil {
		t.Fatalf("find recursive: %v", err)
	}
	if len(deep) != 2 {
		t.Errorf("recursive found %v, want both files", deep)
	}

	if _, err := FindParquet([]string{filepath.Join(dir, "missing")}, false); err == nil {
		t.Error("expected an error for a path that does not exist")
	}
}
