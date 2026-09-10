package catalog

import (
	"path/filepath"
	"sort"
	"time"
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
	// One canonical form per distinct path, so a catalog of forty thousand
	// rows does not pay for the same resolution forty thousand times.
	canon := map[string]string{}
	pathOf := func(p string) string {
		if c, ok := canon[p]; ok {
			return c
		}
		c := canonicalOutput(p)
		canon[p] = c
		return c
	}

	// An output path can have belonged to more than one table over the life of
	// a catalog, because nothing is ever removed from it: a table renamed, or
	// a folder reused for a different extract, leaves both. The scan describes
	// the file as it is now, so the identity it adopts is the one the most
	// recent conversion of that file wrote, not whichever row happens to sit
	// last in the catalog. Ordering there is by table name, so taking the last
	// would have picked the alphabetically greatest -- an obsolete ZOldTable
	// over the ANewTable that actually wrote the file.
	type ident struct {
		table      string
		sourceFile string
		runAt      time.Time
	}
	files := map[string]ident{}
	for _, r := range stored {
		if r.Source != SourceQVD || r.OutputFile == "" || r.SourceTable == "" {
			continue
		}
		p := pathOf(r.OutputFile)
		// Strictly newer wins, so an exact tie keeps the first row seen and the
		// result does not depend on map iteration.
		if cur, ok := files[p]; ok && !r.RunAt.After(cur.runAt) {
			continue
		}
		files[p] = ident{table: r.SourceTable, sourceFile: r.SourceFile, runAt: r.RunAt}
	}
	if len(files) == 0 {
		return current
	}

	// Facts come from the conversion the identity came from. A column of an
	// obsolete table that once shared this output is not this file's column.
	columns := map[key]int{}
	for i, r := range stored {
		if r.Source != SourceQVD {
			continue
		}
		k := keyOf(r)
		if j, dup := columns[k]; dup && !r.RunAt.After(stored[j].RunAt) {
			continue
		}
		columns[k] = i
	}

	out := make([]Row, len(current))
	copy(out, current)
	for i := range out {
		if out[i].Source != SourceParquet || out[i].OutputFile == "" {
			continue
		}
		// The path is the whole of the evidence. Where it does not match, the
		// scan is describing a file this catalog has no conversion for, and it
		// is filed under its own name -- a duplicate table at worst, where
		// guessing on a shared file name would put a description of one file
		// into another table's row.
		id, known := files[pathOf(out[i].OutputFile)]
		if !known {
			continue
		}
		// The whole file belongs to the table that produced it, including a
		// column the conversion never wrote. That column has no QVD side to
		// adopt, so it stays a parquet row -- honestly empty rather than
		// claiming facts nothing recorded.
		out[i].SourceTable = id.table
		j, ok := columns[key{table: id.table, column: out[i].ColumnName}]
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

// canonicalOutput reduces the spellings of one output file to a single string,
// so a path recorded by one run names the same file to the next.
//
// filepath.Clean is not enough: a run given a relative --out-dir would record
// "out/orders.parquet", which means nothing outside the directory it was
// launched from. Rows are written through this rather than only compared
// through it, so what the catalog holds is a path that identifies the file
// from anywhere, and matching is an equality test on evidence rather than a
// guess between spellings.
//
// This is the manifest's notion of path identity, which compares an input
// across runs for the same reason (see canonicalInputPath). A path that cannot
// be made absolute, or that does not resolve because the output is not there,
// falls back to what can be worked out; a run that then fails to match records
// the table again rather than claiming the wrong one.
func canonicalOutput(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}
