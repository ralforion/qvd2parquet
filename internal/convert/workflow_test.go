package convert

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestCIMatrixCoversEveryReleaseTarget keeps the CI build matrix and the
// release script's platform list identical. They are declared in two files, so
// a target added to one and not the other would either ship untested or be
// tested but never released -- both silent.
func TestCIMatrixCoversEveryReleaseTarget(t *testing.T) {
	ci := ciMatrixTargets(t, readTextFile(t, ".github", "workflows", "ci.yml"))
	rel := ciMatrixTargets(t, readTextFile(t, ".github", "workflows", "release.yml"))
	script := releaseScriptTargets(t, readTextFile(t, "scripts", "build-release.sh"))

	if len(ci) == 0 || len(rel) == 0 || len(script) == 0 {
		t.Fatalf("parsed %d ci, %d release and %d script targets; the parsing is wrong",
			len(ci), len(rel), len(script))
	}
	for _, other := range []struct {
		name    string
		targets []string
	}{{"release workflow", rel}, {"build-release.sh", script}} {
		if strings.Join(ci, ",") != strings.Join(other.targets, ",") {
			t.Errorf("CI matrix and %s list different targets:\n  ci:  %v\n  %s: %v",
				other.name, ci, other.name, other.targets)
		}
	}
}

// ciMatrixTargets pulls the "target:" list out of the build job's matrix.
func ciMatrixTargets(t *testing.T, ci string) []string {
	t.Helper()
	i := strings.Index(ci, "        target:")
	if i < 0 {
		t.Fatal("no matrix target list in ci.yml")
	}
	var out []string
	for _, line := range strings.Split(ci[i:], "\n")[1:] {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") {
			break
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
	}
	sort.Strings(out)
	return out
}

// releaseScriptTargets pulls the DEFAULT_PLATFORMS list out of the script.
func releaseScriptTargets(t *testing.T, script string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?s)DEFAULT_PLATFORMS="(.*?)"`).FindStringSubmatch(script)
	if m == nil {
		t.Fatal("no DEFAULT_PLATFORMS in build-release.sh")
	}
	var out []string
	for _, f := range strings.Fields(m[1]) {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// TestCIGateDependsOnTheMatrix guards the arrangement that lets branch
// protection require one stable check name: the gate job must fail when any
// matrix job does, rather than passing because it merely ran.
func TestCIGateDependsOnTheMatrix(t *testing.T) {
	ci := readTextFile(t, ".github", "workflows", "ci.yml")

	i := strings.Index(ci, "  cross-compile:")
	if i < 0 {
		t.Fatal("no cross-compile job; branch protection requires that check name")
	}
	gate := ci[i:]
	for _, want := range []string{"needs: build", "if: always()", "needs.build.result"} {
		if !strings.Contains(gate, want) {
			t.Errorf("the cross-compile gate should contain %q, or a failing matrix job "+
				"would not fail the required check", want)
		}
	}
}
