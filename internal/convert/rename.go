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
