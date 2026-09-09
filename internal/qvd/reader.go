package qvd

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"
	"strings"
)

// File is an opened QVD ready for symbol and record access.
type File struct {
	Path   string
	Header *TableHeader
	// Columns holds every field in the file, in header order. Record decoding
	// needs the bit layout of skipped columns too, so none are removed.
	Columns []Column
	// HeaderEnd is the byte offset of the first symbol table.
	HeaderEnd int64
	// RecordStart is the byte offset of the first record.
	RecordStart int64
	// RecordByteSize is the fixed width of one record.
	RecordByteSize int
	// NoOfRecords is the declared row count.
	NoOfRecords int64

	// VerifyReads makes ReadSymbols check each column's symbol table against a
	// second read of the same range, and fail on the first that does not
	// match. Records are checked the same way, per chunk, by the caller.
	VerifyReads bool

	// Symbols is indexed by column index; nil for skipped columns.
	Symbols [][]Symbol
	// Profiles is indexed by column index; nil for skipped columns.
	Profiles []*ColumnProfile

	f *os.File
}

// Open reads and validates the header of the QVD at path.
func Open(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	raw, end, err := readHeaderVerified(path, f, os.Open)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	h, err := ParseHeaderXML(raw)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := h.Validate(); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	qf := &File{
		Path:           path,
		Header:         h,
		Columns:        h.Columns(),
		HeaderEnd:      end,
		RecordByteSize: h.RecordByteSize,
		NoOfRecords:    h.NoOfRecords,
		Symbols:        make([][]Symbol, len(h.Fields)),
		Profiles:       make([]*ColumnProfile, len(h.Fields)),
		f:              f,
	}
	return qf, nil
}

// readHeaderVerified reads the header from two handles and requires the two
// reads to agree before returning anything.
//
// The header is read twice always, not only after a failure. It is a few
// hundred kilobytes against files of gigabytes, so the cost is nothing, and it
// catches what a failure-triggered retry cannot: a read that differs but still
// parses. A wrong digit in NoOfRecords or BitWidth is valid XML, and a
// conversion built on it would be wrong in a way nothing downstream checks.
//
// Two reads that disagree end the file here rather than picking a winner.
// There is no way to tell which read was right, so a conversion built on
// either is a guess, and a file that does not read consistently is not one to
// convert on the strength of the read that happened to parse. The failure is a
// signal about the machine or the process, not about the file, and it says so.
func readHeaderVerified(path string, first *os.File,
	open func(string) (*os.File, error)) ([]byte, int64, error) {

	raw, end, err := ReadHeaderBytes(first)

	second, oerr := open(path)
	if oerr != nil {
		return nil, 0, fmt.Errorf("the header could not be read a second time to verify it: %w", oerr)
	}
	defer second.Close()

	// The path can name a different file than the first handle if the input
	// was replaced mid-run, and then the two reads differ for a reason that
	// has nothing to do with how they were read. Saying "read wrong" about
	// that would be false.
	same, serr := sameOpenFile(first, second)
	if serr != nil {
		return nil, 0, fmt.Errorf("the header could not be verified: %w", serr)
	}
	if !same {
		return nil, 0, errors.New("the file at this path was replaced while it was being read")
	}

	raw2, _, err2 := ReadHeaderBytes(second)

	switch {
	case err != nil && err2 != nil:
		return nil, 0, err // consistently unreadable: report it as it stands
	case err != nil:
		return nil, 0, fmt.Errorf("the header read once and not the other time (%w), "+
			"so the file was read wrong rather than written wrong", err)
	case err2 != nil:
		return nil, 0, fmt.Errorf("a second read of the header failed where the first did not (%w), "+
			"so the file was read wrong rather than written wrong", err2)
	case !bytes.Equal(raw, raw2):
		off := firstDiffOffset(raw, raw2)
		return nil, 0, fmt.Errorf("two reads of the header returned different bytes: %d then %d, "+
			"first difference at offset %d, line %d. The file was read wrong rather than written wrong, "+
			"so neither read can be trusted", len(raw), len(raw2), off, lineAt(raw, off))
	}
	return raw, end, nil
}

