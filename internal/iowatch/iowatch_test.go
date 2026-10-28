package iowatch

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// withClock pins now for one test and returns a function that moves it on.
func withClock(t *testing.T) (advance func(time.Duration)) {
	t.Helper()
	at := time.Date(2026, 10, 28, 9, 33, 0, 0, time.UTC)
	prev := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = prev })
	return func(d time.Duration) { at = at.Add(d) }
}

func TestStuckNamesTheOldestCallPastTheLimit(t *testing.T) {
	advance := withClock(t)
	endWrite := Begin("write", `Y:\daily\ADRC.parquet.tmp-1`)
	advance(time.Minute)
	endRead := Begin("read", `F:\SAP\ADRC.qvd`)
	defer endRead()

	advance(9 * time.Minute)
	if c, ok := Stuck(10 * time.Minute); !ok || c.Op != "write" {
		t.Fatalf("Stuck = %v, %v; want the write, in flight for exactly 10m", c, ok)
	}
	if _, ok := Stuck(11 * time.Minute); ok {
		t.Error("nothing has been in flight for 11m")
	}

	endWrite()
	endWrite() // a second end is harmless
	if _, ok := Stuck(10 * time.Minute); ok {
		t.Error("the write returned; the read is only 9m old")
	}
	advance(time.Minute)
	if c, ok := Stuck(10 * time.Minute); !ok || c.String() != `read from F:\SAP\ADRC.qvd` {
		t.Errorf("Stuck = %v, %v; want the read", c, ok)
	}
}

// A call that returned is gone, so a long run that keeps making progress is
// never reported however long it takes.
func TestWrappersRecordOnlyWhileTheCallRuns(t *testing.T) {
	advance := withClock(t)
	var sink bytes.Buffer
	w := Writer(&sink, "out")
	r := Reader(strings.NewReader("abc"), "in")
	ra := ReaderAt(strings.NewReader("abc"), "in")
	for range 3 {
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Read(make([]byte, 1)); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if _, err := ra.ReadAt(make([]byte, 1), 0); err != nil {
			t.Fatal(err)
		}
		advance(time.Hour)
	}
	if c, ok := Stuck(time.Nanosecond); ok {
		t.Errorf("%v is still registered after returning", c)
	}
	if sink.String() != "xxx" {
		t.Errorf("writes passed through as %q", sink.String())
	}
}

func TestCallString(t *testing.T) {
	for c, want := range map[Call]string{
		{Op: "write", Path: "a"}:  "write to a",
		{Op: "read", Path: "a"}:   "read from a",
		{Op: "rename", Path: "a"}: "rename of a",
	} {
		if got := c.String(); got != want {
			t.Errorf("%#v = %q, want %q", c, got, want)
		}
	}
}
