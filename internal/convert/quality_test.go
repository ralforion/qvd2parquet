package convert

import (
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
)

func intColumn(name string, strategy ValueStrategy) *ColumnMetrics {
	return NewColumnMetrics(&ResolvedColumn{Name: name, ArrowType: arrowInt64, Strategy: strategy})
}

func TestColumnMetricsCountsAndIntegerAggregates(t *testing.T) {
	m := intColumn("Id", StrategyInt64)
	for _, v := range []Value{{Int: 5}, {Null: true}, {Int: -3}, {Int: 10}} {
		m.Observe(v, false)
	}
	s := m.Stats()
	if m.Rows != 4 || s.Nulls != 1 || s.NonNulls != 3 {
		t.Errorf("counts: rows=%d nulls=%d nonNulls=%d", m.Rows, s.Nulls, s.NonNulls)
	}
	if s.Sum != "12" || s.Min != "-3" || s.Max != "10" {
		t.Errorf("aggregates: sum=%s min=%s max=%s, want 12 / -3 / 10", s.Sum, s.Min, s.Max)
	}
}

func TestColumnMetricsDateTimeAggregatesUsePhysicalValues(t *testing.T) {
	for _, strategy := range []ValueStrategy{StrategyDate32, StrategyTimestampMicros, StrategyTimeMillis} {
		m := intColumn("T", strategy)
		m.Observe(Value{Int: 100}, false)
		m.Observe(Value{Int: 300}, false)
		s := m.Stats()
		if s.Sum != "400" || s.Min != "100" || s.Max != "300" {
			t.Errorf("%v: sum=%s min=%s max=%s", strategy, s.Sum, s.Min, s.Max)
		}
	}
}

func TestColumnMetricsIntegerSumIsExactBeyondFloat64(t *testing.T) {
	m := intColumn("Big", StrategyInt64)
	// Three values whose sum would lose precision if accumulated as float64.
	for i := 0; i < 3; i++ {
		m.Observe(Value{Int: math.MaxInt64 / 4}, false)
	}
	want := new(big.Int).Mul(big.NewInt(math.MaxInt64/4), big.NewInt(3)).String()
	if got := m.Stats().Sum; got != want {
		t.Errorf("sum = %s, want %s", got, want)
	}
}

func TestColumnMetricsDecimalAggregatesAreExact(t *testing.T) {
	rc := &ResolvedColumn{
		Name:      "Amount",
		ArrowType: &arrow.Decimal128Type{Precision: 12, Scale: 2},
		Strategy:  StrategyDecimal,
		Decimal:   DecimalSpec{Precision: 12, Scale: 2},
	}
	m := NewColumnMetrics(rc)
	for _, s := range []string{"12345", "-1050", "1"} { // 123.45, -10.50, 0.01
		m.Observe(Value{Scaled: mustBig(s)}, false)
	}
	m.Observe(Value{Null: true}, false)

	st := m.Stats()
	if st.Sum != "112.96" {
		t.Errorf("sum = %s, want 112.96", st.Sum)
	}
	if st.Min != "-10.50" || st.Max != "123.45" {
		t.Errorf("range = [%s,%s], want [-10.50,123.45]", st.Min, st.Max)
	}
	if st.Nulls != 1 || st.NonNulls != 3 {
		t.Errorf("counts = %d null, %d non-null", st.Nulls, st.NonNulls)
	}
}

func TestColumnMetricsFloatAggregates(t *testing.T) {
	m := NewColumnMetrics(&ResolvedColumn{Name: "R", ArrowType: arrowF64, Strategy: StrategyFloat64})
	for _, v := range []float64{1.5, -2.5, 4} {
		m.Observe(Value{Float: v}, false)
	}
	s := m.Stats()
	if s.sumF != 3 {
		t.Errorf("sum = %v, want 3", s.sumF)
	}
	if s.sumSqF != 1.5*1.5+2.5*2.5+16 {
		t.Errorf("sum of squares = %v", s.sumSqF)
	}
	if s.Min != "-2.5" || s.Max != "4" {
		t.Errorf("range = [%s,%s]", s.Min, s.Max)
	}
}

