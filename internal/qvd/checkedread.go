package qvd

import (
	"fmt"
	"io"
)

// A Read or ReadAt that reports more bytes than it was given is impossible,
// and Go does not check for it: on Windows, internal/poll passes the count
// ReadFile writes into lpNumberOfBytesRead straight through execIO, os.File
// returns it unchanged, and io.LimitReader and io.SectionReader pass it on.
// One such report has been seen from Windows on a share during a network
// outage: 4308 bytes claimed for a 4096-byte buffer.
//
// Believed, such a count crashes rather than corrupts. bufio.Reader adds it to
// its write index and the next slice of the buffer panics with
//
//	slice bounds out of range [:4308] with capacity 4096
//
// so this is not protection against wrong data, and nothing here could be: the
// runtime catches an impossible count either way. What it decides is which.
//
// A panic takes the whole run with it, and a batch converting for hours loses
// every file still in flight. An error fails one file and lets the rest
// finish. That is the entire benefit, and it is worth one comparison per read.
//
// It guards sequential reads, which are the ones where the count is visible
// before anything slices by it. On the ReadAt path the count is consumed
// inside os.File.ReadAt, which slices the caller's buffer by it (b = b[m:])
// before any wrapper of ours regains control, so that path keeps the panic and
// reads that are worth guarding are made sequentially.
type checkedReader struct{ r io.Reader }

func (c checkedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n < 0 || n > len(p) {
		return 0, impossibleCount(n, len(p))
	}
	return n, err
}

func impossibleCount(n, size int) error {
	return fmt.Errorf("%w: a read of %d byte(s) reported %d byte(s) read, which cannot happen; "+
		"nothing read here can be trusted", ErrUnstableRead, size, n)
}
