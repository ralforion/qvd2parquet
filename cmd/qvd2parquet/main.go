// Command qvd2parquet converts Qlik QVD files to Parquet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

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
)

// Program identity. version is overridden at build time with
// -ldflags "-X main.version=..."; scripts/build-release.sh does this.
const (
	programName = "qvd2parquet"
	copyright   = "(c) RALFORION d.o.o."
)

var version = "0.1.0"

// banner is the identification line printed at startup. It goes to stderr so
// it never contaminates piped output.
func banner() string {
	return fmt.Sprintf("%s %s  %s", programName, version, copyright)
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
		fmt.Fprintf(out, "Usage:\n  qvd2parquet [options] input.qvd output.parquet\n\n")
		fmt.Fprintf(out, "Options:\n")
		fs.PrintDefaults()
		fmt.Fprintf(out, "\nExit codes:\n"+
			"  0  success\n"+
			"  1  CLI usage error\n"+
			"  2  unsupported QVD feature\n"+
			"  3  schema/type policy error\n"+
			"  4  input read/decode error\n"+
			"  5  output/write error\n"+
			"  6  quality gate failure\n")
	}

	def := convert.DefaultOptions()
	var (
		columns       = fs.String("columns", "", "Convert only these comma-separated columns")
		exclude       = fs.String("exclude", "", "Skip fields matching these comma-separated wildcard patterns, e.g. '%*'")
		fieldRegex    = fs.String("field-regex", "", "Rewrite field names with this regexp; use (?P<name>...) and optional (?P<comment>...)")
		fieldName     = fs.String("field-name", "", "Template for the new column name (default \"${name}\")")
		fieldComment  = fs.String("field-comment", "", "Template for the column comment (default \"${comment}\")")
		mixed         = fs.String("mixed", def.Mixed.String(), "Mixed-type strategy: error|string|promote|dual-columns")
		dual          = fs.String("dual", def.Dual.String(), "Dual strategy: numeric|text|columns")
		promote       = fs.String("numeric-promote", def.NumericPromote.String(), "Numeric widening: decimal (exact, scale inferred from values) | true (float64) | false")
		strFallback   = fs.Bool("mixed-string-fallback", def.MixedStringFallback, "Convert otherwise-invalid mixed columns to string")
		decSource     = fs.String("decimal-source", def.DecimalSource.String(), "Decimal extraction: auto|text|numeric")
		decStrict     = fs.Bool("decimal-strict", def.DecimalStrict, "Fail instead of rounding when a decimal value does not fit its scale")
		compression   = fs.String("compression", def.Compression, "Parquet compression: zstd|snappy|gzip|uncompressed")
		batchRows     = fs.Int("batch-rows", def.BatchRows, "Rows per Arrow batch and Parquet row group")
		workers       = fs.Int("workers", def.Workers, "Decode workers, 0 means runtime.NumCPU()")
		timezone      = fs.String("timezone", def.TimezoneName, "Local|UTC|IANA timezone name for date/time conversion")
		schemaPath    = fs.String("schema", "", "Optional explicit schema override JSON")
		schemaReport  = fs.String("schema-report", "", "Write the inferred schema/profile report to this path")
		qualityGate   = fs.String("quality-gate", def.Quality.String(), "Validation mode: none|basic|numeric|full")
		qualityReport = fs.String("quality-report", "", "Write the post-conversion quality report to this path")
		qualityTol    = fs.Float64("quality-tolerance", def.QualityRelTolerance, "Relative tolerance for floating-point quality checks")
		qualityAbsTol = fs.Float64("quality-abs-tolerance", def.QualityAbsTolerance, "Absolute tolerance for floating-point quality checks")
		progress      = fs.Int64("progress", def.ProgressEvery, "Log every N rows, 0 disables progress")
		force         = fs.Bool("force", false, "Overwrite an existing output file")
		strict        = fs.Bool("strict", false, "Enable strict validation defaults")
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
	if fs.NArg() != 2 {
		fmt.Fprintf(os.Stderr, "%s: expected an input and an output path, got %d argument(s)\n\n",
			programName, fs.NArg())
		fs.Usage()
		return exitUsage
	}
	inputPath, outputPath := fs.Arg(0), fs.Arg(1)

	var err error
	opts := def
	opts.MixedStringFallback = *strFallback
	opts.DecimalStrict = *decStrict
	opts.Compression = *compression
	opts.BatchRows = *batchRows
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
	if opts.Location, err = qvd.ParseLocation(*timezone); err != nil {
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintln(os.Stderr, banner())

	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, programName+": "+format+"\n", args...)
	}

	stats, _, err := convert.Run(ctx, inputPath, outputPath, &opts, logf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", programName, err)
		return exitCodeFor(err)
	}

	logf("wrote %s: %d rows, %d columns, %s in %s (%.0f rows/s)",
		outputPath, stats.Rows, stats.Columns, humanBytes(stats.OutputBytes),
		stats.Elapsed.Round(1e6), stats.RowsPerSecond())
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
