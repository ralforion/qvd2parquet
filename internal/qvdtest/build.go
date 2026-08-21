// Package qvdtest builds synthetic QVD files for tests and benchmarks.
package qvdtest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"

	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// Field describes one synthetic column: its declared format, its symbol table
// and, per row, which symbol the record points at (-1 means null).
type Field struct {
	Name    string
	Type    string // NumberFormat/Type, e.g. "INTEGER", "MONEY"
	NDec    int
	Dec     string // decimal separator, literal
	Thou    string // thousands separator, literal
	Symbols []qvd.Symbol
	Rows    []int
	// Tags are Qlik semantic field tags such as "$numeric", "$date".
	Tags []string
}

// Table is a synthetic QVD.
type Table struct {
	Name   string
	Fields []Field
}

// EncodeSymbol writes one symbol in QVD symbol-table form.
func EncodeSymbol(w *bytes.Buffer, s qvd.Symbol) {
	switch s.Kind {
	case qvd.SymbolNull:
		w.WriteByte(0x00)
	case qvd.SymbolInt:
		w.WriteByte(0x01)
		binary.Write(w, binary.LittleEndian, int32(s.Int))
	case qvd.SymbolFloat:
		w.WriteByte(0x02)
		binary.Write(w, binary.LittleEndian, math.Float64bits(s.Float))
	case qvd.SymbolString:
		w.WriteByte(0x04)
		w.WriteString(s.Text)
		w.WriteByte(0x00)
	case qvd.SymbolDualIntString:
		w.WriteByte(0x05)
		binary.Write(w, binary.LittleEndian, int32(s.Int))
		w.WriteString(s.Text)
		w.WriteByte(0x00)
	case qvd.SymbolDualFloatString:
		w.WriteByte(0x06)
		binary.Write(w, binary.LittleEndian, math.Float64bits(s.Float))
		w.WriteString(s.Text)
		w.WriteByte(0x00)
	}
}

// layout is the computed record encoding for one field.
type layout struct {
	bitOffset int
	bitWidth  int
	bias      int64
}

// Build writes the table to path and returns the number of rows.
func Build(path string, t Table) (int64, error) {
	if len(t.Fields) == 0 {
		return 0, fmt.Errorf("qvdtest: table %q has no fields", t.Name)
	}
	rows := len(t.Fields[0].Rows)
	for _, f := range t.Fields {
		if len(f.Rows) != rows {
			return 0, fmt.Errorf("qvdtest: field %q has %d rows, expected %d", f.Name, len(f.Rows), rows)
		}
	}

	// Symbol tables.
	var symBuf bytes.Buffer
	offsets := make([]int64, len(t.Fields))
	lengths := make([]int64, len(t.Fields))
	for i, f := range t.Fields {
		offsets[i] = int64(symBuf.Len())
		for _, s := range f.Symbols {
			EncodeSymbol(&symBuf, s)
		}
		lengths[i] = int64(symBuf.Len()) - offsets[i]
	}

	// Record bit layout. A null in any row forces bias -1 so the stored index
	// can be shifted up by one and index 0 decodes as null.
	layouts := make([]layout, len(t.Fields))
	bitPos := 0
	for i, f := range t.Fields {
		hasNull := false
		for _, r := range f.Rows {
			if r < 0 {
				hasNull = true
				break
			}
		}
		l := layout{bitOffset: bitPos}
		maxStored := len(f.Symbols) - 1
		if hasNull {
			l.bias = -1
			maxStored = len(f.Symbols)
		}
		switch {
		case maxStored <= 0 && !hasNull:
			l.bitWidth = 0 // single symbol, no record bits
		case maxStored <= 0:
			l.bitWidth = 1 // nullable, so the record still needs one bit
		default:
			l.bitWidth = bits.Len64(uint64(maxStored))
		}
		layouts[i] = l
		bitPos += l.bitWidth
	}
	recordByteSize := (bitPos + 7) / 8
	if recordByteSize == 0 && rows > 0 {
		recordByteSize = 1
	}

	// Records.
	recBuf := make([]byte, rows*recordByteSize)
	for r := 0; r < rows; r++ {
		rec := recBuf[r*recordByteSize : (r+1)*recordByteSize]
		for i, f := range t.Fields {
			l := layouts[i]
			if l.bitWidth == 0 {
				continue
			}
			stored := int64(f.Rows[r]) - l.bias
			if f.Rows[r] < 0 {
				stored = 0 // decodes to bias, i.e. -1, i.e. null
			}
			writeBitsLE(rec, l.bitOffset, l.bitWidth, uint64(stored))
		}
	}

	header := buildHeader(t, layouts, offsets, lengths, rows, recordByteSize, int64(symBuf.Len()))

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	var out bytes.Buffer
	out.WriteString(header)
	out.WriteByte(0x00)
	out.Write(symBuf.Bytes())
	out.Write(recBuf)
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		return 0, err
	}
	return int64(rows), nil
}

