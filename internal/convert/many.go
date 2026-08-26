package convert

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ralforion/qvd2parquet/internal/parquetwrite"
)

// FileResult is the outcome of converting one input file.
type FileResult struct {
	Input   string
	Output  string
	Stats   *Stats
	Quality *QualityReport
	Err     error
	// Skipped is set when an existing output was left alone because --force
	// was not given.
	Skipped bool
	Started time.Time
	Elapsed time.Duration
}

// Failed reports whether the file did not convert.
func (r FileResult) Failed() bool { return r.Err != nil }

// BatchResult summarizes a whole run.
type BatchResult struct {
	Results   []FileResult
	Converted int
	Failed    int
	Skipped   int
	Rows      int64
	Bytes     int64
	Elapsed   time.Duration
}

// ExitCode is the worst outcome across the run, so a batch reports the most
// serious problem it hit rather than merely "something failed".
func (b *BatchResult) ExitCode(codeFor func(error) int) int {
	worst := 0
	for _, r := range b.Results {
		if r.Err == nil {
			continue
		}
		// Higher codes are more specific failures, not more severe ones, so
		// rank explicitly: a policy problem the user can fix outranks a
		// generic read error.
		if c := codeFor(r.Err); rank(c) > rank(worst) {
			worst = c
		}
	}
	return worst
}

// rank orders exit codes by how much they should dominate a batch summary.
func rank(code int) int {
	switch code {
	case 0:
		return 0
	case 7: // cancelled: the summary is incomplete, which outranks any
		// individual file's verdict, since the rest were never attempted
		return 7
	case 4: // input read/decode
		return 1
	case 5: // output/write
		return 2
	case 2: // unsupported QVD feature
		return 3
	case 6: // quality gate
		return 4
	case 3: // schema/type policy
		return 5
	default:
		return 6
	}
}

// InputProblem is a path that could not be examined. It is reported as a
// failed file rather than aborting the run, so one bad argument does not stop
// the inputs beside it from converting.
type InputProblem struct {
	Path string
	Err  error
}

// FindInputs expands the command line into a sorted list of .qvd files. A
// directory contributes the QVD files it contains, recursively when asked; any
// other path is taken literally, so an oddly named file can still be converted.
//
// A path that cannot be examined is returned as a problem, not an error: the
// batch guarantee is that every input is attempted, and aborting here would
// discard the valid inputs listed beside a mistyped one.
func FindInputs(paths []string, recursive bool) ([]string, []InputProblem) {
	seen := make(map[string]bool)
	var out []string
	var problems []InputProblem
	add := func(p string) {
		if abs, err := filepath.Abs(p); err == nil {
			if seen[abs] {
				return
			}
			seen[abs] = true
		}
		out = append(out, p)
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			problems = append(problems, InputProblem{Path: p, Err: fmt.Errorf("%w: %v", ErrInput, err)})
			continue
		}
		if !info.IsDir() {
			add(p)
			continue
		}
		if err := walkQVDs(p, recursive, add); err != nil {
			problems = append(problems, InputProblem{Path: p, Err: err})
		}
	}
	sort.Strings(out)
	sort.Slice(problems, func(i, j int) bool { return problems[i].Path < problems[j].Path })
	return out, problems
}

func walkQVDs(dir string, recursive bool, add func(string)) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInput, err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			if recursive {
				if err := walkQVDs(p, recursive, add); err != nil {
					return err
				}
			}
		case strings.EqualFold(filepath.Ext(e.Name()), ".qvd"):
			add(p)
		}
	}
	return nil
}

// OutputPathFor maps an input file onto its output under outDir, replacing the
// .qvd extension with .parquet.
func OutputPathFor(input, outDir string) string {
	base := filepath.Base(input)
	base = strings.TrimSuffix(base, filepath.Ext(base)) + ".parquet"
	return filepath.Join(outDir, base)
}

// ManyOptions configures a multi-file run.
type ManyOptions struct {
	// OutDir receives the converted files.
	OutDir string
	// FileWorkers is how many files convert at once. The decode workers are
	// divided between them, so the total stays near the automatic worker
	// count rather than multiplying it by the number of files in flight.
	FileWorkers int
	// Recursive descends into subdirectories when expanding a directory.
	Recursive bool
	// Log receives one structured record per file. May be nil.
	Log *LogWriter
	// Problems are inputs that could not even be examined. They are reported
	// as failures alongside the files that did convert.
	Problems []InputProblem
}

