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

func TestCompareNote(t *testing.T) {
	same := compareNote([]byte("abc"), []byte("abc"), nil)
	if !strings.Contains(same, "the same 3 bytes") {
		t.Errorf("compareNote = %q", same)
	}
	diff := compareNote([]byte("abc"), []byte("abX"), nil)
	if !strings.Contains(diff, "DIFFERENT BYTES") || !strings.Contains(diff, "offset 2") {
		t.Errorf("compareNote = %q", diff)
	}
	if got := compareNote(nil, nil, os.ErrNotExist); !strings.Contains(got, "failed too") {
		t.Errorf("compareNote = %q", got)
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

// The case a failure-triggered retry cannot catch: two reads that differ and
// both parse. A wrong digit in NoOfRecords is valid XML, so nothing about the
// parse would ever mention it.
func TestOpenReportsTwoReadsThatDifferAndBothParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drift.qvd")
	firstBytes := append([]byte(sampleHeader), 0x00)
	if err := os.WriteFile(path, firstBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	other := append([]byte(strings.Replace(sampleHeader,
		"<NoOfRecords>3</NoOfRecords>", "<NoOfRecords>4</NoOfRecords>", 1)), 0x00)

	h, _, trusted, note, err := readHeaderRetryingWithOpen(path, f,
		func(p string) (*os.File, error) {
			if err := os.WriteFile(path, other, 0o644); err != nil {
				return nil, err
			}
			return os.Open(p)
		})
	if err != nil {
		f.Close()
		t.Fatalf("readHeaderRetryingWithOpen: %v", err)
	}
	defer trusted.Close()

	if h.NoOfRecords != 3 {
		t.Errorf("NoOfRecords = %d, want the first read's 3", h.NoOfRecords)
	}
	if !strings.Contains(note, "DIFFERENT BYTES") {
		t.Errorf("a disagreement between two parseable reads went unreported: %q", note)
	}
	if !strings.Contains(note, "offset") {
		t.Errorf("note names no offset: %q", note)
	}
}

// A header that only a tolerant parse accepts must not slip past the second
// read: the strict parse is what decides whether a header is looked at twice.
func TestToleratedHeaderStillGetsASecondRead(t *testing.T) {
	// An unknown entity is accepted by encoding/xml with Strict false.
	raw := strings.Replace(sampleHeader, "<TableName>Sales</TableName>",
		"<TableName>Sales &nope; Ltd</TableName>", 1)
	if _, err := decodeHeaderStrict([]byte(raw)); err == nil {
		t.Fatal("expected the strict decoder to reject an unknown entity")
	}
	if _, err := decodeHeader([]byte(raw)); err != nil {
		t.Fatalf("expected the tolerant decoder to accept it: %v", err)
	}
}
