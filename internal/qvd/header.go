package qvd

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ErrUnsupported marks QVD features this version deliberately does not handle.
var ErrUnsupported = errors.New("unsupported QVD feature")

// maxHeaderBytes bounds the XML header scan so a non-QVD input fails fast
// instead of buffering the whole file.
const maxHeaderBytes = 64 << 20

// TableHeader mirrors the subset of QvdTableHeader that the converter needs.
type TableHeader struct {
	XMLName        xml.Name      `xml:"QvdTableHeader"`
	QvBuildNo      string        `xml:"QvBuildNo"`
	CreatorDoc     string        `xml:"CreatorDoc"`
	CreateUtcTime  string        `xml:"CreateUtcTime"`
	TableName      string        `xml:"TableName"`
	Compression    string        `xml:"Compression"`
	RecordByteSize int           `xml:"RecordByteSize"`
	NoOfRecords    int64         `xml:"NoOfRecords"`
	Offset         int64         `xml:"Offset"`
	Length         int64         `xml:"Length"`
	Fields         []FieldHeader `xml:"Fields>QvdFieldHeader"`
}

// FieldHeader mirrors one QvdFieldHeader element.
type FieldHeader struct {
	FieldName    string       `xml:"FieldName"`
	BitOffset    int          `xml:"BitOffset"`
	BitWidth     int          `xml:"BitWidth"`
	Bias         int64        `xml:"Bias"`
	NumberFormat NumberFormat `xml:"NumberFormat"`
	NoOfSymbols  int64        `xml:"NoOfSymbols"`
	Offset       int64        `xml:"Offset"`
	Length       int64        `xml:"Length"`
	Comment      string       `xml:"Comment"`
	// Tags carries Qlik's semantic field tags, such as $numeric, $integer,
	// $text, $date, $timestamp and $time. Qlik Sense and newer QlikView
	// versions populate these even when NumberFormat/Type is left empty, so
	// they are often the only reliable statement of a field's meaning.
	Tags []string `xml:"Tags>String"`
}

// NumberFormat mirrors the field's declared display/number format.
type NumberFormat struct {
	Type    string `xml:"Type"`
	NDec    int    `xml:"nDec"`
	UseThou int    `xml:"UseThou"`
	Fmt     string `xml:"Fmt"`
	Dec     string `xml:"Dec"`
	Thou    string `xml:"Thou"`
}

// ReadHeader consumes the UTF-8 XML header up to and including its terminating
// 0x00 byte and returns the parsed header plus the byte offset at which the
// first symbol table begins.
func ReadHeader(r io.Reader) (*TableHeader, int64, error) {
	br := bufio.NewReader(io.LimitReader(r, maxHeaderBytes))
	raw, err := br.ReadBytes(0x00)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, 0, errors.New("no XML header terminator (0x00) found: not a QVD file?")
		}
		return nil, 0, fmt.Errorf("read XML header: %w", err)
	}
	end := int64(len(raw))
	raw = raw[:len(raw)-1] // drop the terminator

	h, err := ParseHeaderXML(raw)
	if err != nil {
		return nil, 0, err
	}
	return h, end, nil
}

// ParseHeaderXML unmarshals the raw header bytes (without the 0x00 terminator).
func ParseHeaderXML(raw []byte) (*TableHeader, error) {
	// Some writers emit a UTF-8 BOM before the declaration.
	raw = trimBOM(raw)
	var h TableHeader
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	if err := dec.Decode(&h); err != nil {
		return nil, fmt.Errorf("parse QVD XML header: %w", err)
	}
	if h.XMLName.Local != "QvdTableHeader" {
		return nil, fmt.Errorf("unexpected QVD header root element %q", h.XMLName.Local)
	}
	return &h, nil
}

