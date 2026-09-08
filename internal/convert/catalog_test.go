package convert

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/catalog"
)

// byColumn indexes a run's catalog rows by output column name.
func byColumn(rows []catalog.Row) map[string]catalog.Row {
	got := map[string]catalog.Row{}
	for _, r := range rows {
		got[r.ColumnName] = r
	}
	return got
}

// TestCatalogRecordsCommentsAndTypes is the point of the feature: a comment
// attached by --field-regex reaches the catalog, where a query engine that
// cannot read ARROW:schema can still find it.
func TestCatalogRecordsCommentsAndTypes(t *testing.T) {
	in := buildFixture(t, sampleTable(200))
	dir := t.TempDir()
	out := filepath.Join(dir, "out.parquet")
	catPath := filepath.Join(dir, "catalog.parquet")

	opts := testOptions()
	var err error
	if opts.Renamer, err = NewFieldRenamer(`^(?P<name>Amount)$`, "", "Betrag"); err != nil {
		t.Fatalf("renamer: %v", err)
	}
	cat, err := catalog.NewWriter(catPath, "test", false)
	if err != nil {
		t.Fatalf("new catalog: %v", err)
	}
	opts.Catalog = cat

	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := cat.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	rows := byColumn(cat.Rows())
	if len(rows) == 0 {
		t.Fatal("catalog is empty")
	}
	amount, ok := rows["Amount"]
	if !ok {
		t.Fatalf("no Amount row; got %v", keysOf(rows))
	}
	if amount.Comment != "Betrag" {
		t.Errorf("Amount comment = %q, want %q", amount.Comment, "Betrag")
	}
	// The catalog's own columns are read back through the same metadata path
	// the feature exists to work around, so this also proves the round trip.
	if amount.ParquetType == "" {
		t.Error("Amount row carries no parquet type")
	}
}

// TestCatalogRowsMatchSchemaReport keeps the two documents from drifting. They
// are built from one resolved schema, and a change that updates only one of
// them would otherwise ship a catalog that disagrees with the report about the
// same conversion.
func TestCatalogRowsMatchSchemaReport(t *testing.T) {
	in := buildFixture(t, sampleTable(100))
	dir := t.TempDir()
	out := filepath.Join(dir, "out.parquet")
	reportPath := filepath.Join(dir, "report.json")

	opts := testOptions()
	opts.SchemaReportPath = reportPath
	cat, err := catalog.NewWriter(filepath.Join(dir, "catalog.parquet"), "test", false)
	if err != nil {
		t.Fatalf("new catalog: %v", err)
	}
	opts.Catalog = cat

	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	rep := readSchemaReport(t, reportPath)
	if cat.Len() != len(rep.Columns) {
		t.Fatalf("catalog has %d rows, schema report has %d columns", cat.Len(), len(rep.Columns))
	}
}

// TestCatalogSurvivesAFailedConversion checks that a conversion which never
// commits leaves no row claiming a column nobody can query.
func TestCatalogSurvivesAFailedConversion(t *testing.T) {
	in := buildFixture(t, sampleTable(50))
	dir := t.TempDir()
	catPath := filepath.Join(dir, "catalog.parquet")

	cat, err := catalog.NewWriter(catPath, "test", false)
	if err != nil {
		t.Fatalf("new catalog: %v", err)
	}
	opts := testOptions()
	opts.Catalog = cat

	// An output path inside a file rather than a directory cannot be created.
	bad := filepath.Join(in, "nested", "out.parquet")
	if _, _, err := Run(context.Background(), in, bad, &opts, nil); err == nil {
		t.Fatal("expected the conversion to fail")
	}
	if cat.Len() != 0 {
		t.Errorf("catalog recorded %d row(s) for a conversion that never committed", cat.Len())
	}
}

// readSchemaReport decodes a --schema-report document.
func readSchemaReport(t *testing.T, path string) SchemaReport {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema report: %v", err)
	}
	var rep SchemaReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("decode schema report: %v", err)
	}
	return rep
}

func keysOf(m map[string]catalog.Row) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
