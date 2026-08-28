package convert

import (
	"fmt"
	"regexp"
	"strings"
)

// FieldRenamer rewrites QVD field names, and optionally derives a per-column
// comment, by applying a regular expression to the original name.
//
// It exists because QVD field names are often composite. A field written as
//
//	A057-||-DATBI-||-Ende Gültigkeit
//
// carries the table, the technical name and a description in one string. With
//
//	--field-regex '^[^-]*-\|\|-(?P<name>[^-]*)-\|\|-(?P<comment>.*)$'
//
// the column is written as "DATBI" with "Ende Gültigkeit" attached as its
// Parquet field comment.
type FieldRenamer struct {
	Regex *regexp.Regexp
	// NameTemplate expands to the output column name. Defaults to "${name}".
	NameTemplate string
	// CommentTemplate expands to the column comment. Defaults to "${comment}".
	CommentTemplate string
}

// DefaultNameTemplate and DefaultCommentTemplate read the conventionally named
// capture groups, so the common case needs only --field-regex.
const (
	DefaultNameTemplate    = "${name}"
	DefaultCommentTemplate = "${comment}"
)

// NewFieldRenamer compiles the rename configuration. A blank expression
// disables renaming and returns a nil renamer.
func NewFieldRenamer(expr, nameTemplate, commentTemplate string) (*FieldRenamer, error) {
	if strings.TrimSpace(expr) == "" {
		if strings.TrimSpace(nameTemplate) != "" || strings.TrimSpace(commentTemplate) != "" {
			return nil, fmt.Errorf("--field-name and --field-comment need --field-regex")
		}
		return nil, nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid --field-regex %q: %w", expr, err)
	}
	r := &FieldRenamer{
		Regex:           re,
		NameTemplate:    orDefault(nameTemplate, DefaultNameTemplate),
		CommentTemplate: orDefault(commentTemplate, DefaultCommentTemplate),
	}
	// A regex with no usable group would silently blank every name; catching
	// that here is far clearer than an empty-column-name error later.
	if err := r.checkTemplates(); err != nil {
		return nil, err
	}
	return r, nil
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// checkTemplates rejects a name template that can never expand to anything.
func (r *FieldRenamer) checkTemplates() error {
	if r.NameTemplate == DefaultNameTemplate && !r.hasGroup("name") {
		return fmt.Errorf("--field-regex %q has no capture group named 'name'; "+
			"add one with (?P<name>...) or set --field-name to a template such as '$1'",
			r.Regex.String())
	}
	return nil
}

func (r *FieldRenamer) hasGroup(want string) bool {
	for _, n := range r.Regex.SubexpNames() {
		if n == want {
			return true
		}
	}
	return false
}

// Apply rewrites one field name. A name the expression does not match is left
// untouched, so a rename rule can target a subset of the fields. An expansion
// that yields an empty name is also treated as no match, rather than producing
// an unnamed column.
func (r *FieldRenamer) Apply(original string) (name, comment string) {
	if r == nil {
		return original, ""
	}
	m := r.Regex.FindStringSubmatchIndex(original)
	if m == nil {
		return original, ""
	}
	name = string(r.Regex.ExpandString(nil, r.NameTemplate, original, m))
	if strings.TrimSpace(name) == "" {
		name = original
	}
	if r.CommentTemplate != DefaultCommentTemplate || r.hasGroup("comment") {
		comment = strings.TrimSpace(string(r.Regex.ExpandString(nil, r.CommentTemplate, original, m)))
	}
	return name, comment
}

// RenameSummary records what a --field-regex did across the fields a run
// selected. A field the expression does not match keeps its original name,
// which is the right behaviour for a rule aimed at a subset, but on a wide
// SAP extract the handful of untouched fields is invisible among two hundred
// renamed ones.
type RenameSummary struct {
	// Fields is how many selected fields the expression was applied to.
	Fields int
	// Renamed is how many came out with a different name or gained a comment.
	Renamed int
	// Unchanged names the fields the expression did nothing for, in header
	// order.
	Unchanged []string
}

// SummarizeRenames applies r to each name and reports what changed. A nil
// renamer means no --field-regex was given, and the zero summary says so.
func SummarizeRenames(r *FieldRenamer, names []string) RenameSummary {
	if r == nil {
		return RenameSummary{}
	}
	s := RenameSummary{Fields: len(names)}
	for _, n := range names {
		// A field can match and still keep its name while gaining a comment,
		// which is a rename doing its job, so both halves count as changed.
		name, comment := r.Apply(n)
		if name != n || comment != "" {
			s.Renamed++
			continue
		}
		s.Unchanged = append(s.Unchanged, n)
	}
	return s
}

// Line renders the summary for a log or report, naming at most max unchanged
// fields so a file where the expression matched nothing does not print two
// hundred names. It returns "" when no renaming was configured.
func (s RenameSummary) Line(max int) string {
	if s.Fields == 0 {
		return ""
	}
	line := fmt.Sprintf("%d of %d field(s) renamed", s.Renamed, s.Fields)
	if len(s.Unchanged) == 0 {
		return line
	}
	shown := s.Unchanged
	suffix := ""
	if max > 0 && len(shown) > max {
		shown = shown[:max]
		suffix = fmt.Sprintf(" and %d more", len(s.Unchanged)-max)
	}
	return fmt.Sprintf("%s, %d unchanged: %s%s",
		line, len(s.Unchanged), strings.Join(shown, ", "), suffix)
}
