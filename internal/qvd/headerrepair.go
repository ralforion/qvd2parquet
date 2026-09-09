package qvd

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// stripIllegalControls removes the invisible bytes that cannot occur in a tag:
// the C0 controls XML 1.0 forbids anywhere in a document, not even escaped
// (0x00-0x08, 0x0B, 0x0C, 0x0E-0x1F), and 0x7F, which XML allows in text but
// not in a name.
//
// Every one of them renders as nothing in an editor and none survive a copy
// and paste, so a header holding one looks perfectly correct to anyone reading
// it while the parser stops on a line that says `<NoOfSymbols>1</NoOfSymbols>`
// and reports "expected attribute name in element". Dropping them is the only
// repair available, since XML 1.0 has no escape for the C0 set either.
func stripIllegalControls(raw []byte) []byte {
	if bytes.IndexFunc(raw, isIllegalControl) < 0 {
		return raw
	}
	out := make([]byte, 0, len(raw))
	for _, c := range raw {
		if !isIllegalControl(rune(c)) {
			out = append(out, c)
		}
	}
	return out
}

func isIllegalControl(r rune) bool {
	return r == 0x7F || (r < 0x20 && r != '\t' && r != '\n' && r != '\r')
}

// escapeStrayMarkup escapes the '<' and '&' bytes in a QVD header that cannot
// be starting markup, so that a header Qlik wrote without escaping its own
// string values parses as XML.
//
// Qlik copies field names, comments and number formats into the header
// verbatim. A SAP-derived name such as `Ist <Soll (Abw.)` or a comment
// containing `R&D` is then not well-formed XML, and the whole file is
// unreadable over one character in one name. Every such byte is escaped here
// except where it genuinely opens a tag, a declaration, a comment, a CDATA
// section or an entity reference, which leaves valid headers untouched.
//
// The one case this cannot recover is a value that looks exactly like a tag,
// `Menge <Soll>` say: nothing in the bytes distinguishes that from markup, so
// it stays markup, as it already was.
func escapeStrayMarkup(raw []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(raw) + len(raw)/16)
	for i := 0; i < len(raw); {
		switch raw[i] {
		case '<':
			if n := markupLen(raw[i:]); n > 0 {
				out.Write(raw[i : i+n])
				i += n
				continue
			}
			out.WriteString("&lt;")
			i++
		case '&':
			if n := entityLen(raw[i:]); n > 0 {
				out.Write(raw[i : i+n])
				i += n
				continue
			}
			out.WriteString("&amp;")
			i++
		default:
			out.WriteByte(raw[i])
			i++
		}
	}
	return out.Bytes()
}

// rootEndTag is the last thing a QVD header should contain.
const rootEndTag = "</QvdTableHeader"

// completeRootElement cuts the header back to the end of its root element,
// supplying the closing '>' when the bytes do not have one.
//
// The header is terminated by a 0x00 byte, not by its own last character, and
// what sits between the two is up to the writer: observed QVDs pad the gap
// with whitespace, and at least one leaves the root end tag itself a byte
// short, as `</QvdTableHeader`. Either way the XML the file states is
// everything up to and including that tag, so that is what gets parsed.
func completeRootElement(raw []byte) []byte {
	// The first occurrence is the real one: anything repeating it later is
	// trailing content, which is exactly what this trims.
	i := bytes.Index(raw, []byte(rootEndTag))
	if i < 0 {
		return raw
	}
	j := i + len(rootEndTag)
	for j < len(raw) && isSpaceByte(raw[j]) {
		j++
	}
	if j < len(raw) && raw[j] == '>' {
		return raw[:j+1] // complete tag; drop whatever follows it
	}
	out := make([]byte, 0, i+len(rootEndTag)+1)
	out = append(out, raw[:i+len(rootEndTag)]...)
	return append(out, '>')
}

// markupLen reports the length of the well-formed markup starting at b[0]=='<',
// or 0 if what follows is not markup at all.
func markupLen(b []byte) int {
	if len(b) < 2 {
		return 0
	}
	switch b[1] {
	case '?': // processing instruction, including the XML declaration
		return delimitedLen(b, "?>")
	case '!':
		switch {
		case bytes.HasPrefix(b, []byte("<!--")):
			return delimitedLen(b, "-->")
		case bytes.HasPrefix(b, []byte("<![CDATA[")):
			return delimitedLen(b, "]]>")
		default: // <!DOCTYPE ...>, which a QVD header has no reason to carry
			return delimitedLen(b, ">")
		}
	}
	return tagLen(b)
}

// delimitedLen reports the length of b up to and including the first end, or 0
// if end never comes.
func delimitedLen(b []byte, end string) int {
	if j := bytes.Index(b, []byte(end)); j >= 0 {
		return j + len(end)
	}
	return 0
}

