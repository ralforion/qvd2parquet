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

	"github.com/ralforion/qvd2parquet/internal/catalog"
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

// --console-log records the prose output, which is the half the JSON log does
// not carry: the notes, the progress, and what the run was doing when it was
// stopped. It is a tee rather than a redirect, so the screen keeps everything
// too, and it starts with the banner so the file says which build wrote it.
func TestConsoleLog(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	fixture := filepath.Join("..", "..", "testdata", "sample-small.qvd")

	t.Run("records the screen output", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "out.parquet")
		consolePath := filepath.Join(dir, "logs", "run.txt")

		cmd := exec.Command(bin, "--force", "--progress", "0",
			"--quality-gate", "none", "--console-log", consolePath, fixture, out)
		var screen bytes.Buffer
		cmd.Stderr = &screen
		if err := cmd.Run(); err != nil {
			t.Fatalf("run failed: %v\n%s", err, screen.String())
		}

		raw, err := os.ReadFile(consolePath)
		if err != nil {
			t.Fatalf("the console log was not written: %v", err)
		}
		recorded := string(raw)
		if !strings.HasPrefix(recorded, "qvd2parquet ") {
			t.Errorf("the console log should open with the banner, got:\n%s", firstLine(recorded))
		}
		// The banner reaches the screen before the guards have run, so it is
		// written to the file on its own; everything after it must match.
		if want := "wrote " + out; !strings.Contains(recorded, want) {
			t.Errorf("the console log is missing %q:\n%s", want, recorded)
		}
		for _, line := range strings.Split(strings.TrimSpace(screen.String()), "\n") {
			if !strings.Contains(recorded, line) {
				t.Errorf("the screen printed a line the console log does not hold:\n%s", line)
			}
		}
		if !strings.Contains(screen.String(), "wrote "+out) {
			t.Error("the console log swallowed the screen output instead of copying it")
		}
	})

	// The file cannot be opened until the path guards have run, and by then
	// the run has printed its banner and any note about the inputs it
	// selected. A mistyped --include-files pattern is exactly the line an
	// operator goes looking for afterwards, so it has to be in the file too.
	t.Run("records what was printed before it opened", func(t *testing.T) {
		dir := t.TempDir()
		outDir := filepath.Join(dir, "out")
		consolePath := filepath.Join(dir, "run.txt")

		cmd := exec.Command(bin, "--force", "--progress", "0", "--quality-gate", "none",
			"--out-dir", outDir, "--include-files", "*,ZZNOPE",
			"--console-log", consolePath, fixture)
		var screen bytes.Buffer
		cmd.Stderr = &screen
		if err := cmd.Run(); err != nil {
			t.Fatalf("run failed: %v\n%s", err, screen.String())
		}
		if !strings.Contains(screen.String(), "ZZNOPE") {
			t.Fatalf("the run printed no note to record:\n%s", screen.String())
		}

		raw, err := os.ReadFile(consolePath)
		if err != nil {
			t.Fatalf("the console log was not written: %v", err)
		}
		recorded := string(raw)
		if !strings.HasPrefix(recorded, "qvd2parquet ") {
			t.Errorf("the console log should open with the banner, got:\n%s", firstLine(recorded))
		}
		for _, line := range strings.Split(strings.TrimSpace(screen.String()), "\n") {
			if !strings.Contains(recorded, line) {
				t.Errorf("the screen printed a line the console log does not hold:\n%s\n\ngot:\n%s",
					line, recorded)
			}
		}
	})

	// The file is created by truncating, so every path the run reads or writes
	// has to be refused, exactly as --log and --catalog-out are.
	t.Run("refuses a path the run uses", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "out.parquet")
		logPath := filepath.Join(dir, "run.jsonl")
		cases := []struct {
			name string
			args []string
			want string
		}{
			{"the input", []string{"--console-log", fixture}, "--console-log path must differ from the input"},
			{"the output", []string{"--console-log", out}, "--console-log path must differ from the output"},
			{"--log", []string{"--log", logPath, "--console-log", logPath},
				"--console-log path must differ from --log"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				args := append([]string{"--force", "--progress", "0", "--quality-gate", "none"}, tc.args...)
				cmd := exec.Command(bin, append(args, fixture, out)...)
				combined, err := cmd.CombinedOutput()
				exitErr, ok := err.(*exec.ExitError)
				if !ok || exitErr.ExitCode() != exitUsage {
					t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
				}
				if !strings.Contains(string(combined), tc.want) {
					t.Errorf("message = %s, want %q", combined, tc.want)
				}
			})
		}
		// The refusal must not have created the file it was refusing to share.
		if _, err := os.Stat(logPath); err == nil {
			t.Error("a refused run left the log path truncated")
		}
	})

	// A batch derives its paths under --out-dir, so the guard has to run after
	// the inputs are expanded, and before the file is created.
	t.Run("refuses a batch output", func(t *testing.T) {
		dir := t.TempDir()
		outDir := filepath.Join(dir, "out")
		consolePath := filepath.Join(outDir, "sample-small.parquet")

		cmd := exec.Command(bin, "--force", "--progress", "0", "--quality-gate", "none",
			"--out-dir", outDir, "--console-log", consolePath, fixture)
		combined, err := cmd.CombinedOutput()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != exitUsage {
			t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
		}
		if !strings.Contains(string(combined), "--console-log path must differ from the output") {
			t.Errorf("message = %s", combined)
		}
	})
}

