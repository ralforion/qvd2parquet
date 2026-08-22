package convert

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// ErrSchemaPolicy marks a schema/type policy failure (CLI exit code 3).
var ErrSchemaPolicy = errors.New("schema/type policy error")

// ValueStrategy selects how a symbol is turned into an output value.
type ValueStrategy int

const (
	// StrategyNull emits nulls only; used for all-null columns.
	StrategyNull ValueStrategy = iota
	// StrategyString writes the display string, formatting numerics when needed.
	StrategyString
	// StrategyInt64 writes the integer numeric side.
	StrategyInt64
	// StrategyFloat64 writes the numeric side as a double.
	StrategyFloat64
	// StrategyDate32 writes days since the Unix epoch.
	StrategyDate32
	// StrategyTimestampMicros writes microseconds since the Unix epoch.
	StrategyTimestampMicros
	// StrategyTimeMillis writes milliseconds since midnight.
	StrategyTimeMillis
	// StrategyDecimal writes an exact scaled decimal.
	StrategyDecimal
	// StrategyDualText writes the display side of a dual into a companion column.
	StrategyDualText
)

func (s ValueStrategy) String() string {
	return [...]string{
		"StrategyNull", "StrategyString", "StrategyInt64", "StrategyFloat64",
		"StrategyDate32", "StrategyTimestampMicros", "StrategyTimeMillis",
		"StrategyDecimal", "StrategyDualText",
	}[s]
}

// IsNumericAggregatable reports whether numeric quality metrics apply.
func (s ValueStrategy) IsNumericAggregatable() bool {
	switch s {
	case StrategyInt64, StrategyFloat64, StrategyDate32, StrategyTimestampMicros,
		StrategyTimeMillis, StrategyDecimal:
		return true
	}
	return false
}

// ResolvedColumn is one output Parquet column.
type ResolvedColumn struct {
	// SourceIndex is the index of the QVD field this column reads from.
	SourceIndex int
	Name        string
	// OriginalName is the field name as stored in the QVD, before --field-regex
	// renaming. Error messages use it so they point at the source file.
	OriginalName string
	// Comment is attached as Parquet field metadata when non-empty.
	Comment   string
	ArrowType arrow.DataType
	Nullable  bool
	Strategy  ValueStrategy
	// Decimal is set when Strategy is StrategyDecimal.
	Decimal DecimalSpec
	// Scaled holds the pre-converted scaled decimal per symbol index, so record
	// decoding never re-parses a symbol. Only set for StrategyDecimal.
	Scaled []*big.Int
	// DecimalFromText and DecimalFromNumeric record where digits came from.
	DecimalFromText    bool
	DecimalFromNumeric bool
	// DecimalRounded counts values that did not fit the declared scale and
	// were rounded to it. Non-zero only when --decimal-strict is false.
	DecimalRounded int64
	// NonFiniteNulls counts symbols written as null because they are NaN or
	// infinite and so cannot be represented in this column's type.
	NonFiniteNulls int64
	// DecSep and ThouSep carry the source field's declared separators, so a
	// display string can be compared against the written value.
	DecSep  string
	ThouSep string
}

// ResolvedSchema is the full output schema plus the reasoning behind it.
type ResolvedSchema struct {
	Columns []ResolvedColumn
	Arrow   *arrow.Schema
	// Notes explains, per source column, how the type was chosen.
	Notes []string
}

// SchemaOverride is the --schema JSON document.
type SchemaOverride struct {
	Columns map[string]ColumnOverride `json:"columns"`
}

// ColumnOverride pins one column's output type.
type ColumnOverride struct {
	Type      string `json:"type"`
	Precision int32  `json:"precision"`
	Scale     int32  `json:"scale"`
}

