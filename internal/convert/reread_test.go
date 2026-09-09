package convert

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

func verifyFixture(t *testing.T, rows []int) string {
	t.Helper()
	return buildFixture(t, qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Name", Type: "ASCII", Rows: rows,
			Symbols: []qvd.Symbol{qvdtest.Str("alpha"), qvdtest.Str("beta"), qvdtest.Str("gamma")}},
		{Name: "N", Type: "INTEGER", Rows: rows,
			Symbols: []qvd.Symbol{qvdtest.Int(10), qvdtest.Int(20), qvdtest.Int(30)}},
	}})
}

// converts and returns the digests the run recorded.
func convertVerifying(t *testing.T, in, out string) (*qvd.File, []DecodeChunk, [][32]byte) {
	t.Helper()
	opts := testOptions()
	opts.Quality = QualityReread
	f, err := qvd.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	f.VerifyReads = true
	if err := f.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		t.Fatal(err)
	}
	rs, err := ResolveSchema(f, &opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := NewConverter(f, rs, &opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conv.Run(context.Background(), discardSink{}, nil); err != nil {
		t.Fatal(err)
	}
	chunks, digests := conv.ReadDigests()
	if len(digests) == 0 {
		t.Fatal("no chunk digests were recorded")
	}
	return f, chunks, digests
}

// The ordinary case: what was read is what the file holds.
func TestVerifySourceReadsAgrees(t *testing.T) {
	in := verifyFixture(t, []int{0, 1, 2, 1})
	f, chunks, digests := convertVerifying(t, in, "")
	defer f.Close()

	diffs, err := VerifySourceReads(context.Background(), in, f, chunks, digests, nil)
	if err != nil {
		t.Fatalf("VerifySourceReads: %v", err)
	}
	if len(diffs) > 0 {
		t.Errorf("a file that did not change reported differences: %v", diffs)
	}
}

// A record byte that reads differently the second time is caught, and reported
// as a byte range rather than as a column: an offset is what anyone chasing
// this below our level can act on.
func TestVerifySourceReadsCatchesChangedRecords(t *testing.T) {
	in := verifyFixture(t, []int{0, 1, 2, 1})
	f, chunks, digests := convertVerifying(t, in, "")
	defer f.Close()

	flipByte(t, in, f.RecordStart)

	diffs, err := VerifySourceReads(context.Background(), in, f, chunks, digests, nil)
	if err != nil {
		t.Fatalf("VerifySourceReads: %v", err)
	}
	if len(diffs) == 0 {
		t.Fatal("a changed record byte went undetected")
	}
	if !strings.Contains(diffs[0], "records for rows") || !strings.Contains(diffs[0], "offset") {
		t.Errorf("diff does not name the range: %v", diffs)
	}
}

// A symbol byte matters more than a record byte: every row referring to it
// carries the wrong value.
func TestVerifySourceReadsCatchesChangedSymbols(t *testing.T) {
	in := verifyFixture(t, []int{0, 1, 2, 1})
	f, chunks, digests := convertVerifying(t, in, "")
	defer f.Close()

	// Somewhere inside the first column's symbol table.
	flipByte(t, in, f.SymbolRanges()[0].Offset+2)

	diffs, err := VerifySourceReads(context.Background(), in, f, chunks, digests, nil)
	if err != nil {
		t.Fatalf("VerifySourceReads: %v", err)
	}
	if len(diffs) == 0 {
		t.Fatal("a changed symbol byte went undetected")
	}
	if !strings.Contains(diffs[0], `symbol table of column "Name"`) {
		t.Errorf("diff does not name the column: %v", diffs)
	}
}

func TestVerifySourceReadsRefusesAReplacedFile(t *testing.T) {
	in := verifyFixture(t, []int{0, 1})
	f, chunks, digests := convertVerifying(t, in, "")
	defer f.Close()

	other := verifyFixture(t, []int{0, 1, 2})
	if _, err := VerifySourceReads(context.Background(), other, f, chunks, digests, nil); err == nil ||
		!strings.Contains(err.Error(), "replaced") {
		t.Errorf("error = %v, want it to say the file was replaced", err)
	}
}

// End to end: a healthy file converts under the mode and says so.
func TestQualityRereadConverts(t *testing.T) {
	in := verifyFixture(t, []int{0, 1, 2, 1})
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
	if !strings.Contains(strings.Join(lines, "\n"), "verified source read") {
		t.Errorf("the verification was not reported: %v", lines)
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

// flipByte changes one byte of a file in place, standing in for a read that
// came back different.
func flipByte(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0x01
	if _, err := f.WriteAt(b[:], off); err != nil {
		t.Fatal(err)
	}
}

// Every mode from full up compares value fingerprints. This was an equality
// check against full, so reread quietly graded like numeric: it collected the
// fingerprints and never looked at them.
func TestFingerprintsAreComparedFromFullUpwards(t *testing.T) {
	in := buildFixture(t, sampleTable(300))
	dir := t.TempDir()
	out := filepath.Join(dir, "out.parquet")
	opts := testOptions()
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []QualityMode{QualityFull, QualityReread} {
		t.Run(mode.String(), func(t *testing.T) {
			qf, rs, metrics := reconvert(t, in, &opts)
			defer qf.Close()

			// Everything else about the source still matches the Parquet, so
			// only a fingerprint comparison can catch this.
			metrics.Columns[0].fp.add([32]byte{1, 2, 3})

			o := opts
			o.Quality = mode
			report, err := RunQualityGate(context.Background(), in, out, out, rs, metrics, &o, nil)
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed {
				t.Fatalf("%s did not compare value fingerprints", mode)
			}
			if !strings.Contains(strings.Join(report.Columns[0].Errors, " "), "fingerprint differs") {
				t.Errorf("errors = %v", report.Columns[0].Errors)
			}
		})
	}
}
