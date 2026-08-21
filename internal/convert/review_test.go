package convert

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// --decimal-strict=false must round an inexact value to the declared scale.
// It must never drop it: turning a value into a null loses data silently, and
// the quality gate cannot catch it because the metrics are collected from the
// already-converted value.
func TestNonStrictDecimalRoundsInsteadOfNulling(t *testing.T) {
	f := qvdtest.Field{
		Name: "Amount", Type: "MONEY", NDec: 2, Dec: ".",
		Symbols: []qvd.Symbol{
			qvdtest.Float(1.0 / 3), // 0.333... -> 0.33
			qvdtest.Float(2.0 / 3), // 0.666... -> 0.67, rounds up
			qvdtest.Float(-2.0 / 3),
			qvdtest.Float(9.99), // already exact
		},
		Rows: []int{0, 1, 2, 3},
	}
	rs := mustResolve(t, f, func(o *Options) { o.DecimalStrict = false })
	c := rs.Columns[0]
	if c.Strategy != StrategyDecimal {
		t.Fatalf("strategy = %v, want StrategyDecimal", c.Strategy)
	}

	want := []string{"33", "67", "-67", "999"}
	for i, w := range want {
		if c.Scaled[i] == nil {
			t.Errorf("symbol %d was dropped to null instead of being rounded", i)
			continue
		}
		if got := c.Scaled[i].String(); got != w {
			t.Errorf("symbol %d scaled = %s, want %s", i, got, w)
		}
	}
}

// The same guarantee has to survive the whole pipeline: no nulls appear in a
// column whose source symbols were all non-null.
func TestNonStrictDecimalWritesNoUnexpectedNulls(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{{
		Name: "Amount", Type: "MONEY", NDec: 2, Dec: ".",
		Symbols: []qvd.Symbol{qvdtest.Float(1.0 / 3), qvdtest.Float(2.0 / 7)},
		Rows:    []int{0, 1, 0, 1},
	}}}
	in := buildFixture(t, tbl)
	out := filepath.Join(t.TempDir(), "out.parquet")

	opts := testOptions()
	opts.DecimalStrict = false
	opts.Quality = QualityFull
	_, report, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("quality gate failed: %+v", report)
	}
	if got := report.Columns[0].Source.Nulls; got != 0 {
		t.Errorf("Amount has %d nulls, want 0: inexact values must be rounded, not dropped", got)
	}
	if got := report.Columns[0].Source.NonNulls; got != 4 {
		t.Errorf("Amount has %d non-null values, want 4", got)
	}
}

// Strict mode is still strict.
func TestStrictDecimalStillFails(t *testing.T) {
	f := qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 2,
		Symbols: []qvd.Symbol{qvdtest.Float(1.0 / 3)}, Rows: []int{0}}
	_, err := resolve(t, f, func(o *Options) { o.DecimalStrict = true })
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("err = %v, want ErrSchemaPolicy", err)
	}
	if !strings.Contains(err.Error(), "--decimal-strict=false") {
		t.Errorf("error should name the flag that relaxes this: %v", err)
	}
}

