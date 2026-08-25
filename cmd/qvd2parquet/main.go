// Command qvd2parquet converts Qlik QVD files to Parquet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/ralforion/qvd2parquet/internal/convert"
	"github.com/ralforion/qvd2parquet/internal/parquetwrite"
	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// Exit codes, as documented in the README.
const (
	exitOK          = 0
	exitUsage       = 1
	exitUnsupported = 2
	exitSchema      = 3
	exitInput       = 4
	exitOutput      = 5
	exitQuality     = 6
	exitCanceled    = 7
)

// Program identity. version is overridden at build time with
// -ldflags "-X main.version=..."; scripts/build-release.sh does this.
const (
	programName = "qvd2parquet"
	copyright   = "(c) 2026, RALFORION d.o.o."
)

// defaultVersion is what a plain "go build" reports. Release archives override
// version with -ldflags, and "go install module@vX.Y.Z" supplies the tag
// through the embedded build info, so all three paths agree.
const defaultVersion = "2.1.0"

var version = defaultVersion

func init() {
	// Only consult the build info when -ldflags did not already set a version,
	// so an explicit release stamp always wins.
	if version != defaultVersion {
		return
	}
	if v, ok := taggedModuleVersion(); ok {
		version = v
	}
}

// releaseTag matches a plain semantic version. Go records "(devel)" for a
// local build and a pseudo-version such as
// "v0.3.1-0.20260821175410-838a20064371" for an untagged commit; neither is a
// release, so both are ignored in favour of defaultVersion.
var releaseTag = regexp.MustCompile(`^v(\d+\.\d+\.\d+)$`)

