package convert

import (
	"errors"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
	"math/big"
	"strings"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/qvd"
)

func TestScaledFromText(t *testing.T) {
	tests := []struct {
		text    string
		scale   int32
		decSep  string
		thouSep string
		want    string
		wantErr bool
	}{
		{"123.45", 2, ".", ",", "12345", false},
		{"1.234,56", 2, ",", ".", "123456", false},
		{"-10,50", 2, ",", ".", "-1050", false},
		{"1 234,50", 2, ",", " ", "123450", false},
		{"0", 2, ".", "", "0", false},
		{".5", 2, ".", "", "50", false},
		{"(12.34)", 2, ".", "", "-1234", false},
		{"+7", 3, ".", "", "7000", false},
		{"1.2300", 2, ".", "", "123", false}, // trailing zeros are droppable
		{"1.234", 2, ".", "", "", true},      // a real third decimal is not
		{"12,3 EUR", 2, ",", ".", "", true},  // suffixes are rejected
		{"", 2, ".", "", "", true},
		{"abc", 2, ".", "", "", true},
	}
	for _, tc := range tests {
		got, err := ScaledFromText(tc.text, tc.scale, tc.decSep, tc.thouSep)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ScaledFromText(%q) = %v, want an error", tc.text, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ScaledFromText(%q): %v", tc.text, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("ScaledFromText(%q, scale=%d) = %s, want %s", tc.text, tc.scale, got, tc.want)
		}
	}
}

func TestScaledFromTextTooManyDecimalsIsInexact(t *testing.T) {
	_, err := ScaledFromText("1.239", 2, ".", "")
	if !errors.Is(err, ErrDecimalInexact) {
		t.Fatalf("err = %v, want ErrDecimalInexact", err)
	}
}

func TestScaledFromFloat(t *testing.T) {
	tests := []struct {
		v       float64
		scale   int32
		want    string
		wantErr bool
	}{
		{123.45, 2, "12345", false},
		{-10.5, 2, "-1050", false},
		{0.1, 2, "10", false},  // 0.1 is not exact in binary; noise is rounded away
		{1.0 / 3, 2, "", true}, // genuinely more decimals than the scale
		{1.239, 2, "", true},
		{1e15, 2, "100000000000000000", false},
		{0, 4, "0", false},
	}
	for _, tc := range tests {
		got, err := ScaledFromFloat(tc.v, tc.scale)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ScaledFromFloat(%v, %d) = %v, want an error", tc.v, tc.scale, got)
			} else if !errors.Is(err, ErrDecimalInexact) {
				t.Errorf("ScaledFromFloat(%v): err = %v, want ErrDecimalInexact", tc.v, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ScaledFromFloat(%v, %d): %v", tc.v, tc.scale, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("ScaledFromFloat(%v, %d) = %s, want %s", tc.v, tc.scale, got, tc.want)
		}
	}
}

func TestDecimalExtractorPrefersText(t *testing.T) {
	// The binary side lost the intent; the display string kept it.
	ex := &DecimalExtractor{Scale: 2, Source: DecimalAuto, Strict: true, DecSep: ","}
	s := qvd.Symbol{Kind: qvd.SymbolDualFloatString, Float: 1.0 / 3, Text: "0,33"}
	got, err := ex.Scaled(s)
	if err != nil {
		t.Fatalf("Scaled: %v", err)
	}
	if got.String() != "33" {
		t.Errorf("scaled = %s, want 33", got)
	}
	if !ex.UsedText || ex.UsedNumeric {
		t.Errorf("UsedText=%v UsedNumeric=%v, want text only", ex.UsedText, ex.UsedNumeric)
	}
}

func TestDecimalExtractorFallsBackToNumeric(t *testing.T) {
	ex := &DecimalExtractor{Scale: 2, Source: DecimalAuto, Strict: true}
	got, err := ex.Scaled(qvd.Symbol{Kind: qvd.SymbolFloat, Float: 9.99})
	if err != nil {
		t.Fatalf("Scaled: %v", err)
	}
	if got.String() != "999" {
		t.Errorf("scaled = %s, want 999", got)
	}
	if !ex.UsedNumeric {
		t.Error("UsedNumeric should be set")
	}
}

