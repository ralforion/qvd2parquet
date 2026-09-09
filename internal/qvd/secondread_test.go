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

func TestOpenRepairsHeaderOnlyAfterASecondReadAgrees(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.qvd")
	raw := append([]byte(strings.Replace(sampleHeader,
		"<NoOfSymbols>5</NoOfSymbols>", "<NoOfSymbols\x7f>5</NoOfSymbols>", 1)), 0x00)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if !f.Header.Repaired {
		t.Fatal("Open should still repair a header whose bad bytes repeat")
	}
	if f.Header.ReadNote != "" {
		t.Errorf("ReadNote = %q, want empty for stable malformed bytes", f.Header.ReadNote)
	}
}

func TestOpenRetriesBeforeRepairingHeader(t *testing.T) {
	earlyTerminator := append([]byte{}, sampleHeader[:strings.Index(sampleHeader, "<Fields>")]...)
	earlyTerminator = append(earlyTerminator, 0x00)
	earlyTerminator = append(earlyTerminator, sampleHeader[strings.Index(sampleHeader, "<Fields>"):]...)
	earlyTerminator = append(earlyTerminator, 0x00)

	cases := []struct {
		name string
		raw  []byte
	}{
		{
			name: "control byte in tag",
			raw: append([]byte(strings.Replace(sampleHeader,
				"<NoOfSymbols>5</NoOfSymbols>", "<NoOfSymbols\x7f>5</NoOfSymbols>", 1)), 0x00),
		},
		{
			name: "early terminator",
			raw:  earlyTerminator,
		},
		{
			name: "mangled end tag",
			raw: append([]byte(strings.Replace(sampleHeader,
				"</BitOffset>", "</B itOffset>", 1)), 0x00),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "flaky.qvd")
			if err := os.WriteFile(path, tc.raw, 0o644); err != nil {
				t.Fatal(err)
			}
			first, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}

			correct := append([]byte(sampleHeader), 0x00)
			h, _, trusted, note, err := readHeaderRetryingWithOpen(path, first,
				func(p string) (*os.File, error) {
					if err := os.WriteFile(path, correct, 0o644); err != nil {
						return nil, err
					}
					return os.Open(p)
				})
			if err != nil {
				first.Close()
				t.Fatalf("readHeaderRetryingWithOpen: %v", err)
			}
			defer trusted.Close()
			if trusted == first {
				t.Fatal("retry should use the handle that produced the trusted header")
			}
			if h.Repaired {
				t.Fatal("a transient bad read was repaired instead of retried")
			}
			if h.TableName != "Sales" {
				t.Errorf("TableName = %q, want Sales", h.TableName)
			}
			if !strings.Contains(note, "second read returned different bytes") ||
				!strings.Contains(note, "and parsed") {
				t.Errorf("ReadNote = %q", note)
			}
		})
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
