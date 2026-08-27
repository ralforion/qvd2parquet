package convert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// sapStyleTable mirrors a SAP extract: QlikView key fields prefixed with '%',
// and composite field names of the form "TABLE-||-FIELD-||-Description".
func sapStyleTable() qvdtest.Table {
	rows := []int{0, 1, 0, 1}
	return qvdtest.Table{Name: "A057", Fields: []qvdtest.Field{
		{Name: "%A057_PKEY", Type: "ASCII", Rows: rows,
			Symbols: []qvd.Symbol{qvdtest.Str("k1"), qvdtest.Str("k2")}},
		{Name: "%SYS_TS", Type: "INTEGER", Rows: rows,
			Symbols: []qvd.Symbol{qvdtest.Int(1), qvdtest.Int(2)}},
		{Name: "A057-||-DATBI-||-Ende Gültigkeit", Type: "INTEGER", Rows: rows,
			Symbols: []qvd.Symbol{qvdtest.Int(45000), qvdtest.Int(45001)}},
		{Name: "A057-||-KSCHL-||-Konditionsart", Type: "ASCII", Rows: rows,
			Symbols: []qvd.Symbol{qvdtest.Str("PR00"), qvdtest.Str("K007")}},
		{Name: "PlainField", Type: "ASCII", Rows: rows,
			Symbols: []qvd.Symbol{qvdtest.Str("a"), qvdtest.Str("b")}},
	}}
}

// sapRegex extracts the technical name and the description from a composite
// field name. It is the expression documented in the README.
const sapRegex = `^[^-]*-\|\|-(?P<name>[^-]*)-\|\|-(?P<comment>.*)$`

func TestExcludeByWildcard(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	out := filepath.Join(t.TempDir(), "out.parquet")

	opts := testOptions()
	opts.Exclude = []string{"%*"}
	opts.Quality = QualityFull
	stats, report, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("quality gate failed: %+v", report)
	}
	if stats.Columns != 3 {
		t.Fatalf("got %d columns, want 3 after excluding the %% fields", stats.Columns)
	}
	for _, c := range report.Columns {
		if strings.HasPrefix(c.Name, "%") {
			t.Errorf("column %q should have been excluded", c.Name)
		}
	}
}

func TestExcludeMultiplePatterns(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	opts.Exclude = []string{"%*", "Plain*"}
	stats, _, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Columns != 2 {
		t.Errorf("got %d columns, want 2", stats.Columns)
	}
}

func TestExcludeEverythingIsAnError(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	opts := testOptions()
	opts.Exclude = []string{"*"}
	_, _, err := Run(context.Background(), in, filepath.Join(t.TempDir(), "o.parquet"), &opts, nil)
	if err == nil || !strings.Contains(err.Error(), "removed every column") {
		t.Fatalf("err = %v, want an every-column-excluded error", err)
	}
}

func TestFieldRegexRenamesAndComments(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	out := filepath.Join(t.TempDir(), "out.parquet")

	renamer, err := NewFieldRenamer(sapRegex, "", "")
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Exclude = []string{"%*"}
	opts.Renamer = renamer
	opts.Quality = QualityFull

	_, report, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("quality gate failed: %+v", report)
	}

	var names []string
	for _, c := range report.Columns {
		names = append(names, c.Name)
	}
	want := []string{"DATBI", "KSCHL", "PlainField"} // PlainField does not match, so it is untouched
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("columns = %v, want %v", names, want)
	}

	// The description must survive as Parquet field metadata, and the original
	// QVD name must be recoverable.
	schema, _ := readParquet(t, out)
	md := map[string]map[string]string{}
	for _, f := range schema.Fields() {
		m := map[string]string{}
		for i, k := range f.Metadata.Keys() {
			m[k] = f.Metadata.Values()[i]
		}
		md[f.Name] = m
	}
	if got := md["DATBI"]["comment"]; got != "Ende Gültigkeit" {
		t.Errorf("DATBI comment = %q, want %q", got, "Ende Gültigkeit")
	}
	if got := md["DATBI"]["qvd.field"]; got != "A057-||-DATBI-||-Ende Gültigkeit" {
		t.Errorf("DATBI qvd.field = %q", got)
	}
	if got := md["KSCHL"]["comment"]; got != "Konditionsart" {
		t.Errorf("KSCHL comment = %q", got)
	}
	// The Parquet reader adds its own keys (PARQUET:field_id), so check only
	// the ones this converter writes.
	for _, k := range []string{"comment", "qvd.field"} {
		if v, ok := md["PlainField"][k]; ok {
			t.Errorf("unmatched field PlainField should have no %q metadata, got %q", k, v)
		}
	}
}

