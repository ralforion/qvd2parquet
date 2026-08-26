package convert

import (
	"strings"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// A date column's range must read as dates. A QVD stores serial day numbers,
// so a goods-issue date a millennium out arrives as the integer 411241 and
// hides in a profile among plausible neighbours.
func TestValueRangeRendersDatesNotSerials(t *testing.T) {
	syms := []qvd.Symbol{qvdtest.Int(43826), qvdtest.Int(45000), qvdtest.Int(411241)}
	rows := []int{0, 1, 2}
	rs, qf := resolveFixture(t, qvdtest.Field{
		Name: "WADAT", Type: "DATE", Symbols: syms, Rows: rows,
	})
	defer qf.Close()

	got := ValueRange(&rs.Columns[0], qf.Profiles[rs.Columns[0].SourceIndex], nil)
	const want = "2019-12-27 .. 3025-12-08"
	if got != want {
		t.Errorf("range = %q, want %q", got, want)
	}
	if strings.Contains(got, "411241") {
		t.Error("the raw Qlik serial leaked into the rendered range")
	}
}

// Text columns have no range worth printing.
func TestValueRangeEmptyForText(t *testing.T) {
	rs, qf := resolveFixture(t, qvdtest.Field{
		Name: "Name", Type: "ASCII",
		Symbols: []qvd.Symbol{qvdtest.Str("a"), qvdtest.Str("b")},
		Rows:    []int{0, 1},
	})
	defer qf.Close()
	if got := ValueRange(&rs.Columns[0], qf.Profiles[0], nil); got != "" {
		t.Errorf("text column reported a range: %q", got)
	}
}

// A decimal's precision is inferred from its values, so every decimal column
// fits its data exactly and the interesting question is what a later load has
// left. Only the column short of room is reported.
func TestDecimalHeadroom(t *testing.T) {
	for _, tc := range []struct {
		name    string
		syms    []qvd.Symbol
		wantHi  bool // at or above DecimalTightFraction
		wantPct float64
	}{
		{"fills most of the range", []qvd.Symbol{qvdtest.Float(8115022364.86)}, true, 0.81},
		{"plenty of room", []qvd.Symbol{qvdtest.Float(12.50)}, false, 0.13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs, qf := resolveFixture(t, qvdtest.Field{
				Name: "V", Type: "REAL", NDec: 2, Dec: ".",
				Symbols: tc.syms, Rows: []int{0},
			})
			defer qf.Close()
			c := &rs.Columns[0]
			used := DecimalHeadroom(c, qf.Profiles[c.SourceIndex])
			if got := used >= DecimalTightFraction; got != tc.wantHi {
				t.Errorf("used %.2f, tight = %v, want %v", used, got, tc.wantHi)
			}
			if diff := used - tc.wantPct; diff > 0.02 || diff < -0.02 {
				t.Errorf("used = %.3f, want about %.2f", used, tc.wantPct)
			}
		})
	}
}

// A column that is not a decimal has no headroom to report.
func TestDecimalHeadroomIgnoresNonDecimals(t *testing.T) {
	rs, qf := resolveFixture(t, qvdtest.Field{
		Name: "D", Type: "DATE",
		Symbols: []qvd.Symbol{qvdtest.Int(45000)}, Rows: []int{0},
	})
	defer qf.Close()
	if got := DecimalHeadroom(&rs.Columns[0], qf.Profiles[0]); got != 0 {
		t.Errorf("headroom for a date column = %v, want 0", got)
	}
}

func TestDecimalLimit(t *testing.T) {
	for _, tc := range []struct {
		precision, scale int32
		want             string
	}{
		{12, 2, "9999999999.99"},
		{4, 2, "99.99"},
		{14, 0, "99999999999999"},
	} {
		if got := scaledText(scaledLimit(tc.precision), tc.scale); got != tc.want {
			t.Errorf("limit(%d, %d) = %q, want %q", tc.precision, tc.scale, got, tc.want)
		}
	}
}