func TestMergeIsOrderIndependent(t *testing.T) {
	values := [][]Value{
		{{Int: 1}, {Int: 2}},
		{{Null: true}, {Int: 30}},
		{{Int: -7}},
	}
	build := func(order []int) *ColumnMetrics {
		total := intColumn("Id", StrategyInt64)
		for _, i := range order {
			chunk := intColumn("Id", StrategyInt64)
			for _, v := range values[i] {
				chunk.Observe(v, true)
			}
			total.Merge(chunk)
		}
		return total
	}
	a := build([]int{0, 1, 2}).Stats()
	b := build([]int{2, 0, 1}).Stats()
	if a.Sum != b.Sum || a.Min != b.Min || a.Max != b.Max {
		t.Errorf("aggregates depend on merge order: %+v vs %+v", a, b)
	}
	if a.Hash != b.Hash {
		t.Errorf("fingerprint depends on merge order: %s vs %s", a.Hash, b.Hash)
	}
	if a.Nulls != 1 || a.NonNulls != 4 {
		t.Errorf("merged counts = %d null, %d non-null", a.Nulls, a.NonNulls)
	}
}

func TestCanonicalHashDistinguishesValues(t *testing.T) {
	hashOf := func(rc *ResolvedColumn, vals ...Value) string {
		m := NewColumnMetrics(rc)
		for _, v := range vals {
			m.Observe(v, true)
		}
		return m.Stats().Hash
	}
	strCol := &ResolvedColumn{Name: "S", ArrowType: arrowString, Strategy: StrategyString}
	intCol := &ResolvedColumn{Name: "S", ArrowType: arrowInt64, Strategy: StrategyInt64}

	// A null must not hash like an empty string or a zero.
	if hashOf(strCol, Value{Null: true}) == hashOf(strCol, Value{Str: ""}) {
		t.Error("null and empty string hash the same")
	}
	if hashOf(intCol, Value{Null: true}) == hashOf(intCol, Value{Int: 0}) {
		t.Error("null and zero hash the same")
	}
	// A changed string value must change the fingerprint.
	if hashOf(strCol, Value{Str: "a"}) == hashOf(strCol, Value{Str: "b"}) {
		t.Error("different strings hash the same")
	}
	// Column identity is part of the digest, so the same value in a
	// differently-typed column hashes differently.
	if hashOf(strCol, Value{Str: "1"}) == hashOf(intCol, Value{Int: 1}) {
		t.Error("values from differently-typed columns hash the same")
	}
	// The multiset digest is sensitive to duplication.
	if hashOf(intCol, Value{Int: 1}) == hashOf(intCol, Value{Int: 1}, Value{Int: 1}) {
		t.Error("a duplicated value did not change the fingerprint")
	}
}

func TestCanonicalDecimalHashIncludesPrecisionAndScale(t *testing.T) {
	mk := func(p, s int32) string {
		rc := &ResolvedColumn{
			Name:      "D",
			ArrowType: &arrow.Decimal128Type{Precision: p, Scale: s},
			Strategy:  StrategyDecimal,
			Decimal:   DecimalSpec{Precision: p, Scale: s},
		}
		m := NewColumnMetrics(rc)
		m.Observe(Value{Scaled: mustBig("100")}, true)
		return m.Stats().Hash
	}
	// The same scaled integer means a different number at a different scale.
	if mk(10, 2) == mk(10, 4) {
		t.Error("scale is not part of the decimal fingerprint")
	}
	if mk(10, 2) == mk(12, 2) {
		t.Error("precision is not part of the decimal fingerprint")
	}
}

func TestFloatsClose(t *testing.T) {
	tests := []struct {
		a, b, rel, abs float64
		want           bool
	}{
		{1, 1, 0, 0, true},
		{1, 1 + 1e-12, 1e-9, 0, true},
		{1, 1.1, 1e-9, 0, false},
		{1, 1.05, 0, 0.1, true},
		{1e9, 1e9 + 1, 1e-9, 0, true},
		{0, 1e-10, 1e-9, 0, true},
		{math.NaN(), math.NaN(), 0, 0, true},
		{math.NaN(), 1, 1, 1, false},
	}
	for _, tc := range tests {
		if got := floatsClose(tc.a, tc.b, tc.rel, tc.abs); got != tc.want {
			t.Errorf("floatsClose(%v, %v, rel=%v, abs=%v) = %v, want %v",
				tc.a, tc.b, tc.rel, tc.abs, got, tc.want)
		}
	}
}

func TestQualityReportErrNamesColumnsAndMetrics(t *testing.T) {
	r := &QualityReport{
		Passed: false,
		Errors: []string{"row count differs: source 10, Parquet 9"},
		Columns: []ColumnComparison{
			{Name: "Amount", Passed: false, Errors: []string{"sum differs: source 1.00, Parquet 2.00"}},
		},
	}
	err := r.Err()
	if err == nil {
		t.Fatal("a failed report should produce an error")
	}
	for _, want := range []string{"row count differs", `column "Amount"`, "sum differs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	if (&QualityReport{Passed: true}).Err() != nil {
		t.Error("a passing report should produce no error")
	}
}

func mustBig(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad big int " + s)
	}
	return v
}
