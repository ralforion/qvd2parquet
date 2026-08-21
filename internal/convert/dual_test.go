package convert

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

func TestClassifyDualFormattingVsInformative(t *testing.T) {
	tests := []struct {
		name     string
		col      qvd.Column
		strategy ValueStrategy
		syms     []qvd.Symbol
		want     DualKind
	}{
		{
			"localized numbers are formatting",
			qvd.Column{QlikType: qvd.QlikMoney, DecSep: ",", ThouSep: "."},
			StrategyDecimal,
			[]qvd.Symbol{qvdtest.DualFloat(1234.56, "1.234,56"), qvdtest.DualFloat(-10.5, "-10,50")},
			DualFormatting,
		},
		{
			"plain numbers are formatting",
			qvd.Column{QlikType: qvd.QlikInteger},
			StrategyInt64,
			[]qvd.Symbol{qvdtest.DualInt(10, "10"), qvdtest.DualInt(-3, "-3")},
			DualFormatting,
		},
		{
			"labels are informative",
			qvd.Column{QlikType: qvd.QlikInteger},
			StrategyInt64,
			[]qvd.Symbol{qvdtest.DualInt(1, "Open"), qvdtest.DualInt(2, "Closed")},
			DualInformative,
		},
		{
			"one label among numbers is still informative",
			qvd.Column{QlikType: qvd.QlikInteger},
			StrategyInt64,
			[]qvd.Symbol{qvdtest.DualInt(10, "10"), qvdtest.DualInt(-1, "unknown")},
			DualInformative,
		},
		{
			"rendered dates are formatting beside a date column",
			qvd.Column{QlikType: qvd.QlikDate},
			StrategyDate32,
			[]qvd.Symbol{qvdtest.DualInt(40502, "11/20/2010")},
			DualFormatting,
		},
		{
			// Beside a bare number the reader cannot know it is a date, so the
			// text is worth keeping.
			"rendered dates are informative beside a number",
			qvd.Column{QlikType: qvd.QlikUnknown},
			StrategyFloat64,
			[]qvd.Symbol{qvdtest.DualInt(40502, "11/20/2010")},
			DualInformative,
		},
		{
			"empty display strings carry nothing",
			qvd.Column{QlikType: qvd.QlikInteger},
			StrategyInt64,
			[]qvd.Symbol{qvdtest.DualInt(1, ""), qvdtest.DualInt(2, "")},
			DualFormatting,
		},
		{
			"no duals at all",
			qvd.Column{QlikType: qvd.QlikInteger},
			StrategyInt64,
			[]qvd.Symbol{qvdtest.Int(1)},
			DualNone,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc := &ResolvedColumn{
				Strategy: tc.strategy,
				DecSep:   tc.col.DecSep,
				ThouSep:  tc.col.ThouSep,
			}
			got := ClassifyDual(tc.col, tc.syms, rc, time.UTC)
			if got.Kind != tc.want {
				t.Errorf("kind = %v, want %v (informative=%d of %d, example %q)",
					got.Kind, tc.want, got.Informative, got.Duals, got.Example)
			}
		})
	}
}

// The default --dual=auto must add a text column only for informative duals.
func TestDualAutoEmitsTextOnlyWhenInformative(t *testing.T) {
	tests := []struct {
		name        string
		field       qvdtest.Field
		wantColumns int
	}{
		{
			"localized money keeps one column",
			qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 2, Dec: ",", Thou: ".", Rows: []int{0, 1},
				Symbols: []qvd.Symbol{
					qvdtest.DualFloat(1234.56, "1.234,56"),
					qvdtest.DualFloat(-10.5, "-10,50"),
				}},
			1,
		},
		{
			"formatted date keeps one column",
			qvdtest.Field{Name: "Day", Type: "DATE", Rows: []int{0, 1},
				Symbols: []qvd.Symbol{
					qvdtest.DualInt(45000, "15.03.2023"),
					qvdtest.DualInt(45001, "16.03.2023"),
				}},
			1,
		},
		{
			"status labels produce two columns",
			qvdtest.Field{Name: "Status", Type: "INTEGER", Rows: []int{0, 1},
				Symbols: []qvd.Symbol{
					qvdtest.DualInt(1, "Open"),
					qvdtest.DualInt(2, "Closed"),
				}},
			2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := mustResolve(t, tc.field, nil) // --dual defaults to auto
			if len(rs.Columns) != tc.wantColumns {
				var names []string
				for _, c := range rs.Columns {
					names = append(names, c.Name)
				}
				t.Fatalf("got %d columns %v, want %d", len(rs.Columns), names, tc.wantColumns)
			}
			if tc.wantColumns == 2 {
				if rs.Columns[1].Name != tc.field.Name+"__text" {
					t.Errorf("second column = %q", rs.Columns[1].Name)
				}
				// The reason must be stated, not silent.
				if !strings.Contains(strings.Join(rs.Notes, " "), "carry text the number does not") {
					t.Errorf("notes should explain why: %v", rs.Notes)
				}
			}
		})
	}
}

