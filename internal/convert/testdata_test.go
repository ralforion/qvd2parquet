package convert

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// committedFixtures are the synthetic QVD files checked into testdata/. They
// are produced by internal/qvdtest/cmd/genfixture and cover every resolved
// output type, so they give the converter a stable regression target that does
// not depend on any private data.
var committedFixtures = []struct {
	file string
	rows int64
}{
	{"sample-small.qvd", 1000},
}

func TestCommittedFixtures(t *testing.T) {
	for _, fx := range committedFixtures {
		t.Run(fx.file, func(t *testing.T) {
			in := filepath.Join("..", "..", "testdata", fx.file)
			if _, err := os.Stat(in); err != nil {
				t.Fatalf("fixture missing: %v", err)
			}
			dir := t.TempDir()
			out := filepath.Join(dir, "out.parquet")

			opts := testOptions()
			opts.Quality = QualityFull
			opts.SchemaReportPath = filepath.Join(dir, "schema.json")

			stats, report, err := Run(context.Background(), in, out, &opts, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if stats.Rows != fx.rows {
				t.Errorf("rows = %d, want %d", stats.Rows, fx.rows)
			}
			if !report.Passed {
				t.Fatalf("full quality gate failed: %+v", report)
			}

			// The fixture covers every strategy the resolver can produce.
			want := map[string]string{
				"Id":     "int64",
				"Name":   "utf8",
				"Amount": "decimal(7, 2)",
				"Day":    "date32",
				"Ratio":  "decimal(3, 1)", // decimal by default
				"Clock":  "time32[ms]",
			}
			got := map[string]string{}
			for _, c := range report.Columns {
				got[c.Name] = c.Type
			}
			for name, wantType := range want {
				if got[name] != wantType {
					t.Errorf("column %q type = %q, want %q", name, got[name], wantType)
				}
			}
		})
	}
}
