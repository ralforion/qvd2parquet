//go:build unix

package parquetwrite

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The mode a file is created with is the caller's, not os.Create's 0666: the
// reports were 0644 and stay so under a umask of 002, which would let 0666
// through as group-writable.
func TestWriteFileKeepsTheCallersMode(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0o002))
	path := filepath.Join(t.TempDir(), "report.json")
	if err := WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode %v, want -rw-r--r--", got)
	}
}
