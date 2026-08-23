// Package qvd implements parsing of Qlik QVD files: XML header, symbol
// tables and bit-stuffed fixed-width records. It has no Parquet or Arrow
// dependency.
package qvd

import (
	"fmt"
	"strings"
)

// QlikType is the declared NumberFormat/Type of a QVD field.
type QlikType int

const (
	QlikUnknown QlikType = iota
	QlikASCII
	QlikDate
	QlikTimestamp
	QlikTime
	QlikInteger
	QlikReal
	QlikFix
	QlikMoney
)

func (t QlikType) String() string {
	switch t {
	case QlikASCII:
		return "ASCII"
	case QlikDate:
		return "DATE"
	case QlikTimestamp:
		return "TIMESTAMP"
	case QlikTime:
		return "TIME"
	case QlikInteger:
		return "INTEGER"
	case QlikReal:
		return "REAL"
	case QlikFix:
		return "FIX"
	case QlikMoney:
		return "MONEY"
	default:
		return "UNKNOWN"
	}
}

// IsDateTime reports whether the type carries Qlik serial date/time semantics.
func (t QlikType) IsDateTime() bool {
	return t == QlikDate || t == QlikTimestamp || t == QlikTime
}

// IsDecimal reports whether the type must be preserved as an exact decimal.
func (t QlikType) IsDecimal() bool {
	return t == QlikFix || t == QlikMoney
}

// ParseQlikType maps the header's NumberFormat/Type string onto QlikType.
func ParseQlikType(s string) QlikType {
	switch s {
	case "ASCII":
		return QlikASCII
	case "DATE":
		return QlikDate
	case "TIMESTAMP":
		return QlikTimestamp
	case "TIME":
		return QlikTime
	case "INTEGER":
		return QlikInteger
	case "REAL":
		return QlikReal
	case "FIX":
		return QlikFix
	case "MONEY":
		return QlikMoney
	default:
		return QlikUnknown
	}
}

// Column is the normalized per-field metadata used by the decoder.
type Column struct {
	Index       int
	Name        string
	QlikType    QlikType
	BitOffset   int
	BitWidth    int
	Bias        int64
	SymbolCount int64
	Offset      int64
	Length      int64
	Selected    bool

	// NDec is NumberFormat/nDec, the declared number of decimals. It is only
	// meaningful for the decimal-like types.
	NDec int
	// DecSep is the declared decimal separator from NumberFormat/Dec.
	DecSep string
	// ThouSep is the declared thousands separator from NumberFormat/Thou.
	ThouSep string
	// Fmt is the declared display format string.
	Fmt string
	// Tags holds the field's Qlik semantic tags, lower-cased: $numeric,
	// $integer, $text, $date, $timestamp and so on. Qlik Sense and newer
	// QlikView versions populate these even when NumberFormat/Type is left
	// empty, so they are often the only reliable statement of a field's
	// meaning.
	Tags []string
}

// HasTag reports whether the column carries the given Qlik tag, which may be
// written with or without the leading "$".
func (c Column) HasTag(tag string) bool {
	tag = strings.ToLower(strings.TrimPrefix(tag, "$"))
	for _, t := range c.Tags {
		if strings.TrimPrefix(t, "$") == tag {
			return true
		}
	}
	return false
}

// TaggedType maps Qlik's date/time tags onto a QlikType, which is how a field
// declaring no NumberFormat/Type can still state that it holds dates. It
// reports false when the column carries no such tag.
func (c Column) TaggedType() (QlikType, bool) {
	switch {
	case c.HasTag("timestamp"):
		return QlikTimestamp, true
	case c.HasTag("date"):
		return QlikDate, true
	case c.HasTag("time"), c.HasTag("interval"):
		return QlikTime, true
	}
	return QlikUnknown, false
}

// SymbolKind describes which sides of a decoded symbol carry a value.
type SymbolKind uint8

const (
	SymbolNull SymbolKind = iota
	SymbolString
	SymbolInt
	SymbolFloat
	SymbolDualIntString
	SymbolDualFloatString
)

func (k SymbolKind) String() string {
	switch k {
	case SymbolString:
		return "string"
	case SymbolInt:
		return "int"
	case SymbolFloat:
		return "float"
	case SymbolDualIntString:
		return "dualInt"
	case SymbolDualFloatString:
		return "dualFloat"
	default:
		return "null"
	}
}