// firstLine is the head of a message, for a failure that would otherwise print
// a whole run's output.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
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
		// The single-file path assembles its own FileResult, so it is its own
		// chance to leave the table out. The fixture is sample-small.qvd
		// holding BigSales, which is also the case a name taken from the file
		// name would get wrong.
		if file["table"] != "BigSales" {
			t.Errorf("file record has table %q, want \"BigSales\": %v", file["table"], file)
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

	// A conversion can fail long after the header was read, and that record is
	// the one an operator goes looking for. Naming the table only on success
	// would leave a failure in a folder of timestamp-named extracts saying
	// nothing about which table it was.
	t.Run("failure after the header still names the table", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "out.parquet")
		logPath := filepath.Join(dir, "run.jsonl")

		cmd := exec.Command(bin, "--progress", "0", "--quality-gate", "none",
			"--columns", "Nope", "--log", logPath, fixture, out)
		combined, err := cmd.CombinedOutput()
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("expected the run to fail: %v\n%s", err, combined)
		}

		file := readLogRecords(t, logPath)[0]
		if file["status"] != "failed" || file["error"] == "" {
			t.Fatalf("file record = %v", file)
		}
		if file["table"] != "BigSales" {
			t.Errorf("a failure past the header should still name the table, got %q: %v",
				file["table"], file)
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

// TestCatalogOutDoesNotTruncateInput is the guard the log already has, applied
// to --catalog-out. The catalog writer replaces whatever file it is pointed
// at, so an early version that created it before validating the paths turned a
// correctly refused run into a destroyed QVD: the run printed the refusal and
// wrote an empty Parquet over the input on its way out.
func TestCatalogOutDoesNotTruncateInput(t *testing.T) {
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

	// --force is the case that matters: without it the existence check alone
	// refuses, and the guard is never reached.
	cmd := exec.Command(bin, "--progress", "0", "--force",
		"--out-dir", filepath.Join(dir, "out"), "--catalog-out", input, inDir)
	combined, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitUsage {
		t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
	}
	if !strings.Contains(string(combined), "--catalog-out path must differ from the input") {
		t.Errorf("missing diagnostic:\n%s", combined)
	}
	after, err := os.ReadFile(input)
	if err != nil {
		t.Fatalf("read input after rejection: %v", err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("input replaced: %d bytes before, %d after", len(original), len(after))
	}
}

// TestCatalogOutDoesNotCollideWithLog covers the other direction: both write
// with O_TRUNC, so whichever finishes second silently destroys the first.
func TestCatalogOutDoesNotCollideWithLog(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	dir := t.TempDir()
	shared := filepath.Join(dir, "both.parquet")

	cmd := exec.Command(bin, "--progress", "0",
		"--out-dir", filepath.Join(dir, "out"),
		"--catalog-out", shared, "--log", shared,
		filepath.Join("..", "..", "testdata", "sample-small.qvd"))
	combined, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitUsage {
		t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
	}
	if !strings.Contains(string(combined), "--catalog-out path must differ from --log") {
		t.Errorf("missing diagnostic:\n%s", combined)
	}
}

// TestCatalogScanReadsCommentsBackOutOfParquet is the after-the-fact route: a
// conversion that forgot --catalog-out can still be catalogued, because the
// comment is in the file it wrote.
func TestCatalogScanReadsCommentsBackOutOfParquet(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")

	convert := exec.Command(bin, "--progress", "0", "--out-dir", outDir,
		"--field-regex", `^(?P<name>Amount)$`, "--field-comment", "Betrag",
		filepath.Join("..", "..", "testdata", "sample-small.qvd"))
	if out, err := convert.CombinedOutput(); err != nil {
		t.Fatalf("convert: %v\n%s", err, out)
	}

	catalogPath := filepath.Join(dir, "catalog.parquet")
	scan := exec.Command(bin, "--progress", "0", "--catalog-scan",
		"--catalog-out", catalogPath, outDir)
	out, err := scan.CombinedOutput()
	if err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "wrote catalog to") {
		t.Errorf("scan did not report writing a catalog:\n%s", out)
	}
	if _, err := os.Stat(catalogPath); err != nil {
		t.Fatalf("no catalog written: %v", err)
	}

	rows, err := catalog.ScanFile(catalogPath)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	if len(rows) != len(catalog.Schema.Fields()) {
		t.Errorf("catalog has %d columns, want %d", len(rows), len(catalog.Schema.Fields()))
	}
}

// TestCatalogScanNeedsCatalogOut keeps the two flags from being usable apart,
// since a scan with nowhere to write is a run that reads a folder and reports
// nothing.
func TestCatalogScanNeedsCatalogOut(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	cmd := exec.Command(bin, "--catalog-scan", t.TempDir())
	combined, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitUsage {
		t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
	}
	if !strings.Contains(string(combined), "--catalog-scan needs --catalog-out") {
		t.Errorf("missing diagnostic:\n%s", combined)
	}
}

// TestCatalogWriteFailureFailsTheRun covers a catalog that cannot be committed
// after an otherwise successful conversion. The close used to run from a defer
// that only printed, so the process reported success while the catalog the
// caller was waiting on did not exist.
func TestCatalogWriteFailureFailsTheRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	dir := t.TempDir()
	// A directory cannot be replaced by the writer's rename, so the commit
	// fails after the conversion has already succeeded.
	blocked := filepath.Join(dir, "catalog.parquet")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("create blocking directory: %v", err)
	}

	cmd := exec.Command(bin, "--progress", "0", "--force", "--catalog-out", blocked,
		filepath.Join("..", "..", "testdata", "sample-small.qvd"),
		filepath.Join(dir, "out.parquet"))
	combined, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitOutput {
		t.Fatalf("exit = %v, want %d\n%s", err, exitOutput, combined)
	}
	if !strings.Contains(string(combined), "output error") {
		t.Errorf("missing diagnostic:\n%s", combined)
	}
}

