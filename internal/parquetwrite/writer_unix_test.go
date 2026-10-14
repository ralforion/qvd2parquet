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

// A report the run may write but not read, in a directory it may not change,
// was replaceable with os.WriteFile's write-only open. Opening it read-write
// is refused, and the fallback cannot delete it either, so the open has to
// stay write-only.
func TestWriteFileNeedsNoReadPermission(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, []byte("previous run\n"), 0o200); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
		f.Close()
		t.Skip("the file opened for reading despite its mode; running as root?")
	}

	if err := WriteFile(path, []byte("this run\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	os.Chmod(path, 0o600)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "this run\n" {
		t.Fatalf("file holds %q, want the new contents only", got)
	}
}