// LoadSchemaOverride reads and validates a --schema document.
func LoadSchemaOverride(path string) (*SchemaOverride, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read schema override %s: %w", path, err)
	}
	var so SchemaOverride
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&so); err != nil {
		return nil, fmt.Errorf("parse schema override %s: %w", path, err)
	}
	for name, co := range so.Columns {
		switch strings.ToLower(co.Type) {
		case "string", "int64", "float64", "date32", "timestamp", "time", "decimal":
		default:
			return nil, fmt.Errorf("schema override for %q: unknown type %q "+
				"(want string|int64|float64|date32|timestamp|time|decimal)", name, co.Type)
		}
		if strings.EqualFold(co.Type, "decimal") {
			if co.Scale < 0 || co.Precision <= 0 || co.Precision > maxDecimal128Precision {
				return nil, fmt.Errorf("schema override for %q: decimal needs 0 < precision <= %d and scale >= 0, got precision=%d scale=%d",
					name, maxDecimal128Precision, co.Precision, co.Scale)
			}
			if co.Scale > co.Precision {
				return nil, fmt.Errorf("schema override for %q: scale %d exceeds precision %d", name, co.Scale, co.Precision)
			}
		}
	}
	return &so, nil
}

func (so *SchemaOverride) lookup(name string) (ColumnOverride, bool) {
	if so == nil {
		return ColumnOverride{}, false
	}
	if co, ok := so.Columns[name]; ok {
		return co, true
	}
	for k, co := range so.Columns {
		if strings.EqualFold(k, name) {
			return co, true
		}
	}
	return ColumnOverride{}, false
}

// requireConvertibleDateTime validates a date/time pin by running the same
// conversion the decode workers will run, on every symbol. Checking only the
// symbol kind would let NaN, an out-of-range serial day or an oversized
// timestamp through schema resolution and fail mid-conversion instead.
func requireConvertibleDateTime(col qvd.Column, syms []qvd.Symbol, pinned string,
	loc *time.Location, emptyAsNull bool) (int64, error) {
	// The type may come from the header, a Qlik tag, inference or --schema, so
	// state what was resolved rather than assuming an override.
	reject := func(i int, s qvd.Symbol, why string) error {
		return fmt.Errorf("%w: column %q resolves to %s, but symbol %d (%v %q) %s",
			ErrSchemaPolicy, col.Name, pinned, i, s.Kind, s.Text, why)
	}
	var nonFinite int64
	for i, s := range syms {
		if symbolIsAbsent(s, emptyAsNull) {
			continue
		}
		n, ok := s.Numeric()
		if !ok {
			return 0, reject(i, s, "carries no numeric value")
		}
		// NaN and infinity are not values that a date can hold, and nothing is
		// lost by writing them as null. A finite value that simply does not fit
		// is different: nulling it would discard real data, so that still
		// fails.
		if isNonFinite(n) {
			nonFinite++
			continue
		}
		switch pinned {
		case "date32":
			if _, ok := qvd.QlikDaysToDate32(n); !ok {
				return 0, reject(i, s, fmt.Sprintf("has serial day %v, which is out of range for date32", n))
			}
		case "timestamp":
			if _, ok := qvd.QlikDaysToTimestampMicros(n, loc); !ok {
				return 0, reject(i, s, fmt.Sprintf("has serial timestamp %v, which is out of range", n))
			}
		case "time":
			if _, ok := qvd.QlikFractionToTimeMillis(n); !ok {
				return 0, reject(i, s, fmt.Sprintf("has time value %v, which is out of range", n))
			}
		}
	}
	return nonFinite, nil
}

// symbolIsAbsent reports whether a symbol carries no value for conversion
// purposes. Every schema-time validator has to use this, or it will reject a
// placeholder the conversion is about to write as null.
func symbolIsAbsent(s qvd.Symbol, emptyAsNull bool) bool {
	if s.Kind == qvd.SymbolNull {
		return true
	}
	return emptyAsNull && s.Kind == qvd.SymbolString && s.Text == ""
}

// isNonFinite reports whether f is NaN or infinite, neither of which any of the
// typed output columns can represent.
func isNonFinite(f float64) bool { return math.IsNaN(f) || math.IsInf(f, 0) }

