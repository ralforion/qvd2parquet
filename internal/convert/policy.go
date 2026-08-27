// Package convert resolves a Parquet schema from profiled QVD symbols and
// converts QVD records into Arrow batches.
package convert

import (
	"fmt"
	"strings"
	"time"

	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// MixedStrategy selects how columns holding more than one logical type are
// resolved.
type MixedStrategy int

const (
	// MixedError fails the conversion on an ambiguous column. Default.
	MixedError MixedStrategy = iota
	// MixedString writes the whole column as UTF-8.
	MixedString
	// MixedPromote widens int+float to float64 and keeps pure text as string.
	MixedPromote
	// MixedDualColumns emits a separate ${name}__text column for duals.
	MixedDualColumns
)

// ParseMixedStrategy maps the --mixed flag value.
func ParseMixedStrategy(s string) (MixedStrategy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return MixedError, nil
	case "string":
		return MixedString, nil
	case "promote":
		return MixedPromote, nil
	case "dual-columns":
		return MixedDualColumns, nil
	}
	return 0, fmt.Errorf("invalid --mixed %q: want error|string|promote|dual-columns", s)
}

func (m MixedStrategy) String() string {
	return [...]string{"error", "string", "promote", "dual-columns"}[m]
}

// DualStrategy selects which side of a Qlik dual value is written.
type DualStrategy int

const (
	// DualAuto writes both sides only when the display strings carry
	// information the number does not. Default.
	DualAuto DualStrategy = iota
	// DualNumeric writes the numeric side.
	DualNumeric
	// DualText writes the display string.
	DualText
	// DualColumns writes both, in two Parquet columns.
	DualColumns
)

// ParseDualStrategy maps the --dual flag value.
func ParseDualStrategy(s string) (DualStrategy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto":
		return DualAuto, nil
	case "numeric":
		return DualNumeric, nil
	case "text":
		return DualText, nil
	case "columns":
		return DualColumns, nil
	}
	return 0, fmt.Errorf("invalid --dual %q: want auto|numeric|text|columns", s)
}

func (d DualStrategy) String() string {
	return [...]string{"auto", "numeric", "text", "columns"}[d]
}

// DecimalSource selects where exact decimal digits are taken from.
type DecimalSource int

const (
	// DecimalAuto prefers the dual display string and falls back to the
	// binary numeric payload. Default.
	DecimalAuto DecimalSource = iota
	// DecimalText requires a display string.
	DecimalText
	// DecimalNumeric always scales the binary numeric payload.
	DecimalNumeric
)

// ParseDecimalSource maps the --decimal-source flag value.
func ParseDecimalSource(s string) (DecimalSource, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto":
		return DecimalAuto, nil
	case "text":
		return DecimalText, nil
	case "numeric":
		return DecimalNumeric, nil
	}
	return 0, fmt.Errorf("invalid --decimal-source %q: want auto|text|numeric", s)
}

func (d DecimalSource) String() string {
	return [...]string{"auto", "text", "numeric"}[d]
}

// NumericPromote selects how a numeric column that is not already a declared
// decimal type is widened.
type NumericPromote int

const (
	// PromoteNone forbids widening; a column mixing integer and double
	// symbols is a policy error.
	PromoteNone NumericPromote = iota
	// PromoteFloat64 widens to float64.
	PromoteFloat64
	// PromoteDecimal prefers an exact decimal, inferring the smallest scale
	// at which every value is representable. Default.
	PromoteDecimal
)

// ParseNumericPromote maps the --numeric-promote flag value. The historical
// boolean spellings stay valid.
func ParseNumericPromote(s string) (NumericPromote, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "false", "0", "none", "off":
		return PromoteNone, nil
	case "true", "1", "float64", "on":
		return PromoteFloat64, nil
	case "decimal":
		return PromoteDecimal, nil
	}
	return 0, fmt.Errorf("invalid --numeric-promote %q: want true|false|decimal", s)
}

func (n NumericPromote) String() string {
	return [...]string{"false", "true", "decimal"}[n]
}

// Enabled reports whether any widening is permitted.
func (n NumericPromote) Enabled() bool { return n != PromoteNone }

// QualityMode selects how thoroughly the written Parquet file is validated.
type QualityMode int

const (
	// QualityNone skips post-write validation.
	QualityNone QualityMode = iota
	// QualityBasic validates row counts, schema and null counts.
	QualityBasic
	// QualityNumeric adds numeric/date/time aggregates.
	QualityNumeric
	// QualityFull adds order-independent value fingerprints. Default: a
	// conversion nobody checked is not a conversion anybody can trust, and
	// the fingerprints are what catch a value that survived the type policy
	// but not the round trip.
	QualityFull
)

// ParseQualityMode maps the --quality-gate flag value.
func ParseQualityMode(s string) (QualityMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none":
		return QualityNone, nil
	case "basic":
		return QualityBasic, nil
	case "numeric":
		return QualityNumeric, nil
	case "full":
		return QualityFull, nil
	}
	return 0, fmt.Errorf("invalid --quality-gate %q: want none|basic|numeric|full", s)
}

func (q QualityMode) String() string {
	return [...]string{"none", "basic", "numeric", "full"}[q]
}

