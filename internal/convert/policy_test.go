package convert

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// resolve builds a one-column synthetic QVD and resolves its schema.
func resolve(t *testing.T, f qvdtest.Field, mutate func(*Options)) (*ResolvedSchema, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.qvd")
	if _, err := qvdtest.Build(path, qvdtest.Table{Name: "T", Fields: []qvdtest.Field{f}}); err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	qf, err := qvd.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { qf.Close() })
	if err := qf.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		t.Fatalf("read symbols: %v", err)
	}
	opts := DefaultOptions()
	if mutate != nil {
		mutate(&opts)
	}
	if err := opts.Validate(); err != nil {
		t.Fatalf("validate options: %v", err)
	}
	// Honour --schema when the test sets it; otherwise a test that configures
	// an override would silently exercise plain inference instead.
	var override *SchemaOverride
	if opts.SchemaOverridePath != "" {
		var err error
		if override, err = LoadSchemaOverride(opts.SchemaOverridePath); err != nil {
			t.Fatalf("load schema override: %v", err)
		}
	}
	return ResolveSchema(qf, &opts, override)
}

func mustResolve(t *testing.T, f qvdtest.Field, mutate func(*Options)) *ResolvedSchema {
	t.Helper()
	rs, err := resolve(t, f, mutate)
	if err != nil {
		t.Fatalf("ResolveSchema: %v", err)
	}
	return rs
}

func TestResolvePureTypes(t *testing.T) {
	tests := []struct {
		name         string
		field        qvdtest.Field
		wantType     string
		wantStrategy ValueStrategy
	}{
		{
			"pure string", qvdtest.Field{Name: "S", Type: "ASCII",
				Symbols: []qvd.Symbol{qvdtest.Str("a"), qvdtest.Str("b")}, Rows: []int{0, 1}},
			"utf8", StrategyString,
		},
		{
			"pure integer", qvdtest.Field{Name: "I", Type: "INTEGER",
				Symbols: []qvd.Symbol{qvdtest.Int(1), qvdtest.Int(2)}, Rows: []int{0, 1}},
			"int64", StrategyInt64,
		},
		{
			// Under the default --numeric-promote=decimal a REAL column with
			// a representable scale becomes an exact decimal.
			"pure real", qvdtest.Field{Name: "R", Type: "REAL",
				Symbols: []qvd.Symbol{qvdtest.Float(1.5), qvdtest.Float(2.5)}, Rows: []int{0, 1}},
			"decimal(2, 1)", StrategyDecimal,
		},
		{
			// A REAL column with no representable scale stays float64.
			"unrepresentable real", qvdtest.Field{Name: "R", Type: "REAL",
				Symbols: []qvd.Symbol{qvdtest.Float(1.0 / 3), qvdtest.Float(2.5)}, Rows: []int{0, 1}},
			"float64", StrategyFloat64,
		},
		{
			"date", qvdtest.Field{Name: "D", Type: "DATE",
				Symbols: []qvd.Symbol{qvdtest.Int(45000), qvdtest.Int(45001)}, Rows: []int{0, 1}},
			"date32", StrategyDate32,
		},
		{
			"time", qvdtest.Field{Name: "T", Type: "TIME",
				Symbols: []qvd.Symbol{qvdtest.Float(0.5), qvdtest.Float(0.25)}, Rows: []int{0, 1}},
			"time32[ms]", StrategyTimeMillis,
		},
		{
			"all null", qvdtest.Field{Name: "N", Type: "INTEGER",
				Symbols: []qvd.Symbol{qvdtest.Null()}, Rows: []int{-1, -1}},
			"utf8", StrategyNull,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := mustResolve(t, tc.field, nil)
			if len(rs.Columns) != 1 {
				t.Fatalf("got %d output columns", len(rs.Columns))
			}
			c := rs.Columns[0]
			if c.ArrowType.String() != tc.wantType {
				t.Errorf("type = %s, want %s", c.ArrowType, tc.wantType)
			}
			if c.Strategy != tc.wantStrategy {
				t.Errorf("strategy = %v, want %v", c.Strategy, tc.wantStrategy)
			}
			if !c.Nullable {
				t.Error("column should be nullable")
			}
		})
	}
}

