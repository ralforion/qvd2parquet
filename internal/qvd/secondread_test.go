package qvd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A header is read twice and the reads must agree. Two reads that differ end
// the file: there is no way to tell which was right, so converting on either
// is a guess.
func TestOpenRefusesWhenTwoReadsDiffer(t *testing.T) {
	cases := map[string]string{
		// Differs and both parse: valid XML either way, which is the case a
		// failure-triggered retry could never see.
		"both parse": strings.Replace(sampleHeader,
			"<NoOfRecords>3</NoOfRecords>", "<NoOfRecords>4</NoOfRecords>", 1),
		// Differs and only the second parses.
		"second parses": sampleHeader,
	}
	for name, second := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "drift.qvd")
			firstBytes := []byte(sampleHeader)
			if name == "second parses" {
				firstBytes = []byte(strings.Replace(sampleHeader,
					"<NoOfSymbols>5</NoOfSymbols>", "<NoOfSymbols\x7f>5</NoOfSymbols>", 1))
			}
			if err := os.WriteFile(path, append(firstBytes, 0x00), 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			_, _, err = readHeaderVerified(path, f, func(p string) (*os.File, error) {
				if err := os.WriteFile(p, append([]byte(second), 0x00), 0o644); err != nil {
					return nil, err
				}
				return os.Open(p)
			})
			if err == nil {
				t.Fatal("want an error when two reads of the header differ")
			}
			for _, want := range []string{"different bytes", "offset", "read wrong"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

// Reads that differ because the file was replaced are not reads that went
// wrong, and must not be reported as though they were.
func TestOpenRefusesAReplacedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swapped.qvd")
	if err := os.WriteFile(path, append([]byte(sampleHeader), 0x00), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	_, _, err = readHeaderVerified(path, f, func(p string) (*os.File, error) {
		other := filepath.Join(dir, "other.qvd")
		if err := os.WriteFile(other, append([]byte(sampleHeader), 0x00), 0o644); err != nil {
			return nil, err
		}
		return os.Open(other) // a different file at the same moment
	})
	if err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Errorf("error = %v, want it to say the file was replaced", err)
	}
}

func TestOpenAcceptsTwoAgreeingReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.qvd")
	if err := os.WriteFile(path, append([]byte(sampleHeader), 0x00), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if f.Header.TableName != "Sales" || f.Header.Repaired {
		t.Errorf("TableName=%q repaired=%v", f.Header.TableName, f.Header.Repaired)
	}
}

// A header that is consistently malformed is still mended, since both reads
// agree that this is what the file holds.
func TestOpenStillMendsAConsistentlyMalformedHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.qvd")
	raw := strings.Replace(sampleHeader,
		"<NoOfSymbols>5</NoOfSymbols>", "<NoOfSymbols\x7f>5</NoOfSymbols>", 1)
	if err := os.WriteFile(path, append([]byte(raw), 0x00), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if !f.Header.Repaired {
		t.Error("header was not marked as repaired")
	}
	if got := f.Header.Fields[1].NoOfSymbols; got != 5 {
		t.Errorf("NoOfSymbols = %d, want 5", got)
	}
}

func TestSameOpenFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fa, _ := os.Open(a)
	defer fa.Close()
	fa2, _ := os.Open(a)
	defer fa2.Close()
	fb, _ := os.Open(b)
	defer fb.Close()

	if same, err := sameOpenFile(fa, fa2); err != nil || !same {
		t.Errorf("same file reported as different: %v %v", same, err)
	}
	if same, err := sameOpenFile(fa, fb); err != nil || same {
		t.Errorf("different files reported as same: %v %v", same, err)
	}
}
