package convert

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// The common way to collapse two fields onto one name is a --field-regex that
// keeps only the technical part of a composite name. --duplicate-names=suffix
// must then write both columns, with the second one's data intact.
func TestDuplicateNamesSuffixKeepsBothRenamedFields(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "A-||-DATBI-||-one", Type: "ASCII", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Str("x")}},
		{Name: "B-||-DATBI-||-two", Type: "ASCII", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Str("y")}},
	}}
	in := buildFixture(t, tbl)
	out := filepath.Join(t.TempDir(), "o.parquet")

	renamer, err := NewFieldRenamer(sapRegex, "", "")
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Renamer = renamer
	opts.DuplicateNames = DuplicateSuffix

	stats, _, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stats.Duplicates) != 1 {
		t.Fatalf("Duplicates = %v, want one rename", stats.Duplicates)
	}
	if d := stats.Duplicates[0]; d.From != "DATBI" || d.To != "DATBI_2" {
		t.Errorf("rename = %q -> %q, want \"DATBI\" -> \"DATBI_2\"", d.From, d.To)
	}

	schema, _ := readParquet(t, out)
	var names []string
	for _, f := range schema.Fields() {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "DATBI,DATBI_2" {
		t.Fatalf("columns = %s, want DATBI,DATBI_2", got)
	}
	// Neither column may lose the field it came from, which is the whole
	// point of not dropping the duplicate.
	for i, want := range []string{"A-||-DATBI-||-one", "B-||-DATBI-||-two"} {
		md := schema.Field(i).Metadata
		if v, ok := md.GetValue("qvd.field"); !ok || v != want {
			t.Errorf("column %d qvd.field = %q (present %v), want %q", i, v, ok, want)
		}
	}
	rows := readParquetRows(t, out)
	if len(rows) != 1 || rows[0]["DATBI"] != "x" || rows[0]["DATBI_2"] != "y" {
		t.Errorf("rows = %v, want DATBI=x and DATBI_2=y", rows)
	}
}

// The suffix must skip a name a real field already carries, whichever order
// the fields appear in, or the rename would collide in its turn.
func TestDuplicateNamesSuffixSkipsNamesAlreadyTaken(t *testing.T) {
	cols := []ResolvedColumn{
		{SourceIndex: 0, Name: "A", OriginalName: "A"},
		{SourceIndex: 1, Name: "A", OriginalName: "A#2"},
		{SourceIndex: 2, Name: "A_2", OriginalName: "A_2"},
		{SourceIndex: 3, Name: "A", OriginalName: "A#3"},
	}
	dups, err := resolveNameCollisions(cols, DuplicateSuffix)
	if err != nil {
		t.Fatalf("resolveNameCollisions: %v", err)
	}
	var got []string
	for _, c := range cols {
		got = append(got, c.Name)
	}
	if want := "A,A_3,A_2,A_4"; strings.Join(got, ",") != want {
		t.Errorf("names = %s, want %s", strings.Join(got, ","), want)
	}
	if len(dups) != 2 {
		t.Errorf("dups = %v, want two renames", dups)
	}
}

// A generated "${name}__text" companion yields to the real field of that
// name: a reader asking for "Qty__text" must get the QVD field, not the
// display side of Qty.
func TestDuplicateNamesSuffixKeepsDualTextCompanion(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Qty", Type: "INTEGER", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.DualInt(1, "one")}},
		{Name: "Qty__text", Type: "ASCII", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Str("collides")}},
	}}
	in := buildFixture(t, tbl)
	out := filepath.Join(t.TempDir(), "o.parquet")

	opts := testOptions()
	opts.Dual = DualColumns
	opts.DuplicateNames = DuplicateSuffix
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rows := readParquetRows(t, out)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one", rows)
	}
	if got := rows[0]["Qty__text"]; got != "collides" {
		t.Errorf("Qty__text = %q, want the source field's value %q", got, "collides")
	}
	if got := rows[0]["Qty__text_2"]; got != "one" {
		t.Errorf("Qty__text_2 = %q, want the dual's display string %q", got, "one")
	}
}

// The default stays an error, and it names the flag that keeps both columns.
func TestDuplicateNamesDefaultErrorOffersTheSuffixMode(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "A-||-DATBI-||-one", Type: "ASCII", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Str("x")}},
		{Name: "B-||-DATBI-||-two", Type: "ASCII", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Str("y")}},
	}}
	in := buildFixture(t, tbl)
	renamer, err := NewFieldRenamer(sapRegex, "", "")
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Renamer = renamer

	_, _, err = Run(context.Background(), in, filepath.Join(t.TempDir(), "o.parquet"), &opts, nil)
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("err = %v, want ErrSchemaPolicy", err)
	}
	if !strings.Contains(err.Error(), "--duplicate-names=suffix") {
		t.Errorf("error should offer the mode that keeps both: %v", err)
	}
}

