package convert

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/ralforion/qvd2parquet/internal/parquetwrite"
	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// sampleTable is a fixture exercising every resolved strategy.
func sampleTable(rows int) qvdtest.Table {
	ids := make([]int, rows)
	names := make([]int, rows)
	amounts := make([]int, rows)
	dates := make([]int, rows)
	reals := make([]int, rows)
	stamps := make([]int, rows)
	for i := 0; i < rows; i++ {
		ids[i] = i % 4
		names[i] = i % 3
		amounts[i] = i % 3
		dates[i] = i % 2
		// Every fifth row is null in the real column.
		if i%5 == 0 {
			reals[i] = -1
		} else {
			reals[i] = i % 2
		}
		stamps[i] = i % 2
	}
	return qvdtest.Table{
		Name: "Sales",
		Fields: []qvdtest.Field{
			{Name: "Id", Type: "INTEGER", Rows: ids,
				Symbols: []qvd.Symbol{qvdtest.Int(10), qvdtest.Int(20), qvdtest.Int(30), qvdtest.Int(-5)}},
			{Name: "Name", Type: "ASCII", Rows: names,
				Symbols: []qvd.Symbol{qvdtest.Str("alpha"), qvdtest.Str("beta"), qvdtest.Str("")}},
			{Name: "Amount", Type: "MONEY", NDec: 2, Dec: ",", Thou: ".", Rows: amounts,
				Symbols: []qvd.Symbol{
					qvdtest.DualFloat(1234.56, "1.234,56"),
					qvdtest.DualFloat(-10.5, "-10,50"),
					qvdtest.DualFloat(0, "0,00"),
				}},
			{Name: "Day", Type: "DATE", Rows: dates,
				Symbols: []qvd.Symbol{qvdtest.Int(45000), qvdtest.Int(45001)}},
			{Name: "Ratio", Type: "REAL", Rows: reals,
				Symbols: []qvd.Symbol{qvdtest.Float(0.25), qvdtest.Float(-1.75)}},
			{Name: "Seen", Type: "TIMESTAMP", Rows: stamps,
				Symbols: []qvd.Symbol{qvdtest.Float(45000.5), qvdtest.Float(45001.25)}},
		},
	}
}

func buildFixture(t *testing.T, tbl qvdtest.Table) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), tbl.Name+".qvd")
	if _, err := qvdtest.Build(path, tbl); err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	return path
}

func testOptions() Options {
	o := DefaultOptions()
	o.Location = time.UTC
	o.TimezoneName = "UTC"
	o.ProgressEvery = 0
	o.Compression = "snappy"
	return o
}

func TestRunEndToEnd(t *testing.T) {
	const rows = 5000
	in := buildFixture(t, sampleTable(rows))
	out := filepath.Join(t.TempDir(), "out.parquet")

	opts := testOptions()
	opts.BatchRows = 512
	stats, _, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Rows != rows {
		t.Errorf("rows = %d, want %d", stats.Rows, rows)
	}
	if stats.Columns != 6 {
		t.Errorf("columns = %d, want 6", stats.Columns)
	}
	if stats.OutputBytes <= 0 {
		t.Error("output file is empty")
	}

	// Read back and check the schema and a few values.
	schema, records := readParquet(t, out)
	wantTypes := map[string]string{
		"Id": "int64", "Name": "utf8", "Amount": "decimal(6, 2)",
		"Day": "date32", "Ratio": "decimal(3, 2)", // decimal by default
	}
	for name, want := range wantTypes {
		idxs := schema.FieldIndices(name)
		if len(idxs) == 0 {
			t.Fatalf("column %q missing from the Parquet schema", name)
		}
		if got := schema.Field(idxs[0]).Type.String(); got != want {
			t.Errorf("column %q type = %s, want %s", name, got, want)
		}
	}
	if got := schema.Field(schema.FieldIndices("Seen")[0]).Type.String(); !strings.HasPrefix(got, "timestamp[us") {
		t.Errorf("Seen type = %s, want timestamp[us...]", got)
	}

	var total int64
	for _, rec := range records {
		total += rec.NumRows()
	}
	if total != rows {
		t.Errorf("Parquet holds %d rows, want %d", total, rows)
	}
}