// Arrow type constructors used by the resolver.
var (
	arrowInt64  = arrow.PrimitiveTypes.Int64
	arrowF64    = arrow.PrimitiveTypes.Float64
	arrowString = arrow.BinaryTypes.String
	arrowDate32 = arrow.FixedWidthTypes.Date32
	arrowTime32 = arrow.FixedWidthTypes.Time32ms
)

// ResolveSchema turns profiled QVD columns into the output Parquet schema.
func ResolveSchema(f *qvd.File, opts *Options, override *SchemaOverride) (*ResolvedSchema, error) {
	rs := &ResolvedSchema{}
	tsType := &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: arrowTimeZoneName(opts)}

	for _, idx := range f.SelectedColumns() {
		col := f.Columns[idx]
		prof := f.Profiles[idx]
		syms := f.Symbols[idx]

		// With --empty-as-null an empty string is absent, so the type resolver
		// must read the profile the same way the conversion will.
		if opts.EmptyStringAsNull {
			prof = prof.WithEmptyStringsAsNulls()
		}
		cols, note, err := resolveColumn(col, prof, syms, opts, override, tsType)
		if err != nil {
			return nil, err
		}
		if len(cols) > 0 && cols[0].Name != col.Name {
			note += fmt.Sprintf("; written as %q", cols[0].Name)
			if cols[0].Comment != "" {
				note += fmt.Sprintf(" with comment %q", cols[0].Comment)
			}
		}
		rs.Columns = append(rs.Columns, cols...)
		rs.Notes = append(rs.Notes, note)
	}

	if err := checkNameCollisions(rs.Columns); err != nil {
		return nil, err
	}

	fields := make([]arrow.Field, len(rs.Columns))
	for i, c := range rs.Columns {
		f := arrow.Field{Name: c.Name, Type: c.ArrowType, Nullable: c.Nullable}
		md := map[string]string{}
		if c.Comment != "" {
			md["comment"] = c.Comment
		}
		if c.OriginalName != "" && c.OriginalName != c.Name {
			md["qvd.field"] = c.OriginalName
		}
		if len(md) > 0 {
			f.Metadata = arrow.MetadataFrom(md)
		}
		fields[i] = f
	}
	rs.Arrow = arrow.NewSchema(fields, nil)
	return rs, nil
}

// checkNameCollisions rejects duplicate output column names. A generated
// "${name}__text" companion column can collide with a real source column of
// that name, which would produce an ambiguous Parquet schema.
func checkNameCollisions(cols []ResolvedColumn) error {
	seen := make(map[string]int, len(cols))
	for i, c := range cols {
		if prev, ok := seen[c.Name]; ok {
			hint := ""
			if c.Strategy == StrategyDualText || cols[prev].Strategy == StrategyDualText {
				hint = "; the name is generated for a dual column's display side, so drop it with " +
					"--dual=numeric, rename the source column, or select a different set with --columns"
			}
			if cols[prev].OriginalName != c.Name || c.OriginalName != c.Name {
				hint += fmt.Sprintf("; source fields %q and %q",
					cols[prev].OriginalName, c.OriginalName)
			}
			return fmt.Errorf("%w: duplicate output column name %q (from source columns %d and %d)%s",
				ErrSchemaPolicy, c.Name, cols[prev].SourceIndex, c.SourceIndex, hint)
		}
		seen[c.Name] = i
	}
	return nil
}

// arrowTimeZoneName is the timezone stamped into the Arrow timestamp type.
//
// An empty name is not a missing value: it makes the column a naive wall clock
// (Parquet isAdjustedToUTC=false), which is what a QVD actually stores. Naming
// a zone instead asserts that the wall clocks were recorded in it, turning them
// into instants -- a claim the QVD itself never makes. Parquet cannot record
// the name either way; only Arrow metadata carries it.
func arrowTimeZoneName(opts *Options) string {
	if opts.NaiveTimestamps {
		return ""
	}
	if opts.Location == nil {
		return "UTC"
	}
	return opts.Location.String()
}

// timestampTypeLabel renders a timestamp type the way the schema notes and the
// --inspect output name it.
func timestampTypeLabel(t arrow.DataType) string {
	ts, ok := t.(*arrow.TimestampType)
	if !ok {
		return t.String()
	}
	if ts.TimeZone == "" {
		return "timestamp[us]"
	}
	return fmt.Sprintf("timestamp[us, tz=%s]", ts.TimeZone)
}

