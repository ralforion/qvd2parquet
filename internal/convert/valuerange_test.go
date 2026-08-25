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
		want             float64
	}{
		{12, 2, 9999999999.99},
		{4, 2, 99.99},
		{14, 0, 99999999999999},
	} {
		if got := decimalLimit(tc.precision, tc.scale); got != tc.want {
			t.Errorf("decimalLimit(%d, %d) = %v, want %v", tc.precision, tc.scale, got, tc.want)
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
