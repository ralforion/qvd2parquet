package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ralforion/qvd2parquet/internal/convert"
)

// buildCLI compiles the command once for the tests that need to run it as a
// process: signal handling and exit codes are only observable from outside.
func buildCLI(t *testing.T) string {
	t.Helper()
	name := "qvd2parquet"
	if runtime.GOOS == "windows" {
		// Windows will not execute a file without the extension.
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func readLogRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	records := make([]map[string]any, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &records[i]); err != nil {
			t.Fatalf("log line %d is not JSON: %v\n%s", i+1, err, line)
		}
	}
	return records
}

// A successful run must not announce a cancellation. The signal handler used
// to wait on the run's context, which the deferred cleanup also cancels, so
// nearly every successful conversion printed "cancelling" on its way out.
func TestSuccessfulRunDoesNotAnnounceCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	dir := t.TempDir()
	in := filepath.Join("..", "..", "testdata", "sample-small.qvd")
	if _, err := os.Stat(in); err != nil {
		t.Fatalf("fixture missing, this test would otherwise pass by skipping: %v", err)
	}

	// Repeated, because the bug was a race: it appeared on most runs but not
	// all, and once per test would have been a coin flip.
	for i := 0; i < 20; i++ {
		out := filepath.Join(dir, "out.parquet")
		cmd := exec.Command(bin, "--force", "--progress", "0",
			"--quality-gate", "none", in, out)
		combined, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run %d failed: %v\n%s", i, err, combined)
		}
		if strings.Contains(string(combined), "cancelling") {
			t.Fatalf("run %d announced a cancellation on a successful run:\n%s", i, combined)
		}
	}
}

func TestSingleFileLog(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	fixture := filepath.Join("..", "..", "testdata", "sample-small.qvd")

	// A single-file conversion must honour --log with the same file-plus-summary
	// shape as a batch run. The flag used to be accepted and silently ignored.
	t.Run("success records file and summary", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "out.parquet")
		logPath := filepath.Join(dir, "logs", "run.jsonl")

		cmd := exec.Command(bin, "--force", "--progress", "0",
			"--quality-gate", "none", "--log", logPath, fixture, out)
		if combined, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("run failed: %v\n%s", err, combined)
		}

		records := readLogRecords(t, logPath)
		if len(records) != 2 {
			t.Fatalf("got %d log records, want file and summary", len(records))
		}
		file, summary := records[0], records[1]
		if file["type"] != "file" || file["status"] != "ok" ||
			file["input"] != fixture || file["output"] != out ||
			file["rows"] != float64(1000) || file["error"] != "" {
			t.Errorf("file record = %v", file)
		}
		if summary["type"] != "summary" || summary["files"] != float64(1) ||
			summary["converted"] != float64(1) || summary["failed"] != float64(0) ||
			summary["rows"] != float64(1000) {
			t.Errorf("summary record = %v", summary)
		}
	})

	t.Run("failure records file and summary", func(t *testing.T) {
		dir := t.TempDir()
		in := filepath.Join(dir, "missing.qvd")
		out := filepath.Join(dir, "out.parquet")
		logPath := filepath.Join(dir, "run.jsonl")

		cmd := exec.Command(bin, "--progress", "0", "--quality-gate", "none",
			"--log", logPath, in, out)
		combined, err := cmd.CombinedOutput()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != exitInput {
			t.Fatalf("exit = %v, want %d\n%s", err, exitInput, combined)
		}

		records := readLogRecords(t, logPath)
		if len(records) != 2 {
			t.Fatalf("got %d log records, want file and summary", len(records))
		}
		file, summary := records[0], records[1]
		if file["type"] != "file" || file["status"] != "failed" ||
			file["input"] != in || file["output"] != "" || file["error"] == "" {
			t.Errorf("file record = %v", file)
		}
		if summary["type"] != "summary" || summary["files"] != float64(1) ||
			summary["converted"] != float64(0) || summary["failed"] != float64(1) {
			t.Errorf("summary record = %v", summary)
		}
	})

	t.Run("inspect rejects log", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "run.jsonl")
		cmd := exec.Command(bin, "--inspect", "--log", logPath, fixture)
		combined, err := cmd.CombinedOutput()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != exitUsage {
			t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
		}
		if !strings.Contains(string(combined), "cannot be combined with --inspect") {
			t.Fatalf("missing diagnostic:\n%s", combined)
		}
		if _, err := os.Stat(logPath); !os.IsNotExist(err) {
			t.Fatalf("log created during inspect: %v", err)
		}
	})

	t.Run("log cannot overwrite schema", func(t *testing.T) {
		dir := t.TempDir()
		schemaPath := filepath.Join(dir, "schema.json")
		original := []byte(`{"fields": []}`)
		if err := os.WriteFile(schemaPath, original, 0o600); err != nil {
			t.Fatalf("write schema: %v", err)
		}
		out := filepath.Join(dir, "out.parquet")
		cmd := exec.Command(bin, "--schema", schemaPath, "--log", schemaPath, fixture, out)
		combined, err := cmd.CombinedOutput()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != exitUsage {
			t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
		}
		after, err := os.ReadFile(schemaPath)
		if err != nil {
			t.Fatalf("read schema after rejection: %v", err)
		}
		if !bytes.Equal(after, original) {
			t.Fatal("schema changed when used as --log path")
		}
	})
}