func TestResolveTimestampCarriesTimezone(t *testing.T) {
	f := qvdtest.Field{Name: "TS", Type: "TIMESTAMP",
		Symbols: []qvd.Symbol{qvdtest.Float(45000.5)}, Rows: []int{0}}
	rs := mustResolve(t, f, func(o *Options) { o.Location = utc(); o.TimezoneName = "UTC" })
	if got := rs.Columns[0].ArrowType.String(); !strings.Contains(got, "timestamp[us") {
		t.Errorf("type = %s, want a timestamp[us] type", got)
	}
}

// A QVD stores a naive wall clock, so --timezone=none must not stamp a zone:
// the Arrow type carries no name, which is what makes Parquet record
// isAdjustedToUTC=false and stops every reader shifting the value.
func TestNaiveTimestampsCarryNoTimezone(t *testing.T) {
	f := qvdtest.Field{Name: "TS", Type: "TIMESTAMP",
		Symbols: []qvd.Symbol{qvdtest.Float(45000.5)}, Rows: []int{0}}
	rs := mustResolve(t, f, func(o *Options) {
		o.Location = utc()
		o.TimezoneName = "none"
		o.NaiveTimestamps = true
	})
	if got := rs.Columns[0].ArrowType.String(); got != "timestamp[us]" {
		t.Errorf("type = %s, want timestamp[us] with no timezone", got)
	}
}

// Reinterpreting a wall clock in UTC is the identity mapping, so the naive and
// UTC modes must agree on every stored value and differ only in the type.
func TestNaiveAndUTCStoreTheSameValues(t *testing.T) {
	for _, serial := range []float64{45000.5, 42382.2604166667, 25569, 0.25} {
		naive, ok := qvd.QlikDaysToTimestampMicros(serial, nil)
		if !ok {
			t.Fatalf("serial %v did not convert", serial)
		}
		asUTC, ok := qvd.QlikDaysToTimestampMicros(serial, time.UTC)
		if !ok || naive != asUTC {
			t.Errorf("serial %v: naive %d != UTC %d", serial, naive, asUTC)
		}
	}
}

func TestIntPlusFloatPromotes(t *testing.T) {
	f := qvdtest.Field{Name: "M", Type: "REAL",
		Symbols: []qvd.Symbol{qvdtest.Int(1), qvdtest.Float(2.5)}, Rows: []int{0, 1}}

	// The default is --numeric-promote=decimal, so an int+float column with a
	// representable scale becomes an exact decimal.
	rs := mustResolve(t, f, nil)
	if rs.Columns[0].Strategy != StrategyDecimal {
		t.Errorf("strategy = %v, want StrategyDecimal", rs.Columns[0].Strategy)
	}
	// --numeric-promote=true still selects the float64 widening.
	rs = mustResolve(t, f, func(o *Options) { o.NumericPromote = PromoteFloat64 })
	if rs.Columns[0].Strategy != StrategyFloat64 {
		t.Errorf("--numeric-promote=true strategy = %v, want StrategyFloat64", rs.Columns[0].Strategy)
	}

	_, err := resolve(t, f, func(o *Options) { o.NumericPromote = PromoteNone })
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("err = %v, want ErrSchemaPolicy", err)
	}
	if !strings.Contains(err.Error(), "--numeric-promote") {
		t.Errorf("error should name the flag that fixes it: %v", err)
	}
}

