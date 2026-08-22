package convert

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strconv"
)

// ErrQualityGate marks a failed post-write validation (CLI exit code 6).
var ErrQualityGate = errors.New("quality gate failure")

// fingerprint is an order-independent multiset digest: chunks can be merged in
// any order, so parallel decoding needs no reordering to validate.
type fingerprint struct {
	count int64
	xor   [32]byte
	sum   big.Int // sum of digests modulo 2^256
}

var mod256 = new(big.Int).Lsh(big.NewInt(1), 256)

func (fp *fingerprint) add(digest [32]byte) {
	fp.count++
	for i := range fp.xor {
		fp.xor[i] ^= digest[i]
	}
	var d big.Int
	d.SetBytes(digest[:])
	fp.sum.Add(&fp.sum, &d)
	fp.sum.Mod(&fp.sum, mod256)
}

func (fp *fingerprint) merge(o *fingerprint) {
	fp.count += o.count
	for i := range fp.xor {
		fp.xor[i] ^= o.xor[i]
	}
	fp.sum.Add(&fp.sum, &o.sum)
	fp.sum.Mod(&fp.sum, mod256)
}

func (fp *fingerprint) String() string {
	if fp.count == 0 {
		return ""
	}
	h := sha256.New()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(fp.count))
	h.Write(b[:])
	h.Write(fp.xor[:])
	h.Write(fp.sum.Bytes())
	return hex.EncodeToString(h.Sum(nil))
}

// ColumnMetrics accumulates order-independent per-column statistics over the
// values actually written.
type ColumnMetrics struct {
	Name     string
	Type     string
	Strategy ValueStrategy
	Decimal  DecimalSpec

	Rows     int64
	Nulls    int64
	NonNulls int64

	// Integer-family aggregates (int64, date32, timestamp[ms], time32[ms]).
	intSum        big.Int
	intMin        int64
	intMax        int64
	hasIntExtrema bool

	// Float aggregates.
	floatSum    float64
	floatSumSq  float64
	floatMin    float64
	floatMax    float64
	hasFloatExt bool

	// Decimal aggregates, exact via scaled integers.
	decSum        big.Int
	decMin        big.Int
	decMax        big.Int
	hasDecExtrema bool

	fp fingerprint
}

// NewColumnMetrics prepares metrics for one output column.
func NewColumnMetrics(rc *ResolvedColumn) *ColumnMetrics {
	return &ColumnMetrics{
		Name:     rc.Name,
		Type:     rc.ArrowType.String(),
		Strategy: rc.Strategy,
		Decimal:  rc.Decimal,
	}
}

// Observe folds one written value into the metrics. hashValues enables the
// order-independent fingerprint used by --quality-gate=full.
func (m *ColumnMetrics) Observe(v Value, hashValues bool) {
	m.Rows++
	if v.Null {
		m.Nulls++
	} else {
		m.NonNulls++
		switch m.Strategy {
		case StrategyInt64, StrategyDate32, StrategyTimestampMicros, StrategyTimeMillis:
			m.intSum.Add(&m.intSum, big.NewInt(v.Int))
			if !m.hasIntExtrema {
				m.intMin, m.intMax, m.hasIntExtrema = v.Int, v.Int, true
			} else {
				if v.Int < m.intMin {
					m.intMin = v.Int
				}
				if v.Int > m.intMax {
					m.intMax = v.Int
				}
			}
		case StrategyFloat64:
			m.floatSum += v.Float
			m.floatSumSq += v.Float * v.Float
			if !m.hasFloatExt {
				m.floatMin, m.floatMax, m.hasFloatExt = v.Float, v.Float, true
			} else {
				if v.Float < m.floatMin {
					m.floatMin = v.Float
				}
				if v.Float > m.floatMax {
					m.floatMax = v.Float
				}
			}
		case StrategyDecimal:
			m.decSum.Add(&m.decSum, v.Scaled)
			if !m.hasDecExtrema {
				m.decMin.Set(v.Scaled)
				m.decMax.Set(v.Scaled)
				m.hasDecExtrema = true
			} else {
				if v.Scaled.Cmp(&m.decMin) < 0 {
					m.decMin.Set(v.Scaled)
				}
				if v.Scaled.Cmp(&m.decMax) > 0 {
					m.decMax.Set(v.Scaled)
				}
			}
		}
	}
	if hashValues {
		m.fp.add(m.canonicalDigest(v))
	}
}

