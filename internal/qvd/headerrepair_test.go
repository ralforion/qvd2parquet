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

func TestHeaderLineContext(t *testing.T) {
	raw := []byte("a\nb\n  <FieldName>Ist <Soll</FieldName>\r\nd\n")
	got := headerLineContext(raw, 3)
	if want := " (line 3: <FieldName>Ist <Soll</FieldName>)"; got != want {
		t.Errorf("headerLineContext = %q, want %q", got, want)
	}
	if got := headerLineContext(raw, 99); got != "" {
		t.Errorf("out-of-range line = %q, want empty", got)
	}
	long := []byte(strings.Repeat("x", 400))
	if got := headerLineContext(long, 1); !strings.HasSuffix(got, "...)") {
		t.Errorf("long line not truncated: %q", got)
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
		if got := string(escapeStrayMarkup([]byte(s))); got != s {
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
		if got := string(escapeStrayMarkup([]byte(in))); got != want {
			t.Errorf("escapeStrayMarkup(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscapeStrayMarkupHandlesUTF8Names(t *testing.T) {
	// Field names in these files are German; the escape must not treat a
	// multi-byte character as a stray byte or split one.
	in := `<a>Zähler <Soll (Abw.) & Rückmeldung</a>`
	want := `<a>Zähler &lt;Soll (Abw.) &amp; Rückmeldung</a>`
	if got := string(escapeStrayMarkup([]byte(in))); got != want {
		t.Errorf("escapeStrayMarkup = %q, want %q", got, want)
	}
}
