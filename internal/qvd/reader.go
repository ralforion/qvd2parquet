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
	// Records are decoded from the handle whose read produced this header.
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

// readHeaderRetrying reads the header twice, from two handles, and returns the
// one that holds up.
//
// The header is read twice always, not only after a failure. It is a few
// hundred kilobytes against files of gigabytes, so the cost is nothing, and
// whether two consecutive reads of the same bytes agree is the one fact about
// a suspect header that cannot be recovered after the run: by the time anyone
// goes to look, the file on disk reads correctly either way. It also catches
// what a failure-triggered retry cannot, a read that differs but still parses
// -- a wrong digit in NoOfRecords or BitWidth is valid XML and would otherwise
// go through unremarked.
//
// The gate is a strict parse. A tolerant one accepts an unknown entity or an
// unclosed element and hands back a header nobody checked, which is the case
// the second read exists to catch. Tolerant parsing and then repair are tried
// only after both reads have been given the strict test, so a transient bad
// read is never mistaken for a malformed QVD, and a malformed QVD is never
// quietly rewritten on the strength of one read.
//
// The returned handle is the one whose read was trusted, and the other is
// closed: records must be decoded from the file that produced the header the
// schema was built from.
func readHeaderRetrying(path string, first *os.File) (*TableHeader, int64, *os.File, string, error) {
	return readHeaderRetryingWithOpen(path, first, os.Open)
}

// headerRead is one attempt at reading the header.
type headerRead struct {
	f   *os.File
	raw []byte
	end int64
	err error
}

// readHeaderRetryingWithOpen is readHeaderRetrying with the reopen injected, so
// that a read which fails once and succeeds next can be tested.
func readHeaderRetryingWithOpen(path string, first *os.File,
	open func(string) (*os.File, error)) (*TableHeader, int64, *os.File, string, error) {

	a := headerRead{f: first}
	a.raw, a.end, a.err = ReadHeaderBytes(a.f)

	var b headerRead
	if f2, err := open(path); err != nil {
		b.err = err
	} else {
		b.f = f2
		b.raw, b.end, b.err = ReadHeaderBytes(b.f)
	}

	disagree := ""
	if a.err == nil && b.err == nil && !bytes.Equal(a.raw, b.raw) {
		off := firstDiffOffset(a.raw, b.raw)
		disagree = fmt.Sprintf("TWO READS OF THE HEADER RETURNED DIFFERENT BYTES: %d then %d, "+
			"first difference at offset %d, line %d; the file was read wrong rather than written wrong",
			len(a.raw), len(b.raw), off, lineAt(a.raw, off))
	}

	// Strict first, on both reads, before anything tolerant or mended.
	for _, r := range []headerRead{a, b} {
		if r.err != nil {
			continue
		}
		h, err := decodeHeaderStrict(r.raw)
		if err != nil {
			continue
		}
		note := disagree
		if r.f == b.f && a.err == nil {
			note = joinNotes(fmt.Sprintf("the first read of the header did not parse; a second read %s and parsed",
				describeDiff(a.raw, b.raw)), disagree)
		}
		return h, r.end, keep(r.f, a.f, b.f), note, nil
	}

	// Neither read is valid XML, so the header is not valid XML. Mend what can
	// be mended without guessing; ParseHeaderXML records what it did.
	for _, r := range []headerRead{a, b} {
		if r.err != nil {
			continue
		}
		if h, err := ParseHeaderXML(r.raw); err == nil {
			return h, r.end, keep(r.f, a.f, b.f), disagree, nil
		}
	}

	if b.f != nil {
		b.f.Close()
	}
	err := a.err
	if err == nil {
		_, err = ParseHeaderXML(a.raw)
	}
	return nil, 0, nil, "", fmt.Errorf("%w%s", err, compareNote(a.raw, b.raw, b.err))
}

// keep closes whichever of the two handles was not chosen and returns the one
// that was. A nil handle is one that never opened.
func keep(chosen, first, second *os.File) *os.File {
	for _, f := range []*os.File{first, second} {
		if f != nil && f != chosen {
			f.Close()
		}
	}
	return chosen
}

// joinNotes puts two warnings on one line, dropping the empty ones.
func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// describeDiff says how two reads of the same header differ.
func describeDiff(a, b []byte) string {
	if bytes.Equal(a, b) {
		return "returned the same bytes"
	}
	off := firstDiffOffset(a, b)
	return fmt.Sprintf("returned different bytes (%d then %d, first difference at offset %d, line %d)",
		len(a), len(b), off, lineAt(a, off))
}

// firstDiffOffset is the offset of the first byte at which a and b differ.
func firstDiffOffset(a, b []byte) int {
	off := 0
	for off < len(a) && off < len(b) && a[off] == b[off] {
		off++
	}
	return off
}

// compareNote reports what a second read of the header saw, for a header that
// could not be parsed either way. Nothing outside the process can tell a file
// that was written wrong from one that was read wrong after the fact, since
// the bytes on disk are the same either way by the time anyone looks.
func compareNote(raw, raw2 []byte, err2 error) string {
	switch {
	case err2 != nil:
		return fmt.Sprintf(" [a second read of the header failed too: %v]", err2)
	case bytes.Equal(raw, raw2):
		return fmt.Sprintf(" [a second read returned the same %d bytes, so the file holds what was parsed]", len(raw))
	}
	off := firstDiffOffset(raw, raw2)
	return fmt.Sprintf(" [TWO READS OF THE HEADER RETURNED DIFFERENT BYTES: %d then %d, "+
		"first difference at offset %d, line %d; it was read wrong rather than written wrong]",
		len(raw), len(raw2), off, lineAt(raw, off))
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