func TestRunRefusesToOverwrite(t *testing.T) {
	in := buildFixture(t, sampleTable(10))
	out := filepath.Join(t.TempDir(), "out.parquet")
	if err := os.WriteFile(out, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	_, _, err := Run(context.Background(), in, out, &opts, nil)
	if !errors.Is(err, parquetwrite.ErrOutput) {
		t.Fatalf("err = %v, want ErrOutput", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "existing" {
		t.Error("the existing output was modified despite --force being unset")
	}

	opts.Force = true
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatalf("Run with --force: %v", err)
	}
	if b, _ := os.ReadFile(out); string(b) == "existing" {
		t.Error("--force did not overwrite the output")
	}
}

func TestRunLeavesNoTempFileOnFailure(t *testing.T) {
	// A mixed column fails schema resolution, before any output is created.
	tbl := qvdtest.Table{Name: "Bad", Fields: []qvdtest.Field{
		{Name: "V", Type: "ASCII", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Int(1), qvdtest.Str("x")}},
	}}
	in := buildFixture(t, tbl)
	dir := t.TempDir()
	out := filepath.Join(dir, "out.parquet")

	opts := testOptions()
	if _, _, err := Run(context.Background(), in, out, &opts, nil); !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("err = %v, want ErrSchemaPolicy", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("output directory should be empty, has %d entries", len(entries))
	}
}

func TestRunSelectedColumns(t *testing.T) {
	in := buildFixture(t, sampleTable(100))
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	opts.Columns = []string{"id", "Amount"} // matching is case-insensitive
	stats, _, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Columns != 2 {
		t.Fatalf("columns = %d, want 2", stats.Columns)
	}
	schema, _ := readParquet(t, out)
	if len(schema.Fields()) != 2 || schema.Field(0).Name != "Id" || schema.Field(1).Name != "Amount" {
		t.Errorf("schema = %v, want Id and Amount in header order", schema.Fields())
	}
}

func TestRunUnknownColumn(t *testing.T) {
	in := buildFixture(t, sampleTable(10))
	opts := testOptions()
	opts.Columns = []string{"Nope"}
	_, _, err := Run(context.Background(), in, filepath.Join(t.TempDir(), "o.parquet"), &opts, nil)
	if err == nil || !strings.Contains(err.Error(), "no such column") {
		t.Fatalf("err = %v, want a missing-column error", err)
	}
}

func TestRunWorkerCountsAgree(t *testing.T) {
	const rows = 3000
	in := buildFixture(t, sampleTable(rows))

	var hashes []string
	for _, workers := range []int{1, 4} {
		out := filepath.Join(t.TempDir(), "out.parquet")
		opts := testOptions()
		opts.Workers = workers
		opts.BatchRows = 256
		opts.Quality = QualityFull
		_, report, err := Run(context.Background(), in, out, &opts, nil)
		if err != nil {
			t.Fatalf("Run with --workers=%d: %v", workers, err)
		}
		if !report.Passed {
			t.Fatalf("--workers=%d failed the full quality gate: %+v", workers, report.Errors)
		}
		var h strings.Builder
		for _, c := range report.Columns {
			h.WriteString(c.Name + "=" + c.Source.Hash + ";")
		}
		hashes = append(hashes, h.String())
	}
	if hashes[0] != hashes[1] {
		t.Error("--workers=1 and --workers=4 produced different content fingerprints")
	}
}

func TestQualityGateModes(t *testing.T) {
	in := buildFixture(t, sampleTable(1000))
	for _, mode := range []QualityMode{QualityBasic, QualityNumeric, QualityFull} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "out.parquet")
			reportPath := filepath.Join(dir, "quality.json")
			opts := testOptions()
			opts.Quality = mode
			opts.QualityReportPath = reportPath
			opts.BatchRows = 128

			_, report, err := Run(context.Background(), in, out, &opts, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !report.Passed {
				t.Fatalf("quality gate %s failed: %+v", mode, report)
			}
			if report.RowsSource != 1000 || report.RowsParquet != 1000 {
				t.Errorf("rows source=%d parquet=%d", report.RowsSource, report.RowsParquet)
			}
			// The report must be written on success too.
			var onDisk QualityReport
			b, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatalf("read quality report: %v", err)
			}
			if err := json.Unmarshal(b, &onDisk); err != nil {
				t.Fatalf("parse quality report: %v", err)
			}
			if !onDisk.Passed || onDisk.Mode != mode.String() {
				t.Errorf("report on disk = %+v", onDisk)
			}
			if mode != QualityBasic {
				assertNumericAggregates(t, &onDisk)
			}
			if mode == QualityFull {
				for _, c := range onDisk.Columns {
					if c.Source.Hash == "" {
						t.Errorf("column %q has no fingerprint in full mode", c.Name)
					}
				}
			}
		})
	}
}