func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// Validate checks the structural invariants the decoder relies on.
func (h *TableHeader) Validate() error {
	if c := strings.TrimSpace(h.Compression); c != "" && c != "0" {
		return fmt.Errorf("%w: compression %q", ErrUnsupported, c)
	}
	if len(h.Fields) == 0 {
		return errors.New("QVD header declares no fields")
	}
	if h.NoOfRecords < 0 {
		return fmt.Errorf("QVD header declares negative NoOfRecords %d", h.NoOfRecords)
	}
	if h.NoOfRecords > 0 && h.RecordByteSize <= 0 {
		return fmt.Errorf("QVD header declares %d records but RecordByteSize %d",
			h.NoOfRecords, h.RecordByteSize)
	}
	for i, f := range h.Fields {
		switch {
		case f.FieldName == "":
			return fmt.Errorf("field %d has an empty FieldName", i)
		case f.BitOffset < 0:
			return fmt.Errorf("field %q has negative BitOffset %d", f.FieldName, f.BitOffset)
		case f.BitWidth < 0:
			return fmt.Errorf("field %q has negative BitWidth %d", f.FieldName, f.BitWidth)
		case f.BitWidth > 64:
			return fmt.Errorf("field %q has BitWidth %d, more than 64 bits", f.FieldName, f.BitWidth)
		case f.NoOfSymbols < 0:
			return fmt.Errorf("field %q has negative NoOfSymbols %d", f.FieldName, f.NoOfSymbols)
		case f.Length < 0:
			return fmt.Errorf("field %q has negative symbol table Length %d", f.FieldName, f.Length)
		}
		if end := f.BitOffset + f.BitWidth; h.NoOfRecords > 0 && end > h.RecordByteSize*8 {
			return fmt.Errorf("field %q bit range [%d,%d) exceeds RecordByteSize %d (%d bits)",
				f.FieldName, f.BitOffset, end, h.RecordByteSize, h.RecordByteSize*8)
		}
	}
	if err := h.checkBitRanges(); err != nil {
		return err
	}
	return nil
}

// checkBitRanges rejects overlapping record bit ranges, which would mean two
// fields decode from the same bits.
func (h *TableHeader) checkBitRanges() error {
	type span struct {
		lo, hi int // [lo,hi)
		name   string
	}
	spans := make([]span, 0, len(h.Fields))
	for _, f := range h.Fields {
		if f.BitWidth == 0 {
			continue // single-symbol field, occupies no record bits
		}
		spans = append(spans, span{f.BitOffset, f.BitOffset + f.BitWidth, f.FieldName})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })
	for i := 1; i < len(spans); i++ {
		if spans[i].lo < spans[i-1].hi {
			return fmt.Errorf("fields %q [%d,%d) and %q [%d,%d) have overlapping record bit ranges",
				spans[i-1].name, spans[i-1].lo, spans[i-1].hi,
				spans[i].name, spans[i].lo, spans[i].hi)
		}
	}
	return nil
}

// Columns builds the normalized column model. Every column is marked selected;
// callers narrow the selection afterwards.
func (h *TableHeader) Columns() []Column {
	cols := make([]Column, len(h.Fields))
	for i, f := range h.Fields {
		cols[i] = Column{
			Index:       i,
			Name:        f.FieldName,
			QlikType:    ParseQlikType(f.NumberFormat.Type),
			BitOffset:   f.BitOffset,
			BitWidth:    f.BitWidth,
			Bias:        f.Bias,
			SymbolCount: f.NoOfSymbols,
			Offset:      f.Offset,
			Length:      f.Length,
			Selected:    true,
			NDec:        f.NumberFormat.NDec,
			DecSep:      decodeSeparator(f.NumberFormat.Dec),
			ThouSep:     decodeSeparator(f.NumberFormat.Thou),
			Fmt:         f.NumberFormat.Fmt,
			Tags:        normalizeTags(f.Tags),
		}
	}
	return cols
}

// normalizeTags lower-cases and trims the field's tags, dropping blanks.
func normalizeTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// decodeSeparator normalizes a NumberFormat separator, which Qlik may store
// either literally or as a decimal character code.
func decodeSeparator(s string) string {
	if s == "" {
		return ""
	}
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 && n < 0x110000 {
		return string(rune(n))
	}
	return s
}