// canonicalDigest hashes the canonical binary form of one value. Nulls are
// marked explicitly so a null never collides with a zero or empty string.
func (m *ColumnMetrics) canonicalDigest(v Value) [32]byte {
	h := sha256.New()
	h.Write([]byte(m.Name))
	h.Write([]byte{0})
	h.Write([]byte(m.Type))
	h.Write([]byte{0})
	if v.Null {
		h.Write([]byte{0x00})
		var d [32]byte
		copy(d[:], h.Sum(nil))
		return d
	}
	h.Write([]byte{0x01})
	var buf [8]byte
	switch m.Strategy {
	case StrategyString, StrategyDualText, StrategyNull:
		binary.LittleEndian.PutUint64(buf[:], uint64(len(v.Str)))
		h.Write(buf[:])
		h.Write([]byte(v.Str))
	case StrategyInt64, StrategyDate32, StrategyTimestampMicros, StrategyTimeMillis:
		binary.LittleEndian.PutUint64(buf[:], uint64(v.Int))
		h.Write(buf[:])
	case StrategyFloat64:
		// Normalize -0 so it hashes like 0; NaN gets one canonical bit pattern.
		f := v.Float
		if f == 0 {
			f = 0
		}
		if math.IsNaN(f) {
			f = math.Float64frombits(0x7FF8000000000000)
		}
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(f))
		h.Write(buf[:])
	case StrategyDecimal:
		binary.LittleEndian.PutUint32(buf[:4], uint32(m.Decimal.Precision))
		binary.LittleEndian.PutUint32(buf[4:], uint32(m.Decimal.Scale))
		h.Write(buf[:])
		h.Write([]byte(v.Scaled.String()))
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d
}

// Merge folds another chunk's metrics for the same column into m.
func (m *ColumnMetrics) Merge(o *ColumnMetrics) {
	m.Rows += o.Rows
	m.Nulls += o.Nulls
	m.NonNulls += o.NonNulls
	m.intSum.Add(&m.intSum, &o.intSum)
	if o.hasIntExtrema {
		if !m.hasIntExtrema {
			m.intMin, m.intMax, m.hasIntExtrema = o.intMin, o.intMax, true
		} else {
			if o.intMin < m.intMin {
				m.intMin = o.intMin
			}
			if o.intMax > m.intMax {
				m.intMax = o.intMax
			}
		}
	}
	m.floatSum += o.floatSum
	m.floatSumSq += o.floatSumSq
	if o.hasFloatExt {
		if !m.hasFloatExt {
			m.floatMin, m.floatMax, m.hasFloatExt = o.floatMin, o.floatMax, true
		} else {
			if o.floatMin < m.floatMin {
				m.floatMin = o.floatMin
			}
			if o.floatMax > m.floatMax {
				m.floatMax = o.floatMax
			}
		}
	}
	m.decSum.Add(&m.decSum, &o.decSum)
	if o.hasDecExtrema {
		if !m.hasDecExtrema {
			m.decMin.Set(&o.decMin)
			m.decMax.Set(&o.decMax)
			m.hasDecExtrema = true
		} else {
			if o.decMin.Cmp(&m.decMin) < 0 {
				m.decMin.Set(&o.decMin)
			}
			if o.decMax.Cmp(&m.decMax) > 0 {
				m.decMax.Set(&o.decMax)
			}
		}
	}
	m.fp.merge(&o.fp)
}

// ColumnStats is the JSON-facing view of a column's metrics.
type ColumnStats struct {
	Nulls    int64  `json:"nulls"`
	NonNulls int64  `json:"nonNulls"`
	Sum      string `json:"sum,omitempty"`
	Min      string `json:"min,omitempty"`
	Max      string `json:"max,omitempty"`
	SumSq    string `json:"sumSquares,omitempty"`
	Hash     string `json:"hash,omitempty"`

	// Raw float values kept for tolerance comparison; not serialized.
	sumF   float64
	sumSqF float64
	minF   float64
	maxF   float64
}

