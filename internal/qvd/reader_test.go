package qvd

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeQVD assembles a minimal QVD: header, symbol tables, records.
func writeQVD(t *testing.T, header string, symbols, records []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.qvd")
	body := append([]byte(header), 0x00)
	body = append(body, symbols...)
	body = append(body, records...)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// twoColumnFile builds a file with an INTEGER column (3 symbols) and an ASCII
// column (2 symbols), sharing one byte per record.
func twoColumnFile(t *testing.T) string {
	t.Helper()
	var syms []byte
	for _, v := range []int32{10, 20, 30} {
		b := []byte{tagInt, 0, 0, 0, 0}
		binary.LittleEndian.PutUint32(b[1:], uint32(v))
		syms = append(syms, b...)
	}
	intLen := len(syms)
	for _, s := range []string{"aa", "bb"} {
		syms = append(syms, tagString)
		syms = append(syms, s...)
		syms = append(syms, 0x00)
	}
	strLen := len(syms) - intLen

	header := `<QvdTableHeader><TableName>T</TableName><Fields>` +
		`<QvdFieldHeader><FieldName>Num</FieldName><BitOffset>0</BitOffset><BitWidth>2</BitWidth>` +
		`<Bias>0</Bias><NumberFormat><Type>INTEGER</Type></NumberFormat>` +
		`<NoOfSymbols>3</NoOfSymbols><Offset>0</Offset><Length>` + itoa(intLen) + `</Length></QvdFieldHeader>` +
		`<QvdFieldHeader><FieldName>Text</FieldName><BitOffset>2</BitOffset><BitWidth>1</BitWidth>` +
		`<Bias>0</Bias><NumberFormat><Type>ASCII</Type></NumberFormat>` +
		`<NoOfSymbols>2</NoOfSymbols><Offset>` + itoa(intLen) + `</Offset><Length>` + itoa(strLen) + `</Length></QvdFieldHeader>` +
		`</Fields><RecordByteSize>1</RecordByteSize><NoOfRecords>3</NoOfRecords></QvdTableHeader>`

	// Rows: (Num=0,Text=0), (Num=1,Text=1), (Num=2,Text=0).
	records := []byte{0b000, 0b101, 0b010}
	return writeQVD(t, header, syms, records)
}

func itoa(n int) string { return strconv.Itoa(n) }

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func TestOpenAndReadSymbols(t *testing.T) {
	f, err := Open(twoColumnFile(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	if f.Header.TableName != "T" || f.NoOfRecords != 3 || f.RecordByteSize != 1 {
		t.Errorf("header = %+v", f.Header)
	}
	if got := f.ColumnNames(); len(got) != 2 || got[0] != "Num" || got[1] != "Text" {
		t.Errorf("column names = %v", got)
	}
	if err := f.ReadSymbols(UnknownSymbolError); err != nil {
		t.Fatalf("ReadSymbols: %v", err)
	}
	if len(f.Symbols[0]) != 3 || f.Symbols[0][2].Int != 30 {
		t.Errorf("Num symbols = %+v", f.Symbols[0])
	}
	if len(f.Symbols[1]) != 2 || f.Symbols[1][1].Text != "bb" {
		t.Errorf("Text symbols = %+v", f.Symbols[1])
	}
	if f.RecordStart == 0 || f.RecordStart <= f.HeaderEnd {
		t.Errorf("RecordStart = %d, HeaderEnd = %d", f.RecordStart, f.HeaderEnd)
	}

	// Decode all three records.
	raw := make([]byte, 3)
	if _, err := f.FileHandle().ReadAt(raw, f.RecordStart); err != nil {
		t.Fatal(err)
	}
	out := make([]int64, 2)
	wantNum := []int64{10, 20, 30}
	wantText := []string{"aa", "bb", "aa"}
	for r := 0; r < 3; r++ {
		if err := DecodeRecord(raw[r:r+1], f.Columns, out); err != nil {
			t.Fatal(err)
		}
		num, err := f.Symbol(0, out[0])
		if err != nil {
			t.Fatal(err)
		}
		txt, err := f.Symbol(1, out[1])
		if err != nil {
			t.Fatal(err)
		}
		if num.Int != wantNum[r] || txt.Text != wantText[r] {
			t.Errorf("row %d = (%d, %q), want (%d, %q)", r, num.Int, txt.Text, wantNum[r], wantText[r])
		}
	}
}

func TestSelectColumnsSkipsSymbolTables(t *testing.T) {
	f, err := Open(twoColumnFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := f.SelectColumns([]string{"text"}); err != nil { // case-insensitive
		t.Fatalf("SelectColumns: %v", err)
	}
	if got := f.SelectedColumns(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("selected = %v, want [1]", got)
	}
	if err := f.ReadSymbols(UnknownSymbolError); err != nil {
		t.Fatalf("ReadSymbols: %v", err)
	}
	if f.Symbols[0] != nil {
		t.Error("the skipped column's symbols should not be loaded")
	}
	if len(f.Symbols[1]) != 2 {
		t.Errorf("selected column has %d symbols, want 2", len(f.Symbols[1]))
	}
	// Skipping must still land on the record area.
	raw := make([]byte, 3)
	if _, err := f.FileHandle().ReadAt(raw, f.RecordStart); err != nil {
		t.Fatalf("record area misplaced after skipping a symbol table: %v", err)
	}
	if raw[1] != 0b101 {
		t.Errorf("record byte = %#b, want 0b101", raw[1])
	}
}

func TestSelectColumnsUnknownName(t *testing.T) {
	f, err := Open(twoColumnFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = f.SelectColumns([]string{"Num", "Missing"})
	if err == nil {
		t.Fatal("expected an error for an unknown column")
	}
	for _, want := range []string{"missing", "Num", "Text"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestSelectColumnsEmptySelectsAll(t *testing.T) {
	f, err := Open(twoColumnFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.SelectColumns(nil); err != nil {
		t.Fatal(err)
	}
	if len(f.SelectedColumns()) != 2 {
		t.Errorf("an empty selection should keep all columns")
	}
}

func TestOpenRejectsNonQVD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not.qvd")
	if err := os.WriteFile(path, []byte("hello, world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("expected an error for a non-QVD file")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "nope.qvd")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestReadSymbolsDetectsTruncatedRecordArea(t *testing.T) {
	path := twoColumnFile(t)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the last two record bytes; the header still claims three rows.
	if err := os.WriteFile(path, body[:len(body)-2], 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = f.ReadSymbols(UnknownSymbolError)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("err = %v, want a truncation error", err)
	}
}

func TestReadSymbolsReportsColumnOnFailure(t *testing.T) {
	// A symbol table whose declared count exceeds its declared length.
	syms := []byte{tagInt, 1, 0, 0, 0}
	header := `<QvdTableHeader><TableName>T</TableName><Fields>` +
		`<QvdFieldHeader><FieldName>Broken</FieldName><BitOffset>0</BitOffset><BitWidth>2</BitWidth>` +
		`<Bias>0</Bias><NumberFormat><Type>INTEGER</Type></NumberFormat>` +
		`<NoOfSymbols>3</NoOfSymbols><Offset>0</Offset><Length>5</Length></QvdFieldHeader>` +
		`</Fields><RecordByteSize>1</RecordByteSize><NoOfRecords>1</NoOfRecords></QvdTableHeader>`
	path := writeQVD(t, header, syms, []byte{0})

	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = f.ReadSymbols(UnknownSymbolError)
	if err == nil {
		t.Fatal("expected a symbol decoding error")
	}
	if !strings.Contains(err.Error(), `"Broken"`) {
		t.Errorf("error %q should name the failing column", err)
	}
}

func TestSymbolNegativeIndexIsNull(t *testing.T) {
	f, err := Open(twoColumnFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.ReadSymbols(UnknownSymbolError); err != nil {
		t.Fatal(err)
	}
	s, err := f.Symbol(0, NullSymbol)
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != SymbolNull {
		t.Errorf("negative symbol index = %v, want SymbolNull", s.Kind)
	}
}

func TestSymbolDoubleRoundTrip(t *testing.T) {
	// Guard the little-endian double decoding against byte-order regressions.
	want := -1234.5678
	b := append([]byte{tagDouble}, make([]byte, 8)...)
	binary.LittleEndian.PutUint64(b[1:], math.Float64bits(want))
	syms, _, err := ReadSymbolTable(bytesReader(b), 1, UnknownSymbolError)
	if err != nil {
		t.Fatal(err)
	}
	if syms[0].Float != want {
		t.Errorf("double = %v, want %v", syms[0].Float, want)
	}
}

// A pattern that drops nothing has to be reported, because the two ways of
// getting one wrong both look like success: "%" is not the wildcard "%*", and
// --exclude never sees the name --field-regex would produce.
func TestExcludeColumnsReportsPatternsThatMatchNothing(t *testing.T) {
	for _, tc := range []struct {
		name          string
		patterns      []string
		wantDropped   string
		wantUnmatched string
	}{
		{"wildcard forgotten", []string{"%"}, "", "%"},
		{"wildcard present", []string{"%*"}, "%KEY", ""},
		{"case is ignored", []string{"counter"}, "Counter", ""},
		{"two patterns, one dead", []string{"%*", "nope"}, "%KEY", "nope"},
		{"both cover the same field", []string{"%*", "%KE*"}, "%KEY", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &File{Path: "t.qvd", Columns: []Column{
				{Name: "%KEY", Selected: true},
				{Name: "Counter", Selected: true},
				{Name: "Value", Selected: true},
			}}
			dropped, unmatched, err := f.ExcludeColumns(tc.patterns)
			if err != nil {
				t.Fatalf("ExcludeColumns: %v", err)
			}
			if got := strings.Join(dropped, ","); got != tc.wantDropped {
				t.Errorf("dropped = %q, want %q", got, tc.wantDropped)
			}
			if got := strings.Join(unmatched, ","); got != tc.wantUnmatched {
				t.Errorf("unmatched = %q, want %q", got, tc.wantUnmatched)
			}
		})
	}
}

// A pattern is judged against the columns selected on entry, so --columns
// having already dropped a field does not turn a working pattern into a
// reported one.
func TestExcludeColumnsIgnoresAlreadyDeselectedFields(t *testing.T) {
	f := &File{Path: "t.qvd", Columns: []Column{
		{Name: "%KEY", Selected: false},
		{Name: "Value", Selected: true},
	}}
	_, unmatched, err := f.ExcludeColumns([]string{"%*"})
	if err != nil {
		t.Fatalf("ExcludeColumns: %v", err)
	}
	if strings.Join(unmatched, ",") != "%*" {
		t.Errorf("unmatched = %v, want [%%*]: the field was not in the selection", unmatched)
	}
}
