package convert

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readTextFile reads a repository text file with line endings normalized.
// Windows checkouts use CRLF, which would otherwise leave a stray \r on every
// line and break the comparisons below.
func readTextFile(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// TestChangelogCoversCurrentVersion keeps CHANGELOG.md and the version the CLI
// reports from drifting apart. The release workflow builds its notes from the
// changelog and fails when the section is missing, so catching it here turns a
// failed release into a failed test.
func TestChangelogCoversCurrentVersion(t *testing.T) {
	changelog := readTextFile(t, "CHANGELOG.md")
	main := readTextFile(t, "cmd", "qvd2parquet", "main.go")

	m := regexp.MustCompile(`defaultVersion = "([^"]+)"`).FindStringSubmatch(main)
	if m == nil {
		t.Fatal("could not find defaultVersion in main.go")
	}
	version := m[1]

	if !strings.Contains(changelog, "## ["+version+"]") {
		t.Errorf("CHANGELOG.md has no '## [%s]' section; the release workflow "+
			"builds its notes from the changelog and would fail on that tag", version)
	}
}

// TestChangelogSectionsAreOrdered catches a section added in the wrong place,
// which would make the extraction script return the wrong notes.
func TestChangelogSectionsAreOrdered(t *testing.T) {
	b := readTextFile(t, "CHANGELOG.md")
	headings := regexp.MustCompile(`(?m)^## \[([^\]]+)\]`).FindAllStringSubmatch(b, -1)
	if len(headings) < 2 {
		t.Fatalf("expected several sections, found %d", len(headings))
	}
	if headings[0][1] != "Unreleased" {
		t.Errorf("first section is %q, want Unreleased", headings[0][1])
	}
	// Every released section needs a link definition at the bottom.
	for _, h := range headings {
		if !strings.Contains(b, "\n["+h[1]+"]: https://") {
			t.Errorf("section %q has no link definition", h[1])
		}
	}
}

// TestReadmeBannerMatchesCode keeps the banner shown in the README in step
// with the constants the CLI actually prints.
func TestReadmeBannerMatchesCode(t *testing.T) {
	main := readTextFile(t, "cmd", "qvd2parquet", "main.go")
	readme := readTextFile(t, "README.md")

	get := func(name string) string {
		m := regexp.MustCompile(name + `\s+= "([^"]+)"`).FindStringSubmatch(main)
		if m == nil {
			t.Fatalf("could not find %s in main.go", name)
		}
		return m[1]
	}
	version, copyright := get("defaultVersion"), get("copyright")
	want := "qvd2parquet " + version + "  " + copyright

	for _, line := range strings.Split(readme, "\n") {
		if strings.HasPrefix(line, "qvd2parquet ") && strings.Contains(line, "RALFORION") {
			if line != want {
				t.Errorf("README banner line %q does not match the code's %q", line, want)
			}
		}
	}
}
