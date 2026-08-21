package convert

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

func TestParseNumericPromote(t *testing.T) {
	cases := map[string]NumericPromote{
		"true": PromoteFloat64, "1": PromoteFloat64, "float64": PromoteFloat64, "on": PromoteFloat64,
		"false": PromoteNone, "0": PromoteNone, "none": PromoteNone, "off": PromoteNone,
		"decimal": PromoteDecimal, "DECIMAL": PromoteDecimal, " decimal ": PromoteDecimal,
	}
	for in, want := range cases {
		got, err := ParseNumericPromote(in)
		if err != nil || got != want {
			t.Errorf("ParseNumericPromote(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseNumericPromote("maybe"); err == nil {
		t.Error("expected an error for an invalid --numeric-promote")
	}
	if PromoteNone.Enabled() || !PromoteFloat64.Enabled() || !PromoteDecimal.Enabled() {
		t.Error("Enabled() is wrong for some mode")
	}
}

func TestInferScaleFromValues(t *testing.T) {
	tests := []struct {
		name   string
		vals   []float64
		want   int32
		wantOK bool
	}{
		{"integers", []float64{1, 2, 399}, 0, true},
		{"one decimal", []float64{1.1, 2, 3}, 1, true},
		{"two decimals", []float64{1.1, 7.75, 399}, 2, true},
		{"negatives", []float64{-10.5, 3.25}, 2, true},
		{"repeating fraction", []float64{1.0 / 3}, 0, false},
		{"beyond the bound", []float64{1.0000000001}, 0, false},
		{"NaN", []float64{math.NaN()}, 0, false},
		{"infinity", []float64{math.Inf(1)}, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			syms := make([]qvd.Symbol, len(tc.vals))
			for i, v := range tc.vals {
				syms[i] = qvdtest.Float(v)
			}
			got, ok := InferScaleFromValues(syms)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (scale %d)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Errorf("scale = %d, want %d", got, tc.want)
			}
		})
	}

	// A column with no numeric symbols yields no scale.
	if _, ok := InferScaleFromValues([]qvd.Symbol{qvdtest.Str("x")}); ok {
		t.Error("a text-only column should not yield a scale")
	}
	// Integer symbols count toward the scale decision.
	got, ok := InferScaleFromValues([]qvd.Symbol{qvdtest.Int(5), qvdtest.Float(1.25)})
	if !ok || got != 2 {
		t.Errorf("mixed int+float scale = %d, %v; want 2, true", got, ok)
	}
}

// --numeric-promote=decimal turns any column carrying fractional values into an
// exact decimal, whether or not it also holds integer symbols.
func TestPromoteDecimalResolvesFloatColumns(t *testing.T) {
	tests := []struct {
		name string
		syms []qvd.Symbol
		want string
	}{
		{"mixed int and float", []qvd.Symbol{qvdtest.Int(20), qvdtest.Float(7.75)}, "decimal(4, 2)"},
		{"pure float", []qvd.Symbol{qvdtest.Float(304.7), qvdtest.Float(1.1)}, "decimal(4, 1)"},
		{"whole doubles", []qvd.Symbol{qvdtest.Float(399), qvdtest.Float(6)}, "decimal(3, 0)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := make([]int, len(tc.syms))
			for i := range rows {
				rows[i] = i
			}
			f := qvdtest.Field{Name: "Price", Type: "REAL", Symbols: tc.syms, Rows: rows}
			rs := mustResolve(t, f, func(o *Options) { o.NumericPromote = PromoteDecimal })
			c := rs.Columns[0]
			if c.Strategy != StrategyDecimal {
				t.Fatalf("strategy = %v, want StrategyDecimal", c.Strategy)
			}
			if got := c.ArrowType.String(); got != tc.want {
				t.Errorf("type = %s, want %s", got, tc.want)
			}
			if !c.DecimalFromNumeric {
				t.Error("digits should be reported as coming from numeric payloads")
			}
		})
	}
}

// Pure-integer columns gain nothing from decimal(p,0) and must stay int64.
func TestPromoteDecimalLeavesIntegerColumnsAlone(t *testing.T) {
	f := qvdtest.Field{Name: "Qty", Type: "INTEGER",
		Symbols: []qvd.Symbol{qvdtest.Int(1), qvdtest.Int(39)}, Rows: []int{0, 1}}
	rs := mustResolve(t, f, func(o *Options) { o.NumericPromote = PromoteDecimal })
	if got := rs.Columns[0].ArrowType.String(); got != "int64" {
		t.Errorf("type = %s, want int64", got)
	}
}

