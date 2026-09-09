package convert

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// runVerifying converts under the mode that checks every read, and asserts the
// output is there on success and gone on failure.
func runVerifying(t *testing.T, in string, batchRows int) error {
	t.Helper()
	opts := testOptions()
	opts.Quality = QualityReread
	if batchRows > 0 {
		opts.BatchRows = batchRows
	}
	out := filepath.Join(t.TempDir(), "out.parquet")
	_, _, err := Run(context.Background(), in, out, &opts, nil)
	_, serr := os.Stat(out)
	switch {
	case err == nil && serr != nil:
		t.Fatalf("conversion reported success but wrote nothing: %v", serr)
	case err != nil && serr == nil:
		t.Error("a failed verification left an output file behind")
	}
	return err
}

// The ordinary case: every range reads the same way twice.
func TestVerifyingRunConverts(t *testing.T) {
	if err := runVerifying(t, verifyFixture(t, []int{0, 1, 2, 1}), 0); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// A record byte that reads differently stops the conversion at that chunk,
// naming the offset and both values. The verifying read is made to return
// something else, which is the situation the check exists for and the one
// thing a real file cannot be made to do on demand.
func TestVerifyingRunStopsOnAChangedRecord(t *testing.T) {
	rows := make([]int, 400)
	for i := range rows {
		rows[i] = i % 3
	}
	in := verifyFixture(t, rows)

	restore := openVerifyReader
	openVerifyReader = func(qf *qvd.File) (io.ReaderAt, func(), error) {
		r, closeFn, err := restore(qf)
		if err != nil {
			return nil, nil, err
		}
		return flipAt{r: r, at: qf.RecordStart + 3}, closeFn, nil
	}
	defer func() { openVerifyReader = restore }()

	err := runVerifying(t, in, 64)
	if err == nil {
		t.Fatal("a record byte that read differently was not caught")
	}
	if !errors.Is(err, qvd.ErrUnstableRead) {
		t.Errorf("error = %v, want ErrUnstableRead", err)
	}
	for _, want := range []string{"byte at offset", "read 0x"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// Failing at the first bad chunk is the point of checking per chunk: the rest
// of the file is not decoded once nothing read from it can be trusted.
func TestVerifyingRunStopsEarly(t *testing.T) {
	rows := make([]int, 4000)
	for i := range rows {
		rows[i] = i % 3
	}
	in := verifyFixture(t, rows)

	var chunksRead int64
	restore := openVerifyReader
	openVerifyReader = func(qf *qvd.File) (io.ReaderAt, func(), error) {
		r, closeFn, err := restore(qf)
		if err != nil {
			return nil, nil, err
		}
		return countingFlip{r: r, at: qf.RecordStart + 3, n: &chunksRead}, closeFn, nil
	}
	defer func() { openVerifyReader = restore }()

	if err := runVerifying(t, in, 64); err == nil {
		t.Fatal("want a failure")
	}
	// 4000 rows at 64 per chunk is 63 chunks; a run that carried on would
	// verify most of them.
	if chunksRead > 32 {
		t.Errorf("verified %d chunks after the first failure; it should stop early", chunksRead)
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

// flipAt reads through to r and changes one byte, standing in for a range that
// comes back different the second time.
type flipAt struct {
	r  io.ReaderAt
	at int64
}

func (f flipAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.r.ReadAt(p, off)
	if i := f.at - off; i >= 0 && i < int64(n) {
		p[i] ^= 0x01
	}
	return n, err
}

// countingFlip is flipAt that also counts how many ranges were verified.
type countingFlip struct {
	r  io.ReaderAt
	at int64
	n  *int64
}

func (f countingFlip) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.r.ReadAt(p, off)
	atomic.AddInt64(f.n, 1)
	if i := f.at - off; i >= 0 && i < int64(n) {
		p[i] ^= 0x01
	}
	return n, err
}

// A verifier that cannot be opened must fail the conversion, not quietly skip
// the check. Handle exhaustion in a long batch is exactly where this would
// happen, and exactly where the check is wanted.
func TestVerifyingRunFailsWhenTheVerifierCannotOpen(t *testing.T) {
	in := verifyFixture(t, []int{0, 1, 2, 1})

	restore := openVerifyReader
	openVerifyReader = func(qf *qvd.File) (io.ReaderAt, func(), error) {
		return nil, nil, errors.New("too many open files")
	}
	defer func() { openVerifyReader = restore }()

	err := runVerifying(t, in, 0)
	if err == nil {
		t.Fatal("the conversion succeeded with no verification performed")
	}
	if !strings.Contains(err.Error(), "too many open files") {
		t.Errorf("error = %v, want the opener's failure", err)
	}
}