// sameOpenFile reports whether two handles refer to the same file.
func sameOpenFile(a, b *os.File) (bool, error) {
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

// firstDiffOffset is the offset of the first byte at which a and b differ.
func firstDiffOffset(a, b []byte) int {
	off := 0
	for off < len(a) && off < len(b) && a[off] == b[off] {
		off++
	}
	return off
}

// ErrUnstableRead marks a range of the file that did not read the same way
// twice. It is not a statement about the file: the bytes on disk are whatever
// they are, and two reads of them disagreeing says the reading went wrong.
var ErrUnstableRead = errors.New("the file did not read the same way twice")

// openVerifyReader opens the reader used to read a range a second time. It is
// a variable so a test can supply a reader that returns something else, which
// is the situation the check exists for and the one thing a real file cannot
// be made to do on demand.
var openVerifyReader = (*File).openVerifyHandle

// openVerifyHandle opens a private handle on the same file, for reading a
// range a second time. Reopening by path can land on a different file if the
// input is replaced mid-run, which would make every comparison fail for a
// reason that has nothing to do with how the bytes were read.
func (qf *File) openVerifyHandle() (io.ReaderAt, func() error, error) {
	v, err := os.Open(qf.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s to verify reads: %w", qf.Path, err)
	}
	same, err := sameOpenFile(qf.f, v)
	if err != nil || !same {
		v.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("verify reads of %s: %w", qf.Path, err)
		}
		return nil, nil, fmt.Errorf("verify reads of %s: the file was replaced while it was being read", qf.Path)
	}
	return v, v.Close, nil
}

// digestMatches reads length bytes at off and reports whether they hash to
// want. It streams, so a symbol table of any size costs one buffer.
func digestMatches(f io.ReaderAt, off, length int64, want [32]byte) (bool, error) {
	sum := sha256.New()
	buf := make([]byte, 1<<20)
	for read := int64(0); read < length; {
		n := int64(len(buf))
		if rest := length - read; rest < n {
			n = rest
		}
		got, err := f.ReadAt(buf[:n], off+read)
		if int64(got) != n {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return false, fmt.Errorf("read %d of %d bytes at offset %d: %w", got, n, off+read, err)
		}
		sum.Write(buf[:n])
		read += n
	}
	var have [32]byte
	copy(have[:], sum.Sum(nil))
	return have == want, nil
}

// ByteRange is a span of the file.
type ByteRange struct {
	Offset, Length int64
}

// SymbolRanges is the byte range of every column's symbol table, in header
// order, so a later pass can read exactly what ReadSymbols read. Ranges for
// unselected columns are returned too, with the same offsets, since skipping a
// column does not move the ones after it.
func (qf *File) SymbolRanges() []ByteRange {
	out := make([]ByteRange, len(qf.Columns))
	pos := qf.HeaderEnd
	for i := range qf.Columns {
		out[i] = ByteRange{Offset: pos, Length: qf.Columns[i].Length}
		pos += qf.Columns[i].Length
	}
	return out
}

// Close releases the underlying file handle.
func (qf *File) Close() error { return qf.f.Close() }

// FileHandle exposes the underlying file for concurrent ReadAt access.
func (qf *File) FileHandle() *os.File { return qf.f }

