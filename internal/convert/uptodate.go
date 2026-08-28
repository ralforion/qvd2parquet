package convert

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ralforion/qvd2parquet/internal/qvd"
)

// ManifestName is the record a folder conversion keeps of what it produced,
// written into --out-dir and read back by --skip-up-to-date. It is a dotfile
// so the Parquet readers that scan a directory ignore it, as they ignore any
// name beginning with a dot or an underscore.
const ManifestName = ".qvd2parquet-manifest.json"

// manifestFormat versions the file itself. A manifest written by a format the
// binary does not know is discarded rather than misread, which costs one
// conversion and never a wrong skip.
const manifestFormat = 1

// Manifest records what a run produced, so a later one can tell an output it
// made itself from an output that merely exists.
//
// It exists because the obvious test, "is the .parquet newer than the .qvd",
// answers the wrong question. It cannot see that the conversion options
// changed since, so a run with a different --decimal-source or --encoding
// would skip a file it has never actually converted that way. It is fooled by
// a re-extracted QVD copied with its timestamps preserved, which arrives
// looking older than the output it should replace. And it trusts two clocks on
// a network share to agree. The manifest answers "did this exact run already
// produce this file" instead, which is the question worth asking.
type Manifest struct {
	Format int `json:"format"`
	// Entries are keyed by the output file name within --out-dir, since that
	// is what the run promises to have produced.
	Entries map[string]ManifestEntry `json:"entries"`
}

// ManifestEntry is one converted file, described well enough to tell whether
// converting it again would produce the same thing.
type ManifestEntry struct {
	// Input is the absolute path the output was produced from. It is compared,
	// not merely recorded: the entry is keyed by the output name, and two
	// folders can each hold an A.qvd. Size and timestamp cannot separate those
	// two inputs when a copy preserved the timestamps, and skipping then hands
	// back a Parquet built from the other one.
	Input         string `json:"input"`
	InputSize     int64  `json:"inputSize"`
	InputModTime  string `json:"inputModTime"`
	OutputSize    int64  `json:"outputSize"`
	OutputModTime string `json:"outputModTime"`
	// Fingerprint identifies the conversion options the output was written
	// with, so changing a flag invalidates it.
	Fingerprint string `json:"fingerprint"`
	Rows        int64  `json:"rows"`
	ConvertedAt string `json:"convertedAt"`
}

// ManifestPath is where a folder conversion keeps its record.
func ManifestPath(outDir string) string { return filepath.Join(outDir, ManifestName) }

