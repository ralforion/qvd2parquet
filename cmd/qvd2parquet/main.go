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
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/ralforion/qvd2parquet/internal/catalog"
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
const defaultVersion = "2.6.1"

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
		fmt.Fprintf(out, "  qvd2parquet --inspect [options] input.qvd\n")
		fmt.Fprintf(out, "  qvd2parquet --catalog-scan --catalog-out catalog.parquet <file-or-directory>...\n\n")
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
		dupNames      = fs.String("duplicate-names", def.DuplicateNames.String(), "Two columns resolving to one name: error|suffix (suffix keeps both as NAME and NAME_2)")
		promote       = fs.String("numeric-promote", def.NumericPromote.String(), "Numeric widening: decimal (exact, scale inferred from values) | true (float64) | false")
		strFallback   = fs.Bool("mixed-string-fallback", def.MixedStringFallback, "Convert otherwise-invalid mixed columns to string")
		decSource     = fs.String("decimal-source", def.DecimalSource.String(), "Decimal extraction: auto|text|numeric")
		decStrict     = fs.Bool("decimal-strict", def.DecimalStrict, "Fail instead of rounding when a decimal value does not fit its scale")
		compression   = fs.String("compression", def.Compression, "Parquet compression: zstd|snappy|gzip|uncompressed")
		encoding      = fs.String("encoding", "", "Pin column encodings: PATTERN=ENCODING,... or 'auto' to measure per file")
		batchRows     = fs.Int("batch-rows", def.BatchRows, "Rows per Arrow batch, 0 sizes it from the column count to hold in-flight memory steady")
		rowGroupRows  = fs.Int("row-group-rows", def.RowGroupRows, "Rows per Parquet row group")
		workers       = fs.Int("workers", def.Workers, "Decode workers, 0 means one per 2 CPUs (minimum 2)")
		timezone      = fs.String("timezone", def.TimezoneName, "none|Local|UTC|IANA timezone name for date/time conversion; none writes a naive wall clock")
		schemaPath    = fs.String("schema", "", "Optional explicit schema override JSON")
		schemaReport  = fs.String("schema-report", "", "Write the inferred schema/profile report to this path")
		qualityGate   = fs.String("quality-gate", def.Quality.String(), "Validation mode: none|basic|numeric|full|reread")
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
		includeFiles  = fs.String("include-files", "", "With --out-dir, convert only the files matching these comma-separated wildcard patterns, e.g. 'CE*'")
		excludeFiles  = fs.String("exclude-files", "", "With --out-dir, skip the files matching these comma-separated wildcard patterns")
		skipUpToDate  = fs.Bool("skip-up-to-date", false, "With --out-dir, leave a file alone when this exact run already produced its output")
		logPath       = fs.String("log", "", "Write one JSON Lines record per input, then a summary")
		consolePath   = fs.String("console-log", "", "Also write the screen output to this file, as it is printed")
		inspect       = fs.Bool("inspect", false, "Read only the header and symbol tables, print the schema, and exit")
		catalogOut    = fs.String("catalog-out", "", "Write a column-grain catalog of the run to this Parquet path: one row per output column, with its comment")
		catalogScan   = fs.Bool("catalog-scan", false, "With --catalog-out, read the columns out of existing .parquet inputs instead of converting")
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
	// Four shapes: a 1:1 conversion, an inspect, a batch into --out-dir, and a
	// catalog scan of Parquet files something else already wrote.
	batch := *outDir != ""
	scan := *catalogScan
	switch {
	case scan && *catalogOut == "":
		fmt.Fprintf(stderr, "%s: --catalog-scan needs --catalog-out to write to\n", programName)
		return exitUsage
	case scan && *logPath != "":
		fmt.Fprintf(stderr, "%s: --log records conversions and cannot be combined with --catalog-scan\n", programName)
		return exitUsage
	case scan && (batch || *inspect):
		fmt.Fprintf(stderr, "%s: --catalog-scan reads finished Parquet files and "+
			"converts nothing, so it cannot be combined with --out-dir or --inspect\n", programName)
		return exitUsage
	case scan && fs.NArg() < 1:
		fmt.Fprintf(stderr, "%s: --catalog-scan needs at least one .parquet file or directory\n\n", programName)
		fs.Usage()
		return exitUsage
	case batch && fs.NArg() < 1:
		fmt.Fprintf(stderr, "%s: --out-dir needs at least one input file or directory\n\n", programName)
		fs.Usage()
		return exitUsage
	case batch && *inspect:
		fmt.Fprintf(stderr, "%s: --inspect and --out-dir cannot be combined; "+
			"inspect one file at a time\n", programName)
		return exitUsage
	case *inspect && *logPath != "":
		fmt.Fprintf(stderr, "%s: --log records conversions and cannot be combined with --inspect\n", programName)
		return exitUsage
	case !batch && !scan && *inspect && fs.NArg() != 1:
		fmt.Fprintf(stderr, "%s: --inspect expects an input path, got %d argument(s)\n\n",
			programName, fs.NArg())
		fs.Usage()
		return exitUsage
	case !batch && !scan && !*inspect && fs.NArg() != 2:
		fmt.Fprintf(stderr, "%s: expected an input and an output path, got %d argument(s); "+
			"use --out-dir to convert several files\n\n", programName, fs.NArg())
		fs.Usage()
		return exitUsage
	}
	var inputPath, outputPath string
	if !batch && !scan {
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

	if opts.Encodings, err = convert.ParseEncodingSpec(*encoding); err != nil {
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
	if opts.DuplicateNames, err = convert.ParseDuplicateNamePolicy(*dupNames); err != nil {
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
			fmt.Fprintf(stderr,
				"%s: cancelling, finishing the current step; press Ctrl-C again to stop now\n",
				programName)
		case <-ctx.Done():
			// The run finished and the deferred cancel fired. Not a signal:
			// say nothing.
		}
	}()

	fmt.Fprintln(stderr, banner())

	logf := func(format string, args ...any) {
		fmt.Fprintf(stderr, programName+": "+format+"\n", args...)
	}

	if scan {
		return runCatalogScan(fs.Args(), *catalogOut, *consolePath, *recursive, opts.Force, logf)
	}

	if *inspect {
		return runInspect(ctx, inputPath, &opts, *catalogOut, *consolePath, logf)
	}
	if batch {
		sel := convert.InputSelection{
			Recursive: *recursive,
			Include:   splitList(*includeFiles),
			Exclude:   splitList(*excludeFiles),
		}
		return runBatch(ctx, fs.Args(), &opts, *outDir, *fileWorkers, sel, *skipUpToDate,
			*logPath, *catalogOut, *consolePath, logf)
	}

	return runSingle(ctx, inputPath, outputPath, &opts, *logPath, *catalogOut, *consolePath, logf)
}