func TestParseDuplicateNamePolicy(t *testing.T) {
	for in, want := range map[string]DuplicateNamePolicy{
		"error": DuplicateError, "suffix": DuplicateSuffix, " SUFFIX ": DuplicateSuffix,
	} {
		got, err := ParseDuplicateNamePolicy(in)
		if err != nil || got != want {
			t.Errorf("ParseDuplicateNamePolicy(%q) = (%v, %v), want %v", in, got, err, want)
		}
	}
	if _, err := ParseDuplicateNamePolicy("rename"); err == nil {
		t.Error("an unknown mode should be rejected")
	}
}

// The refusal offers a name suffix mode would actually write. Hard-coding
// "${name}_2" made it name a column the file already carries, so following the
// advice produced a different name than the message promised.
func TestDuplicateNameErrorOffersTheNameSuffixWouldWrite(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "A", Type: "ASCII", Rows: []int{0}, Symbols: []qvd.Symbol{qvdtest.Str("x")}},
		{Name: "A_2", Type: "ASCII", Rows: []int{0}, Symbols: []qvd.Symbol{qvdtest.Str("y")}},
		{Name: "A", Type: "ASCII", Rows: []int{0}, Symbols: []qvd.Symbol{qvdtest.Str("z")}},
	}}
	in := buildFixture(t, tbl)

	opts := testOptions()
	_, _, err := Run(context.Background(), in, filepath.Join(t.TempDir(), "o.parquet"), &opts, nil)
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("err = %v, want ErrSchemaPolicy", err)
	}
	if !strings.Contains(err.Error(), `writing the second as "A_3"`) {
		t.Errorf("error should offer \"A_3\", the name the suffix mode takes: %v", err)
	}

	// And the advice must hold: what it promised is what the run writes.
	opts.DuplicateNames = DuplicateSuffix
	out := filepath.Join(t.TempDir(), "o.parquet")
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatalf("suffix run: %v", err)
	}
	schema, _ := readParquet(t, out)
	var names []string
	for _, f := range schema.Fields() {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "A,A_2,A_3" {
		t.Errorf("columns = %s, want A,A_2,A_3", got)
	}
}

// The note announcing a dual's companion column is written before collisions
// are resolved. A renamed companion must not leave the old name standing in
// it: the note travels into the schema log, --inspect, --schema-report and the
// catalog.
func TestDuplicateNamesSuffixCorrectsTheDualCompanionNote(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Qty", Type: "INTEGER", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.DualInt(1, "one")}},
		{Name: "Qty__text", Type: "ASCII", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Str("collides")}},
	}}
	in := buildFixture(t, tbl)

	f, err := qvd.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Dual = DualColumns
	opts.DuplicateNames = DuplicateSuffix
	rs, err := ResolveSchema(f, &opts, nil)
	if err != nil {
		t.Fatalf("ResolveSchema: %v", err)
	}

	note := rs.Notes[0]
	if !strings.Contains(note, `"Qty__text_2"`) {
		t.Errorf("the note should name the column actually written: %s", note)
	}
	if strings.Contains(note, `display side written to "Qty__text"`) ||
		strings.Contains(note, `kept in "Qty__text"`) {
		t.Errorf("the note still announces the name the companion lost: %s", note)
	}
}

// The companion clause carries an example display string, and that example can
// be the very name the companion is losing. Rewriting the name out of the
// finished note hit the example instead of the column mention, corrupting the
// evidence and still naming the wrong column.
func TestDuplicateNamesSuffixNoteSurvivesAnExampleThatLooksLikeTheName(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		// "Qty__text" is informative text beside 1, and is also the name the
		// companion column would take.
		{Name: "Qty", Type: "INTEGER", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.DualInt(1, "Qty__text")}},
		{Name: "Qty__text", Type: "ASCII", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Str("collides")}},
	}}
	in := buildFixture(t, tbl)

	f, err := qvd.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.DuplicateNames = DuplicateSuffix
	rs, err := ResolveSchema(f, &opts, nil)
	if err != nil {
		t.Fatalf("ResolveSchema: %v", err)
	}

	note := rs.Notes[0]
	// The example is evidence and must be quoted as it stands in the file.
	if !strings.Contains(note, `(e.g. "Qty__text" beside 1)`) {
		t.Errorf("the example display string was altered: %s", note)
	}
	if !strings.Contains(note, `kept in "Qty__text_2"`) {
		t.Errorf("the note should name the column actually written: %s", note)
	}
	// And the column has to be there, holding the display string.
	out := filepath.Join(t.TempDir(), "o.parquet")
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := readParquetRows(t, out)
	if len(rows) != 1 || rows[0]["Qty__text"] != "collides" || rows[0]["Qty__text_2"] != "Qty__text" {
		t.Errorf("rows = %v, want Qty__text=collides and Qty__text_2=Qty__text", rows)
	}
}