func TestValidateLogPath(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.jsonl")
	tests := []struct {
		name   string
		input  string
		output string
		opts   convert.Options
	}{
		{name: "input", input: logPath},
		{name: "output", output: logPath},
		{name: "schema", opts: convert.Options{SchemaOverridePath: logPath}},
		{name: "schema report", opts: convert.Options{SchemaReportPath: logPath}},
		{name: "quality report", opts: convert.Options{QualityReportPath: logPath}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateLogPath(logPath, tc.input, tc.output, &tc.opts); err == nil {
				t.Fatal("collision accepted")
			}
		})
	}
}

// A batch derives its paths instead of taking them from the command line, so
// the single-file guard cannot see them: the inputs come from expanding
// directories and every output and per-file report is generated under
// --out-dir. Without a guard of its own, --log truncated whichever of them it
// named.
func TestValidateBatchLogPath(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	input := filepath.Join(dir, "in", "sales.qvd")
	inputs := []string{input}

	// A path FindInputs could not examine is still an input: the run reports it
	// as a failed file and writes it to the log.
	missing := filepath.Join(dir, "in", "gone.qvd")
	problems := []convert.InputProblem{{Path: missing, Err: convert.ErrInput}}

	collisions := []struct {
		name    string
		logPath string
		opts    convert.Options
	}{
		{
			name:    "an expanded input",
			logPath: input,
		},
		{
			name:    "an input FindInputs could not examine",
			logPath: missing,
		},
		{
			name:    "a derived output",
			logPath: filepath.Join(outDir, "sales.parquet"),
		},
		{
			name:    "a derived output under another casing",
			logPath: filepath.Join(outDir, "SALES.PARQUET"),
		},
		{
			name:    "a per-file schema report",
			logPath: filepath.Join(outDir, "sales.schema.json"),
			opts:    convert.Options{SchemaReportPath: filepath.Join(outDir, "schema.json")},
		},
		{
			name:    "a per-file quality report",
			logPath: filepath.Join(outDir, "sales.quality.json"),
			opts:    convert.Options{QualityReportPath: filepath.Join(outDir, "quality.json")},
		},
		{
			name:    "the schema override",
			logPath: filepath.Join(dir, "schema.json"),
			opts:    convert.Options{SchemaOverridePath: filepath.Join(dir, "schema.json")},
		},
	}
	for _, tc := range collisions {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateBatchLogPath(tc.logPath, inputs, problems, outDir, &tc.opts); err == nil {
				t.Fatal("collision accepted")
			}
		})
	}

	// The guard must not reject a log that merely sits beside the outputs,
	// which is where a batch log normally goes.
	var opts convert.Options
	if err := validateBatchLogPath(filepath.Join(outDir, "run.jsonl"), inputs, problems, outDir, &opts); err != nil {
		t.Errorf("log beside the outputs rejected: %v", err)
	}
}