// tagLen reports the length of the start or end tag at b[0]=='<', or 0 if the
// bytes are not one. Attributes are accepted even though QVD headers do not
// use them, so that a header carrying one is not mangled into text.
func tagLen(b []byte) int {
	i := 1
	closing := false
	if i < len(b) && b[i] == '/' {
		closing = true
		i++
	}
	n := nameLen(b[i:])
	if n == 0 {
		return 0
	}
	i += n
	for i < len(b) {
		spaced := false
		for i < len(b) && isSpaceByte(b[i]) {
			spaced = true
			i++
		}
		if i >= len(b) {
			return 0
		}
		switch {
		case b[i] == '>':
			return i + 1
		case b[i] == '/' && !closing:
			if i+1 < len(b) && b[i+1] == '>' {
				return i + 2
			}
			return 0
		case closing || !spaced:
			// An end tag holds nothing but its name, and an attribute must be
			// separated from what precedes it by whitespace.
			return 0
		}
		n := attrLen(b[i:])
		if n == 0 {
			return 0
		}
		i += n
	}
	return 0
}

// attrLen reports the length of one name="value" attribute at b[0], or 0.
func attrLen(b []byte) int {
	i := nameLen(b)
	if i == 0 {
		return 0
	}
	for i < len(b) && isSpaceByte(b[i]) {
		i++
	}
	if i >= len(b) || b[i] != '=' {
		return 0
	}
	i++
	for i < len(b) && isSpaceByte(b[i]) {
		i++
	}
	if i >= len(b) || (b[i] != '"' && b[i] != '\'') {
		return 0
	}
	quote := b[i]
	i++
	for ; i < len(b); i++ {
		switch b[i] {
		case quote:
			return i + 1
		case '<': // never legal inside an attribute value
			return 0
		}
	}
	return 0
}

// entityLen reports the length of the entity reference at b[0]=='&', or 0 if
// the ampersand is a literal one.
func entityLen(b []byte) int {
	i := 1
	if i < len(b) && b[i] == '#' {
		i++
		digits := isDigitByte
		if i < len(b) && (b[i] == 'x' || b[i] == 'X') {
			i++
			digits = isHexByte
		}
		start := i
		for i < len(b) && digits(b[i]) {
			i++
		}
		if i == start {
			return 0
		}
	} else {
		n := nameLen(b[i:])
		if n == 0 {
			return 0
		}
		i += n
	}
	if i < len(b) && b[i] == ';' {
		return i + 1
	}
	return 0
}

// nameLen reports the length of the XML name at b[0], or 0 if there is none.
func nameLen(b []byte) int {
	if len(b) == 0 || !isNameStartByte(b[0]) {
		return 0
	}
	i := 1
	for i < len(b) && isNameByte(b[i]) {
		i++
	}
	return i
}

func isNameStartByte(c byte) bool {
	return c == '_' || c == ':' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		c >= 0x80 // a lead byte of a multi-byte UTF-8 name character
}

func isNameByte(c byte) bool {
	return isNameStartByte(c) || c == '-' || c == '.' || (c >= '0' && c <= '9')
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func isDigitByte(c byte) bool {
	return c >= '0' && c <= '9'
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// headerDiagnostics describes a header this package could not parse: how much
// of it there was, whether it ends where a header should, and the source line
// the parser stopped on. A QVD cannot be opened as text to go and look, so an
// error naming only a line number leaves the caller with nowhere to go.
func headerDiagnostics(raw []byte, line int) string {
	lines := bytes.Count(raw, []byte("\n")) + 1
	var root string
	switch i := bytes.Index(raw, []byte(rootEndTag)); {
	case i < 0:
		root = "no " + rootEndTag + "> end tag"
	case bytes.Contains(raw[i:], []byte(rootEndTag+">")):
		root = fmt.Sprintf("%s> ends on line %d", rootEndTag,
			bytes.Count(raw[:i], []byte("\n"))+1)
	default:
		root = rootEndTag + "> end tag is incomplete"
	}
	out := fmt.Sprintf(" (header %d bytes, %d lines; %s", len(raw), lines, root)
	if text := headerLine(raw, line); text != "" {
		out += fmt.Sprintf("; line %d: %s", line, text)
	}
	return out + ")"
}

// headerLine returns the 1-based source line, quoted so that a control byte or
// a stray piece of invalid UTF-8 is visible rather than invisible, which is
// the whole point of printing it.
func headerLine(raw []byte, line int) string {
	if line <= 0 {
		return ""
	}
	s := raw
	for n := 1; n < line; n++ {
		j := bytes.IndexByte(s, '\n')
		if j < 0 {
			return ""
		}
		s = s[j+1:]
	}
	if j := bytes.IndexByte(s, '\n'); j >= 0 {
		s = s[:j]
	}
	text := strings.TrimSpace(string(bytes.TrimRight(s, "\r")))
	if text == "" {
		return ""
	}
	const max = 160
	if len(text) > max {
		text = text[:max] + "..."
	}
	return strconv.Quote(text)
}