// TestCatalogOutDoesNotTakeAFailedInputPath is the guard the log already has
// for a path FindInputs could not examine. Such a path never reaches the
// inputs list, so the loop over inputs does not see it, and the catalog took
// the name of the very file the run was about to report as missing: exit 4,
// "no such file", and an empty Parquet sitting at that path.
func TestCatalogOutDoesNotTakeAFailedInputPath(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.qvd")

	cmd := exec.Command(bin, "--progress", "0", "--out-dir", filepath.Join(dir, "out"),
		"--catalog-out", missing, missing)
	combined, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitUsage {
		t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
	}
	if !strings.Contains(string(combined), "--catalog-out path must differ from the input") {
		t.Errorf("missing diagnostic:\n%s", combined)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("the run created a file at the input path it reported as missing")
	}
}

// TestCatalogScanRejectsLog keeps --log from being accepted and ignored. A
// scan converts nothing, so it has no file records to write, and a run that
// silently produced no log would look like one that had lost it.
func TestCatalogScanRejectsLog(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	dir := t.TempDir()

	cmd := exec.Command(bin, "--progress", "0", "--catalog-scan",
		"--catalog-out", filepath.Join(dir, "catalog.parquet"),
		"--log", filepath.Join(dir, "run.jsonl"), dir)
	combined, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitUsage {
		t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
	}
	if !strings.Contains(string(combined), "--log records conversions and cannot be combined with --catalog-scan") {
		t.Errorf("missing diagnostic:\n%s", combined)
	}
}

