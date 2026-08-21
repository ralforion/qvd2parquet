package qvd

import (
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
	h, end, err := ReadHeader(f)
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
// It returns the names that were excluded, in header order. Excluding every
// column is an error, since the output would have no columns.
func (qf *File) ExcludeColumns(patterns []string) ([]string, error) {
	var pats []string
	for _, p := range patterns {
		if p = strings.TrimSpace(p); p != "" {
			pats = append(pats, p)
		}
	}
	if len(pats) == 0 {
		return nil, nil
	}
	var dropped []string
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
		return nil, fmt.Errorf("--exclude %s removed every column from %s",
			strings.Join(pats, ", "), qf.Path)
	}
	return dropped, nil
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