// RunMany converts every input, continuing past a failure so one bad file does
// not hide the state of the rest. The caller decides the exit code from the
// result.
func RunMany(ctx context.Context, inputs []string, opts *Options, many *ManyOptions, logf Logf) (*BatchResult, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if many.OutDir == "" {
		return nil, fmt.Errorf("no output directory given")
	}
	if err := os.MkdirAll(many.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: create output directory %s: %v", parquetwrite.ErrOutput, many.OutDir, err)
	}

	// Two inputs with the same base name would map to one output. The
	// per-file --force guard would report that as a pre-existing file, and
	// with --force it would silently overwrite, so catch it before converting
	// anything.
	if err := checkOutputCollisions(inputs, many.OutDir); err != nil {
		return nil, err
	}

	fileWorkers, perFile := splitWorkerBudget(many.FileWorkers, opts.Workers, len(inputs))
	if fileWorkers > 1 {
		logf("converting %d file(s), %d at a time, %d decode worker(s) each",
			len(inputs), fileWorkers, perFile)
	} else {
		logf("converting %d file(s)", len(inputs))
	}

	// Files convert on their own goroutines and each now reports its own
	// progress, so the writer needs serializing or two files interleave
	// mid-line.
	var logMu sync.Mutex
	safeLogf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logf(format, args...)
	}

	start := time.Now()
	results := make([]FileResult, len(inputs))
	sem := make(chan struct{}, fileWorkers)
	var wg sync.WaitGroup

	for i, in := range inputs {
		select {
		case <-ctx.Done():
			// Record the remaining files as cancelled rather than silently
			// dropping them from the summary. ErrCanceled rather than the
			// context's own error: a raw context.Canceled carries no domain
			// meaning, so it maps to the generic input-error exit code and
			// tells the user their files failed to read.
			for j := i; j < len(inputs); j++ {
				results[j] = FileResult{
					Input: inputs[j],
					Err:   fmt.Errorf("%w before this file was converted", ErrCanceled),
				}
			}
			i = len(inputs)
		default:
		}
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(i int, in string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = convertOne(ctx, in, opts, many, perFile, safeLogf)
		}(i, in)
	}
	wg.Wait()

	// An unreadable input is a failure of that input, not of the run.
	for _, p := range many.Problems {
		results = append(results, FileResult{Input: p.Path, Err: p.Err, Started: start})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Input < results[j].Input })

	b := &BatchResult{Results: results, Elapsed: time.Since(start)}
	for _, r := range results {
		switch {
		case r.Err != nil:
			b.Failed++
		case r.Skipped:
			b.Skipped++
		default:
			b.Converted++
			if r.Stats != nil {
				b.Rows += r.Stats.Rows
				b.Bytes += r.Stats.OutputBytes
			}
		}
		if many.Log != nil {
			many.Log.File(r)
		}
	}
	if many.Log != nil {
		many.Log.Summary(b)
	}
	return b, nil
}

// checkOutputCollisions rejects a run in which two inputs would produce the
// same output file.
func checkOutputCollisions(inputs []string, outDir string) error {
	byOutput := make(map[string][]string, len(inputs))
	for _, in := range inputs {
		out := OutputPathFor(in, outDir)
		byOutput[out] = append(byOutput[out], in)
	}
	var clashes []string
	for out, ins := range byOutput {
		if len(ins) > 1 {
			sort.Strings(ins)
			clashes = append(clashes, fmt.Sprintf("%s <- %s",
				DisplayPath(out), strings.Join(displayAll(ins), ", ")))
		}
	}
	if len(clashes) == 0 {
		return nil
	}
	sort.Strings(clashes)
	return fmt.Errorf("%w: %d output name collision(s); rename the inputs or convert the "+
		"directories separately:\n  %s",
		parquetwrite.ErrOutput, len(clashes), strings.Join(clashes, "\n  "))
}

func displayAll(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = DisplayPath(p)
	}
	return out
}

