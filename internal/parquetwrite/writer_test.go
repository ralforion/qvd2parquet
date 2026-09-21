package parquetwrite

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/file"
)

// The footer grows with every row group, and FooterBytes reports what was
// actually written rather than an estimate of it.
func TestFooterBytesGrowsWithRowGroups(t *testing.T) {
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "KNUMV", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "KPOSN", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}, nil)

	footers := make(map[int64]int64)
	for _, rowGroupRows := range []int64{100, 1000} {
		path := filepath.Join(t.TempDir(), "out.parquet")
		w, err := Create(path, schema, Options{Compression: compress.Codecs.Zstd, RowGroupRows: rowGroupRows}, false)
		if err != nil {
			t.Fatal(err)
		}
		if w.FooterBytes() != 0 {
			t.Errorf("FooterBytes before Close = %d, want 0", w.FooterBytes())
		}
		b := array.NewRecordBuilder(DefaultAllocator, schema)
		for r := 0; r < 1000; r++ {
			b.Field(0).(*array.StringBuilder).Append("0000012345")
			b.Field(1).(*array.Int64Builder).Append(int64(r))
		}
		rec := b.NewRecord()
		err = w.Write(rec)
		rec.Release()
		b.Release()
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := w.Commit(); err != nil {
			t.Fatal(err)
		}

		rdr, err := file.OpenParquetFile(path, false)
		if err != nil {
			t.Fatal(err)
		}
		groups := int64(rdr.NumRowGroups())
		rdr.Close()
		if want := 1000 / rowGroupRows; groups != want {
			t.Errorf("%d rows per row group wrote %d row groups, want %d", rowGroupRows, groups, want)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := w.FooterBytes(); got <= 0 || got >= fi.Size() {
			t.Errorf("FooterBytes = %d on a file of %d bytes", got, fi.Size())
		}
		footers[rowGroupRows] = w.FooterBytes()
	}
	if footers[100] <= footers[1000] {
		t.Errorf("ten row groups wrote a footer of %d bytes, one wrote %d: expected the first to be larger",
			footers[100], footers[1000])
	}
}
