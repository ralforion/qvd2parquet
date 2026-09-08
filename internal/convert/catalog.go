package convert

import (
	"github.com/ralforion/qvd2parquet/internal/catalog"
	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// CatalogRows describes one conversion's output columns for --catalog-out.
//
// It reads the same resolved schema the --schema-report does, so the two agree
// by construction rather than by being kept in step. The report is one JSON
// document per input and explains a decision in full; these rows are one table
// for the whole run and carry only what a downstream query would filter or
// join on.
func CatalogRows(inputPath, outputPath string, f *qvd.File, rs *ResolvedSchema, opts *Options) []catalog.Row {
	if f == nil || rs == nil {
		return nil
	}
	notes := notesBySourceColumn(rs)
	rows := make([]catalog.Row, 0, len(rs.Columns))
	for i := range rs.Columns {
		c := &rs.Columns[i]
		src := f.Columns[c.SourceIndex]
		rows = append(rows, catalog.Row{
			Source:       catalog.SourceQVD,
			SourceFile:   inputPath,
			SourceTable:  f.Header.TableName,
			SourceRows:   f.NoOfRecords,
			OutputFile:   outputPath,
			Ordinal:      int32(i + 1),
			ColumnName:   c.Name,
			SourceColumn: src.Name,
			Comment:      c.Comment,
			QlikType:     src.QlikType.String(),
			ParquetType:  c.ArrowType.String(),
			Nullable:     c.Nullable,
			Symbols:      src.SymbolCount,
			HasSymbols:   true,
			ValueRange:   ValueRange(c, f.Profiles[c.SourceIndex], opts),
			Strategy:     c.Strategy.String(),
			Note:         notes[c.SourceIndex],
		})
	}
	return rows
}

// notesBySourceColumn maps the resolver's per-source-column notes onto the
// source index they describe. A dual written as two output columns shares one
// note, which is why the mapping is not simply positional.
func notesBySourceColumn(rs *ResolvedSchema) map[int]string {
	notes := map[int]string{}
	ni := 0
	seen := map[int]bool{}
	for _, c := range rs.Columns {
		if !seen[c.SourceIndex] {
			seen[c.SourceIndex] = true
			if ni < len(rs.Notes) {
				notes[c.SourceIndex] = rs.Notes[ni]
				ni++
			}
		}
	}
	return notes
}