// The end the batch guard protects. Pointing --log at an input used to truncate
// it before conversion: a 17 KiB QVD became a few hundred bytes of JSON Lines
// reporting that the file it had just destroyed was not a QVD.
func TestBatchLogDoesNotTruncateInput(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	fixture := filepath.Join("..", "..", "testdata", "sample-small.qvd")
	original, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	dir := t.TempDir()
	inDir := filepath.Join(dir, "in")
	if err := os.Mkdir(inDir, 0o755); err != nil {
		t.Fatalf("create input directory: %v", err)
	}
	input := filepath.Join(inDir, "sample-small.qvd")
	if err := os.WriteFile(input, original, 0o600); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}

	cmd := exec.Command(bin, "--progress", "0", "--out-dir", filepath.Join(dir, "out"),
		"--log", input, inDir)
	combined, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitUsage {
		t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
	}
	if !strings.Contains(string(combined), "--log path must differ from the input") {
		t.Errorf("missing diagnostic:\n%s", combined)
	}
	after, err := os.ReadFile(input)
	if err != nil {
		t.Fatalf("read input after rejection: %v", err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("input truncated: %d bytes before, %d after", len(original), len(after))
	}
}

// A path that could not be examined never reaches the inputs list, so the guard
// used to let the log take it. The run then named the file as missing and
// created it in the same breath: exit 4, "no such file", and a JSON Lines log
// sitting at that very path whose first record reports its own failure.
func TestBatchLogDoesNotTakeAFailedInputPath(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.qvd")

	cmd := exec.Command(bin, "--progress", "0", "--out-dir", filepath.Join(dir, "out"),
		"--log", missing, missing)
	combined, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitUsage {
		t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
	}
	if !strings.Contains(string(combined), "--log path must differ from the input") {
		t.Errorf("missing diagnostic:\n%s", combined)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("log created at the path the run reported as a missing input: %v", err)
	}
}

// Pointing --log at a path a batch will write its Parquet to used to exit 0 and
// report writing both, leaving the output in place and no log at all.
func TestBatchLogDoesNotCollideWithOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	fixture := filepath.Join("..", "..", "testdata", "sample-small.qvd")

	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	cmd := exec.Command(bin, "--force", "--progress", "0", "--quality-gate", "none",
		"--out-dir", outDir, "--log", filepath.Join(outDir, "sample-small.parquet"), fixture)
	combined, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitUsage {
		t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
	}
	if !strings.Contains(string(combined), "--log path must differ from the output") {
		t.Errorf("missing diagnostic:\n%s", combined)
	}
	if entries, err := os.ReadDir(outDir); err == nil && len(entries) > 0 {
		t.Errorf("output directory written despite rejection: %v", entries)
	}
}

func TestSamePathResolvesSymlinkedParent(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	aliasDir := filepath.Join(dir, "alias")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !samePath(filepath.Join(realDir, "nested", "out.parquet"),
		filepath.Join(aliasDir, "nested", "out.parquet")) {
		t.Fatal("symlinked parent aliases treated as different paths")
	}
}

// Case-only aliases are a collision on every platform, not just Windows.
// Whether two spellings name one file is a property of the filesystem, not the
// OS: macOS is case-insensitive by default, Linux mounts exFAT, NTFS and SMB
// that way, and Windows supports per-directory case sensitivity.
//
// Reproduced before this was folded everywhere, on macOS/APFS: --log RUN.JSONL
// beside an output of run.jsonl exited 0, reported writing both, and left one
// file named RUN.JSONL whose first four bytes were PAR1. The log had been
// written to an unlinked handle and was gone.
func TestSamePathIgnoresCase(t *testing.T) {
	dir := t.TempDir()
	if !samePath(filepath.Join(dir, "OUT.parquet"), filepath.Join(dir, "out.parquet")) {
		t.Fatal("case-only aliases treated as different paths")
	}
	if !samePath(filepath.Join(dir, "RUN.JSONL"), filepath.Join(dir, "run.jsonl")) {
		t.Fatal("case-only aliases of a log path treated as different paths")
	}
	// Folding must not swallow paths that genuinely differ.
	if samePath(filepath.Join(dir, "out.parquet"), filepath.Join(dir, "other.parquet")) {
		t.Fatal("distinct paths reported as the same")
	}
}