// Stats renders the metrics for reporting and comparison.
func (m *ColumnMetrics) Stats() ColumnStats {
	s := ColumnStats{Nulls: m.Nulls, NonNulls: m.NonNulls, Hash: m.fp.String()}
	switch m.Strategy {
	case StrategyInt64, StrategyDate32, StrategyTimestampMicros, StrategyTimeMillis:
		s.Sum = m.intSum.String()
		if m.hasIntExtrema {
			s.Min = strconv.FormatInt(m.intMin, 10)
			s.Max = strconv.FormatInt(m.intMax, 10)
		}
	case StrategyFloat64:
		s.Sum = strconv.FormatFloat(m.floatSum, 'g', -1, 64)
		s.SumSq = strconv.FormatFloat(m.floatSumSq, 'g', -1, 64)
		s.sumF, s.sumSqF = m.floatSum, m.floatSumSq
		if m.hasFloatExt {
			s.Min = strconv.FormatFloat(m.floatMin, 'g', -1, 64)
			s.Max = strconv.FormatFloat(m.floatMax, 'g', -1, 64)
			s.minF, s.maxF = m.floatMin, m.floatMax
		}
	case StrategyDecimal:
		s.Sum = FormatScaled(&m.decSum, m.Decimal.Scale)
		if m.hasDecExtrema {
			s.Min = FormatScaled(&m.decMin, m.Decimal.Scale)
			s.Max = FormatScaled(&m.decMax, m.Decimal.Scale)
		}
	}
	return s
}

// Metrics is the whole-file metric set.
type Metrics struct {
	Rows    int64
	Columns []*ColumnMetrics
}

// NewMetrics prepares metrics for every output column.
func NewMetrics(rs *ResolvedSchema) *Metrics {
	m := &Metrics{Columns: make([]*ColumnMetrics, len(rs.Columns))}
	for i := range rs.Columns {
		m.Columns[i] = NewColumnMetrics(&rs.Columns[i])
	}
	return m
}

// Merge folds another chunk's metrics into m.
func (m *Metrics) Merge(o *Metrics) {
	m.Rows += o.Rows
	for i := range m.Columns {
		m.Columns[i].Merge(o.Columns[i])
	}
}

// ColumnComparison is one column's entry in the quality report.
type ColumnComparison struct {
	Name    string      `json:"name"`
	Type    string      `json:"type"`
	Source  ColumnStats `json:"source"`
	Parquet ColumnStats `json:"parquet"`
	Passed  bool        `json:"passed"`
	Errors  []string    `json:"errors,omitempty"`
}

// QualityReport is the --quality-report document.
type QualityReport struct {
	Input       string             `json:"input"`
	Output      string             `json:"output"`
	Mode        string             `json:"mode"`
	Passed      bool               `json:"passed"`
	RowsSource  int64              `json:"rowsSource"`
	RowsParquet int64              `json:"rowsParquet"`
	Columns     []ColumnComparison `json:"columns,omitempty"`
	Errors      []string           `json:"errors,omitempty"`
}

// Write saves the report as indented JSON.
func (r *QualityReport) Write(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode quality report: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write quality report %s: %w", path, err)
	}
	return nil
}

// Err returns a quality gate error if the report did not pass.
func (r *QualityReport) Err() error {
	if r.Passed {
		return nil
	}
	msgs := append([]string{}, r.Errors...)
	for _, c := range r.Columns {
		for _, e := range c.Errors {
			msgs = append(msgs, fmt.Sprintf("column %q: %s", c.Name, e))
		}
	}
	if len(msgs) == 0 {
		msgs = []string{"validation failed"}
	}
	limit := len(msgs)
	if limit > 10 {
		limit = 10
	}
	extra := ""
	if len(msgs) > limit {
		extra = fmt.Sprintf(" (and %d more)", len(msgs)-limit)
	}
	return fmt.Errorf("%w: %s%s", ErrQualityGate, joinLines(msgs[:limit]), extra)
}

func joinLines(msgs []string) string {
	out := ""
	for i, m := range msgs {
		if i > 0 {
			out += "; "
		}
		out += m
	}
	return out
}

// floatsClose applies the documented relative/absolute tolerance rule.
func floatsClose(a, b, relTol, absTol float64) bool {
	if a == b {
		return true
	}
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	d := math.Abs(a - b)
	if d <= absTol {
		return true
	}
	scale := math.Max(math.Max(math.Abs(a), math.Abs(b)), 1)
	return d <= relTol*scale
}
