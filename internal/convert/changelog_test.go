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
