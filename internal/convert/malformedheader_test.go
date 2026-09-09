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

// A header carrying an unescaped '<' or '&' in a field name is not well-formed
// XML. The file still has to convert, and the column has to keep the name the
// file states.
func TestMalformedHeaderConverts(t *testing.T) {
	const name = "AFRU-||-ABARB-||-Ist <Soll (Abw.)"
	tbl := qvdtest.Table{Name: "AFRU", Fields: []qvdtest.Field{
		{Name: name, Type: "ASCII", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Str("a"), qvdtest.Str("b")}},
		{Name: "R&D", Type: "INTEGER", Rows: []int{0, 1},
			Symbols: []qvd.Symbol{qvdtest.Int(1), qvdtest.Int(2)}},
	}}
	in := buildFixture(t, tbl)
	out := filepath.Join(t.TempDir(), "out.parquet")

	var lines []string
	opts := testOptions()
	stats, _, err := Run(context.Background(), in, out, &opts,
		func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Rows != 2 {
		t.Errorf("rows = %d, want 2", stats.Rows)
	}

	schema, _ := readParquet(t, out)
	for _, want := range []string{name, "R&D"} {
		if len(schema.FieldIndices(want)) == 0 {
			t.Errorf("column %q missing from %v", want, schema.Fields())
		}
	}
	// The warning has to name what was wrong, not just that something was.
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "malformed XML header") || !strings.Contains(joined, "line ") {
		t.Errorf("the malformed header was not reported with its cause: %v", lines)
	}
}