func writeBitsLE(rec []byte, bitOffset, bitWidth int, v uint64) {
	for i := 0; i < bitWidth; i++ {
		if v&(1<<uint(i)) != 0 {
			p := bitOffset + i
			rec[p/8] |= 1 << uint(p%8)
		}
	}
}

func buildHeader(t Table, layouts []layout, offsets, lengths []int64,
	rows, recordByteSize int, symbolBytes int64) string {

	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString("<QvdTableHeader>\n")
	fmt.Fprintf(&b, "  <QvBuildNo>50000</QvBuildNo>\n")
	fmt.Fprintf(&b, "  <CreatorDoc>qvdtest</CreatorDoc>\n")
	fmt.Fprintf(&b, "  <TableName>%s</TableName>\n", t.Name)
	b.WriteString("  <Fields>\n")
	for i, f := range t.Fields {
		l := layouts[i]
		b.WriteString("    <QvdFieldHeader>\n")
		fmt.Fprintf(&b, "      <FieldName>%s</FieldName>\n", f.Name)
		fmt.Fprintf(&b, "      <BitOffset>%d</BitOffset>\n", l.bitOffset)
		fmt.Fprintf(&b, "      <BitWidth>%d</BitWidth>\n", l.bitWidth)
		fmt.Fprintf(&b, "      <Bias>%d</Bias>\n", l.bias)
		b.WriteString("      <NumberFormat>\n")
		fmt.Fprintf(&b, "        <Type>%s</Type>\n", orDefault(f.Type, "UNKNOWN"))
		fmt.Fprintf(&b, "        <nDec>%d</nDec>\n", f.NDec)
		fmt.Fprintf(&b, "        <UseThou>%d</UseThou>\n", boolToInt(f.Thou != ""))
		fmt.Fprintf(&b, "        <Fmt></Fmt>\n")
		fmt.Fprintf(&b, "        <Dec>%s</Dec>\n", sepCode(f.Dec))
		fmt.Fprintf(&b, "        <Thou>%s</Thou>\n", sepCode(f.Thou))
		b.WriteString("      </NumberFormat>\n")
		fmt.Fprintf(&b, "      <NoOfSymbols>%d</NoOfSymbols>\n", len(f.Symbols))
		fmt.Fprintf(&b, "      <Offset>%d</Offset>\n", offsets[i])
		fmt.Fprintf(&b, "      <Length>%d</Length>\n", lengths[i])
		b.WriteString("      <Tags>\n")
		for _, t := range f.Tags {
			fmt.Fprintf(&b, "        <String>%s</String>\n", t)
		}
		b.WriteString("      </Tags>\n")
		b.WriteString("    </QvdFieldHeader>\n")
	}
	b.WriteString("  </Fields>\n")
	fmt.Fprintf(&b, "  <RecordByteSize>%d</RecordByteSize>\n", recordByteSize)
	fmt.Fprintf(&b, "  <NoOfRecords>%d</NoOfRecords>\n", rows)
	fmt.Fprintf(&b, "  <Offset>%d</Offset>\n", symbolBytes)
	fmt.Fprintf(&b, "  <Length>%d</Length>\n", int64(rows)*int64(recordByteSize))
	b.WriteString("</QvdTableHeader>")
	return b.String()
}

// sepCode renders a separator the way Qlik does, as a decimal character code.
func sepCode(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf("%d", []rune(s)[0])
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Helpers for building symbol tables concisely.

// Int returns an integer symbol.
func Int(v int64) qvd.Symbol { return qvd.Symbol{Kind: qvd.SymbolInt, Int: v} }

// Float returns a double symbol.
func Float(v float64) qvd.Symbol { return qvd.Symbol{Kind: qvd.SymbolFloat, Float: v} }

// Str returns a string symbol.
func Str(v string) qvd.Symbol { return qvd.Symbol{Kind: qvd.SymbolString, Text: v} }

// DualInt returns an integer symbol carrying a display string.
func DualInt(v int64, text string) qvd.Symbol {
	return qvd.Symbol{Kind: qvd.SymbolDualIntString, Int: v, Text: text}
}

// DualFloat returns a double symbol carrying a display string.
func DualFloat(v float64, text string) qvd.Symbol {
	return qvd.Symbol{Kind: qvd.SymbolDualFloatString, Float: v, Text: text}
}

// Null returns a null symbol.
func Null() qvd.Symbol { return qvd.Symbol{Kind: qvd.SymbolNull} }
