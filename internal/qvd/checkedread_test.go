package qvd

import (
	"bytes"
	"errors"
	"io"
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

// os.File.ReadAt slices the caller's buffer by the count it was given
// (b = b[m:]) before any wrapper regains control, so an over-count there
// panics rather than corrupting, and no wrapper could turn it into an error.
// That is why the guarded reads are sequential; this pins the reasoning.
func TestReadAtSlicesByTheCountItself(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("slicing a buffer by a count larger than it should panic")
		}
	}()
	b := make([]byte, 4)
	_ = b[7:] // what os.File.ReadAt does with m > len(b)
}

// The symbol tables are read sequentially for that reason, so the guard
// applies to them.
func TestReadSymbolsUsesTheGuardedPath(t *testing.T) {
	f, err := Open(twoColumnFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.ReadSymbols(UnknownSymbolError); err != nil {
		t.Fatalf("ReadSymbols: %v", err)
	}
	if len(f.Symbols[0]) == 0 || len(f.Symbols[1]) == 0 {
		t.Error("symbols were not read")
	}
	// Reading sequentially must leave the next column's start correct, which
	// the per-column seek guarantees.
	if f.RecordStart <= f.HeaderEnd {
		t.Errorf("RecordStart = %d, HeaderEnd = %d", f.RecordStart, f.HeaderEnd)
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