func TestFieldRenamerTemplates(t *testing.T) {
	// Numbered groups work when the regex has no named groups.
	r, err := NewFieldRenamer(`^(\w+)-\|\|-(\w+)-\|\|-(.*)$`, "$2", "$3")
	if err != nil {
		t.Fatal(err)
	}
	name, comment := r.Apply("A057-||-DATBI-||-Ende Gültigkeit")
	if name != "DATBI" || comment != "Ende Gültigkeit" {
		t.Errorf("got (%q, %q), want (DATBI, Ende Gültigkeit)", name, comment)
	}

	// Templates can combine groups.
	r, err = NewFieldRenamer(`^(\w+)-\|\|-(\w+)-\|\|-.*$`, "${1}_${2}", " ")
	if err != nil {
		t.Fatal(err)
	}
	if name, _ := r.Apply("A057-||-DATBI-||-x"); name != "A057_DATBI" {
		t.Errorf("name = %q, want A057_DATBI", name)
	}
}

func TestFieldRenamerLeavesNonMatchingNames(t *testing.T) {
	r, err := NewFieldRenamer(sapRegex, "", "")
	if err != nil {
		t.Fatal(err)
	}
	name, comment := r.Apply("PlainField")
	if name != "PlainField" || comment != "" {
		t.Errorf("got (%q, %q), want (PlainField, \"\")", name, comment)
	}
	// A nil renamer is a no-op, so callers need no nil checks.
	var nilRenamer *FieldRenamer
	if n, c := nilRenamer.Apply("X"); n != "X" || c != "" {
		t.Errorf("nil renamer changed %q", n)
	}
}

func TestFieldRenamerRejectsBadConfig(t *testing.T) {
	if _, err := NewFieldRenamer("([", "", ""); err == nil {
		t.Error("expected an error for an invalid regexp")
	}
	// A regex with no 'name' group and no explicit template would blank every
	// column name, so it is rejected up front.
	_, err := NewFieldRenamer(`^(\w+)-(\w+)$`, "", "")
	if err == nil {
		t.Fatal("expected an error for a regex with no 'name' group")
	}
	if !strings.Contains(err.Error(), "(?P<name>") {
		t.Errorf("error should show how to fix it: %v", err)
	}
	// Templates without a regex are a usage error.
	if _, err := NewFieldRenamer("", "$1", ""); err == nil {
		t.Error("--field-name without --field-regex should fail")
	}
	// A blank configuration disables renaming entirely.
	r, err := NewFieldRenamer("", "", "")
	if r != nil || err != nil {
		t.Errorf("blank config gave (%v, %v), want (nil, nil)", r, err)
	}
}

// Two fields collapsing to the same name must be rejected, not silently
// produce a duplicate Parquet column.
func TestRenameCollisionIsRejected(t *testing.T) {
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
	for _, want := range []string{"DATBI", "A-||-DATBI-||-one", "B-||-DATBI-||-two"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// Excluding a field must not disturb the record bit layout of the rest.
func TestExcludeKeepsRemainingValuesCorrect(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	dir := t.TempDir()

	all := testOptions()
	allOut := filepath.Join(dir, "all.parquet")
	if _, _, err := Run(context.Background(), in, allOut, &all, nil); err != nil {
		t.Fatal(err)
	}
	some := testOptions()
	some.Exclude = []string{"%*"}
	someOut := filepath.Join(dir, "some.parquet")
	if _, _, err := Run(context.Background(), in, someOut, &some, nil); err != nil {
		t.Fatal(err)
	}

	pick := func(path, col string) []string {
		schema, records := readParquet(t, path)
		idx := schema.FieldIndices(col)
		if len(idx) == 0 {
			t.Fatalf("%s has no column %q", path, col)
		}
		var out []string
		for _, rec := range records {
			for r := 0; r < int(rec.NumRows()); r++ {
				out = append(out, cellString(rec.Column(idx[0]), r))
			}
		}
		return out
	}
	for _, col := range []string{"A057-||-KSCHL-||-Konditionsart", "PlainField"} {
		a, b := pick(allOut, col), pick(someOut, col)
		if strings.Join(a, ",") != strings.Join(b, ",") {
			t.Errorf("column %q differs after exclusion: %v vs %v", col, a, b)
		}
	}
}

// A pattern that drops nothing must say so. Both mistakes below look exactly
// like success today: "%" without the asterisk matches only a field named
// "%", and the renamed name is never what --exclude sees.
func TestExcludePatternMatchingNothingIsReported(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	renamer, err := NewFieldRenamer(sapRegex, "", "")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		exclude []string
		want    []string
	}{
		{"wildcard forgotten", []string{"%"}, []string{"%"}},
		{"renamed name, not the original", []string{"DATBI"}, []string{"DATBI"}},
		{"one of two patterns is dead", []string{"%*", "Nothing*"}, []string{"Nothing*"}},
		{"both patterns hit the same field", []string{"%A057*", "%A*"}, nil},
		{"every pattern matches", []string{"%*"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			opts.Exclude = tc.exclude
			opts.Renamer = renamer

			var lines []string
			logf := func(format string, args ...any) {
				lines = append(lines, fmt.Sprintf(format, args...))
			}
			out := filepath.Join(t.TempDir(), "out.parquet")
			stats, _, err := Run(context.Background(), in, out, &opts, logf)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if strings.Join(stats.ExcludeNoMatch, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ExcludeNoMatch = %v, want %v", stats.ExcludeNoMatch, tc.want)
			}

			noted := false
			for _, l := range lines {
				if strings.Contains(l, "matched no field") {
					noted = true
					for _, p := range tc.want {
						if !strings.Contains(l, strconv.Quote(p)) {
							t.Errorf("note %q does not name pattern %q", l, p)
						}
					}
				}
			}
			if noted != (len(tc.want) > 0) {
				t.Errorf("note printed = %v, want %v; lines: %v", noted, len(tc.want) > 0, lines)
			}
		})
	}
}

// The exclusion check must not be confused by --columns having already
// narrowed the field list.
func TestExcludeNoMatchWithColumnSelection(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	opts := testOptions()
	opts.Columns = []string{"PlainField", "A057-||-DATBI-||-Ende Gültigkeit"}
	opts.Exclude = []string{"A057*"}

	out := filepath.Join(t.TempDir(), "out.parquet")
	stats, _, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stats.ExcludeNoMatch) != 0 {
		t.Errorf("ExcludeNoMatch = %v, want none", stats.ExcludeNoMatch)
	}
	if stats.Columns != 1 {
		t.Errorf("got %d columns, want 1", stats.Columns)
	}
}

