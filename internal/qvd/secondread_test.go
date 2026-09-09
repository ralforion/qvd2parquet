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

func TestDescribeDiff(t *testing.T) {
	if got := describeDiff([]byte("abc"), []byte("abc")); got != "returned the same bytes" {
		t.Errorf("describeDiff = %q", got)
	}
	got := describeDiff([]byte("line1\nab"), []byte("line1\naX"))
	for _, want := range []string{"different bytes", "offset 7", "line 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeDiff = %q, want it to mention %q", got, want)
		}
	}
}

func TestReadNoteEmptyForAGoodHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.qvd")
	raw := append([]byte(sampleHeader), 0x00)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if f.Header.ReadNote != "" {
		t.Errorf("ReadNote = %q, want empty when the first read parsed", f.Header.ReadNote)
	}
}