// The explicit strategies must still override the classification.
func TestDualExplicitStrategiesOverrideAuto(t *testing.T) {
	// Formatting-only duals, which auto would collapse to one column.
	f := qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 2, Dec: ",", Rows: []int{0},
		Symbols: []qvd.Symbol{qvdtest.DualFloat(12.5, "12,50")}}

	if rs := mustResolve(t, f, func(o *Options) { o.Dual = DualColumns }); len(rs.Columns) != 2 {
		t.Errorf("--dual=columns gave %d columns, want 2", len(rs.Columns))
	}
	if rs := mustResolve(t, f, func(o *Options) { o.Dual = DualNumeric }); len(rs.Columns) != 1 {
		t.Errorf("--dual=numeric gave %d columns, want 1", len(rs.Columns))
	}
	rs := mustResolve(t, f, func(o *Options) { o.Dual = DualText })
	if len(rs.Columns) != 1 || rs.Columns[0].Strategy != StrategyString {
		t.Errorf("--dual=text gave %+v, want a single utf8 column", rs.Columns)
	}
}

func TestTextRendersDate(t *testing.T) {
	// 40502 is the Excel/Qlik serial for 2010-11-20.
	const serial = 40502
	for _, text := range []string{
		"11/20/2010", "20.11.2010", "2010-11-20", "20-11-10",
		"2010-11-20 00:00:00", "20 Nov 2010",
	} {
		if !textRendersDate(text, serial, time.UTC) {
			t.Errorf("%q should be recognized as a rendering of serial %d", text, serial)
		}
	}
	for _, text := range []string{
		"Order 11 of 2010", // leftover words
		"Open",             // no digits
		"12345",            // a single number is not evidence
		"01/02/2003",       // a different date
	} {
		if textRendersDate(text, serial, time.UTC) {
			t.Errorf("%q should not be treated as a rendering of serial %d", text, serial)
		}
	}
}

// A display string rendered in another timezone can name the neighbouring day,
// which must not defeat the match. Real data depends on this.
func TestTextRendersDateToleratesTimezoneShift(t *testing.T) {
	// 40377.958333 is 2010-07-18 23:00 UTC, written out as "07/19/2010" by a
	// UTC+1 producer.
	if !textRendersDate("07/19/2010", 40377.958333333336, time.UTC) {
		t.Error("a one-day timezone shift should still match")
	}
	// Two days away is not a timezone artifact.
	if textRendersDate("07/21/2010", 40377.958333333336, time.UTC) {
		t.Error("a two-day difference should not match")
	}
}

