package convert

import (
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