func TestDecimalExtractorIntSymbol(t *testing.T) {
	ex := &DecimalExtractor{Scale: 3, Source: DecimalNumeric, Strict: true}
	got, err := ex.Scaled(qvd.Symbol{Kind: qvd.SymbolInt, Int: -12})
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "-12000" {
		t.Errorf("scaled = %s, want -12000", got)
	}
}

func TestDecimalExtractorNull(t *testing.T) {
	ex := &DecimalExtractor{Scale: 2, Source: DecimalAuto, Strict: true}
	got, err := ex.Scaled(qvd.Symbol{Kind: qvd.SymbolNull})
	if err != nil || got != nil {
		t.Errorf("null symbol -> %v, %v; want nil, nil", got, err)
	}
}

func TestDecimalExtractorTextSourceRequiresString(t *testing.T) {
	ex := &DecimalExtractor{Scale: 2, Source: DecimalText, Strict: true}
	if _, err := ex.Scaled(qvd.Symbol{Kind: qvd.SymbolFloat, Float: 1.5}); err == nil {
		t.Fatal("--decimal-source=text should reject a symbol without a display string")
	}
}

func TestResolveDecimalSpecPrecision(t *testing.T) {
	symbols := []qvd.Symbol{
		{Kind: qvd.SymbolFloat, Float: -99999.99},
		{Kind: qvd.SymbolFloat, Float: 1.25},
		{Kind: qvd.SymbolNull},
	}
	ex := &DecimalExtractor{Scale: 2, Source: DecimalNumeric, Strict: true}
	spec, scaled, err := ResolveDecimalSpec("Amount", symbols, ex)
	if err != nil {
		t.Fatalf("ResolveDecimalSpec: %v", err)
	}
	// -9999999 has 7 digits; the sign does not consume precision.
	if spec.Precision != 7 || spec.Scale != 2 {
		t.Errorf("spec = %v, want decimal(7,2)", spec)
	}
	if scaled[0].String() != "-9999999" || scaled[2] != nil {
		t.Errorf("scaled = %v", scaled)
	}
}

func TestResolveDecimalSpecFailsOnInexact(t *testing.T) {
	symbols := []qvd.Symbol{{Kind: qvd.SymbolFloat, Float: 1.0 / 3}}
	ex := &DecimalExtractor{Scale: 2, Source: DecimalNumeric, Strict: true}
	_, _, err := ResolveDecimalSpec("Rate", symbols, ex)
	if err == nil {
		t.Fatal("expected a strict-mode failure")
	}
	if !errors.Is(err, ErrDecimalInexact) {
		t.Errorf("err = %v, want ErrDecimalInexact", err)
	}
}

func TestResolveDecimalSpecPrecisionOverflow(t *testing.T) {
	huge, _ := new(big.Float).SetString("1e40")
	f, _ := huge.Float64()
	symbols := []qvd.Symbol{{Kind: qvd.SymbolFloat, Float: f}}
	ex := &DecimalExtractor{Scale: 2, Source: DecimalNumeric, Strict: true}
	if _, _, err := ResolveDecimalSpec("Big", symbols, ex); err == nil {
		t.Fatal("expected a precision overflow error")
	}
}

func TestInferScaleFromSymbols(t *testing.T) {
	symbols := []qvd.Symbol{
		{Kind: qvd.SymbolDualFloatString, Text: "1,5"},
		{Kind: qvd.SymbolDualFloatString, Text: "10,125"},
		{Kind: qvd.SymbolDualFloatString, Text: "3"},
	}
	scale, ok := InferScaleFromSymbols(symbols, ",")
	if !ok || scale != 3 {
		t.Errorf("scale = %d, ok = %v; want 3, true", scale, ok)
	}
	if _, ok := InferScaleFromSymbols([]qvd.Symbol{{Kind: qvd.SymbolFloat}}, "."); ok {
		t.Error("a column with no display strings should not yield a scale")
	}
}