// TestRefusedRunLeavesTheCatalogAlone covers a run refused by a guard that has
// nothing to do with the catalog.
//
// The catalog used to be opened before the log's own path validation, so a
// command rejected for an unrelated collision still ran the deferred close on
// its way out and replaced the catalog with an empty Parquet. The refusal
// printed on the way past made the damage look impossible, which is what makes
// this worth a test in both modes rather than a reordering and a shrug.
func TestRefusedRunLeavesTheCatalogAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	fixture := filepath.Join("..", "..", "testdata", "sample-small.qvd")
	const sentinel = "not a parquet file"

	for _, tc := range []struct {
		name string
		args func(dir, catalog, schema string) []string
	}{
		{"single", func(dir, catalog, schema string) []string {
			return []string{"--progress", "0", "--force",
				"--catalog-out", catalog, "--schema", schema, "--log", schema,
				fixture, filepath.Join(dir, "out.parquet")}
		}},
		{"batch", func(dir, catalog, schema string) []string {
			return []string{"--progress", "0", "--force",
				"--out-dir", filepath.Join(dir, "out"),
				"--catalog-out", catalog, "--schema", schema, "--log", schema,
				fixture}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			catalogPath := filepath.Join(dir, "catalog.parquet")
			schemaPath := filepath.Join(dir, "schema.json")
			if err := os.WriteFile(schemaPath, []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(catalogPath, []byte(sentinel), 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(bin, tc.args(dir, catalogPath, schemaPath)...)
			combined, err := cmd.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != exitUsage {
				t.Fatalf("exit = %v, want %d\n%s", err, exitUsage, combined)
			}
			if !strings.Contains(string(combined), "--log path must differ from --schema") {
				t.Errorf("missing diagnostic:\n%s", combined)
			}
			after, err := os.ReadFile(catalogPath)
			if err != nil {
				t.Fatalf("read catalog after rejection: %v", err)
			}
			if string(after) != sentinel {
				t.Fatalf("refused run replaced the catalog: %q", string(after))
			}
		})
	}
}

// TestOutputCollisionLeavesLogAndCatalogAlone covers a batch refused for a
// reason neither writer knows about.
//
// RunMany rejects two inputs that would produce one output, but it did so
// after the CLI had already opened the log and the catalog, both by
// truncating. The run printed the collision and exited non-zero having
// replaced an existing catalog with an empty Parquet and emptied the log.
func TestOutputCollisionLeavesLogAndCatalogAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sample-small.qvd"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	dir := t.TempDir()
	// Two directories holding the same base name map to one output.
	var inDirs []string
	for _, name := range []string{"a", "b"} {
		sub := filepath.Join(dir, name)
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "same.qvd"), fixture, 0o600); err != nil {
			t.Fatal(err)
		}
		inDirs = append(inDirs, sub)
	}

	const catalogSentinel, logSentinel = "not a parquet file", "not a log\n"
	catalogPath := filepath.Join(dir, "catalog.parquet")
	logPath := filepath.Join(dir, "run.jsonl")
	if err := os.WriteFile(catalogPath, []byte(catalogSentinel), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(logSentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	args := append([]string{"--progress", "0", "--force",
		"--out-dir", filepath.Join(dir, "out"),
		"--catalog-out", catalogPath, "--log", logPath}, inDirs...)
	combined, err := exec.Command(bin, args...).CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitOutput {
		t.Fatalf("exit = %v, want %d\n%s", err, exitOutput, combined)
	}
	if !strings.Contains(string(combined), "output name collision") {
		t.Errorf("missing diagnostic:\n%s", combined)
	}

	for _, f := range []struct{ path, want string }{
		{catalogPath, catalogSentinel},
		{logPath, logSentinel},
	} {
		got, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatalf("read %s after rejection: %v", f.path, err)
		}
		if string(got) != f.want {
			t.Errorf("refused run rewrote %s: %q", filepath.Base(f.path), string(got))
		}
	}
}

