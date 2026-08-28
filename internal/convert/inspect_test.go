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

func TestInspectResolvesSchemaWithoutReadingRecords(t *testing.T) {
	in := buildFixture(t, sampleTable(5000))
	opts := testOptions()

	rep, err := Inspect(context.Background(), in, &opts)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	defer rep.Close()

	if rep.SchemaErr != nil {
		t.Fatalf("schema failed: %v", rep.SchemaErr)
	}
	if rep.TableName != "Sales" || rep.Rows != 5000 {
		t.Errorf("table=%q rows=%d", rep.TableName, rep.Rows)
	}
	if len(rep.Schema.Columns) != 6 {
		t.Errorf("got %d columns, want 6", len(rep.Schema.Columns))
	}
	if rep.SymbolCount == 0 {
		t.Error("no symbols counted")
	}
	// The record area is reported but never read; that is the whole point.
	if rep.RecordBytes != int64(rep.Rows)*int64(rep.RecordByteSize) {
		t.Errorf("RecordBytes = %d, want %d*%d", rep.RecordBytes, rep.Rows, rep.RecordByteSize)
	}
	if rep.RecordBytes <= rep.SymbolBytes {
		t.Errorf("this fixture should have more record bytes (%d) than symbol bytes (%d)",
			rep.RecordBytes, rep.SymbolBytes)
	}
}

