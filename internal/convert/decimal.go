package convert

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// maxDecimal128Precision is the largest precision Arrow decimal128 can hold.
const maxDecimal128Precision = 38

// ErrDecimalInexact reports a value that cannot be represented exactly at the
// column's declared scale.
var ErrDecimalInexact = errors.New("value is not exact at the declared decimal scale")

// errNonFinite marks a NaN or infinite payload, which becomes a null rather
// than failing the column.
var errNonFinite = errors.New("value is not finite")

// DecimalSpec is a resolved Parquet decimal type.
type DecimalSpec struct {
	Precision int32 `json:"precision"`
	Scale     int32 `json:"scale"`
}

func (s DecimalSpec) String() string { return fmt.Sprintf("decimal(%d,%d)", s.Precision, s.Scale) }

// decimalTolerance bounds the binary floating-point representation noise that
// may be rounded away when scaling a double. It is small enough that a value
// carrying more decimal places than the declared scale is still rejected.
const decimalTolerance = 1e-6

// pow10 returns 10^n as a big.Int, for n >= 0.
func pow10(n int32) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// decimalDigits returns the number of decimal digits in |v|, at least 1.
func decimalDigits(v *big.Int) int32 {
	a := new(big.Int).Abs(v)
	if a.Sign() == 0 {
		return 1
	}
	return int32(len(a.String()))
}

// ScaledFromText parses a display string into an integer scaled by 10^scale,
// failing if the string carries more significant decimals than the scale
// allows. decSep is the declared decimal separator ("." when empty) and
// thouSep the declared thousands separator, which is stripped when present.
func ScaledFromText(text string, scale int32, decSep, thouSep string) (*big.Int, error) {
	return scaledFromText(text, scale, decSep, thouSep, false)
}

// ScaledFromTextRounded is ScaledFromText but rounds excess decimals
// half-away-from-zero instead of failing. It is used when --decimal-strict is
// disabled.
func ScaledFromTextRounded(text string, scale int32, decSep, thouSep string) (*big.Int, error) {
	return scaledFromText(text, scale, decSep, thouSep, true)
}

func scaledFromText(text string, scale int32, decSep, thouSep string, round bool) (*big.Int, error) {
	s := strings.TrimSpace(text)
	if s == "" {
		return nil, fmt.Errorf("empty display string")
	}
	if decSep == "" {
		decSep = "."
	}
	// Strip the thousands separator, but never one that equals the decimal
	// separator or a sign/digit character.
	if thouSep != "" && thouSep != decSep {
		s = strings.ReplaceAll(s, thouSep, "")
	}
	s = strings.ReplaceAll(s, " ", "") // non-breaking space grouping
	s = strings.ReplaceAll(s, " ", "")

	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg, s = true, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	// Accounting-style negatives.
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg, s = true, s[1:len(s)-1]
	}
	// Trailing currency or unit suffixes are not decimal digits; reject them
	// rather than guessing.
	intPart, fracPart := s, ""
	if i := strings.Index(s, decSep); i >= 0 {
		intPart, fracPart = s[:i], s[i+len(decSep):]
	}
	if intPart == "" {
		intPart = "0"
	}
	if !allDigits(intPart) || !allDigits(fracPart) {
		return nil, fmt.Errorf("%q is not a plain decimal number", text)
	}
	roundUp := false
	if int32(len(fracPart)) > scale {
		// More decimals than the declared scale: trailing zeros are harmless,
		// significant digits are not.
		extra := fracPart[scale:]
		if strings.Trim(extra, "0") != "" {
			if !round {
				return nil, fmt.Errorf("%w: %q has %d decimals, scale is %d",
					ErrDecimalInexact, text, len(fracPart), scale)
			}
			// Round half away from zero on the first dropped digit.
			roundUp = extra[0] >= '5'
		}
		fracPart = fracPart[:scale]
	}
	for int32(len(fracPart)) < scale {
		fracPart += "0"
	}
	v, ok := new(big.Int).SetString(intPart+fracPart, 10)
	if !ok {
		return nil, fmt.Errorf("%q is not a plain decimal number", text)
	}
	if roundUp {
		v.Add(v, big.NewInt(1))
	}
	if neg {
		v.Neg(v)
	}
	return v, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ScaledFromFloat converts a binary double to an integer scaled by 10^scale,
// failing when the value carries more precision than the scale allows.
func ScaledFromFloat(v float64, scale int32) (*big.Int, error) {
	return scaledFromFloat(v, scale, false)
}

// ScaledFromFloatRounded is ScaledFromFloat but rounds a value carrying more
// precision than the scale allows instead of failing. It is used when
// --decimal-strict is disabled.
func ScaledFromFloatRounded(v float64, scale int32) (*big.Int, error) {
	return scaledFromFloat(v, scale, true)
}