// SelectColumns restricts conversion to the named columns, matched
// case-insensitively, preserving header order. An empty list selects all.
func (qf *File) SelectColumns(names []string) error {
	if len(names) == 0 {
		return nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		want[strings.ToLower(n)] = false
	}
	for i := range qf.Columns {
		key := strings.ToLower(qf.Columns[i].Name)
		if _, ok := want[key]; ok {
			want[key] = true
			qf.Columns[i].Selected = true
		} else {
			qf.Columns[i].Selected = false
		}
	}
	var missing []string
	for n, found := range want {
		if !found {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("no such column(s) in %s: %s (available: %s)",
			qf.Path, strings.Join(missing, ", "), strings.Join(qf.ColumnNames(), ", "))
	}
	return nil
}

// ExcludeColumns deselects every column whose name matches one of the given
// shell-style wildcard patterns ('*' and '?', case-insensitive). Patterns are
// matched against the field's original QVD name, before any renaming, so they
// describe what is visible in the source file.
//
// It returns the names that were excluded, in header order, and the patterns
// that matched no selected field. A pattern matching nothing is not an error:
// one command line is often pointed at a folder of tables that do not all
// carry the same fields. It is worth reporting, though, because the two ways
// to write a pattern wrongly ("%" for "%*", or the renamed field instead of
// the original) both look exactly like success. Excluding every column is an
// error, since the output would have no columns.
func (qf *File) ExcludeColumns(patterns []string) (dropped, unmatched []string, err error) {
	var pats []string
	for _, p := range patterns {
		if p = strings.TrimSpace(p); p != "" {
			pats = append(pats, p)
		}
	}
	if len(pats) == 0 {
		return nil, nil, nil
	}
	// Each pattern is judged against the columns selected on entry, not
	// against what earlier patterns have left, so two patterns covering the
	// same field do not make the second one look useless.
	var candidates []string
	for i := range qf.Columns {
		if qf.Columns[i].Selected {
			candidates = append(candidates, qf.Columns[i].Name)
		}
	}
	for _, p := range pats {
		matched := false
		for _, name := range candidates {
			if MatchGlob(p, name) {
				matched = true
				break
			}
		}
		if !matched {
			unmatched = append(unmatched, p)
		}
	}
	for i := range qf.Columns {
		if !qf.Columns[i].Selected {
			continue
		}
		if MatchesAnyGlob(pats, qf.Columns[i].Name) {
			qf.Columns[i].Selected = false
			dropped = append(dropped, qf.Columns[i].Name)
		}
	}
	if len(qf.SelectedColumns()) == 0 {
		return nil, nil, fmt.Errorf("--exclude %s removed every column from %s",
			strings.Join(pats, ", "), qf.Path)
	}
	return dropped, unmatched, nil
}

// ColumnNames returns every field name in header order.
func (qf *File) ColumnNames() []string {
	out := make([]string, len(qf.Columns))
	for i, c := range qf.Columns {
		out[i] = c.Name
	}
	return out
}

// SelectedColumns returns the indexes of the columns to convert.
func (qf *File) SelectedColumns() []int {
	var out []int
	for i, c := range qf.Columns {
		if c.Selected {
			out = append(out, i)
		}
	}
	return out
}

// ReadSymbols decodes the symbol table of every selected column and skips over
// the tables of the rest. It also computes RecordStart.
func (qf *File) ReadSymbols(policy UnknownSymbolPolicy) error {
	// A private handle for the check, so the verifying read cannot disturb the
	// position of the one doing the reading.
	var verify io.ReaderAt
	if qf.VerifyReads {
		v, closeFn, err := openVerifyReader(qf)
		if err != nil {
			return err
		}
		defer closeFn()
		verify = v
	}
	if _, err := qf.f.Seek(qf.HeaderEnd, io.SeekStart); err != nil {
		return fmt.Errorf("seek to symbol area: %w", err)
	}
	pos := qf.HeaderEnd
	for i := range qf.Columns {
		c := &qf.Columns[i]
		if !c.Selected {
			pos += c.Length
			if _, err := qf.f.Seek(pos, io.SeekStart); err != nil {
				return fmt.Errorf("skip symbol table of column %q: %w", c.Name, err)
			}
			continue
		}
		// Read exactly the declared table length so a decoding bug in one
		// column cannot desynchronize the following ones.
		sec := io.NewSectionReader(qf.f, pos, c.Length)
		var r io.Reader = sec
		var sum hash.Hash
		if qf.VerifyReads {
			sum = sha256.New()
			r = io.TeeReader(sec, sum)
		}
		syms, prof, err := ReadSymbolTable(r, c.SymbolCount, policy)
		if err != nil {
			return fmt.Errorf("read symbols for column %q: %w", c.Name, err)
		}
		if sum != nil {
			// The declared length can exceed what the symbols occupy, and the
			// check covers the range this pass claimed to read, not the part
			// of it that happened to be used.
			if _, err := io.Copy(io.Discard, r); err != nil {
				return fmt.Errorf("read symbols for column %q: %w", c.Name, err)
			}
			// Checked here rather than at the end of the conversion: this
			// column's symbols are about to be decoded into every row that
			// refers to them, and there is nothing to gain from doing that
			// work first.
			var want [32]byte
			copy(want[:], sum.Sum(nil))
			same, err := digestMatches(verify, pos, c.Length, want)
			if err != nil {
				return fmt.Errorf("verify symbols for column %q: %w", c.Name, err)
			}
			if !same {
				return fmt.Errorf("%w: the symbol table of column %q, %d bytes at offset %d, "+
					"did not read the same way twice", ErrUnstableRead, c.Name, c.Length, pos)
			}
		}
		qf.Symbols[i] = syms
		qf.Profiles[i] = prof
		pos += c.Length
	}
	qf.RecordStart = pos
	if err := qf.checkRecordArea(); err != nil {
		return err
	}
	return nil
}

func (qf *File) checkRecordArea() error {
	st, err := qf.f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", qf.Path, err)
	}
	need := qf.RecordStart + qf.NoOfRecords*int64(qf.RecordByteSize)
	if need > st.Size() {
		return fmt.Errorf("%s is truncated: %d records of %d bytes starting at offset %d need %d bytes, file has %d",
			qf.Path, qf.NoOfRecords, qf.RecordByteSize, qf.RecordStart, need, st.Size())
	}
	return nil
}

// Symbol resolves one symbol index for a column, validating the range.
func (qf *File) Symbol(colIdx int, symIdx int64) (Symbol, error) {
	syms := qf.Symbols[colIdx]
	if symIdx < 0 {
		return Symbol{Kind: SymbolNull}, nil
	}
	if symIdx >= int64(len(syms)) {
		return Symbol{}, fmt.Errorf("column %q: decoded symbol id %d, but symbol table has %d entries",
			qf.Columns[colIdx].Name, symIdx, len(syms))
	}
	return syms[symIdx], nil
}