// Inspect must agree with what a real conversion produces, or it would mislead.
func TestInspectMatchesConversionSchema(t *testing.T) {
	in := buildFixture(t, sampleTable(500))
	opts := testOptions()

	rep, err := Inspect(context.Background(), in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()

	out := filepath.Join(t.TempDir(), "out.parquet")
	convOpts := testOptions()
	convOpts.Quality = QualityBasic
	_, report, err := Run(context.Background(), in, out, &convOpts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("conversion gate failed: %+v", report)
	}

	if len(rep.Schema.Columns) != len(report.Columns) {
		t.Fatalf("inspect saw %d columns, conversion wrote %d",
			len(rep.Schema.Columns), len(report.Columns))
	}
	for i, c := range rep.Schema.Columns {
		if c.Name != report.Columns[i].Name {
			t.Errorf("column %d: inspect %q, conversion %q", i, c.Name, report.Columns[i].Name)
		}
		if c.ArrowType.String() != report.Columns[i].Type {
			t.Errorf("column %q: inspect type %s, conversion type %s",
				c.Name, c.ArrowType, report.Columns[i].Type)
		}
	}
}

func TestInspectHonoursExcludeAndRename(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	renamer, err := NewFieldRenamer(sapRegex, "", "")
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Exclude = []string{"%*"}
	opts.Renamer = renamer

	rep, err := Inspect(context.Background(), in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()

	if len(rep.Excluded) != 2 {
		t.Errorf("excluded = %v, want 2 entries", rep.Excluded)
	}
	var names []string
	for _, c := range rep.Schema.Columns {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "DATBI,KSCHL,PlainField" {
		t.Errorf("columns = %s", got)
	}
}

// A file the type policy rejects must still produce a useful report rather
// than only an error, because the profiles are what explain the rejection.
func TestInspectReportsSchemaFailureWithProfiles(t *testing.T) {
	tbl := qvdtest.Table{Name: "Bad", Fields: []qvdtest.Field{
		{Name: "CustomerID", Type: "ASCII", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Int(42), qvdtest.Str("N/A")}},
	}}
	in := buildFixture(t, tbl)
	opts := testOptions()

	rep, err := Inspect(context.Background(), in, &opts)
	if err != nil {
		t.Fatalf("Inspect should not return the policy error: %v", err)
	}
	defer rep.Close()

	if rep.SchemaErr == nil {
		t.Fatal("expected a schema policy failure")
	}
	if !errors.Is(rep.SchemaErr, ErrSchemaPolicy) {
		t.Errorf("SchemaErr = %v, want ErrSchemaPolicy", rep.SchemaErr)
	}
	// The rendered report must fall back to raw profiles.
	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{"Schema could not be resolved", "CustomerID", "STRINGS"} {
		if !strings.Contains(out, want) {
			t.Errorf("report should contain %q:\n%s", want, out)
		}
	}
}

func TestInspectWriteRendersSchemaTable(t *testing.T) {
	in := buildFixture(t, sampleTable(120))
	opts := testOptions()
	rep, err := Inspect(context.Background(), in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()

	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"Table           Sales", "Rows            120",
		"COLUMN", "PARQUET TYPE", "Amount", "decimal", "not read",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report should contain %q:\n%s", want, out)
		}
	}
}

func TestInspectMissingFile(t *testing.T) {
	opts := testOptions()
	if _, err := Inspect(context.Background(), filepath.Join(t.TempDir(), "nope.qvd"), &opts); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// Inspect must not create anything.
func TestInspectWritesNoFiles(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.qvd")
	if _, err := qvdtest.Build(in, sampleTable(100)); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	rep, err := Inspect(context.Background(), in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	rep.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "in.qvd" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("inspect created files: %v", names)
	}
}

func TestWithThousands(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000",
		5000000: "5,000,000", 1234567890: "1,234,567,890", -4321: "-4,321",
	} {
		if got := withThousands(in); got != want {
			t.Errorf("withThousands(%d) = %q, want %q", in, got, want)
		}
	}
}

// Inspect is where a command line is checked before a conversion that runs
// for a quarter of an hour, so both silent outcomes have to be visible here.
func TestInspectWriteReportsDeadPatternsAndUntouchedFields(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	renamer, err := NewFieldRenamer(sapRegex, "", "")
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Exclude = []string{"%SYS*", "COUNTER"}
	opts.Renamer = renamer

	rep, err := Inspect(context.Background(), in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()

	if strings.Join(rep.ExcludeNoMatch, ",") != "COUNTER" {
		t.Errorf("ExcludeNoMatch = %v, want [COUNTER]", rep.ExcludeNoMatch)
	}

	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		`Exclude         "COUNTER" matched no field`,
		"Field regex     2 of 4 field(s) renamed, 2 unchanged: %A057_PKEY, PlainField",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
}

// With nothing to report, neither line appears at all.
func TestInspectWriteStaysQuietWhenEveryPatternMatches(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	opts := testOptions()
	opts.Exclude = []string{"%*"}

	rep, err := Inspect(context.Background(), in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()

	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatal(err)
	}
	if out := sb.String(); strings.Contains(out, "matched no field") || strings.Contains(out, "Field regex") {
		t.Errorf("unexpected reporting:\n%s", out)
	}
}

// A file the type policy rejects has no resolved schema, and that is exactly
// when the rest of the command line wants checking: the run has to be fixed
// and repeated, so a pattern or an expression that is also wrong should not
// wait for the next attempt to show itself.
func TestInspectReportsSelectionEvenWhenTheSchemaFails(t *testing.T) {
	tbl := qvdtest.Table{Name: "Bad", Fields: []qvdtest.Field{
		{Name: "%BAD_PKEY", Type: "ASCII", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Str("k1"), qvdtest.Str("k2")}},
		{Name: "Bad-||-CustomerID-||-Kunde", Type: "ASCII", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Int(42), qvdtest.Str("N/A")}},
	}}
	in := buildFixture(t, tbl)
	renamer, err := NewFieldRenamer(sapRegex, "", "")
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Renamer = renamer
	opts.Exclude = []string{"COUNTER"}

	rep, err := Inspect(context.Background(), in, &opts)
	if err != nil {
		t.Fatalf("Inspect should not return the policy error: %v", err)
	}
	defer rep.Close()

	if rep.SchemaErr == nil {
		t.Fatal("this fixture should fail the mixed-type policy")
	}
	if rep.Schema != nil {
		t.Fatal("fixture assumption wrong: a rejected file has no resolved schema")
	}

	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		`Exclude         "COUNTER" matched no field`,
		"Field regex     1 of 2 field(s) renamed, 1 unchanged: %BAD_PKEY",
		"Schema could not be resolved",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report should contain %q:\n%s", want, out)
		}
	}
}
