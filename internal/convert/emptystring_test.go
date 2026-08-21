package convert

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// Qlik treats an empty string as absent, so by default it is written as null
// rather than as a zero-length string.
func TestEmptyStringBecomesNull(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Name", Type: "ASCII", Rows: []int{0, 1, 2},
			Symbols: []qvd.Symbol{qvdtest.Str("alpha"), qvdtest.Str(""), qvdtest.Str("beta")}},
	}}
	in := buildFixture(t, tbl)
	out := filepath.Join(t.TempDir(), "out.parquet")

	opts := testOptions()
	opts.Quality = QualityFull
	_, report, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("quality gate failed: %+v", report)
	}
	if got := report.Columns[0].Source.Nulls; got != 1 {
		t.Errorf("nulls = %d, want 1: the empty string should be null", got)
	}

	// And the written file must agree.
	schema, records := readParquet(t, out)
	idx := schema.FieldIndices("Name")[0]
	var nulls int
	for _, rec := range records {
		col := rec.Column(idx)
		for r := 0; r < int(rec.NumRows()); r++ {
			if col.IsNull(r) {
				nulls++
			}
		}
	}
	if nulls != 1 {
		t.Errorf("Parquet holds %d nulls, want 1", nulls)
	}
}

// --empty-as-null=false keeps the distinction between "" and null.
func TestEmptyStringPreservedWhenDisabled(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Name", Type: "ASCII", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Str("alpha"), qvdtest.Str("")}},
	}}
	in := buildFixture(t, tbl)
	out := filepath.Join(t.TempDir(), "out.parquet")

	opts := testOptions()
	opts.EmptyStringAsNull = false
	opts.Quality = QualityFull
	_, report, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("quality gate failed: %+v", report)
	}
	if got := report.Columns[0].Source.Nulls; got != 0 {
		t.Errorf("nulls = %d, want 0 with --empty-as-null=false", got)
	}
}

// A dual's display side follows the same rule.
func TestEmptyDualTextBecomesNull(t *testing.T) {
	f := qvdtest.Field{Name: "Status", Type: "INTEGER", Rows: []int{0, 1},
		Symbols: []qvd.Symbol{
			qvdtest.DualInt(1, "Open"),
			qvdtest.DualInt(2, ""), // informative column, but this text is empty
		}}
	rs := mustResolve(t, f, func(o *Options) { o.Dual = DualColumns })
	conv := converterFor(t, f, rs)
	v, err := conv.ConvertAt(1, 1, qvd.Symbol{Kind: qvd.SymbolDualIntString, Int: 2, Text: ""})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Null {
		t.Errorf("an empty display string should be null, got %q", v.Str)
	}
}

// converterFor builds a Converter over an already-resolved schema.
func converterFor(t *testing.T, f qvdtest.Field, rs *ResolvedSchema) *Converter {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.qvd")
	if _, err := qvdtest.Build(path, qvdtest.Table{Name: "T", Fields: []qvdtest.Field{f}}); err != nil {
		t.Fatal(err)
	}
	qf, err := qvd.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { qf.Close() })
	if err := qf.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Dual = DualColumns
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	conv, err := NewConverter(qf, rs, &opts)
	if err != nil {
		t.Fatal(err)
	}
	return conv
}