// assertNumericAggregates checks that a known column's aggregates survived the
// Parquet round trip exactly.
func assertNumericAggregates(t *testing.T, r *QualityReport) {
	t.Helper()
	for _, c := range r.Columns {
		if c.Name != "Amount" {
			continue
		}
		if c.Source.Sum != c.Parquet.Sum {
			t.Errorf("decimal sum drifted: source %s, Parquet %s", c.Source.Sum, c.Parquet.Sum)
		}
		if c.Source.Min != "-10.50" {
			t.Errorf("Amount min = %q, want -10.50", c.Source.Min)
		}
		if c.Source.Max != "1234.56" {
			t.Errorf("Amount max = %q, want 1234.56", c.Source.Max)
		}
		return
	}
	t.Error("no Amount column in the quality report")
}

func TestQualityGateDetectsTruncatedOutput(t *testing.T) {
	// The gate reads the temp file, so corrupt it by pointing the gate at a
	// deliberately truncated copy.
	in := buildFixture(t, sampleTable(200))
	dir := t.TempDir()
	out := filepath.Join(dir, "out.parquet")
	opts := testOptions()
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(dir, "truncated.parquet")
	if err := os.WriteFile(truncated, full[:len(full)/2], 0o644); err != nil {
		t.Fatal(err)
	}

	qf, rs, metrics := reconvert(t, in, &opts)
	defer qf.Close()
	opts.Quality = QualityBasic
	report, err := RunQualityGate(in, out, truncated, rs, metrics, &opts)
	if err != nil {
		t.Fatalf("RunQualityGate: %v", err)
	}
	if report.Passed {
		t.Fatal("a truncated Parquet file should fail the quality gate")
	}
	if len(report.Errors) == 0 {
		t.Error("the report should explain why it failed")
	}
	if !errors.Is(report.Err(), ErrQualityGate) {
		t.Errorf("report.Err() = %v, want ErrQualityGate", report.Err())
	}
}

func TestQualityGateDetectsRowCountMismatch(t *testing.T) {
	in := buildFixture(t, sampleTable(300))
	dir := t.TempDir()
	out := filepath.Join(dir, "out.parquet")
	opts := testOptions()
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatal(err)
	}
	qf, rs, metrics := reconvert(t, in, &opts)
	defer qf.Close()

	metrics.Rows++ // pretend one more source row than was written
	opts.Quality = QualityBasic
	report, err := RunQualityGate(in, out, out, rs, metrics, &opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed {
		t.Fatal("a row count mismatch should fail the basic gate")
	}
	if !strings.Contains(strings.Join(report.Errors, " "), "row count differs") {
		t.Errorf("errors = %v", report.Errors)
	}
}