// The end the fold exists to protect: --log must be refused when it names the
// output under another casing, rather than the two silently clobbering.
func TestValidateLogPathRejectsCaseVariantOfOutput(t *testing.T) {
	dir := t.TempDir()
	var opts convert.Options
	err := validateLogPath(
		filepath.Join(dir, "RUN.JSONL"),
		filepath.Join(dir, "in.qvd"),
		filepath.Join(dir, "run.jsonl"),
		&opts,
	)
	if err == nil {
		t.Fatal("a --log path differing from the output only by case was accepted")
	}
	if !strings.Contains(err.Error(), "the output path") {
		t.Errorf("error should name the colliding path, got: %v", err)
	}
}

// Inspect is a preflight check, so it has to exit the way the conversion
// would. Reporting a problem and exiting 0 is worse than not checking: a
// script gating on it would go on to start the run that is about to fail.
func TestInspectExitsNonZeroOnAnEncodingItCannotApply(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	in := filepath.Join("..", "..", "testdata", "sample-small.qvd")
	if _, err := os.Stat(in); err != nil {
		t.Fatalf("fixture missing, this test would otherwise pass by skipping: %v", err)
	}

	// Id is written as int64, which cannot carry a byte array encoding.
	cmd := exec.Command(bin, "--inspect", "--encoding", "Id=delta_byte_array", in)
	combined, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("inspect exited 0 despite refusing the encoding:\n%s", combined)
	}
	code := cmd.ProcessState.ExitCode()
	if code != exitSchema {
		t.Errorf("exit code = %d, want %d (schema/type policy):\n%s", code, exitSchema, combined)
	}
	if !strings.Contains(string(combined), "does not fit column \"Id\"") {
		t.Errorf("output should explain the refusal:\n%s", combined)
	}

	// The same conversion must fail the same way, or inspect would not be
	// predicting it.
	out := filepath.Join(t.TempDir(), "out.parquet")
	cmd = exec.Command(bin, "--force", "--encoding", "Id=delta_byte_array", in, out)
	combined, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the conversion accepted what inspect refused:\n%s", combined)
	}
	if got := cmd.ProcessState.ExitCode(); got != code {
		t.Errorf("conversion exit code = %d, inspect said %d", got, code)
	}
}

