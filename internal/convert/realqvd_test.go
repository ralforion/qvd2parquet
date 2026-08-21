package convert

import (
	"context"
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

func realFixture(name string) string { return filepath.Join("..", "..", "testdata", "real", name) }

// TestRealQVDProducts converts a QVD written by QlikView (build 11282) and
// compares every cell against the CSV the Java reference reader produced from
// the same file. This validates the decoder against real QlikView output
// rather than against this project's own synthetic encoder.
//
// The fixture is a useful stress case: its fields are not stored in bit-offset
// order (0, 53, 8, ...), and Listenpreis genuinely mixes integer and double
// symbols in one column.
func TestRealQVDProducts(t *testing.T) {
	in := realFixture("products.qvd")
	out := filepath.Join(t.TempDir(), "products.parquet")

	opts := testOptions()
	opts.Quality = QualityFull
	stats, report, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("full quality gate failed: %+v", report)
	}
	if stats.Rows != 77 || stats.Columns != 9 {
		t.Fatalf("got %d rows and %d columns, want 77 and 9", stats.Rows, stats.Columns)
	}

	// Listenpreis mixes 25 integer and 35 double symbols, so it must widen.
	types := map[string]string{}
	for _, c := range report.Columns {
		types[c.Name] = c.Type
	}
	// The default is --numeric-promote=decimal, so the two price columns
	// resolve to exact decimals even though QlikView declares them REAL.
	for name, want := range map[string]string{
		"Einkaufspreis": "decimal(5, 2)", // REAL, scale inferred from values
		"KategorieNr":   "int64",         // INTEGER
		"Produktname":   "utf8",          // text
		"Listenpreis":   "decimal(5, 2)", // int + double, scale inferred
	} {
		if types[name] != want {
			t.Errorf("column %q resolved to %q, want %q", name, types[name], want)
		}
	}

	got := readParquetRows(t, out)
	want := readReferenceCSV(t, realFixture("products.expected.csv"))
	if len(got) != len(want) {
		t.Fatalf("Parquet has %d rows, the reference CSV has %d", len(got), len(want))
	}

	// Row order is not preserved by design, so index both sides by ProduktNr.
	byKey := func(rows []map[string]string) map[string]map[string]string {
		m := make(map[string]map[string]string, len(rows))
		for _, r := range rows {
			m[r["ProduktNr"]] = r
		}
		return m
	}
	gotByKey, wantByKey := byKey(got), byKey(want)
	if len(gotByKey) != len(want) {
		t.Fatalf("ProduktNr is not unique in the Parquet output: %d distinct keys for %d rows",
			len(gotByKey), len(want))
	}

	// Columns the Java reader formats identically to this converter.
	exact := []string{"KategorieNr", "LieferantNr", "MengeAufLager", "MengeBestellt",
		"Produktname", "StückProEinheit"}
	// Columns the Java reader writes with a German decimal comma.
	numeric := []string{"Einkaufspreis", "Listenpreis"}

	for key, w := range wantByKey {
		g, ok := gotByKey[key]
		if !ok {
			t.Errorf("ProduktNr %s missing from the Parquet output", key)
			continue
		}
		for _, col := range exact {
			if g[col] != w[col] {
				t.Errorf("ProduktNr %s column %q: Parquet %q, Java reader %q", key, col, g[col], w[col])
			}
		}
		for _, col := range numeric {
			gv, err1 := strconv.ParseFloat(g[col], 64)
			wv, err2 := strconv.ParseFloat(strings.ReplaceAll(w[col], ",", "."), 64)
			if err1 != nil || err2 != nil {
				t.Errorf("ProduktNr %s column %q: unparseable (%q / %q)", key, col, g[col], w[col])
				continue
			}
			if math.Abs(gv-wv) > 1e-9 {
				t.Errorf("ProduktNr %s column %q: Parquet %v, Java reader %v", key, col, gv, wv)
			}
		}
	}
}

