package convert

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ralforion/qvd2parquet/internal/iowatch"
	"github.com/ralforion/qvd2parquet/internal/parquetwrite"
)

// holdConversion makes the conversion of the input named base wait inside a
// write to stuck that does not return, until the test ends. The write is
// registered with iowatch exactly as a real one is. It returns a channel
// closed once the held conversion has finished, after the test released it.
func holdConversion(t *testing.T, base, stuck string) <-chan struct{} {
	t.Helper()
	release := make(chan struct{})
	finished := make(chan struct{})
	prev := runHook
	runHook = func(in string) func() {
		if filepath.Base(in) != base {
			return func() {}
		}
		end := iowatch.Begin("write", stuck)
		<-release
		end()
		return func() { close(finished) }
	}
	prevGrace := stallGrace
	stallGrace = 100 * time.Millisecond
	t.Cleanup(func() {
		close(release)
		// The held conversion runs on once released, cancelled; it must be
		// done before the next test, or before TempDir removes its files.
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("the held conversion did not finish after being released")
		}
		runHook = prev
		stallGrace = prevGrace
	})
	return finished
}

// syncLines collects log lines from goroutines that outlive the run.
type syncLines struct {
	mu    sync.Mutex
	lines []string
}

func (s *syncLines) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

func (s *syncLines) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}

// A write that never returns stops the batch: the file is reported as
// stalled, the files after it as not converted, nothing is retried, and the
// run returns instead of waiting on the call.
func TestRunManyStopsOnAStalledWrite(t *testing.T) {
	src := folderFixture(t, false, false) // a.qvd and b.qvd
	outDir := filepath.Join(t.TempDir(), "out")
	holdConversion(t, "a.qvd", filepath.Join(outDir, "a.parquet.tmp-1"))
	var lines syncLines
	retried := false
	withRetryHook(t, func() { retried = true })

	inputs := FindInputs([]string{src}, InputSelection{}).Files
	opts := testOptions()
	many := &ManyOptions{OutDir: outDir, StallTimeout: 300 * time.Millisecond,
		SkipUpToDate: true, ToolVersion: "3.1.0"}
	start := time.Now()
	b, err := RunMany(context.Background(), inputs, &opts, many, lines.logf)
	if err != nil {
		t.Fatalf("RunMany: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the run took %s; it must return soon after the stall", took)
	}

	if b.Converted != 0 || b.Failed != 2 {
		t.Fatalf("converted=%d failed=%d, want 0 and 2:\n%s", b.Converted, b.Failed, b.Summary())
	}
	if code := b.ExitCode(testExitCode); code != 8 {
		t.Errorf("exit code %d, want 8", code)
	}
	if retried {
		t.Error("a stalled run must not retry anything")
	}
	for _, r := range b.Results {
		if !errors.Is(r.Err, ErrStalled) {
			t.Errorf("%s: %v, want ErrStalled", filepath.Base(r.Input), r.Err)
		}
		switch filepath.Base(r.Input) {
		case "a.qvd":
			if !strings.Contains(r.Err.Error(), "write to ") || !strings.Contains(r.Err.Error(), "did not return in 300ms") {
				t.Errorf("a.qvd: %v, want the stuck write named", r.Err)
			}
		case "b.qvd":
			if !strings.Contains(r.Err.Error(), "not converted") {
				t.Errorf("b.qvd: %v, want not converted", r.Err)
			}
		}
	}
	out := lines.String()
	if !strings.Contains(out, "stalled: write to ") || !strings.Contains(out, killHint()) {
		t.Errorf("the console must name the stuck call and how to end the process:\n%s", out)
	}
}