// resolveColumn decides the output column(s) for one QVD field. It returns one
// column normally, two when a dual is split.
func resolveColumn(col qvd.Column, prof *qvd.ColumnProfile, syms []qvd.Symbol,
	opts *Options, override *SchemaOverride, tsType arrow.DataType) ([]ResolvedColumn, string, error) {

	name, comment := opts.Renamer.Apply(col.Name)
	base := ResolvedColumn{
		SourceIndex:  col.Index,
		Name:         name,
		OriginalName: col.Name,
		Comment:      comment,
		Nullable:     true,
		DecSep:       col.DecSep,
		ThouSep:      col.ThouSep,
	}

	// An explicit override wins over inference, but is still validated against
	// the symbols actually present.
	if co, ok := override.lookup(col.Name); ok {
		rc, err := applyOverride(base, co, col, syms, tsType, opts.Location, opts.EmptyStringAsNull)
		if err != nil {
			return nil, "", err
		}
		return []ResolvedColumn{rc}, fmt.Sprintf("%s: pinned to %s by --schema", col.Name, rc.ArrowType), nil
	}

	if prof.HasOnlyNulls() {
		base.ArrowType, base.Strategy = arrowString, StrategyNull
		return []ResolvedColumn{base}, fmt.Sprintf("%s: all %d symbols are null, written as nullable utf8",
			col.Name, prof.Symbols), nil
	}

	// A column mixing plain strings with numerics is the dangerous case.
	if prof.HasMixedScalarFamilies() {
		switch {
		case opts.Mixed == MixedString || opts.MixedStringFallback:
			base.ArrowType, base.Strategy = arrowString, StrategyString
			return []ResolvedColumn{base}, fmt.Sprintf(
				"%s: mixed text and numeric symbols (%s), written as utf8", col.Name, prof.Describe()), nil
		default:
			return nil, "", fmt.Errorf("%w: mixed type column %q: symbols contain %d numeric values and %d strings; "+
				"use --mixed=string to write this column as UTF-8, or --mixed-string-fallback",
				ErrSchemaPolicy, col.Name, prof.Numeric(), prof.Strings)
		}
	}

	// Pure text.
	if prof.HasOnlyText() {
		base.ArrowType, base.Strategy = arrowString, StrategyString
		return []ResolvedColumn{base}, fmt.Sprintf("%s: %d text symbols, written as utf8", col.Name, prof.Strings), nil
	}

	// From here on the column is numeric or dual-numeric only.
	dual := prof.HasDuals()

	// --dual=auto is resolved after the numeric side, below, because whether
	// the display strings are redundant depends on the type they end up beside.
	effectiveDual := opts.Dual

	// --mixed=string converts every column to text on request.
	if opts.Mixed == MixedString {
		base.ArrowType, base.Strategy = arrowString, StrategyString
		return []ResolvedColumn{base}, fmt.Sprintf("%s: --mixed=string, written as utf8", col.Name), nil
	}

	// A dual whose text side is requested becomes a plain string column.
	if dual && effectiveDual == DualText {
		base.ArrowType, base.Strategy = arrowString, StrategyString
		return []ResolvedColumn{base}, fmt.Sprintf(
			"%s: dual column, --dual=text selects the display side (utf8)", col.Name), nil
	}

	numeric, note, err := resolveNumericColumn(base, col, prof, syms, opts, tsType)
	if err != nil {
		return nil, "", err
	}
	out := []ResolvedColumn{numeric}

	// --dual=auto keeps the display strings only when they carry something the
	// resolved numeric column does not. A localized number is redundant, and so
	// is a rendered date -- but only when the number is actually written as a
	// date type, since beside a bare float64 the reader has no way to know the
	// value is a date at all.
	dualNote := ""
	if effectiveDual == DualAuto {
		effectiveDual = DualNumeric
		if dual {
			// Name the column that will actually be generated: with
			// --field-regex the output name differs from the source field's.
			cl := ClassifyDual(col, syms, &numeric, opts.Location)
			dualNote = cl.Note(col.Name, numeric.Name+"__text")
			if cl.Kind == DualInformative {
				effectiveDual = DualColumns
			}
		}
	}

	if dual && effectiveDual == DualColumns {
		text := ResolvedColumn{
			SourceIndex:  col.Index,
			Name:         numeric.Name + "__text",
			OriginalName: col.Name,
			Comment:      numeric.Comment,
			ArrowType:    arrowString,
			Nullable:     true,
			Strategy:     StrategyDualText,
		}
		out = append(out, text)
		if dualNote != "" {
			note += "; " + dualNote
		} else {
			note += fmt.Sprintf("; display side written to %q", text.Name)
		}
	} else if dualNote != "" {
		note += "; " + dualNote
	}
	return out, note, nil
}