func scaledFromFloat(v float64, scale int32, round bool) (*big.Int, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, fmt.Errorf("%w: %v is not finite", ErrDecimalInexact, v)
	}
	scaled := v * math.Pow(10, float64(scale))
	if math.Abs(scaled) >= 1e18 {
		// Beyond float64's exact integer range; go through the decimal text
		// form, which is exact for any finite double.
		return scaledFromFloatBig(v, scale, round)
	}
	rounded := math.Round(scaled)
	if !round && math.Abs(scaled-rounded) > decimalTolerance {
		return nil, fmt.Errorf("%w: %v scaled by 10^%d is %v", ErrDecimalInexact, v, scale, scaled)
	}
	return big.NewInt(int64(rounded)), nil
}

// scaledFromFloatBig scales through big.Float, which represents any finite
// double exactly.
func scaledFromFloatBig(v float64, scale int32, round bool) (*big.Int, error) {
	bf := new(big.Float).SetPrec(200).SetFloat64(v)
	bf.Mul(bf, new(big.Float).SetPrec(200).SetInt(pow10(scale)))
	i, acc := bf.Int(nil)
	if acc == big.Exact {
		return i, nil
	}
	// Allow rounding away representation noise only.
	frac := new(big.Float).SetPrec(200).Sub(bf, new(big.Float).SetPrec(200).SetInt(i))
	f, _ := frac.Float64()
	if math.Abs(f) <= decimalTolerance {
		return i, nil
	}
	if math.Abs(f) >= 1-decimalTolerance {
		if f > 0 {
			return i.Add(i, big.NewInt(1)), nil
		}
		return i.Sub(i, big.NewInt(1)), nil
	}
	if round {
		if f >= 0.5 {
			return i.Add(i, big.NewInt(1)), nil
		}
		if f <= -0.5 {
			return i.Sub(i, big.NewInt(1)), nil
		}
		return i, nil
	}
	return nil, fmt.Errorf("%w: %v cannot be scaled by 10^%d exactly", ErrDecimalInexact, v, scale)
}

// DecimalExtractor converts the symbols of one column into scaled integers
// according to the configured decimal source.
type DecimalExtractor struct {
	Scale   int32
	Source  DecimalSource
	Strict  bool
	DecSep  string
	ThouSep string

	// UsedText and UsedNumeric record which payloads actually supplied digits,
	// for the schema report.
	UsedText    bool
	UsedNumeric bool
	// Rounded counts values that had to be rounded to the declared scale,
	// which is only possible when Strict is false.
	Rounded int64
	// NonFinite counts NaN and infinite payloads, which are written as null.
	NonFinite int64
}

// Scaled converts one symbol. It returns (nil, nil) for a null symbol.
func (e *DecimalExtractor) Scaled(s qvd.Symbol) (*big.Int, error) {
	if s.Kind == qvd.SymbolNull {
		return nil, nil
	}
	// With --decimal-strict=false a value carrying more decimals than the
	// declared scale is rounded to that scale. It is never dropped: silently
	// turning an inexact value into a null would lose data, and no later check
	// could recover it because the metrics describe the converted value.
	tryText := func() (*big.Int, error) {
		if !s.Kind.HasText() || strings.TrimSpace(s.Text) == "" {
			return nil, fmt.Errorf("symbol has no display string")
		}
		if e.Strict {
			return ScaledFromText(s.Text, e.Scale, e.DecSep, e.ThouSep)
		}
		v, err := ScaledFromTextRounded(s.Text, e.Scale, e.DecSep, e.ThouSep)
		if err == nil {
			if _, strict := ScaledFromText(s.Text, e.Scale, e.DecSep, e.ThouSep); strict != nil {
				e.Rounded++
			}
		}
		return v, err
	}
	tryNumeric := func() (*big.Int, error) {
		n, ok := s.Numeric()
		if !ok {
			return nil, fmt.Errorf("symbol has no numeric payload")
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			// Not a value a decimal can hold, and nothing is lost by writing
			// null. errNonFinite is recognized by ResolveDecimalSpec.
			return nil, errNonFinite
		}
		if s.Kind == qvd.SymbolInt || s.Kind == qvd.SymbolDualIntString {
			return new(big.Int).Mul(big.NewInt(s.Int), pow10(e.Scale)), nil
		}
		if e.Strict {
			return ScaledFromFloat(n, e.Scale)
		}
		v, err := ScaledFromFloatRounded(n, e.Scale)
		if err == nil {
			if _, strict := ScaledFromFloat(n, e.Scale); strict != nil {
				e.Rounded++
			}
		}
		return v, err
	}

	switch e.Source {
	case DecimalText:
		v, err := tryText()
		if err != nil {
			return nil, err
		}
		e.UsedText = true
		return v, nil

	case DecimalNumeric:
		v, err := tryNumeric()
		if err != nil {
			return nil, err
		}
		e.UsedNumeric = true
		return v, nil

	default: // DecimalAuto: the display string preserves decimal intent best.
		if v, err := tryText(); err == nil {
			e.UsedText = true
			return v, nil
		} else if errors.Is(err, ErrDecimalInexact) && e.Strict {
			// The display string itself carries too many decimals; the numeric
			// side cannot be more faithful.
			return nil, err
		}
		v, err := tryNumeric()
		if err != nil {
			return nil, err
		}
		e.UsedNumeric = true
		return v, nil
	}
}