// resolveFixture builds a one-field QVD and resolves its schema.
func resolveFixture(t *testing.T, f qvdtest.Field) (*ResolvedSchema, *qvd.File) {
	t.Helper()
	path := buildFixture(t, qvdtest.Table{Name: "T", Fields: []qvdtest.Field{f}})
	qf, err := qvd.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := qf.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	rs, err := ResolveSchema(qf, &opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rs, qf
}

// A decimal's range and headroom must come from the values the column writes,
// not from the numeric profile. A dual's decimal may be built from its display
// string, so a symbol carrying the payload 1000 beside the text "999.99" is
// written as 999.99 and the profile's 1000 is a value the column never holds.
// Reading the profile reported a range and a used fraction outside the type's
// own limit.
func TestDecimalRangeFollowsDisplayStrings(t *testing.T) {
	rs, qf := resolveFixture(t, qvdtest.Field{
		Name: "Betrag", Type: "MONEY", NDec: 2, Dec: ".",
		Symbols: []qvd.Symbol{
			qvdtest.DualFloat(1000, "999.99"),
			qvdtest.DualFloat(2, "2.00"),
		},
		Rows: []int{0, 1},
	})
	defer qf.Close()

	c := &rs.Columns[0]
	if !c.DecimalFromText {
		t.Fatal("fixture no longer resolves from display strings")
	}

	got := ValueRange(c, qf.Profiles[c.SourceIndex], nil)
	const want = "2.00 .. 999.99"
	if got != want {
		t.Errorf("range = %q, want %q", got, want)
	}
	if strings.Contains(got, "1000") {
		t.Error("the numeric payload leaked into a range built from display strings")
	}

	// The widest written value is exactly the type's limit, so the fraction is
	// 1.0 and never above it.
	used := DecimalHeadroom(c, qf.Profiles[c.SourceIndex])
	if used > 1.0 {
		t.Errorf("used fraction = %v, which is outside the column's own type", used)
	}
	if used < 0.99 {
		t.Errorf("used fraction = %v, want about 1.0", used)
	}
}

// Nulls carry no value and must not widen a range.
func TestDecimalRangeIgnoresNulls(t *testing.T) {
	rs, qf := resolveFixture(t, qvdtest.Field{
		Name: "V", Type: "REAL", NDec: 2, Dec: ".",
		Symbols: []qvd.Symbol{qvdtest.Null(), qvdtest.Float(4.25), qvdtest.Float(-1.5)},
		Rows:    []int{0, 1, 2},
	})
	defer qf.Close()
	c := &rs.Columns[0]
	if got, want := ValueRange(c, qf.Profiles[c.SourceIndex], nil), "-1.50 .. 4.25"; got != want {
		t.Errorf("range = %q, want %q", got, want)
	}
}

// A column pinned to decimal by --schema may hold nothing but text symbols.
// Those have no numeric bounds, so requiring them before reaching the decimal
// branch left the column with a headroom figure and no range to read it
// against.
func TestDecimalRangeOverTextSymbols(t *testing.T) {
	path := buildFixture(t, qvdtest.Table{Name: "T", Fields: []qvdtest.Field{{
		Name: "Betrag", Type: "ASCII",
		Symbols: []qvd.Symbol{qvdtest.Str("2.00"), qvdtest.Str("999.99")},
		Rows:    []int{0, 1},
	}}})
	qf, err := qvd.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer qf.Close()
	if err := qf.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	override := &SchemaOverride{Columns: map[string]ColumnOverride{
		"Betrag": {Type: "decimal", Precision: 5, Scale: 2},
	}}
	rs, err := ResolveSchema(qf, &opts, override)
	if err != nil {
		t.Fatal(err)
	}

	c := &rs.Columns[0]
	if c.Strategy != StrategyDecimal {
		t.Fatalf("override did not pin the column to decimal, got %v", c.Strategy)
	}
	prof := qf.Profiles[c.SourceIndex]
	if _, _, ok := numericBounds(prof); ok {
		t.Fatal("fixture no longer holds text-only symbols")
	}

	got := ValueRange(c, prof, &opts)
	const want = "2.00 .. 999.99"
	if got != want {
		t.Errorf("range = %q, want %q", got, want)
	}
	// The headroom was already right; the range has to agree with it.
	if used := DecimalHeadroom(c, prof); used < 0.99 || used > 1.0 {
		t.Errorf("used fraction = %v, want about 1.0", used)
	}
}