// HasNumeric reports whether the symbol carries a numeric payload.
func (k SymbolKind) HasNumeric() bool {
	return k == SymbolInt || k == SymbolFloat || k == SymbolDualIntString || k == SymbolDualFloatString
}

// HasText reports whether the symbol carries a display string.
func (k SymbolKind) HasText() bool {
	return k == SymbolString || k == SymbolDualIntString || k == SymbolDualFloatString
}

// Symbol is one entry of a QVD symbol table. Dual values keep both sides.
type Symbol struct {
	Kind  SymbolKind
	Int   int64
	Float float64
	Text  string
}

// Numeric returns the symbol's numeric value as a float64 and whether one exists.
func (s Symbol) Numeric() (float64, bool) {
	switch s.Kind {
	case SymbolInt, SymbolDualIntString:
		return float64(s.Int), true
	case SymbolFloat, SymbolDualFloatString:
		return s.Float, true
	}
	return 0, false
}

// ColumnProfile counts the symbol kinds actually present in a column and is the
// single place where mixed-type detection happens.
type ColumnProfile struct {
	Symbols    int64 `json:"symbols"`
	Nulls      int64 `json:"nulls"`
	Strings    int64 `json:"strings"`
	Ints       int64 `json:"ints"`
	Floats     int64 `json:"floats"`
	DualInts   int64 `json:"dualInts"`
	DualFloats int64 `json:"dualFloats"`
	EmptyText  int64 `json:"emptyText"`
	// EmptyStrings counts symbols that are nothing but an empty string. Unlike
	// EmptyText it excludes duals, whose numeric side is still a value.
	EmptyStrings int64   `json:"emptyStrings"`
	MaxTextLen   int     `json:"maxTextLen"`
	MinInt       int64   `json:"minInt"`
	MaxInt       int64   `json:"maxInt"`
	MinFloat     float64 `json:"minFloat"`
	MaxFloat     float64 `json:"maxFloat"`

	hasInt   bool
	hasFloat bool
}

// TextIsLosslessForMixed reports whether a column mixing text and numbers can
// be written as text without inventing a rendering for any value: every symbol
// carries its own display string, and the numeric side is integer.
//
// Decimals are deliberately excluded. An integer stored beside its own text is
// an identifier rather than a quantity -- "0901" is not the number 901, and
// reading it as one destroys it -- whereas a decimal mixed with text is more
// likely a measurement whose display string is a formatting of it. Flattening
// that to text is a judgement for the caller, so it still stops.
func (p *ColumnProfile) TextIsLosslessForMixed() bool {
	// A dual whose display string is empty is the exception: writing the text
	// side drops its number, and under the default policy the empty string
	// then becomes null, so the value disappears entirely. EmptyText counts
	// every empty display string and EmptyStrings only the bare ones, so their
	// difference is the number of duals with nothing to write.
	if p.EmptyText != p.EmptyStrings {
		return false
	}
	return p.Ints == 0 && p.Floats == 0 && p.DualFloats == 0 &&
		p.DualInts > 0 && p.Strings > 0
}

// Observe folds one symbol into the profile.
func (p *ColumnProfile) Observe(s Symbol) {
	p.Symbols++
	switch s.Kind {
	case SymbolNull:
		p.Nulls++
	case SymbolString:
		p.Strings++
	case SymbolInt:
		p.Ints++
	case SymbolFloat:
		p.Floats++
	case SymbolDualIntString:
		p.DualInts++
	case SymbolDualFloatString:
		p.DualFloats++
	}
	if s.Kind.HasText() {
		if s.Text == "" {
			p.EmptyText++
			if s.Kind == SymbolString {
				p.EmptyStrings++
			}
		}
		if len(s.Text) > p.MaxTextLen {
			p.MaxTextLen = len(s.Text)
		}
	}
	switch s.Kind {
	case SymbolInt, SymbolDualIntString:
		if !p.hasInt {
			p.MinInt, p.MaxInt, p.hasInt = s.Int, s.Int, true
		} else {
			if s.Int < p.MinInt {
				p.MinInt = s.Int
			}
			if s.Int > p.MaxInt {
				p.MaxInt = s.Int
			}
		}
	case SymbolFloat, SymbolDualFloatString:
		if !p.hasFloat {
			p.MinFloat, p.MaxFloat, p.hasFloat = s.Float, s.Float, true
		} else {
			if s.Float < p.MinFloat {
				p.MinFloat = s.Float
			}
			if s.Float > p.MaxFloat {
				p.MaxFloat = s.Float
			}
		}
	}
}