// openCatalog creates the run's catalog writer and returns a function that
// closes it, reporting the exit code the closing warrants.
//
// It is called only after the path guards have run, for the same reason
// NewLogWriter is: the writer replaces whatever file it is pointed at, so
// creating it before the guards would let a run that was correctly refused
// destroy the input it was refused for.
//
// The close returns a code rather than only printing, because the catalog is
// an output a scheduled job waits on. A run whose conversion succeeded and
// whose catalog could not be written has not done what it was asked, and
// exiting 0 would tell the job otherwise.
func openCatalog(path string, opts *convert.Options, logf convert.Logf) (func() int, error) {
	if path == "" {
		return func() int { return exitOK }, nil
	}
	cat, err := catalog.NewWriter(path, version, opts.Force)
	if err != nil {
		return nil, err
	}
	opts.Catalog = cat
	return func() int {
		if err := cat.Close(); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", programName, err)
			return exitCodeFor(err)
		}
		// A run that never began writes no catalog, so there is nothing to
		// announce and the existing file at that path is still whatever it was.
		if !cat.Started() {
			return exitOK
		}
		logf("wrote catalog to %s: %d column(s)", cat.Path(), cat.Len())
		return exitOK
	}, nil
}

// closeCatalogInto runs the close and upgrades an otherwise successful exit
// code to the failure. A conversion that already failed keeps its own code:
// that is the failure worth reporting, and the catalog's is a consequence.
func closeCatalogInto(code *int, closeCatalog func() int) {
	if c := closeCatalog(); c != exitOK && *code == exitOK {
		*code = c
	}
}

