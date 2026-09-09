package convert

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// maxReadDiffsNamed bounds how many differing ranges are named. The count of
// them is not bounded: one range out of thousands is a flip, and most of them
// is something systematic, and that difference is worth more than the fourth
// offset.
const maxReadDiffsNamed = 3

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

	var diffs readDiffs
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
		diffs.check(sum == f.SymbolDigests[i], func() string {
			return fmt.Sprintf(
				"the symbol table of column %q, %d bytes at offset %d, read differently the second time",
				f.Columns[i].Name, r.Length, r.Offset)
		})
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
		diffs.check(sum == chunkDigests[ch.Index], func() string {
			return fmt.Sprintf(
				"records for rows %d..%d, %d bytes at offset %d, read differently the second time",
				ch.StartRow, ch.StartRow+int64(ch.RowCount), size, ch.ByteOffset)
		})
		if progress != nil {
			progress(ch.StartRow + int64(ch.RowCount))
		}
	}
	return diffs.report(), nil
}

// readDiffs collects the ranges that did not read the same way twice.
//
// The scan does not stop at the first one. The bytes have already been read
// once by the conversion, so finishing the check costs a fraction of what has
// already been spent, and the total is the most useful thing it can report:
// one differing range out of thousands is a flip, and most of them differing
// is something systematic. Those call for different questions.
type readDiffs struct {
	named   []string
	differ  int
	checked int
}

func (d *readDiffs) check(same bool, describe func() string) {
	d.checked++
	if same {
		return
	}
	d.differ++
	if len(d.named) < maxReadDiffsNamed {
		d.named = append(d.named, describe())
	}
}

// report is empty when every range read the same way twice.
func (d *readDiffs) report() []string {
	if d.differ == 0 {
		return nil
	}
	out := append([]string(nil), d.named...)
	return append(out, fmt.Sprintf("%d of %d byte range(s) read differently in total",
		d.differ, d.checked))
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
