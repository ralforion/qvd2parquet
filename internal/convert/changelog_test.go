package convert

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestChangelogCoversCurrentVersion keeps CHANGELOG.md and the version the CLI
// reports from drifting apart. The release workflow builds its notes from the
// changelog and fails when the section is missing, so catching it here turns a
// failed release into a failed test.
func TestChangelogCoversCurrentVersion(t *testing.T) {
	root := filepath.Join("..", "..")
	changelog, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	main, err := os.ReadFile(filepath.Join(root, "cmd", "qvd2parquet", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	m := regexp.MustCompile(`defaultVersion = "([^"]+)"`).FindSubmatch(main)
	if m == nil {
		t.Fatal("could not find defaultVersion in main.go")
	}
	version := string(m[1])

	if !strings.Contains(string(changelog), "## ["+version+"]") {
		t.Errorf("CHANGELOG.md has no '## [%s]' section; the release workflow "+
			"builds its notes from the changelog and would fail on that tag", version)
	}
}

// TestChangelogSectionsAreOrdered catches a section added in the wrong place,
// which would make the extraction script return the wrong notes.
func TestChangelogSectionsAreOrdered(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	headings := regexp.MustCompile(`(?m)^## \[([^\]]+)\]`).FindAllStringSubmatch(string(b), -1)
	if len(headings) < 2 {
		t.Fatalf("expected several sections, found %d", len(headings))
	}
	if headings[0][1] != "Unreleased" {
		t.Errorf("first section is %q, want Unreleased", headings[0][1])
	}
	// Every released section needs a link definition at the bottom.
	for _, h := range headings {
		if !strings.Contains(string(b), "\n["+h[1]+"]: https://") {
			t.Errorf("section %q has no link definition", h[1])
		}
	}
}

// TestReadmeBannerMatchesCode keeps the banner shown in the README in step
// with the constants the CLI actually prints.
func TestReadmeBannerMatchesCode(t *testing.T) {
	root := filepath.Join("..", "..")
	main, err := os.ReadFile(filepath.Join(root, "cmd", "qvd2parquet", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}

	get := func(name string) string {
		m := regexp.MustCompile(name + `\s+= "([^"]+)"`).FindSubmatch(main)
		if m == nil {
			t.Fatalf("could not find %s in main.go", name)
		}
		return string(m[1])
	}
	version, copyright := get("defaultVersion"), get("copyright")
	want := "qvd2parquet " + version + "  " + copyright

	for _, line := range strings.Split(string(readme), "\n") {
		if strings.HasPrefix(line, "qvd2parquet ") && strings.Contains(line, "RALFORION") {
			if line != want {
				t.Errorf("README banner line %q does not match the code's %q", line, want)
			}
		}
	}
}
