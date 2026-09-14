package convert

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/ralforion/qvd2parquet/internal/parquetwrite"
	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

func TestParseEncodingRules(t *testing.T) {
	spec, err := ParseEncodingSpec("%*_PKEY=delta_byte_array, Belege*=plain")
	if err != nil {
		t.Fatalf("ParseEncodingRules: %v", err)
	}
	rules := spec.Rules
	if len(rules) != 2 || spec.Auto {
		t.Fatalf("got %d rules (auto=%v), want 2 and no auto: %+v", len(rules), spec.Auto, spec)
	}
	if rules[0].Pattern != "%*_PKEY" || rules[0].Encoding != parquetwrite.EncodingDeltaByteArray {
		t.Errorf("first rule = %+v", rules[0])
	}
	if rules[1].Pattern != "Belege*" || rules[1].Encoding != parquetwrite.EncodingPlain {
		t.Errorf("second rule = %+v", rules[1])
	}
	if spec, err := ParseEncodingSpec("  "); err != nil || spec.Rules != nil || spec.Auto {
		t.Errorf("empty spec = %+v, %v; want nothing and no error", spec, err)
	}

	// "auto" composes with explicit rules, and an explicit rule wins.
	spec, err = ParseEncodingSpec("auto, %*_PKEY=plain")
	if err != nil || !spec.Auto || len(spec.Rules) != 1 {
		t.Errorf("ParseEncodingSpec(auto plus a rule) = %+v, %v", spec, err)
	}

	for _, bad := range []string{"nopattern", "=plain", "KEY=nosuchencoding", "KEY="} {
		if _, err := ParseEncodingSpec(bad); err == nil {
			t.Errorf("ParseEncodingRules(%q) should fail", bad)
		}
	}
}

// A pattern has to reach a column under either name: the output name is what a
// reader of the Parquet file sees, and the original QVD name is what a rule
// aimed at a folder of SAP tables can be written against.
func TestResolveEncodingsMatchesEitherName(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	renamer, err := NewFieldRenamer(sapRegex, "", "")
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Renamer = renamer

	f, err := qvd.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		t.Fatal(err)
	}
	rs, err := ResolveSchema(f, &opts, nil)
	if err != nil {
		t.Fatal(err)
	}

	// %A057_PKEY keeps its name; KSCHL is reached only under its output name,
	// since the original is "A057-||-KSCHL-||-Konditionsart".
	spec, err := ParseEncodingSpec("%*_PKEY=delta_byte_array,KSCHL=plain,Nothing*=plain")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := ResolveEncodings(spec.Rules, rs, f)
	if err != nil {
		t.Fatalf("ResolveEncodings: %v", err)
	}
	if got := enc.ByColumn["%A057_PKEY"]; got != parquetwrite.EncodingDeltaByteArray {
		t.Errorf("%%A057_PKEY = %q, want delta_byte_array", got)
	}
	if got := enc.ByColumn["KSCHL"]; got != parquetwrite.EncodingPlain {
		t.Errorf("KSCHL = %q, want plain", got)
	}
	if len(enc.ByColumn) != 2 {
		t.Errorf("pinned %v, want exactly the two columns", enc.ByColumn)
	}
	if strings.Join(enc.Unmatched, ",") != "Nothing*" {
		t.Errorf("unmatched = %v, want [Nothing*]", enc.Unmatched)
	}
}

// A later rule wins, so a broad pattern can carry a specific exception.
func TestResolveEncodingsLastRuleWins(t *testing.T) {
	rs := &ResolvedSchema{Columns: []ResolvedColumn{
		{Name: "A", ArrowType: arrowString, SourceIndex: 0},
		{Name: "B", ArrowType: arrowString, SourceIndex: 0},
	}}
	f := &qvd.File{Columns: []qvd.Column{{Name: "A"}}}

	spec, err := ParseEncodingSpec("*=delta_byte_array,B=plain")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := ResolveEncodings(spec.Rules, rs, f)
	if err != nil {
		t.Fatal(err)
	}
	if enc.ByColumn["A"] != parquetwrite.EncodingDeltaByteArray || enc.ByColumn["B"] != parquetwrite.EncodingPlain {
		t.Errorf("resolved = %v", enc.ByColumn)
	}
	if strings.Join(enc.Pinned, " ") != "A=delta_byte_array B=plain" {
		t.Errorf("pinned = %v", enc.Pinned)
	}
}