// resolveNumericColumn picks the typed representation for a numeric column.
func resolveNumericColumn(base ResolvedColumn, col qvd.Column, prof *qvd.ColumnProfile,
	syms []qvd.Symbol, opts *Options, tsType arrow.DataType) (ResolvedColumn, string, error) {

	qlikType := col.QlikType
	inferNote := ""
	// A QVD written by something other than QlikView often leaves
	// NumberFormat/Type empty. Without this, a date column would be written as
	// a bare Excel serial that no reader can interpret.
	if qlikType == qvd.QlikUnknown {
		// Qlik's own semantic tags are authoritative and, unlike the display
		// strings, are present on plain numeric fields that carry no duals.
		if tagged, ok := col.TaggedType(); ok {
			qlikType = tagged
			inferNote = fmt.Sprintf("no declared type, but tagged $%s", strings.ToLower(tagged.String()))
		} else if opts.InferDates {
			if inf, ok := InferDateTimeFromDuals(col, syms, opts.Location); ok {
				qlikType = inf.Type
				inferNote = inf.Note()
			}
		}
	}

	switch qlikType {
	case qvd.QlikDate:
		nonFinite, err := requireConvertibleDateTime(col, syms, "date32", opts.Location, opts.EmptyStringAsNull)
		if err != nil {
			return ResolvedColumn{}, "", err
		}
		base.NonFiniteNulls = nonFinite
		base.ArrowType, base.Strategy = arrowDate32, StrategyDate32
		return base, withNonFiniteNote(withInferNote(
			fmt.Sprintf("%s: DATE, written as date32 (days since epoch)", col.Name), inferNote), nonFinite), nil

	case qvd.QlikTimestamp:
		nonFinite, err := requireConvertibleDateTime(col, syms, "timestamp", opts.Location, opts.EmptyStringAsNull)
		if err != nil {
			return ResolvedColumn{}, "", err
		}
		base.NonFiniteNulls = nonFinite
		base.ArrowType, base.Strategy = tsType, StrategyTimestampMicros
		return base, withNonFiniteNote(withInferNote(fmt.Sprintf("%s: TIMESTAMP, written as %s",
			col.Name, timestampTypeLabel(tsType)), inferNote), nonFinite), nil

	case qvd.QlikTime:
		nonFinite, err := requireConvertibleDateTime(col, syms, "time", opts.Location, opts.EmptyStringAsNull)
		if err != nil {
			return ResolvedColumn{}, "", err
		}
		base.NonFiniteNulls = nonFinite
		base.ArrowType, base.Strategy = arrowTime32, StrategyTimeMillis
		return base, withNonFiniteNote(withInferNote(
			fmt.Sprintf("%s: TIME, written as time32[ms] (milliseconds since midnight)", col.Name), inferNote), nonFinite), nil

	case qvd.QlikFix, qvd.QlikMoney:
		return resolveDecimalColumn(base, col, syms, opts)
	}

	// --numeric-promote=decimal prefers an exact decimal over a binary double
	// for any column carrying fractional values. Pure-integer columns stay
	// int64, since decimal(p,0) would gain nothing. The declared MONEY/FIX
	// types are already handled above and are unaffected.
	if opts.NumericPromote == PromoteDecimal && prof.FloatLike() > 0 && prof.Strings == 0 {
		return resolvePromotedDecimalColumn(base, col, prof, syms, opts)
	}

	// INTEGER, REAL, ASCII-with-numbers and UNKNOWN fall through to the
	// profile-driven choice.
	switch {
	case prof.HasOnlyInts():
		base.ArrowType, base.Strategy = arrowInt64, StrategyInt64
		return base, fmt.Sprintf("%s: %s with %d integer symbols, written as int64",
			col.Name, col.QlikType, prof.IntLike()), nil

	case prof.HasOnlyFloats():
		base.ArrowType, base.Strategy = arrowF64, StrategyFloat64
		return base, fmt.Sprintf("%s: %s with %d double symbols, written as float64",
			col.Name, col.QlikType, prof.FloatLike()), nil

	case prof.CanPromoteIntToFloat():
		allowed := opts.NumericPromote.Enabled() &&
			(opts.Mixed == MixedPromote || opts.Mixed == MixedError || opts.Mixed == MixedDualColumns)
		if !allowed {
			return ResolvedColumn{}, "", fmt.Errorf(
				"%w: mixed type column %q: symbols contain %d integers and %d doubles; "+
					"use --numeric-promote=true to widen the column to float64, "+
					"--numeric-promote=decimal for an exact decimal, or --mixed=string",
				ErrSchemaPolicy, col.Name, prof.IntLike(), prof.FloatLike())
		}
		base.ArrowType, base.Strategy = arrowF64, StrategyFloat64
		return base, fmt.Sprintf("%s: %d integer and %d double symbols promoted to float64",
			col.Name, prof.IntLike(), prof.FloatLike()), nil
	}

	return ResolvedColumn{}, "", fmt.Errorf("%w: column %q: cannot resolve a type from %s",
		ErrSchemaPolicy, col.Name, prof.Describe())
}

