package qvd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// overReporter hands back a count larger than the buffer it was given, which
// is what a Windows read has been observed to do on a share during a network
// outage, and what nothing in the Go stack checks for.
type overReporter struct {
	r     io.Reader
	extra int
}

func (o *overReporter) Read(p []byte) (int, error) {
	n, err := o.r.Read(p)
	if n > 0 {
		n += o.extra
	}
	return n, err
}

func TestCheckedReaderRefusesAnImpossibleCount(t *testing.T) {
	src := &overReporter{r: strings.NewReader("hello"), extra: 7}
	_, err := checkedReader{src}.Read(make([]byte, 5))
	if !errors.Is(err, ErrUnstableRead) {
		t.Fatalf("error = %v, want ErrUnstableRead", err)
	}
	if !strings.Contains(err.Error(), "cannot happen") {
		t.Errorf("error = %v", err)
	}
}

func TestCheckedReaderPassesOrdinaryReads(t *testing.T) {
	var out bytes.Buffer
	n, err := io.Copy(&out, checkedReader{strings.NewReader("hello")})
	if err != nil || n != 5 || out.String() != "hello" {
		t.Errorf("Copy = %d, %v, %q", n, err, out.String())
	}
}

type overReporterAt struct{ r io.ReaderAt }

func (o overReporterAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := o.r.ReadAt(p, off)
	return n + 1, err
}

func TestCheckedReaderAtRefusesAnImpossibleCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("abcdefgh"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if _, err := (checkedReaderAt{overReporterAt{f}}).ReadAt(make([]byte, 4), 0); !errors.Is(err, ErrUnstableRead) {
		t.Fatalf("error = %v, want ErrUnstableRead", err)
	}
	// The ordinary path is untouched.
	p := make([]byte, 4)
	if n, err := (checkedReaderAt{f}).ReadAt(p, 2); n != 4 || err != nil || string(p) != "cdef" {
		t.Errorf("ReadAt = %d, %v, %q", n, err, p)
	}
}

// The header read is the one that matters: an over-reported count there puts
// bytes that were never read into the middle of the XML.
func TestReadHeaderBytesRefusesAnImpossibleCount(t *testing.T) {
	body := append([]byte(sampleHeader), 0x00)
	// The over-report has to exceed the slice bufio hands down, which is the
	// only shape of it that is impossible rather than merely surprising.
	_, _, err := ReadHeaderBytes(&overReporter{r: bytes.NewReader(body), extra: 8192})
	if !errors.Is(err, ErrUnstableRead) {
		t.Fatalf("error = %v, want ErrUnstableRead", err)
	}
}
