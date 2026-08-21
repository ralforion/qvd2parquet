package convert

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/parquetwrite"
	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// folderFixture builds a directory of QVDs, optionally with a nested one and a
// deliberately corrupt file.
func folderFixture(t *testing.T, withNested, withBroken bool) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "in")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.qvd", "b.qvd"} {
		if _, err := qvdtest.Build(filepath.Join(src, name), sampleTable(50)); err != nil {
			t.Fatal(err)
		}
	}
	// A non-QVD file in the same directory must be ignored.
	if err := os.WriteFile(filepath.Join(src, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if withNested {
		if _, err := qvdtest.Build(filepath.Join(src, "sub", "nested.qvd"), sampleTable(20)); err != nil {
			t.Fatal(err)
		}
	}
	if withBroken {
		if err := os.WriteFile(filepath.Join(src, "broken.qvd"), []byte("not a qvd"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func TestFindInputs(t *testing.T) {
	src := folderFixture(t, true, false)

	flat, err := FindInputs([]string{src}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(flat) != 2 {
		t.Errorf("non-recursive found %d files, want 2 (and no notes.txt): %v", len(flat), flat)
	}

	deep, err := FindInputs([]string{src}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(deep) != 3 {
		t.Errorf("recursive found %d files, want 3: %v", len(deep), deep)
	}

	// A named file is taken literally, whatever its extension.
	other := filepath.Join(t.TempDir(), "odd.name")
	if _, err := qvdtest.Build(other, sampleTable(5)); err != nil {
		t.Fatal(err)
	}
	got, err := FindInputs([]string{other}, false)
	if err != nil || len(got) != 1 {
		t.Errorf("explicit file: %v, %v", got, err)
	}

	// The same file named twice is converted once.
	dup, err := FindInputs([]string{other, other}, false)
	if err != nil || len(dup) != 1 {
		t.Errorf("duplicate inputs: %v, %v", dup, err)
	}

	if _, err := FindInputs([]string{filepath.Join(src, "nope")}, false); err == nil {
		t.Error("a missing path should be an error")
	}
}

func TestOutputPathFor(t *testing.T) {
	for _, tc := range []struct{ in, dir, want string }{
		{"a.qvd", "out", filepath.Join("out", "a.parquet")},
		{filepath.Join("x", "y", "b.QVD"), "out", filepath.Join("out", "b.parquet")},
		{"no-extension", "out", filepath.Join("out", "no-extension.parquet")},
	} {
		if got := OutputPathFor(tc.in, tc.dir); got != tc.want {
			t.Errorf("OutputPathFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A failing file must not stop the others, and the run must still report failure.
func TestRunManyContinuesPastAFailure(t *testing.T) {
	src := folderFixture(t, false, true)
	outDir := filepath.Join(t.TempDir(), "out")

	inputs, err := FindInputs([]string{src}, false)
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	b, err := RunMany(context.Background(), inputs, &opts, &ManyOptions{OutDir: outDir}, nil)
	if err != nil {
		t.Fatalf("RunMany: %v", err)
	}
	if b.Converted != 2 || b.Failed != 1 {
		t.Errorf("converted=%d failed=%d, want 2 and 1", b.Converted, b.Failed)
	}
	if b.Rows != 100 {
		t.Errorf("rows = %d, want 100", b.Rows)
	}
	// The good files are on disk despite the bad one.
	for _, name := range []string{"a.parquet", "b.parquet"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
	if code := b.ExitCode(testExitCode); code == 0 {
		t.Error("a run with a failure must not exit 0")
	}
	if !strings.Contains(b.Summary(), "FAILED (1)") {
		t.Errorf("summary should list the failure:\n%s", b.Summary())
	}
}

// The exit code reports the most actionable failure, not merely the last one.
func TestBatchExitCodeRanksFailures(t *testing.T) {
	b := &BatchResult{Results: []FileResult{
		{Err: ErrInput},
		{Err: ErrSchemaPolicy},
		{Err: parquetwrite.ErrOutput},
	}}
	if got := b.ExitCode(testExitCode); got != 3 {
		t.Errorf("exit code = %d, want 3 (the schema policy error)", got)
	}
	if got := (&BatchResult{}).ExitCode(testExitCode); got != 0 {
		t.Errorf("an empty run exits %d, want 0", got)
	}
	clean := &BatchResult{Results: []FileResult{{}, {}}}
	if got := clean.ExitCode(testExitCode); got != 0 {
		t.Errorf("a clean run exits %d, want 0", got)
	}
}

func testExitCode(err error) int {
	switch {
	case errors.Is(err, ErrQualityGate):
		return 6
	case errors.Is(err, parquetwrite.ErrOutput):
		return 5
	case errors.Is(err, ErrSchemaPolicy):
		return 3
	case errors.Is(err, qvd.ErrUnsupported):
		return 2
	case errors.Is(err, ErrInput):
		return 4
	}
	return 4
}

// Two inputs mapping to one output must be refused before anything is written,
// because --force would otherwise silently overwrite the first result.
func TestRunManyRejectsOutputCollisions(t *testing.T) {
	src := folderFixture(t, true, false)
	// sub/nested.qvd and a same-named file at the top collide.
	if _, err := qvdtest.Build(filepath.Join(src, "nested.qvd"), sampleTable(5)); err != nil {
		t.Fatal(err)
	}
	inputs, err := FindInputs([]string{src}, true)
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "out")

	opts := testOptions()
	opts.Force = true // must not paper over the collision
	_, err = RunMany(context.Background(), inputs, &opts, &ManyOptions{OutDir: outDir}, nil)
	if !errors.Is(err, parquetwrite.ErrOutput) {
		t.Fatalf("err = %v, want ErrOutput", err)
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Errorf("error should name the problem: %v", err)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Errorf("nothing should have been written, found %d entries", len(entries))
	}
}

// Converting several files at once must not multiply the decode workers.
func TestSplitWorkerBudget(t *testing.T) {
	for _, tc := range []struct {
		fileWorkers, decode, files int
		wantFiles, wantPerFile     int
	}{
		{1, 16, 10, 1, 16},
		{4, 16, 10, 4, 4},
		{8, 16, 10, 8, 2},
		{32, 16, 10, 10, 1}, // capped at the number of files
		{4, 2, 10, 4, 1},    // never below one worker
		{0, 16, 10, 1, 16},  // unset means sequential
	} {
		gotFiles, gotPer := splitWorkerBudget(tc.fileWorkers, tc.decode, tc.files)
		if gotFiles != tc.wantFiles || gotPer != tc.wantPerFile {
			t.Errorf("splitWorkerBudget(%d, %d, %d) = %d, %d; want %d, %d",
				tc.fileWorkers, tc.decode, tc.files, gotFiles, gotPer,
				tc.wantFiles, tc.wantPerFile)
		}
		if gotFiles*gotPer > tc.decode && tc.decode >= tc.fileWorkers {
			t.Errorf("budget oversubscribed: %d files x %d workers > %d",
				gotFiles, gotPer, tc.decode)
		}
	}
}

// Files convert correctly when several run at once.
func TestRunManyConcurrent(t *testing.T) {
	src := folderFixture(t, true, false)
	inputs, err := FindInputs([]string{src}, true)
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "out")
	opts := testOptions()
	opts.Quality = QualityFull

	b, err := RunMany(context.Background(), inputs, &opts,
		&ManyOptions{OutDir: outDir, FileWorkers: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Failed != 0 || b.Converted != 3 {
		t.Fatalf("converted=%d failed=%d, want 3 and 0: %s", b.Converted, b.Failed, b.Summary())
	}
	for _, r := range b.Results {
		if r.Quality == nil || !r.Quality.Passed {
			t.Errorf("%s did not pass its quality gate", r.Input)
		}
	}
}

// The log must be machine-readable, one record per file plus a summary.
func TestLogWriterRecords(t *testing.T) {
	src := folderFixture(t, false, true)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.jsonl")

	log, err := NewLogWriter(logPath)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := FindInputs([]string{src}, false)
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Quality = QualityNumeric
	if _, err := RunMany(context.Background(), inputs, &opts,
		&ManyOptions{OutDir: filepath.Join(dir, "out"), Log: log}, nil); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 { // 3 files + summary
		t.Fatalf("got %d log lines, want 4:\n%s", len(lines), raw)
	}

	var files, failed int
	for _, l := range lines[:len(lines)-1] {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("line is not JSON: %v\n%s", err, l)
		}
		if rec["type"] != "file" {
			t.Errorf("expected a file record, got %v", rec["type"])
		}
		files++
		if rec["status"] == "failed" {
			failed++
			if rec["error"] == "" || rec["error"] == nil {
				t.Error("a failed record must carry the reason")
			}
		} else if rec["qualityPassed"] != true {
			t.Errorf("a converted record should report its quality verdict: %v", rec)
		}
	}
	if files != 3 || failed != 1 {
		t.Errorf("log has %d file records with %d failures, want 3 and 1", files, failed)
	}

	var summary map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["type"] != "summary" {
		t.Fatalf("last line is not the summary: %v", summary)
	}
	if summary["converted"] != float64(2) || summary["failed"] != float64(1) {
		t.Errorf("summary = %v", summary)
	}
}

// A per-file report path must not have every file overwrite one document.
func TestPerFileReportPaths(t *testing.T) {
	got := perFileReport(filepath.Join("reports", "schema.json"), filepath.Join("in", "sales.qvd"), "out")
	want := filepath.Join("reports", "sales.schema.json")
	if got != want {
		t.Errorf("perFileReport = %q, want %q", got, want)
	}
	// A bare filename lands beside the outputs.
	if got := perFileReport("quality.json", "sales.qvd", "out"); got != filepath.Join("out", "sales.quality.json") {
		t.Errorf("bare name = %q", got)
	}
	if got := perFileReport("", "sales.qvd", "out"); got != "" {
		t.Errorf("no report configured should stay empty, got %q", got)
	}
}

// Every record must carry every field, including the empty ones. If a field
// were omitted when empty, its column would be absent from a run where no
// record happened to carry it -- so `where status='failed'` would fail to bind
// on a run with no failures, breaking the monitoring query exactly when
// everything worked.
func TestLogSchemaIsStableOnACleanRun(t *testing.T) {
	src := folderFixture(t, false, false) // no broken file: nothing fails
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.jsonl")

	log, err := NewLogWriter(logPath)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := FindInputs([]string{src}, false)
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Quality = QualityNumeric
	if _, err := RunMany(context.Background(), inputs, &opts,
		&ManyOptions{OutDir: filepath.Join(dir, "out"), Log: log}, nil); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"type", "time", "input", "output", "status", "error", "elapsedMs",
		"rows", "columns", "outputBytes", "symbolsRead", "rowsPerSecond",
		"qualityMode", "qualityPassed", "qualityErrors",
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["type"] != "file" {
			continue
		}
		if rec["status"] != "ok" {
			t.Fatalf("fixture assumption wrong: %v", rec)
		}
		for _, k := range want {
			if _, ok := rec[k]; !ok {
				t.Errorf("field %q is missing from a successful record; a query "+
					"selecting it would not bind on a clean run:\n%s", k, line)
			}
		}
	}
}