// withInferNote appends the type-inference explanation when there is one.
func withInferNote(note, infer string) string {
	if infer == "" {
		return note
	}
	return note + "; " + infer
}

// withNonFiniteNote records values written as null because they are NaN or
// infinite, so the substitution is reported rather than silent.
func withNonFiniteNote(note string, n int64) string {
	if n == 0 {
		return note
	}
	return note + fmt.Sprintf("; %d non-finite value(s) written as null", n)
}

// resolvePromotedDecimalColumn resolves a REAL-ish column to an exact decimal
// under --numeric-promote=decimal. The declared NumberFormat/nDec is not
// trustworthy here (Qlik writes a filler value for REAL), so the scale is
// derived from the values themselves.
func resolvePromotedDecimalColumn(base ResolvedColumn, col qvd.Column, prof *qvd.ColumnProfile,
	syms []qvd.Symbol, opts *Options) (ResolvedColumn, string, error) {

	scale, ok := InferScaleFromValues(syms)
	if !ok {
		// No fixed scale represents these values, so the column really is
		// floating point. Falling back is only a failure when the user asked
		// for decimal explicitly; by default it is the correct answer, and
		// float64 is exactly what such a column would have resolved to anyway.
		//
		// This is deliberately independent of --decimal-strict, which governs
		// rounding a value that does not fit an established scale. Having no
		// inferable scale at all is a different question: the column is simply
		// not decimal-shaped.
		if !opts.NumericPromoteExplicit {
			base.ArrowType, base.Strategy = arrowF64, StrategyFloat64
			return base, fmt.Sprintf(
				"%s: %s carries values needing more than %d decimals, so no exact scale exists; "+
					"written as float64", col.Name, col.QlikType, MaxInferredDecimalScale), nil
		}
		return ResolvedColumn{}, "", fmt.Errorf(
			"%w: column %q: --numeric-promote=decimal cannot derive an exact scale within %d decimals "+
				"(the declared %s carries no usable NumberFormat/nDec); pin it with "+
				"--schema {\"columns\":{%q:{\"type\":\"decimal\",\"precision\":18,\"scale\":2}}}, "+
				"or use --numeric-promote=true to write it as float64",
			ErrSchemaPolicy, col.Name, MaxInferredDecimalScale, col.QlikType, col.Name)
	}

	ex := &DecimalExtractor{
		Scale:       scale,
		Source:      DecimalNumeric, // the numeric side is what was profiled
		Strict:      opts.DecimalStrict,
		DecSep:      col.DecSep,
		ThouSep:     col.ThouSep,
		EmptyAsNull: opts.EmptyStringAsNull,
	}
	spec, scaled, err := ResolveDecimalSpec(col.Name, syms, ex)
	if err != nil {
		return ResolvedColumn{}, "", fmt.Errorf("%w: %s", ErrSchemaPolicy, err)
	}

	base.ArrowType = &arrow.Decimal128Type{Precision: spec.Precision, Scale: spec.Scale}
	base.Strategy, base.Decimal, base.Scaled = StrategyDecimal, spec, scaled
	base.DecimalFromNumeric = true
	base.DecimalRounded = ex.Rounded
	base.NonFiniteNulls = ex.NonFinite

	mix := fmt.Sprintf("%d double symbols", prof.FloatLike())
	if prof.IntLike() > 0 {
		mix = fmt.Sprintf("%d integer and %d double symbols", prof.IntLike(), prof.FloatLike())
	}
	return base, fmt.Sprintf("%s: %s with %s promoted to %s; scale %d inferred from values",
		col.Name, col.QlikType, mix, spec, scale), nil
}

