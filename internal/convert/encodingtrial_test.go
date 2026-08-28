package convert

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/parquet"
	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// keyTable builds a table shaped like a SAP extract with a Qlik composite
// primary key: one distinct value per row, 39 characters, either in document
// order or shuffled. The order is the whole question, since delta_byte_array
// stores each value against the row before it.
func keyTable(rows int, ordered bool) qvdtest.Table {
	keys := make([]qvd.Symbol, rows)
	for i := 0; i < rows; i++ {
		doc := 100000000 + i/3
		keys[i] = qvdtest.Str(fmt.Sprintf("CE10500|1000|%010d|%06d|%08d", doc, (i%3)*10+10, 20240101+i%1231))
	}
	if !ordered {
		r := rand.New(rand.NewSource(11))
		r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	}
	rowIdx := make([]int, rows)
	amounts := make([]int, rows)
	for i := range rowIdx {
		rowIdx[i] = i
		amounts[i] = i % 5
	}
	return qvdtest.Table{Name: "CE10500", Fields: []qvdtest.Field{
		{Name: "%CE10500_PKEY", Type: "ASCII", Rows: rowIdx, Symbols: keys},
		{Name: "Amount", Type: "INTEGER", Rows: amounts, Symbols: []qvd.Symbol{
			qvdtest.Int(1), qvdtest.Int(2), qvdtest.Int(3), qvdtest.Int(4), qvdtest.Int(5)}},
	}}
}