// TestLogOpenFailureLeavesTheCatalogAlone covers a setup step that fails after
// the catalog writer exists.
//
// Reordering guards ahead of the writers did not cover this: NewLogWriter runs
// after both are open and can fail on its own, at which point the deferred
// close wrote a catalog for a run that never converted anything, over whatever
// catalog was already there. The writer is now armed by the run starting, so
// the caller's setup ordering cannot reintroduce this.
func TestLogOpenFailureLeavesTheCatalogAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	fixture := filepath.Join("..", "..", "testdata", "sample-small.qvd")
	const sentinel = "not a parquet file"

	for _, tc := range []struct {
		name string
		args func(dir, catalog, log string) []string
	}{
		{"single", func(dir, catalog, log string) []string {
			return []string{"--progress", "0", "--force",
				"--catalog-out", catalog, "--log", log,
				fixture, filepath.Join(dir, "out.parquet")}
		}},
		{"batch", func(dir, catalog, log string) []string {
			return []string{"--progress", "0", "--force",
				"--out-dir", filepath.Join(dir, "out"),
				"--catalog-out", catalog, "--log", log, fixture}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// A regular file where the log needs a directory, so NewLogWriter
			// fails after both writers have been created.
			blocker := filepath.Join(dir, "blocker")
			if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			catalogPath := filepath.Join(dir, "catalog.parquet")
			if err := os.WriteFile(catalogPath, []byte(sentinel), 0o644); err != nil {
				t.Fatal(err)
			}

			args := tc.args(dir, catalogPath, filepath.Join(blocker, "run.jsonl"))
			combined, err := exec.Command(bin, args...).CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != exitOutput {
				t.Fatalf("exit = %v, want %d\n%s", err, exitOutput, combined)
			}
			got, err := os.ReadFile(catalogPath)
			if err != nil {
				t.Fatalf("read catalog after failure: %v", err)
			}
			if string(got) != sentinel {
				t.Fatalf("a run that never started rewrote the catalog: %q", string(got))
			}
		})
	}
}

