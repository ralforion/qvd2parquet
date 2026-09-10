package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// col builds a row for one column of one table, with everything a merge keys
// on or is expected to carry across.
func col(table, name, pqType string, ordinal int32) Row {
	return Row{
		Source:       SourceQVD,
		SourceFile:   table + ".qvd",
		SourceTable:  table,
		OutputFile:   table + ".parquet",
		Ordinal:      ordinal,
		ColumnName:   name,
		SourceColumn: name,
		ParquetType:  pqType,
		HasSymbols:   true,
	}
}

// writeCatalog runs a whole catalog write through the writer, so the tests
// exercise the same path a run takes rather than a hand-built file.
func writeCatalog(t *testing.T, path, version string, force bool, rows []Row) *Writer {
	t.Helper()
	w, err := NewWriter(path, version, force)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	w.Add(rows)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return w
}

// byKey indexes a catalog for assertions.
func byKey(rows []Row) map[key]Row {
	m := make(map[key]Row, len(rows))
	for _, r := range rows {
		m[keyOf(r)] = r
	}
	return m
}

// A run over one table must not erase the record of the others. This is the
// whole point of an additive catalog: a nightly job converts a subset.
func TestCatalogKeepsTablesTheRunDidNotTouch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.parquet")
	writeCatalog(t, path, "1", false, []Row{
		col("A057", "DATBI", "int64", 1),
		col("A057", "KSCHL", "utf8", 2),
		col("MARA", "MATNR", "utf8", 1),
	})
	w := writeCatalog(t, path, "2", false, []Row{col("MARA", "MATNR", "utf8", 1)})

	if !w.Merging() {
		t.Fatal("second run did not merge into the existing catalog")
	}
	if w.StoredLen() != 3 {
		t.Fatalf("StoredLen = %d, want 3", w.StoredLen())
	}

	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("catalog holds %d rows, want 3: %+v", len(got), got)
	}
	idx := byKey(got)
	if r, ok := idx[key{"A057", "DATBI"}]; !ok {
		t.Error("A057.DATBI was dropped by a run that only converted MARA")
	} else if r.ToolVersion != "1" {
		t.Errorf("A057.DATBI tool_version = %q, want the run that wrote it, %q", r.ToolVersion, "1")
	}
	if r := idx[key{"MARA", "MATNR"}]; r.ToolVersion != "2" {
		t.Errorf("MARA.MATNR tool_version = %q, want the newer run, %q", r.ToolVersion, "2")
	}
}

// A column whose type changed is updated in place rather than recorded twice.
func TestCatalogUpdatesAChangedType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.parquet")
	writeCatalog(t, path, "1", false, []Row{col("A057", "KBETR", "float64", 1)})
	writeCatalog(t, path, "2", false, []Row{col("A057", "KBETR", "decimal(4, 2)", 1)})

	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("catalog holds %d rows, want 1: %+v", len(got), got)
	}
	if got[0].ParquetType != "decimal(4, 2)" {
		t.Errorf("parquet_type = %q, want the type the later run wrote", got[0].ParquetType)
	}
}

// A new column of a table the catalog already knows is added beside the ones
// it has, and a column the run no longer produces is kept.
func TestCatalogAddsAndKeepsColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.parquet")
	writeCatalog(t, path, "1", false, []Row{
		col("A057", "DATBI", "int64", 1),
		col("A057", "GONE", "utf8", 2),
	})
	writeCatalog(t, path, "2", false, []Row{
		col("A057", "DATBI", "int64", 1),
		col("A057", "NEW", "utf8", 2),
	})

	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	idx := byKey(got)
	for _, want := range []string{"DATBI", "GONE", "NEW"} {
		if _, ok := idx[key{"A057", want}]; !ok {
			t.Errorf("A057.%s missing from the merged catalog", want)
		}
	}
	// The dropped column keeps the run that last saw it, which is what makes
	// it findable as stale.
	if r := idx[key{"A057", "GONE"}]; r.ToolVersion != "1" {
		t.Errorf("A057.GONE tool_version = %q, want %q", r.ToolVersion, "1")
	}
}

// --force is still replace, for the run that wants to start over.
func TestForceReplacesTheCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.parquet")
	writeCatalog(t, path, "1", false, []Row{col("A057", "DATBI", "int64", 1)})
	w := writeCatalog(t, path, "2", true, []Row{col("MARA", "MATNR", "utf8", 1)})

	if w.Merging() {
		t.Error("--force merged instead of replacing")
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 1 || got[0].SourceTable != "MARA" {
		t.Fatalf("catalog = %+v, want only the forced run's row", got)
	}
}

// A run that accounted for nothing leaves the catalog alone, merge or not.
func TestUnstartedRunLeavesTheCatalogIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.parquet")
	writeCatalog(t, path, "1", false, []Row{col("A057", "DATBI", "int64", 1)})

	w, err := NewWriter(path, "2", false)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 1 || got[0].ToolVersion != "1" {
		t.Fatalf("catalog = %+v, want the previous run untouched", got)
	}
}