func taggedModuleVersion() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	m := releaseTag.FindStringSubmatch(info.Main.Version)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// banner is the identification line printed at startup. It goes to stderr so
// it never contaminates piped output.
func banner() string {
	// The release script stamps a git tag such as "v0.3.0"; render every build
	// path identically by dropping the leading "v".
	return fmt.Sprintf("%s %s  %s", programName, strings.TrimPrefix(version, "v"), copyright)
}

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet(programName, flag.ContinueOnError)
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintf(out, "%s\n", banner())
		fmt.Fprintf(out, "Convert Qlik QVD files to Parquet.\n\n")
		fmt.Fprintf(out, "Usage:\n  qvd2parquet [options] input.qvd output.parquet\n")
		fmt.Fprintf(out, "  qvd2parquet --out-dir DIR [options] <file-or-directory>...\n")
		fmt.Fprintf(out, "  qvd2parquet --inspect [options] input.qvd\n\n")
		fmt.Fprintf(out, "Options:\n")
		fs.PrintDefaults()
		fmt.Fprintf(out, "\nExit codes:\n"+
			"  0  success\n"+
			"  1  CLI usage error\n"+
			"  2  unsupported QVD feature\n"+
			"  3  schema/type policy error\n"+
			"  4  input read/decode error\n"+
			"  5  output/write error\n"+
			"  6  quality gate failure\n"+
			"  7  cancelled by Ctrl-C or SIGTERM\n")
	}

	def := convert.DefaultOptions()
	var (
		columns       = fs.String("columns", "", "Convert only these comma-separated columns")
		exclude       = fs.String("exclude", "", "Skip fields matching these comma-separated wildcard patterns, e.g. '%*'")
		fieldRegex    = fs.String("field-regex", "", "Rewrite field names with this regexp; use (?P<name>...) and optional (?P<comment>...)")
		fieldName     = fs.String("field-name", "", "Template for the new column name (default \"${name}\")")
		fieldComment  = fs.String("field-comment", "", "Template for the column comment (default \"${comment}\")")
		mixed         = fs.String("mixed", def.Mixed.String(), "Mixed-type strategy: error|string|promote|dual-columns")
		dual          = fs.String("dual", def.Dual.String(), "Dual strategy: auto|numeric|text|columns")
		promote       = fs.String("numeric-promote", def.NumericPromote.String(), "Numeric widening: decimal (exact, scale inferred from values) | true (float64) | false")
		strFallback   = fs.Bool("mixed-string-fallback", def.MixedStringFallback, "Convert otherwise-invalid mixed columns to string")
		decSource     = fs.String("decimal-source", def.DecimalSource.String(), "Decimal extraction: auto|text|numeric")
		decStrict     = fs.Bool("decimal-strict", def.DecimalStrict, "Fail instead of rounding when a decimal value does not fit its scale")
		compression   = fs.String("compression", def.Compression, "Parquet compression: zstd|snappy|gzip|uncompressed")
		batchRows     = fs.Int("batch-rows", def.BatchRows, "Rows per Arrow batch, 0 sizes it from the column count to hold in-flight memory steady")
		rowGroupRows  = fs.Int("row-group-rows", def.RowGroupRows, "Rows per Parquet row group")
		workers       = fs.Int("workers", def.Workers, "Decode workers, 0 means one per 2 CPUs (minimum 2)")
		timezone      = fs.String("timezone", def.TimezoneName, "none|Local|UTC|IANA timezone name for date/time conversion; none writes a naive wall clock")
		schemaPath    = fs.String("schema", "", "Optional explicit schema override JSON")
		schemaReport  = fs.String("schema-report", "", "Write the inferred schema/profile report to this path")
		qualityGate   = fs.String("quality-gate", def.Quality.String(), "Validation mode: none|basic|numeric|full")
		qualityReport = fs.String("quality-report", "", "Write the post-conversion quality report to this path")
		qualityTol    = fs.Float64("quality-tolerance", def.QualityRelTolerance, "Relative tolerance for floating-point quality checks")
		qualityAbsTol = fs.Float64("quality-abs-tolerance", def.QualityAbsTolerance, "Absolute tolerance for floating-point quality checks")
		progress      = fs.Int64("progress", def.ProgressEvery, "Log every N rows, 0 disables progress")
		force         = fs.Bool("force", false, "Overwrite an existing output file")
		strict        = fs.Bool("strict", false, "Enable strict validation defaults")
		emptyAsNull   = fs.Bool("empty-as-null", def.EmptyStringAsNull, "Write an empty string symbol as null, as Qlik treats it")
		inferDates    = fs.Bool("infer-dates", def.InferDates, "Read an untyped column as a date/timestamp when its display strings render its serial value as one")
		outDir        = fs.String("out-dir", "", "Convert every input into this directory, one .parquet per .qvd")
		fileWorkers   = fs.Int("file-workers", 1, "Files to convert at once; decode workers are divided between them")
		recursive     = fs.Bool("recursive", false, "With --out-dir, descend into subdirectories")
		logPath       = fs.String("log", "", "Write one JSON Lines record per input, then a summary")
		inspect       = fs.Bool("inspect", false, "Read only the header and symbol tables, print the schema, and exit")
		showVersion   = fs.Bool("version", false, "Print the version and exit")
	)

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *showVersion {
		fmt.Println(banner())
		return exitOK
	}
	// Three shapes: a 1:1 conversion, an inspect, and a batch into --out-dir.
	batch := *outDir != ""
	switch {
	case batch && fs.NArg() < 1:
		fmt.Fprintf(os.Stderr, "%s: --out-dir needs at least one input file or directory\n\n", programName)
		fs.Usage()
		return exitUsage
	case batch && *inspect:
		fmt.Fprintf(os.Stderr, "%s: --inspect and --out-dir cannot be combined; "+
			"inspect one file at a time\n", programName)
		return exitUsage
	case *inspect && *logPath != "":
		fmt.Fprintf(os.Stderr, "%s: --log records conversions and cannot be combined with --inspect\n", programName)
		return exitUsage
	case !batch && *inspect && fs.NArg() != 1:
		fmt.Fprintf(os.Stderr, "%s: --inspect expects an input path, got %d argument(s)\n\n",
			programName, fs.NArg())
		fs.Usage()
		return exitUsage
	case !batch && !*inspect && fs.NArg() != 2:
		fmt.Fprintf(os.Stderr, "%s: expected an input and an output path, got %d argument(s); "+
			"use --out-dir to convert several files\n\n", programName, fs.NArg())
		fs.Usage()
		return exitUsage
	}
	var inputPath, outputPath string
	if !batch {
		inputPath = fs.Arg(0)
		if !*inspect {
			outputPath = fs.Arg(1)
		}
	}

	var err error
	opts := def
	opts.MixedStringFallback = *strFallback
	opts.DecimalStrict = *decStrict
	opts.InferDates = *inferDates
	opts.EmptyStringAsNull = *emptyAsNull
	opts.Compression = *compression
	opts.BatchRows = *batchRows
	opts.RowGroupRows = *rowGroupRows
	opts.Workers = *workers
	opts.SchemaOverridePath = *schemaPath
	opts.SchemaReportPath = *schemaReport
	opts.QualityReportPath = *qualityReport
	opts.QualityRelTolerance = *qualityTol
	opts.QualityAbsTolerance = *qualityAbsTol
	opts.ProgressEvery = *progress
	opts.Force = *force
	opts.Strict = *strict

	opts.Columns = splitList(*columns)
	opts.Exclude = splitList(*exclude)

	if opts.Renamer, err = convert.NewFieldRenamer(*fieldRegex, *fieldName, *fieldComment); err != nil {
		return usageErr(err)
	}

	if opts.NumericPromote, err = convert.ParseNumericPromote(*promote); err != nil {
		return usageErr(err)
	}
	opts.NumericPromoteExplicit = fsSet(fs, "numeric-promote")
	if opts.Mixed, err = convert.ParseMixedStrategy(*mixed); err != nil {
		return usageErr(err)
	}
	if opts.Dual, err = convert.ParseDualStrategy(*dual); err != nil {
		return usageErr(err)
	}
	if opts.DecimalSource, err = convert.ParseDecimalSource(*decSource); err != nil {
		return usageErr(err)
	}
	if opts.Quality, err = convert.ParseQualityMode(*qualityGate); err != nil {
		return usageErr(err)
	}
	if _, err = parquetwrite.ParseCompression(opts.Compression); err != nil {
		return usageErr(err)
	}
	if opts.Location, opts.NaiveTimestamps, err = qvd.ParseLocation(*timezone); err != nil {
		return usageErr(err)
	}
	opts.TimezoneName = *timezone
	if *strict {
		// Strict mode refuses any silent type widening or lossy decimal.
		opts.DecimalStrict = true
		if !fsSet(fs, "numeric-promote") {
			opts.NumericPromote = convert.PromoteNone
		}
	}
	if err := opts.Validate(); err != nil {
		return usageErr(err)
	}

	// Handled explicitly rather than with signal.NotifyContext, whose stop
	// function cancels the context as well as unregistering the handler. A
	// goroutine waiting on Done therefore cannot tell a real signal from the
	// deferred cleanup of a successful run, and announces a cancellation on
	// almost every one.
	//
	// Once a signal arrives the default handler is restored, so an impatient
	// second Ctrl-C terminates the process outright: the first one only asks,
	// and the step in progress -- the quality gate on a wide file -- can take
	// minutes to wind down. A signal with no visible effect reads as a hang,
	// hence the message.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
			cancel()
			signal.Stop(signals)
			fmt.Fprintf(os.Stderr,
				"%s: cancelling, finishing the current step; press Ctrl-C again to stop now\n",
				programName)
		case <-ctx.Done():
			// The run finished and the deferred cancel fired. Not a signal:
			// say nothing.
		}
	}()

	fmt.Fprintln(os.Stderr, banner())

	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, programName+": "+format+"\n", args...)
	}

	if *inspect {
		return runInspect(inputPath, &opts)
	}
	if batch {
		return runBatch(ctx, fs.Args(), &opts, *outDir, *fileWorkers, *recursive, *logPath, logf)
	}

	return runSingle(ctx, inputPath, outputPath, &opts, *logPath, logf)
}

