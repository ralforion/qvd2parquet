package convert

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ralforion/qvd2parquet/internal/qvd"
	"github.com/ralforion/qvd2parquet/internal/qvdtest"
)

// Every per-file note has to come after the line that says which file it is
// about. Printed before it, a note lands under whatever the batch last said,
// and after a failed file it reads as though the failure produced it.
func TestPerFileNotesFollowTheFileLine(t *testing.T) {
	tbl := qvdtest.Table{Name: "T", Fields: []qvdtest.Field{
		{Name: "Keep", Type: "ASCII", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Str("a"), qvdtest.Str("b")}},
		{Name: "Counter", Type: "INTEGER", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Int(1), qvdtest.Int(2)}},
	}}
	in := buildFixture(t, tbl)
	out := filepath.Join(t.TempDir(), "out.parquet")

	opts := testOptions()
	opts.Exclude = []string{"Counter", "NoSuchField"}

	var lines []string
	if _, _, err := Run(context.Background(), in, out, &opts,
		func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }); err != nil {
		t.Fatalf("Run: %v", err)
	}

	indexOf := func(substr string) int {
		for i, l := range lines {
			if strings.Contains(l, substr) {
				return i
			}
		}
		t.Fatalf("no log line contains %q: %v", substr, lines)
		return -1
	}
	file := indexOf("table \"T\"")
	if got := indexOf("excluded 1 column(s)"); got < file {
		t.Errorf("the exclusion note (line %d) came before the file line (line %d)", got, file)
	}
	if got := indexOf("matched no field"); got < file {
		t.Errorf("the unmatched-pattern note (line %d) came before the file line (line %d)", got, file)
	}
}
