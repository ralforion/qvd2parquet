package convert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ralforion/qvd2parquet/internal/parquetwrite"
	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

func init() {
	// Every test with a broken input retries it, and none of them is about
	// the wait.
	retryWait = 0
}

// withRetryHook installs fn as beforeRetry for one test.
func withRetryHook(t *testing.T, fn func()) {
	t.Helper()
	prev := beforeRetry
	beforeRetry = fn
	t.Cleanup(func() { beforeRetry = prev })
}

// A file another process was still writing fails its first attempt and
// converts on the second, once the writer has finished. Both attempts stay on
// the record.
func TestRunManyRetriesAFileThatWasBeingWritten(t *testing.T) {
	src := folderFixture(t, false, true)
	broken := filepath.Join(src, "broken.qvd")
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	logPath := filepath.Join(dir, "run.jsonl")

	// The copy job finishes while the batch is converting the other files.
	withRetryHook(t, func() {
		if _, err := qvdtest.Build(broken, sampleTable(30)); err != nil {
			t.Fatal(err)
		}
	})
	log, err := NewLogWriter(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	inputs := FindInputs([]string{src}, InputSelection{}).Files
	opts := testOptions()
	many := &ManyOptions{OutDir: outDir, Log: log, SkipUpToDate: true, ToolVersion: "3.1.0"}
	b, err := RunMany(context.Background(), inputs, &opts, many, logf)
	if err != nil {
		t.Fatalf("RunMany: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	if b.Converted != 3 || b.Failed != 0 || b.Retried != 1 {
		t.Fatalf("converted=%d failed=%d retried=%d, want 3, 0 and 1:\n%s",
			b.Converted, b.Failed, b.Retried, b.Summary())
	}
	if code := b.ExitCode(testExitCode); code != 0 {
		t.Errorf("exit code %d, want 0: the file converted", code)
	}
	if b.Rows != 130 {
		t.Errorf("rows = %d, want 130", b.Rows)
	}
	sum := b.Summary()
	if !strings.Contains(sum, "1 retried") || !strings.Contains(sum, "CONVERTED ON RETRY (1)") ||
		!strings.Contains(sum, "broken.qvd") {
		t.Errorf("the summary must name the file that needed a retry:\n%s", sum)
	}
	if strings.Contains(sum, "FAILED") {
		t.Errorf("nothing failed in the end:\n%s", sum)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "retry ") {
		t.Errorf("the console must say a file is being retried:\n%s", strings.Join(lines, "\n"))
	}

	var r FileResult
	for _, x := range b.Results {
		if x.Input == broken {
			r = x
		}
	}
	if r.FirstErr == nil || r.Err != nil {
		t.Fatalf("broken.qvd: first=%v err=%v, want a first failure and no final one", r.FirstErr, r.Err)
	}

	// Two records for the retried file: attempt 1 failed, attempt 2 ok.
	attempts := map[float64]string{}
	var summary map[string]any
	for _, line := range logLines(t, logPath) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["type"] == "summary" {
			summary = rec
			continue
		}
		if _, ok := rec["attempt"]; !ok {
			t.Errorf("record without attempt: %s", line)
		}
		if rec["input"] == broken {
			attempts[rec["attempt"].(float64)] = rec["status"].(string)
		}
	}
	if attempts[1] != "failed" || attempts[2] != "ok" || len(attempts) != 2 {
		t.Errorf("broken.qvd records = %v, want attempt 1 failed and attempt 2 ok", attempts)
	}
	if summary["retried"] != float64(1) {
		t.Errorf("summary retried = %v, want 1", summary["retried"])
	}

	// The manifest records the second attempt like any conversion.
	many.Log = nil // closed above
	again, err := RunMany(context.Background(), inputs, &opts, many, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Skipped != 3 {
		t.Errorf("next run skipped %d, want 3: the retried file is up to date", again.Skipped)
	}
}

// The second attempt waits until retryWait has passed since the first one
// ended, so a short batch does not retry into the same unfinished copy.
func TestRetryWaitsAfterTheFirstAttempt(t *testing.T) {
	src := folderFixture(t, false, true)
	prevWait := retryWait
	retryWait = 400 * time.Millisecond
	t.Cleanup(func() { retryWait = prevWait })

	inputs := FindInputs([]string{src}, InputSelection{}).Files
	opts := testOptions()
	b, err := RunMany(context.Background(), inputs, &opts, &ManyOptions{OutDir: filepath.Join(t.TempDir(), "out")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range b.Results {
		if !r.Retried() {
			continue
		}
		// The first attempt ended after the run started, so the second
		// beginning retryWait after it is at least that far into the run.
		if gap := r.Started.Sub(time.Now().Add(-b.Elapsed)); gap < retryWait {
			t.Errorf("second attempt began %s into the run, want at least %s", gap, retryWait)
		}
		return
	}
	t.Fatal("broken.qvd was not retried")
}

// A file that fails twice is a failure, reported with the second error and
// marked as retried.
func TestRunManyReportsAFileThatFailsTheRetryToo(t *testing.T) {
	src := folderFixture(t, false, true)
	inputs := FindInputs([]string{src}, InputSelection{}).Files
	opts := testOptions()
	b, err := RunMany(context.Background(), inputs, &opts, &ManyOptions{OutDir: filepath.Join(t.TempDir(), "out")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Converted != 2 || b.Failed != 1 || b.Retried != 1 {
		t.Fatalf("converted=%d failed=%d retried=%d, want 2, 1 and 1", b.Converted, b.Failed, b.Retried)
	}
	if code := b.ExitCode(testExitCode); code == 0 {
		t.Error("a file that failed twice must not exit 0")
	}
	sum := b.Summary()
	if !strings.Contains(sum, "FAILED (1)") || !strings.Contains(sum, "(failed on retry too)") {
		t.Errorf("summary:\n%s", sum)
	}
	if strings.Contains(sum, "CONVERTED ON RETRY") {
		t.Errorf("nothing converted on retry:\n%s", sum)
	}
}

// A cancelled batch retries nothing, and the first failure stands.
func TestCancelledRunDoesNotRetry(t *testing.T) {
	src := folderFixture(t, false, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	withRetryHook(t, func() { t.Error("a cancelled batch started retrying") })

	inputs := FindInputs([]string{src}, InputSelection{}).Files
	opts := testOptions()
	results := []FileResult{{Input: inputs[0], Err: fmt.Errorf("%w: short read", ErrInput)}}
	cancel()
	retryFailed(ctx, results, &opts, &ManyOptions{OutDir: t.TempDir()}, watchStalls(ctx, 0, nil, nil),
		func(FileResult) {},
		func(FileResult) {}, func(string, ...any) {})
	if results[0].Retried() || !errors.Is(results[0].Err, ErrInput) {
		t.Errorf("result = %+v, want the first failure unchanged", results[0])
	}
}

// Only a read or decode failure is retried. The rest are of the run's own
// making and would fail the same way again.
func TestRetryableFailures(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{fmt.Errorf("%w: row 485432: decoded symbol id 7166", ErrInput), true},
		{readFailure{fmt.Errorf("read symbols for column %q: %w", "X", errors.New("unexpected EOF"))}, true},
		{readFailure{fmt.Errorf("%w: unknown symbol tag 0x40", qvd.ErrUnsupported)}, true},
		{readFailure{errors.New("no XML header terminator (0x00) found: not a QVD file?")}, true},
		// Not known to be a read of the input: not retried.
		{fmt.Errorf("%w: compression %q", qvd.ErrUnsupported, "zlib"), false},
		{errors.New("write quality report: open q.json: not a directory"), false},
		{errors.New(`schema override: column "X" does not exist`), false},
		{errors.New("something nobody classified"), false},
		{fmt.Errorf("%w: rows 0..10", qvd.ErrUnstableRead), true},
		{fmt.Errorf("%w: wrapped %w", ErrQualityGate, qvd.ErrUnstableRead), true},
		{fmt.Errorf("%w: mixed column", ErrSchemaPolicy), false},
		{fmt.Errorf("%w: disk full", parquetwrite.ErrOutput), false},
		{fmt.Errorf("x: %w", parquetwrite.ErrOutputLost), false},
		{fmt.Errorf("%w: sum differs", ErrQualityGate), false},
		{fmt.Errorf("%w before this file was converted", ErrCanceled), false},
		{context.Canceled, false},
	} {
		if got := retryable(c.err); got != c.want {
			t.Errorf("retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// A failure that is not a read of the input is not retried: the second
// attempt would repeat the whole conversion only to fail the same way.
func TestRunManyDoesNotRetryFailuresOfItsOwnMaking(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(t *testing.T, opts *Options)
	}{
		{"quality report cannot be written", func(t *testing.T, opts *Options) {
			notADir := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(notADir, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			opts.Quality = QualityNumeric
			opts.QualityReportPath = filepath.Join(notADir, "q.json")
		}},
		{"schema override does not apply", func(t *testing.T, opts *Options) {
			p := filepath.Join(t.TempDir(), "schema.json")
			if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
			opts.SchemaOverridePath = p
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := folderFixture(t, false, false)
			withRetryHook(t, func() { t.Error("nothing here is a read failure, yet retries started") })
			inputs := FindInputs([]string{src}, InputSelection{}).Files
			opts := testOptions()
			c.set(t, &opts)
			b, err := RunMany(context.Background(), inputs, &opts,
				&ManyOptions{OutDir: filepath.Join(t.TempDir(), "out")}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if b.Failed != 2 || b.Retried != 0 {
				t.Errorf("failed=%d retried=%d, want 2 and 0:\n%s", b.Failed, b.Retried, b.Summary())
			}
		})
	}
}

// A retry that loses an output while the run is being cancelled reports the
// loss. Dropping it as a cancellation would leave the old read error and exit
// 4 standing for a file whose output is gone.
func TestRetryThatLosesAnOutputDuringCancellationIsReported(t *testing.T) {
	src := folderFixture(t, false, true)
	broken := filepath.Join(src, "broken.qvd")
	outDir := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "broken.parquet"), []byte("old output"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Between the attempts the file is repaired, and from then on the share
	// refuses every rename. The first refusal sends --force to delete the
	// existing output and rename again; Ctrl-C arrives during that second
	// rename, which fails too, and the output is lost.
	withRetryHook(t, func() {
		if _, err := qvdtest.Build(broken, sampleTable(30)); err != nil {
			t.Fatal(err)
		}
		renames := 0
		restore := parquetwrite.SetRenameForTest(func(from, to string) error {
			if renames++; renames == 2 {
				cancel()
			}
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrPermission}
		})
		t.Cleanup(restore)
	})

	inputs := FindInputs([]string{src}, InputSelection{}).Files
	opts := testOptions()
	opts.Force = true
	b, err := RunMany(ctx, inputs, &opts, &ManyOptions{OutDir: outDir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var r FileResult
	for _, x := range b.Results {
		if x.Input == broken {
			r = x
		}
	}
	if !r.Retried() || !errors.Is(r.Err, parquetwrite.ErrOutputLost) {
		t.Fatalf("broken.qvd: retried=%v err=%v, want the retry's ErrOutputLost", r.Retried(), r.Err)
	}
	if b.Retried != 1 {
		t.Errorf("retried = %d, want 1", b.Retried)
	}
	if code := b.ExitCode(testExitCode); code != 5 {
		t.Errorf("exit code %d, want 5 for the lost output", code)
	}
}
