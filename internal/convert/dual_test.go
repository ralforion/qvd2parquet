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
			got := ClassifyDual(tc.col, tc.syms, tc.strategy, time.UTC)
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