// An encoding the column's type cannot carry is refused with the reason, not
// passed to the Parquet library to ignore or fail on obscurely.
func TestResolveEncodingsRejectsAMismatchedType(t *testing.T) {
	rs := &ResolvedSchema{Columns: []ResolvedColumn{
		{Name: "DATBI", ArrowType: arrowInt64, SourceIndex: 0},
	}}
	f := &qvd.File{Columns: []qvd.Column{{Name: "DATBI"}}}

	spec, _ := ParseEncodingSpec("DATBI=delta_byte_array")
	_, err := ResolveEncodings(spec.Rules, rs, f)
	if err == nil {
		t.Fatal("delta_byte_array on an int64 column should be refused")
	}
	for _, want := range []string{"does not fit", "int64", "delta_binary_packed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}

	// The same encoding on a string column is fine, and delta_binary_packed
	// on a date column is too: a date is an integer with a logical type.
	ok := &ResolvedSchema{Columns: []ResolvedColumn{
		{Name: "S", ArrowType: arrowString, SourceIndex: 0},
		{Name: "D", ArrowType: arrowDate32, SourceIndex: 0},
	}}
	spec, _ = ParseEncodingSpec("S=delta_byte_array,D=delta_binary_packed")
	if _, err := ResolveEncodings(spec.Rules, ok, f); err != nil {
		t.Errorf("ResolveEncodings: %v", err)
	}
}

// The point of the flag: the encoding actually reaches the file, the
// dictionary goes with it, and the values survive the round trip.
func TestPinnedEncodingReachesTheFile(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	out := filepath.Join(t.TempDir(), "out.parquet")

	opts := testOptions()
	opts.Quality = QualityFull
	opts.SchemaReportPath = filepath.Join(t.TempDir(), "schema.json")
	spec, err := ParseEncodingSpec("%A057_PKEY=delta_byte_array")
	if err != nil {
		t.Fatal(err)
	}
	opts.Encodings = spec

	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	_, report, err := Run(context.Background(), in, out, &opts, logf)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("quality gate failed: %+v", report)
	}

	encodings := columnEncodings(t, out)
	got := encodings["%A057_PKEY"]
	if !hasEncoding(got, parquet.Encodings.DeltaByteArray) {
		t.Errorf("%%A057_PKEY encodings = %v, want DELTA_BYTE_ARRAY", got)
	}
	if hasEncoding(got, parquet.Encodings.RLEDict) {
		t.Errorf("%%A057_PKEY still carries a dictionary: %v", got)
	}
	// An unpinned column of the same type keeps the default.
	if plain := encodings["PlainField"]; hasEncoding(plain, parquet.Encodings.DeltaByteArray) {
		t.Errorf("PlainField should keep the default encoding, got %v", plain)
	}

	var summary string
	for _, l := range lines {
		if strings.HasPrefix(l, "encoding: ") {
			summary = l
		}
	}
	if summary != "encoding: %A057_PKEY=delta_byte_array" {
		t.Errorf("log line = %q", summary)
	}

	b, err := os.ReadFile(opts.SchemaReportPath)
	if err != nil {
		t.Fatal(err)
	}
	var rep SchemaReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Columns {
		want := ""
		if c.Name == "%A057_PKEY" {
			want = "delta_byte_array"
		}
		if c.Encoding != want {
			t.Errorf("report encoding for %s = %q, want %q", c.Name, c.Encoding, want)
		}
	}
}

// Without the flag nothing changes, which is what the compatibility promise
// rests on.
func TestDefaultEncodingIsUnchanged(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatal(err)
	}
	for name, encs := range columnEncodings(t, out) {
		if hasEncoding(encs, parquet.Encodings.DeltaByteArray) {
			t.Errorf("column %q was written as delta_byte_array without being asked: %v", name, encs)
		}
	}
}