// TestRealQVDWideSparse converts a QVD with 19 columns, several of which have
// an empty symbol table, plus field names containing '#'. Columns with no
// symbols exercise the zero-bit-width null path.
func TestRealQVDWideSparse(t *testing.T) {
	in := realFixture("wide-sparse.qvd")
	out := filepath.Join(t.TempDir(), "wide.parquet")

	opts := testOptions()
	opts.Quality = QualityFull
	stats, report, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("full quality gate failed: %+v", report)
	}
	if stats.Rows != 120 || stats.Columns != 19 {
		t.Fatalf("got %d rows and %d columns, want 120 and 19", stats.Rows, stats.Columns)
	}

	// Columns whose symbol table is empty must be entirely null, not zeros.
	var allNull int
	for _, c := range report.Columns {
		if c.Source.NonNulls == 0 {
			allNull++
			if c.Source.Nulls != 120 {
				t.Errorf("column %q has %d nulls over 120 rows", c.Name, c.Source.Nulls)
			}
		}
	}
	if allNull == 0 {
		t.Error("expected at least one all-null column in this fixture")
	}

	// A '#' in a field name must survive into the Parquet schema.
	found := false
	for _, c := range report.Columns {
		if strings.HasPrefix(c.Name, "#") {
			found = true
		}
	}
	if !found {
		t.Error("expected a column name starting with '#'")
	}
}

// readParquetRows renders every cell as a string for comparison.
func readParquetRows(t *testing.T, path string) []map[string]string {
	t.Helper()
	schema, records := readParquet(t, path)
	var out []map[string]string
	for _, rec := range records {
		for r := 0; r < int(rec.NumRows()); r++ {
			row := make(map[string]string, len(schema.Fields()))
			for c, f := range schema.Fields() {
				row[f.Name] = cellString(rec.Column(c), r)
			}
			out = append(out, row)
		}
	}
	return out
}

func cellString(col arrow.Array, row int) string {
	if col.IsNull(row) {
		return ""
	}
	switch a := col.(type) {
	case *array.String:
		return a.Value(row)
	case *array.Int64:
		return strconv.FormatInt(a.Value(row), 10)
	case *array.Float64:
		return strconv.FormatFloat(a.Value(row), 'g', -1, 64)
	}
	return col.ValueStr(row)
}

// readReferenceCSV parses the semicolon-separated CSV the Java reader emits.
func readReferenceCSV(t *testing.T, path string) []map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.Comma = ';'
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("parse reference CSV: %v", err)
	}
	if len(rows) < 2 {
		t.Fatal("reference CSV has no data rows")
	}
	header := rows[0]
	// The file starts with a UTF-8 BOM.
	header[0] = strings.TrimPrefix(header[0], "\ufeff")

	var out []map[string]string
	for _, rec := range rows[1:] {
		if len(rec) != len(header) {
			continue
		}
		m := make(map[string]string, len(header))
		for i, h := range header {
			m[h] = rec[i]
		}
		out = append(out, m)
	}
	return out
}

// --numeric-promote=decimal on the real QlikView fixture: both price columns
// must become exact decimals while the quantity columns stay integers. The
// file declares them REAL with nDec=14 and a format carrying no decimal
// separator, so the scale can only come from the values.
func TestRealQVDDecimalPromotion(t *testing.T) {
	in := realFixture("products.qvd")
	out := filepath.Join(t.TempDir(), "promoted.parquet")

	opts := testOptions()
	opts.NumericPromote = PromoteDecimal
	opts.Quality = QualityFull
	_, report, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("full quality gate failed: %+v", report)
	}

	types := map[string]string{}
	for _, c := range report.Columns {
		types[c.Name] = c.Type
	}
	for name, want := range map[string]string{
		"Einkaufspreis": "decimal(5, 2)",
		"Listenpreis":   "decimal(5, 2)",
		"MengeAufLager": "int64", // a quantity gains nothing from decimal(p,0)
		"KategorieNr":   "int64",
		"Produktname":   "utf8",
	} {
		if types[name] != want {
			t.Errorf("column %q resolved to %q, want %q", name, types[name], want)
		}
	}

	// The decimal sum must be exact, and must agree with the float64 run.
	var decSum string
	for _, c := range report.Columns {
		if c.Name == "Listenpreis" {
			decSum = c.Source.Sum
		}
	}
	if decSum == "" {
		t.Fatal("no Listenpreis metrics in the report")
	}

	floatOpts := testOptions()
	floatOpts.NumericPromote = PromoteFloat64
	floatOpts.Quality = QualityNumeric
	floatOut := filepath.Join(t.TempDir(), "float.parquet")
	_, floatReport, err := Run(context.Background(), in, floatOut, &floatOpts, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range floatReport.Columns {
		if c.Name != "Listenpreis" {
			continue
		}
		dec, errD := strconv.ParseFloat(decSum, 64)
		flt, errF := strconv.ParseFloat(c.Source.Sum, 64)
		if errD != nil || errF != nil {
			t.Fatalf("unparseable sums %q / %q", decSum, c.Source.Sum)
		}
		if math.Abs(dec-flt) > 1e-6 {
			t.Errorf("decimal sum %s disagrees with the float64 sum %s", decSum, c.Source.Sum)
		}
	}
}
