package convert

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"
)

// mutators change an option to something a fingerprint must notice. Scalars
// are handled generically below; anything else has to be named here, so a new
// option of a shape the walker does not understand fails this test rather than
// dropping silently out of every fingerprint.
var mutators = map[string]func(t *testing.T, o *Options){
	"Renamer": func(t *testing.T, o *Options) {
		o.Renamer = &FieldRenamer{Regex: regexp.MustCompile(`^(?P<name>.*)$`), NameTemplate: "${name}"}
	},
	"Location":  func(t *testing.T, o *Options) { o.Location = time.FixedZone("CET", 3600) },
	"Encodings": func(t *testing.T, o *Options) { o.Encodings = EncodingSpec{Auto: true} },
	// A path, but one the fingerprint opens: it has to name a real file.
	"SchemaOverridePath": func(t *testing.T, o *Options) {
		o.SchemaOverridePath = filepath.Join(t.TempDir(), "schema.json")
		if err := os.WriteFile(o.SchemaOverridePath, []byte(`{"columns":{}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	},
}

// TestFingerprintNoticesEveryOptionThatMatters walks Options field by field.
// An option that can change what is written and does not change the
// fingerprint is the failure that produces a wrong skip, which is the one
// mistake this feature must not make.
func TestFingerprintNoticesEveryOptionThatMatters(t *testing.T) {
	base := DefaultOptions()
	want, err := FingerprintOptions(&base, "2.2.0")
	if err != nil {
		t.Fatalf("fingerprint of the defaults: %v", err)
	}

	typ := reflect.TypeOf(base)
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if fingerprintIgnores[name] {
			continue
		}
		o := DefaultOptions()
		if m, ok := mutators[name]; ok {
			m(t, &o)
		} else if !mutateScalar(reflect.ValueOf(&o).Elem().Field(i)) {
			t.Errorf("%s is neither a scalar nor listed in mutators; decide whether it "+
				"changes what is written and add it there or to fingerprintIgnores", name)
			continue
		}
		got, err := FingerprintOptions(&o, "2.2.0")
		if err != nil {
			t.Errorf("fingerprint with %s changed: %v", name, err)
			continue
		}
		if got == want {
			t.Errorf("changing %s did not change the fingerprint", name)
		}
	}
}

// mutateScalar sets a field to something other than what it holds, reporting
// whether it knew how.
func mutateScalar(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.String:
		v.SetString(v.String() + "-changed")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(v.Float() + 1)
	case reflect.Slice:
		v.Set(reflect.Append(v, reflect.New(v.Type().Elem()).Elem()))
	default:
		return false
	}
	return true
}

// The options that cannot change what is written must stay out, or a folder
// would reconvert itself over a different --workers.
func TestFingerprintIgnoresWhatCannotChangeTheOutput(t *testing.T) {
	base := DefaultOptions()
	want, err := FingerprintOptions(&base, "2.2.0")
	if err != nil {
		t.Fatal(err)
	}
	o := DefaultOptions()
	o.Workers = 8
	o.ProgressEvery = 42
	o.Force = true
	o.SchemaReportPath = "schema.json"
	o.QualityReportPath = "quality.json"
	got, err := FingerprintOptions(&o, "2.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("parallelism and report paths changed the fingerprint")
	}
}

// From 2.0.0 a default does not change what an existing file converts to
// outside a major bump, so a minor upgrade must not reconvert a folder and a
// major one must.
func TestFingerprintFollowsTheMajorVersionOnly(t *testing.T) {
	o := DefaultOptions()
	v220, err := FingerprintOptions(&o, "2.2.0")
	if err != nil {
		t.Fatal(err)
	}
	v290, _ := FingerprintOptions(&o, "2.9.1")
	v300, _ := FingerprintOptions(&o, "3.0.0")
	if v220 != v290 {
		t.Errorf("a minor upgrade invalidated the manifest")
	}
	if v220 == v300 {
		t.Errorf("a major upgrade left the manifest valid")
	}
}

// The schema override is a file the flag only names. Editing it changes every
// output it touches, so its contents have to count.
func TestFingerprintReadsTheSchemaOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, []byte(`{"columns":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	o := DefaultOptions()
	o.SchemaOverridePath = path
	before, err := FingerprintOptions(&o, "2.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"columns":{"Id":{"type":"string"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := FingerprintOptions(&o, "2.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Errorf("editing the --schema file left the fingerprint unchanged")
	}

	// A --schema that cannot be read is an error rather than a fingerprint
	// over a file nobody looked at.
	o.SchemaOverridePath = filepath.Join(t.TempDir(), "missing.json")
	if _, err := FingerprintOptions(&o, "2.2.0"); err == nil {
		t.Errorf("an unreadable --schema fingerprinted anyway")
	}
}

// Joining a list with a separator the values may contain lets two different
// option sets hash the same, and a skip on a wrong fingerprint is exactly the
// stale output this feature exists to prevent. Qlik field names contain spaces
// routinely, so --columns is the ordinary case.
func TestFingerprintDistinguishesAmbiguousLists(t *testing.T) {
	fp := func(mutate func(o *Options)) string {
		t.Helper()
		o := DefaultOptions()
		mutate(&o)
		got, err := FingerprintOptions(&o, "2.2.0")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	for _, tc := range []struct {
		name string
		a, b func(o *Options)
	}{
		{
			"one column named with a space against two columns",
			func(o *Options) { o.Columns = []string{"Sales Amount"} },
			func(o *Options) { o.Columns = []string{"Sales", "Amount"} },
		},
		{
			"the same for --exclude patterns",
			func(o *Options) { o.Exclude = []string{"A B"} },
			func(o *Options) { o.Exclude = []string{"A", "B"} },
		},
		{
			"a list against the empty one it renders like",
			func(o *Options) { o.Columns = []string{""} },
			func(o *Options) { o.Columns = []string{} },
		},
		{
			"an encoding rule whose pattern carries the separator",
			func(o *Options) {
				o.Encodings = EncodingSpec{Rules: []EncodingRule{{Pattern: "A B", Encoding: "plain"}}}
			},
			func(o *Options) {
				o.Encodings = EncodingSpec{Rules: []EncodingRule{
					{Pattern: "A", Encoding: "plain"}, {Pattern: "B", Encoding: "plain"},
				}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if fp(tc.a) == fp(tc.b) {
				t.Errorf("two different option sets fingerprinted the same")
			}
		})
	}
}

// The entry is keyed by the output name, and two folders can each hold an
// A.qvd. Size and timestamp cannot tell those two inputs apart once a copy has
// preserved the timestamps, so the entry has to name the input it came from.
func TestManifestChecksTheInputItRecorded(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(outDir, "A.parquet")
	if err := os.WriteFile(out, []byte("output"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two sources of the same name and size, with the timestamps a copy that
	// preserved them would leave behind.
	same := time.Now().Add(-time.Hour)
	var inputs []string
	for i, content := range []string{"prod-data", "test-data"} {
		d := filepath.Join(dir, []string{"prod", "test"}[i])
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		in := filepath.Join(d, "A.qvd")
		if err := os.WriteFile(in, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(in, same, same); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, in)
	}

	m := LoadManifest(outDir)
	m.Record(inputs[0], out, "fp", 10)
	if !m.UpToDate(inputs[0], out, "fp") {
		t.Fatal("the input that was recorded is not up to date")
	}
	if m.UpToDate(inputs[1], out, "fp") {
		t.Error("a different source file of the same name, size and timestamp was " +
			"skipped: the output would be the other folder's")
	}

	// The same file reached by a relative path from the directory holding it
	// is still the same file, so a script run from elsewhere does not
	// reconvert the folder.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(filepath.Dir(inputs[0])); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	if !m.UpToDate("A.qvd", out, "fp") {
		t.Error("the same file named relatively was not recognized")
	}
}

// manifestFixture converts nothing; it writes the two files an entry
// describes and returns the manifest recording them.
func manifestFixture(t *testing.T) (dir, in, out string, m *Manifest) {
	t.Helper()
	dir = t.TempDir()
	in = filepath.Join(dir, "a.qvd")
	out = filepath.Join(dir, "a.parquet")
	if err := os.WriteFile(in, []byte("input"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("output"), 0o644); err != nil {
		t.Fatal(err)
	}
	m = LoadManifest(dir)
	m.Record(in, out, "fp", 10)
	return dir, in, out, m
}

func TestManifestUpToDate(t *testing.T) {
	dir, in, out, m := manifestFixture(t)

	if !m.UpToDate(in, out, "fp") {
		t.Fatal("a file this run just recorded is not up to date")
	}
	if m.UpToDate(in, out, "other") {
		t.Error("a changed fingerprint was still up to date")
	}
	if (*Manifest)(nil).UpToDate(in, out, "fp") {
		t.Error("a nil manifest reported something up to date")
	}

	// A re-extracted input, at the same size, the way a nightly extract
	// arrives.
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(in, later, later); err != nil {
		t.Fatal(err)
	}
	if m.UpToDate(in, out, "fp") {
		t.Error("a re-extracted input was still up to date")
	}
	m.Record(in, out, "fp", 10)

	// An output edited or replaced by something else since.
	if err := os.WriteFile(out, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m.UpToDate(in, out, "fp") {
		t.Error("a replaced output was still up to date")
	}
	m.Record(in, out, "fp", 10)

	// An output deleted since: the manifest remembers it, the disk does not.
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	if m.UpToDate(in, out, "fp") {
		t.Error("a deleted output was still up to date")
	}

	// An input that has gone away converts, and reports the failure through
	// the usual path rather than being skipped into silence.
	if err := os.Remove(in); err != nil {
		t.Fatal(err)
	}
	if m.UpToDate(in, out, "fp") {
		t.Error("a missing input was up to date")
	}
	_ = dir
}

func TestManifestRoundTrip(t *testing.T) {
	dir, in, out, m := manifestFixture(t)
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	if !LoadManifest(dir).UpToDate(in, out, "fp") {
		t.Error("a saved manifest did not survive the round trip")
	}

	// Anything wrong with the file converts the folder again rather than
	// failing the run: the worst a lost manifest can do is repeat work.
	if err := os.WriteFile(ManifestPath(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadManifest(dir); len(got.Entries) != 0 {
		t.Errorf("a corrupt manifest was read as %v", got.Entries)
	}

	// So does one written by a format this binary does not know.
	future, err := json.Marshal(Manifest{Format: manifestFormat + 1, Entries: m.Entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ManifestPath(dir), future, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadManifest(dir); len(got.Entries) != 0 {
		t.Errorf("a manifest from a later format was read as %v", got.Entries)
	}
}
