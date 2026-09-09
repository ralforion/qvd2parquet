package qvd

import (
	"strings"
	"testing"
)

// badNameHeader is sampleHeader with one field name Qlik wrote unescaped.
func badNameHeader(name string) string {
	return strings.Replace(sampleHeader, "<FieldName>Amount</FieldName>",
		"<FieldName>"+name+"</FieldName>", 1)
}

func TestParseHeaderXMLRepairsUnescapedNames(t *testing.T) {
	// Every one of these is a real shape of the same mistake, and each makes
	// encoding/xml fail with a different message, so the repair cannot key off
	// the error text.
	names := []string{
		"Ist <Soll (Abw.)",   // expected attribute name in element
		"Menge <5 %",         // invalid XML name
		"Dauer < 8 Std",      // expected element name after <
		"Abw <-> Plan",       // invalid XML name
		"R&D Kosten",         // a bare ampersand, which xml tolerates but should survive
		"Soll & Ist <> Plan", // both, in one name
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			h, err := ParseHeaderXML([]byte(badNameHeader(name)))
			if err != nil {
				t.Fatalf("ParseHeaderXML: %v", err)
			}
			if len(h.Fields) != 2 {
				t.Fatalf("got %d fields, want 2", len(h.Fields))
			}
			if got := h.Fields[1].FieldName; got != name {
				t.Errorf("FieldName = %q, want %q", got, name)
			}
			// The other fields must come through untouched.
			if h.Fields[0].FieldName != "Id" || h.Fields[1].NoOfSymbols != 5 {
				t.Errorf("neighbouring values disturbed: %+v", h.Fields)
			}
		})
	}
}

func TestParseHeaderXMLLeavesValidHeadersAlone(t *testing.T) {
	h, err := ParseHeaderXML([]byte(sampleHeader))
	if err != nil {
		t.Fatalf("ParseHeaderXML: %v", err)
	}
	if h.Repaired {
		t.Error("a well-formed header was reported as repaired")
	}
}

func TestParseHeaderXMLMarksRepairedHeaders(t *testing.T) {
	h, err := ParseHeaderXML([]byte(badNameHeader("Ist <Soll (Abw.)")))
	if err != nil {
		t.Fatalf("ParseHeaderXML: %v", err)
	}
	if !h.Repaired {
		t.Error("a repaired header was not marked as one")
	}
}

func TestParseHeaderXMLReportsTheOffendingLine(t *testing.T) {
	// A header no escaping can rescue -- here one truncated mid-element --
	// has to say what it choked on, since the caller cannot open a QVD as text
	// to go and look at the line number itself.
	raw := sampleHeader[:strings.Index(sampleHeader, "<Fmt>#.##0,00</Fmt>")+10]
	_, err := ParseHeaderXML([]byte(raw))
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "line ") || !strings.Contains(err.Error(), "<Fmt>") {
		t.Errorf("error does not quote the offending line: %v", err)
	}
}

func TestHeaderLine(t *testing.T) {
	raw := []byte("a\nb\n  <FieldName>Ist <Soll</FieldName>\r\nd\n")
	if got, want := headerLine(raw, 3), `"<FieldName>Ist <Soll</FieldName>"`; got != want {
		t.Errorf("headerLine = %q, want %q", got, want)
	}
	if got := headerLine(raw, 99); got != "" {
		t.Errorf("out-of-range line = %q, want empty", got)
	}
	if got := headerLine([]byte(strings.Repeat("x", 400)), 1); !strings.Contains(got, "...") {
		t.Errorf("long line not truncated: %q", got)
	}
}