// Without a timeout nothing is watched, and a slow conversion is just slow.
func TestRunManyWithoutStallTimeoutWaits(t *testing.T) {
	src := folderFixture(t, false, false)
	release := make(chan struct{})
	prev := runHook
	runHook = func(in string) func() {
		if filepath.Base(in) == "a.qvd" {
			<-release
		}
		return func() {}
	}
	t.Cleanup(func() { runHook = prev })
	time.AfterFunc(500*time.Millisecond, func() { close(release) })

	inputs := FindInputs([]string{src}, InputSelection{}).Files
	opts := testOptions()
	b, err := RunMany(context.Background(), inputs, &opts, &ManyOptions{OutDir: filepath.Join(t.TempDir(), "out")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Converted != 2 {
		t.Errorf("converted=%d, want 2:\n%s", b.Converted, b.Summary())
	}
}

// The single-file path stops the same way.
func TestRunWatchedStopsOnAStalledWrite(t *testing.T) {
	src := folderFixture(t, false, false)
	out := filepath.Join(t.TempDir(), "a.parquet")
	holdConversion(t, "a.qvd", out+".tmp-1")
	var lines syncLines
	opts := testOptions()
	_, _, err := RunWatched(context.Background(), filepath.Join(src, "a.qvd"),
		out, &opts, 300*time.Millisecond, lines.logf)
	if !errors.Is(err, ErrStalled) || !strings.Contains(err.Error(), "did not return in 300ms") {
		t.Errorf("err = %v, want the stalled write", err)
	}
	if testExitCode(err) != 8 {
		t.Errorf("exit code %d, want 8", testExitCode(err))
	}
}

// A stall is not something a second attempt in the same run can fix: the
// call is still stuck.
func TestStalledIsNotRetryable(t *testing.T) {
	if retryable(fmt.Errorf("%w: write to x did not return", ErrStalled)) {
		t.Error("a stalled file must not be retried")
	}
}

func TestTimeoutString(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Minute:        "10m",
		90 * time.Second:        "1m30s",
		2 * time.Hour:           "2h",
		300 * time.Millisecond:  "300ms",
		time.Hour + time.Minute: "1h1m",
	} {
		if got := timeoutString(d); got != want {
			t.Errorf("timeoutString(%s) = %q, want %q", d, got, want)
		}
	}
}

// A retry runs under the same watch: a retry that waits on a stuck share
// stops the run like a first attempt does, rather than hanging it.
func TestRetryThatStallsStopsTheRun(t *testing.T) {
	src := folderFixture(t, false, true)
	broken := filepath.Join(src, "broken.qvd")
	outDir := filepath.Join(t.TempDir(), "out")
	release := make(chan struct{})
	finished := make(chan struct{})
	attempts := 0
	prev, prevGrace := runHook, stallGrace
	stallGrace = 100 * time.Millisecond
	runHook = func(in string) func() {
		if in != broken {
			return func() {}
		}
		attempts++
		if attempts == 1 {
			return func() {} // the first attempt fails on its own: not a QVD
		}
		end := iowatch.Begin("write", filepath.Join(outDir, "broken.parquet.tmp-1"))
		<-release
		end()
		return func() { close(finished) }
	}
	t.Cleanup(func() {
		close(release)
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("the held retry did not finish after being released")
		}
		runHook, stallGrace = prev, prevGrace
	})
	withRetryHook(t, func() {})

	inputs := FindInputs([]string{src}, InputSelection{}).Files
	opts := testOptions()
	start := time.Now()
	b, err := RunMany(context.Background(), inputs, &opts,
		&ManyOptions{OutDir: outDir, StallTimeout: 300 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the run took %s; a stalled retry must not hold it", took)
	}
	var r FileResult
	for _, x := range b.Results {
		if x.Input == broken {
			r = x
		}
	}
	if !r.Retried() || !errors.Is(r.Err, ErrStalled) {
		t.Errorf("broken.qvd: retried=%v err=%v, want a stalled retry", r.Retried(), r.Err)
	}
	if b.Converted != 2 {
		t.Errorf("converted = %d, want the other two", b.Converted)
	}
	if code := b.ExitCode(testExitCode); code != 8 {
		t.Errorf("exit code %d, want 8", code)
	}
}

// Ctrl-C does not end the watch. The run is cancelled, but the conversion
// still waits on the stuck call, and only the watch can give up on it.
func TestCancelledRunStillStopsOnAStall(t *testing.T) {
	src := folderFixture(t, false, false)
	outDir := filepath.Join(t.TempDir(), "out")
	holdConversion(t, "a.qvd", filepath.Join(outDir, "a.parquet.tmp-1"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel) // Ctrl-C, while a.qvd is stuck

	inputs := FindInputs([]string{src}, InputSelection{}).Files
	opts := testOptions()
	done := make(chan *BatchResult, 1)
	go func() {
		b, err := RunMany(ctx, inputs, &opts, &ManyOptions{OutDir: outDir, StallTimeout: 300 * time.Millisecond}, nil)
		if err != nil {
			t.Error(err)
		}
		done <- b
	}()
	select {
	case b := <-done:
		if code := b.ExitCode(testExitCode); code != 8 {
			t.Errorf("exit code %d, want 8:\n%s", code, b.Summary())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled run hung on the stuck call")
	}
}

// After a stall, the manifest is written with a time limit: it sits in the
// output directory, which can be the share that stalled.
func TestManifestSaveAfterAStallIsGivenUpOn(t *testing.T) {
	src := folderFixture(t, false, false)
	outDir := filepath.Join(t.TempDir(), "out")
	holdConversion(t, "a.qvd", filepath.Join(outDir, "a.parquet.tmp-1"))
	release, returned := make(chan struct{}), make(chan struct{})
	prevSave := saveManifestFile
	saveManifestFile = func(m *Manifest, dir string) error {
		defer close(returned)
		<-release
		return nil
	}
	t.Cleanup(func() {
		// The save given up on is still running: restore only once it has
		// returned, or the restore races with it.
		close(release)
		<-returned
		saveManifestFile = prevSave
	})

	var lines syncLines
	inputs := FindInputs([]string{src}, InputSelection{}).Files
	opts := testOptions()
	done := make(chan *BatchResult, 1)
	go func() {
		b, err := RunMany(context.Background(), inputs, &opts, &ManyOptions{OutDir: outDir,
			StallTimeout: 300 * time.Millisecond, SkipUpToDate: true, ToolVersion: "3.1.0"}, lines.logf)
		if err != nil {
			t.Error(err)
		}
		done <- b
	}()
	select {
	case b := <-done:
		if code := b.ExitCode(testExitCode); code != 8 {
			t.Errorf("exit code %d, want 8", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run hung writing the manifest after the stall")
	}
	if out := lines.String(); !strings.Contains(out, "gave up on writing "+ManifestName) {
		t.Errorf("the console must say the manifest was given up on:\n%s", out)
	}
}

// A manifest save already stuck when another file stalls is given up on too.
// With two files converting at once, b.qvd finishes and blocks saving the
// manifest, then a.qvd stalls; the batch must not wait on b's save forever.
func TestBookkeepingStuckBeforeTheStallIsGivenUpOn(t *testing.T) {
	src := folderFixture(t, false, false)
	outDir := filepath.Join(t.TempDir(), "out")
	holdConversion(t, "a.qvd", filepath.Join(outDir, "a.parquet.tmp-1"))
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var first sync.Once
	prevSave := saveManifestFile
	saveManifestFile = func(m *Manifest, dir string) error {
		isFirst := false
		first.Do(func() { isFirst = true; close(entered) })
		<-release
		if isFirst {
			close(returned)
		}
		return nil
	}
	t.Cleanup(func() {
		// b's save is still running, given up on: restore only once it has
		// returned, or the restore races with it. Nothing saves after it,
		// since the manifest is given up on from then on.
		close(release)
		<-entered
		<-returned
		saveManifestFile = prevSave
	})

	inputs := FindInputs([]string{src}, InputSelection{}).Files
	opts := testOptions()
	done := make(chan *BatchResult, 1)
	go func() {
		b, err := RunMany(context.Background(), inputs, &opts, &ManyOptions{OutDir: outDir, FileWorkers: 2,
			StallTimeout: 300 * time.Millisecond, SkipUpToDate: true, ToolVersion: "3.1.0"}, nil)
		if err != nil {
			t.Error(err)
		}
		done <- b
	}()
	select {
	case b := <-done:
		if code := b.ExitCode(testExitCode); code != 8 {
			t.Errorf("exit code %d, want 8:\n%s", code, b.Summary())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the batch hung on a manifest save that was stuck before the stall")
	}
}

// A stuck call that returns during the grace period and then fails leaves the
// file stalled, with the later error kept. A rename that hung past the
// timeout and was then refused exited 5, as a plain output error.
func TestStallSurvivesACallThatReturnsDuringGrace(t *testing.T) {
	defer parquetwrite.SetRenameForTest(func(from, to string) error {
		time.Sleep(time.Second) // past the timeout, inside the grace period
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrPermission}
	})()
	prevGrace := stallGrace
	stallGrace = 5 * time.Second
	t.Cleanup(func() { stallGrace = prevGrace })

	check := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, ErrStalled) || !errors.Is(err, parquetwrite.ErrOutput) ||
			!strings.Contains(err.Error(), "rename of ") {
			t.Errorf("err = %v, want the stalled rename with the refusal after it", err)
		}
		if code := testExitCode(err); code != 8 {
			t.Errorf("exit code %d, want 8", code)
		}
	}
	t.Run("single file", func(t *testing.T) {
		src := folderFixture(t, false, false)
		opts := testOptions()
		_, _, err := RunWatched(context.Background(), filepath.Join(src, "a.qvd"),
			filepath.Join(t.TempDir(), "a.parquet"), &opts, 300*time.Millisecond, nil)
		check(t, err)
	})
	t.Run("batch", func(t *testing.T) {
		src := folderFixture(t, false, false)
		inputs := FindInputs([]string{src}, InputSelection{}).Files[:1]
		opts := testOptions()
		b, err := RunMany(context.Background(), inputs, &opts,
			&ManyOptions{OutDir: filepath.Join(t.TempDir(), "out"), StallTimeout: 300 * time.Millisecond}, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, b.Results[0].Err)
	})
}