// InferScaleFromSymbols derives a decimal scale from the display strings of a
// column when NumberFormat/nDec is absent.
func InferScaleFromSymbols(symbols []qvd.Symbol, decSep string) (int32, bool) {
	if decSep == "" {
		decSep = "."
	}
	var maxScale int32
	seen := false
	for _, s := range symbols {
		if !s.Kind.HasText() {
			continue
		}
		t := strings.TrimSpace(s.Text)
		i := strings.Index(t, decSep)
		if i < 0 {
			if allDigits(strings.TrimLeft(t, "+-")) && t != "" {
				seen = true
			}
			continue
		}
		frac := strings.TrimRight(t[i+len(decSep):], "0")
		if !allDigits(t[i+len(decSep):]) {
			continue
		}
		seen = true
		if n := int32(len(frac)); n > maxScale {
			maxScale = n
		}
	}
	return maxScale, seen
}

// MaxInferredDecimalScale bounds the scale that InferScaleFromValues will
// derive. Business data rarely needs more, and a large inferred scale would
// consume the decimal128 precision budget for no benefit. A column needing
// more must be pinned with --schema.
const MaxInferredDecimalScale = 9

// InferScaleFromValues derives the smallest scale at which every numeric
// symbol is exactly representable, by inspecting each value's shortest
// round-tripping decimal form. It reports false when no scale up to
// MaxInferredDecimalScale suffices, or when the column has no numeric values.
//
// This is used for --numeric-promote=decimal, where the QVD declares a plain
// REAL and so carries no trustworthy NumberFormat/nDec.
func InferScaleFromValues(symbols []qvd.Symbol) (int32, bool) {
	var maxScale int32
	seen := false
	for _, s := range symbols {
		v, ok := s.Numeric()
		if !ok {
			continue
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		seen = true
		n, ok := decimalsNeeded(v)
		if !ok || n > MaxInferredDecimalScale {
			return 0, false
		}
		if n > maxScale {
			maxScale = n
		}
	}
	return maxScale, seen
}

// decimalsNeeded returns how many decimal places the shortest representation
// of v uses. Exponent forms are rejected, since they indicate a magnitude that
// a fixed scale should not be guessed for.
func decimalsNeeded(v float64) (int32, bool) {
	str := strconv.FormatFloat(v, 'f', -1, 64)
	if strings.ContainsAny(str, "eE") {
		return 0, false
	}
	i := strings.IndexByte(str, '.')
	if i < 0 {
		return 0, true
	}
	return int32(len(str) - i - 1), true
}

// ResolveDecimalSpec scales every symbol of a column to derive the precision
// needed alongside the given scale.
func ResolveDecimalSpec(colName string, symbols []qvd.Symbol, ex *DecimalExtractor) (DecimalSpec, []*big.Int, error) {
	scaled := make([]*big.Int, len(symbols))
	var maxDigits int32 = 1
	for i, s := range symbols {
		v, err := ex.Scaled(s)
		if errors.Is(err, errNonFinite) {
			ex.NonFinite++
			continue // leaves scaled[i] nil, which is written as null
		}
		if err != nil {
			return DecimalSpec{}, nil, fmt.Errorf(
				"column %q symbol %d (%v %q): %w; relax with --decimal-strict=false to round to the declared scale, or pin the column with --schema",
				colName, i, s.Kind, s.Text, err)
		}
		scaled[i] = v
		if v != nil {
			if d := decimalDigits(v); d > maxDigits {
				maxDigits = d
			}
		}
	}
	spec := DecimalSpec{Precision: maxDigits, Scale: ex.Scale}
	if spec.Precision < spec.Scale {
		spec.Precision = spec.Scale
	}
	if spec.Precision > maxDecimal128Precision {
		return DecimalSpec{}, nil, fmt.Errorf(
			"column %q needs decimal precision %d at scale %d, more than decimal128 supports (%d); pin a narrower type with --schema",
			colName, spec.Precision, spec.Scale, maxDecimal128Precision)
	}
	return spec, scaled, nil
}

// FormatScaled renders a scaled integer as a decimal string with the given scale.
func FormatScaled(v *big.Int, scale int32) string {
	if v == nil {
		return ""
	}
	if scale == 0 {
		return v.String()
	}
	neg := v.Sign() < 0
	digits := new(big.Int).Abs(v).String()
	for int32(len(digits)) <= scale {
		digits = "0" + digits
	}
	cut := int32(len(digits)) - scale
	out := digits[:cut] + "." + digits[cut:]
	if neg {
		out = "-" + out
	}
	return out
}