// runSingle converts one explicit input/output pair. Its log uses the same
// file-plus-summary records as runBatch so automation can query either mode
// without knowing how many files the command converted.
func runSingle(ctx context.Context, inputPath, outputPath string, opts *convert.Options,
	logPath string, logf convert.Logf) int {

	var log *convert.LogWriter
	if logPath != "" {
		if err := validateLogPath(logPath, inputPath, outputPath, opts); err != nil {
			return usageErr(err)
		}
		var err error
		if log, err = convert.NewLogWriter(logPath); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", programName, err)
			return exitOutput
		}
		defer log.Close()
	}

	started := time.Now()
	stats, quality, err := convert.Run(ctx, inputPath, outputPath, opts, logf)
	elapsed := time.Since(started)
	if log != nil {
		result := convert.FileResult{
			Input: inputPath, Output: outputPath, Stats: stats, Quality: quality,
			Err: err, Started: started, Elapsed: elapsed,
		}
		summary := &convert.BatchResult{Results: []convert.FileResult{result}, Elapsed: elapsed}
		if err != nil {
			summary.Failed = 1
		} else {
			summary.Converted = 1
			summary.Rows = stats.Rows
			summary.Bytes = stats.OutputBytes
		}
		log.File(result)
		log.Summary(summary)
		fmt.Fprintf(os.Stderr, "%s: wrote %s\n", programName, logPath)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", programName, err)
		return exitCodeFor(err)
	}

	logf("wrote %s: %d rows, %d columns, %s in %s overall (%.0f rows/s)",
		outputPath, stats.Rows, stats.Columns, humanBytes(stats.OutputBytes),
		stats.Elapsed.Round(1e6), stats.RowsPerSecond())
	return exitOK
}

// validateLogPath prevents NewLogWriter from truncating a conversion input or
// sharing a destination with another output.
func validateLogPath(logPath, inputPath, outputPath string, opts *convert.Options) error {
	paths := []struct {
		name string
		path string
	}{
		{"the input path", inputPath},
		{"the output path", outputPath},
		{"--schema", opts.SchemaOverridePath},
		{"--schema-report", opts.SchemaReportPath},
		{"--quality-report", opts.QualityReportPath},
	}
	for _, p := range paths {
		if p.path != "" && samePath(logPath, p.path) {
			return fmt.Errorf("--log path must differ from %s", p.name)
		}
	}
	return nil
}

