package catalog

import "sort"

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