// The measurement has to find a win where one exists, and the recommendation
// has to be actionable: the column, the encoding, and what it measured.
func TestTrialFindsAWinOnAnOrderedKey(t *testing.T) {
	in := buildFixture(t, keyTable(30000, true))
	opts := testOptions()
	opts.Encodings, _ = ParseEncodingSpec("auto")

	rep, err := Inspect(in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()
	if rep.EncodingErr != nil {
		t.Fatalf("trial failed: %v", rep.EncodingErr)
	}

	if len(rep.Encodings.Trials) != 1 {
		t.Fatalf("trials = %+v, want only the key column: a five symbol integer "+
			"column cannot overflow a dictionary", rep.Encodings.Trials)
	}
	got := rep.Encodings.Trials[0]
	if got.Column != "%CE10500_PKEY" || got.Encoding != "delta_byte_array" {
		t.Errorf("trial = %+v", got)
	}
	if !got.Worthwhile() {
		t.Errorf("ratio %.2f on an ordered key should be worth adopting", got.Ratio())
	}
	if got.SampledRows != 30000 {
		t.Errorf("sampled %d rows, want the whole small file", got.SampledRows)
	}

	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"Columns that would compress better with a different encoding:",
		"%CE10500_PKEY  delta_byte_array, measured ",
		`pin them instead with --encoding "%CE10500_PKEY=delta_byte_array"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
}

// Refusing to recommend is the more important half. The same values in a
// different row order gain nothing, and a measurement that says so is what
// separates this from a rule of thumb.
func TestTrialDeclinesOnAScrambledKey(t *testing.T) {
	in := buildFixture(t, keyTable(30000, false))
	opts := testOptions()
	opts.Encodings, _ = ParseEncodingSpec("auto")

	rep, err := Inspect(in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()

	if len(rep.Encodings.Trials) != 1 {
		t.Fatalf("trials = %+v", rep.Encodings.Trials)
	}
	if rep.Encodings.Trials[0].Worthwhile() {
		t.Errorf("ratio %.2f on a shuffled key should not be recommended", rep.Encodings.Trials[0].Ratio())
	}
	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "none is worth changing") {
		t.Errorf("inspect should say the measurement found nothing:\n%s", sb.String())
	}
}

// Inspect reads no records unless the measurement is asked for, which is the
// promise that makes it a cheap pre-flight check on a twenty million row file.
func TestInspectReadsNoRecordsWithoutAuto(t *testing.T) {
	in := buildFixture(t, keyTable(30000, true))
	opts := testOptions()

	rep, err := Inspect(in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()
	if len(rep.Encodings.Trials) != 0 {
		t.Errorf("inspect measured without being asked: %+v", rep.Encodings.Trials)
	}
	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "compress better") {
		t.Errorf("unexpected recommendation:\n%s", sb.String())
	}
}

// What auto is for: the conversion measures and adopts, per file, with no
// pattern naming a column.
func TestAutoAdoptsTheMeasuredEncoding(t *testing.T) {
	in := buildFixture(t, keyTable(30000, true))
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	opts.Quality = QualityFull
	opts.Encodings, _ = ParseEncodingSpec("auto")

	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	stats, report, err := Run(context.Background(), in, out, &opts, logf)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("quality gate failed: %+v", report)
	}

	encs := columnEncodings(t, out)["%CE10500_PKEY"]
	if !hasEncoding(encs, parquet.Encodings.DeltaByteArray) {
		t.Errorf("the key was not written with the measured encoding: %v", encs)
	}
	if strings.Join(stats.Encodings, ",") != "%CE10500_PKEY=delta_byte_array" {
		t.Errorf("stats encodings = %v", stats.Encodings)
	}

	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"encoding: measured 1 column(s) on 30,000 sampled rows",
		"1 adopted, 0 left as they are",
		"encoding: %CE10500_PKEY  delta_byte_array, measured ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("log missing %q:\n%s", want, joined)
		}
	}
}

// An explicit rule is a decision. A measurement may not overrule it, or the
// flag would silently stop meaning what it says.
func TestExplicitRuleBeatsTheMeasurement(t *testing.T) {
	in := buildFixture(t, keyTable(30000, true))
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	opts.Encodings, _ = ParseEncodingSpec("auto,%*_PKEY=plain")

	stats, _, err := Run(context.Background(), in, out, &opts, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Join(stats.Encodings, ",") != "%CE10500_PKEY=plain" {
		t.Errorf("encodings = %v, want the explicit rule to hold", stats.Encodings)
	}
	encs := columnEncodings(t, out)["%CE10500_PKEY"]
	if hasEncoding(encs, parquet.Encodings.DeltaByteArray) {
		t.Errorf("the measurement overruled an explicit rule: %v", encs)
	}
}

// An explicit rule is a decision, so the measurement must not even offer an
// opinion on that column: a recommendation nothing may act on reads as the
// tool arguing with itself.
func TestPinnedColumnsAreNotMeasured(t *testing.T) {
	in := buildFixture(t, keyTable(30000, true))
	opts := testOptions()
	opts.Encodings, _ = ParseEncodingSpec("auto,%*_PKEY=plain")

	rep, err := Inspect(in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()
	for _, tr := range rep.Encodings.Trials {
		if tr.Column == "%CE10500_PKEY" {
			t.Errorf("a pinned column was measured and would be recommended: %+v", tr)
		}
	}
	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "%CE10500_PKEY  delta_byte_array") {
		t.Errorf("inspect recommended against an explicit rule:\n%s", sb.String())
	}
}

// A measured encoding has to reach the schema report. The report used to be
// written before the measurement ran, so a conversion could write a column one
// way while its report described another.
func TestSchemaReportCarriesAMeasuredEncoding(t *testing.T) {
	in := buildFixture(t, keyTable(30000, true))
	out := filepath.Join(t.TempDir(), "out.parquet")
	opts := testOptions()
	opts.Encodings, _ = ParseEncodingSpec("auto")
	opts.SchemaReportPath = filepath.Join(t.TempDir(), "schema.json")

	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatalf("Run: %v", err)
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
		if c.Name == "%CE10500_PKEY" {
			want = "delta_byte_array"
		}
		if c.Encoding != want {
			t.Errorf("report encoding for %s = %q, want %q", c.Name, c.Encoding, want)
		}
	}
}

// The windows are where the measurement gets its evidence, so their placement
// is worth pinning: three spread across a large file, one on a small one.
func TestTrialWindowPlacement(t *testing.T) {
	large := &qvd.File{NoOfRecords: 20_000_000, RecordByteSize: 232, RecordStart: 1000}
	chunks := trialChunks(large)
	if len(chunks) != trialWindows {
		t.Fatalf("got %d windows, want %d", len(chunks), trialWindows)
	}
	if chunks[0].StartRow != 0 {
		t.Errorf("first window starts at %d, want the head", chunks[0].StartRow)
	}
	if last := chunks[len(chunks)-1]; last.StartRow+int64(last.RowCount) != large.NoOfRecords {
		t.Errorf("last window ends at %d, want the tail at %d",
			last.StartRow+int64(last.RowCount), large.NoOfRecords)
	}
	for _, c := range chunks {
		if c.RowCount != trialWindowRows {
			t.Errorf("window %d covers %d rows, want %d", c.Index, c.RowCount, trialWindowRows)
		}
		want := large.RecordStart + c.StartRow*int64(large.RecordByteSize)
		if c.ByteOffset != want {
			t.Errorf("window %d reads at %d, want %d", c.Index, c.ByteOffset, want)
		}
	}

	// A file smaller than three windows is sampled once, over everything it
	// has. Row order is what the measurement turns on, so a sorted head and a
	// shuffled tail must never answer for each other: a 150,000 row file
	// judged on its first 100,000 alone could adopt the wrong encoding.
	for _, rows := range []int64{5000, 100_001, 150_000, 299_999, 300_000} {
		small := &qvd.File{NoOfRecords: rows, RecordByteSize: 8, RecordStart: 100}
		chunks = trialChunks(small)
		if len(chunks) != 1 {
			t.Errorf("%d rows gave %d windows, want one over everything", rows, len(chunks))
			continue
		}
		if int64(chunks[0].RowCount) != rows || chunks[0].StartRow != 0 {
			t.Errorf("%d rows: window covers %d rows from %d, want all of them",
				rows, chunks[0].RowCount, chunks[0].StartRow)
		}
	}
	if empty := trialChunks(&qvd.File{}); empty != nil {
		t.Errorf("an empty file has nothing to sample: %+v", empty)
	}
}

// A wide extract can have dozens of candidate columns, and holding every one's
// sampled rows at once would cost hundreds of megabytes to answer a question
// about file size. The measurement works in groups; what it reports must not
// depend on how the groups fell.
func TestTrialGroupsDoNotChangeTheAnswer(t *testing.T) {
	// Enough distinct long values per column to overflow a dictionary page,
	// which is what makes a column a candidate at all.
	rows := 25000
	fields := []qvdtest.Field{}
	rowIdx := make([]int, rows)
	for i := range rowIdx {
		rowIdx[i] = i
	}
	// More candidate columns than one group holds, each with enough distinct
	// long values to overflow a dictionary page.
	for c := 0; c < trialGroupColumns+3; c++ {
		syms := make([]qvd.Symbol, rows)
		for i := 0; i < rows; i++ {
			syms[i] = qvdtest.Str(fmt.Sprintf("COL%02d|%010d|%030d", c, i, i))
		}
		fields = append(fields, qvdtest.Field{
			Name: fmt.Sprintf("KEY%02d", c), Type: "ASCII", Rows: rowIdx, Symbols: syms})
	}
	in := buildFixture(t, qvdtest.Table{Name: "Wide", Fields: fields})

	opts := testOptions()
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

	trials, err := TrialEncodings(context.Background(), f, rs, &opts)
	if err != nil {
		t.Fatalf("TrialEncodings: %v", err)
	}
	if len(trials) != len(fields) {
		t.Fatalf("measured %d columns, want all %d", len(trials), len(fields))
	}
	// Every column here holds the same shape of value, so every one must come
	// out with the same verdict regardless of which group it landed in.
	for _, tr := range trials {
		if !tr.Worthwhile() {
			t.Errorf("%s measured %.2f, but every column here has the same shape",
				tr.Column, tr.Ratio())
		}
		if tr.SampledRows != int64(rows) {
			t.Errorf("%s sampled %d rows, want %d", tr.Column, tr.SampledRows, rows)
		}
	}
}

// The inspect JSON has to describe the same file the conversion would write,
// measurement included. It is what a scripted preflight reads, and a report
// that omits the measured encoding says the column is written a way it is not.
func TestInspectSchemaReportCarriesTheMeasurement(t *testing.T) {
	in := buildFixture(t, keyTable(30000, true))
	opts := testOptions()
	opts.Encodings, _ = ParseEncodingSpec("auto")
	opts.SchemaReportPath = filepath.Join(t.TempDir(), "inspect-schema.json")

	rep, err := Inspect(in, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()
	if err := WriteSchemaReport(opts.SchemaReportPath, in, rep.File, rep.Schema, &opts, rep.Encodings); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(opts.SchemaReportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report SchemaReport
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Columns {
		want := ""
		if c.Name == "%CE10500_PKEY" {
			want = "delta_byte_array"
		}
		if c.Encoding != want {
			t.Errorf("inspect report encoding for %s = %q, want %q", c.Name, c.Encoding, want)
		}
	}

	// A conversion with the same options must agree, which is the whole
	// claim inspect makes.
	out := filepath.Join(t.TempDir(), "out.parquet")
	convOpts := testOptions()
	convOpts.Encodings, _ = ParseEncodingSpec("auto")
	stats, _, err := Run(context.Background(), in, out, &convOpts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(stats.Encodings, ",") != "%CE10500_PKEY=delta_byte_array" {
		t.Errorf("the conversion chose %v, inspect predicted delta_byte_array", stats.Encodings)
	}
}

// An output that cannot be written must be refused before the work is done for
// it. Measuring first would spend a symbol pass and several sample windows to
// arrive at "already exists".
func TestExistingOutputIsRefusedBeforeMeasuring(t *testing.T) {
	in := buildFixture(t, keyTable(30000, true))
	out := filepath.Join(t.TempDir(), "taken.parquet")
	if err := os.WriteFile(out, []byte("not parquet"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := testOptions()
	opts.Encodings, _ = ParseEncodingSpec("auto")
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	_, _, err := Run(context.Background(), in, out, &opts, logf)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want an already-exists output error", err)
	}
	for _, l := range lines {
		if strings.Contains(l, "measured") || strings.Contains(l, "read ") {
			t.Errorf("work was done before refusing the output: %q", l)
		}
	}
	if b, _ := os.ReadFile(out); string(b) != "not parquet" {
		t.Error("the existing output was touched")
	}
}