func TestParseLocalizedNumber(t *testing.T) {
	tests := []struct {
		s, dec, thou string
		want         float64
		ok           bool
	}{
		{"1.234,56", ",", ".", 1234.56, true},
		{"1,234.56", ".", ",", 1234.56, true},
		{"-10,50", ",", ".", -10.5, true},
		{"(12,34)", ",", ".", -12.34, true},
		{"1 234,50", ",", " ", 1234.5, true},
		{"12", ".", "", 12, true},
		{"abc", ".", "", 0, false},
		{"12,3.4", ",", "", 0, false}, // mixed separators are not described by the declaration
	}
	for _, tc := range tests {
		got, ok := parseLocalizedNumber(tc.s, tc.dec, tc.thou)
		if ok != tc.ok {
			t.Errorf("parseLocalizedNumber(%q) ok = %v, want %v", tc.s, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseLocalizedNumber(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// A blank display string is no evidence of anything, so it must not support
// reading an untyped numeric column as a date.
func TestInferDateRejectsBlankDisplayStrings(t *testing.T) {
	col := qvd.Column{QlikType: qvd.QlikUnknown}
	syms := []qvd.Symbol{
		qvdtest.DualInt(40502, ""),
		qvdtest.DualInt(40503, ""),
	}
	if inf, ok := InferDateTimeFromDuals(col, syms, time.UTC); ok {
		t.Errorf("blank display strings should not infer %v", inf.Type)
	}
	// One blank among real dates is also not enough.
	syms = []qvd.Symbol{
		qvdtest.DualInt(40502, "11/20/2010"),
		qvdtest.DualInt(40503, ""),
	}
	if _, ok := InferDateTimeFromDuals(col, syms, time.UTC); ok {
		t.Error("a blank display string should block inference")
	}
}

// A TIME column's display string has no calendar day, so it must not be
// treated as informative just because textRendersDate wants one.
func TestTimeDualsAreFormatting(t *testing.T) {
	f := qvdtest.Field{Name: "Clock", Type: "TIME", Rows: []int{0, 1},
		Symbols: []qvd.Symbol{
			qvdtest.DualFloat(0.5, "12:00:00"),
			qvdtest.DualFloat(0.25, "06:00:00"),
		}}
	rs := mustResolve(t, f, nil) // --dual defaults to auto
	if len(rs.Columns) != 1 {
		var names []string
		for _, c := range rs.Columns {
			names = append(names, c.Name)
		}
		t.Fatalf("got %d columns %v, want 1: a rendered time adds nothing to a time32 column",
			len(rs.Columns), names)
	}
	if rs.Columns[0].Strategy != StrategyTimeMillis {
		t.Errorf("strategy = %v, want StrategyTimeMillis", rs.Columns[0].Strategy)
	}
}

func TestTextRendersTime(t *testing.T) {
	// 0.5 of a day is 12:00:00.
	for _, s := range []string{"12:00:00", "12:00", "12:00:00 PM"} {
		if !textRendersTime(s, 0.5) {
			t.Errorf("%q should render 0.5 of a day", s)
		}
	}
	for _, s := range []string{"06:30:00", "Open"} {
		if textRendersTime(s, 0.5) {
			t.Errorf("%q should not render 0.5 of a day", s)
		}
	}
}

// A decimal column may be written from the display string or from a rounded
// payload, so redundancy must be judged against the written value.
func TestDecimalDualComparesAgainstWrittenValue(t *testing.T) {
	// The payload carries more precision than the declared scale, so the
	// written value is the rounded 1.23 that the display string already shows.
	f := qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 2, Dec: ".", Rows: []int{0, 1},
		Symbols: []qvd.Symbol{
			qvdtest.DualFloat(1.234, "1.23"),
			qvdtest.DualFloat(9.876, "9.88"),
		}}
	rs := mustResolve(t, f, nil)
	if len(rs.Columns) != 1 {
		var names []string
		for _, c := range rs.Columns {
			names = append(names, c.Name)
		}
		t.Fatalf("got %d columns %v, want 1: the text matches the written decimal",
			len(rs.Columns), names)
	}
	if got := rs.Columns[0].Scaled[0].String(); got != "123" {
		t.Errorf("scaled = %s, want 123", got)
	}

	// A genuine label beside a decimal is still kept.
	f2 := qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 2, Dec: ".", Rows: []int{0},
		Symbols: []qvd.Symbol{qvdtest.DualFloat(1.23, "billed")}}
	if rs := mustResolve(t, f2, nil); len(rs.Columns) != 2 {
		t.Errorf("a label beside a decimal should be kept, got %d columns", len(rs.Columns))
	}
}

// The Qlik $date/$timestamp tags identify a date column with no duals at all,
// which the display-string heuristic cannot do.
func TestTaggedTypeIdentifiesPlainNumericDates(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		syms []qvd.Symbol
		want string
	}{
		{"$date", []string{"$numeric", "$date"},
			[]qvd.Symbol{qvdtest.Int(45000), qvdtest.Int(45001)}, "date32"},
		{"$timestamp", []string{"$numeric", "$timestamp"},
			[]qvd.Symbol{qvdtest.Float(45000.25), qvdtest.Float(45001.5)}, "timestamp[ms, tz=UTC]"},
		{"$integer stays numeric", []string{"$numeric", "$integer"},
			[]qvd.Symbol{qvdtest.Int(45000), qvdtest.Int(45001)}, "int64"},
		{"no tags stay numeric", nil,
			[]qvd.Symbol{qvdtest.Int(45000), qvdtest.Int(45001)}, "int64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := qvdtest.Field{Name: "V", Type: "", Rows: []int{0, 1}, Tags: tc.tags, Symbols: tc.syms}
			rs := mustResolve(t, f, func(o *Options) { o.Location = utc(); o.TimezoneName = "UTC" })
			if got := rs.Columns[0].ArrowType.String(); got != tc.want {
				t.Errorf("type = %s, want %s", got, tc.want)
			}
		})
	}
}

// A declared type still wins over a tag, since it is the more specific
// statement.
func TestDeclaredTypeBeatsTag(t *testing.T) {
	f := qvdtest.Field{Name: "V", Type: "INTEGER", Rows: []int{0},
		Tags: []string{"$date"}, Symbols: []qvd.Symbol{qvdtest.Int(45000)}}
	rs := mustResolve(t, f, nil)
	if got := rs.Columns[0].ArrowType.String(); got != "int64" {
		t.Errorf("type = %s, want int64: a declared INTEGER outranks a $date tag", got)
	}
}

// The resolved column must carry the source field's separators, or a
// localized decimal is compared against "." and misjudged as informative.
func TestResolvedColumnCarriesSeparators(t *testing.T) {
	f := qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 2, Dec: ",", Thou: ".", Rows: []int{0},
		Symbols: []qvd.Symbol{qvdtest.DualFloat(1234.56, "1.234,56")}}
	rs := mustResolve(t, f, nil)
	c := rs.Columns[0]
	if c.DecSep != "," || c.ThouSep != "." {
		t.Errorf("separators = %q / %q, want , / .", c.DecSep, c.ThouSep)
	}
}