func TestScaledFromTextRounding(t *testing.T) {
	tests := []struct{ text, want string }{
		{"1.234", "123"}, // rounds down
		{"1.235", "124"}, // rounds half away from zero
		{"1.239", "124"},
		{"-1.235", "-124"}, // away from zero on the negative side too
		{"1.2300", "123"},  // trailing zeros are not rounding
	}
	for _, tc := range tests {
		got, err := ScaledFromTextRounded(tc.text, 2, ".", "")
		if err != nil {
			t.Errorf("ScaledFromTextRounded(%q): %v", tc.text, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("ScaledFromTextRounded(%q) = %s, want %s", tc.text, got, tc.want)
		}
	}
}

// A timestamp pinned by --schema must carry the run's --timezone, not a
// hard-coded UTC that contradicts how the values were converted.
func TestTimestampOverrideUsesConfiguredTimezone(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	path := filepath.Join(t.TempDir(), "t.qvd")
	fld := qvdtest.Field{Name: "Seen", Type: "REAL",
		Symbols: []qvd.Symbol{qvdtest.Float(45108.5)}, Rows: []int{0}}
	if _, err := qvdtest.Build(path, qvdtest.Table{Name: "T", Fields: []qvdtest.Field{fld}}); err != nil {
		t.Fatal(err)
	}
	qf, err := qvd.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer qf.Close()
	if err := qf.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		t.Fatal(err)
	}

	opts := DefaultOptions()
	opts.Location, opts.TimezoneName = berlin, "Europe/Berlin"
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	override := &SchemaOverride{Columns: map[string]ColumnOverride{"Seen": {Type: "timestamp"}}}
	rs, err := ResolveSchema(qf, &opts, override)
	if err != nil {
		t.Fatalf("ResolveSchema: %v", err)
	}
	ts, ok := rs.Columns[0].ArrowType.(*arrow.TimestampType)
	if !ok {
		t.Fatalf("type = %s, want a timestamp type", rs.Columns[0].ArrowType)
	}
	if ts.TimeZone != "Europe/Berlin" {
		t.Errorf("timestamp timezone = %q, want Europe/Berlin: the metadata must match "+
			"the timezone the values were converted with", ts.TimeZone)
	}
}

// Pinning a text column to a date/time type must fail at schema resolution,
// not part-way through writing.
func TestDateTimeOverridesValidateSymbols(t *testing.T) {
	for _, pinned := range []string{"date32", "timestamp", "time"} {
		t.Run(pinned, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.qvd")
			fld := qvdtest.Field{Name: "V", Type: "ASCII",
				Symbols: []qvd.Symbol{qvdtest.Str("not a date")}, Rows: []int{0}}
			if _, err := qvdtest.Build(path, qvdtest.Table{Name: "T", Fields: []qvdtest.Field{fld}}); err != nil {
				t.Fatal(err)
			}
			qf, err := qvd.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer qf.Close()
			if err := qf.ReadSymbols(qvd.UnknownSymbolError); err != nil {
				t.Fatal(err)
			}
			opts := DefaultOptions()
			opts.Location = utc()
			if err := opts.Validate(); err != nil {
				t.Fatal(err)
			}
			override := &SchemaOverride{Columns: map[string]ColumnOverride{"V": {Type: pinned}}}
			_, err = ResolveSchema(qf, &opts, override)
			if !errors.Is(err, ErrSchemaPolicy) {
				t.Fatalf("pinning a string column to %s gave %v, want ErrSchemaPolicy", pinned, err)
			}
		})
	}
}

// A generated "${name}__text" column must not silently collide with a real
// source column of the same name.
func TestDualTextColumnNameCollisionIsRejected(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Qty", Type: "INTEGER", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.DualInt(1, "one")}},
		{Name: "Qty__text", Type: "ASCII", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Str("collides")}},
	}}
	in := buildFixture(t, tbl)
	out := filepath.Join(t.TempDir(), "out.parquet")

	opts := testOptions()
	opts.Dual = DualColumns
	_, _, err := Run(context.Background(), in, out, &opts, nil)
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("err = %v, want ErrSchemaPolicy for the duplicate output name", err)
	}
	if !strings.Contains(err.Error(), "Qty__text") {
		t.Errorf("error should name the colliding column: %v", err)
	}
}

// The default --dual=auto generates a companion column for an informative
// dual, so it collides here just as --dual=columns does. --dual=numeric drops
// the display side and converts cleanly.
func TestDualTextNameCollidesUnderAutoButNotNumeric(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Qty", Type: "INTEGER", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.DualInt(1, "one")}}, // "one" is informative
		{Name: "Qty__text", Type: "ASCII", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Str("collides")}},
	}}
	in := buildFixture(t, tbl)

	opts := testOptions() // --dual=auto
	_, _, err := Run(context.Background(), in, filepath.Join(t.TempDir(), "a.parquet"), &opts, nil)
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("auto err = %v, want ErrSchemaPolicy for the duplicate name", err)
	}
	if !strings.Contains(err.Error(), "--dual=numeric") {
		t.Errorf("error should offer the flag that resolves it: %v", err)
	}

	opts.Dual = DualNumeric
	if _, _, err := Run(context.Background(), in, filepath.Join(t.TempDir(), "b.parquet"), &opts, nil); err != nil {
		t.Fatalf("--dual=numeric should convert cleanly: %v", err)
	}
}

