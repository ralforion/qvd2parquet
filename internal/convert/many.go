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
	"github.com/ralforion/qvd2parquet/internal/qvd"
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

// InputSelection narrows what a folder conversion picks up: whether to descend
// into subdirectories, and which file names to take. The name patterns apply
// to what a directory contributes, not to a file named on the command line,
// since a path you typed you meant.
type InputSelection struct {
	// Recursive descends into subdirectories when expanding a directory.
	Recursive bool
	// Include keeps only the files whose name matches one of these
	// shell-style wildcard patterns. Empty keeps everything the walk finds.
	Include []string
	// Exclude drops the files whose name matches, and wins over Include, so a
	// broad Include can be narrowed by a specific Exclude.
	Exclude []string
}

// Patterns lists the selection's name patterns as they were given, quoted, so
// a message about them can show a pattern the shell mangled on the way in.
func (s InputSelection) Patterns() string {
	return quotedList(append(append([]string{}, s.Include...), s.Exclude...))
}

// FoundInputs is what the command line expanded to.
type FoundInputs struct {
	// Files are the .qvd files to convert, sorted and deduplicated.
	Files []string
	// Problems are paths that could not be examined.
	Problems []InputProblem
	// Filtered counts the files a directory offered that Include or Exclude
	// dropped. Without it a mistyped pattern is indistinguishable from a
	// folder that really did hold only the files converted.
	Filtered int
	// Unmatched holds the name patterns that reached no file, in the order
	// Include then Exclude.
	Unmatched []string
}

// Notes describes how the selection went, for the caller to report. A pattern
// that matches nothing is reported rather than rejected, as --exclude and
// --encoding do, since one command line is often pointed at a folder of tables
// that do not all carry the same files.
func (f *FoundInputs) Notes() []string {
	var out []string
	if len(f.Unmatched) > 0 {
		out = append(out, fmt.Sprintf("--include-files/--exclude-files %s matched no file; "+
			"patterns are wildcards over the file name, with or without the .qvd extension",
			quotedList(f.Unmatched)))
	}
	if f.Filtered > 0 {
		out = append(out, fmt.Sprintf("%d file(s) dropped by --include-files/--exclude-files, %d left",
			f.Filtered, len(f.Files)))
	}
	return out
}

// FindInputs expands the command line into a sorted list of .qvd files. A
// directory contributes the QVD files it contains, recursively when asked and
// narrowed by the selection's name patterns; any other path is taken
// literally, so an oddly named file can still be converted.
//
// A path that cannot be examined is returned as a problem, not an error: the
// batch guarantee is that every input is attempted, and aborting here would
// discard the valid inputs listed beside a mistyped one.
func FindInputs(paths []string, sel InputSelection) FoundInputs {
	var found FoundInputs
	filter := newNameFilter(sel)
	seen := make(map[string]bool)
	add := func(p string) {
		if abs, err := filepath.Abs(p); err == nil {
			if seen[abs] {
				return
			}
			seen[abs] = true
		}
		found.Files = append(found.Files, p)
	}
	// Only what a directory offers is filtered; an expanded wildcard is the
	// user naming files, at one remove.
	addFromDir := func(p string) {
		if !filter.keep(filepath.Base(p)) {
			found.Filtered++
			return
		}
		add(p)
	}

	var take func(p string, fromWildcard bool)
	take = func(p string, fromWildcard bool) {
		info, err := os.Stat(p)
		if err != nil {
			// A wildcard the shell did not expand reaches us verbatim, which
			// is what happens on Windows: cmd.exe expands nothing, and
			// PowerShell does not expand for an external command. Without
			// this the pattern would be reported as a missing file.
			if !fromWildcard && hasWildcard(p) {
				matches, err := expandWildcard(p)
				if err != nil {
					found.Problems = append(found.Problems, InputProblem{Path: p, Err: err})
					return
				}
				for _, m := range matches {
					take(m, true)
				}
				return
			}
			found.Problems = append(found.Problems, InputProblem{Path: p, Err: fmt.Errorf("%w: %v", ErrInput, err)})
			return
		}
		if !info.IsDir() {
			add(p)
			return
		}
		if err := walkQVDs(p, sel.Recursive, addFromDir); err != nil {
			found.Problems = append(found.Problems, InputProblem{Path: p, Err: err})
		}
	}

	for _, p := range paths {
		take(p, false)
	}
	found.Unmatched = filter.unmatched()
	sort.Strings(found.Files)
	sort.Slice(found.Problems, func(i, j int) bool { return found.Problems[i].Path < found.Problems[j].Path })
	return found
}