// TestSkippedFileThatCannotBeCataloguedFailsTheRun covers the one path where a
// catalog can come out incomplete rather than absent.
//
// A skipped file writes no rows of its own, so its existing output is scanned
// instead. That scan used to fail with a note and the run carried on, so a
// folder whose output could not be read produced exit 0 and a catalog silently
// missing a table -- worse than no catalog, because a job downstream has no way
// to tell. It is also a finding on its own terms: the manifest says the output
// is current and it cannot be read.
func TestSkippedFileThatCannotBeCataloguedFailsTheRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sample-small.qvd"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "A.qvd"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")

	// A first run to write the manifest the second one will skip on.
	if out, err := exec.Command(bin, "--progress", "0", "--out-dir", outDir,
		"--skip-up-to-date", src).CombinedOutput(); err != nil {
		t.Fatalf("first run: %v\n%s", err, out)
	}

	// Make the output unreadable as Parquet while leaving the size and
	// modification time the manifest compares against untouched, so the run
	// still decides the file is up to date. Overwriting in place rather than
	// removing permissions keeps this meaningful on Windows, where chmod does
	// not take away read access.
	output := filepath.Join(outDir, "A.parquet")
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, bytes.Repeat([]byte("x"), int(info.Size())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(output, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	catalogPath := filepath.Join(dir, "catalog.parquet")
	combined, err := exec.Command(bin, "--progress", "0", "--out-dir", outDir,
		"--skip-up-to-date", "--catalog-out", catalogPath, src).CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitOutput {
		t.Fatalf("exit = %v, want %d\n%s", err, exitOutput, combined)
	}
	if !strings.Contains(string(combined), "could not be read") {
		t.Errorf("missing diagnostic:\n%s", combined)
	}
	// Without --catalog-out the same folder is skipped silently, since nothing
	// asked for the output to be read.
	if out, err := exec.Command(bin, "--progress", "0", "--out-dir", outDir,
		"--skip-up-to-date", src).CombinedOutput(); err != nil {
		t.Fatalf("a run not asking for a catalog should still skip: %v\n%s", err, out)
	}
}

// TestRunThatConvertsNothingLeavesTheCatalogAlone covers every way a run can
// end before a single input has been accounted for.
//
// Arming the catalog when the run "started" was still too early: Run begins
// before it opens the input or checks the output, so a missing input exited 4
// having replaced the catalog with an empty Parquet. The writer is now armed
// by an input actually being converted, skipped or scanned, which is the only
// point at which there is something to describe.
func TestRunThatConvertsNothingLeavesTheCatalogAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	fixture := filepath.Join("..", "..", "testdata", "sample-small.qvd")
	const sentinel = "not a parquet file"

	for _, tc := range []struct {
		name string
		want int
		args func(dir, catalog string) []string
	}{
		{"missing input", exitInput, func(dir, catalog string) []string {
			return []string{"--progress", "0", "--force", "--catalog-out", catalog,
				filepath.Join(dir, "missing.qvd"), filepath.Join(dir, "out.parquet")}
		}},
		{"batch missing input", exitInput, func(dir, catalog string) []string {
			return []string{"--progress", "0", "--force",
				"--out-dir", filepath.Join(dir, "out"), "--catalog-out", catalog,
				filepath.Join(dir, "gone.qvd")}
		}},
		{"log cannot be opened", exitOutput, func(dir, catalog string) []string {
			blocker := filepath.Join(dir, "blocker")
			if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			return []string{"--progress", "0", "--force", "--catalog-out", catalog,
				"--log", filepath.Join(blocker, "run.jsonl"),
				fixture, filepath.Join(dir, "out.parquet")}
		}},
		{"scan finds nothing readable", exitInput, func(dir, catalog string) []string {
			junk := filepath.Join(dir, "junk")
			if err := os.Mkdir(junk, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(junk, "a.parquet"), []byte("nope"), 0o644); err != nil {
				t.Fatal(err)
			}
			return []string{"--progress", "0", "--force", "--catalog-scan",
				"--catalog-out", catalog, junk}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			catalogPath := filepath.Join(dir, "catalog.parquet")
			if err := os.WriteFile(catalogPath, []byte(sentinel), 0o644); err != nil {
				t.Fatal(err)
			}

			combined, err := exec.Command(bin, tc.args(dir, catalogPath)...).CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != tc.want {
				t.Fatalf("exit = %v, want %d\n%s", err, tc.want, combined)
			}
			got, err := os.ReadFile(catalogPath)
			if err != nil {
				t.Fatalf("read catalog after failure: %v", err)
			}
			if string(got) != sentinel {
				t.Fatalf("a run that accounted for nothing rewrote the catalog: %q", string(got))
			}
		})
	}
}

