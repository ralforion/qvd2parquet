package qvdtest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// TestWriteSAPDemoFixture writes a BSEG-shaped QVD -- SAP accounting document
// line items -- so the converter can be demonstrated and benchmarked against
// something with the shape of a real extract. It is skipped unless a
// destination is given, since it writes tens of megabytes:
//
//	SAPDEMO_OUT=/tmp/sapdemo/BSEG.qvd go test ./internal/qvdtest -run SAPDemo -v
//
// SAPDEMO_ROWS overrides the row count, which defaults to 2,400,000.
//
// The field names and formats are SAP's; the data is generated. Nothing here
// comes from a real system.
//
// It is a fixture rather than an assertion, but it earns its place in the tests
// by covering shapes the small fixtures do not: zero-padded key columns, MONEY
// with a declared scale, and dated duals on a column the header leaves untyped.
func TestWriteSAPDemoFixture(t *testing.T) {
	out := os.Getenv("SAPDEMO_OUT")
	if out == "" {
		t.Skip("set SAPDEMO_OUT=/path/to/BSEG.qvd to write the demo fixture")
	}
	rows := 2_400_000
	if s := os.Getenv("SAPDEMO_ROWS"); s != "" {
		if _, err := fmt.Sscan(s, &rows); err != nil || rows <= 0 {
			t.Fatalf("SAPDEMO_ROWS=%q is not a positive row count", s)
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}

	// SAP keeps its keys as fixed-width character codes: BELNR is CHAR(10), so
	// document 100000001 is stored "0100000001". Reading one as an integer
	// drops the padding and the key stops joining back.
	padded := func(n, count, step int64) []qvd.Symbol {
		out := make([]qvd.Symbol, 0, count)
		for i := int64(0); i < count; i++ {
			v := n + i*step
			out = append(out, qvdtest.DualInt(v, fmt.Sprintf("%010d", v)))
		}
		return out
	}
	// MONEY carries a declared scale, so the amounts must stay exact decimals
	// rather than becoming float64.
	money := func(count int, base, step float64) []qvd.Symbol {
		out := make([]qvd.Symbol, 0, count)
		for i := 0; i < count; i++ {
			v := base + float64(i)*step
			out = append(out, qvdtest.Float(float64(int64(v*100))/100))
		}
		return out
	}
	// BUDAT and CPUDT are untyped in the header and carry their date only in
	// the display string, which is what date inference has to pick up.
	dates := func(count, offset int) []qvd.Symbol {
		out := make([]qvd.Symbol, 0, count)
		for d := 0; d < count; d++ {
			serial := 45292 + d + offset // 45292 is 2024-01-01
			y, m, dd := civilFromSerial(serial)
			out = append(out, qvdtest.DualInt(int64(serial), fmt.Sprintf("%04d-%02d-%02d", y, m, dd)))
		}
		return out
	}
	ints := func(from, count int64) []qvd.Symbol {
		out := make([]qvd.Symbol, 0, count)
		for i := int64(0); i < count; i++ {
			out = append(out, qvdtest.Int(from+i))
		}
		return out
	}
	strs := func(vs ...string) []qvd.Symbol {
		out := make([]qvd.Symbol, 0, len(vs))
		for _, v := range vs {
			out = append(out, qvdtest.Str(v))
		}
		return out
	}
	// Spread rows over each symbol table with a stride coprime to most table
	// sizes, so the columns vary independently rather than moving in lockstep.
	cycle := func(n int) []int {
		idx := make([]int, rows)
		for i := range idx {
			idx[i] = (i*7919 + i/13) % n
		}
		return idx
	}
	field := func(name, typ string, nDec int, syms []qvd.Symbol, tags ...string) qvdtest.Field {
		return qvdtest.Field{Name: name, Type: typ, NDec: nDec,
			Symbols: syms, Rows: cycle(len(syms)), Tags: tags}
	}

	tbl := qvdtest.Table{Name: "BSEG", Fields: []qvdtest.Field{
		field("MANDT", "", 0, strs("800"), "$ascii", "$text"),
		field("BUKRS", "", 0, strs("1000", "2000", "3000"), "$ascii", "$text"),
		field("BELNR", "", 0, padded(100000001, 4000, 1)),
		field("GJAHR", "INTEGER", 0, ints(2024, 1), "$numeric", "$integer"),
		field("BUZEI", "INTEGER", 0, ints(1, 200), "$numeric", "$integer"),
		field("KOART", "", 0, strs("S", "D", "K"), "$ascii", "$text"),
		field("SHKZG", "", 0, strs("S", "H"), "$ascii", "$text"),
		field("HKONT", "", 0, padded(400000, 120, 10)),
		field("KOSTL", "", 0, padded(1000, 60, 1)),
		field("DMBTR", "MONEY", 2, money(20000, 12.05, 1.37), "$numeric"),
		field("WRBTR", "MONEY", 2, money(20000, 13.05, 1.48), "$numeric"),
		field("WAERS", "", 0, strs("EUR", "USD", "CHF"), "$ascii", "$text"),
		field("BUDAT", "", 0, dates(366, 0)),
		field("CPUDT", "", 0, dates(366, 1)),
		field("SGTXT", "", 0, strs("Wareneingang", "Rechnung Lieferant", "Zahlungsausgang",
			"Umbuchung", "Abgrenzung", "Skonto"), "$ascii", "$text"),
	}}

	written, err := qvdtest.Build(out, tbl)
	if err != nil {
		t.Fatal(err)
	}
	// Build returns the row count, so the file size comes from the file.
	size := int64(-1)
	if fi, err := os.Stat(out); err == nil {
		size = fi.Size()
	}
	t.Logf("wrote %s: %d rows, %d fields, %.1f MiB",
		out, written, len(tbl.Fields), float64(size)/(1<<20))
}

// civilFromSerial converts a Qlik serial day number to a calendar date, using
// Howard Hinnant's civil_from_days.
func civilFromSerial(serial int) (int, int, int) {
	z := serial + 693594 - 719163 + 719468
	era := z / 146097
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}
