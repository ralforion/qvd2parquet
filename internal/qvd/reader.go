package qvd

import (
	"bytes"
	"fmt"
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
	h, end, trusted, readNote, err := readHeaderRetrying(path, f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f = trusted
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
	h.ReadNote = readNote
	return qf, nil
}

// readHeaderRetrying reads and parses the header, and on any failure reads it
// once more from a new handle before giving up.
//
// A header that will not read or parse once and does both a moment later, over
// a file whose bytes have not changed, was not read correctly the first time.
// Nothing the caller can do about that is better than trying again, and in a
// batch that runs for hours over hundreds of files it is the difference
// between a warning on one file and losing that file's whole conversion.
//
// The retry is bounded at one, and it is not silent: a second read that
// disagrees with the first is reported, with the offset it disagrees at,
// because a file being read differently on two consecutive attempts is worth
// more attention than the conversion it rescued.
func readHeaderRetrying(path string, f *os.File) (*TableHeader, int64, *os.File, string, error) {
	return readHeaderRetryingWithOpen(path, f, os.Open)
}

func readHeaderRetryingWithOpen(path string, f *os.File, open func(string) (*os.File, error)) (*TableHeader, int64, *os.File, string, error) {
	raw, end, readErr := ReadHeaderBytes(f)
	err := readErr
	var h *TableHeader
	if err == nil {
		h, err = parseHeaderXMLStrict(raw)
	}
	if err == nil {
		return h, end, f, "", nil
	}

	f2, oerr := open(path)
	if oerr != nil {
		return nil, 0, f, "", err
	}
	same, serr := sameOpenFile(f, f2)
	if serr != nil {
		f2.Close()
		return nil, 0, f, "", fmt.Errorf("%w [could not verify the retry read came from the same file: %v]", err, serr)
	}
	if !same {
		f2.Close()
		return nil, 0, f, "", fmt.Errorf("%w [the retry opened a different file; the input changed while its header was being read]", err)
	}
	raw2, end2, readErr2 := ReadHeaderBytes(f2)
	err2 := readErr2
	if err2 == nil {
		h, err2 = parseHeaderXMLStrict(raw2)
	}
	diff := describeDiff(raw, raw2)
	if err2 == nil {
		f.Close()
		return h, end2, f2, fmt.Sprintf(
			"the first read of the header failed (%v); a second read %s and parsed",
			err, diff), nil
	}
	defer f2.Close()

	if readErr == nil && readErr2 == nil && bytes.Equal(raw, raw2) {
		h, err = ParseHeaderXML(raw)
		if err == nil {
			return h, end, f, "", nil
		}
		return nil, 0, f, "", fmt.Errorf("%w [a second read returned the same %d bytes, so the file holds what was parsed]", err, len(raw))
	}
	// Both attempts failed. Report the first failure, since that is the
	// one whose bytes were examined, and say whether the second read saw
	// the same file: written wrong and read wrong call for opposite
	// responses and look identical afterwards.
	return nil, 0, f, "", fmt.Errorf("%w [a second read %s and also failed: %v]", err, diff, err2)
}

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

// describeDiff says how two reads of the same header differ.
func describeDiff(a, b []byte) string {
	if bytes.Equal(a, b) {
		return "returned the same bytes"
	}
	off := 0
	for off < len(a) && off < len(b) && a[off] == b[off] {
		off++
	}
	return fmt.Sprintf("returned different bytes (%d then %d, first difference at offset %d, line %d)",
		len(a), len(b), off, lineAt(a, off))
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
		syms, prof, err := ReadSymbolTable(sec, c.SymbolCount, policy)
		if err != nil {
			return fmt.Errorf("read symbols for column %q: %w", c.Name, err)
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