// A path holding something that is not a catalog fails when the writer opens,
// before the run converts anything, rather than being silently replaced.
func TestNonCatalogPathFailsEarly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notacatalog.parquet")
	writeCommented(t, path)

	_, err := NewWriter(path, "1", false)
	if err == nil {
		t.Fatal("NewWriter accepted a Parquet file that is not a catalog")
	}
	if !strings.Contains(err.Error(), "not a catalog") {
		t.Errorf("error = %v, want it to say the file is not a catalog", err)
	}

	// The file is still there: nothing was written over it.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("stat after refusal: %v", err)
	}
}

// The rows a merge writes are ordered by table and ordinal, so a table's
// columns stay together however many runs contributed them.
func TestMergedCatalogIsOrdered(t *testing.T) {
	rows := Merge(
		[]Row{col("MARA", "MTART", "utf8", 2), col("A057", "KSCHL", "utf8", 2)},
		[]Row{col("MARA", "MATNR", "utf8", 1), col("A057", "DATBI", "int64", 1)},
	)
	var got []string
	for _, r := range rows {
		got = append(got, r.SourceTable+"."+r.ColumnName)
	}
	want := []string{"A057.DATBI", "A057.KSCHL", "MARA.MATNR", "MARA.MTART"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// Reading a catalog back has to preserve what a merge then writes out again,
// including the null in symbols and the run timestamp.
func TestRoundTripPreservesRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.parquet")
	in := col("A057", "KBETR", "decimal(4, 2)", 3)
	in.Comment = "Betrag"
	in.QlikType = "MONEY"
	in.Nullable = true
	in.HasSymbols = false
	in.SourceRows = 1234
	in.ValueRange = "0.00..99.99"
	in.Strategy = "decimal"
	in.Note = "written as decimal(4, 2)"
	w := writeCatalog(t, path, "1", false, []Row{in})

	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d rows, want 1", len(got))
	}
	g := got[0]
	if g.HasSymbols {
		t.Error("symbols came back non-null, want the null that was written")
	}
	if g.RunAt.IsZero() || g.RunAt.Sub(w.RunAt()).Abs() > time.Second {
		t.Errorf("run_at = %v, want the writer's %v", g.RunAt, w.RunAt())
	}
	g.RunAt, g.HasSymbols = in.RunAt, in.HasSymbols
	want := in
	want.ToolVersion = "1"
	if g != want {
		t.Errorf("round trip changed the row:\n got %+v\nwant %+v", g, want)
	}
}

// scanned is a row as ScanFile produces one: it knows the file it read and
// what is in its schema, and nothing about the QVD behind it.
func scanned(output, name string, ordinal int32) Row {
	return Row{
		Source:       SourceParquet,
		SourceFile:   output,
		SourceTable:  strings.TrimSuffix(filepath.Base(output), filepath.Ext(output)),
		OutputFile:   output,
		Ordinal:      ordinal,
		ColumnName:   name,
		SourceColumn: name,
		ParquetType:  "int64",
	}
}

// A scan of a file the catalog was written from adopts the table it was
// converted under, rather than filing a second copy under the file's name.
func TestScanAdoptsTheConvertedTableIdentity(t *testing.T) {
	stored := col("HeaderOrders", "Id", "int64", 1)
	stored.OutputFile = "out/orders.parquet"
	stored.SourceFile = "orders.qvd"
	stored.QlikType = "INTEGER"
	stored.Strategy = "int64"
	stored.ValueRange = "1..2"
	stored.Note = "2 integer symbols"

	got := Merge([]Row{stored}, []Row{scanned("out/orders.parquet", "Id", 1)})
	if len(got) != 1 {
		t.Fatalf("merge produced %d rows, want 1: %+v", len(got), got)
	}
	if got[0].SourceTable != "HeaderOrders" {
		t.Errorf("source_table = %q, want the converted identity", got[0].SourceTable)
	}
	// The QVD side is what the scan cannot see, so it comes from the stored row.
	for _, f := range []struct{ name, got, want string }{
		{"qlik_type", got[0].QlikType, "INTEGER"},
		{"strategy", got[0].Strategy, "int64"},
		{"value_range", got[0].ValueRange, "1..2"},
		{"note", got[0].Note, "2 integer symbols"},
		{"source", got[0].Source, SourceQVD},
		{"source_file", got[0].SourceFile, "orders.qvd"},
	} {
		if f.got != f.want {
			t.Errorf("%s = %q, want %q", f.name, f.got, f.want)
		}
	}
}

// A column the conversion never wrote has no QVD side to adopt. It joins the
// table its file belongs to and stays honestly a parquet row.
func TestScanOfAnUnknownColumnStaysAParquetRow(t *testing.T) {
	stored := col("HeaderOrders", "Id", "int64", 1)
	stored.OutputFile = "out/orders.parquet"
	stored.QlikType = "INTEGER"

	got := Merge([]Row{stored}, []Row{scanned("out/orders.parquet", "Added", 2)})
	idx := byKey(got)
	r, ok := idx[key{"HeaderOrders", "Added"}]
	if !ok {
		t.Fatalf("the new column did not join its file's table: %+v", got)
	}
	if r.Source != SourceParquet {
		t.Errorf("source = %q, want %q for a column no conversion recorded", r.Source, SourceParquet)
	}
	if r.QlikType != "" {
		t.Errorf("qlik_type = %q, want empty: nothing recorded one", r.QlikType)
	}
}

