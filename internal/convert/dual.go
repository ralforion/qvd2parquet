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
func ClassifyDual(col qvd.Column, syms []qvd.Symbol, rc *ResolvedColumn, loc *time.Location) DualClassification {
	var c DualClassification
	strategy := rc.Strategy
	for i, s := range syms {
		if s.Kind != qvd.SymbolDualIntString && s.Kind != qvd.SymbolDualFloatString {
			continue
		}
		c.Duals++
		n, _ := s.Numeric()

		redundant := false
		switch strategy {
		case StrategyDecimal:
			// The decimal column may be written from the display string itself,
			// or from a rounded payload, so compare against what will actually
			// be written rather than the raw double.
			redundant = textRendersDecimal(s.Text, i, rc)
		case StrategyDate32, StrategyTimestampMicros:
			redundant = textRendersDate(s.Text, n, loc)
		case StrategyTimeMillis:
			redundant = textRendersTime(s.Text, n)
		default:
			redundant = textRendersNumber(s.Text, n, col.DecSep, col.ThouSep)
		}
		// A display string that is simply the number is always redundant.
		if !redundant {
			redundant = textRendersNumber(s.Text, n, col.DecSep, col.ThouSep)
		}
		if redundant {
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

// dateWords are the alphabetic tokens that may legitimately appear inside a
// rendered date: month and weekday names, meridiem markers, ordinal suffixes
// and timezone abbreviations. Anything else means the string says more than
// the date does.
//
// The list errs on the side of being short. A word wrongly rejected only costs
// a redundant text column, whereas a word wrongly accepted drops text that
// carried information -- "Due 11/20/2010" must not read as pure formatting.
var dateWords = map[string]bool{}

// monthWords maps a recognized month name onto its number, and weekdayWords
// onto its weekday, so a name that contradicts the value is rejected rather
// than merely tolerated. Allowing a word without checking it would let
// "Mon, 20 Nov 2010" pass for a Saturday.
var monthWords = map[string]int{}
var weekdayWords = map[string]time.Weekday{}

// timeWords are the only alphabetic tokens allowed beside a clock time. A TIME
// value carries no calendar date, so month and weekday names are not part of
// rendering one.
var timeWords = map[string]bool{}

func init() {
	// English and German month names, long and abbreviated. German QVDs are
	// the common case for this converter.
	months := [][]string{
		{"jan", "january", "januar", "jän", "jaen", "jänner"},
		{"feb", "february", "februar"},
		{"mar", "march", "mär", "maer", "märz", "maerz", "mrz"},
		{"apr", "april"},
		{"may", "mai"},
		{"jun", "june", "juni"},
		{"jul", "july", "juli"},
		{"aug", "august"},
		{"sep", "sept", "september"},
		{"oct", "october", "okt", "oktober"},
		{"nov", "november"},
		{"dec", "december", "dez", "dezember"},
	}
	for i, names := range months {
		for _, n := range names {
			monthWords[n] = i + 1
			dateWords[n] = true
		}
	}
	weekdays := [][]string{
		{"sun", "sunday", "son", "sonntag"},
		{"mon", "monday", "montag"},
		{"tue", "tues", "tuesday", "die", "dienstag"},
		{"wed", "wednesday", "mit", "mittwoch"},
		{"thu", "thur", "thurs", "thursday", "don", "donnerstag"},
		{"fri", "friday", "fre", "freitag"},
		{"sat", "saturday", "sam", "samstag", "sonnabend"},
	}
	for i, names := range weekdays {
		for _, n := range names {
			weekdayWords[n] = time.Weekday(i)
			dateWords[n] = true
		}
	}

	// Meridiem markers, ordinal suffixes and timezone abbreviations. The ISO
	// 8601 "T" separator is handled before tokenizing, not here.
	shared := []string{
		"am", "pm", "a", "p",
		"utc", "gmt", "z", "cet", "cest", "mez", "mesz", "est", "edt",
		"cst", "cdt", "mst", "mdt", "pst", "pdt", "bst", "wet", "west",
	}
	for _, w := range shared {
		dateWords[w] = true
		timeWords[w] = true
	}
	// Ordinal suffixes belong to a date ("20th"), never to a clock time.
	for _, w := range []string{"st", "nd", "rd", "th"} {
		dateWords[w] = true
	}
}

// isoSeparator matches the "T" that ISO 8601 puts between a date and a time,
// so it can be treated as punctuation rather than an unknown word.
var isoSeparator = regexp.MustCompile(`(\d)[Tt](\d)`)

// stripISOSeparator replaces the ISO 8601 date/time separator with a space.
func stripISOSeparator(t string) string {
	return isoSeparator.ReplaceAllString(t, "$1 $2")
}

// weekdayWordMatches reports whether every weekday name in t names d's weekday.
func weekdayWordMatches(t string, d time.Weekday) bool {
	for _, w := range alphaRuns.FindAllString(t, -1) {
		if got, ok := weekdayWords[strings.ToLower(w)]; ok && got != d {
			return false
		}
	}
	return true
}

// timeTextRemainderOK is the clock-time counterpart of dateTextRemainderOK. It
// allows only the words that belong beside a time, so a month or weekday name
// -- which a time32 value cannot encode -- keeps its text column.
func timeTextRemainderOK(t string) bool {
	for _, w := range alphaRuns.FindAllString(t, -1) {
		if !timeWords[strings.ToLower(w)] {
			return false
		}
	}
	rest := dateTokens.ReplaceAllString(t, " ")
	rest = alphaRuns.ReplaceAllString(rest, " ")
	for _, sep := range []string{":", ".", ",", " ", "+", "-"} {
		rest = strings.ReplaceAll(rest, sep, "")
	}
	return strings.TrimSpace(rest) == ""
}

// alphaRuns extracts the maximal alphabetic tokens from a string.
var alphaRuns = regexp.MustCompile(`[\p{L}]+`)

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
	t = stripISOSeparator(t)
	if !dateTextRemainderOK(t) {
		return false
	}
	us, ok := qvd.QlikDaysToTimestampMicros(n, loc)
	if !ok {
		return false
	}
	ts := time.UnixMicro(us).In(loc)

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
		if tokensMatchDate(tokens, d, ts) &&
			monthWordMatches(t, int(d.Month())) && weekdayWordMatches(t, d.Weekday()) {
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

// dateTextRemainderOK reports whether everything in t that is not a digit
// belongs to a rendered date: separators, and only words drawn from dateWords.
// This is what rejects "Order 11 of 2010" and "Due 11/20/2010".
func dateTextRemainderOK(t string) bool {
	for _, w := range alphaRuns.FindAllString(t, -1) {
		if !dateWords[strings.ToLower(w)] {
			return false
		}
	}
	// Whatever remains once digits and words are gone must be punctuation.
	rest := dateTokens.ReplaceAllString(t, " ")
	rest = alphaRuns.ReplaceAllString(rest, " ")
	for _, sep := range []string{"/", ".", "-", ":", ",", " ", "'", "+"} {
		rest = strings.ReplaceAll(rest, sep, "")
	}
	return strings.TrimSpace(rest) == ""
}

// monthWordMatches reports whether every month name in t names the month m.
// A string that says "Jan" beside a November value is not a rendering of it.
func monthWordMatches(t string, m int) bool {
	for _, w := range alphaRuns.FindAllString(t, -1) {
		if got, ok := monthWords[strings.ToLower(w)]; ok && got != m {
			return false
		}
	}
	return true
}

// textRendersTime reports whether text renders the fraction-of-a-day value n
// as a clock time. A TIME column carries no calendar date, so the day-token
// requirement that textRendersDate applies would reject "12:30:00" outright.
func textRendersTime(text string, n float64) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}
	if !timeTextRemainderOK(t) {
		return false
	}
	ms, ok := qvd.QlikFractionToTimeMillis(n)
	if !ok {
		return false
	}
	total := int(ms)
	h, m := total/3600000, total/60000%60
	sec, milli := total/1000%60, total%1000

	// A meridiem marker is a claim about the value, not decoration: "12:00 PM"
	// must not be accepted as a rendering of midnight. Reject the string when
	// the marker disagrees with the hour.
	switch {
	case hasMeridiem(t, "pm") && h < 12:
		return false
	case hasMeridiem(t, "am") && h >= 12:
		return false
	}

	allowed := map[int]bool{h: true, m: true, sec: true, milli: true}
	if h%12 == 0 {
		allowed[12] = true
	} else {
		allowed[h%12] = true
	}
	nums := dateTokens.FindAllString(t, -1)
	if len(nums) < 2 {
		return false
	}
	for _, tok := range nums {
		v, err := strconv.Atoi(tok)
		if err != nil || !allowed[v] {
			return false
		}
	}
	return true
}

// hasMeridiem reports whether text carries the given AM/PM marker as a
// standalone token, so that a word merely containing those letters does not
// count.
func hasMeridiem(text, marker string) bool {
	lower := strings.ToLower(text)
	for _, form := range []string{marker, marker[:1] + "." + marker[1:] + "."} {
		i := strings.Index(lower, form)
		if i < 0 {
			continue
		}
		beforeOK := i == 0 || !isLetter(rune(lower[i-1]))
		end := i + len(form)
		afterOK := end >= len(lower) || !isLetter(rune(lower[end]))
		if beforeOK && afterOK {
			return true
		}
	}
	return false
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// textRendersDecimal reports whether text is a rendering of the exact decimal
// that will be written for symbol index i.
func textRendersDecimal(text string, i int, rc *ResolvedColumn) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}
	if i < 0 || i >= len(rc.Scaled) || rc.Scaled[i] == nil {
		return false
	}
	// FormatScaled renders with "." as the separator, so compare the parsed
	// values rather than the strings.
	want, err := strconv.ParseFloat(FormatScaled(rc.Scaled[i], rc.Decimal.Scale), 64)
	if err != nil {
		return false
	}
	got, ok := parseLocalizedNumber(t, decSepOr(rc), thouSepOr(rc))
	if !ok {
		return false
	}
	// Both sides carry at most Scale decimals, so half a unit in the last
	// place is the right tolerance.
	eps := math.Pow(10, -float64(rc.Decimal.Scale)) / 2
	return math.Abs(want-got) <= eps
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
// inferred as dates: the years 1600 to 2200. The bound is a sanity check, not
// the real filter -- textRendersDate does that work by requiring the display
// string to render this particular serial -- so it only has to exclude values
// no date could plausibly hold.
//
// The lower end reaches 1600 rather than 1900 because historical series are
// real: the Stockholm temperature record starts in 1756, serial -52593, and a
// 1900 floor left it as a bare integer beside a __text sidecar. Before about
// 1582 a date is ambiguous about which calendar it is in, so that is a
// reasonable place to stop.
const (
	minDateSerial = -109571.0 // 1600-01-01
	maxDateSerial = 109575.0  // 2200-01-01
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
func InferDateTimeFromDuals(col qvd.Column, syms []qvd.Symbol, loc *time.Location,
	emptyAsNull bool) (DateInference, bool) {
	var inf DateInference
	hasFraction, hasClock := false, false

	for _, s := range syms {
		// A symbol carrying no value says nothing either way. Skipping it
		// matters: a single empty-string placeholder among thousands of dated
		// duals used to disqualify the whole column, which left an untyped
		// Qlik serial and a __text sidecar where a timestamp belonged. The
		// value is written as null regardless, so it is no evidence against
		// the rest.
		if symbolIsAbsent(s, emptyAsNull) {
			continue
		}
		// Every value-bearing symbol must be a dual: a bare number carries no
		// evidence, so inferring from a partial column would be a guess.
		if s.Kind != qvd.SymbolDualIntString && s.Kind != qvd.SymbolDualFloatString {
			return DateInference{}, false
		}
		// A blank display string says nothing about the value, so it cannot
		// support reading the column as a date.
		if strings.TrimSpace(s.Text) == "" {
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

// decSepOr and thouSepOr fall back to the conventional separators when the
// column's header declared none.
func decSepOr(rc *ResolvedColumn) string {
	if rc.DecSep == "" {
		return "."
	}
	return rc.DecSep
}

func thouSepOr(rc *ResolvedColumn) string { return rc.ThouSep }