// A German-localized MONEY column whose payload rounds to what the display
// string already shows must not produce a redundant text column.
func TestLocalizedDecimalDualIsRedundant(t *testing.T) {
	tests := []struct {
		name    string
		dec     string
		thou    string
		symbols []qvd.Symbol
		want    int
	}{
		{"comma decimal, rounded payload", ",", ".",
			[]qvd.Symbol{qvdtest.DualFloat(1.234, "1,23"), qvdtest.DualFloat(9.876, "9,88")}, 1},
		{"comma decimal with grouping", ",", ".",
			[]qvd.Symbol{qvdtest.DualFloat(1234.56, "1.234,56")}, 1},
		{"dot decimal", ".", ",",
			[]qvd.Symbol{qvdtest.DualFloat(1.234, "1.23")}, 1},
		{"a genuine label is still kept", ",", ".",
			[]qvd.Symbol{qvdtest.DualFloat(1.23, "storniert")}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := make([]int, len(tc.symbols))
			for i := range rows {
				rows[i] = i
			}
			f := qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 2,
				Dec: tc.dec, Thou: tc.thou, Symbols: tc.symbols, Rows: rows}
			rs := mustResolve(t, f, nil)
			if len(rs.Columns) != tc.want {
				var names []string
				for _, c := range rs.Columns {
					names = append(names, c.Name)
				}
				t.Errorf("got %d columns %v, want %d", len(rs.Columns), names, tc.want)
			}
		})
	}
}