// The file selection has to hold end to end: the flags reach the walk, an
// unexpanded wildcard survives the trip through the argument list, and a
// pattern that selects nothing says so rather than looking like an empty
// folder.
func TestBatchSelectsFilesByName(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	fixture := filepath.Join("..", "..", "testdata", "sample-small.qvd")
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("fixture missing, this test would otherwise pass by skipping: %v", err)
	}
	src := t.TempDir()
	for _, name := range []string{"CE10500.qvd", "CE10501.qvd", "BSEG.qvd"} {
		if err := os.WriteFile(filepath.Join(src, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	outDir := filepath.Join(t.TempDir(), "included")
	cmd := exec.Command(bin, "--out-dir", outDir, "--include-files", "CE*", src)
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, combined)
	}
	written, err := filepath.Glob(filepath.Join(outDir, "*.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 {
		t.Errorf("--include-files 'CE*' wrote %v, want the two CE files", written)
	}
	// The file the pattern dropped is reported, or a mistyped pattern would
	// look like a folder that held only what converted.
	if !strings.Contains(string(combined), "1 file(s) dropped") {
		t.Errorf("the dropped file was not reported:\n%s", combined)
	}

	// An unexpanded wildcard is what cmd.exe and PowerShell hand over.
	outDir = filepath.Join(t.TempDir(), "wildcard")
	cmd = exec.Command(bin, "--out-dir", outDir, filepath.Join(src, "CE*.qvd"))
	if combined, err = cmd.CombinedOutput(); err != nil {
		t.Fatalf("wildcard path failed: %v\n%s", err, combined)
	}
	if written, err = filepath.Glob(filepath.Join(outDir, "*.parquet")); err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 {
		t.Errorf("wildcard path wrote %v, want the two CE files", written)
	}

	// Selecting nothing is a usage error that names the reason.
	cmd = exec.Command(bin, "--out-dir", filepath.Join(t.TempDir(), "none"),
		"--include-files", "ZZ*", src)
	combined, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a selection matching nothing exited 0:\n%s", combined)
	}
	if got := cmd.ProcessState.ExitCode(); got != exitUsage {
		t.Errorf("exit code = %d, want %d (usage)", got, exitUsage)
	}
	if !strings.Contains(string(combined), `--include-files/--exclude-files "ZZ*" left none of the 3 .qvd file(s)`) {
		t.Errorf("output should blame the patterns, not an empty folder:\n%s", combined)
	}
}

// --skip-up-to-date has to reach the run, and the manifest has to be the one
// thing a nightly job can rely on: the second run over an unchanged folder
// does nothing.
func TestBatchSkipsUpToDateFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	fixture := filepath.Join("..", "..", "testdata", "sample-small.qvd")
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("fixture missing, this test would otherwise pass by skipping: %v", err)
	}
	src := t.TempDir()
	for _, name := range []string{"A.qvd", "B.qvd"} {
		if err := os.WriteFile(filepath.Join(src, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outDir := filepath.Join(t.TempDir(), "out")

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"--out-dir", outDir, "--skip-up-to-date"}, args...)...)
		combined, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run failed: %v\n%s", err, combined)
		}
		return string(combined)
	}

	if out := run(src); strings.Contains(out, "skip ") {
		t.Errorf("the first run skipped something:\n%s", out)
	}
	// The manifest is a dotfile, so a reader scanning the directory for
	// Parquet ignores it the way it ignores any name beginning with a dot.
	if _, err := os.Stat(filepath.Join(outDir, convert.ManifestName)); err != nil {
		t.Fatalf("no manifest written: %v", err)
	}

	out := run(src)
	if !strings.Contains(out, "2 skipped") {
		t.Errorf("the second run should have skipped both files:\n%s", out)
	}

	// A nightly job is --force --skip-up-to-date: permission to overwrite the
	// stale, and a decision to leave the current alone. Dropping the flag is
	// how the same job reruns everything.
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(src, "A.qvd"), later, later); err != nil {
		t.Fatal(err)
	}
	out = run("--force", src)
	if !strings.Contains(out, "1 skipped") || !strings.Contains(out, "converted 1/2") {
		t.Errorf("only the changed file should have converted:\n%s", out)
	}

	// Pointing the log at the manifest would destroy the run's own record.
	cmd := exec.Command(bin, "--out-dir", outDir, "--skip-up-to-date",
		"--log", filepath.Join(outDir, convert.ManifestName), src)
	combined, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("--log over the manifest was accepted:\n%s", combined)
	}
	if got := cmd.ProcessState.ExitCode(); got != exitUsage {
		t.Errorf("exit code = %d, want %d (usage)", got, exitUsage)
	}
}

// A pin inspect accepts must leave it exiting 0, or the gate would be useless
// in the other direction.
func TestInspectExitsZeroOnAnEncodingItAccepts(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	in := filepath.Join("..", "..", "testdata", "sample-small.qvd")
	cmd := exec.Command(bin, "--inspect", "--encoding", "Name=delta_byte_array", in)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("inspect failed on a valid pin: %v\n%s", err, combined)
	}
}