// resolveDecimalColumn resolves MONEY/FIX to an exact Parquet decimal.
func resolveDecimalColumn(base ResolvedColumn, col qvd.Column, syms []qvd.Symbol, opts *Options) (ResolvedColumn, string, error) {
	scale := int32(col.NDec)
	scaleSource := "NumberFormat/nDec"
	if col.NDec <= 0 {
		inferred, ok := InferScaleFromSymbols(syms, col.DecSep)
		if !ok {
			if col.NDec == 0 {
				// nDec is genuinely zero and no display strings contradict it.
				scale, scaleSource = 0, "NumberFormat/nDec (0)"
			} else {
				return ResolvedColumn{}, "", fmt.Errorf(
					"%w: column %q is %s but neither NumberFormat/nDec nor display strings provide a decimal scale; "+
						"pin it with --schema {\"columns\":{%q:{\"type\":\"decimal\",\"precision\":18,\"scale\":2}}}",
					ErrSchemaPolicy, col.Name, col.QlikType, col.Name)
			}
		} else {
			scale, scaleSource = inferred, "inferred from display strings"
		}
	}

	ex := &DecimalExtractor{
		Scale:       scale,
		Source:      opts.DecimalSource,
		Strict:      opts.DecimalStrict,
		DecSep:      col.DecSep,
		ThouSep:     col.ThouSep,
		EmptyAsNull: opts.EmptyStringAsNull,
	}
	spec, scaled, err := ResolveDecimalSpec(col.Name, syms, ex)
	if err != nil {
		return ResolvedColumn{}, "", fmt.Errorf("%w: %s", ErrSchemaPolicy, err)
	}

	base.ArrowType = &arrow.Decimal128Type{Precision: spec.Precision, Scale: spec.Scale}
	base.Strategy = StrategyDecimal
	base.Decimal = spec
	base.Scaled = scaled
	base.DecimalFromText = ex.UsedText
	base.DecimalFromNumeric = ex.UsedNumeric
	base.DecimalRounded = ex.Rounded
	base.NonFiniteNulls = ex.NonFinite

	src := "no values"
	switch {
	case ex.UsedText && ex.UsedNumeric:
		src = "dual display strings and numeric payloads"
	case ex.UsedText:
		src = "dual display strings"
	case ex.UsedNumeric:
		src = "numeric payloads"
	}
	note := fmt.Sprintf("%s: %s, written as %s; scale from %s, digits from %s",
		col.Name, col.QlikType, spec, scaleSource, src)
	if ex.NonFinite > 0 {
		note += fmt.Sprintf("; %d non-finite value(s) written as null", ex.NonFinite)
	}
	if ex.Rounded > 0 {
		note += fmt.Sprintf("; %d value(s) rounded to scale %d (--decimal-strict=false)",
			ex.Rounded, spec.Scale)
	}
	return base, note, nil
}

