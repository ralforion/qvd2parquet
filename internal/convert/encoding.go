package convert

import (
	"fmt"
	"sort"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/ralforion/qvd2parquet/internal/parquetwrite"
	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// EncodingRule pins every column matching Pattern to an encoding. The pattern
// is a shell-style wildcard, matched against the output column name and
// against the original QVD field name, so one rule covers a folder of SAP
// tables whose keys are named per table: "%*_PKEY" reaches %CE10500_PKEY and
// %BSEG_PKEY alike.
type EncodingRule struct {
	Pattern  string
	Encoding parquetwrite.Encoding
}

// EncodingSpec is the parsed --encoding value: explicit rules, and whether to
// measure. The two compose, and an explicit rule wins over a measurement, so
// "auto,%*_PKEY=plain" measures every candidate column except that one.
type EncodingSpec struct {
	// Auto asks for the encoding to be measured per file rather than named.
	// A pattern cannot know what a table's key looks like, and over a folder
	// of tables it is the only form that can answer per file.
	Auto  bool
	Rules []EncodingRule
}

// AutoKeyword requests the measurement instead of naming an encoding.
const AutoKeyword = "auto"

// ParseEncodingSpec reads the --encoding value: a comma-separated list of
// PATTERN=ENCODING, the word "auto", or both.
func ParseEncodingSpec(spec string) (EncodingSpec, error) {
	var out EncodingSpec
	if strings.TrimSpace(spec) == "" {
		return out, nil
	}
	for _, part := range strings.Split(spec, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(part), AutoKeyword) {
			out.Auto = true
			continue
		}
		pattern, name, ok := strings.Cut(part, "=")
		pattern = strings.TrimSpace(pattern)
		if !ok || pattern == "" {
			return EncodingSpec{}, fmt.Errorf("invalid --encoding %q: want PATTERN=ENCODING "+
				"or %s, for example '%%*_PKEY=delta_byte_array'", part, AutoKeyword)
		}
		enc, err := parquetwrite.ParseEncoding(name)
		if err != nil {
			return EncodingSpec{}, fmt.Errorf("invalid --encoding %q: %w", part, err)
		}
		out.Rules = append(out.Rules, EncodingRule{Pattern: pattern, Encoding: enc})
	}
	return out, nil
}

// ResolvedEncodings is the outcome of applying the rules to a resolved schema.
type ResolvedEncodings struct {
	// ByColumn maps output column name to the encoding it is pinned to.
	ByColumn map[string]parquetwrite.Encoding
	// Pinned describes each pinned column as "NAME=encoding", in schema
	// order, for a log line.
	Pinned []string
	// Measured holds the trials a measurement adopted, so a run can report
	// what it chose and on what evidence.
	Measured []EncodingTrial
	// Unmatched holds patterns that reached no column. As with --exclude a
	// pattern matching nothing is reported rather than rejected, since one
	// command line covers a folder of tables with differing fields.
	Unmatched []string
}

// ResolveEncodings applies the rules to the schema. A later rule wins over an
// earlier one, so a broad pattern can be followed by a specific exception.
func ResolveEncodings(rules []EncodingRule, rs *ResolvedSchema, f *qvd.File) (*ResolvedEncodings, error) {
	out := &ResolvedEncodings{ByColumn: map[string]parquetwrite.Encoding{}}
	if len(rules) == 0 {
		return out, nil
	}
	used := make([]bool, len(rules))
	for i := range rs.Columns {
		c := &rs.Columns[i]
		original := ""
		if c.SourceIndex >= 0 && c.SourceIndex < len(f.Columns) {
			original = f.Columns[c.SourceIndex].Name
		}
		for ri, r := range rules {
			if !qvd.MatchGlob(r.Pattern, c.Name) && !qvd.MatchGlob(r.Pattern, original) {
				continue
			}
			used[ri] = true
			if err := checkEncodingFits(c, r.Encoding); err != nil {
				return nil, err
			}
			out.ByColumn[c.Name] = r.Encoding
		}
	}
	for i := range rs.Columns {
		if enc, ok := out.ByColumn[rs.Columns[i].Name]; ok {
			out.Pinned = append(out.Pinned, fmt.Sprintf("%s=%s", rs.Columns[i].Name, enc))
		}
	}
	for ri, r := range rules {
		if !used[ri] {
			out.Unmatched = append(out.Unmatched, r.Pattern)
		}
	}
	return out, nil
}

// checkEncodingFits rejects an encoding the column's type cannot carry. The
// writer would otherwise either ignore the request or fail deep inside the
// Parquet library, neither of which explains what to do about it.
func checkEncodingFits(c *ResolvedColumn, enc parquetwrite.Encoding) error {
	switch enc {
	case parquetwrite.EncodingDefault, parquetwrite.EncodingDictionary, parquetwrite.EncodingPlain:
		return nil
	case parquetwrite.EncodingDeltaByteArray, parquetwrite.EncodingDeltaLengthByteArray:
		if isByteArrayType(c.ArrowType) {
			return nil
		}
	case parquetwrite.EncodingDeltaBinaryPacked:
		if isIntegerBackedType(c.ArrowType) {
			return nil
		}
	}
	return fmt.Errorf("%w: --encoding %s does not fit column %q, which is written as %s; %s",
		ErrSchemaPolicy, enc, c.Name, c.ArrowType, encodingAdviceFor(c.ArrowType))
}

// isByteArrayType reports whether the column is stored as a Parquet byte
// array, which is what the delta string encodings need.
func isByteArrayType(t arrow.DataType) bool {
	switch t.ID() {
	case arrow.STRING, arrow.LARGE_STRING, arrow.BINARY, arrow.LARGE_BINARY:
		return true
	}
	return false
}

// isIntegerBackedType reports whether the column is stored as a Parquet
// integer, which delta binary packing needs. Dates and times qualify: they are
// integers with a logical type on top.
func isIntegerBackedType(t arrow.DataType) bool {
	switch t.ID() {
	case arrow.INT32, arrow.INT64, arrow.DATE32, arrow.TIME32, arrow.TIME64, arrow.TIMESTAMP:
		return true
	}
	return false
}

// encodingAdviceFor names the encodings that do fit a type.
func encodingAdviceFor(t arrow.DataType) string {
	fits := []string{string(parquetwrite.EncodingDictionary), string(parquetwrite.EncodingPlain)}
	switch {
	case isByteArrayType(t):
		fits = append(fits, string(parquetwrite.EncodingDeltaByteArray),
			string(parquetwrite.EncodingDeltaLengthByteArray))
	case isIntegerBackedType(t):
		fits = append(fits, string(parquetwrite.EncodingDeltaBinaryPacked))
	}
	sort.Strings(fits)
	return "it takes " + strings.Join(fits, ", ")
}
