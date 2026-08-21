package convert

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// DualKind classifies what a column's dual display strings actually carry.
type DualKind int

const (
	// DualNone means the column has no dual symbols.
	DualNone DualKind = iota
	// DualFormatting means every display string is a rendering of the numeric
	// value: a localized number, or the formatted form of a date/time serial.
	// The text adds no information, so writing it would only duplicate the
	// numeric column.
	DualFormatting
	// DualInformative means at least one display string carries something the
	// number does not, such as "one" beside 1 or a status label beside a code.
	// Dropping it would lose data.
	DualInformative
)

func (k DualKind) String() string {
	return [...]string{"none", "formatting", "informative"}[k]
}

// DualClassification is the result of inspecting a column's dual symbols.
type DualClassification struct {
	Kind DualKind
	// Duals is how many symbols carry both a number and a display string.
	Duals int64
	// Informative counts the display strings that are not a rendering of the
	// numeric value.
	Informative int64
	// Example is one such display string, for the explanatory note.
	Example string
	// ExampleNumber is the numeric value that Example sat beside.
	ExampleNumber float64
}

// ClassifyDual decides whether a column's display strings carry information
// beyond what the resolved numeric column already encodes.
//
// A display string counts as pure formatting when it parses back to the same
// number under the field's declared separators, or when it renders the Qlik
// serial value as a date and the column is actually written as a date/time
// type.
//
// The strategy matters: a serial such as 40502 shown as "11/20/2010" is
// redundant beside a date32 column, but not beside a float64 one, where the
// reader has no way to know the number is a date at all.
func ClassifyDual(col qvd.Column, syms []qvd.Symbol, strategy ValueStrategy, loc *time.Location) DualClassification {
	var c DualClassification
	dateTyped := strategy == StrategyDate32 || strategy == StrategyTimestampMillis ||
		strategy == StrategyTimeMillis
	for _, s := range syms {
		if s.Kind != qvd.SymbolDualIntString && s.Kind != qvd.SymbolDualFloatString {
			continue
		}
		c.Duals++
		n, _ := s.Numeric()
		if textRendersNumber(s.Text, n, col.DecSep, col.ThouSep) ||
			(dateTyped && textRendersDate(s.Text, n, loc)) {
			continue
		}
		c.Informative++
		if c.Example == "" {
			c.Example, c.ExampleNumber = s.Text, n
		}
	}
	switch {
	case c.Duals == 0:
		c.Kind = DualNone
	case c.Informative > 0:
		c.Kind = DualInformative
	default:
		c.Kind = DualFormatting
	}
	return c
}

// textRendersNumber reports whether text is merely a rendering of n.
func textRendersNumber(text string, n float64, decSep, thouSep string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		// An empty display string carries nothing to preserve.
		return true
	}
	v, ok := parseLocalizedNumber(t, decSep, thouSep)
	if !ok {
		return false
	}
	// Compare with a tolerance that absorbs the rounding a display format
	// applies, e.g. "1.234,56" shown for 1234.5600000001.
	if n == v {
		return true
	}
	diff := math.Abs(n - v)
	scale := math.Max(math.Max(math.Abs(n), math.Abs(v)), 1)
	return diff <= 1e-9*scale
}

// dateTokens matches runs of digits, which are the parts of a rendered date.
var dateTokens = regexp.MustCompile(`\d+`)

// dateLeftovers is what may remain once digits and date punctuation are
// removed: a month abbreviation, an AM/PM marker, or a timezone suffix.
var dateLeftovers = regexp.MustCompile(`^[\p{L}]{0,5}$`)

// textRendersDate reports whether text is a rendering of the Qlik serial value
// n as a date or time, without assuming any particular format.
//
// Rather than trying to parse arbitrary layouts, it converts the serial to a
// date and checks that every number appearing in the text is one of that
// date's components. "11/20/2010", "20.11.2010" and "2010-11-20 00:00" all
// satisfy this for serial 40502, while "Order 11 of 2010" does not, because
// the leftover words are rejected.
func textRendersDate(text string, n float64, loc *time.Location) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}
	if !dateTextRemainderOK(t) {
		return false
	}
	ms, ok := qvd.QlikDaysToTimestampMillis(n, loc)
	if !ok {
		return false
	}
	ts := time.UnixMilli(ms).In(loc)

	nums := dateTokens.FindAllString(t, -1)
	if len(nums) < 2 {
		// A single number is not enough evidence that this is a date.
		return false
	}
	tokens := make([]int, 0, len(nums))
	for _, tok := range nums {
		v, err := strconv.Atoi(tok)
		if err != nil {
			return false
		}
		tokens = append(tokens, v)
	}

	// The display string was rendered by whoever wrote the QVD, in a timezone
	// this converter does not know. A whole-hour offset shifts the clock and
	// can move the calendar date by a day, so try the neighbouring dates too.
	// Real data needs this: the serial 40377.958333 is 2010-07-18 23:00 UTC
	// but was written out as "07/19/2010" by a UTC+1 producer.
	for _, d := range []time.Time{ts, ts.AddDate(0, 0, -1), ts.AddDate(0, 0, 1)} {
		if tokensMatchDate(tokens, d, ts) {
			return true
		}
	}
	return false
}