// applyOverride pins a column's type and validates it against the symbols, so
// an impossible pin fails as a schema policy error before anything is written
// rather than as a decode error mid-conversion.
func applyOverride(base ResolvedColumn, co ColumnOverride, col qvd.Column,
	syms []qvd.Symbol, tsType arrow.DataType, loc *time.Location, emptyAsNull bool) (ResolvedColumn, error) {
	switch strings.ToLower(co.Type) {
	case "string":
		base.ArrowType, base.Strategy = arrowString, StrategyString
	case "int64":
		for i, s := range syms {
			if symbolIsAbsent(s, emptyAsNull) {
				continue
			}
			if s.Kind == qvd.SymbolFloat || s.Kind == qvd.SymbolDualFloatString {
				return base, fmt.Errorf("%w: schema override pins %q to int64, but symbol %d is a double (%v)",
					ErrSchemaPolicy, col.Name, i, s.Float)
			}
			if s.Kind == qvd.SymbolString {
				return base, fmt.Errorf("%w: schema override pins %q to int64, but symbol %d is the string %q",
					ErrSchemaPolicy, col.Name, i, s.Text)
			}
		}
		base.ArrowType, base.Strategy = arrowInt64, StrategyInt64
	case "float64":
		for i, s := range syms {
			if symbolIsAbsent(s, emptyAsNull) {
				continue
			}
			if s.Kind == qvd.SymbolString {
				return base, fmt.Errorf("%w: schema override pins %q to float64, but symbol %d is the string %q",
					ErrSchemaPolicy, col.Name, i, s.Text)
			}
		}
		base.ArrowType, base.Strategy = arrowF64, StrategyFloat64
	case "date32":
		nonFinite, err := requireConvertibleDateTime(col, syms, "date32", loc, emptyAsNull)
		if err != nil {
			return base, err
		}
		base.NonFiniteNulls = nonFinite
		base.ArrowType, base.Strategy = arrowDate32, StrategyDate32
	case "timestamp":
		nonFinite, err := requireConvertibleDateTime(col, syms, "timestamp", loc, emptyAsNull)
		if err != nil {
			return base, err
		}
		base.NonFiniteNulls = nonFinite
		// Use the run's configured timezone, so the type metadata matches how
		// the values are actually converted.
		base.ArrowType, base.Strategy = tsType, StrategyTimestampMicros
	case "time":
		nonFinite, err := requireConvertibleDateTime(col, syms, "time", loc, emptyAsNull)
		if err != nil {
			return base, err
		}
		base.NonFiniteNulls = nonFinite
		base.ArrowType, base.Strategy = arrowTime32, StrategyTimeMillis
	case "decimal":
		ex := &DecimalExtractor{
			Scale:       co.Scale,
			Source:      DecimalAuto,
			Strict:      true,
			DecSep:      col.DecSep,
			ThouSep:     col.ThouSep,
			EmptyAsNull: emptyAsNull,
		}
		spec, scaled, err := ResolveDecimalSpec(col.Name, syms, ex)
		if err != nil {
			return base, fmt.Errorf("%w: %s", ErrSchemaPolicy, err)
		}
		if spec.Precision > co.Precision {
			return base, fmt.Errorf("%w: schema override pins %q to decimal(%d,%d), but the data needs precision %d",
				ErrSchemaPolicy, col.Name, co.Precision, co.Scale, spec.Precision)
		}
		spec.Precision = co.Precision
		base.ArrowType = &arrow.Decimal128Type{Precision: spec.Precision, Scale: spec.Scale}
		base.Strategy, base.Decimal, base.Scaled = StrategyDecimal, spec, scaled
		base.DecimalFromText, base.DecimalFromNumeric = ex.UsedText, ex.UsedNumeric
	}
	return base, nil
}