func TestQualityGateDetectsChangedAggregates(t *testing.T) {
	in := buildFixture(t, sampleTable(300))
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	// Pin float64 so Ratio exercises the floating-point comparison path.
	// Amount is declared MONEY and stays decimal either way, so both the
	// tolerance-based and the exact comparison are covered.
	opts.NumericPromote = PromoteFloat64
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		mode    QualityMode
		corrupt func(*Metrics)
		want    string
	}{
		{"null count", QualityBasic, func(m *Metrics) {
			c := columnMetrics(m, "Ratio")
			c.Nulls++
			c.NonNulls--
		}, "null count differs"},
		{"integer sum", QualityNumeric, func(m *Metrics) {
			c := columnMetrics(m, "Id")
			c.Observe(Value{Int: 1}, false)
			c.Rows--
			c.NonNulls--
		}, "sum differs"},
		{"decimal sum", QualityNumeric, func(m *Metrics) {
			c := columnMetrics(m, "Amount")
			c.decSum.Add(&c.decSum, bigOne())
		}, "sum differs"},
		{"float sum", QualityNumeric, func(m *Metrics) {
			columnMetrics(m, "Ratio").floatSum += 1000
		}, "sum differs beyond tolerance"},
		{"string value", QualityFull, func(m *Metrics) {
			columnMetrics(m, "Name").Observe(Value{Str: "injected"}, true)
		}, "fingerprint differs"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			qf, rs, metrics := reconvert(t, in, &opts)
			defer qf.Close()
			tc.corrupt(metrics)
			o := opts
			o.Quality = tc.mode
			report, err := RunQualityGate(in, out, out, rs, metrics, &o)
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed {
				t.Fatalf("corrupting the %s should have failed the %s gate", tc.name, tc.mode)
			}
			all := strings.Join(report.Errors, " ")
			for _, c := range report.Columns {
				all += " " + strings.Join(c.Errors, " ")
			}
			if !strings.Contains(all, tc.want) {
				t.Errorf("errors = %q, want them to mention %q", all, tc.want)
			}
		})
	}
}

func TestFloatSumWithinTolerancePasses(t *testing.T) {
	in := buildFixture(t, sampleTable(300))
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	// Pin float64 so this exercises the floating-point tolerance path rather
	// than the exact decimal comparison.
	opts.NumericPromote = PromoteFloat64
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatal(err)
	}
	qf, rs, metrics := reconvert(t, in, &opts)
	defer qf.Close()

	c := columnMetrics(metrics, "Ratio")
	if c.Strategy != StrategyFloat64 {
		t.Fatalf("Ratio strategy = %v, want StrategyFloat64 for this test", c.Strategy)
	}
	c.floatSum += math.Abs(c.floatSum) * 1e-12 // well inside the default 1e-9

	opts.Quality = QualityNumeric
	report, err := RunQualityGate(in, out, out, rs, metrics, &opts)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("a sub-tolerance float drift should pass: %+v", report.Columns)
	}
}

func TestSchemaReport(t *testing.T) {
	in := buildFixture(t, sampleTable(50))
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "schema.json")
	opts := testOptions()
	opts.SchemaReportPath = reportPath
	if _, _, err := Run(context.Background(), in, filepath.Join(dir, "o.parquet"), &opts, nil); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var rep SchemaReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("parse schema report: %v", err)
	}
	if rep.TableName != "Sales" || rep.Rows != 50 {
		t.Errorf("report header = %+v", rep)
	}
	if len(rep.Columns) != 6 {
		t.Fatalf("report has %d columns, want 6", len(rep.Columns))
	}
	for _, c := range rep.Columns {
		if c.Note == "" {
			t.Errorf("column %q has no explanation", c.Name)
		}
		if c.Profile == nil {
			t.Errorf("column %q has no profile", c.Name)
		}
		if c.Name == "Amount" {
			if c.Decimal == nil || c.Decimal.Scale != 2 {
				t.Errorf("Amount decimal report = %+v", c.Decimal)
			}
			if !c.Decimal.FromText {
				t.Error("Amount digits should be reported as coming from display strings")
			}
			if c.QlikType != "MONEY" {
				t.Errorf("Amount qlikType = %q", c.QlikType)
			}
		}
	}
}

func TestChunks(t *testing.T) {
	chunks := Chunks(10, 4, 2, 100)
	want := []DecodeChunk{
		{Index: 0, StartRow: 0, RowCount: 4, ByteOffset: 100},
		{Index: 1, StartRow: 4, RowCount: 4, ByteOffset: 108},
		{Index: 2, StartRow: 8, RowCount: 2, ByteOffset: 116},
	}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks, want %d", len(chunks), len(want))
	}
	for i := range want {
		if chunks[i] != want[i] {
			t.Errorf("chunk %d = %+v, want %+v", i, chunks[i], want[i])
		}
	}
	if Chunks(0, 4, 2, 0) != nil {
		t.Error("zero rows should produce no chunks")
	}
}

