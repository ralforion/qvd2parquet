package convert

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// InspectReport is the result of reading a QVD's header and symbol tables
// without touching the record area.
type InspectReport struct {
	Path           string
	TableName      string
	Rows           int64
	RecordByteSize int
	// RecordBytes is how much of the file the records occupy, which inspect
	// deliberately never reads.
	RecordBytes int64
	SymbolBytes int64
	SymbolCount int64
	FieldCount  int
	Excluded    []string
	// ExcludeNoMatch holds the --exclude patterns that matched no field.
	// Inspect is where a command line gets checked before a long conversion,
	// so a pattern that will drop nothing belongs here above all.
	ExcludeNoMatch []string
	// Renames records what --field-regex did to the selected fields. It is
	// held here rather than read from Schema because a file the type policy
	// rejects has no resolved schema, and a run that fails on one column is
	// exactly when the rest of the command line wants checking.
	Renames RenameSummary
	// Encodings reports the columns --encoding pins, and EncodingErr the
	// reason a pin cannot be applied. Inspect predicts the conversion, so a
	// pin that would fail the run has to fail here too.
	Encodings   *ResolvedEncodings
	EncodingErr error
	// Trials holds the encoding measurements --encoding auto asks for, best
	// saving first. Empty unless it was asked for.
	Trials  []EncodingTrial
	Elapsed time.Duration

	Schema *ResolvedSchema
	File   *qvd.File
	// Options is the configuration the report was produced under, so the
	// rendering can reflect the same rules the conversion would apply.
	Options *Options
	// SchemaErr is set when the type policy rejects the file. The rest of the
	// report is still filled in, so inspect can explain the failure instead of
	// just returning an error.
	SchemaErr error
}

// Inspect reads the header and the symbol tables of a QVD and resolves the
// schema that a conversion would produce, without decoding any records. On a
// large file this touches only a small prefix, so it is fast regardless of row
// count.
//
// A schema policy failure is reported in the result rather than returned, so
// the caller can still show the profiles that explain it.
func Inspect(inputPath string, opts *Options) (*InspectReport, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	start := time.Now()

	f, err := qvd.Open(inputPath)
	if err != nil {
		return nil, err
	}
	if err := f.SelectColumns(opts.Columns); err != nil {
		f.Close()
		return nil, err
	}
	excluded, unmatched, err := f.ExcludeColumns(opts.Exclude)
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := f.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		f.Close()
		return nil, err
	}

	rep := &InspectReport{
		Path:           inputPath,
		TableName:      f.Header.TableName,
		Rows:           f.NoOfRecords,
		RecordByteSize: f.RecordByteSize,
		RecordBytes:    f.NoOfRecords * int64(f.RecordByteSize),
		SymbolBytes:    f.RecordStart - f.HeaderEnd,
		FieldCount:     len(f.Columns),
		Excluded:       excluded,
		ExcludeNoMatch: unmatched,
		Renames:        SummarizeRenames(opts.Renamer, selectedNames(f)),
		File:           f,
		Options:        opts,
	}
	for _, p := range f.Profiles {
		if p != nil {
			rep.SymbolCount += p.Symbols
		}
	}

	var override *SchemaOverride
	if opts.SchemaOverridePath != "" {
		if override, err = LoadSchemaOverride(opts.SchemaOverridePath); err != nil {
			f.Close()
			return nil, err
		}
	}
	rep.Schema, rep.SchemaErr = ResolveSchema(f, opts, override)
	if rep.Schema != nil {
		rep.Encodings, rep.EncodingErr = ResolveEncodings(opts.Encodings.Rules, rep.Schema, f)
		if opts.Encodings.Auto && rep.EncodingErr == nil {
			// The one place inspect reads records. It is asked for
			// explicitly, so the promise that inspect touches only a prefix
			// of the file holds unless you want the measurement.
			rep.Trials, rep.EncodingErr = TrialEncodings(context.Background(), f, rep.Schema, opts)
		}
	}
	rep.Elapsed = time.Since(start)
	return rep, nil
}

