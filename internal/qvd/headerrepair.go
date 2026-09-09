package qvd

import (
	"bytes"
	"fmt"
	"strings"
)

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

// headerLineContext renders the 1-based source line the XML parser stopped on,
// so that a header this package cannot repair names the text that broke it
// instead of only a line number in a file the caller cannot open as text.
func headerLineContext(raw []byte, line int) string {
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
	return fmt.Sprintf(" (line %d: %s)", line, text)
}