// hasWildcard reports whether a path element carries a pattern rather than a
// name. Only the two wildcards the tool matches with count.
func hasWildcard(s string) bool { return strings.ContainsAny(s, "*?") }

// expandWildcard resolves a pattern in the last element of a path against the
// directory holding it, matching case-insensitively as every other pattern in
// the tool does. It takes .qvd files and directories, so "qvds/*" does not
// sweep up the .parquet files written beside them.
func expandWildcard(pattern string) ([]string, error) {
	dir, base := filepath.Split(pattern)
	if hasWildcard(dir) {
		return nil, fmt.Errorf("%w: %s: a wildcard is expanded only in the last element of a path",
			ErrInput, pattern)
	}
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInput, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && !strings.EqualFold(filepath.Ext(e.Name()), ".qvd") {
			continue
		}
		if qvd.MatchGlob(base, e.Name()) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s matched no .qvd file or directory", ErrInput, pattern)
	}
	return out, nil
}

// nameFilter applies the selection's patterns to a file name and remembers
// which of them ever matched, so one that reached nothing can be reported.
type nameFilter struct {
	include, exclude         []string
	usedInclude, usedExclude []bool
}

func newNameFilter(sel InputSelection) *nameFilter {
	return &nameFilter{
		include:     sel.Include,
		exclude:     sel.Exclude,
		usedInclude: make([]bool, len(sel.Include)),
		usedExclude: make([]bool, len(sel.Exclude)),
	}
}

// keep reports whether a file survives the patterns. Each is matched against
// the name with and without its extension, so "CE*", "CE*.qvd" and "*.qvd" all
// behave as they look, the way an encoding rule matches a column under both
// its names. Exclude and Include are both evaluated before either decides, so
// a pattern that only ever matched an excluded file still counts as used.
func (f *nameFilter) keep(name string) bool {
	if f == nil {
		return true
	}
	included := f.hit(f.include, f.usedInclude, name)
	excluded := f.hit(f.exclude, f.usedExclude, name)
	if excluded {
		return false
	}
	return len(f.include) == 0 || included
}

func (f *nameFilter) hit(patterns []string, used []bool, name string) bool {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	any := false
	for i, p := range patterns {
		if qvd.MatchGlob(p, name) || qvd.MatchGlob(p, stem) {
			used[i] = true
			any = true
		}
	}
	return any
}

func (f *nameFilter) unmatched() []string {
	var out []string
	for i, p := range f.include {
		if !f.usedInclude[i] {
			out = append(out, p)
		}
	}
	for i, p := range f.exclude {
		if !f.usedExclude[i] {
			out = append(out, p)
		}
	}
	return out
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
			results[i] = convertOne(ctx, in, opts, many, perFile, fileWorkers > 1, safeLogf)
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
// prefixProgress reports whether files convert concurrently, in which case
// every line has to name the file it belongs to. It is the effective
// concurrency after splitWorkerBudget has clamped it, not the requested
// --file-workers: asking for four and converting one file runs one at a time,
// and prefixing there would contradict the "converting 1 file(s)" above it.
func convertOne(ctx context.Context, in string, opts *Options, many *ManyOptions, perFile int, prefixProgress bool, logf Logf) FileResult {
	r := FileResult{Input: in, Output: OutputPathFor(in, many.OutDir), Started: time.Now()}

	// Each file gets its own copy, so a per-file report path cannot leak
	// between goroutines.
	o := *opts
	o.Workers = perFile
	o.SchemaReportPath = PerFileReportPath(opts.SchemaReportPath, in, many.OutDir)
	o.QualityReportPath = PerFileReportPath(opts.QualityReportPath, in, many.OutDir)

	// A batch run used to discard everything the conversion said, so a single
	// large file showed one line on starting and nothing again until it
	// finished, which on a twenty-million-row table is a quarter of an hour of
	// silence. Converting several at once, each line is prefixed with the file
	// it belongs to; converting one at a time, nothing can interleave and the
	// prefix would only be noise.
	fileLogf := logf
	if prefixProgress {
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

// PerFileReportPath turns a single report path into a per-file one, so a batch
// does not have every file overwrite the same document. It is exported because
// the command has to know every path a batch will write before it opens the
// log, so the log cannot be pointed at one of them.
func PerFileReportPath(path, input, outDir string) string {
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
