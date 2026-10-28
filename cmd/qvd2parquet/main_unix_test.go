//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ralforion/qvd2parquet/internal/convert"
)

// A stalled single-file run with --log writes its record without reading the
// input again. Naming the table rereads the header, which waits on the same
// stuck call, and the run hung there with an empty log. A named pipe nothing
// writes to is an input whose open never returns.
func TestSingleFileStallWritesTheLogWithoutRereading(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.qvd")
	if err := syscall.Mkfifo(in, 0o644); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	t.Cleanup(func() {
		// Opening the other end lets the abandoned open return, so nothing
		// is left blocked when the directory is removed.
		if f, err := os.OpenFile(in, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	defer convert.SetStallGraceForTest(100 * time.Millisecond)()

	logPath := filepath.Join(dir, "run.jsonl")
	opts := convert.DefaultOptions()
	done := make(chan int, 1)
	go func() {
		done <- runSingle(context.Background(), in, filepath.Join(dir, "out.parquet"), &opts,
			300*time.Millisecond, logPath, "", "", func(string, ...any) {})
	}()
	select {
	case code := <-done:
		if code != exitStalled {
			t.Errorf("exit code %d, want %d", code, exitStalled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled run hung after the stall, writing its log")
	}

	records := readLogRecords(t, logPath)
	if len(records) != 2 || records[0]["status"] != "failed" ||
		!strings.Contains(records[0]["error"].(string), "stalled") {
		t.Errorf("log = %v, want the stalled file record and the summary", records)
	}
}
