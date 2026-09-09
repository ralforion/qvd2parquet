package convert

import (
	"context"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"

	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// discardSink decodes without keeping anything. The second pass exists to
// produce metrics, not a file.
type discardSink struct{}

func (discardSink) Write(arrow.Record) error { return nil }

// RereadSourceMetrics decodes the QVD a second time, from scratch, and returns
// the metrics of that read.
//
// Every other check in this package validates the written Parquet against what
// the conversion believed it read. None of them can question that belief: a
// record byte that was read wrong yields a different symbol index, and a
// different symbol index yields a value that is entirely well formed. There is
// no syntax to violate, nothing downstream to notice, and the Parquet will
// faithfully contain it. Only reading the source again can tell.
//
// So this opens the file again, re-reads the symbol tables, and decodes every
// record a second time with the schema the first pass resolved. Holding the
// schema fixed is deliberate: the question is whether the same bytes read the
// same way twice, not whether the schema would be resolved the same, and a
// difference in the answer is a difference in what was read.
func RereadSourceMetrics(ctx context.Context, inputPath string, rs *ResolvedSchema,
	opts *Options, progress ProgressFunc) (*Metrics, error) {

	f, err := qvd.Open(inputPath)
	if err != nil {
		return nil, fmt.Errorf("reread %s: %w", inputPath, err)
	}
	defer f.Close()

	if err := f.SelectColumns(opts.Columns); err != nil {
		return nil, fmt.Errorf("reread %s: %w", inputPath, err)
	}
	if _, _, err := f.ExcludeColumns(opts.Exclude); err != nil {
		return nil, fmt.Errorf("reread %s: %w", inputPath, err)
	}
	if err := f.ReadSymbols(qvd.UnknownSymbolError); err != nil {
		return nil, fmt.Errorf("reread %s: %w", inputPath, err)
	}

	conv, err := NewConverter(f, rs, opts)
	if err != nil {
		return nil, fmt.Errorf("reread %s: %w", inputPath, err)
	}
	return conv.Run(ctx, discardSink{}, progress)
}

// CompareSourceReads reports every way two passes over the same QVD disagree.
//
// The comparison is exact, with none of the tolerance the Parquet comparison
// needs. Both passes decoded the same bytes with the same schema through the
// same code, so every figure has to match to the digit; anything else means
// the file did not read the same way twice, and neither read can be trusted.
func CompareSourceReads(first, second *Metrics) []string {
	var errs []string
	if first.Rows != second.Rows {
		errs = append(errs, fmt.Sprintf("row count differs between two reads of the source: %d then %d",
			first.Rows, second.Rows))
	}
	if len(first.Columns) != len(second.Columns) {
		return append(errs, fmt.Sprintf("column count differs between two reads of the source: %d then %d",
			len(first.Columns), len(second.Columns)))
	}
	for i := range first.Columns {
		a, b := first.Columns[i].Stats(), second.Columns[i].Stats()
		name := first.Columns[i].Name
		for _, d := range []struct{ what, x, y string }{
			{"nulls", fmt.Sprint(a.Nulls), fmt.Sprint(b.Nulls)},
			{"non-nulls", fmt.Sprint(a.NonNulls), fmt.Sprint(b.NonNulls)},
			{"value fingerprint", a.Hash, b.Hash},
			{"sum", a.Sum, b.Sum},
			{"min", a.Min, b.Min},
			{"max", a.Max, b.Max},
		} {
			if d.x != d.y {
				errs = append(errs, fmt.Sprintf(
					"column %q: %s differs between two reads of the source: %s then %s",
					name, d.what, shortHash(d.x), shortHash(d.y)))
			}
		}
	}
	return errs
}