// TestScanThatReadsNothingSaysSo checks the reporting, not the writing.
//
// A scan in which every file failed accounts for nothing, so no catalog is
// written and the path keeps whatever it held. The summary line was printed
// unconditionally, so the run named a catalog that does not exist, which is
// the one thing a message about an output must not do.
func TestScanThatReadsNothingSaysSo(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk")
	if err := os.Mkdir(junk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(junk, "a.parquet"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(dir, "catalog.parquet")

	combined, err := exec.Command(bin, "--progress", "0", "--catalog-scan",
		"--catalog-out", catalogPath, junk).CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitInput {
		t.Fatalf("exit = %v, want %d\n%s", err, exitInput, combined)
	}
	if strings.Contains(string(combined), "wrote catalog to") {
		t.Errorf("announced a catalog it did not write:\n%s", combined)
	}
	if !strings.Contains(string(combined), "no catalog written") {
		t.Errorf("did not say the catalog was skipped:\n%s", combined)
	}
	if _, err := os.Stat(catalogPath); !os.IsNotExist(err) {
		t.Errorf("catalog exists after a scan that read nothing (err = %v)", err)
	}

	// A scan that read something still announces it, counting only the files
	// it managed to read.
	good := filepath.Join(junk, "good.parquet")
	if out, err := exec.Command(bin, "--progress", "0",
		filepath.Join("..", "..", "testdata", "sample-small.qvd"), good).CombinedOutput(); err != nil {
		t.Fatalf("convert: %v\n%s", err, out)
	}
	combined, err = exec.Command(bin, "--progress", "0", "--catalog-scan",
		"--catalog-out", catalogPath, junk).CombinedOutput()
	exitErr, ok = err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitInput {
		t.Fatalf("partial scan exit = %v, want %d\n%s", err, exitInput, combined)
	}
	if !strings.Contains(string(combined), "from 1 file(s)") {
		t.Errorf("partial scan should count only what it read:\n%s", combined)
	}
	if _, err := os.Stat(catalogPath); err != nil {
		t.Errorf("partial scan wrote no catalog: %v", err)
	}
}

// --duplicate-names has to hold end to end: the default refuses a schema whose
// columns collide, and the suffix mode writes every one of them, says so on
// screen, and records what it did in the log and the schema report.
func TestDuplicateNamesEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	fixture := filepath.Join("..", "..", "testdata", "sample-small.qvd")
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("fixture missing, this test would otherwise pass by skipping: %v", err)
	}
	// Every field renamed to "F", which is the collision a too-coarse
	// --field-regex produces on a real extract.
	collide := []string{"--field-regex", ".", "--field-name", "F"}

	t.Run("default refuses", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "out.parquet")
		args := append(append([]string{}, collide...), "--progress", "0", fixture, out)
		cmd := exec.Command(bin, args...)
		combined, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("conversion accepted colliding column names:\n%s", combined)
		}
		if got := cmd.ProcessState.ExitCode(); got != exitSchema {
			t.Errorf("exit code = %d, want %d (schema/type policy):\n%s", got, exitSchema, combined)
		}
		if !strings.Contains(string(combined), "--duplicate-names=suffix") {
			t.Errorf("the refusal should name the mode that keeps both:\n%s", combined)
		}
		if _, err := os.Stat(out); err == nil {
			t.Error("a refused conversion must not leave an output file")
		}
	})

	t.Run("suffix keeps every column", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "out.parquet")
		logPath := filepath.Join(dir, "run.jsonl")
		report := filepath.Join(dir, "schema.json")
		args := append(append([]string{}, collide...), "--duplicate-names", "suffix",
			"--progress", "0", "--log", logPath, "--schema-report", report, fixture, out)
		cmd := exec.Command(bin, args...)
		combined, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run failed: %v\n%s", err, combined)
		}
		if !strings.Contains(string(combined), `duplicate-names: 6 duplicate output column name(s)`) {
			t.Errorf("the run should report the renames:\n%s", combined)
		}

		records := readLogRecords(t, logPath)
		if len(records) != 2 {
			t.Fatalf("got %d log records, want file and summary", len(records))
		}
		// Seven fields, six of them renamed: the first keeps "F".
		if records[0]["duplicateNames"] != float64(6) || records[0]["columns"] != float64(7) {
			t.Errorf("file record = %v", records[0])
		}

		raw, err := os.ReadFile(report)
		if err != nil {
			t.Fatalf("read schema report: %v", err)
		}
		var rep struct {
			DuplicateNames []struct{ From, To string } `json:"duplicateNames"`
			Columns        []struct {
				Name         string `json:"name"`
				SourceColumn string `json:"sourceColumn"`
			} `json:"columns"`
		}
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatalf("schema report is not JSON: %v", err)
		}
		if len(rep.DuplicateNames) != 6 {
			t.Fatalf("report lists %d renames, want 6: %s", len(rep.DuplicateNames), raw)
		}
		if d := rep.DuplicateNames[0]; d.From != "F" || d.To != "F_2" {
			t.Errorf("first rename = %q -> %q, want \"F\" -> \"F_2\"", d.From, d.To)
		}
		// Every source field must still be there, under a name of its own.
		names := map[string]string{}
		for _, c := range rep.Columns {
			if prev, ok := names[c.Name]; ok {
				t.Errorf("column %q written twice, for %q and %q", c.Name, prev, c.SourceColumn)
			}
			names[c.Name] = c.SourceColumn
		}
		if len(names) != 7 {
			t.Errorf("report has %d output columns, want 7: %v", len(names), names)
		}
	})
}