func TestIntPlusStringFailsUnderMixedError(t *testing.T) {
	f := qvdtest.Field{Name: "CustomerID", Type: "ASCII",
		Symbols: []qvd.Symbol{qvdtest.Int(1), qvdtest.Str("N/A")}, Rows: []int{0, 1}}

	_, err := resolve(t, f, nil)
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("err = %v, want ErrSchemaPolicy", err)
	}
	for _, want := range []string{"CustomerID", "--mixed=string"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestIntPlusStringUnderMixedString(t *testing.T) {
	f := qvdtest.Field{Name: "C", Type: "ASCII",
		Symbols: []qvd.Symbol{qvdtest.Int(1), qvdtest.Str("N/A")}, Rows: []int{0, 1}}
	rs := mustResolve(t, f, func(o *Options) { o.Mixed = MixedString })
	if rs.Columns[0].Strategy != StrategyString {
		t.Errorf("strategy = %v, want StrategyString", rs.Columns[0].Strategy)
	}
}

func TestIntPlusStringUnderStringFallback(t *testing.T) {
	f := qvdtest.Field{Name: "C", Type: "ASCII",
		Symbols: []qvd.Symbol{qvdtest.Int(1), qvdtest.Str("N/A")}, Rows: []int{0, 1}}
	rs := mustResolve(t, f, func(o *Options) { o.Mixed = MixedPromote; o.MixedStringFallback = true })
	if rs.Columns[0].Strategy != StrategyString {
		t.Errorf("strategy = %v, want StrategyString", rs.Columns[0].Strategy)
	}
}

func TestDualStrategies(t *testing.T) {
	f := qvdtest.Field{Name: "Qty", Type: "INTEGER",
		Symbols: []qvd.Symbol{qvdtest.DualInt(1, "one"), qvdtest.DualInt(2, "two")}, Rows: []int{0, 1}}

	t.Run("numeric", func(t *testing.T) {
		rs := mustResolve(t, f, func(o *Options) { o.Dual = DualNumeric })
		if len(rs.Columns) != 1 || rs.Columns[0].Strategy != StrategyInt64 {
			t.Errorf("columns = %+v, want a single int64 column", rs.Columns)
		}
	})
	t.Run("text", func(t *testing.T) {
		rs := mustResolve(t, f, func(o *Options) { o.Dual = DualText })
		if len(rs.Columns) != 1 || rs.Columns[0].Strategy != StrategyString {
			t.Errorf("columns = %+v, want a single utf8 column", rs.Columns)
		}
	})
	t.Run("columns", func(t *testing.T) {
		rs := mustResolve(t, f, func(o *Options) { o.Dual = DualColumns })
		if len(rs.Columns) != 2 {
			t.Fatalf("got %d columns, want 2", len(rs.Columns))
		}
		if rs.Columns[0].Name != "Qty" || rs.Columns[0].Strategy != StrategyInt64 {
			t.Errorf("first column = %+v", rs.Columns[0])
		}
		if rs.Columns[1].Name != "Qty__text" || rs.Columns[1].Strategy != StrategyDualText {
			t.Errorf("second column = %+v", rs.Columns[1])
		}
	})
	t.Run("mixed dual-columns implies dual columns", func(t *testing.T) {
		rs := mustResolve(t, f, func(o *Options) { o.Mixed = MixedDualColumns })
		if len(rs.Columns) != 2 {
			t.Fatalf("--mixed=dual-columns should emit 2 columns, got %d", len(rs.Columns))
		}
	})
}

func TestMoneyAndFixResolveToDecimal(t *testing.T) {
	for _, qlikType := range []string{"MONEY", "FIX"} {
		t.Run(qlikType, func(t *testing.T) {
			f := qvdtest.Field{
				Name: "Amount", Type: qlikType, NDec: 2, Dec: ",", Thou: ".",
				Symbols: []qvd.Symbol{
					qvdtest.DualFloat(1234.56, "1.234,56"),
					qvdtest.DualFloat(-10.5, "-10,50"),
				},
				Rows: []int{0, 1},
			}
			rs := mustResolve(t, f, nil)
			c := rs.Columns[0]
			if c.Strategy != StrategyDecimal {
				t.Fatalf("%s resolved to %v, want StrategyDecimal", qlikType, c.Strategy)
			}
			if c.Decimal.Scale != 2 {
				t.Errorf("scale = %d, want 2", c.Decimal.Scale)
			}
			if c.Decimal.Precision != 6 { // 123456
				t.Errorf("precision = %d, want 6", c.Decimal.Precision)
			}
			if !strings.HasPrefix(c.ArrowType.String(), "decimal") {
				t.Errorf("arrow type = %s, want a decimal type", c.ArrowType)
			}
			if !c.DecimalFromText {
				t.Error("digits should have come from the display strings")
			}
			if c.Scaled[0].String() != "123456" || c.Scaled[1].String() != "-1050" {
				t.Errorf("scaled = %v, %v", c.Scaled[0], c.Scaled[1])
			}
		})
	}
}

func TestMoneyNeverBecomesFloat(t *testing.T) {
	f := qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 2,
		Symbols: []qvd.Symbol{qvdtest.Float(9.99)}, Rows: []int{0}}
	rs := mustResolve(t, f, nil)
	if got := rs.Columns[0].ArrowType.String(); strings.Contains(got, "float") {
		t.Fatalf("MONEY resolved to %s; it must never default to float64", got)
	}
}