// A meridiem marker is a claim about the value: "12:00 PM" is noon, not
// midnight, so it must not be accepted as a rendering of 0.0.
func TestTextRendersTimeHonoursMeridiem(t *testing.T) {
	const midnight, noon = 0.0, 0.5
	tests := []struct {
		text string
		v    float64
		want bool
	}{
		{"12:00 AM", midnight, true},
		{"12:00 PM", midnight, false}, // noon claimed for midnight
		{"12:00 PM", noon, true},
		{"12:00 AM", noon, false}, // midnight claimed for noon
		{"12:00", noon, true},     // no marker, so no claim
		{"12:00", midnight, true},
		{"11:00 PM", 23.0 / 24, true},
		{"11:00 AM", 23.0 / 24, false},
	}
	for _, tc := range tests {
		if got := textRendersTime(tc.text, tc.v); got != tc.want {
			t.Errorf("textRendersTime(%q, %v) = %v, want %v", tc.text, tc.v, got, tc.want)
		}
	}
}

func TestHasMeridiem(t *testing.T) {
	for _, s := range []string{"12:00 PM", "12:00pm", "12:00 P.M.", "12:00 pm"} {
		if !hasMeridiem(s, "pm") {
			t.Errorf("%q should carry a PM marker", s)
		}
	}
	// A word that merely contains the letters is not a marker.
	for _, s := range []string{"12:00 Sample", "12:00 ampere"} {
		if hasMeridiem(s, "am") {
			t.Errorf("%q should not count as an AM marker", s)
		}
	}
}