func TestHeaderDiagnostics(t *testing.T) {
	full := []byte("<QvdTableHeader>\n<x>\n</QvdTableHeader>")
	if got := headerDiagnostics(full, 2); !strings.Contains(got, "ends on line 3") ||
		!strings.Contains(got, "38 bytes, 3 lines") || !strings.Contains(got, `line 2: "<x>"`) {
		t.Errorf("headerDiagnostics = %q", got)
	}
	// The shape seen in the wild: the end tag never closes, and whitespace
	// padding runs on to the terminator.
	cut := []byte("<QvdTableHeader>\n<x>\n</QvdTableHeader\n\n\n")
	if got := headerDiagnostics(cut, 6); !strings.Contains(got, "end tag is incomplete") {
		t.Errorf("headerDiagnostics = %q", got)
	}
	if got := headerDiagnostics([]byte("<QvdTableHeader>\n"), 2); !strings.Contains(got, "no </QvdTableHeader> end tag") {
		t.Errorf("headerDiagnostics = %q", got)
	}
}

func TestCompleteRootElement(t *testing.T) {
	const body = "<QvdTableHeader>\n  <TableName>T</TableName>\n </QvdTableHeader>"
	cases := map[string]string{
		body:                        body,               // already exact
		body + "\n\n\n":             body,               // padding after the end tag
		body + "\x01\x02":           body,               // or anything else after it
		body[:len(body)-1]:          body,               // the missing '>'
		body[:len(body)-1] + "\n\n": body,               // missing '>', then padding
		"<QvdTableHeader>":          "<QvdTableHeader>", // no end tag to work with
	}
	for in, want := range cases {
		got, _ := completeRootElement([]byte(in))
		if string(got) != want {
			t.Errorf("completeRootElement(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseHeaderXMLCompletesTheRootElement(t *testing.T) {
	// A header whose root end tag is a byte short, padded out to where the
	// 0x00 terminator sits. Left alone the parser runs off the end of the
	// padding and reports an EOF a thousand lines past the last field.
	raw := strings.TrimSuffix(sampleHeader, ">") + strings.Repeat("\n", 1286)
	h, err := ParseHeaderXML([]byte(raw))
	if err != nil {
		t.Fatalf("ParseHeaderXML: %v", err)
	}
	if len(h.Fields) != 2 || !h.Repaired {
		t.Errorf("got %d fields, repaired=%v", len(h.Fields), h.Repaired)
	}
	if err := h.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestEscapeStrayMarkupKeepsRealMarkup(t *testing.T) {
	valid := []string{
		`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><a><b/></a>`,
		`<a x="1" y='2'><!-- c > d --><b/></a>`,
		`<a><![CDATA[ raw < text & more ]]></a>`,
		`<a>&amp; &lt; &#39; &#x27; &apos;</a>`,
		`<a
   x="1"
></a>`,
	}
	for _, s := range valid {
		got, _ := escapeStrayMarkup([]byte(s))
		if string(got) != s {
			t.Errorf("escapeStrayMarkup(%q) = %q, want it unchanged", s, got)
		}
	}
}

func TestEscapeStrayMarkupEscapesStrayBytes(t *testing.T) {
	cases := map[string]string{
		`<a>x < y</a>`:      `<a>x &lt; y</a>`,
		`<a>x <5 % y</a>`:   `<a>x &lt;5 % y</a>`,
		`<a>AT&T</a>`:       `<a>AT&amp;T</a>`,
		`<a>&; &# &#x;</a>`: `<a>&amp;; &amp;# &amp;#x;</a>`,
		`<a>x <b c>y</a>`:   `<a>x &lt;b c>y</a>`, // an attribute without a value is not markup
		`<a>1 <2</a>`:       `<a>1 &lt;2</a>`,
	}
	for in, want := range cases {
		got, _ := escapeStrayMarkup([]byte(in))
		if string(got) != want {
			t.Errorf("escapeStrayMarkup(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscapeStrayMarkupHandlesUTF8Names(t *testing.T) {
	// Field names in these files are German; the escape must not treat a
	// multi-byte character as a stray byte or split one.
	in := `<a>Zähler <Soll (Abw.) & Rückmeldung</a>`
	want := `<a>Zähler &lt;Soll (Abw.) &amp; Rückmeldung</a>`
	got, _ := escapeStrayMarkup([]byte(in))
	if string(got) != want {
		t.Errorf("escapeStrayMarkup = %q, want %q", got, want)
	}
}

func TestParseHeaderXMLDropsIllegalControlBytes(t *testing.T) {
	// A vertical tab inside a tag renders as nothing in an editor and does not
	// survive a copy and paste, so the line looks correct while the parser
	// reports "expected attribute name in element" on it. The field's values
	// have to come through intact once the byte is dropped.
	for _, bad := range []string{"\v", "\f", "\x01", "\x1f", "\x7f"} {
		raw := strings.Replace(sampleHeader, "<NoOfSymbols>5</NoOfSymbols>",
			"<NoOfSymbols"+bad+">5</NoOfSymbols>", 1)
		h, err := ParseHeaderXML([]byte(raw))
		if err != nil {
			t.Errorf("%q: ParseHeaderXML: %v", bad, err)
			continue
		}
		if !h.Repaired {
			t.Errorf("%q: not marked as repaired", bad)
		}
		if got := h.Fields[1].NoOfSymbols; got != 5 {
			t.Errorf("%q: NoOfSymbols = %d, want 5", bad, got)
		}
		if got := h.Fields[1].FieldName; got != "Amount" {
			t.Errorf("%q: FieldName = %q", bad, got)
		}
	}
}

func TestParseHeaderXMLDropsControlBytesInValues(t *testing.T) {
	raw := strings.Replace(sampleHeader, "<FieldName>Amount</FieldName>",
		"<FieldName>Amount\vNetto</FieldName>", 1)
	h, err := ParseHeaderXML([]byte(raw))
	if err != nil {
		t.Fatalf("ParseHeaderXML: %v", err)
	}
	// XML 1.0 has no escape for these bytes, so dropping is the only repair.
	if got := h.Fields[1].FieldName; got != "AmountNetto" {
		t.Errorf("FieldName = %q, want %q", got, "AmountNetto")
	}
}

func TestStripIllegalControls(t *testing.T) {
	if got, _ := stripIllegalControls([]byte("a\tb\nc\rd")); string(got) != "a\tb\nc\rd" {
		t.Errorf("tab, newline and carriage return must survive: %q", got)
	}
	if got, _ := stripIllegalControls([]byte("a\x00\x08b\v\fc\x0e\x1fd\x7f")); string(got) != "abcd" {
		t.Errorf("stripIllegalControls = %q, want %q", got, "abcd")
	}
	in := []byte("nothing to do")
	got, note := stripIllegalControls(in)
	if &got[0] != &in[0] || note != "" {
		t.Errorf("a clean header should be left alone, note=%q", note)
	}
}

func TestRepairNoteSaysWhatWasWrong(t *testing.T) {
	// The bytes at fault are usually invisible and a QVD cannot be opened as
	// text, so this note is the only account anyone gets of what was repaired.
	cases := []struct{ name, raw, want string }{
		{"control byte", strings.Replace(sampleHeader,
			"<NoOfSymbols>5</NoOfSymbols>", "<NoOfSymbols\x7f>5</NoOfSymbols>", 1),
			"0x7F on line 22"},
		{"unescaped markup", strings.Replace(sampleHeader,
			"<FieldName>Amount</FieldName>", "<FieldName>Ist <Soll & mehr</FieldName>", 1),
			`escaped 1 unescaped "<", first on line 17`},
		{"unclosed root", strings.TrimSuffix(sampleHeader, ">") + strings.Repeat("\n", 1286),
			"closed the root end tag on line 31"},
	}
	for _, c := range cases {
		h, err := ParseHeaderXML([]byte(c.raw))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !strings.Contains(h.RepairNote, c.want) {
			t.Errorf("%s: note = %q, want it to mention %q", c.name, h.RepairNote, c.want)
		}
	}
}