// The fields a --field-regex leaves alone are the ones worth naming: on a
// wide extract they are invisible among the renamed ones.
func TestFieldRegexReportsWhatItLeftAlone(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	renamer, err := NewFieldRenamer(sapRegex, "", "")
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Renamer = renamer
	opts.SchemaReportPath = filepath.Join(t.TempDir(), "schema.json")

	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	out := filepath.Join(t.TempDir(), "out.parquet")
	stats, _, err := Run(context.Background(), in, out, &opts, logf)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The two "-||-" fields are renamed; the key, the timestamp and the plain
	// field are not, which is what surprised a user on a 213 column extract.
	want := []string{"%A057_PKEY", "%SYS_TS", "PlainField"}
	if strings.Join(stats.Renames.Unchanged, ",") != strings.Join(want, ",") {
		t.Errorf("unchanged = %v, want %v", stats.Renames.Unchanged, want)
	}
	if stats.Renames.Renamed != 2 || stats.Renames.Fields != 5 {
		t.Errorf("renamed %d of %d, want 2 of 5", stats.Renames.Renamed, stats.Renames.Fields)
	}

	var summary string
	for _, l := range lines {
		if strings.HasPrefix(l, "field-regex: ") {
			summary = l
		}
	}
	if !strings.Contains(summary, "2 of 5 field(s) renamed, 3 unchanged: %A057_PKEY") {
		t.Errorf("summary line = %q", summary)
	}

	b, err := os.ReadFile(opts.SchemaReportPath)
	if err != nil {
		t.Fatal(err)
	}
	var rep SchemaReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.FieldRegex == nil {
		t.Fatal("schema report carries no fieldRegex section")
	}
	if rep.FieldRegex.Renamed != 2 || strings.Join(rep.FieldRegex.Unchanged, ",") != strings.Join(want, ",") {
		t.Errorf("report fieldRegex = %+v", rep.FieldRegex)
	}
}

// Without --field-regex there is nothing to report, and the log and the
// report must stay quiet rather than claim nothing was renamed.
func TestNoFieldRegexReportsNothing(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	opts := testOptions()
	opts.SchemaReportPath = filepath.Join(t.TempDir(), "schema.json")

	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	out := filepath.Join(t.TempDir(), "out.parquet")
	if _, _, err := Run(context.Background(), in, out, &opts, logf); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "field-regex: ") {
			t.Errorf("unexpected summary without --field-regex: %q", l)
		}
	}
	b, _ := os.ReadFile(opts.SchemaReportPath)
	var rep SchemaReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.FieldRegex != nil {
		t.Errorf("fieldRegex section = %+v, want absent", rep.FieldRegex)
	}
}

// A long list of untouched fields must not print two hundred names.
func TestRenameSummaryCapsTheNamesItPrints(t *testing.T) {
	s := RenameSummary{Fields: 9, Renamed: 1, Unchanged: []string{"a", "b", "c", "d", "e", "f", "g", "h"}}
	got := s.Line(5)
	want := "1 of 9 field(s) renamed, 8 unchanged: a, b, c, d, e and 3 more"
	if got != want {
		t.Errorf("Line(5) = %q, want %q", got, want)
	}
	if all := s.Line(0); !strings.HasSuffix(all, "a, b, c, d, e, f, g, h") {
		t.Errorf("Line(0) = %q, want every name", all)
	}
	if none := (RenameSummary{}).Line(5); none != "" {
		t.Errorf("zero summary Line = %q, want empty", none)
	}
	full := RenameSummary{Fields: 2, Renamed: 2}
	if got := full.Line(5); got != "2 of 2 field(s) renamed" {
		t.Errorf("Line with nothing unchanged = %q", got)
	}
}
