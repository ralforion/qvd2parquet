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
// Nothing downstream survives it. bufio.Reader adds the count to its write
// index, so a buffer that claims more than it holds leaves the reader slicing
// bytes that were never written by that read: whatever the buffer held before,
// spliced into the middle of the data. In an XML header that is a stray byte
// inside a tag, from nowhere, on a file that reads correctly the next time.
//
// Whether that has ever happened here is unknown. It costs one comparison per
// read to refuse it rather than decode it.
type checkedReader struct{ r io.Reader }

func (c checkedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n < 0 || n > len(p) {
		return 0, impossibleCount(n, len(p))
	}
	return n, err
}

// checkedReaderAt is checkedReader for the ReadAt path.
type checkedReaderAt struct{ r io.ReaderAt }

func (c checkedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	if n < 0 || n > len(p) {
		return 0, impossibleCount(n, len(p))
	}
	return n, err
}

func impossibleCount(n, size int) error {
	return fmt.Errorf("%w: a read of %d byte(s) reported %d byte(s) read, which cannot happen; "+
		"nothing read here can be trusted", ErrUnstableRead, size, n)
}