// runSingle converts one explicit input/output pair. Its log uses the same
// file-plus-summary records as runBatch so automation can query either mode
// without knowing how many files the command converted.
func runSingle(ctx context.Context, inputPath, outputPath string, opts *convert.Options,
	logPath, catalogPath, consolePath string, logf convert.Logf) (code int) {

	// Every path guard runs before either writer is created. Both the log and
	// the catalog are written by truncating, so a writer opened ahead of a
	// guard that then refuses the run destroys a file on the way out -- and
	// the refusal it prints makes that damage look impossible.
	if err := validateCatalogPath(catalogPath, inputPath, outputPath, opts); err != nil {
		return usageErr(err)
	}
	if err := checkCollisions(catalogPath, "--catalog-out",
		[]logCollision{{"--log", logPath}}); err != nil {
		return usageErr(err)
	}
	if logPath != "" {
		if err := validateLogPath(logPath, inputPath, outputPath, opts); err != nil {
			return usageErr(err)
		}
	}
	if err := validateWriterPath(consolePath, "--console-log", inputPath, outputPath, opts); err != nil {
		return usageErr(err)
	}
	if code := startConsoleLog(consolePath, []logCollision{
		{"--log", logPath}, {"--catalog-out", catalogPath},
	}); code != exitOK {
		return code
	}
	defer stderr.Close()

	closeCatalog, err := openCatalog(catalogPath, opts, logf)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitCodeFor(err)
	}
	defer closeCatalogInto(&code, closeCatalog)

	var log *convert.LogWriter
	if logPath != "" {
		var err error
		if log, err = convert.NewLogWriter(logPath); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", programName, err)
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
		if err != nil {
			// A conversion that got past the header knows the table, and a
			// failed record is the one worth being able to group. Stats
			// carries it when the conversion succeeded.
			result.Table = convert.TableNameOf(inputPath)
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
		fmt.Fprintf(stderr, "%s: wrote %s\n", programName, logPath)
	}

	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitCodeFor(err)
	}

	logf("wrote %s: %d rows, %d columns, %s in %s overall (%.0f rows/s)",
		outputPath, stats.Rows, stats.Columns, humanBytes(stats.OutputBytes),
		stats.Elapsed.Round(1e6), stats.RowsPerSecond())
	return exitOK
}

// logCollision is a path the log must not share with another file, paired with
// the name to report it under.
type logCollision struct {
	name string
	path string
}

// checkLogCollisions reports the first path the log would collide with.
// NewLogWriter creates its file with O_TRUNC, so a collision destroys whichever
// of the two is written second while the run still reports writing both.
func checkLogCollisions(logPath string, paths []logCollision) error {
	return checkCollisions(logPath, "--log", paths)
}

// checkCollisions is the same check for any output that truncates what it
// opens. The flag is named in the message because --log and --catalog-out
// share the guard and only the caller knows which one is at fault.
func checkCollisions(path, flag string, paths []logCollision) error {
	for _, p := range paths {
		if p.path != "" && samePath(path, p.path) {
			return fmt.Errorf("%s path must differ from %s", flag, p.name)
		}
	}
	return nil
}

// validateWriterPath prevents a writer that truncates what it opens from
// destroying a conversion input or sharing a destination with another output.
// Three flags need it and need exactly the same list: --log, --catalog-out and
// --console-log.
func validateWriterPath(path, flag, inputPath, outputPath string, opts *convert.Options) error {
	if path == "" {
		return nil
	}
	return checkCollisions(path, flag, []logCollision{
		{"the input path", inputPath},
		{"the output path", outputPath},
		{"--schema", opts.SchemaOverridePath},
		{"--schema-report", opts.SchemaReportPath},
		{"--quality-report", opts.QualityReportPath},
	})
}

// validateLogPath is the guard for --log.
func validateLogPath(logPath, inputPath, outputPath string, opts *convert.Options) error {
	return validateWriterPath(logPath, "--log", inputPath, outputPath, opts)
}

// validateCatalogPath is the same guard for --catalog-out. Its writer
// truncates whatever it opens, exactly as the log's does, so pointing it at an
// input would destroy the file the run was asked to read.
func validateCatalogPath(catalogPath, inputPath, outputPath string, opts *convert.Options) error {
	return validateWriterPath(catalogPath, "--catalog-out", inputPath, outputPath, opts)
}

