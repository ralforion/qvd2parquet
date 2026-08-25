package convert

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// benchTable is a wide-ish mixed-type fixture: integers, high-cardinality
// strings, exact decimals, dates and doubles with nulls.
func benchTable(rows int) qvdtest.Table {
	const idCard, nameCard, amountCard, dayCard, ratioCard = 1000, 500, 50, 365, 100

	idSyms := make([]qvd.Symbol, idCard)
	for i := range idSyms {
		idSyms[i] = qvdtest.Int(int64(i))
	}
	nameSyms := make([]qvd.Symbol, nameCard)
	for i := range nameSyms {
		nameSyms[i] = qvdtest.Str(fmt.Sprintf("customer-%06d", i))
	}
	amountSyms := make([]qvd.Symbol, amountCard)
	for i := range amountSyms {
		v := float64(i)*10 + 0.25
		amountSyms[i] = qvdtest.DualFloat(v, fmt.Sprintf("%.2f", v))
	}
	daySyms := make([]qvd.Symbol, dayCard)
	for i := range daySyms {
		daySyms[i] = qvdtest.Int(int64(45000 + i))
	}
	ratioSyms := make([]qvd.Symbol, ratioCard)
	for i := range ratioSyms {
		ratioSyms[i] = qvdtest.Float(float64(i) / 7)
	}

	ids := make([]int, rows)
	names := make([]int, rows)
	amounts := make([]int, rows)
	days := make([]int, rows)
	ratios := make([]int, rows)
	for i := 0; i < rows; i++ {
		ids[i] = i % idCard
		names[i] = i % nameCard
		amounts[i] = i % amountCard
		days[i] = i % dayCard
		if i%17 == 0 {
			ratios[i] = -1 // null
		} else {
			ratios[i] = i % ratioCard
		}
	}
	return qvdtest.Table{Name: "Bench", Fields: []qvdtest.Field{
		{Name: "Id", Type: "INTEGER", Symbols: idSyms, Rows: ids},
		{Name: "Name", Type: "ASCII", Symbols: nameSyms, Rows: names},
		{Name: "Amount", Type: "MONEY", NDec: 2, Dec: ".", Symbols: amountSyms, Rows: amounts},
		{Name: "Day", Type: "DATE", Symbols: daySyms, Rows: days},
		{Name: "Ratio", Type: "REAL", Symbols: ratioSyms, Rows: ratios},
	}}
}

func benchFixture(b *testing.B, rows int) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "bench.qvd")
	if _, err := qvdtest.Build(path, benchTable(rows)); err != nil {
		b.Fatal(err)
	}
	return path
}

// BenchmarkDecode measures QVD read and Arrow batch construction without any
// Parquet writing.
func BenchmarkDecode(b *testing.B) {
	for _, workers := range []int{1, 2, 4, 8, 0} {
		name := fmt.Sprintf("workers=%d", workers)
		if workers == 0 {
			name = "workers=numcpu"
		}
		b.Run(name, func(b *testing.B) {
			const rows = 200000
			in := benchFixture(b, rows)
			conv := benchConverter(b, in, workers)
			b.ResetTimer()
			b.SetBytes(int64(rows))
			for i := 0; i < b.N; i++ {
				if _, err := conv.Run(context.Background(), discardSink{}, nil); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(rows*b.N)/b.Elapsed().Seconds(), "rows/s")
		})
	}
}

// BenchmarkConvert measures the whole pipeline including Parquet writing.
func BenchmarkConvert(b *testing.B) {
	for _, codec := range []string{"snappy", "zstd", "uncompressed"} {
		b.Run("compression="+codec, func(b *testing.B) {
			const rows = 200000
			in := benchFixture(b, rows)
			opts := testBenchOptions()
			opts.Compression = codec
			dir := b.TempDir()
			b.ResetTimer()
			var outBytes int64
			for i := 0; i < b.N; i++ {
				out := filepath.Join(dir, fmt.Sprintf("out-%d.parquet", i))
				st, _, err := Run(context.Background(), in, out, &opts, nil)
				if err != nil {
					b.Fatal(err)
				}
				outBytes = st.OutputBytes
				os.Remove(out)
			}
			b.ReportMetric(float64(rows*b.N)/b.Elapsed().Seconds(), "rows/s")
			b.ReportMetric(float64(outBytes), "output_bytes")
		})
	}
}

// BenchmarkQualityGate measures the overhead of each validation mode.
func BenchmarkQualityGate(b *testing.B) {
	for _, mode := range []QualityMode{QualityNone, QualityBasic, QualityNumeric, QualityFull} {
		b.Run("mode="+mode.String(), func(b *testing.B) {
			const rows = 100000
			in := benchFixture(b, rows)
			opts := testBenchOptions()
			opts.Quality = mode
			dir := b.TempDir()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out := filepath.Join(dir, fmt.Sprintf("out-%d.parquet", i))
				if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
					b.Fatal(err)
				}
				os.Remove(out)
			}
			b.ReportMetric(float64(rows*b.N)/b.Elapsed().Seconds(), "rows/s")
		})
	}
}

// BenchmarkBatchRows explores the batch size / row group tradeoff.
func BenchmarkBatchRows(b *testing.B) {
	for _, batch := range []int{4096, 16384, 65536, 262144} {
		b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
			const rows = 200000
			in := benchFixture(b, rows)
			opts := testBenchOptions()
			opts.BatchRows = batch
			dir := b.TempDir()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out := filepath.Join(dir, fmt.Sprintf("out-%d.parquet", i))
				if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
					b.Fatal(err)
				}
				os.Remove(out)
			}
			b.ReportMetric(float64(rows*b.N)/b.Elapsed().Seconds(), "rows/s")
		})
	}
}

func testBenchOptions() Options {
	o := DefaultOptions()
	o.Location = utc()
	o.TimezoneName = "UTC"
	o.ProgressEvery = 0
	o.Compression = "snappy"
	o.Force = true
	// The gate defaults to full, which reads the whole output back and digests
	// every cell -- and, through Options.Quality, also makes the decode
	// workers fingerprint each value. That is several times the cost of the
	// work these benchmarks exist to measure, so it would swamp the decode,
	// conversion and batch-size baselines the README quotes.
	// BenchmarkQualityGate sets the mode it is measuring and is unaffected.
	o.Quality = QualityNone
	return o
}

func benchConverter(b *testing.B, in string, workers int) *Converter {
	b.Helper()
	qf, err := qvd.Open(in)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { qf.Close() })
	if err := qf.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		b.Fatal(err)
	}
	opts := testBenchOptions()
	opts.Workers = workers
	if err := opts.Validate(); err != nil {
		b.Fatal(err)
	}
	rs, err := ResolveSchema(qf, &opts, nil)
	if err != nil {
		b.Fatal(err)
	}
	conv, err := NewConverter(qf, rs, &opts)
	if err != nil {
		b.Fatal(err)
	}
	return conv
}