// A truncated record area must be reported, not silently short-read.
func TestShortReadIsReported(t *testing.T) {
	conv, qf := parallelFixture(t, 2000)
	// Claim more rows than the file actually holds.
	qf.NoOfRecords += 5000

	_, err := conv.Run(context.Background(), &collectSink{}, nil)
	if err == nil {
		t.Fatal("reading past the end of the record area should fail")
	}
	if !errors.Is(err, ErrInput) {
		t.Errorf("err = %v, want ErrInput", err)
	}
	if !strings.Contains(err.Error(), "bytes") {
		t.Errorf("error should report the short read: %v", err)
	}
}

// A date/time override must be validated by running the real conversion, not
// just by checking the symbol kind. NaN and out-of-range serials are numeric
// but still unconvertible, and must fail at schema resolution.
func TestDateTimeOverridesRejectUnconvertibleNumerics(t *testing.T) {
	tests := []struct {
		name   string
		pinned string
		sym    qvd.Symbol
	}{
		{"NaN date", "date32", qvdtest.Float(math.NaN())},
		{"NaN timestamp", "timestamp", qvdtest.Float(math.NaN())},
		{"NaN time", "time", qvdtest.Float(math.NaN())},
		{"huge serial day", "date32", qvdtest.Float(1e30)},
		{"huge timestamp", "timestamp", qvdtest.Float(1e30)},
		{"infinite time", "time", qvdtest.Float(math.Inf(1))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.qvd")
			fld := qvdtest.Field{Name: "V", Type: "REAL",
				Symbols: []qvd.Symbol{qvdtest.Float(45000), tc.sym}, Rows: []int{0, 1}}
			if _, err := qvdtest.Build(path, qvdtest.Table{Name: "T", Fields: []qvdtest.Field{fld}}); err != nil {
				t.Fatal(err)
			}
			qf, err := qvd.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer qf.Close()
			if err := qf.ReadSymbols(qvd.UnknownSymbolError); err != nil {
				t.Fatal(err)
			}
			opts := DefaultOptions()
			opts.Location = utc()
			if err := opts.Validate(); err != nil {
				t.Fatal(err)
			}
			override := &SchemaOverride{Columns: map[string]ColumnOverride{"V": {Type: tc.pinned}}}
			_, err = ResolveSchema(qf, &opts, override)
			if !errors.Is(err, ErrSchemaPolicy) {
				t.Fatalf("pinning %s to %s gave %v, want ErrSchemaPolicy at schema resolution",
					tc.name, tc.pinned, err)
			}
			if !strings.Contains(err.Error(), "V") {
				t.Errorf("error should name the column: %v", err)
			}
		})
	}
}

// A valid date/time override still resolves, and nulls are skipped.
func TestDateTimeOverrideAcceptsConvertibleValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.qvd")
	fld := qvdtest.Field{Name: "D", Type: "REAL",
		Symbols: []qvd.Symbol{qvdtest.Float(45000.5), qvdtest.Null()}, Rows: []int{0, -1}}
	if _, err := qvdtest.Build(path, qvdtest.Table{Name: "T", Fields: []qvdtest.Field{fld}}); err != nil {
		t.Fatal(err)
	}
	qf, err := qvd.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer qf.Close()
	if err := qf.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	opts.Location = utc()
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	override := &SchemaOverride{Columns: map[string]ColumnOverride{"D": {Type: "date32"}}}
	rs, err := ResolveSchema(qf, &opts, override)
	if err != nil {
		t.Fatalf("a convertible date column should resolve: %v", err)
	}
	if rs.Columns[0].Strategy != StrategyDate32 {
		t.Errorf("strategy = %v, want StrategyDate32", rs.Columns[0].Strategy)
	}
}