// Close releases the inspected file.
func (r *InspectReport) Close() error {
	if r.File != nil {
		return r.File.Close()
	}
	return nil
}

// Write renders the report as a human-readable table.
func (r *InspectReport) Write(w io.Writer) error {
	fmt.Fprintf(w, "File            %s\n", r.Path)
	fmt.Fprintf(w, "Table           %s\n", r.TableName)
	fmt.Fprintf(w, "Rows            %s\n", withThousands(r.Rows))
	fmt.Fprintf(w, "Record size     %d %s\n", r.RecordByteSize, plural(int64(r.RecordByteSize), "byte"))
	fmt.Fprintf(w, "Symbols read    %s in %s (%s)\n",
		withThousands(r.SymbolCount), r.Elapsed.Round(time.Millisecond), humanBytes(r.SymbolBytes))
	fmt.Fprintf(w, "Records skipped %s not read\n", humanBytes(r.RecordBytes))

	selected := len(r.File.SelectedColumns())
	fmt.Fprintf(w, "Fields          %d of %d selected", selected, r.FieldCount)
	if len(r.Excluded) > 0 {
		fmt.Fprintf(w, " (%d excluded: %s)", len(r.Excluded), strings.Join(r.Excluded, ", "))
	}
	fmt.Fprintln(w)
	if len(r.ExcludeNoMatch) > 0 {
		fmt.Fprintf(w, "Exclude         %s matched no field\n", quotedList(r.ExcludeNoMatch))
	}
	if line := r.Renames.Line(maxNamedFields); line != "" {
		fmt.Fprintf(w, "Field regex     %s\n", line)
	}
	switch {
	case r.EncodingErr != nil:
		fmt.Fprintf(w, "Encoding        cannot be applied: %v\n", r.EncodingErr)
	case r.Encodings != nil:
		if len(r.Encodings.Pinned) > 0 {
			fmt.Fprintf(w, "Encoding        %s\n", strings.Join(r.Encodings.Pinned, ", "))
		}
		if len(r.Encodings.Unmatched) > 0 {
			fmt.Fprintf(w, "Encoding        %s matched no column\n", quotedList(r.Encodings.Unmatched))
		}
	}
	fmt.Fprintln(w)

	if r.SchemaErr != nil {
		fmt.Fprintf(w, "Schema could not be resolved:\n  %v\n\n", r.SchemaErr)
		return r.writeProfiles(w)
	}
	return r.writeSchema(w)
}

// writeSchema prints one row per output column.
func (r *InspectReport) writeSchema(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "COLUMN\tQVD TYPE\tSYMBOLS\tNULLS\tPARQUET TYPE\tRANGE\tNOTES")

	noteFor := r.notesBySource()
	for i := range r.Schema.Columns {
		c := &r.Schema.Columns[i]
		src := r.File.Columns[c.SourceIndex]
		prof := r.File.Profiles[c.SourceIndex]

		name := c.Name
		if c.OriginalName != "" && c.OriginalName != c.Name {
			name = fmt.Sprintf("%s (%s)", c.Name, c.OriginalName)
		}
		// Report the nulls the conversion will write, which includes empty
		// strings when --empty-as-null is on. Inspect exists to predict the
		// conversion, so it must not show a different number.
		var nulls string
		if prof != nil {
			effective := prof
			if r.Options != nil && r.Options.EmptyStringAsNull {
				effective = prof.WithEmptyStringsAsNulls()
			}
			nulls = withThousands(effective.Nulls)
		}
		var syms string
		if prof != nil {
			syms = withThousands(prof.Symbols)
		}
		note := noteFor[c.SourceIndex]
		if c.Comment != "" {
			note = c.Comment
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			name, src.QlikType, syms, nulls, c.ArrowType,
			ValueRange(c, prof, r.Options), note)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	var rounded int64
	for _, c := range r.Schema.Columns {
		rounded += c.DecimalRounded
	}
	if rounded > 0 {
		fmt.Fprintf(w, "\n%s decimal value(s) would be rounded to their column's scale.\n",
			withThousands(rounded))
	}

	// A decimal's precision is inferred from the values, so it always fits
	// them exactly and "does it fit" is never the question. The question is
	// what a later load has left, and that is what these columns are short of.
	var tight []string
	for i := range r.Schema.Columns {
		c := &r.Schema.Columns[i]
		if used := DecimalHeadroom(c, r.File.Profiles[c.SourceIndex]); used >= DecimalTightFraction {
			tight = append(tight, fmt.Sprintf("%s (%s, %.0f%% used)",
				c.Name, c.ArrowType, used*100))
		}
	}
	if len(tight) > 0 {
		fmt.Fprintf(w, "\nDecimal columns with little room left for larger values:\n  %s\n",
			strings.Join(tight, "\n  "))
		fmt.Fprintf(w, "Pin them with --schema if a later load may exceed the range.\n")
	}
	r.writeTrials(w)
	return nil
}

