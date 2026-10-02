package convert

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// deltaTree builds two table folders of two daily deltas each. Every delta
// holds amounts of a different size, so without a pin each infers its own
// decimal precision, which is the drift a folder schema exists to stop.
func deltaTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	amounts := map[string]float64{"d1": 1.5, "d2": 12345.25}
	for _, table := range []string{"BSEG", "BKPF"} {
		dir := filepath.Join(root, table)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for day, amount := range amounts {
			tbl := qvdtest.Table{Name: table, Fields: []qvdtest.Field{
				{Name: "Amount", Type: "REAL", Rows: []int{0, 0},
					Symbols: []qvd.Symbol{qvdtest.Float(amount)}},
				{Name: "Code", Type: "INTEGER", Rows: []int{0, 0},
					Symbols: []qvd.Symbol{qvdtest.Int(7)}},
			}}
			if _, err := qvdtest.Build(filepath.Join(dir, day+".qvd"), tbl); err != nil {
				t.Fatalf("build fixture: %v", err)
			}
		}
	}
	return root
}

func writeFolderSchema(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FolderSchemaName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runTree converts the tree the way --recursive --keep-tree does.
func runTree(t *testing.T, root, outDir string, opts Options, skip bool) *BatchResult {
	t.Helper()
	found := FindInputs([]string{root}, InputSelection{Recursive: true})
	opts.Force = true
	b, err := RunMany(context.Background(), found.Files, &opts, &ManyOptions{
		OutDir: outDir, Tree: found.Tree, SkipUpToDate: skip, ToolVersion: "2.11.0",
	}, nil)
	if err != nil {
		t.Fatalf("RunMany: %v", err)
	}
	return b
}

func columnType(t *testing.T, path, column string) string {
	t.Helper()
	schema, _ := readParquet(t, path)
	fields, ok := schema.FieldsByName(column)
	if !ok {
		t.Fatalf("%s has no column %s", path, column)
	}
	return fields[0].Type.String()
}

// One run over a tree of tables pins each folder by its own schema and leaves
// a folder without one to inference.
func TestFolderSchemaPinsEveryFileInItsFolder(t *testing.T) {
	root := deltaTree(t)
	writeFolderSchema(t, filepath.Join(root, "BSEG"),
		`{"columns":{"Amount":{"type":"decimal","precision":18,"scale":2}}}`)
	outDir := filepath.Join(t.TempDir(), "out")

	b := runTree(t, root, outDir, testOptions(), false)
	if b.Converted != 4 || b.Failed != 0 {
		t.Fatalf("converted=%d failed=%d, want 4 and 0", b.Converted, b.Failed)
	}
	for _, day := range []string{"d1", "d2"} {
		if got := columnType(t, filepath.Join(outDir, "BSEG", day+".parquet"), "Amount"); got != "decimal(18, 2)" {
			t.Errorf("BSEG/%s Amount = %s, want the pinned decimal(18, 2)", day, got)
		}
	}
	d1 := columnType(t, filepath.Join(outDir, "BKPF", "d1.parquet"), "Amount")
	d2 := columnType(t, filepath.Join(outDir, "BKPF", "d2.parquet"), "Amount")
	if d1 == d2 {
		t.Errorf("BKPF deltas both inferred %s; the fixture no longer shows the drift it guards", d1)
	}
}

// A column pinned by --schema and by the folder takes the folder's pin, even
// spelled with other capitals, and a --schema pin the folder does not mention
// still applies.
func TestFolderSchemaWinsOverSchemaFlag(t *testing.T) {
	root := deltaTree(t)
	writeFolderSchema(t, filepath.Join(root, "BSEG"),
		`{"columns":{"Amount":{"type":"decimal","precision":18,"scale":2}}}`)
	global := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(global, []byte(`{"columns":{"amount":{"type":"float64"},"Code":{"type":"string"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.SchemaOverridePath = global
	outDir := filepath.Join(t.TempDir(), "out")

	runTree(t, root, outDir, opts, false)
	bseg := filepath.Join(outDir, "BSEG", "d1.parquet")
	if got := columnType(t, bseg, "Amount"); got != "decimal(18, 2)" {
		t.Errorf("BSEG Amount = %s, want the folder's decimal(18, 2)", got)
	}
	if got := columnType(t, bseg, "Code"); got != "utf8" {
		t.Errorf("BSEG Code = %s, want --schema's utf8", got)
	}
	if got := columnType(t, filepath.Join(outDir, "BKPF", "d1.parquet"), "Amount"); got != "float64" {
		t.Errorf("BKPF Amount = %s, want --schema's float64", got)
	}
}

// Adding, editing or removing a folder schema reconverts that folder and no
// other, and a tree with no schema file keeps the fingerprint it had before
// the lookup existed.
func TestFolderSchemaReconvertsOnlyItsFolder(t *testing.T) {
	root := deltaTree(t)
	outDir := filepath.Join(t.TempDir(), "out")
	opts := testOptions()
	bseg := filepath.Join(root, "BSEG")

	steps := []struct {
		name      string
		change    func()
		converted int
	}{
		{"first run", func() {}, 4},
		{"unchanged", func() {}, 0},
		{"schema added", func() {
			writeFolderSchema(t, bseg, `{"columns":{"Amount":{"type":"decimal","precision":18,"scale":2}}}`)
		}, 2},
		{"unchanged with schema", func() {}, 0},
		{"schema edited", func() {
			writeFolderSchema(t, bseg, `{"columns":{"Amount":{"type":"decimal","precision":20,"scale":2}}}`)
		}, 2},
		{"schema removed", func() {
			if err := os.Remove(filepath.Join(bseg, FolderSchemaName)); err != nil {
				t.Fatal(err)
			}
		}, 2},
	}
	for _, s := range steps {
		s.change()
		b := runTree(t, root, outDir, opts, true)
		if b.Converted != s.converted || b.Skipped != 4-s.converted || b.Failed != 0 {
			t.Errorf("%s: converted=%d skipped=%d failed=%d, want %d converted",
				s.name, b.Converted, b.Skipped, b.Failed, s.converted)
		}
	}
}

// The cache is not part of the fingerprint: setting it must not reconvert a
// folder converted by a release that had no such field.
func TestFolderSchemasLeaveTheFingerprintAlone(t *testing.T) {
	o := testOptions()
	want, err := FingerprintOptions(&o, "2.11.0")
	if err != nil {
		t.Fatal(err)
	}
	o.FolderSchemas = NewFolderSchemas()
	got, err := FingerprintOptions(&o, "2.11.0")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Error("a FolderSchemas cache changed the fingerprint")
	}
}

// A schema that cannot be parsed fails every file of its folder, naming the
// file, and leaves the other folders converting.
func TestFolderSchemaBrokenFailsItsFolder(t *testing.T) {
	root := deltaTree(t)
	writeFolderSchema(t, filepath.Join(root, "BSEG"), `{"columns":{"Amount":{"type":"money"}}}`)
	outDir := filepath.Join(t.TempDir(), "out")

	b := runTree(t, root, outDir, testOptions(), true)
	if b.Converted != 2 || b.Failed != 2 {
		t.Fatalf("converted=%d failed=%d, want 2 and 2", b.Converted, b.Failed)
	}
	for _, r := range b.Results {
		if r.Err != nil && !strings.Contains(r.Err.Error(), FolderSchemaName) {
			t.Errorf("%s: %v does not name the schema file", r.Input, r.Err)
		}
	}
}

// Each folder is read once per run, so an edit after the first read is not
// seen by a later file of the same run.
func TestFolderSchemasReadEachFolderOnce(t *testing.T) {
	root := deltaTree(t)
	dir := filepath.Join(root, "BSEG")
	writeFolderSchema(t, dir, `{"columns":{"Amount":{"type":"float64"}}}`)
	c := NewFolderSchemas()

	first := c.For(filepath.Join(dir, "d1.qvd"))
	writeFolderSchema(t, dir, `{"columns":{"Amount":{"type":"string"}}}`)
	second := c.For(filepath.Join(dir, "d2.qvd"))
	if first != second {
		t.Fatal("the second file of a folder read its schema again")
	}
	if got := second.Override.Columns["Amount"].Type; got != "float64" {
		t.Errorf("Amount pinned to %s, want the float64 read first", got)
	}
}

// A folder schema generated from Parquet already written carries the names
// --field-regex produced, so it matches those as well as the QVD's own.
// --schema keeps matching the QVD name alone.
func TestFolderSchemaMatchesRenamedFields(t *testing.T) {
	dir := t.TempDir()
	tbl := qvdtest.Table{Name: "A057", Fields: []qvdtest.Field{
		{Name: "A057-||-KBETR-||-Betrag", Type: "REAL", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Float(1.5)}},
		{Name: "A057-||-KMEIN-||-Menge", Type: "REAL", Rows: []int{0},
			Symbols: []qvd.Symbol{qvdtest.Float(2.5)}},
	}}
	in := filepath.Join(dir, "A057.qvd")
	if _, err := qvdtest.Build(in, tbl); err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	writeFolderSchema(t, dir, `{"columns":{"KBETR":{"type":"decimal","precision":18,"scale":2}}}`)
	global := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(global, []byte(`{"columns":{"KMEIN":{"type":"float64"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := testOptions()
	opts.SchemaOverridePath = global
	r, err := NewFieldRenamer(`^[^-]*-\|\|-(?P<name>[^-]*)-\|\|-(?P<comment>.*)$`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	opts.Renamer = r
	out := filepath.Join(t.TempDir(), "A057.parquet")
	if _, _, err := Run(context.Background(), in, out, &opts, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := columnType(t, out, "KBETR"); got != "decimal(18, 2)" {
		t.Errorf("KBETR = %s, want the folder's decimal(18, 2) matched by its output name", got)
	}
	if got := columnType(t, out, "KMEIN"); got == "float64" {
		t.Error("--schema matched a renamed name; it must keep matching the QVD name only")
	}
}