// A pattern reaching no column is reported rather than rejected, as with
// --exclude: one command line covers a folder of tables with differing fields.
func TestEncodingPatternMatchingNothingIsANote(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	opts.Encodings, _ = ParseEncodingSpec("NoSuchColumn*=plain")

	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	if _, _, err := Run(context.Background(), in, out, &opts, logf); err != nil {
		t.Fatalf("Run: %v", err)
	}
	noted := false
	for _, l := range lines {
		if strings.Contains(l, `--encoding "NoSuchColumn*" matched no column`) {
			noted = true
		}
	}
	if !noted {
		t.Errorf("no note about the dead pattern: %v", lines)
	}
}

// Inspect predicts the conversion, so it has to show the pins and refuse the
// same mismatches rather than letting a long run discover them.
func TestInspectShowsEncodings(t *testing.T) {
	in := buildFixture(t, sapStyleTable())
	opts := testOptions()
	opts.Encodings, _ = ParseEncodingSpec("%A057_PKEY=delta_byte_array,Dead*=plain")

	rep, err := Inspect(context.Background(), in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()
	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"Encoding        %A057_PKEY=delta_byte_array",
		`Encoding        "Dead*" matched no column`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}

	bad := testOptions()
	bad.Encodings, _ = ParseEncodingSpec("%SYS_TS=delta_byte_array")
	badRep, err := Inspect(context.Background(), in, &bad)
	if err != nil {
		t.Fatal(err)
	}
	defer badRep.Close()
	if badRep.EncodingErr == nil {
		t.Fatal("an int column pinned to delta_byte_array should be refused")
	}
	sb.Reset()
	if err := badRep.Write(&sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "Encoding        cannot be applied:") {
		t.Errorf("inspect should explain the refusal:\n%s", sb.String())
	}
}

// columnEncodings reads back the encodings each column chunk was written with.
func columnEncodings(t *testing.T, path string) map[string][]parquet.Encoding {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rdr, err := file.NewParquetReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()

	out := map[string][]parquet.Encoding{}
	for rg := 0; rg < rdr.NumRowGroups(); rg++ {
		meta := rdr.MetaData().RowGroup(rg)
		for c := 0; c < meta.NumColumns(); c++ {
			chunk, err := meta.ColumnChunk(c)
			if err != nil {
				t.Fatal(err)
			}
			name := meta.Schema.Column(c).Name()
			out[name] = append(out[name], chunk.Encodings()...)
		}
	}
	return out
}

func hasEncoding(encs []parquet.Encoding, want parquet.Encoding) bool {
	for _, e := range encs {
		if e == want {
			return true
		}
	}
	return false
}

// The QVD header states every field's symbol count, so a column whose symbols
// cannot fit the dictionary page is written plain from the first row, and a
// column that fits keeps its dictionary. The writer's own fallback reaches
// plain too, but only after a full dictionary page and indices per row group.
func TestDictionaryIsDecidedFromTheSymbolTable(t *testing.T) {
	in := buildFixture(t, keyTable(30000, true))
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatal(err)
	}
	encodings := columnEncodings(t, out)
	key := encodings["%CE10500_PKEY"]
	if hasEncoding(key, parquet.Encodings.RLEDict) || !hasEncoding(key, parquet.Encodings.Plain) {
		t.Errorf("30000 distinct 39-character values should be plain from the start, got %v", key)
	}
	if amount := encodings["Amount"]; !hasEncoding(amount, parquet.Encodings.RLEDict) {
		t.Errorf("a five symbol column should keep its dictionary, got %v", amount)
	}

	// A rule naming the dictionary wins over the symbol table.
	pinned := testOptions()
	pinned.Encodings, _ = ParseEncodingSpec("%*_PKEY=dictionary")
	out2 := filepath.Join(t.TempDir(), "pinned.parquet")
	if _, _, err := Run(context.Background(), in, out2, &pinned, nil); err != nil {
		t.Fatal(err)
	}
	if key := columnEncodings(t, out2)["%CE10500_PKEY"]; !hasEncoding(key, parquet.Encodings.RLEDict) {
		t.Errorf("KEY=dictionary should keep the dictionary, got %v", key)
	}
}