// A scan of a file the catalog knows nothing about keeps the file's own name,
// which is all there is to go on.
func TestScanOfAnUnknownFileKeepsItsFileName(t *testing.T) {
	got := Merge(
		[]Row{col("HeaderOrders", "Id", "int64", 1)},
		[]Row{scanned("out/other.parquet", "Id", 1)},
	)
	if _, ok := byKey(got)[key{"other", "Id"}]; !ok {
		t.Errorf("scan of an uncatalogued file did not keep its own name: %+v", got)
	}
}

// landedOn is the table the scanned row ended up under. The stored rows
// survive a misfiled scan, so asserting that a table is present proves
// nothing: what matters is which row the scan refreshed.
func landedOn(t *testing.T, rows []Row) string {
	t.Helper()
	var found []string
	for _, r := range rows {
		if r.ToolVersion == "scan" {
			found = append(found, r.SourceTable)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected the scan to land on exactly one row, landed on %v of %+v", found, rows)
	}
	return found[0]
}

// scanRow is a scanned row marked so the assertions can find it.
func scanRow(output, name string, ordinal int32) Row {
	r := scanned(output, name, ordinal)
	r.ToolVersion = "scan"
	return r
}

// An output path can have belonged to more than one table, because a catalog
// never removes anything. A scan describes the file as it is now, so it has to
// refresh the table the most recent conversion of that file wrote -- not
// whichever row sorts last, which is what taking the last stored row did.
func TestScanAdoptsTheNewestTableForAReusedOutput(t *testing.T) {
	old := col("ZOldTable", "Id", "int64", 1)
	old.OutputFile = "out/orders.parquet"
	old.QlikType = "TEXT"
	old.Strategy = "utf8"
	old.ToolVersion = "old"
	old.RunAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	current := col("ANewTable", "Id", "int64", 1)
	current.OutputFile = "out/orders.parquet"
	current.QlikType = "INTEGER"
	current.Strategy = "int64"
	current.ToolVersion = "new"
	current.RunAt = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	// Sorted as the catalog stores them, so ZOldTable is the last row.
	stored := []Row{current, old}
	sortRows(stored)

	got := Merge(stored, []Row{scanRow("out/orders.parquet", "Id", 1)})
	if len(got) != 2 {
		t.Fatalf("merge produced %d rows, want 2: %+v", len(got), got)
	}
	if table := landedOn(t, got); table != "ANewTable" {
		t.Errorf("the scan refreshed %q, want the table that last wrote the file", table)
	}
	idx := byKey(got)
	if r := idx[key{"ANewTable", "Id"}]; r.QlikType != "INTEGER" || r.Strategy != "int64" {
		t.Errorf("facts came from the obsolete table: qlik_type=%q strategy=%q", r.QlikType, r.Strategy)
	}
	// The obsolete table is still there and untouched: nothing is removed, and
	// its run_at is what marks it stale.
	z := idx[key{"ZOldTable", "Id"}]
	if z.ToolVersion != "old" || z.QlikType != "TEXT" {
		t.Errorf("the obsolete row was rewritten: %+v", z)
	}
}

// The base-name fallback covers a path recorded relative to a working
// directory the scan cannot reconstruct, and declines everywhere else.
func TestBaseNameFallbackIsNarrow(t *testing.T) {
	relative := col("HeaderOrders", "Id", "int64", 1)
	relative.OutputFile = "out/orders.parquet"
	relative.QlikType = "INTEGER"
	relative.ToolVersion = "converted"

	absolute := relative
	absolute.OutputFile = filepath.Join(string(filepath.Separator), "srv", "out", "orders.parquet")

	other := col("OtherTable", "Id", "int64", 1)
	other.OutputFile = "elsewhere/orders.parquet"
	other.ToolVersion = "converted"

	scanPath := filepath.Join(string(filepath.Separator), "elsewhere", "entirely", "orders.parquet")

	tests := []struct {
		name      string
		stored    []Row
		wantTable string
		wantRows  int
	}{
		{
			// The stored path means nothing outside the directory it was
			// written in, so the name is all there is, and it is unambiguous.
			"a relative stored path falls back to the name",
			[]Row{relative}, "HeaderOrders", 1,
		},
		{
			// An absolute stored path already means the same file everywhere,
			// so a scan that does not match it is describing a different file.
			"an absolute stored path does not",
			[]Row{absolute}, "orders", 2,
		},
		{
			// Two tables wrote a file of this name. Guessing between them
			// would hand the scan another table's metadata.
			"two tables of the same name decline",
			[]Row{relative, other}, "orders", 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Merge(tt.stored, []Row{scanRow(scanPath, "Id", 1)})
			if len(got) != tt.wantRows {
				t.Errorf("merge produced %d rows, want %d: %+v", len(got), tt.wantRows, got)
			}
			if table := landedOn(t, got); table != tt.wantTable {
				t.Errorf("the scan landed on %q, want %q", table, tt.wantTable)
			}
		})
	}
}
