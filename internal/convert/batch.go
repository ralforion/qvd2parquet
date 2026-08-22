package convert

import (
	"fmt"
	"math"
	"math/big"
	"strconv"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// Value is one converted output cell, in the canonical form of its column's
// strategy. It is what both the Arrow builder and the quality metrics consume,
// so metrics always describe what was actually written.
type Value struct {
	Null   bool
	Str    string
	Int    int64
	Float  float64
	Scaled *big.Int
}

// columnConverter turns a resolved symbol into a Value for one output column.
// It is precomputed per column so the record loop performs no type switching
// on the column definition.
type columnConverter struct {
	col      *ResolvedColumn
	srcCol   qvd.Column
	convert  func(qvd.Symbol) (Value, error)
	appendTo func(array.Builder, Value)
}

// Batch holds the worker-local Arrow builders for one chunk.
type Batch struct {
	schema     *arrow.Schema
	builders   []array.Builder
	converters []columnConverter
	rec        *array.RecordBuilder
	rows       int
	capacity   int
}

// Converter precomputes everything the decode workers need.
type Converter struct {
	Schema  *ResolvedSchema
	File    *qvd.File
	Options *Options

	converters []columnConverter
}

// NewConverter builds the per-column conversion functions.
func NewConverter(f *qvd.File, rs *ResolvedSchema, opts *Options) (*Converter, error) {
	c := &Converter{Schema: rs, File: f, Options: opts}
	c.converters = make([]columnConverter, len(rs.Columns))
	for i := range rs.Columns {
		rc := &rs.Columns[i]
		cc, err := newColumnConverter(rc, f.Columns[rc.SourceIndex], opts)
		if err != nil {
			return nil, err
		}
		c.converters[i] = cc
	}
	return c, nil
}

// NewBatch creates a fresh set of builders sized for capacity rows.
func (c *Converter) NewBatch(mem memory.Allocator, capacity int) *Batch {
	rb := array.NewRecordBuilder(mem, c.Schema.Arrow)
	b := &Batch{
		schema:     c.Schema.Arrow,
		rec:        rb,
		builders:   rb.Fields(),
		converters: c.converters,
		capacity:   capacity,
	}
	for _, bld := range b.builders {
		bld.Reserve(capacity)
	}
	return b
}

// Append writes a converted value into the batch's builder for colIdx.
func (b *Batch) Append(colIdx int, v Value) {
	b.converters[colIdx].appendTo(b.builders[colIdx], v)
}

// EndRow records that a full row has been appended.
func (b *Batch) EndRow() { b.rows++ }

// Rows is the number of rows appended so far.
func (b *Batch) Rows() int { return b.rows }

// NewRecord materializes the accumulated rows. The caller owns the result and
// must Release it. The batch is reset and can be filled again.
func (b *Batch) NewRecord() arrow.Record {
	rec := b.rec.NewRecord()
	b.rows = 0
	for _, bld := range b.builders {
		bld.Reserve(b.capacity)
	}
	return rec
}

// Release frees the builders.
func (b *Batch) Release() { b.rec.Release() }

// newColumnConverter compiles the conversion and append functions for one
// output column.
func newColumnConverter(rc *ResolvedColumn, src qvd.Column, opts *Options) (columnConverter, error) {
	cc := columnConverter{col: rc, srcCol: src}
	loc := opts.Location
	// Qlik treats an empty string as absent, so by default it is written as
	// null rather than as a zero-length string.
	emptyIsNull := opts.EmptyStringAsNull

	switch rc.Strategy {
	case StrategyNull:
		cc.convert = func(qvd.Symbol) (Value, error) { return Value{Null: true}, nil }
		cc.appendTo = func(b array.Builder, _ Value) { b.AppendNull() }

	case StrategyString:
		cc.convert = func(s qvd.Symbol) (Value, error) {
			if s.Kind == qvd.SymbolNull {
				return Value{Null: true}, nil
			}
			str := symbolToString(s)
			if str == "" && emptyIsNull {
				return Value{Null: true}, nil
			}
			return Value{Str: str}, nil
		}
		cc.appendTo = appendString

	case StrategyDualText:
		cc.convert = func(s qvd.Symbol) (Value, error) {
			if s.Kind == qvd.SymbolNull || !s.Kind.HasText() {
				return Value{Null: true}, nil
			}
			if s.Text == "" && emptyIsNull {
				return Value{Null: true}, nil
			}
			return Value{Str: s.Text}, nil
		}
		cc.appendTo = appendString

	case StrategyInt64:
		cc.convert = func(s qvd.Symbol) (Value, error) {
			if s.Kind == qvd.SymbolNull {
				return Value{Null: true}, nil
			}
			switch s.Kind {
			case qvd.SymbolInt, qvd.SymbolDualIntString:
				return Value{Int: s.Int}, nil
			case qvd.SymbolFloat, qvd.SymbolDualFloatString:
				if isNonFinite(s.Float) {
					return Value{Null: true}, nil
				}
				if s.Float == math.Trunc(s.Float) && math.Abs(s.Float) <= math.MaxInt64 {
					return Value{Int: int64(s.Float)}, nil
				}
				return Value{}, fmt.Errorf("column %q: double %v does not fit an int64 column", rc.Name, s.Float)
			}
			return Value{}, fmt.Errorf("column %q: cannot write %v symbol %q as int64", rc.Name, s.Kind, s.Text)
		}
		cc.appendTo = func(b array.Builder, v Value) {
			bl := b.(*array.Int64Builder)
			if v.Null {
				bl.AppendNull()
			} else {
				bl.Append(v.Int)
			}
		}

	case StrategyFloat64:
		cc.convert = func(s qvd.Symbol) (Value, error) {
			if s.Kind == qvd.SymbolNull {
				return Value{Null: true}, nil
			}
			n, ok := s.Numeric()
			if !ok {
				return Value{}, fmt.Errorf("column %q: cannot write %v symbol %q as float64", rc.Name, s.Kind, s.Text)
			}
			return Value{Float: n}, nil
		}
		cc.appendTo = func(b array.Builder, v Value) {
			bl := b.(*array.Float64Builder)
			if v.Null {
				bl.AppendNull()
			} else {
				bl.Append(v.Float)
			}
		}

	case StrategyDate32:
		cc.convert = func(s qvd.Symbol) (Value, error) {
			if s.Kind == qvd.SymbolNull {
				return Value{Null: true}, nil
			}
			n, ok := s.Numeric()
			if !ok {
				return Value{}, fmt.Errorf("column %q: DATE symbol %q has no numeric value", rc.Name, s.Text)
			}
			if isNonFinite(n) {
				return Value{Null: true}, nil
			}
			d, ok := qvd.QlikDaysToDate32(n)
			if !ok {
				return Value{}, fmt.Errorf("column %q: serial day %v is out of range for date32", rc.Name, n)
			}
			return Value{Int: int64(d)}, nil
		}
		cc.appendTo = func(b array.Builder, v Value) {
			bl := b.(*array.Date32Builder)
			if v.Null {
				bl.AppendNull()
			} else {
				bl.Append(arrow.Date32(v.Int))
			}
		}

	case StrategyTimestampMicros:
		cc.convert = func(s qvd.Symbol) (Value, error) {
			if s.Kind == qvd.SymbolNull {
				return Value{Null: true}, nil
			}
			n, ok := s.Numeric()
			if !ok {
				return Value{}, fmt.Errorf("column %q: TIMESTAMP symbol %q has no numeric value", rc.Name, s.Text)
			}
			if isNonFinite(n) {
				return Value{Null: true}, nil
			}
			us, ok := qvd.QlikDaysToTimestampMicros(n, loc)
			if !ok {
				return Value{}, fmt.Errorf("column %q: serial timestamp %v is out of range", rc.Name, n)
			}
			return Value{Int: us}, nil
		}
		cc.appendTo = func(b array.Builder, v Value) {
			bl := b.(*array.TimestampBuilder)
			if v.Null {
				bl.AppendNull()
			} else {
				bl.Append(arrow.Timestamp(v.Int))
			}
		}

	case StrategyTimeMillis:
		cc.convert = func(s qvd.Symbol) (Value, error) {
			if s.Kind == qvd.SymbolNull {
				return Value{Null: true}, nil
			}
			n, ok := s.Numeric()
			if !ok {
				return Value{}, fmt.Errorf("column %q: TIME symbol %q has no numeric value", rc.Name, s.Text)
			}
			if isNonFinite(n) {
				return Value{Null: true}, nil
			}
			ms, ok := qvd.QlikFractionToTimeMillis(n)
			if !ok {
				return Value{}, fmt.Errorf("column %q: time value %v is out of range", rc.Name, n)
			}
			return Value{Int: int64(ms)}, nil
		}
		cc.appendTo = func(b array.Builder, v Value) {
			bl := b.(*array.Time32Builder)
			if v.Null {
				bl.AppendNull()
			} else {
				bl.Append(arrow.Time32(v.Int))
			}
		}

	case StrategyDecimal:
		// Decimal values were scaled once at schema time and are looked up by
		// symbol index, so this column converts through ConvertAt only.
		cc.convert = func(qvd.Symbol) (Value, error) {
			return Value{}, fmt.Errorf("column %q: decimal columns must be converted through ConvertAt", rc.Name)
		}
		cc.appendTo = func(b array.Builder, v Value) {
			bl := b.(*array.Decimal128Builder)
			if v.Null || v.Scaled == nil {
				bl.AppendNull()
			} else {
				bl.Append(decimal128.FromBigInt(v.Scaled))
			}
		}

	default:
		return cc, fmt.Errorf("column %q: unhandled strategy %v", rc.Name, rc.Strategy)
	}

	if emptyIsNull {
		// An empty string symbol is absent, whatever type the column resolved
		// to. Schema resolution reads the profile the same way, so a numeric
		// column may legitimately contain these placeholders.
		inner := cc.convert
		cc.convert = func(s qvd.Symbol) (Value, error) {
			if s.Kind == qvd.SymbolString && s.Text == "" {
				return Value{Null: true}, nil
			}
			return inner(s)
		}
	}
	return cc, nil
}

// ConvertAt resolves one output column's value from a symbol index, which
// decimal columns need because their scaled values are precomputed per symbol.
func (c *Converter) ConvertAt(colIdx int, symIdx int64, sym qvd.Symbol) (Value, error) {
	rc := &c.Schema.Columns[colIdx]
	if rc.Strategy == StrategyDecimal {
		if symIdx < 0 || sym.Kind == qvd.SymbolNull {
			return Value{Null: true}, nil
		}
		if symIdx >= int64(len(rc.Scaled)) {
			return Value{}, fmt.Errorf("column %q: symbol id %d out of range for %d precomputed decimals",
				rc.Name, symIdx, len(rc.Scaled))
		}
		v := rc.Scaled[symIdx]
		if v == nil {
			return Value{Null: true}, nil
		}
		return Value{Scaled: v}, nil
	}
	return c.converters[colIdx].convert(sym)
}

func appendString(b array.Builder, v Value) {
	bl := b.(*array.StringBuilder)
	if v.Null {
		bl.AppendNull()
	} else {
		bl.Append(v.Str)
	}
}

// symbolToString renders a symbol as deterministic, locale-independent text.
// The display string wins when present, because it is what Qlik shows.
func symbolToString(s qvd.Symbol) string {
	if s.Kind.HasText() {
		return s.Text
	}
	switch s.Kind {
	case qvd.SymbolInt:
		return strconv.FormatInt(s.Int, 10)
	case qvd.SymbolFloat:
		return formatFloat(s.Float)
	}
	return ""
}

// formatFloat renders a double with the shortest representation that round-trips.
func formatFloat(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