// skewedTable holds a column of twenty thousand short codes and one long
// outlier, repeated over ten times as many rows. Charged at its longest
// value the dictionary would be 1.3 MB and ruled out; counted, it is 180 KB.
func skewedTable(rows int) qvdtest.Table {
	const distinct = 20_000
	syms := make([]qvd.Symbol, distinct)
	for i := range syms {
		syms[i] = qvdtest.Str(fmt.Sprintf("%05d", i))
	}
	syms[distinct-1] = qvdtest.Str(strings.Repeat("x", 60))
	idx := make([]int, rows)
	for i := range idx {
		idx[i] = i % distinct
	}
	return qvdtest.Table{Name: "SKEW", Fields: []qvdtest.Field{
		{Name: "Code", Type: "ASCII", Rows: idx, Symbols: syms},
	}}
}

// The dictionary is sized from the symbols' actual text, not from every
// symbol at the column's longest, and a column that keeps it does so under
// every compression: without a codec the writer would otherwise still judge
// from its first rows, when nearly every value is new, and drop it.
func TestDictionaryIsKeptWhereItPays(t *testing.T) {
	in := buildFixture(t, skewedTable(200_000))
	for _, compression := range []string{"snappy", "uncompressed"} {
		out := filepath.Join(t.TempDir(), compression+".parquet")
		opts := testOptions()
		opts.Compression = compression
		if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
			t.Fatal(err)
		}
		if code := columnEncodings(t, out)["Code"]; !hasEncoding(code, parquet.Encodings.RLEDict) {
			t.Errorf("%s: 20,000 short codes over 200,000 rows should keep their dictionary, got %v", compression, code)
		}
	}
}

// clusteredTable repeats each of a hundred thousand keys five times in a
// row. The symbol count exceeds the rows of a row group, so a rule reading
// only the count would write it plain, yet each row group holds a fifth of
// its rows in dictionary and the dictionary wins by more than three to one
// without compression. Only the row order could tell, so the symbol table
// must leave the writer's default in place.
func clusteredTable() qvdtest.Table {
	const distinct, repeat = 100_000, 5
	syms := make([]qvd.Symbol, distinct)
	for i := range syms {
		syms[i] = qvdtest.Str(fmt.Sprintf("K%011d", i))
	}
	idx := make([]int, distinct*repeat)
	for i := range idx {
		idx[i] = i / repeat
	}
	return qvdtest.Table{Name: "CLUSTER", Fields: []qvdtest.Field{
		{Name: "Key", Type: "ASCII", Rows: idx, Symbols: syms},
	}}
}

func TestUnsettledColumnKeepsTheWriterDefault(t *testing.T) {
	in := buildFixture(t, clusteredTable())
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	opts.Compression = "uncompressed"
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatal(err)
	}
	if key := columnEncodings(t, out)["Key"]; !hasEncoding(key, parquet.Encodings.RLEDict) {
		t.Errorf("clustered repeats should keep the writer's dictionary, got %v", key)
	}
}

// nearlyDistinctTable holds two hundred thousand integer symbols over two
// hundred and seventy-five thousand sorted rows, so most values appear once
// and a row group holds about forty-eight thousand distinct ones. Priced at
// the width the global count needs, eighteen bits an index, the best case
// for a dictionary comes out no better than plain; priced at the sixteen
// bits a row group needs, it comes out ahead, and only the row order can
// tell, so the writer's default must stand.
func nearlyDistinctTable() qvdtest.Table {
	const symbols, rows = 200_000, 275_000
	syms := make([]qvd.Symbol, symbols)
	for i := range syms {
		syms[i] = qvdtest.Int(int64(i))
	}
	idx := make([]int, rows)
	for i := range idx {
		idx[i] = i * symbols / rows
	}
	return qvdtest.Table{Name: "NEARLY", Fields: []qvdtest.Field{
		{Name: "Key", Type: "INTEGER", Rows: idx, Symbols: syms},
	}}
}

func TestIndexWidthIsPricedPerRowGroup(t *testing.T) {
	in := buildFixture(t, nearlyDistinctTable())
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	opts.Compression = "uncompressed"
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatal(err)
	}
	if key := columnEncodings(t, out)["Key"]; !hasEncoding(key, parquet.Encodings.RLEDict) {
		t.Errorf("sorted nearly distinct integers should keep the writer's dictionary, got %v", key)
	}
}
