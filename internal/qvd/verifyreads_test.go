package qvd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A symbol table that reads differently stops before a single record is
// decoded: those symbols were about to be written into every row that refers
// to them.
func TestReadSymbolsStopsOnAChangedTable(t *testing.T) {
	path := twoColumnFile(t)

	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.VerifyReads = true

	restore := openVerifyReader
	openVerifyReader = func(qf *File) (io.ReaderAt, func() error, error) {
		r, closeFn, err := restore(qf)
		if err != nil {
			return nil, nil, err
		}
		return flipAt{r: r, at: qf.HeaderEnd + 2}, closeFn, nil
	}
	defer func() { openVerifyReader = restore }()

	err = f.ReadSymbols(UnknownSymbolError)
	if err == nil {
		t.Fatal("a symbol table that read differently was not caught")
	}
	if !errors.Is(err, ErrUnstableRead) {
		t.Errorf("error = %v, want ErrUnstableRead", err)
	}
	if !strings.Contains(err.Error(), "symbol table of column") {
		t.Errorf("error = %v, want it to name the symbol table", err)
	}
}

// The ordinary case must not be disturbed by the checking.
func TestReadSymbolsPassesWhenReadsAgree(t *testing.T) {
	f, err := Open(twoColumnFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.VerifyReads = true
	if err := f.ReadSymbols(UnknownSymbolError); err != nil {
		t.Fatalf("ReadSymbols: %v", err)
	}
	if len(f.Symbols[0]) == 0 {
		t.Error("no symbols were read")
	}
}

func TestDigestMatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b")
	if err := os.WriteFile(path, []byte("abcdefgh"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var zero [32]byte
	if ok, err := digestMatches(f, 0, 8, zero); err != nil || ok {
		t.Errorf("digestMatches with a wrong digest = %v, %v", ok, err)
	}
	// Reading past the end is an error, not a mismatch.
	if _, err := digestMatches(f, 0, 99, zero); err == nil {
		t.Error("reading past the end should fail")
	}
}

type flipAt struct {
	r  io.ReaderAt
	at int64
}

func (f flipAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.r.ReadAt(p, off)
	if i := f.at - off; i >= 0 && i < int64(n) {
		p[i] ^= 0x01
	}
	return n, err
}
