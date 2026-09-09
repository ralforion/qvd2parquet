package convert

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

func rereadFixture(t *testing.T, symbols []qvd.Symbol, rows []int) string {
	t.Helper()
	return buildFixture(t, qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Name", Type: "ASCII", Rows: rows, Symbols: symbols},
		{Name: "N", Type: "INTEGER", Rows: rows,
			Symbols: []qvd.Symbol{qvdtest.Int(10), qvdtest.Int(20), qvdtest.Int(30)}},
	}})
}

// The whole point: a second pass over the same bytes must agree with the first.
func TestRereadAgreesWithItself(t *testing.T) {
	in := rereadFixture(t,
		[]qvd.Symbol{qvdtest.Str("a"), qvdtest.Str("b"), qvdtest.Str("c")},
		[]int{0, 1, 2, 1})
	_, rs, first := reconvert(t, in, ptr(testOptions()))

	opts := testOptions()
	opts.Quality = QualityFull
	again, err := RereadSourceMetrics(context.Background(), in, rs, &opts, nil)
	if err != nil {
		t.Fatalf("RereadSourceMetrics: %v", err)
	}
	if diffs := CompareSourceReads(first, again); len(diffs) > 0 {
		t.Errorf("two reads of the same file disagree: %v", diffs)
	}
}

// A record byte read wrong points at a different symbol, and the value that
// comes back is entirely well formed. Nothing but a second read can see it,
// which is what this checks: same schema, same row count, different values.
func TestCompareSourceReadsCatchesADifferentValue(t *testing.T) {
	syms := []qvd.Symbol{qvdtest.Str("a"), qvdtest.Str("b"), qvdtest.Str("c")}
	a := rereadFixture(t, syms, []int{0, 1, 2, 1})
	b := rereadFixture(t, syms, []int{0, 1, 2, 2}) // one row points elsewhere

	_, _, first := reconvert(t, a, ptr(testOptions()))
	_, _, second := reconvert(t, b, ptr(testOptions()))

	diffs := CompareSourceReads(first, second)
	if len(diffs) == 0 {
		t.Fatal("a changed symbol index went undetected")
	}
	joined := strings.Join(diffs, "\n")
	if !strings.Contains(joined, `column "Name"`) || !strings.Contains(joined, "fingerprint") {
		t.Errorf("diffs do not name the column and what differs: %v", diffs)
	}
	if first.Rows != second.Rows {
		t.Fatal("this test is meaningless unless the row counts match")
	}
}

func TestCompareSourceReadsCatchesARowCount(t *testing.T) {
	syms := []qvd.Symbol{qvdtest.Str("a"), qvdtest.Str("b"), qvdtest.Str("c")}
	_, _, first := reconvert(t, rereadFixture(t, syms, []int{0, 1, 2}), ptr(testOptions()))
	_, _, second := reconvert(t, rereadFixture(t, syms, []int{0, 1}), ptr(testOptions()))

	diffs := CompareSourceReads(first, second)
	if len(diffs) == 0 || !strings.Contains(diffs[0], "row count differs") {
		t.Errorf("diffs = %v, want a row count difference", diffs)
	}
}

// --quality reread converts a healthy file and says the passes agree.
func TestQualityRereadConverts(t *testing.T) {
	in := rereadFixture(t,
		[]qvd.Symbol{qvdtest.Str("a"), qvdtest.Str("b"), qvdtest.Str("c")},
		[]int{0, 1, 2, 1})
	out := filepath.Join(t.TempDir(), "out.parquet")

	var lines []string
	opts := testOptions()
	opts.Quality = QualityReread
	stats, report, err := Run(context.Background(), in, out, &opts,
		func(f string, a ...any) { lines = append(lines, f) })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Rows != 4 {
		t.Errorf("rows = %d, want 4", stats.Rows)
	}
	if report == nil || !report.Passed {
		t.Errorf("quality gate did not pass: %+v", report)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "reread source") {
		t.Errorf("the second pass was not reported: %v", lines)
	}
}

func TestParseQualityModeReread(t *testing.T) {
	got, err := ParseQualityMode("reread")
	if err != nil || got != QualityReread {
		t.Fatalf("ParseQualityMode(reread) = %v, %v", got, err)
	}
	if got.String() != "reread" {
		t.Errorf("String = %q", got.String())
	}
	if QualityReread <= QualityFull {
		t.Error("reread must rank above full so fingerprints stay on")
	}
}

func ptr(o Options) *Options { return &o }
