package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildCLI compiles the command once for the tests that need to run it as a
// process: signal handling and exit codes are only observable from outside.
func buildCLI(t *testing.T) string {
	t.Helper()
	name := "qvd2parquet"
	if runtime.GOOS == "windows" {
		// Windows will not execute a file without the extension.
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// A successful run must not announce a cancellation. The signal handler used
// to wait on the run's context, which the deferred cleanup also cancels, so
// nearly every successful conversion printed "cancelling" on its way out.
func TestSuccessfulRunDoesNotAnnounceCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := buildCLI(t)
	dir := t.TempDir()
	in := filepath.Join("..", "..", "testdata", "sample-small.qvd")
	if _, err := os.Stat(in); err != nil {
		t.Fatalf("fixture missing, this test would otherwise pass by skipping: %v", err)
	}

	// Repeated, because the bug was a race: it appeared on most runs but not
	// all, and once per test would have been a coin flip.
	for i := 0; i < 20; i++ {
		out := filepath.Join(dir, "out.parquet")
		cmd := exec.Command(bin, "--force", "--progress", "0",
			"--quality-gate", "none", in, out)
		combined, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run %d failed: %v\n%s", i, err, combined)
		}
		if strings.Contains(string(combined), "cancelling") {
			t.Fatalf("run %d announced a cancellation on a successful run:\n%s", i, combined)
		}
	}
}
