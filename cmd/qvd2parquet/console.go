package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// stderr is where every line of prose output goes. It is one fixed object
// rather than a variable the modes reassign: --console-log attaches its file
// to it after the path guards have run, and by then the signal handler is
// already writing through it from its own goroutine.
var stderr = &consoleLog{}

// consoleLog is the screen output, and --console-log's copy of it.
//
// It exists because the two logs answer different questions. The JSON log says
// what the run did -- a record per file, queryable. The screen says what
// happened while it did it: the note about rounded decimals, the pattern that
// matched nothing, the progress of the file that was still converting when the
// run was killed. Redirecting stderr with the shell does the same thing, and a
// scheduled job that runs the binary directly, through systemd or a Windows
// task, has no shell to do it with.
//
// The screen is written first, so the file never leads what the operator sees,
// and once the file is open nothing buffers: a run that is stopped or killed
// keeps every line it had printed, which is the same guarantee the JSON log
// now makes.
//
// Before the file is open there is a buffer, and it is not an optimization.
// The file is created by truncating, so it cannot be opened until the path
// guards have run, and by then the run has already printed its banner and any
// note about the inputs it selected -- a mistyped --include-files pattern
// among them, which is exactly the kind of line the operator later goes
// looking for. Those lines are held and replayed into the file when it opens,
// so the file holds what the screen held.
type consoleLog struct {
	mu   sync.Mutex
	f    *os.File // nil until --console-log attaches one, and again if it fails
	path string
	// pending is the output printed before the run decided whether there
	// would be a file. settled says that decision has been made, after which
	// nothing is held.
	pending []byte
	settled bool
	dropped int
}

// pendingLimit caps what is held before the file opens. Only the banner and
// the input notes are printed that early, so the cap is never reached in
// practice; it is here so a run that somehow prints a great deal before its
// guards have finished cannot grow the buffer without bound.
const pendingLimit = 1 << 20

// attach opens the file and starts copying into it. The caller checks the path
// against everything the run reads or writes first: the file is created by
// truncating, exactly as --log and --catalog-out are.
func (c *consoleLog) attach(path string) error {
	// The console log commonly sits inside --out-dir, which may not exist yet.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create console log directory %s: %w", dir, err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create console log %s: %w", path, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.f, c.path = f, path
	// Everything the screen has already had goes in first, starting with the
	// banner: a log that does not say which build produced it is worth much
	// less when a conversion is being explained months later.
	if c.dropped > 0 {
		fmt.Fprintf(f, "%s: %d byte(s) printed before this log was opened are not recorded here\n",
			programName, c.dropped)
	}
	if len(c.pending) > 0 {
		f.Write(c.pending)
	}
	c.settle()
	return nil
}

// discard is attach's other half: the run has decided there is no file, so
// nothing more needs holding.
func (c *consoleLog) discard() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settle()
}

// settle ends the pre-attach buffering. The caller holds the lock.
func (c *consoleLog) settle() {
	c.pending, c.settled, c.dropped = nil, true, 0
}

// Write sends one message to the screen and to the file. Both writes are made
// under the lock, so two goroutines cannot land in one order on the screen and
// the other in the file.
func (c *consoleLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := os.Stderr.Write(p)
	if c.f == nil {
		if !c.settled {
			if len(c.pending)+len(p) <= pendingLimit {
				c.pending = append(c.pending, p...)
			} else {
				c.dropped += len(p)
			}
		}
		return n, err
	}
	if _, ferr := c.f.Write(p); ferr != nil {
		// The conversion is what the run is for and the screen still has the
		// output, so a file that cannot be written is dropped with one note
		// rather than failing the run or repeating itself on every line.
		path := c.path
		c.f.Close()
		c.f, c.path = nil, ""
		fmt.Fprintf(os.Stderr, "%s: write %s: %v; the screen output is no longer being recorded\n",
			programName, path, ferr)
	}
	return n, err
}

// Close detaches the file. It is safe on a console log that never had one.
func (c *consoleLog) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.f == nil {
		return nil
	}
	err := c.f.Close()
	c.f, c.path = nil, ""
	return err
}
