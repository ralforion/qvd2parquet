package convert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
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

	foundFlat := FindInputs([]string{src}, InputSelection{})
	flat, problems := foundFlat.Files, foundFlat.Problems
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if len(flat) != 2 {
		t.Errorf("non-recursive found %d files, want 2 (and no notes.txt): %v", len(flat), flat)
	}

	deep := FindInputs([]string{src}, InputSelection{Recursive: true}).Files
	if len(deep) != 3 {
		t.Errorf("recursive found %d files, want 3: %v", len(deep), deep)
	}

	// A named file is taken literally, whatever its extension.
	other := filepath.Join(t.TempDir(), "odd.name")
	if _, err := qvdtest.Build(other, sampleTable(5)); err != nil {
		t.Fatal(err)
	}
	got := FindInputs([]string{other}, InputSelection{}).Files
	if len(got) != 1 {
		t.Errorf("explicit file: %v", got)
	}

	// The same file named twice is converted once.
	dup := FindInputs([]string{other, other}, InputSelection{}).Files
	if len(dup) != 1 {
		t.Errorf("duplicate inputs: %v", dup)
	}

	// A missing path is a reported problem, not an abort: the inputs beside it
	// must still convert.
	foundFound := FindInputs([]string{filepath.Join(src, "nope")}, InputSelection{})
	found, problems := foundFound.Files, foundFound.Problems
	if len(found) != 0 || len(problems) != 1 {
		t.Errorf("missing path gave %v, %v", found, problems)
	}
	if !errors.Is(problems[0].Err, ErrInput) {
		t.Errorf("problem err = %v, want ErrInput", problems[0].Err)
	}
}

func TestFindInputsNamePatterns(t *testing.T) {
	src := folderFixture(t, true, false)

	// A pattern is matched against the name with and without the extension,
	// so all three of these forms reach the same file.
	for _, pat := range []string{"a", "a.qvd", "A*"} {
		got := FindInputs([]string{src}, InputSelection{Include: []string{pat}})
		if len(got.Files) != 1 || filepath.Base(got.Files[0]) != "a.qvd" {
			t.Errorf("--include-files %q gave %v", pat, got.Files)
		}
		if got.Filtered != 1 {
			t.Errorf("--include-files %q filtered %d, want 1", pat, got.Filtered)
		}
	}

	// Exclude wins over include, so a broad include can be narrowed.
	got := FindInputs([]string{src}, InputSelection{
		Recursive: true,
		Include:   []string{"*"},
		Exclude:   []string{"b"},
	})
	if len(got.Files) != 2 {
		t.Errorf("include * exclude b gave %v", got.Files)
	}
	for _, f := range got.Files {
		if filepath.Base(f) == "b.qvd" {
			t.Errorf("excluded file survived: %v", got.Files)
		}
	}

	// A file named on the command line was meant, so the patterns leave it
	// alone; only what a directory offers is filtered.
	named := filepath.Join(src, "b.qvd")
	if got := FindInputs([]string{named}, InputSelection{Include: []string{"a"}}); len(got.Files) != 1 {
		t.Errorf("named file was filtered: %v", got.Files)
	}
}

func TestFindInputsReportsPatternsThatMatchedNothing(t *testing.T) {
	src := folderFixture(t, false, false)

	got := FindInputs([]string{src}, InputSelection{
		Include: []string{"a", "ZZ*"},
		Exclude: []string{"QQ*"},
	})
	if len(got.Files) != 1 {
		t.Fatalf("files = %v, want just a.qvd", got.Files)
	}
	want := []string{"ZZ*", "QQ*"}
	if !reflect.DeepEqual(got.Unmatched, want) {
		t.Errorf("unmatched = %v, want %v", got.Unmatched, want)
	}
	notes := got.Notes()
	if len(notes) != 2 {
		t.Fatalf("notes = %v, want a pattern note and a dropped-count note", notes)
	}
	if !strings.Contains(notes[0], `"ZZ*"`) || !strings.Contains(notes[1], "1 file(s) dropped") {
		t.Errorf("notes = %v", notes)
	}

	// A pattern that only ever matched a file another pattern excluded still
	// counts as used: it is not the one that is wrong.
	both := FindInputs([]string{src}, InputSelection{Include: []string{"a"}, Exclude: []string{"a"}})
	if len(both.Files) != 0 {
		t.Errorf("exclude did not win: %v", both.Files)
	}
	if len(both.Unmatched) != 0 {
		t.Errorf("unmatched = %v, want none", both.Unmatched)
	}
}