// samePath resolves existing symlinks and the nearest existing parent. The
// latter catches two not-yet-created files beneath differently spelled aliases
// of the same directory.
func samePath(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	if errA == nil && errB == nil && os.SameFile(infoA, infoB) {
		return true
	}
	pathA, pathB := canonicalPath(a), canonicalPath(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(pathA, pathB)
	}
	return pathA == pathB
}

func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}

	// EvalSymlinks cannot resolve a dangling final symlink, but Create follows
	// it. Follow that final link explicitly before resolving its parent.
	for i := 0; i < 255; i++ {
		info, err := os.Lstat(abs)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			break
		}
		target, err := os.Readlink(abs)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(abs), target)
		}
		abs = filepath.Clean(target)
	}

	current := abs
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Clean(filepath.Join(parts...))
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(abs)
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}

// runBatch converts every input into --out-dir, continuing past a failure so
// one bad file does not hide the state of the rest.
func runBatch(ctx context.Context, paths []string, opts *convert.Options,
	outDir string, fileWorkers int, recursive bool, logPath string, logf convert.Logf) int {

	inputs, problems := convert.FindInputs(paths, recursive)
	if len(inputs) == 0 && len(problems) == 0 {
		fmt.Fprintf(os.Stderr, "%s: no .qvd files found in %s\n",
			programName, strings.Join(paths, ", "))
		return exitUsage
	}

	// The output directory has to exist before the log, which commonly lives
	// inside it.
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "%s: create output directory %s: %v\n", programName, outDir, err)
		return exitOutput
	}

	var log *convert.LogWriter
	if logPath != "" {
		var err error
		if log, err = convert.NewLogWriter(logPath); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", programName, err)
			return exitOutput
		}
		defer log.Close()
	}

	result, err := convert.RunMany(ctx, inputs, opts, &convert.ManyOptions{
		OutDir:      outDir,
		FileWorkers: fileWorkers,
		Recursive:   recursive,
		Log:         log,
		Problems:    problems,
	}, logf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", programName, err)
		return exitCodeFor(err)
	}

	fmt.Fprintln(os.Stderr, result.Summary())
	if logPath != "" {
		fmt.Fprintf(os.Stderr, "%s: wrote %s\n", programName, logPath)
	}
	return result.ExitCode(exitCodeFor)
}

// runInspect reads the header and symbol tables only, then prints the schema a
// conversion would produce. The report is the command's result, so it goes to
// stdout; diagnostics stay on stderr.
func runInspect(inputPath string, opts *convert.Options) int {
	rep, err := convert.Inspect(inputPath, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", programName, err)
		return exitCodeFor(err)
	}
	defer rep.Close()

	if err := rep.Write(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", programName, err)
		return exitOutput
	}
	if opts.SchemaReportPath != "" {
		if rep.Schema == nil {
			fmt.Fprintf(os.Stderr, "%s: no schema to report: %v\n", programName, rep.SchemaErr)
			return exitSchema
		}
		if err := convert.WriteSchemaReport(opts.SchemaReportPath, inputPath, rep.File, rep.Schema); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", programName, err)
			return exitOutput
		}
		fmt.Fprintf(os.Stderr, "%s: wrote schema report to %s\n", programName, opts.SchemaReportPath)
	}
	// A file the type policy rejects exits non-zero, so scripts can gate on it.
	if rep.SchemaErr != nil {
		return exitCodeFor(rep.SchemaErr)
	}
	return exitOK
}

// splitList parses a comma-separated flag value, dropping blank entries.
func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// fsSet reports whether the named flag was given explicitly.
func fsSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func usageErr(err error) int {
	fmt.Fprintf(os.Stderr, "%s: %v\n", programName, err)
	return exitUsage
}

// exitCodeFor maps an error onto the documented exit codes.
func exitCodeFor(err error) int {
	switch {
	case errors.Is(err, convert.ErrQualityGate):
		return exitQuality
	case errors.Is(err, parquetwrite.ErrOutput):
		return exitOutput
	case errors.Is(err, convert.ErrSchemaPolicy):
		return exitSchema
	case errors.Is(err, qvd.ErrUnsupported):
		return exitUnsupported
	case errors.Is(err, convert.ErrCanceled),
		// A bare context error from any path that has not wrapped it: still a
		// cancellation, and must not fall through to the input-error code.
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return exitCanceled
	case errors.Is(err, convert.ErrInput):
		return exitInput
	default:
		return exitInput
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
