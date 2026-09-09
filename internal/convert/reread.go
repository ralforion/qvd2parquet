package convert

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// maxReadDiffsReported bounds the list of differing ranges. A read that went
// wrong once is the thing to act on; a thousand of them say nothing more than
// the first three do.
const maxReadDiffsReported = 3

// VerifySourceReads reads every byte the conversion read a second time and
// compares it against a digest taken as the conversion read it.
//
// Nothing else in this package covers the read itself. The quality gate
// validates the written Parquet against metrics collected from the values the
// converter produced, so it cannot question those values: a record byte read
// wrong points at a different symbol, the symbol yields a value that is
// entirely well formed, and the Parquet then faithfully contains it. There is
// no syntax to violate and nothing downstream to notice.
//
// Comparing bytes rather than decoded values is both cheaper and more useful.
// The second pass is a sequential read with no decoding behind it, and what it
// reports is a byte range rather than a column: an offset is what anyone
// chasing this below our level can act on.
//
// What it cannot see is a read that is wrong the same way twice. If the bad
// bytes are cached, both passes agree and nothing here fires. That is a limit
// of any check inside one process, and no arrangement of them removes it.
func VerifySourceReads(ctx context.Context, path string, f *qvd.File,
	chunks []DecodeChunk, chunkDigests [][32]byte, progress ProgressFunc) ([]string, error) {

	again, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("verify %s: %w", path, err)
	}
	defer again.Close()

	same, err := sameContent(f.FileHandle(), again)
	if err != nil {
		return nil, fmt.Errorf("verify %s: %w", path, err)
	}
	if !same {
		return nil, fmt.Errorf("verify %s: the file was replaced while it was being converted", path)
	}

	var diffs []string
	buf := make([]byte, 1<<20)

	// Symbol tables first: a wrong byte there is worth more than a wrong
	// record byte, since every row referring to that symbol carries it.
	for i, r := range f.SymbolRanges() {
		if !f.Columns[i].Selected || len(f.SymbolDigests) == 0 {
			continue
		}
		sum, err := digestRange(ctx, again, r.Offset, r.Length, buf)
		if err != nil {
			return nil, fmt.Errorf("verify %s: %w", path, err)
		}
		if sum != f.SymbolDigests[i] {
			diffs = appendDiff(diffs, fmt.Sprintf(
				"the symbol table of column %q, %d bytes at offset %d, read differently the second time",
				f.Columns[i].Name, r.Length, r.Offset))
		}
	}

	for _, ch := range chunks {
		if err := ctx.Err(); err != nil {
			return nil, ErrCanceled
		}
		size := int64(ch.RowCount) * int64(f.RecordByteSize)
		sum, err := digestRange(ctx, again, ch.ByteOffset, size, buf)
		if err != nil {
			return nil, fmt.Errorf("verify %s: %w", path, err)
		}
		if sum != chunkDigests[ch.Index] {
			diffs = appendDiff(diffs, fmt.Sprintf(
				"records for rows %d..%d, %d bytes at offset %d, read differently the second time",
				ch.StartRow, ch.StartRow+int64(ch.RowCount), size, ch.ByteOffset))
		}
		if progress != nil {
			progress(ch.StartRow + int64(ch.RowCount))
		}
	}
	return diffs, nil
}

// appendDiff keeps the first few differences and counts the rest.
func appendDiff(diffs []string, d string) []string {
	if len(diffs) < maxReadDiffsReported {
		return append(diffs, d)
	}
	if len(diffs) == maxReadDiffsReported {
		return append(diffs, "and more ranges after these")
	}
	return diffs
}

// digestRange hashes length bytes at off.
func digestRange(ctx context.Context, f *os.File, off, length int64, buf []byte) ([32]byte, error) {
	sum := sha256.New()
	for read := int64(0); read < length; {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, ErrCanceled
		}
		n := int64(len(buf))
		if rest := length - read; rest < n {
			n = rest
		}
		got, err := f.ReadAt(buf[:n], off+read)
		if err != nil && !(err == io.EOF && int64(got) == n) {
			return [32]byte{}, fmt.Errorf("read %d bytes at offset %d: %w", n, off+read, err)
		}
		if int64(got) != n {
			return [32]byte{}, fmt.Errorf("read %d of %d bytes at offset %d", got, n, off+read)
		}
		sum.Write(buf[:n])
		read += n
	}
	var out [32]byte
	copy(out[:], sum.Sum(nil))
	return out, nil
}

// sameContent reports whether two handles refer to the same file.
func sameContent(a, b *os.File) (bool, error) {
	ai, err := a.Stat()
	if err != nil {
		return false, err
	}
	bi, err := b.Stat()
	if err != nil {
		return false, err
	}
	return os.SameFile(ai, bi), nil
}