// writeTrials reports what the encoding measurement found. Columns that gain
// nothing are counted rather than named: the answer worth reading is the short
// list of columns where a different encoding is measurably smaller.
func (r *InspectReport) writeTrials(w io.Writer) {
	if len(r.Trials) == 0 {
		return
	}
	worthwhile := WorthwhileTrials(r.Trials)
	if len(worthwhile) == 0 {
		fmt.Fprintf(w, "\nMeasured %d column(s) against other encodings on %s sampled rows: "+
			"none is worth changing.\n", len(r.Trials), withThousands(r.Trials[0].SampledRows))
		return
	}
	fmt.Fprintf(w, "\nColumns that would compress better with a different encoding:\n")
	var rules []string
	for _, t := range worthwhile {
		fmt.Fprintf(w, "  %s\n", t.Line())
		rules = append(rules, fmt.Sprintf("%s=%s", t.Column, t.Encoding))
	}
	if rest := len(r.Trials) - len(worthwhile); rest > 0 {
		fmt.Fprintf(w, "%d other column(s) measured no better than the default.\n", rest)
	}
	fmt.Fprintf(w, "Apply with --encoding %q, or --encoding auto to measure every file.\n",
		strings.Join(rules, ","))
}

// writeProfiles prints raw symbol profiles, which is what is useful when the
// type policy rejected the file.
func (r *InspectReport) writeProfiles(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "COLUMN\tQVD TYPE\tSYMBOLS\tNULLS\tINTS\tFLOATS\tSTRINGS\tDUALS")
	for i := range r.File.Columns {
		c := &r.File.Columns[i]
		p := r.File.Profiles[i]
		if !c.Selected || p == nil {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Name, c.QlikType,
			withThousands(p.Symbols), withThousands(p.Nulls),
			withThousands(p.Ints), withThousands(p.Floats),
			withThousands(p.Strings), withThousands(p.DualInts+p.DualFloats))
	}
	return tw.Flush()
}

// notesBySource maps each source column to the note explaining its resolution.
func (r *InspectReport) notesBySource() map[int]string {
	out := map[int]string{}
	seen := map[int]bool{}
	n := 0
	for _, c := range r.Schema.Columns {
		if seen[c.SourceIndex] {
			continue
		}
		seen[c.SourceIndex] = true
		if n < len(r.Schema.Notes) {
			// The note starts with "Name: "; the table already has the name.
			note := r.Schema.Notes[n]
			if i := strings.Index(note, ": "); i >= 0 {
				note = note[i+2:]
			}
			out[c.SourceIndex] = note
			n++
		}
	}
	return out
}

// withThousands groups digits so large row counts stay readable.
func withThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var parts []string
	for len(s) > 3 {
		parts = append(parts, s[len(s)-3:])
		s = s[:len(s)-3]
	}
	parts = append(parts, s)
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	out := strings.Join(parts, ",")
	if neg {
		out = "-" + out
	}
	return out
}

// plural returns word or its simple plural, for counts embedded in a sentence.
func plural(n int64, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// humanBytes renders a byte count with a binary unit.
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
