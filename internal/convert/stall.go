package convert

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ralforion/qvd2parquet/internal/iowatch"
)

// ErrStalled marks a run stopped because a read or write on one of its files
// did not return.
var ErrStalled = errors.New("stalled")

// DefaultStallTimeout is how long one read or write may take before the run
// stops. No real call on a working disk or share comes near it, and a slow
// conversion is not a stalled one: the limit applies to each call, not to the
// file.
const DefaultStallTimeout = 10 * time.Minute

// stallGrace is how long the conversions still running after a stall get to
// wind down before they too are given up on. They are cancelled, and one that
// is not itself stuck stops at its next chunk.
var stallGrace = 10 * time.Second

// SetStallGraceForTest replaces stallGrace and returns a function that
// restores it. It exists for tests in other packages; nothing else calls it.
func SetStallGraceForTest(d time.Duration) (restore func()) {
	saved := stallGrace
	stallGrace = d
	return func() { stallGrace = saved }
}

// stallWatch looks for a read or write that has not returned in time.
//
// A call the operating system never completes cannot be interrupted from the
// program: a write to a stalled share sits in the kernel, and Ctrl-C only asks
// the run to stop at its next step, which it never reaches. So the watch does
// what can be done. It names the call, cancels the run, and lets the caller
// give up on the conversion that made it, so the files and the logs are
// accounted for and the process can exit with a code a scheduled job can act
// on, rather than holding the job for as long as the share takes to recover.
type stallWatch struct {
	timeout time.Duration
	once    sync.Once
	fired   chan struct{}
	call    iowatch.Call

	mu     sync.Mutex
	gaveUp map[string]bool // shutdown work given up on, by kind
}

// watchStalls starts the watch, which ends with life. onStall runs once, when
// a call has been in flight for timeout. A zero timeout watches nothing.
//
// life is the caller's own lifetime, not the run's context. A cancelled run
// still has to finish, and Ctrl-C is no help with a call stuck in the
// kernel: the conversion waits on it all the same, so the watch has to go on
// looking until the caller returns.
func watchStalls(life context.Context, timeout time.Duration, logf Logf, onStall func(iowatch.Call)) *stallWatch {
	w := &stallWatch{timeout: timeout, fired: make(chan struct{}), gaveUp: map[string]bool{}}
	if timeout <= 0 {
		return w
	}
	every := min(30*time.Second, timeout/4)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-life.Done():
				return
			case <-t.C:
			}
			c, ok := iowatch.Stuck(timeout)
			if !ok {
				continue
			}
			w.once.Do(func() {
				w.call = c
				logf("stalled: %s has not returned in %s (started %s); stopping the run. "+
					"The call is waiting in the operating system and cannot be interrupted; "+
					"if this process does not exit, end it with %s",
					c, timeoutString(timeout), c.Start.Format("15:04:05"), killHint())
				onStall(c)
				close(w.fired)
			})
			return
		}
	}()
	return w
}

// await runs convert and returns what it returns, unless the watch fires
// first and the conversion is still running stallGrace later. Then it returns
// false and leaves the conversion where it is: blocked in a call nothing can
// end, and cancelled, so it commits nothing if the call ever returns.
func await[T any](w *stallWatch, convert func() T) (T, bool) {
	done := make(chan T, 1)
	go func() { done <- convert() }()
	select {
	case r := <-done:
		return r, true
	case <-w.fired:
	}
	select {
	case r := <-done:
		return r, true
	case <-time.After(stallGrace):
		var zero T
		return zero, false
	}
}

// stalled reports whether the watch has fired.
func (w *stallWatch) stalled() bool {
	select {
	case <-w.fired:
		return true
	default:
		return false
	}
}

// shutdown runs fn, the writing a run does as it finishes: a log record, the
// manifest. Before a stall it simply runs. After one it goes through
// AfterStall, since the log and the manifest can sit on the share that
// stalled, and once work of a kind has been given up on, the rest of that
// kind is skipped at once rather than each waiting out the grace period on
// the same share. It reports whether fn ran to the end.
func (w *stallWatch) shutdown(kind, what string, logf Logf, fn func()) bool {
	if w == nil || !w.stalled() {
		fn()
		return true
	}
	w.mu.Lock()
	skip := w.gaveUp[kind]
	w.mu.Unlock()
	if skip {
		return false
	}
	if AfterStall(what, logf, fn) {
		return true
	}
	w.mu.Lock()
	w.gaveUp[kind] = true
	w.mu.Unlock()
	return false
}

// AfterStall runs fn as part of shutting down a run that stalled: on its own
// goroutine, given up on if it has not returned after the grace period. What
// is left to write after a stall, the log, the manifest, the catalog, may sit
// on the same stuck share, and the process still has to exit. It reports
// whether fn ran to the end.
func AfterStall(what string, logf Logf, fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(stallGrace):
		if logf != nil {
			logf("note: gave up on %s, which had not returned after %s either", what, timeoutString(stallGrace))
		}
		return false
	}
}

// convertFile runs one conversion of a batch under the watch, and reports it
// as stalled if the watch gives up on it.
func (w *stallWatch) convertFile(in, out string, logf Logf, convert func() FileResult) FileResult {
	started := time.Now()
	r, ok := await(w, convert)
	if ok {
		return r
	}
	r = FileResult{Input: in, Output: out, Started: started, Elapsed: time.Since(started),
		Err: w.abandoned(in, out)}
	logf("FAIL %s: %s", DisplayPath(in), r.Err)
	return r
}

// abandoned is the error of a conversion given up on after the stall: the one
// that made the stuck call, or one that did not wind down in time after it.
func (w *stallWatch) abandoned(input, output string) error {
	if w.call.Path == input || strings.HasPrefix(w.call.Path, output) {
		return fmt.Errorf("%w: %s did not return in %s (started %s); the run stopped here",
			ErrStalled, w.call, timeoutString(w.timeout), w.call.Start.Format("15:04:05"))
	}
	return fmt.Errorf("%w: not finished when the run stopped after %s did not return", ErrStalled, w.call)
}

// stalledCause is why a run was cancelled after a stall.
type stalledCause struct{ call iowatch.Call }

func (s *stalledCause) Error() string {
	return fmt.Sprintf("the run stopped after %s did not return", s.call)
}

// RunWatched is Run under a stall watch: if a read or write on the input or
// the output has not returned after timeout, it reports the conversion as
// stalled and returns, leaving the stuck call behind. A zero timeout is Run.
func RunWatched(ctx context.Context, inputPath, outputPath string, opts *Options,
	timeout time.Duration, logf Logf) (*Stats, *QualityReport, error) {

	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	life, endWatch := context.WithCancel(context.Background())
	defer endWatch()
	w := watchStalls(life, timeout, logf, func(c iowatch.Call) { stop(&stalledCause{c}) })
	type result struct {
		stats   *Stats
		quality *QualityReport
		err     error
	}
	r, ok := await(w, func() result {
		s, q, err := Run(ctx, inputPath, outputPath, opts, logf)
		return result{s, q, stoppedErr(ctx, err)}
	})
	if !ok {
		return nil, nil, w.abandoned(inputPath, outputPath)
	}
	return r.stats, r.quality, r.err
}

// timeoutString renders a timeout the way it was most likely given: 10m, not
// 10m0s.
func timeoutString(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// killHint is the command that ends this process by force.
func killHint() string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("taskkill /F /PID %d", os.Getpid())
	}
	return fmt.Sprintf("kill -9 %d", os.Getpid())
}
