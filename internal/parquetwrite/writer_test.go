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

// A share that refuses to overwrite a file, as an S3 bucket mounted as a
// Windows drive does, still gets the new output under --force: the old file is
// deleted and the rename retried. Without --force nothing is deleted.
func TestCommitReplacesWhenOverwriteIsRefused(t *testing.T) {
	saved := rename
	rename = func(from, to string) error {
		if _, err := os.Stat(to); err == nil {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrPermission}
		}
		return saved(from, to)
	}
	defer func() { rename = saved }()

	schema := arrow.NewSchema([]arrow.Field{{Name: "A", Type: arrow.PrimitiveTypes.Int64}}, nil)
	finish := func(path string, force bool) error {
		w, err := Create(path, schema, Options{Compression: compress.Codecs.Zstd, RowGroupRows: 10}, force)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return w.Commit()
	}

	path := filepath.Join(t.TempDir(), "out.parquet")
	if err := os.WriteFile(path, []byte("old output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := finish(path, true); err != nil {
		t.Fatalf("Commit with force: %v", err)
	}
	if _, err := FooterBytes(path); err != nil {
		t.Errorf("the output was not replaced by the new Parquet file: %v", err)
	}

	// Create checks for an existing file without force, so this writer only
	// meets one that appeared while it was converting.
	path = filepath.Join(t.TempDir(), "out.parquet")
	w, err := Create(path, schema, Options{Compression: compress.Codecs.Zstd, RowGroupRows: 10}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(); err == nil {
		t.Error("Commit without force replaced a file that appeared during the conversion")
	}
	w.Abort()
	if b, _ := os.ReadFile(path); string(b) != "old output" {
		t.Errorf("Commit without force changed the existing file to %q", b)
	}
}