// Options is the resolved conversion configuration.
type Options struct {
	Columns []string
	// Exclude holds shell-style wildcard patterns; a field whose original QVD
	// name matches one of them is not converted.
	Exclude []string
	// Renamer rewrites output column names and comments. Nil disables it.
	Renamer        *FieldRenamer
	Mixed          MixedStrategy
	Dual           DualStrategy
	NumericPromote NumericPromote
	// NumericPromoteExplicit records that the user asked for this promotion
	// mode rather than inheriting the default. An explicit request is a
	// demand: decimal promotion then honours DecimalStrict and fails when no
	// exact scale exists. The default is only a preference, so it falls back
	// to float64 instead of failing an otherwise-valid conversion.
	NumericPromoteExplicit bool
	// InferDates lets a column whose header declares no type be read as a
	// date or timestamp when every display string renders its Excel-style
	// serial value as one.
	InferDates bool
	// EmptyStringAsNull writes an empty string symbol as null, which is how
	// Qlik treats it. Disable to keep "" distinct from null.
	EmptyStringAsNull   bool
	MixedStringFallback bool
	DecimalSource       DecimalSource
	DecimalStrict       bool
	Compression         string
	// Encodings pins named columns to a Parquet encoding. Empty leaves every
	// column on the writer's default.
	Encodings []EncodingRule
	// BatchRows is how many rows one Arrow batch holds. 0 means automatic:
	// see EffectiveBatchRows.
	BatchRows int
	// RowGroupRows caps how many rows go into one Parquet row group. It is
	// independent of BatchRows -- the writer buffers batches until the row
	// group fills -- because the two size different things. BatchRows sizes
	// in-flight memory, RowGroupRows sizes the unit readers scan and
	// dictionaries are built over.
	RowGroupRows int
	Workers      int
	Location     *time.Location
	TimezoneName string
	// NaiveTimestamps writes timestamps with no timezone (Parquet
	// isAdjustedToUTC=false), preserving the QVD's wall clock verbatim.
	NaiveTimestamps     bool
	SchemaOverridePath  string
	SchemaReportPath    string
	Quality             QualityMode
	QualityReportPath   string
	QualityRelTolerance float64
	QualityAbsTolerance float64
	ProgressEvery       int64
	Force               bool
	Strict              bool
}

// DefaultOptions returns the documented defaults.
func DefaultOptions() Options {
	return Options{
		Mixed:               MixedError,
		Dual:                DualAuto,
		NumericPromote:      PromoteDecimal,
		InferDates:          true,
		EmptyStringAsNull:   true,
		DecimalSource:       DecimalAuto,
		DecimalStrict:       false,
		Compression:         "zstd",
		BatchRows:           0,
		RowGroupRows:        DefaultRowGroupRows,
		Workers:             0,
		Location:            time.UTC,
		TimezoneName:        "none",
		NaiveTimestamps:     true,
		Quality:             QualityFull,
		QualityRelTolerance: 1e-9,
		QualityAbsTolerance: 0,
		ProgressEvery:       1000000,
	}
}

// Batch sizing. A batch is held in memory per worker and again in the queue to
// the writer, so its cost is rows * columns * roughly 16 bytes, not rows. A
// fixed row count therefore means a narrow file holds a few MB per batch and a
// wide one holds hundreds, which is how a 200-column file ends up holding tens
// of GB across the workers. Sizing by cells instead keeps in-flight memory
// roughly constant whatever the shape of the file.
const (
	// TargetBatchCells is the cell budget one automatic batch aims for.
	TargetBatchCells = 2_000_000
	// MinAutoBatchRows keeps a very wide file from producing batches so small
	// that per-batch overhead dominates.
	MinAutoBatchRows = 4096
	// MaxAutoBatchRows caps a narrow file, where the cell budget would
	// otherwise allow millions of rows per batch.
	MaxAutoBatchRows = 65536
	// DefaultRowGroupRows is the default Parquet row group size. It matches
	// what row groups were before BatchRows and RowGroupRows were separated,
	// so the default output layout is unchanged.
	DefaultRowGroupRows = 65536
)

// EffectiveBatchRows resolves BatchRows for a file with the given number of
// output columns. An explicit value is returned unchanged; 0 means automatic.
func (o *Options) EffectiveBatchRows(columns int) int {
	if o.BatchRows > 0 {
		return o.BatchRows
	}
	if columns < 1 {
		columns = 1
	}
	n := TargetBatchCells / columns
	if n < MinAutoBatchRows {
		n = MinAutoBatchRows
	}
	if n > MaxAutoBatchRows {
		n = MaxAutoBatchRows
	}
	return n
}

// Validate checks the option combination.
func (o *Options) Validate() error {
	if o.BatchRows < 0 {
		return fmt.Errorf("--batch-rows must not be negative, got %d", o.BatchRows)
	}
	if o.RowGroupRows <= 0 {
		return fmt.Errorf("--row-group-rows must be positive, got %d", o.RowGroupRows)
	}
	if o.Workers < 0 {
		return fmt.Errorf("--workers must not be negative, got %d", o.Workers)
	}
	if o.QualityRelTolerance < 0 || o.QualityAbsTolerance < 0 {
		return fmt.Errorf("quality tolerances must not be negative")
	}
	if o.ProgressEvery < 0 {
		return fmt.Errorf("--progress must not be negative, got %d", o.ProgressEvery)
	}
	// TimezoneName is authoritative, so Location and NaiveTimestamps cannot
	// drift apart. Setting one of them alone used to change the conversion
	// without changing the other, which now that the default is naive would
	// mean a caller naming a zone still got a naive wall clock.
	if name := strings.TrimSpace(o.TimezoneName); name != "" {
		loc, naive, err := qvd.ParseLocation(name)
		if err != nil {
			return err
		}
		o.Location, o.NaiveTimestamps = loc, naive
	}
	if o.Location == nil {
		// Not time.Local: a partially filled Options must not pick up the
		// converting machine's zone by accident, which is the whole reason
		// the default is a naive wall clock.
		o.Location = time.UTC
	}
	// --mixed=dual-columns implies emitting both sides of a dual.
	if o.Mixed == MixedDualColumns && o.Dual != DualColumns {
		o.Dual = DualColumns
	}
	return nil
}