// validateBatchWriterPath is the batch guard, where none of the paths at risk
// were typed on the command line: the inputs come from expanding directories,
// and every output and per-file report is derived from an input under
// --out-dir. It has to run after FindInputs for that reason, and before the
// writer, which truncates whatever it opens.
//
// The message names the offending file rather than its role, because a batch
// may have found hundreds and "the input path" would not say which one.
func validateBatchWriterPath(path, flag string, inputs []string,
	problems []convert.InputProblem, outDir string, opts *convert.Options) error {

	if path == "" {
		return nil
	}
	if err := checkCollisions(path, flag, []logCollision{
		{"--schema", opts.SchemaOverridePath},
	}); err != nil {
		return err
	}
	// A path FindInputs could not examine is still an input: the run reports it
	// as a failed file and writes it to the log. Letting a writer take that
	// path produced a run that named the file as missing and created it in the
	// same breath, the log's first record reporting the failure of the path it
	// was being written to. No output or report is derived from one, so the
	// path itself is the whole check.
	for _, p := range problems {
		if err := checkCollisions(path, flag, []logCollision{
			{"the input " + p.Path, p.Path},
		}); err != nil {
			return err
		}
	}
	for _, in := range inputs {
		out := convert.OutputPathFor(in, outDir)
		if err := checkCollisions(path, flag, []logCollision{
			{"the input " + in, in},
			{"the output " + out, out},
			{"the --schema-report for " + in,
				convert.PerFileReportPath(opts.SchemaReportPath, in, outDir)},
			{"the --quality-report for " + in,
				convert.PerFileReportPath(opts.QualityReportPath, in, outDir)},
		}); err != nil {
			return err
		}
	}
	return nil
}

// validateBatchCatalogPath is the batch guard for --catalog-out.
func validateBatchCatalogPath(catalogPath string, inputs []string,
	problems []convert.InputProblem, outDir string, opts *convert.Options) error {

	return validateBatchWriterPath(catalogPath, "--catalog-out", inputs, problems, outDir, opts)
}

// validateBatchLogPath is the batch guard for --log.
func validateBatchLogPath(logPath string, inputs []string, problems []convert.InputProblem,
	outDir string, opts *convert.Options) error {

	return validateBatchWriterPath(logPath, "--log", inputs, problems, outDir, opts)
}

