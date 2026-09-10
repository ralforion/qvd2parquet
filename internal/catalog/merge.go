package catalog

import (
	"path/filepath"
	"sort"
)

// key identifies a column across runs: the QVD's own table name and the
// column as written.
//
// The table name rather than a path, because a catalog outlives the folder
// layout that produced it. Re-pointing --out-dir, or converting the same
// extract from a different directory, describes the same table and should
// update its rows rather than record a second copy of them.
type key struct {
	table  string
	column string
}

func keyOf(r Row) key { return key{table: r.SourceTable, column: r.ColumnName} }

// Merge folds the rows a run produced into the catalog already on disk.
//
// It is additive. A column the run describes replaces the stored row for it,
// so a changed type, comment or note is updated in place; a column the run did
// not describe is kept exactly as it was, including its run_at. Nothing is
// removed, so a catalog accumulates every table ever converted into it, and a
// run over one table does not erase the other two hundred.
//
// A column that has genuinely gone from a table therefore stays in the
// catalog, carrying the run_at of the last run that saw it. That is what makes
// it findable: a row older than the newest run for its table is a column the
// latest conversion no longer produced.
func Merge(stored, current []Row) []Row {
	current = reconcileScans(stored, current)

	fresh := make(map[key]bool, len(current))
	for _, r := range current {
		fresh[keyOf(r)] = true
	}
	out := make([]Row, 0, len(stored)+len(current))
	for _, r := range stored {
		if !fresh[keyOf(r)] {
			out = append(out, r)
		}
	}
	out = append(out, current...)
	sortRows(out)
	return out
}

// sortRows puts a merged catalog in a stable order: by table, then by the
// column's position in it. Without this a table's surviving rows would sit
// wherever the previous run left them and the run's own rows in a block at the
// end, which reads as two tables of the same name.
func sortRows(rows []Row) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.SourceTable != b.SourceTable {
			return a.SourceTable < b.SourceTable
		}
		if a.Ordinal != b.Ordinal {
			return a.Ordinal < b.Ordinal
		}
		return a.ColumnName < b.ColumnName
	})
}

// reconcileScans resolves rows read back out of a finished Parquet file
// against what the catalog already stores about that file.
//
// A scan sees a Parquet file and nothing else. It cannot know the table name
// the QVD header carried, so it uses the file's own name, and it cannot
// recover qlik_type, symbols, value_range, strategy or note, which never
// reached the Parquet. Merged as-is, both gaps cost something real:
//
//   - A QVD whose header names a different table than its file duplicates.
//     Converting orders.qvd whose header says HeaderOrders stores rows under
//     HeaderOrders; scanning orders.parquet into the same catalog would store
//     a second set under orders, one table arriving as two.
//
//   - --skip-up-to-date scans the outputs it did not reconvert, so a second
//     run of an unchanged folder would replace every row's QVD side with the
//     blanks a scan has for it. The data did not change; the record of it
//     would.
//
// Both are answered by output_file, which is the one thing a scan and the
// conversion that wrote the file agree on. Where the catalog already holds a
// converted row for the file a scan is describing, the scan adopts its table
// identity, and a column of that file adopts the QVD-side facts the scan
// cannot see. What the scan did observe -- the column's name, type,
// nullability, comment, ordinal and the file's row count -- is what the scan
// says, since that is the file as it stands now.
func reconcileScans(stored, current []Row) []Row {
	type ident struct{ table, sourceFile string }
	var (
		files   map[string]ident
		columns map[key]int
	)
	for i, r := range stored {
		if r.Source != SourceQVD || r.OutputFile == "" {
			continue
		}
		if files == nil {
			files, columns = map[string]ident{}, map[key]int{}
		}
		out := filepath.Clean(r.OutputFile)
		if r.SourceTable != "" {
			files[out] = ident{table: r.SourceTable, sourceFile: r.SourceFile}
		}
		columns[key{table: out, column: r.ColumnName}] = i
	}
	if files == nil {
		return current
	}

	out := make([]Row, len(current))
	copy(out, current)
	for i := range out {
		if out[i].Source != SourceParquet || out[i].OutputFile == "" {
			continue
		}
		path := filepath.Clean(out[i].OutputFile)
		id, known := files[path]
		if !known {
			continue
		}
		// The whole file belongs to the table that produced it, including a
		// column the conversion never wrote. That column has no QVD side to
		// adopt, so it stays a parquet row -- honestly empty rather than
		// claiming facts nothing recorded.
		out[i].SourceTable = id.table
		j, ok := columns[key{table: path, column: out[i].ColumnName}]
		if !ok {
			continue
		}
		s := stored[j]
		out[i].Source = s.Source
		out[i].SourceFile = s.SourceFile
		out[i].QlikType = s.QlikType
		out[i].Symbols, out[i].HasSymbols = s.Symbols, s.HasSymbols
		out[i].ValueRange = s.ValueRange
		out[i].Strategy = s.Strategy
		out[i].Note = s.Note
	}
	return out
}