// The note must name the column that is actually generated, which differs from
// the source field name once --field-regex renames it.
func TestDualNoteNamesTheGeneratedColumn(t *testing.T) {
	renamer, err := NewFieldRenamer(`^[^-]*-\|\|-(?P<name>[^-]*)-\|\|-(?P<comment>.*)$`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	f := qvdtest.Field{Name: "A057-||-STATUS-||-Bearbeitungsstatus", Type: "INTEGER", Rows: []int{0, 1},
		Symbols: []qvd.Symbol{qvdtest.DualInt(1, "Open"), qvdtest.DualInt(2, "Closed")}}

	rs := mustResolve(t, f, func(o *Options) { o.Renamer = renamer })
	if len(rs.Columns) != 2 {
		t.Fatalf("got %d columns, want 2", len(rs.Columns))
	}
	generated := rs.Columns[1].Name
	if generated != "STATUS__text" {
		t.Fatalf("generated column = %q, want STATUS__text", generated)
	}
	notes := strings.Join(rs.Notes, " ")
	if !strings.Contains(notes, `"`+generated+`"`) {
		t.Errorf("notes should name the generated column %q: %s", generated, notes)
	}
	if strings.Contains(notes, `"A057-||-STATUS-||-Bearbeitungsstatus__text"`) {
		t.Errorf("notes name a column that does not exist: %s", notes)
	}
}

// Only words that genuinely belong to a rendered date may be swallowed. A
// wrongly rejected word costs a redundant text column; a wrongly accepted one
// drops text that carried information, so the check errs strict.
func TestTextRendersDateRejectsExtraWords(t *testing.T) {
	const serial = 40502 // 2010-11-20
	rejected := []string{
		"Due 11/20/2010",
		"11/20/2010 invoiced",
		"Fällig 20.11.2010",
		"Order 11 of 2010",
		"11/20/2010 (revised)",
		"ship 11/20/2010 asap",
	}
	for _, text := range rejected {
		if textRendersDate(text, serial, time.UTC) {
			t.Errorf("%q carries more than the date and must not be dropped", text)
		}
	}

	accepted := []string{
		"11/20/2010",
		"20.11.2010",
		"2010-11-20",
		"20 Nov 2010",
		"Sat, 20 November 2010",
		"20. Nov 2010",
		"2010-11-20 00:00:00 UTC",
		"20th Nov 2010",
	}
	for _, text := range accepted {
		if !textRendersDate(text, serial, time.UTC) {
			t.Errorf("%q is a plain rendering of the date and should be recognized", text)
		}
	}
}

// A month name that contradicts the value is not a rendering of it.
func TestTextRendersDateChecksMonthNames(t *testing.T) {
	const serial = 40502 // 2010-11-20, a November date
	if !textRendersDate("20 Nov 2010", serial, time.UTC) {
		t.Error("the correct month name should match")
	}
	if textRendersDate("20 Jan 2010", serial, time.UTC) {
		t.Error("January must not match a November value")
	}
	if textRendersDate("20 Dez 2010", serial, time.UTC) {
		t.Error("December must not match a November value")
	}
	// German for November still matches.
	if !textRendersDate("20. November 2010", serial, time.UTC) {
		t.Error("the German month name should match")
	}
}

// Through the resolver: a label wrapped around a date keeps its text column.
func TestDualAutoKeepsAnnotatedDates(t *testing.T) {
	f := qvdtest.Field{Name: "Day", Type: "DATE", Rows: []int{0, 1},
		Symbols: []qvd.Symbol{
			qvdtest.DualInt(40502, "Due 11/20/2010"),
			qvdtest.DualInt(40503, "Due 11/21/2010"),
		}}
	rs := mustResolve(t, f, func(o *Options) { o.Location = utc() })
	if len(rs.Columns) != 2 {
		var names []string
		for _, c := range rs.Columns {
			names = append(names, c.Name)
		}
		t.Fatalf("got %d columns %v, want 2: \"Due\" is not part of the date", len(rs.Columns), names)
	}
}

// A date/time column must fail at schema resolution when a value cannot be
// converted, whatever decided the type: the declared header, a Qlik tag, or
// display-string inference. Failing here means --inspect predicts it and no
// output file is started.
//
// NaN and infinity are the exception: they are not values a date can hold, and
// writing them as null loses nothing. A finite value that simply does not fit
// is different, because nulling it would discard real data.
func TestDateTimeColumnsValidateAtSchemaTime(t *testing.T) {
	tests := []struct {
		name  string
		field qvdtest.Field
	}{
		{
			"declared DATE far out of range",
			qvdtest.Field{Name: "D", Type: "DATE", Rows: []int{0, 1},
				Symbols: []qvd.Symbol{qvdtest.Int(45000), qvdtest.Float(1e30)}},
		},
		{
			"tagged $timestamp out of range",
			qvdtest.Field{Name: "D", Type: "", Tags: []string{"$numeric", "$timestamp"}, Rows: []int{0, 1},
				Symbols: []qvd.Symbol{qvdtest.Int(45000), qvdtest.Float(1e30)}},
		},
		{
			"declared DATE with a text symbol",
			qvdtest.Field{Name: "D", Type: "DATE", Rows: []int{0, 1},
				Symbols: []qvd.Symbol{qvdtest.Int(45000), qvdtest.Str("tomorrow")}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolve(t, tc.field, func(o *Options) { o.Location = utc() })
			if !errors.Is(err, ErrSchemaPolicy) {
				t.Fatalf("err = %v, want ErrSchemaPolicy at schema resolution", err)
			}
			if !strings.Contains(err.Error(), tc.field.Name) {
				t.Errorf("error should name the column: %v", err)
			}
		})
	}
}

// NaN and infinity become null rather than failing the conversion, and the
// substitution is counted so it can be reported.
func TestNonFiniteValuesBecomeNull(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	tests := []struct {
		name  string
		field qvdtest.Field
		want  string
	}{
		{"DATE with NaN", qvdtest.Field{Name: "D", Type: "DATE", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Int(45000), qvdtest.Float(nan)}}, "date32"},
		{"tagged $date with NaN", qvdtest.Field{Name: "D", Type: "", Tags: []string{"$date"},
			Rows:    []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Int(45000), qvdtest.Float(nan)}}, "date32"},
		{"TIMESTAMP with infinity", qvdtest.Field{Name: "D", Type: "TIMESTAMP", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Float(45000.5), qvdtest.Float(inf)}}, "timestamp[ms, tz=UTC]"},
		{"TIME with NaN", qvdtest.Field{Name: "T", Type: "TIME", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Float(0.5), qvdtest.Float(nan)}}, "time32[ms]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := mustResolve(t, tc.field, func(o *Options) { o.Location = utc(); o.TimezoneName = "UTC" })
			c := rs.Columns[0]
			if got := c.ArrowType.String(); got != tc.want {
				t.Fatalf("type = %s, want %s", got, tc.want)
			}
			if c.NonFiniteNulls != 1 {
				t.Errorf("NonFiniteNulls = %d, want 1", c.NonFiniteNulls)
			}
			if !strings.Contains(strings.Join(rs.Notes, " "), "written as null") {
				t.Errorf("the substitution should be reported: %v", rs.Notes)
			}
		})
	}
}