// Declared MONEY/FIX already resolve to decimal and must keep using the
// declared nDec rather than a value-inferred scale.
func TestPromoteDecimalDoesNotOverrideDeclaredMoney(t *testing.T) {
	f := qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 4, Dec: ".",
		Symbols: []qvd.Symbol{qvdtest.DualFloat(1.5, "1.5")}, Rows: []int{0}}
	rs := mustResolve(t, f, func(o *Options) { o.NumericPromote = PromoteDecimal })
	if got := rs.Columns[0].Decimal.Scale; got != 4 {
		t.Errorf("scale = %d, want the declared 4, not a value-inferred 1", got)
	}
}

// Decimal promotion is the default, so a price column is exact out of the box.
func TestDefaultPromotionIsDecimal(t *testing.T) {
	f := qvdtest.Field{Name: "Price", Type: "REAL",
		Symbols: []qvd.Symbol{qvdtest.Int(20), qvdtest.Float(7.75)}, Rows: []int{0, 1}}
	rs := mustResolve(t, f, nil)
	if got := rs.Columns[0].ArrowType.String(); got != "decimal(4, 2)" {
		t.Errorf("default resolved to %s, want decimal(4, 2)", got)
	}
	if DefaultOptions().NumericPromote != PromoteDecimal {
		t.Error("DefaultOptions should promote to decimal")
	}
}

// By default a column with no representable scale silently falls back to
// float64 rather than failing: the default is a preference, not a demand, and
// float64 is what such a column would have resolved to anyway.
func TestDefaultPromotionFallsBackToFloat64(t *testing.T) {
	f := qvdtest.Field{Name: "Rate", Type: "REAL",
		Symbols: []qvd.Symbol{qvdtest.Float(1.0 / 3), qvdtest.Float(2)}, Rows: []int{0, 1}}
	rs := mustResolve(t, f, nil)
	if got := rs.Columns[0].ArrowType.String(); got != "float64" {
		t.Errorf("resolved to %s, want a float64 fallback", got)
	}
	// The schema report must still say what happened.
	joined := strings.Join(rs.Notes, " ")
	if !strings.Contains(joined, "float64") || !strings.Contains(joined, "no exact scale") {
		t.Errorf("notes should explain the fallback: %q", joined)
	}
}

// A value that needs more decimals than can be inferred follows
// --decimal-strict: fail when strict, fall back to float64 when not.
func TestExplicitDecimalPromotionIsADemand(t *testing.T) {
	f := qvdtest.Field{Name: "Rate", Type: "REAL",
		Symbols: []qvd.Symbol{qvdtest.Float(1.0 / 3), qvdtest.Float(2)}, Rows: []int{0, 1}}

	// An explicit --numeric-promote=decimal is a demand, and fails on its own.
	// This must not depend on --decimal-strict, which governs a different
	// question: rounding a value that does not fit an established scale.
	_, err := resolve(t, f, func(o *Options) {
		o.NumericPromote = PromoteDecimal
		o.NumericPromoteExplicit = true
		o.DecimalStrict = false // the default; must not weaken the demand
	})
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("strict mode err = %v, want ErrSchemaPolicy", err)
	}
	for _, want := range []string{"Rate", "--schema", "--numeric-promote=true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}

	// Without the explicit flag the default is only a preference, so the
	// column quietly falls back to float64.
	rs := mustResolve(t, f, func(o *Options) {
		o.NumericPromote = PromoteDecimal
		o.NumericPromoteExplicit = false
	})
	if got := rs.Columns[0].ArrowType.String(); got != "float64" {
		t.Errorf("default fallback resolved to %s, want float64", got)
	}

	// --decimal-strict=true must not turn the default preference into a
	// failure either; explicitness alone decides.
	rs = mustResolve(t, f, func(o *Options) {
		o.NumericPromote = PromoteDecimal
		o.NumericPromoteExplicit = false
		o.DecimalStrict = true
	})
	if got := rs.Columns[0].ArrowType.String(); got != "float64" {
		t.Errorf("--decimal-strict should not affect the fallback, got %s", got)
	}
}

// The error naming the flags must still appear when promotion is disabled.
func TestPromoteNoneStillReportsBothOptions(t *testing.T) {
	f := qvdtest.Field{Name: "M", Type: "REAL",
		Symbols: []qvd.Symbol{qvdtest.Int(1), qvdtest.Float(2.5)}, Rows: []int{0, 1}}
	_, err := resolve(t, f, func(o *Options) { o.NumericPromote = PromoteNone })
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("err = %v, want ErrSchemaPolicy", err)
	}
	for _, want := range []string{"--numeric-promote=true", "--numeric-promote=decimal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should offer %q", err, want)
		}
	}
}