func TestFormatScaled(t *testing.T) {
	tests := []struct {
		v     string
		scale int32
		want  string
	}{
		{"12345", 2, "123.45"},
		{"-1050", 2, "-10.50"},
		{"5", 3, "0.005"},
		{"-5", 3, "-0.005"},
		{"0", 2, "0.00"},
		{"999", 0, "999"},
	}
	for _, tc := range tests {
		v, _ := new(big.Int).SetString(tc.v, 10)
		if got := FormatScaled(v, tc.scale); got != tc.want {
			t.Errorf("FormatScaled(%s, %d) = %q, want %q", tc.v, tc.scale, got, tc.want)
		}
	}
	if got := FormatScaled(nil, 2); got != "" {
		t.Errorf("FormatScaled(nil) = %q, want empty", got)
	}
}

// --decimal-strict reports several offending values, not just the first. One
// example rarely settles whether a column holds genuine extra decimals or
// float64 representation error, and stopping at the first sent a reader to a
// Qlik script to find the rest.
func TestDecimalStrictReportsSeveralExamples(t *testing.T) {
	// Large magnitudes where a third decimal exceeds what a double holds, so
	// every one of them fails at the inferred scale.
	var syms []qvd.Symbol
	for _, v := range []float64{
		8115022364.865, 7115022364.865, 6115022364.865,
		5115022364.865, 4115022364.865,
	} {
		syms = append(syms, qvdtest.Float(v))
	}
	syms = append(syms, qvdtest.Float(12.125))

	ex := &DecimalExtractor{Scale: 3, Strict: true, Source: DecimalNumeric}
	_, _, err := ResolveDecimalSpec("Preisdifferenzen", syms, ex)
	if err == nil {
		t.Fatal("strict mode accepted values it cannot hold exactly")
	}
	if !errors.Is(err, ErrDecimalInexact) {
		t.Fatalf("error does not wrap ErrDecimalInexact: %v", err)
	}
	msg := err.Error()

	// The total, so the reader knows how much was not shown.
	if !strings.Contains(msg, "has 5 such value(s)") {
		t.Errorf("message does not report the total:\n%s", msg)
	}
	if !strings.Contains(msg, "the first 3 shown") {
		t.Errorf("message does not say the list is truncated:\n%s", msg)
	}
	if got := strings.Count(msg, "symbol "); got != StrictExamples {
		t.Errorf("listed %d examples, want %d:\n%s", got, StrictExamples, msg)
	}

	// The value as stored, which is what separates float64 noise from a real
	// third decimal in the source.
	if !strings.Contains(msg, "8115022364.864999771") {
		t.Errorf("message does not show what the double actually holds:\n%s", msg)
	}
	// Never scientific notation: it hides the decimals in question.
	if strings.Contains(msg, "e+09") {
		t.Errorf("value rendered in scientific notation:\n%s", msg)
	}
	// A pure float has no display string, so quoting an empty one says nothing.
	if strings.Contains(msg, `float ""`) {
		t.Errorf("empty display string quoted for a pure float:\n%s", msg)
	}
	// The sentinel's own sentence must appear once, not on every example line.
	if got := strings.Count(msg, ErrDecimalInexact.Error()); got != 1 {
		t.Errorf("sentinel text repeated %d times:\n%s", got, msg)
	}
}

// Fewer offenders than the cap are all listed, with no truncation note.
func TestDecimalStrictListsAllWhenFewerThanCap(t *testing.T) {
	syms := []qvd.Symbol{qvdtest.Float(8115022364.865), qvdtest.Float(12.125)}
	ex := &DecimalExtractor{Scale: 3, Strict: true, Source: DecimalNumeric}
	_, _, err := ResolveDecimalSpec("V", syms, ex)
	if err == nil {
		t.Fatal("strict mode accepted a value it cannot hold exactly")
	}
	msg := err.Error()
	if !strings.Contains(msg, "has 1 such value(s)") {
		t.Errorf("wrong total:\n%s", msg)
	}
	if strings.Contains(msg, "shown") {
		t.Errorf("claimed truncation with nothing truncated:\n%s", msg)
	}
}

func TestScaleStep(t *testing.T) {
	for _, tc := range []struct {
		scale int32
		want  string
	}{{0, "1"}, {1, "0.1"}, {2, "0.01"}, {3, "0.001"}} {
		if got := scaleStep(tc.scale); got != tc.want {
			t.Errorf("scaleStep(%d) = %q, want %q", tc.scale, got, tc.want)
		}
	}
}