func TestWorkerCount(t *testing.T) {
	if got := WorkerCount(4, 10); got != 4 {
		t.Errorf("WorkerCount(4, 10) = %d, want 4", got)
	}
	if got := WorkerCount(8, 3); got != 3 {
		t.Errorf("WorkerCount should not exceed the chunk count, got %d", got)
	}
	if got := WorkerCount(0, 100); got != DefaultWorkers() {
		t.Errorf("WorkerCount(0, 100) = %d, want the default %d", got, DefaultWorkers())
	}
}

// The automatic worker count stays well under one per CPU: only decoding is
// parallel, and every extra worker costs in-flight memory that dominates
// resident size on a wide file.
func TestDefaultWorkers(t *testing.T) {
	got := DefaultWorkers()
	if got < MinDefaultWorkers {
		t.Errorf("DefaultWorkers() = %d, want at least the floor %d", got, MinDefaultWorkers)
	}
	if want := runtime.NumCPU() / 4; want >= MinDefaultWorkers && got != want {
		t.Errorf("DefaultWorkers() = %d, want NumCPU/4 = %d", got, want)
	}
	if got > runtime.NumCPU() && runtime.NumCPU() >= MinDefaultWorkers {
		t.Errorf("DefaultWorkers() = %d, must not exceed NumCPU = %d", got, runtime.NumCPU())
	}
}

func TestRunCancellation(t *testing.T) {
	in := buildFixture(t, sampleTable(20000))
	dir := t.TempDir()
	out := filepath.Join(dir, "out.parquet")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	opts := testOptions()
	opts.BatchRows = 64
	_, _, err := Run(ctx, in, out, &opts, nil)
	if err == nil {
		t.Fatal("a cancelled context should abort the conversion")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a cancelled conversion must not leave a final output file")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file %q was left behind", e.Name())
		}
	}
}

func TestRunEmptyTable(t *testing.T) {
	tbl := qvdtest.Table{Name: "Empty", Fields: []qvdtest.Field{
		{Name: "A", Type: "INTEGER", Symbols: []qvd.Symbol{qvdtest.Int(1)}, Rows: []int{}},
	}}
	in := buildFixture(t, tbl)
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	opts.Quality = QualityBasic
	stats, report, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run on an empty table: %v", err)
	}
	if stats.Rows != 0 {
		t.Errorf("rows = %d, want 0", stats.Rows)
	}
	if !report.Passed {
		t.Errorf("quality gate on an empty table failed: %+v", report)
	}
}

// reconvert re-runs the source side of a conversion so a test can corrupt the
// metrics before comparing them against an already-written Parquet file.
func reconvert(t *testing.T, in string, opts *Options) (*qvd.File, *ResolvedSchema, *Metrics) {
	t.Helper()
	qf, err := qvd.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := qf.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		t.Fatal(err)
	}
	o := *opts
	o.Quality = QualityFull // always collect fingerprints so any mode can be tested
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	rs, err := ResolveSchema(qf, &o, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := NewConverter(qf, rs, &o)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := conv.Run(context.Background(), discardSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return qf, rs, metrics
}

type discardSink struct{}

func (discardSink) Write(arrow.Record) error { return nil }

func columnMetrics(m *Metrics, name string) *ColumnMetrics {
	for _, c := range m.Columns {
		if c.Name == name {
			return c
		}
	}
	panic("no column " + name)
}

func bigOne() *big.Int { return big.NewInt(1) }

// readParquet reopens a written file and returns its Arrow schema and records.
func readParquet(t *testing.T, path string) (*arrow.Schema, []arrow.Record) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	rdr, err := file.NewParquetReader(f)
	if err != nil {
		t.Fatalf("open Parquet %s: %v", path, err)
	}
	t.Cleanup(func() { rdr.Close() })

	ar, err := pqarrow.NewFileReader(rdr, pqarrow.ArrowReadProperties{BatchSize: 1024}, memory.NewGoAllocator())
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ar.Schema()
	if err != nil {
		t.Fatal(err)
	}
	rr, err := ar.GetRecordReader(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rr.Release)

	var out []arrow.Record
	for {
		rec, err := rr.Read()
		if err != nil || rec == nil {
			break
		}
		rec.Retain()
		out = append(out, rec)
		t.Cleanup(rec.Release)
	}
	return schema, out
}
