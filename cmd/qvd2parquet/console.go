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
// and nothing here buffers: a run that is stopped or killed keeps every line
// it had printed, which is the same guarantee the JSON log now makes.
type consoleLog struct {
	mu   sync.Mutex
	f    *os.File // nil until --console-log attaches one, and again if it fails
	path string
}

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
	// The banner has already been printed to the screen by the time the guards
	// have run, so it is written to the file alone. A log that does not say
	// which build produced it is worth much less when a conversion is being
	// explained months later.
	fmt.Fprintf(f, "%s\n", banner())
	return nil
}

// Write sends one message to the screen and to the file. Both writes are made
// under the lock, so two goroutines cannot land in one order on the screen and
// the other in the file.
func (c *consoleLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := os.Stderr.Write(p)
	if c.f == nil {
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