// Valid date/time columns must keep resolving cleanly.
func TestValidDateTimeColumnsStillResolve(t *testing.T) {
	tests := []struct {
		field qvdtest.Field
		want  string
	}{
		{qvdtest.Field{Name: "D", Type: "DATE", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Int(45000)}}, "date32"},
		{qvdtest.Field{Name: "T", Type: "TIME", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Float(0.5)}}, "time32[ms]"},
		{qvdtest.Field{Name: "S", Type: "", Tags: []string{"$timestamp"}, Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Float(45000.25)}}, "timestamp[ms, tz=UTC]"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			rs := mustResolve(t, tc.field, func(o *Options) { o.Location = utc(); o.TimezoneName = "UTC" })
			if got := rs.Columns[0].ArrowType.String(); got != tc.want {
				t.Errorf("type = %s, want %s", got, tc.want)
			}
		})
	}
}

// A clock time carries no calendar date, so a month or weekday name beside it
// says something a time32 value cannot encode and must keep its text column.
func TestTextRendersTimeRejectsCalendarWords(t *testing.T) {
	const noon = 0.5
	for _, s := range []string{"Mon 12:00", "May 12:00", "12:00 Tuesday", "12:00 Nov"} {
		if textRendersTime(s, noon) {
			t.Errorf("%q names a day or month, which a time value cannot encode", s)
		}
	}
	for _, s := range []string{"12:00", "12:00:00", "12:00 PM", "12:00:00 UTC"} {
		if !textRendersTime(s, noon) {
			t.Errorf("%q is a plain rendering of noon", s)
		}
	}
}

// A weekday name is a claim about the date, so it must agree with it.
func TestTextRendersDateChecksWeekdayNames(t *testing.T) {
	// Serial 40502 is Saturday, 20 November 2010.
	const serial = 40502
	if got := time.UnixMilli(func() int64 {
		ms, _ := qvd.QlikDaysToTimestampMillis(serial, time.UTC)
		return ms
	}()).UTC().Weekday(); got != time.Saturday {
		t.Fatalf("fixture assumption wrong: serial %d is a %s", serial, got)
	}

	if !textRendersDate("Sat, 20 Nov 2010", serial, time.UTC) {
		t.Error("the correct weekday should match")
	}
	if !textRendersDate("Samstag, 20. November 2010", serial, time.UTC) {
		t.Error("the German weekday should match")
	}
	if textRendersDate("Mon, 20 Nov 2010", serial, time.UTC) {
		t.Error("Monday must not match a Saturday")
	}
	if textRendersDate("Freitag, 20. November 2010", serial, time.UTC) {
		t.Error("Friday must not match a Saturday")
	}
}

// ISO 8601 puts a "T" between the date and the time; it is punctuation, not a
// word, and must not block recognition.
func TestTextRendersDateAcceptsISO8601(t *testing.T) {
	const serial = 40502
	for _, s := range []string{
		"2010-11-20T00:00:00Z",
		"2010-11-20T00:00:00",
		"2010-11-20t00:00:00z",
		"2010-11-20 00:00:00",
	} {
		if !textRendersDate(s, serial, time.UTC) {
			t.Errorf("%q should be recognized as an ISO rendering", s)
		}
	}
	// A stray T that is not between digits is still an unknown word.
	if textRendersDate("T 11/20/2010", serial, time.UTC) {
		t.Error("a leading \"T\" is not the ISO separator")
	}
}

// ISO-formatted duals must still support reading an untyped column as a date.
func TestInferDatesFromISOStrings(t *testing.T) {
	col := qvd.Column{QlikType: qvd.QlikUnknown}
	syms := []qvd.Symbol{
		qvdtest.DualInt(40502, "2010-11-20T00:00:00Z"),
		qvdtest.DualInt(40503, "2010-11-21T00:00:00Z"),
	}
	inf, ok := InferDateTimeFromDuals(col, syms, time.UTC)
	if !ok {
		t.Fatal("ISO display strings should support date inference")
	}
	if inf.Type != qvd.QlikTimestamp {
		t.Errorf("inferred %v, want QlikTimestamp (the strings carry a clock)", inf.Type)
	}
}