// startConsoleLog checks --console-log against every path the run reads or
// writes and then attaches it, so from here on the screen output is recorded.
// It returns the exit code to fail with, or exitOK.
//
// The guards matter more here than anywhere else: the file is created by
// truncating, and it is the one output a user is likely to point at a folder
// full of the run's own files.
func startConsoleLog(path string, paths []logCollision) int {
	if path == "" {
		stderr.discard()
		return exitOK
	}
	if err := checkCollisions(path, "--console-log", paths); err != nil {
		stderr.discard()
		return usageErr(err)
	}
	if err := stderr.attach(path); err != nil {
		stderr.discard()
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitOutput
	}
	return exitOK
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
	// Fold case on every platform, not only Windows. Whether two spellings
	// name one file is a property of the filesystem, not of the OS: macOS is
	// case-insensitive by default, Linux mounts exFAT, NTFS and SMB that way,
	// and Windows supports per-directory case sensitivity. No list of GOOS
	// values answers the question.
	//
	// os.SameFile above already settles it for files that exist, so this
	// governs only files about to be created -- the --force path. There the
	// two errors are not comparable. Refusing a legal pair costs a rename;
	// allowing an illegal one lets the second writer replace the first, which
	// on a case-insensitive filesystem left a file named RUN.JSONL holding
	// Parquet, the log unlinked, and an exit code of 0. So fold, and accept
	// rejecting a pair that a case-sensitive filesystem would have allowed.
	//
	// This does not cover Unicode normalisation, which APFS also folds:
	// "café.jsonl" and "cafe\u0301.jsonl" are one file there and compare
	// unequal here.
	return strings.EqualFold(canonicalPath(a), canonicalPath(b))
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
	outDir string, fileWorkers int, sel convert.InputSelection, skipUpToDate bool,
	logPath, catalogPath, consolePath string, logf convert.Logf) (code int) {

	found := convert.FindInputs(paths, sel)
	inputs, problems := found.Files, found.Problems
	if len(inputs) == 0 && len(problems) == 0 {
		// Saying only that nothing was found would read as an empty folder
		// when it was the patterns that emptied it.
		if found.Filtered > 0 {
			fmt.Fprintf(stderr, "%s: --include-files/--exclude-files %s left none of the %d .qvd file(s) found in %s\n",
				programName, sel.Patterns(), found.Filtered, strings.Join(paths, ", "))
		} else {
			fmt.Fprintf(stderr, "%s: no .qvd files found in %s\n",
				programName, strings.Join(paths, ", "))
		}
		return exitUsage
	}
	// A mistyped pattern otherwise looks exactly like a folder that held only
	// the files converted.
	for _, note := range found.Notes() {
		logf("note: %s", note)
	}

	// The output directory has to exist before the log, which commonly lives
	// inside it.
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "%s: create output directory %s: %v\n", programName, outDir, err)
		return exitOutput
	}

	// Every guard that can refuse the run outright goes here, before either
	// writer is created. Both the log and the catalog are written by
	// truncating, so a writer opened ahead of a guard that then refuses the
	// run destroys a file on the way out -- and the refusal it prints makes
	// that damage look impossible.
	//
	// That includes the guards which have nothing to do with either path.
	// RunMany rejects two inputs that would produce one output, but it does so
	// after the CLI has opened both writers, so the check has to happen here
	// as well. It is cheap and idempotent, and RunMany keeps its own copy for
	// callers that are not this one.
	if err := convert.CheckOutputCollisions(inputs, outDir); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitCodeFor(err)
	}
	if err := validateBatchCatalogPath(catalogPath, inputs, problems, outDir, opts); err != nil {
		return usageErr(err)
	}
	if err := checkCollisions(catalogPath, "--catalog-out", []logCollision{
		{"--log", logPath},
		{"the --skip-up-to-date manifest", manifestPathIf(skipUpToDate, outDir)},
	}); err != nil {
		return usageErr(err)
	}
	if logPath != "" {
		if skipUpToDate {
			// The manifest is the run's own record of what it produced;
			// pointing the log at it would destroy that record and leave
			// every later run reconverting the folder.
			if err := checkLogCollisions(logPath, []logCollision{
				{"the --skip-up-to-date manifest", convert.ManifestPath(outDir)},
			}); err != nil {
				return usageErr(err)
			}
		}
		if err := validateBatchLogPath(logPath, inputs, problems, outDir, opts); err != nil {
			return usageErr(err)
		}
	}
	if err := validateBatchWriterPath(consolePath, "--console-log", inputs, problems, outDir, opts); err != nil {
		return usageErr(err)
	}
	if code := startConsoleLog(consolePath, []logCollision{
		{"--log", logPath},
		{"--catalog-out", catalogPath},
		{"the --skip-up-to-date manifest", manifestPathIf(skipUpToDate, outDir)},
	}); code != exitOK {
		return code
	}
	defer stderr.Close()

	closeCatalog, err := openCatalog(catalogPath, opts, logf)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitCodeFor(err)
	}
	defer closeCatalogInto(&code, closeCatalog)

	var log *convert.LogWriter
	if logPath != "" {
		var err error
		if log, err = convert.NewLogWriter(logPath); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", programName, err)
			return exitOutput
		}
		defer log.Close()
	}

	result, err2 := convert.RunMany(ctx, inputs, opts, &convert.ManyOptions{
		OutDir:       outDir,
		FileWorkers:  fileWorkers,
		Recursive:    sel.Recursive,
		Log:          log,
		Problems:     problems,
		SkipUpToDate: skipUpToDate,
		ToolVersion:  version,
	}, logf)
	if err2 != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err2)
		return exitCodeFor(err2)
	}

	fmt.Fprintln(stderr, result.Summary())
	if logPath != "" {
		fmt.Fprintf(stderr, "%s: wrote %s\n", programName, logPath)
	}
	return result.ExitCode(exitCodeFor)
}

// manifestPathIf names the --skip-up-to-date manifest only when that flag is
// on, so the collision check has nothing to compare against otherwise.
func manifestPathIf(skipUpToDate bool, outDir string) string {
	if !skipUpToDate {
		return ""
	}
	return convert.ManifestPath(outDir)
}