func TestMoneyInexactFailsUnderStrict(t *testing.T) {
	f := qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 2,
		Symbols: []qvd.Symbol{qvdtest.Float(1.0 / 3)}, Rows: []int{0}}
	_, err := resolve(t, f, func(o *Options) { o.DecimalStrict = true })
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("err = %v, want ErrSchemaPolicy", err)
	}
	if !strings.Contains(err.Error(), "--decimal-strict") {
		t.Errorf("error should suggest --decimal-strict=false: %v", err)
	}
}

// Rounding to the declared scale is the default, matching what Qlik itself
// displays for a MONEY field with nDec decimals.
func TestMoneyInexactRoundsByDefault(t *testing.T) {
	f := qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 2,
		Symbols: []qvd.Symbol{qvdtest.Float(1.0 / 3), qvdtest.Float(2.0 / 3)},
		Rows:    []int{0, 1}}
	rs := mustResolve(t, f, nil)
	c := rs.Columns[0]
	if c.Strategy != StrategyDecimal {
		t.Fatalf("strategy = %v, want StrategyDecimal", c.Strategy)
	}
	if got := c.Scaled[0].String(); got != "33" {
		t.Errorf("1/3 scaled = %s, want 33", got)
	}
	if got := c.Scaled[1].String(); got != "67" {
		t.Errorf("2/3 scaled = %s, want 67", got)
	}
	// Rounding must be counted, so it can be reported rather than hidden.
	if c.DecimalRounded != 2 {
		t.Errorf("DecimalRounded = %d, want 2", c.DecimalRounded)
	}
	joined := strings.Join(rs.Notes, " ")
	if !strings.Contains(joined, "rounded") {
		t.Errorf("the schema note should mention rounding: %q", joined)
	}
}

func TestDefaultsAreDecimalAndRounding(t *testing.T) {
	d := DefaultOptions()
	if d.NumericPromote != PromoteDecimal {
		t.Errorf("NumericPromote = %v, want PromoteDecimal", d.NumericPromote)
	}
	if d.DecimalStrict {
		t.Error("DecimalStrict should default to false, so values round to their scale")
	}
}

func TestMoneyScaleInferredFromDisplayStrings(t *testing.T) {
	f := qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 0, Dec: ",",
		Symbols: []qvd.Symbol{qvdtest.DualFloat(1.5, "1,5"), qvdtest.DualFloat(2.25, "2,25")},
		Rows:    []int{0, 1}}
	rs := mustResolve(t, f, nil)
	if got := rs.Columns[0].Decimal.Scale; got != 2 {
		t.Errorf("inferred scale = %d, want 2", got)
	}
}

func TestSchemaOverrideValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "schema.json")
		if err := writeFile(p, body); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if _, err := LoadSchemaOverride(write(`{"columns":{"A":{"type":"bogus"}}}`)); err == nil {
		t.Error("expected an error for an unknown override type")
	}
	if _, err := LoadSchemaOverride(write(`{"columns":{"A":{"type":"decimal","precision":0,"scale":2}}}`)); err == nil {
		t.Error("expected an error for a zero decimal precision")
	}
	if _, err := LoadSchemaOverride(write(`{"columns":{"A":{"type":"decimal","precision":4,"scale":6}}}`)); err == nil {
		t.Error("expected an error for scale > precision")
	}
	so, err := LoadSchemaOverride(write(`{"columns":{"Amount":{"type":"decimal","precision":18,"scale":4}}}`))
	if err != nil {
		t.Fatalf("LoadSchemaOverride: %v", err)
	}
	if co, ok := so.lookup("amount"); !ok || co.Precision != 18 || co.Scale != 4 {
		t.Errorf("lookup = %+v, %v", co, ok)
	}
}

func TestSchemaOverrideRejectsIncompatibleData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.qvd")
	f := qvdtest.Field{Name: "V", Type: "REAL",
		Symbols: []qvd.Symbol{qvdtest.Float(1.5)}, Rows: []int{0}}
	if _, err := qvdtest.Build(path, qvdtest.Table{Name: "T", Fields: []qvdtest.Field{f}}); err != nil {
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
	override := &SchemaOverride{Columns: map[string]ColumnOverride{"V": {Type: "int64"}}}
	if _, err := ResolveSchema(qf, &opts, override); err == nil {
		t.Fatal("pinning a double column to int64 should fail")
	}
}

func TestSchemaOverridePinsDecimalPrecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.qvd")
	f := qvdtest.Field{Name: "Amount", Type: "MONEY", NDec: 2,
		Symbols: []qvd.Symbol{qvdtest.Float(1.5)}, Rows: []int{0}}
	if _, err := qvdtest.Build(path, qvdtest.Table{Name: "T", Fields: []qvdtest.Field{f}}); err != nil {
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
	override := &SchemaOverride{Columns: map[string]ColumnOverride{
		"Amount": {Type: "decimal", Precision: 18, Scale: 4},
	}}
	rs, err := ResolveSchema(qf, &opts, override)
	if err != nil {
		t.Fatalf("ResolveSchema: %v", err)
	}
	if got := rs.Columns[0].ArrowType.String(); got != "decimal(18, 4)" {
		t.Errorf("type = %s, want decimal(18, 4)", got)
	}
	if rs.Columns[0].Scaled[0].String() != "15000" {
		t.Errorf("scaled = %s, want 15000", rs.Columns[0].Scaled[0])
	}
}

func TestParseFlags(t *testing.T) {
	if _, err := ParseMixedStrategy("nope"); err == nil {
		t.Error("expected an error for an invalid --mixed")
	}
	if _, err := ParseDualStrategy("nope"); err == nil {
		t.Error("expected an error for an invalid --dual")
	}
	if _, err := ParseDecimalSource("nope"); err == nil {
		t.Error("expected an error for an invalid --decimal-source")
	}
	if _, err := ParseQualityMode("nope"); err == nil {
		t.Error("expected an error for an invalid --quality-gate")
	}
	for in, want := range map[string]MixedStrategy{
		"error": MixedError, "string": MixedString,
		"promote": MixedPromote, "dual-columns": MixedDualColumns,
	} {
		got, err := ParseMixedStrategy(in)
		if err != nil || got != want {
			t.Errorf("ParseMixedStrategy(%q) = %v, %v", in, got, err)
		}
		if got.String() != in {
			t.Errorf("%v.String() = %q, want %q", got, got.String(), in)
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	o := DefaultOptions()
	o.BatchRows = 0
	if err := o.Validate(); err == nil {
		t.Error("expected an error for --batch-rows 0")
	}
	o = DefaultOptions()
	o.Workers = -1
	if err := o.Validate(); err == nil {
		t.Error("expected an error for a negative --workers")
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

// A zoned conversion has to place a naive wall clock on a timeline, and twice a
// year that changes it. The QVD names no zone, so the change rests entirely on
// the --timezone claim and must be reported rather than made silently.
func TestZonedTimestampReportsDSTDiscontinuities(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	// 2023-03-26 02:30 never happens in Berlin; 2023-10-29 02:30 happens twice.
	f := qvdtest.Field{Name: "TS", Type: "TIMESTAMP",
		Symbols: []qvd.Symbol{qvdtest.Float(45011.1041666667), qvdtest.Float(45228.1041666667)},
		Rows:    []int{0, 1}}

	rs := mustResolve(t, f, func(o *Options) { o.Location = berlin; o.TimezoneName = "Europe/Berlin" })
	note := strings.Join(rs.Notes, " ")
	for _, want := range []string{"do not exist in this timezone", "occur twice in this timezone", "--timezone=none"} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to mention %q", note, want)
		}
	}

	// The same values carry no such caveat when no zone is claimed.
	rs = mustResolve(t, f, func(o *Options) {
		o.Location = utc()
		o.TimezoneName = "none"
		o.NaiveTimestamps = true
	})
	if note := strings.Join(rs.Notes, " "); strings.Contains(note, "this timezone") {
		t.Errorf("naive mode should report no timezone caveat, got %q", note)
	}
}

// Stamping UTC is a statement about the stored value, not a shortcut past the
// conversion: a zoned run must still place the wall clock on that zone's
// timeline, so it lands on a different instant than a UTC run of the same input.
func TestZonedRunStampsUTCButStillConverts(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	const serial = 45000.5 // 2023-03-15 12:00 wall clock

	zoned, ok := qvd.QlikDaysToTimestampMicros(serial, berlin)
	if !ok {
		t.Fatal("conversion failed")
	}
	asUTC, _ := qvd.QlikDaysToTimestampMicros(serial, time.UTC)
	if zoned == asUTC {
		t.Error("a Berlin run must land on a different instant than a UTC run")
	}
	if delta := asUTC - zoned; delta != int64(time.Hour/time.Microsecond) {
		t.Errorf("delta = %d us, want one hour: Berlin is UTC+1 in March", delta)
	}

	f := qvdtest.Field{Name: "TS", Type: "TIMESTAMP",
		Symbols: []qvd.Symbol{qvdtest.Float(serial)}, Rows: []int{0}}
	rs := mustResolve(t, f, func(o *Options) { o.Location = berlin; o.TimezoneName = "Europe/Berlin" })
	if got := rs.Columns[0].ArrowType.String(); got != "timestamp[us, tz=UTC]" {
		t.Errorf("type = %s, want timestamp[us, tz=UTC]", got)
	}
}

// A column mixing text with numbers normally stops, because writing it as text
// means choosing a rendering for the numbers. When every symbol already carries
// its own display string that choice does not exist, so an integer column
// resolves to utf8 on its own. A zero-padded value is the clearest case: "0901"
// is a code, and reading it as 901 would not survive a round trip.
func TestMixedIntegerWithTextForEverySymbolResolvesToUTF8(t *testing.T) {
	f := qvdtest.Field{Name: "part_num", Type: "",
		Symbols: []qvd.Symbol{qvdtest.DualInt(901, "0901"), qvdtest.Str("0687b1")},
		Rows:    []int{0, 1}}
	rs := mustResolve(t, f, nil)
	if got := rs.Columns[0].ArrowType.String(); got != "utf8" {
		t.Fatalf("type = %s, want utf8", got)
	}
	note := strings.Join(rs.Notes, " ")
	for _, want := range []string{"every value carries a non-empty display string", `"0901"`} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to mention %q", note, want)
		}
	}
}