// convertOne runs a single file, capturing rather than propagating its error.
func convertOne(ctx context.Context, in string, opts *Options, many *ManyOptions, perFile int, logf Logf) FileResult {
	r := FileResult{Input: in, Output: OutputPathFor(in, many.OutDir), Started: time.Now()}

	// Each file gets its own copy, so a per-file report path cannot leak
	// between goroutines.
	o := *opts
	o.Workers = perFile
	o.SchemaReportPath = perFileReport(opts.SchemaReportPath, in, many.OutDir)
	o.QualityReportPath = perFileReport(opts.QualityReportPath, in, many.OutDir)

	// A batch run used to discard everything the conversion said, so a single
	// large file showed one line on starting and nothing again until it
	// finished, which on a twenty-million-row table is a quarter of an hour of
	// silence. Converting several at once, each line is prefixed with the file
	// it belongs to; converting one at a time, nothing can interleave and the
	// prefix would only be noise.
	fileLogf := logf
	if many.FileWorkers > 1 {
		name := DisplayPath(in)
		fileLogf = func(format string, args ...any) {
			// The name is passed as an argument rather than concatenated into
			// the format: a path may contain a percent sign.
			logf("%s: %s", name, fmt.Sprintf(format, args...))
		}
	}
	stats, quality, err := Run(ctx, in, r.Output, &o, fileLogf)
	r.Elapsed = time.Since(r.Started)
	r.Stats, r.Quality, r.Err = stats, quality, err

	switch {
	case err == nil:
		logf("ok   %s -> %s (%s rows, %d columns, %s)",
			DisplayPath(in), DisplayPath(r.Output),
			withThousands(stats.Rows), stats.Columns, humanBytes(stats.OutputBytes))
	default:
		logf("FAIL %s: %s", DisplayPath(in), trimPathPrefix(err.Error(), in))
	}
	return r
}

// perFileReport turns a single report path into a per-file one, so a batch does
// not have every file overwrite the same document.
func perFileReport(path, input, outDir string) string {
	if path == "" {
		return ""
	}
	base := filepath.Base(input)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(filepath.Base(path), ext)
	dir := filepath.Dir(path)
	if dir == "." {
		dir = outDir
	}
	return filepath.Join(dir, fmt.Sprintf("%s.%s%s", base, stem, ext))
}

// splitWorkerBudget divides the decode workers between concurrently converting
// files, so running several at once does not oversubscribe the machine the way
// separate processes would.
func splitWorkerBudget(fileWorkers, decodeWorkers, files int) (int, int) {
	if fileWorkers <= 0 {
		fileWorkers = 1
	}
	if files > 0 && fileWorkers > files {
		fileWorkers = files
	}
	budget := decodeWorkers
	if budget <= 0 {
		budget = DefaultWorkers()
	}
	perFile := budget / fileWorkers
	if perFile < 1 {
		perFile = 1
	}
	return fileWorkers, perFile
}

// DisplayPath shortens a path for human output: relative to the working
// directory when that is shorter, absolute otherwise. The log keeps the full
// path, so nothing is lost.
func DisplayPath(p string) string {
	wd, err := os.Getwd()
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(wd, p)
	if err != nil || strings.HasPrefix(rel, "..") || len(rel) >= len(p) {
		return p
	}
	return rel
}

// trimPathPrefix drops a leading "<path>: " from an error message, since the
// caller has already named the file and repeating it obscures the reason.
func trimPathPrefix(msg, path string) string {
	for _, p := range []string{path, DisplayPath(path)} {
		if strings.HasPrefix(msg, p+": ") {
			return msg[len(p)+2:]
		}
	}
	return msg
}

// Summary renders the human-readable end-of-run report.
func (b *BatchResult) Summary() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "converted %d/%d file(s) in %s",
		b.Converted, len(b.Results), b.Elapsed.Round(time.Millisecond))
	if b.Rows > 0 {
		fmt.Fprintf(&sb, ": %s rows, %s", withThousands(b.Rows), humanBytes(b.Bytes))
	}
	if b.Skipped > 0 {
		fmt.Fprintf(&sb, "; %d skipped", b.Skipped)
	}
	if b.Failed > 0 {
		fmt.Fprintf(&sb, "\n\nFAILED (%d):", b.Failed)
		for _, r := range b.Results {
			if r.Err != nil {
				fmt.Fprintf(&sb, "\n  %-28s %s",
					DisplayPath(r.Input), trimPathPrefix(r.Err.Error(), r.Input))
			}
		}
	}
	return sb.String()
}
