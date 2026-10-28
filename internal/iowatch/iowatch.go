// Package iowatch keeps track of the reads and writes in flight on the files a
// conversion reads and writes, so a call that never returns can be named.
//
// A read or write that the operating system never completes is not something
// a program can interrupt. On Windows a write to a stalled network share sits
// in the kernel, and neither Ctrl-C nor closing the handle brings it back. What
// a program can do is notice: every call on a data file is registered when it
// starts and removed when it returns, and one that has been registered for
// longer than any real call takes is a stall, with the file and the direction
// to say which.
//
// The registry is process-wide. Calls are recorded per read or write, not per
// file, so a long conversion that keeps making progress never looks stuck,
// however long it runs, and a phase that only computes is never mistaken for
// one that waits.
package iowatch

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Call is one read or write in flight.
type Call struct {
	// Op is what the call does: "read", "write", "create", "rename" and so on.
	Op    string
	Path  string
	Start time.Time
}

// String names the call for a message: "write to X", "read from X".
func (c Call) String() string {
	switch c.Op {
	case "read":
		return "read from " + c.Path
	case "write":
		return "write to " + c.Path
	}
	return fmt.Sprintf("%s of %s", c.Op, c.Path)
}

var (
	mu    sync.Mutex
	next  uint64
	calls = map[uint64]Call{}
)

// now is time.Now, replaceable so a test can age a call without waiting.
var now = time.Now

// Begin records that op on path has started and returns the function that
// records its end. The end function may be called more than once.
func Begin(op, path string) (end func()) {
	mu.Lock()
	next++
	id := next
	calls[id] = Call{Op: op, Path: path, Start: now()}
	mu.Unlock()
	return func() {
		mu.Lock()
		delete(calls, id)
		mu.Unlock()
	}
}

// Stuck returns the call that has been in flight longest, if it has been for
// at least d.
func Stuck(d time.Duration) (Call, bool) {
	mu.Lock()
	defer mu.Unlock()
	var oldest Call
	found := false
	for _, c := range calls {
		if !found || c.Start.Before(oldest.Start) {
			oldest, found = c, true
		}
	}
	if !found || now().Sub(oldest.Start) < d {
		return Call{}, false
	}
	return oldest, true
}

// Reader records every Read on r as a read from path.
func Reader(r io.Reader, path string) io.Reader { return reader{r, path} }

type reader struct {
	r    io.Reader
	path string
}

func (r reader) Read(p []byte) (int, error) {
	defer Begin("read", r.path)()
	return r.r.Read(p)
}

// ReaderAt records every ReadAt on r as a read from path.
func ReaderAt(r io.ReaderAt, path string) io.ReaderAt { return readerAt{r, path} }

type readerAt struct {
	r    io.ReaderAt
	path string
}

func (r readerAt) ReadAt(p []byte, off int64) (int, error) {
	defer Begin("read", r.path)()
	return r.r.ReadAt(p, off)
}

// Writer records every Write on w as a write to path.
func Writer(w io.Writer, path string) io.Writer { return writer{w, path} }

type writer struct {
	w    io.Writer
	path string
}

func (w writer) Write(p []byte) (int, error) {
	defer Begin("write", w.path)()
	return w.w.Write(p)
}

// File records every Read, ReadAt and Write on an open file under the name it
// was opened with, and passes every other method through. It is for a reader
// that needs more of the file than one method, such as a Parquet reader that
// seeks to find the footer.
type File struct{ *os.File }

func (f File) Read(p []byte) (int, error) {
	defer Begin("read", f.Name())()
	return f.File.Read(p)
}

func (f File) ReadAt(p []byte, off int64) (int, error) {
	defer Begin("read", f.Name())()
	return f.File.ReadAt(p, off)
}

func (f File) Write(p []byte) (int, error) {
	defer Begin("write", f.Name())()
	return f.File.Write(p)
}