// runCatalogScan builds a catalog from Parquet files that already exist,
// reading each file's footer rather than converting anything.
//
// It is the after-the-fact route: a conversion run without --catalog-out is
// not lost, because the comments it attached are in the files it wrote. What
// a scan cannot recover is the QVD-side profile -- the Qlik type, the symbol
// count, the resolver's note -- which never reached the Parquet. Every row it
// writes says source='parquet' so a query can tell the two apart.
func runCatalogScan(paths []string, catalogPath, consolePath string, recursive, force bool, logf convert.Logf) int {
	files, err := catalog.FindParquet(paths, recursive)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitInput
	}
	if len(files) == 0 {
		fmt.Fprintf(stderr, "%s: no .parquet files found in %s\n",
			programName, strings.Join(paths, ", "))
		return exitUsage
	}
	scanned := make([]logCollision, 0, len(files)+1)
	scanned = append(scanned, logCollision{"--catalog-out", catalogPath})
	for _, f := range files {
		if samePath(f, catalogPath) {
			return usageErr(fmt.Errorf("--catalog-out %s is one of the files being scanned", catalogPath))
		}
		scanned = append(scanned, logCollision{"the scanned file " + f, f})
	}
	if code := startConsoleLog(consolePath, scanned); code != exitOK {
		return code
	}
	defer stderr.Close()

	cat, err := catalog.NewWriter(catalogPath, version, force)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitCodeFor(err)
	}

	// A file that cannot be read is reported and the scan continues, so one
	// bad file in a folder of hundreds does not cost the whole catalog.
	failed := 0
	for _, f := range files {
		rows, err := catalog.ScanFile(f)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", programName, err)
			failed++
			continue
		}
		cat.Add(rows)
	}
	if err := cat.Close(); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitOutput
	}
	// A scan in which every file failed accounted for nothing, so Close wrote
	// no catalog and the path holds whatever it held before. Announcing one
	// anyway named a file that does not exist, which is the one thing a
	// message about an output must not do.
	if cat.Started() {
		logf("wrote catalog to %s: %d column(s) from %d file(s)",
			cat.Path(), cat.Len(), len(files)-failed)
	}
	if failed > 0 {
		logf("note: %d of %d file(s) could not be read", failed, len(files))
		if !cat.Started() {
			logf("no catalog written: %s is unchanged", cat.Path())
		}
		return exitInput
	}
	return exitOK
}

// runInspect reads the header and symbol tables only, then prints the schema a
// conversion would produce. The report is the command's result, so it goes to
// stdout; diagnostics stay on stderr.
func runInspect(ctx context.Context, inputPath string, opts *convert.Options,
	catalogPath, consolePath string, logf convert.Logf) (code int) {

	if err := validateCatalogPath(catalogPath, inputPath, "", opts); err != nil {
		return usageErr(err)
	}
	if err := validateWriterPath(consolePath, "--console-log", inputPath, "", opts); err != nil {
		return usageErr(err)
	}
	if code := startConsoleLog(consolePath, []logCollision{
		{"--catalog-out", catalogPath},
	}); code != exitOK {
		return code
	}
	defer stderr.Close()
	closeCatalog, err := openCatalog(catalogPath, opts, logf)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitCodeFor(err)
	}
	defer closeCatalogInto(&code, closeCatalog)

	rep, err := convert.Inspect(ctx, inputPath, opts)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitCodeFor(err)
	}
	defer rep.Close()

	if err := rep.Write(os.Stdout); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitOutput
	}
	if opts.Catalog != nil && rep.Schema != nil {
		// Inspect writes no Parquet, so the row names the file the conversion
		// would produce as empty rather than inventing one.
		opts.Catalog.Add(convert.CatalogRows(inputPath, "", rep.File, rep.Schema, opts))
	}
	if opts.SchemaReportPath != "" {
		if rep.Schema == nil {
			fmt.Fprintf(stderr, "%s: no schema to report: %v\n", programName, rep.SchemaErr)
			return exitSchema
		}
		if err := convert.WriteSchemaReport(opts.SchemaReportPath, inputPath, rep.File, rep.Schema, opts, rep.Encodings); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", programName, err)
			return exitOutput
		}
		fmt.Fprintf(stderr, "%s: wrote schema report to %s\n", programName, opts.SchemaReportPath)
	}
	// A file the type policy rejects exits non-zero, so scripts can gate on it.
	if rep.SchemaErr != nil {
		return exitCodeFor(rep.SchemaErr)
	}
	// So does a command line the conversion would refuse. Inspect is a
	// preflight check, and one that reports a problem and exits 0 is worse
	// than none: a script gating on it would go on to start the conversion
	// that is about to fail.
	if rep.EncodingErr != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, rep.EncodingErr)
		return exitCodeFor(rep.EncodingErr)
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
	fmt.Fprintf(stderr, "%s: %v\n", programName, err)
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