// WithEmptyStringsAsNulls returns a copy of the profile in which a symbol that
// is nothing but an empty string is counted as a null instead of as text.
//
// Qlik treats an empty string as absent, so with that reading a numeric column
// holding empty placeholders is not a mixed-type column. Type resolution, the
// inspect preview and the conversion all have to agree on this, so they share
// one adjusted profile rather than each applying the rule separately.
func (p *ColumnProfile) WithEmptyStringsAsNulls() *ColumnProfile {
	if p == nil || p.EmptyStrings == 0 {
		return p
	}
	q := *p
	q.Strings -= q.EmptyStrings
	q.Nulls += q.EmptyStrings
	return &q
}

// TextOnly counts symbols that carry only a display string.
func (p *ColumnProfile) TextOnly() int64 { return p.Strings }

// IntLike counts symbols whose numeric side is an integer.
func (p *ColumnProfile) IntLike() int64 { return p.Ints + p.DualInts }

// FloatLike counts symbols whose numeric side is a double.
func (p *ColumnProfile) FloatLike() int64 { return p.Floats + p.DualFloats }

// Numeric counts symbols carrying any numeric payload.
func (p *ColumnProfile) Numeric() int64 { return p.IntLike() + p.FloatLike() }

// HasOnlyNulls reports whether the column has no value-bearing symbol.
func (p *ColumnProfile) HasOnlyNulls() bool { return p.Symbols == 0 || p.Nulls == p.Symbols }

// HasOnlyText reports whether every non-null symbol is a plain string.
func (p *ColumnProfile) HasOnlyText() bool { return p.Strings > 0 && p.Numeric() == 0 }

// HasOnlyInts reports whether every non-null symbol has an integer numeric side.
func (p *ColumnProfile) HasOnlyInts() bool {
	return p.IntLike() > 0 && p.FloatLike() == 0 && p.Strings == 0
}

// HasOnlyFloats reports whether every non-null symbol has a double numeric side.
func (p *ColumnProfile) HasOnlyFloats() bool {
	return p.FloatLike() > 0 && p.IntLike() == 0 && p.Strings == 0
}

// HasOnlyNumeric reports whether no plain-string symbol is present.
func (p *ColumnProfile) HasOnlyNumeric() bool { return p.Numeric() > 0 && p.Strings == 0 }

// HasDuals reports whether any symbol carries both a numeric and a text side.
func (p *ColumnProfile) HasDuals() bool { return p.DualInts+p.DualFloats > 0 }

// HasMixedScalarFamilies reports whether plain strings coexist with numerics.
func (p *ColumnProfile) HasMixedScalarFamilies() bool { return p.Strings > 0 && p.Numeric() > 0 }

// CanPromoteIntToFloat reports whether the column mixes integer and double
// numeric payloads and could be widened to float64.
func (p *ColumnProfile) CanPromoteIntToFloat() bool {
	return p.IntLike() > 0 && p.FloatLike() > 0 && p.Strings == 0
}

// CanUseQlikDeclaredType reports whether the declared type is consistent with
// the symbols actually found.
func (p *ColumnProfile) CanUseQlikDeclaredType(t QlikType) bool {
	switch t {
	case QlikASCII:
		return p.Numeric() == 0
	case QlikDate, QlikTimestamp, QlikTime, QlikInteger, QlikReal, QlikFix, QlikMoney:
		return p.Strings == 0
	}
	return false
}

// Describe renders a short human-readable summary used in error messages.
func (p *ColumnProfile) Describe() string {
	return fmt.Sprintf("%d symbols (%d null, %d int, %d float, %d string, %d dualInt, %d dualFloat)",
		p.Symbols, p.Nulls, p.Ints, p.Floats, p.Strings, p.DualInts, p.DualFloats)
}