func TestFindInputsExpandsWildcardPath(t *testing.T) {
	src := folderFixture(t, true, false)

	// What cmd.exe and PowerShell hand over verbatim, since neither expands
	// for an external command.
	got := FindInputs([]string{filepath.Join(src, "*.qvd")}, InputSelection{})
	if len(got.Problems) != 0 {
		t.Fatalf("unexpected problems: %v", got.Problems)
	}
	if len(got.Files) != 2 {
		t.Errorf("expanded to %v, want a.qvd and b.qvd", got.Files)
	}

	// Case-insensitively, as every other pattern in the tool matches.
	if got := FindInputs([]string{filepath.Join(src, "*.QVD")}, InputSelection{}); len(got.Files) != 2 {
		t.Errorf("uppercase pattern expanded to %v", got.Files)
	}

	// A wildcard takes .qvd files and directories, so it does not sweep up
	// the notes.txt or the .parquet files written beside them.
	star := FindInputs([]string{filepath.Join(src, "*")}, InputSelection{Recursive: true})
	if len(star.Files) != 3 {
		t.Errorf("* expanded to %v, want both files and the nested one", star.Files)
	}

	// The patterns still apply to a directory the wildcard reached.
	sub := FindInputs([]string{filepath.Join(src, "*")}, InputSelection{
		Recursive: true,
		Exclude:   []string{"nested"},
	})
	if len(sub.Files) != 2 {
		t.Errorf("filtered wildcard walk gave %v", sub.Files)
	}

	// A pattern matching nothing is a reported problem, like any other path
	// that could not be used.
	none := FindInputs([]string{filepath.Join(src, "ZZ*.qvd")}, InputSelection{})
	if len(none.Files) != 0 || len(none.Problems) != 1 {
		t.Fatalf("no-match pattern gave %v, %v", none.Files, none.Problems)
	}
	if !errors.Is(none.Problems[0].Err, ErrInput) {
		t.Errorf("problem err = %v, want ErrInput", none.Problems[0].Err)
	}

	// A wildcard in a directory element is refused with a message, rather
	// than half-working.
	for _, pattern := range []string{filepath.Join(src, "*", "*.qvd"), filepath.Join(src, "*", "a.qvd")} {
		deep := FindInputs([]string{pattern}, InputSelection{})
		if len(deep.Problems) != 1 || !strings.Contains(deep.Problems[0].Err.Error(), "last element") {
			t.Errorf("directory wildcard %q gave %v", pattern, deep.Problems)
		}
	}

	// An existing file whose name contains a wildcard character is still
	// taken literally: expansion only happens once the path itself fails.
	if runtime.GOOS != "windows" {
		odd := filepath.Join(t.TempDir(), "odd*name.qvd")
		if _, err := qvdtest.Build(odd, sampleTable(5)); err != nil {
			t.Fatal(err)
		}
		if got := FindInputs([]string{odd}, InputSelection{}); len(got.Files) != 1 {
			t.Errorf("literal path with a wildcard character gave %v, %v", got.Files, got.Problems)
		}
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

	inputs := FindInputs([]string{src}, InputSelection{}).Files
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
	inputs := FindInputs([]string{src}, InputSelection{Recursive: true}).Files
	outDir := filepath.Join(t.TempDir(), "out")

	opts := testOptions()
	opts.Force = true // must not paper over the collision
	_, err := RunMany(context.Background(), inputs, &opts, &ManyOptions{OutDir: outDir}, nil)
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

	// An unset --workers divides the automatic default, not one per CPU, so
	// batch mode and single-file mode agree on how much of the machine to use.
	files, perFile := splitWorkerBudget(2, 0, 10)
	if want := DefaultWorkers() / 2; files != 2 || perFile != max(want, 1) {
		t.Errorf("splitWorkerBudget(2, 0, 10) = %d, %d; want 2, %d",
			files, perFile, max(want, 1))
	}
}

// Files convert correctly when several run at once.
func TestRunManyConcurrent(t *testing.T) {
	src := folderFixture(t, true, false)
	inputs := FindInputs([]string{src}, InputSelection{Recursive: true}).Files
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
	inputs := FindInputs([]string{src}, InputSelection{}).Files
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
	got := PerFileReportPath(filepath.Join("reports", "schema.json"), filepath.Join("in", "sales.qvd"), "out")
	want := filepath.Join("reports", "sales.schema.json")
	if got != want {
		t.Errorf("PerFileReportPath = %q, want %q", got, want)
	}
	// A bare filename lands beside the outputs.
	if got := PerFileReportPath("quality.json", "sales.qvd", "out"); got != filepath.Join("out", "sales.quality.json") {
		t.Errorf("bare name = %q", got)
	}
	if got := PerFileReportPath("", "sales.qvd", "out"); got != "" {
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
	inputs := FindInputs([]string{src}, InputSelection{}).Files
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
		"excludeNoMatch", "fieldsRenamed", "fieldsUnchanged", "encodings",
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

// A path that cannot be examined must be reported as a failed file, not abort
// the run: the batch guarantee is that every input is attempted.
func TestRunManyReportsUnreadableInputs(t *testing.T) {
	src := folderFixture(t, false, false)
	outDir := filepath.Join(t.TempDir(), "out")

	foundGood := FindInputs([]string{src, filepath.Join(src, "nope.qvd")}, InputSelection{})
	good, problems := foundGood.Files, foundGood.Problems
	if len(good) != 2 || len(problems) != 1 {
		t.Fatalf("found %v with problems %v", good, problems)
	}

	opts := testOptions()
	b, err := RunMany(context.Background(), good, &opts,
		&ManyOptions{OutDir: outDir, Problems: problems}, nil)
	if err != nil {
		t.Fatalf("RunMany: %v", err)
	}
	if b.Converted != 2 {
		t.Errorf("converted = %d, want 2: a bad argument must not stop the good ones", b.Converted)
	}
	if b.Failed != 1 {
		t.Errorf("failed = %d, want 1", b.Failed)
	}
	if !strings.Contains(b.Summary(), "nope.qvd") {
		t.Errorf("the summary should name the unreadable input:\n%s", b.Summary())
	}
	// It must also appear in the log, so a batch audit sees it.
	if len(b.Results) != 3 {
		t.Errorf("got %d results, want 3", len(b.Results))
	}
}

// The log commonly sits inside --out-dir, which may not exist yet.
func TestNewLogWriterCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does", "not", "exist", "run.jsonl")
	log, err := NewLogWriter(path)
	if err != nil {
		t.Fatalf("NewLogWriter should create the parent directory: %v", err)
	}
	log.Summary(&BatchResult{})
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("log not written: %v", err)
	}
}

// A cancelled batch must report itself as cancelled, both per file and in the
// summary's exit code. A raw context.Canceled carries no domain meaning and
// maps to the generic input-error code, which tells the user their files
// failed to read when in fact they were never attempted.
func TestRunManyCancellationIsReportedAsCancelled(t *testing.T) {
	src := folderFixture(t, true, false)
	inputs := FindInputs([]string{src}, InputSelection{Recursive: true}).Files
	if len(inputs) == 0 {
		t.Fatal("fixture produced no inputs")
	}
	outDir := filepath.Join(t.TempDir(), "out")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	opts := testOptions()
	b, err := RunMany(ctx, inputs, &opts, &ManyOptions{OutDir: outDir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range b.Results {
		if r.Err == nil {
			t.Errorf("%s converted despite a cancelled context", r.Input)
			continue
		}
		if !errors.Is(r.Err, ErrCanceled) {
			t.Errorf("%s reported %v, want ErrCanceled", r.Input, r.Err)
		}
		if errors.Is(r.Err, ErrInput) {
			t.Errorf("%s reported an input error, which reads as an unreadable file: %v", r.Input, r.Err)
		}
	}

	// The same mapping the CLI applies, including its fallthrough.
	codeFor := func(err error) int {
		switch {
		case errors.Is(err, ErrCanceled):
			return 7
		case errors.Is(err, ErrQualityGate):
			return 6
		case errors.Is(err, ErrSchemaPolicy):
			return 3
		case errors.Is(err, ErrInput):
			return 4
		default:
			return 4
		}
	}
	if got := b.ExitCode(codeFor); got != 7 {
		t.Errorf("cancelled batch exited %d, want 7", got)
	}
}

// A batch run reports each file's progress. It used to pass a nil logger to
// the converter, so a folder holding one large table showed a line on starting
// and nothing again until it finished, which on a twenty-million-row table is
// a quarter of an hour of silence.
func TestRunManyReportsPerFileProgress(t *testing.T) {
	src := folderFixture(t, true, false)
	inputs := FindInputs([]string{src}, InputSelection{Recursive: true}).Files
	outDir := filepath.Join(t.TempDir(), "out")

	opts := testOptions()
	opts.ProgressEvery = 1 // every row, so a small fixture still reports

	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	if _, err := RunMany(context.Background(), inputs, &opts,
		&ManyOptions{OutDir: outDir}, logf); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(lines, "\n")
	for _, want := range []string{"converted ", "conversion finished in"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q line from a batch run:\n%s", want, joined)
		}
	}
	// Converting one at a time, nothing can interleave, so no prefix is added.
	for _, l := range lines {
		if strings.Contains(l, ".qvd: converted") {
			t.Errorf("sequential run prefixed a line that cannot interleave: %s", l)
		}
	}
}

// Converting several at once, every line names the file it belongs to, or the
// output is unreadable.
func TestRunManyPrefixesConcurrentProgress(t *testing.T) {
	src := folderFixture(t, true, false)
	inputs := FindInputs([]string{src}, InputSelection{Recursive: true}).Files
	if len(inputs) < 2 {
		t.Skip("fixture has too few files to run concurrently")
	}
	outDir := filepath.Join(t.TempDir(), "out")

	opts := testOptions()
	opts.ProgressEvery = 1

	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	if _, err := RunMany(context.Background(), inputs, &opts,
		&ManyOptions{OutDir: outDir, FileWorkers: len(inputs)}, logf); err != nil {
		t.Fatal(err)
	}

	var progress, prefixed int
	for _, l := range lines {
		if !strings.Contains(l, "converted ") || strings.Contains(l, "file(s)") {
			continue
		}
		progress++
		if strings.Contains(l, ".qvd: converted") {
			prefixed++
		}
	}
	if progress == 0 {
		t.Fatalf("no progress lines at all:\n%s", strings.Join(lines, "\n"))
	}
	if prefixed != progress {
		t.Errorf("%d of %d progress lines named their file; all of them must",
			prefixed, progress)
	}
}

// The prefix follows the concurrency actually used, not the number asked for.
// splitWorkerBudget clamps --file-workers to the file count, so requesting
// four and converting one file runs one at a time: prefixing there would
// contradict the "converting 1 file(s)" line above it.
func TestRunManyPrefixFollowsEffectiveConcurrency(t *testing.T) {
	src := t.TempDir()
	if _, err := qvdtest.Build(filepath.Join(src, "only.qvd"), sampleTable(200)); err != nil {
		t.Fatal(err)
	}
	inputs := FindInputs([]string{src}, InputSelection{Recursive: true}).Files
	if len(inputs) != 1 {
		t.Fatalf("fixture has %d files, want 1", len(inputs))
	}
	outDir := filepath.Join(t.TempDir(), "out")

	opts := testOptions()
	opts.ProgressEvery = 1

	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	// Four requested, one file, so one at a time in practice.
	if _, err := RunMany(context.Background(), inputs, &opts,
		&ManyOptions{OutDir: outDir, FileWorkers: 4}, logf); err != nil {
		t.Fatal(err)
	}

	var progress int
	for _, l := range lines {
		if !strings.Contains(l, "converted ") || strings.Contains(l, "file(s)") {
			continue
		}
		progress++
		if strings.Contains(l, ".qvd: converted") {
			t.Errorf("prefixed a line although only one file converts at a time: %s", l)
		}
	}
	if progress == 0 {
		t.Fatalf("no progress lines at all:\n%s", strings.Join(lines, "\n"))
	}
}