// The rule is deliberately narrow. A decimal beside text is more likely a
// measurement than a code, and a bare number carries no text to reuse, so both
// still stop and leave the choice to the caller.
func TestMixedStillStopsWhereTextWouldBeInvented(t *testing.T) {
	tests := []struct {
		name string
		syms []qvd.Symbol
	}{
		{"decimal duals", []qvd.Symbol{qvdtest.DualFloat(9.5, "9.5"), qvdtest.Str("n/a")}},
		{"bare integer with no text", []qvd.Symbol{qvdtest.Int(901), qvdtest.Str("0687b1")}},
		{"bare double with no text", []qvd.Symbol{qvdtest.Float(9.5), qvdtest.Str("n/a")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := qvdtest.Field{Name: "V", Type: "", Symbols: tc.syms, Rows: []int{0, 1}}
			_, err := resolve(t, f, nil)
			if err == nil {
				t.Fatal("want a schema policy error, got none")
			}
			if !errors.Is(err, ErrSchemaPolicy) {
				t.Errorf("error = %v, want ErrSchemaPolicy", err)
			}
		})
	}
}

// A dual whose display string is empty has nothing to write on the text side,
// and the default policy then turns that empty string into null, so its number
// would disappear. Counting symbols is not enough to call the column lossless.
func TestMixedStopsWhenADualHasEmptyText(t *testing.T) {
	f := qvdtest.Field{Name: "V", Type: "", Rows: []int{0, 1},
		Symbols: []qvd.Symbol{qvdtest.DualInt(7, ""), qvdtest.Str("A")}}
	_, err := resolve(t, f, nil)
	if err == nil {
		t.Fatal("want a schema policy error: writing text would drop the 7")
	}
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Errorf("error = %v, want ErrSchemaPolicy", err)
	}
}

// A bare empty string is a null placeholder, not a dual with nothing to write,
// so it must not block the lossless path.
func TestMixedAllowsBareEmptyStringPlaceholder(t *testing.T) {
	f := qvdtest.Field{Name: "V", Type: "", Rows: []int{0, 1, 2},
		Symbols: []qvd.Symbol{qvdtest.DualInt(901, "0901"), qvdtest.Str(""), qvdtest.Str("0687b1")}}
	rs := mustResolve(t, f, nil)
	if got := rs.Columns[0].ArrowType.String(); got != "utf8" {
		t.Errorf("type = %s, want utf8", got)
	}
}