// tokensMatchDate reports whether every number in a display string is a
// component of the date d, requiring the day itself to appear so that a
// different date cannot slip through by matching only the month and year.
//
// The hour is left free because a timezone offset shifts it, but the minutes
// and seconds must still line up, which keeps the check strong.
func tokensMatchDate(tokens []int, d, ts time.Time) bool {
	allowed := map[int]bool{
		d.Day(): true, int(d.Month()): true,
		d.Year(): true, d.Year() % 100: true,
		ts.Minute(): true, ts.Second(): true, ts.Nanosecond() / 1e6: true,
	}
	for h := 0; h <= 24; h++ {
		allowed[h] = true // any whole-hour timezone offset, and 24:00 for midnight
	}
	sawDay := false
	for _, v := range tokens {
		if !allowed[v] {
			return false
		}
		if v == d.Day() {
			sawDay = true
		}
	}
	return sawDay
}

// dateTextRemainderOK reports whether what is left after removing the digits
// is plausible date punctuation, optionally with a short word such as a month
// abbreviation or "AM". This is what rejects "Order 11 of 2010".
func dateTextRemainderOK(t string) bool {
	rest := strings.TrimSpace(dateTokens.ReplaceAllString(t, " "))
	for _, sep := range []string{"/", ".", "-", ":", ",", " ", "'"} {
		rest = strings.ReplaceAll(rest, sep, "")
	}
	return dateLeftovers.MatchString(rest)
}

// parseLocalizedNumber parses a display string using the field's declared
// separators, tolerating a leading sign, grouping, and a trailing percent or
// currency-free suffix-less form.
func parseLocalizedNumber(s, decSep, thouSep string) (float64, bool) {
	if decSep == "" {
		decSep = "."
	}
	if thouSep != "" && thouSep != decSep {
		s = strings.ReplaceAll(s, thouSep, "")
	}
	// Qlik commonly groups with a non-breaking or thin space.
	for _, sp := range []string{" ", " ", " ", "'"} {
		s = strings.ReplaceAll(s, sp, "")
	}
	neg := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg, s = true, s[1:len(s)-1]
	}
	if decSep != "." {
		// Reject a string that also uses '.' as a decimal point, which would
		// mean the declared separators do not describe it.
		if strings.Contains(s, ".") {
			return 0, false
		}
		s = strings.Replace(s, decSep, ".", 1)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		v = -v
	}
	return v, true
}

// Note renders the classification for the schema log.
func (c DualClassification) Note(colName, textColName string) string {
	switch c.Kind {
	case DualInformative:
		return fmt.Sprintf(
			"%d of %d display strings carry text the number does not (e.g. %q beside %s), so they are kept in %q",
			c.Informative, c.Duals, c.Example, formatFloat(c.ExampleNumber), textColName)
	case DualFormatting:
		return fmt.Sprintf(
			"all %d display strings are formatted renderings of the numeric value, so no text column is written",
			c.Duals)
	}
	return ""
}

// minDateSerial and maxDateSerial bound the Qlik serial values that may be
// inferred as dates: roughly the years 1900 to 2200. Without a bound, small
// integers such as a quantity of 5 could be read as dates.
const (
	minDateSerial = 1.0      // 1900-01-01
	maxDateSerial = 109575.0 // 2200-01-01
)

// DateInference is the result of looking for date semantics in a column whose
// declared type does not provide any.
type DateInference struct {
	Type qvd.QlikType
	// Checked is how many dual symbols were examined.
	Checked int64
	// Example is one display string that established the reading.
	Example string
}

// InferDateTimeFromDuals detects a date, timestamp or time column whose header
// declares no type, by checking whether every display string renders its
// numeric value as a date under Qlik's serial-day convention.
//
// QVDs written by tools other than QlikView routinely leave NumberFormat/Type
// empty, which would otherwise leave a date column as a bare float64 serial
// that no downstream reader can interpret. Every dual must agree, so a column
// mixing dates with anything else is not inferred.
func InferDateTimeFromDuals(col qvd.Column, syms []qvd.Symbol, loc *time.Location) (DateInference, bool) {
	var inf DateInference
	hasFraction, hasClock := false, false

	for _, s := range syms {
		if s.Kind == qvd.SymbolNull {
			continue
		}
		// Every value-bearing symbol must be a dual: a bare number carries no
		// evidence, so inferring from a partial column would be a guess.
		if s.Kind != qvd.SymbolDualIntString && s.Kind != qvd.SymbolDualFloatString {
			return DateInference{}, false
		}
		n, _ := s.Numeric()
		if n < minDateSerial || n > maxDateSerial {
			return DateInference{}, false
		}
		if !textRendersDate(s.Text, n, loc) {
			return DateInference{}, false
		}
		inf.Checked++
		if inf.Example == "" {
			inf.Example = s.Text
		}
		if _, frac := math.Modf(n); frac != 0 {
			hasFraction = true
		}
		if strings.Contains(s.Text, ":") {
			hasClock = true
		}
	}
	if inf.Checked == 0 {
		return DateInference{}, false
	}

	switch {
	case hasFraction || hasClock:
		inf.Type = qvd.QlikTimestamp
	default:
		inf.Type = qvd.QlikDate
	}
	return inf, true
}

// Note renders the inference for the schema log.
func (d DateInference) Note() string {
	return fmt.Sprintf("no declared type, but all %d display strings render their value as a %s (e.g. %q), so it is read as one",
		d.Checked, strings.ToLower(d.Type.String()), d.Example)
}