// LoadManifest reads the record from a previous run. Anything wrong with it,
// from a missing file to a corrupt one, yields an empty manifest rather than
// an error: the worst a lost manifest can do is convert a file that did not
// need converting, and failing the run instead would be the more expensive
// answer to a file the run itself owns.
func LoadManifest(outDir string) *Manifest {
	empty := &Manifest{Format: manifestFormat, Entries: map[string]ManifestEntry{}}
	b, err := os.ReadFile(ManifestPath(outDir))
	if err != nil {
		return empty
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil || m.Format != manifestFormat || m.Entries == nil {
		return empty
	}
	return &m
}

// Save writes the record back. The caller reports a failure as a note rather
// than a failed run: every file still converted, and the only cost is that the
// next run repeats them.
func (m *Manifest) Save(outDir string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Written through a temporary file and renamed, so an interrupted write
	// leaves the previous manifest intact rather than a truncated one that
	// would be discarded on the next read.
	tmp := ManifestPath(outDir) + fmt.Sprintf(".tmp-%d", os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, ManifestPath(outDir)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// UpToDate reports whether the output can be left alone: the manifest has to
// name it, the input has to be the one that produced it, the options have to
// be the ones it was produced with, and the output itself has to be untouched
// since. Anything else, including a stat that fails, converts.
func (m *Manifest) UpToDate(input, output, fingerprint string) bool {
	if m == nil {
		return false
	}
	e, ok := m.Entries[filepath.Base(output)]
	if !ok || e.Fingerprint != fingerprint {
		return false
	}
	// Compared exactly. A folder that moved, or a path spelled with different
	// capitals on a filesystem that does not care, converts once more than it
	// had to; the alternative is folding two paths together and skipping a
	// file this run has never seen.
	if e.Input != canonicalInputPath(input) {
		return false
	}
	in, err := os.Stat(input)
	if err != nil {
		return false
	}
	if e.InputSize != in.Size() || e.InputModTime != stamp(in.ModTime()) {
		return false
	}
	out, err := os.Stat(output)
	if err != nil {
		return false
	}
	return e.OutputSize == out.Size() && e.OutputModTime == stamp(out.ModTime())
}

// Record notes a file this run converted. A file it could not stat afterwards
// is left out of the manifest, so the next run converts it again rather than
// trusting a half-known entry.
func (m *Manifest) Record(input, output, fingerprint string, rows int64) {
	if m == nil {
		return
	}
	in, err := os.Stat(input)
	if err != nil {
		return
	}
	out, err := os.Stat(output)
	if err != nil {
		return
	}
	m.Entries[filepath.Base(output)] = ManifestEntry{
		Input:         canonicalInputPath(input),
		InputSize:     in.Size(),
		InputModTime:  stamp(in.ModTime()),
		OutputSize:    out.Size(),
		OutputModTime: stamp(out.ModTime()),
		Fingerprint:   fingerprint,
		Rows:          rows,
		ConvertedAt:   stamp(time.Now()),
	}
}

// canonicalInputPath is the identity an entry records, so the same file
// reached by a relative path, through a symlinked parent, or from a different
// working directory is still recognized as the same file. Two spellings that
// resolve differently only cost one conversion, but a folder under /tmp on
// macOS is reached both ways routinely, and reconverting it every night would
// make the feature useless there.
//
// This is the simple form of the command's canonicalPath, which also has to
// follow a dangling final symlink because it reasons about files that do not
// exist yet. An input that cannot be resolved is one the conversion is about
// to fail on anyway.
func canonicalInputPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// stamp formats a modification time. Both sides of every comparison go through
// this, so the two strings are produced identically and can be compared
// without parsing either back.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// fingerprintIgnores names the options that cannot change what is written.
// Everything else counts, including the ones that only decide whether the run
// is allowed to succeed: a tightened quality gate has a verdict to deliver,
// and delivering it means converting the file again.
//
// The list is deliberately an exclusion rather than an inclusion. A new option
// added later is then fingerprinted by default, and the mistake it can make is
// an unnecessary conversion rather than a wrong skip.
var fingerprintIgnores = map[string]bool{
	"Workers":           true, // parallelism
	"ProgressEvery":     true, // logging
	"Force":             true, // permission to overwrite, not what is written
	"SchemaReportPath":  true, // a side document
	"QualityReportPath": true, // a side document
}

// FingerprintOptions identifies a conversion by everything about it that can
// change the bytes written, so a manifest entry survives a rerun with the same
// flags and no other.
func FingerprintOptions(o *Options, toolVersion string) (string, error) {
	h := sha256.New()
	// Only the major version. From 2.0.0 the project promises that a default
	// will not change what an existing file converts to outside a major bump,
	// so a minor upgrade must not reconvert a folder and a major one must.
	fmt.Fprint(h, tagged("tool"), tagged(majorVersion(toolVersion)))

	v := reflect.ValueOf(*o)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Name
		if fingerprintIgnores[name] {
			continue
		}
		s, err := fingerprintValue(v.Field(i))
		if err != nil {
			return "", fmt.Errorf("cannot fingerprint --%s: %w", name, err)
		}
		fmt.Fprint(h, tagged(name), tagged(s))
	}

	// The schema override's contents, not merely its path: editing that file
	// without renaming it changes every output it touches.
	if o.SchemaOverridePath != "" {
		b, err := os.ReadFile(o.SchemaOverridePath)
		if err != nil {
			return "", fmt.Errorf("read --schema %s: %w", o.SchemaOverridePath, err)
		}
		fmt.Fprint(h, tagged("schemaOverride"), tagged(fmt.Sprintf("%x", sha256.Sum256(b))))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// majorVersion takes the leading number of a version string.
func majorVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	if i := strings.Index(v, "."); i >= 0 {
		return v[:i]
	}
	return v
}

// tagged length-prefixes a rendering, so no two different values can produce
// one string. Joining with a separator cannot do that when the separator is a
// character the values may contain: []string{"Sales Amount"} and
// []string{"Sales", "Amount"} both render as "Sales Amount" under a space
// join, and --columns 'Sales Amount' would then fingerprint the same as
// --columns 'Sales,Amount'. Qlik field names contain spaces routinely, so this
// is the ordinary case rather than a contrived one.
func tagged(s string) string { return strconv.Itoa(len(s)) + ":" + s }

// zoneTransitionLimit stops the walk below if a zone ever presents more
// boundaries than a rule set plausibly has. Every zone in tzdata is under two
// hundred across the whole range, since the table ends at its last real
// transition rather than projecting a rule forward for ever.
const zoneTransitionLimit = 100000

// zoneIdentity describes what a timezone does rather than what it is called.
//
// A name is not identity here. time.Local reports its IANA name only when TZ
// is set; with TZ unset, which is how a server usually gets its zone from
// /etc/localtime, its String() is the bare word "Local" on a machine in Berlin
// and on one in Tokyo alike. --timezone Local converts a QVD's wall clock
// against that zone, so its output depends on the converting machine, and a
// manifest fingerprinting the word "Local" would be trusted by a machine that
// would now write different timestamps.
//
// Sampling instants cannot answer it, however many are taken. America/Boise
// and America/Denver agree on the first of January and the first of July in
// every year from 1970 to 2050, and disagree through most of January 1974,
// when Denver took up the emergency daylight saving three weeks before Boise
// did. Narrowing to a range of years cannot answer it either: Africa/Abidjan
// and GMT agree from 1970 onwards and differ by sixteen minutes of local mean
// time in 1900, and a QVD reaches 1900 easily, its own serial epoch being
// 1899-12-30.
//
// So the walk covers every transition over the entire range a conversion
// accepts. ZoneBounds hands back the end of the period holding an instant,
// which is the next transition exactly, so stepping from one to the next
// enumerates the zone's whole rule set with nothing in between and nothing
// outside. It is not expensive: the table stops at the last real transition
// rather than projecting a rule forward for ever, so a busy zone is under two
// hundred steps and UTC is one.
func zoneIdentity(loc *time.Location) string {
	h := sha256.New()
	at := time.UnixMicro(-qvd.MaxTimestampMicros).UTC()
	end := time.UnixMicro(qvd.MaxTimestampMicros).UTC()
	for i := 0; at.Before(end) && i < zoneTransitionLimit; i++ {
		local := at.In(loc)
		name, offset := local.Zone()
		fmt.Fprint(h, tagged(strconv.FormatInt(at.Unix(), 10)),
			tagged(name), tagged(strconv.Itoa(offset)))

		_, next := local.ZoneBounds()
		if next.IsZero() || !next.After(at) {
			// The last period, which for UTC and any fixed offset is the only
			// one. What the zone does is now fully described.
			break
		}
		at = next
	}
	return "zone:" + hex.EncodeToString(h.Sum(nil))
}

// fingerprintValue renders one option as text. An unsupported kind is an error
// rather than a silently stable value, so a future option of a shape this does
// not understand fails the test that walks a fully populated Options rather
// than quietly dropping out of every fingerprint.
func fingerprintValue(v reflect.Value) (string, error) {
	switch v.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return fmt.Sprintf("%v", v.Interface()), nil

	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return "nil", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "[%d]", v.Len())
		for i := 0; i < v.Len(); i++ {
			s, err := fingerprintValue(v.Index(i))
			if err != nil {
				return "", err
			}
			b.WriteString(tagged(s))
		}
		return b.String(), nil

	case reflect.Map:
		if v.IsNil() {
			return "nil", nil
		}
		parts := make([]string, 0, v.Len())
		for _, k := range v.MapKeys() {
			key, err := fingerprintValue(k)
			if err != nil {
				return "", err
			}
			val, err := fingerprintValue(v.MapIndex(k))
			if err != nil {
				return "", err
			}
			parts = append(parts, tagged(key)+tagged(val))
		}
		sort.Strings(parts) // map order is not a property of the options
		return "{" + strings.Join(parts, "") + "}", nil

	case reflect.Struct:
		var b strings.Builder
		b.WriteString("{")
		for i := 0; i < v.NumField(); i++ {
			s, err := fingerprintValue(v.Field(i))
			if err != nil {
				return "", err
			}
			b.WriteString(tagged(v.Type().Field(i).Name) + tagged(s))
		}
		b.WriteString("}")
		return b.String(), nil

	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return "nil", nil
		}
		// A timezone is fingerprinted by what it does rather than what it is
		// called, for the reason in zoneIdentity.
		if loc, ok := v.Interface().(*time.Location); ok {
			return zoneIdentity(loc), nil
		}
		// A compiled regexp describes itself; anything else is walked as the
		// struct it points at. The pointer's own address never enters, or
		// nothing would ever match twice.
		if s, ok := v.Interface().(fmt.Stringer); ok {
			return s.String(), nil
		}
		return fingerprintValue(v.Elem())
	}
	return "", fmt.Errorf("no stable rendering for a %s", v.Kind())
}
