package convert

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// ValueRange renders a column's lowest and highest observed values in the type
// the column will be written as. A QVD stores a date as a serial day number,
// so a profile reports WADAT as 38365..411241 and a goods-issue date in the
// year 3025 reads as an ordinary integer. Rendered as dates the same range
// reads 2005-01-13 .. 3025-12-08, which is the point of showing it at all.
//
// It returns an empty string for columns whose range carries no information,
// such as text, and for any value the conversion itself could not represent.
func ValueRange(c *ResolvedColumn, p *qvd.ColumnProfile, opts *Options) string {
	if p == nil {
		return ""
	}
	lo, hi, ok := numericBounds(p)
	if !ok {
		return ""
	}
	loc := time.UTC
	if opts != nil && opts.Location != nil {
		loc = opts.Location
	}

	switch c.Strategy {
	case StrategyDate32:
		a, aok := qvd.QlikDaysToDate32(lo)
		b, bok := qvd.QlikDaysToDate32(hi)
		if !aok || !bok {
			return ""
		}
		return rangeText(dateText(a), dateText(b))

	case StrategyTimestampMicros:
		a, aok := qvd.QlikDaysToTimestampMicros(lo, loc)
		b, bok := qvd.QlikDaysToTimestampMicros(hi, loc)
		if !aok || !bok {
			return ""
		}
		return rangeText(stampText(a), stampText(b))

	case StrategyTimeMillis:
		a, aok := qvd.QlikFractionToTimeMillis(lo)
		b, bok := qvd.QlikFractionToTimeMillis(hi)
		if !aok || !bok {
			return ""
		}
		return rangeText(clockText(a), clockText(b))

	case StrategyInt64:
		return rangeText(fmt.Sprintf("%d", int64(lo)), fmt.Sprintf("%d", int64(hi)))

	case StrategyDecimal:
		s := int(c.Decimal.Scale)
		return rangeText(strconvFixed(lo, s), strconvFixed(hi, s))

	case StrategyFloat64:
		return rangeText(floatText(lo), floatText(hi))
	}
	return ""
}

// numericBounds spans both the integer and floating symbols of a column, since
// a Qlik field routinely carries both and the extreme may sit on either side.
func numericBounds(p *qvd.ColumnProfile) (lo, hi float64, ok bool) {
	hasInt := p.Ints > 0 || p.DualInts > 0
	hasFloat := p.Floats > 0 || p.DualFloats > 0
	switch {
	case hasInt && hasFloat:
		return math.Min(float64(p.MinInt), p.MinFloat),
			math.Max(float64(p.MaxInt), p.MaxFloat), true
	case hasInt:
		return float64(p.MinInt), float64(p.MaxInt), true
	case hasFloat:
		return p.MinFloat, p.MaxFloat, true
	}
	return 0, 0, false
}

func rangeText(lo, hi string) string {
	if lo == hi {
		return lo
	}
	return lo + " .. " + hi
}

func dateText(days int32) string {
	return time.Unix(int64(days)*86400, 0).UTC().Format("2006-01-02")
}

func stampText(micros int64) string {
	t := time.UnixMicro(micros).UTC()
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02 15:04:05")
	}
	return strings.TrimRight(t.Format("2006-01-02 15:04:05.000000"), "0")
}

func clockText(millis int32) string {
	d := time.Duration(millis) * time.Millisecond
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	s := int(d/time.Second) % 60
	if ms := int(d/time.Millisecond) % 1000; ms != 0 {
		return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func strconvFixed(v float64, scale int) string {
	return fmt.Sprintf("%.*f", scale, v)
}

func floatText(v float64) string {
	return fmt.Sprintf("%g", v)
}

// DecimalTightFraction is the share of a decimal type's range above which the
// column is worth reporting. Chosen so that a column reaching 8,115,022,364.86
// in a decimal(12,2) is named while one reaching 24,560,000 in a decimal(11,3)
// is not.
const DecimalTightFraction = 0.8

// DecimalHeadroom reports how much of a decimal column's range its widest
// observed value already occupies, as a fraction of the type's limit.
//
// The precision is inferred from the values, so every decimal column fits its
// data exactly by construction and "does it fit" is always yes. What differs,
// and what a future load turns into a failure, is how much room is left inside
// the last digit: a decimal(12,2) holding at most 24,560,000 has most of its
// range spare, while one reaching 8,115,022,364.86 has 19% of it.
//
// It returns 0 for a column that is not a decimal or has no observed values.
func DecimalHeadroom(c *ResolvedColumn, p *qvd.ColumnProfile) float64 {
	if c.Strategy != StrategyDecimal || p == nil {
		return 0
	}
	lo, hi, ok := numericBounds(p)
	if !ok {
		return 0
	}
	widest := math.Max(math.Abs(lo), math.Abs(hi))
	limit := decimalLimit(c.Decimal.Precision, c.Decimal.Scale)
	if limit <= 0 {
		return 0
	}
	return widest / limit
}

// decimalLimit is the largest magnitude a decimal(precision, scale) holds:
// 10^(precision-scale) less one unit in the last place.
func decimalLimit(precision, scale int32) float64 {
	if precision <= 0 || scale < 0 || precision < scale {
		return 0
	}
	pow := new(big.Float).SetFloat64(math.Pow(10, float64(precision-scale)))
	ulp := new(big.Float).SetFloat64(math.Pow(10, -float64(scale)))
	limit, _ := new(big.Float).Sub(pow, ulp).Float64()
	return limit
}

// decimalsNearLimit names the decimal columns at or above
// DecimalTightFraction of their type's range, in schema order.
func decimalsNearLimit(rs *ResolvedSchema, f *qvd.File) []string {
	var out []string
	for i := range rs.Columns {
		c := &rs.Columns[i]
		if DecimalHeadroom(c, f.Profiles[c.SourceIndex]) >= DecimalTightFraction {
			out = append(out, c.Name)
		}
	}
	return out
}
