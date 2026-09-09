package qvd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A header that will not parse is either written wrong or read wrong, and the
// error has to say which, because nothing outside the process can tell them
// apart afterwards.
func TestOpenSaysWhetherASecondReadAgrees(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.qvd")
	// Malformed beyond any repair: no root element at all.
	raw := append([]byte("<QvdTableHeader><Fields><FieldName>x</Fiel"), 0x00, 'd', 'a', 't', 'a')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "a second read returned the same") {
		t.Errorf("error does not report the second read: %v", err)
	}
}

func TestSecondReadNoteOnAnUnreadablePath(t *testing.T) {
	if got := SecondReadNote(filepath.Join(t.TempDir(), "gone.qvd"), nil); got != "" {
		t.Errorf("SecondReadNote = %q, want empty for a path that cannot be reopened", got)
	}
}
