package convert

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// With empty strings read as absent, a numeric column holding empty
// placeholders is not a mixed-type column: type resolution has to apply the
// same rule the conversion does.
func TestEmptyPlaceholderDoesNotMakeAColumnMixed(t *testing.T) {
	f := qvdtest.Field{Name: "Qty", Type: "INTEGER", Rows: []int{0, 1, 2},
		Symbols: []qvd.Symbol{qvdtest.Int(7), qvdtest.Str(""), qvdtest.Int(9)}}

	rs := mustResolve(t, f, nil) // --empty-as-null defaults on
	if got := rs.Columns[0].ArrowType.String(); got != "int64" {
		t.Errorf("type = %s, want int64: an empty placeholder is absent, not text", got)
	}

	// With the rule disabled it is genuinely mixed again.
	_, err := resolve(t, f, func(o *Options) { o.EmptyStringAsNull = false })
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Errorf("--empty-as-null=false should still see a mixed column, got %v", err)
	}
}

// End to end: the placeholder becomes a null in the written file.
func TestEmptyPlaceholderConvertsToNull(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Qty", Type: "INTEGER", Rows: []int{0, 1, 2},
			Symbols: []qvd.Symbol{qvdtest.Int(7), qvdtest.Str(""), qvdtest.Int(9)}},
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
	if report.Columns[0].Type != "int64" {
		t.Errorf("type = %s, want int64", report.Columns[0].Type)
	}
	if got := report.Columns[0].Source.Nulls; got != 1 {
		t.Errorf("nulls = %d, want 1", got)
	}
}

// A column of nothing but empty strings is entirely absent.
func TestAllEmptyStringsColumnIsAllNull(t *testing.T) {
	f := qvdtest.Field{Name: "Blank", Type: "ASCII", Rows: []int{0, 0},
		Symbols: []qvd.Symbol{qvdtest.Str("")}}
	rs := mustResolve(t, f, nil)
	if rs.Columns[0].Strategy != StrategyNull {
		t.Errorf("strategy = %v, want StrategyNull", rs.Columns[0].Strategy)
	}
}

// An empty display string on a dual must not make the numeric side absent.
func TestEmptyDualTextDoesNotNullTheNumber(t *testing.T) {
	f := qvdtest.Field{Name: "Qty", Type: "INTEGER", Rows: []int{0, 1},
		Symbols: []qvd.Symbol{qvdtest.DualInt(7, ""), qvdtest.DualInt(9, "")}}
	rs := mustResolve(t, f, nil)
	if got := rs.Columns[0].ArrowType.String(); got != "int64" {
		t.Fatalf("type = %s, want int64", got)
	}
	conv := converterFor(t, f, rs)
	v, err := conv.ConvertAt(0, 0, qvd.Symbol{Kind: qvd.SymbolDualIntString, Int: 7, Text: ""})
	if err != nil {
		t.Fatal(err)
	}
	if v.Null || v.Int != 7 {
		t.Errorf("value = %+v, want 7: an empty display string does not remove the number", v)
	}
}

// Inspect is the preflight for what conversion writes, so its null count must
// match, not the raw QVD count.
func TestInspectNullCountMatchesConversion(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Name", Type: "ASCII", Rows: []int{0, 1, 2},
			Symbols: []qvd.Symbol{qvdtest.Str("alpha"), qvdtest.Str(""), qvdtest.Str("beta")}},
	}}
	in := buildFixture(t, tbl)

	opts := testOptions()
	rep, err := Inspect(in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()
	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "o.parquet")
	o2 := testOptions()
	o2.Quality = QualityBasic
	_, report, err := Run(context.Background(), in, out, &o2, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := report.Columns[0].Source.Nulls
	if want != 1 {
		t.Fatalf("fixture assumption wrong: conversion wrote %d nulls", want)
	}
	for _, line := range strings.Split(sb.String(), "\n") {
		if strings.HasPrefix(line, "Name ") {
			if !strings.Contains(line, " 1 ") {
				t.Errorf("inspect row %q should report %d null(s)", line, want)
			}
			return
		}
	}
	t.Errorf("no Name row in the inspect report:\n%s", sb.String())
}

// A decimal column may also carry empty placeholders; they must resolve and
// convert as null rather than failing.
func TestEmptyPlaceholderInDecimalColumn(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Amount", Type: "MONEY", NDec: 2, Dec: ".", Rows: []int{0, 1, 2},
			Symbols: []qvd.Symbol{qvdtest.Float(1.5), qvdtest.Str(""), qvdtest.Float(2.25)}},
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
	if !strings.HasPrefix(report.Columns[0].Type, "decimal") {
		t.Errorf("type = %s, want a decimal", report.Columns[0].Type)
	}
	if got := report.Columns[0].Source.Nulls; got != 1 {
		t.Errorf("nulls = %d, want 1", got)
	}
	if got := report.Columns[0].Source.Sum; got != "3.75" {
		t.Errorf("sum = %s, want 3.75", got)
	}
}

// A pinned decimal column must read empty placeholders the same way.
func TestEmptyPlaceholderWithSchemaOverride(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schemaPath,
		[]byte(`{"columns":{"Amount":{"type":"decimal","precision":12,"scale":2}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Amount", Type: "MONEY", NDec: 2, Dec: ".", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Float(1.5), qvdtest.Str("")}},
	}}
	in := buildFixture(t, tbl)

	opts := testOptions()
	opts.SchemaOverridePath = schemaPath
	opts.Quality = QualityFull
	_, report, err := Run(context.Background(), in, filepath.Join(dir, "o.parquet"), &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("quality gate failed: %+v", report)
	}
	if report.Columns[0].Type != "decimal(12, 2)" {
		t.Errorf("type = %s, want decimal(12, 2)", report.Columns[0].Type)
	}
	if got := report.Columns[0].Source.Nulls; got != 1 {
		t.Errorf("nulls = %d, want 1", got)
	}
}
