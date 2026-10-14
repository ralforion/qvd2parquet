package parquetwrite

import (
	"errors"
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

// A file that cannot be opened for writing but can be deleted is how an S3
// bucket mounted as a Windows drive treats the log a previous run left. A
// read-only file in a writable directory refuses the same way, on every
// platform, so CreateFile has to delete it and start a new one.
func TestCreateFileReplacesAFileThatRefusesWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	if err := os.WriteFile(path, []byte("previous run\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Create(path); err == nil {
		f.Close()
		t.Skip("the file opened for writing despite its mode; running as root?")
	}

	if err := WriteFile(path, []byte("this run\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "this run\n" {
		t.Fatalf("file holds %q, want the new contents only", got)
	}
}

// Only a regular file is deleted to make room: a path that cannot be created
// for another reason keeps its original error and is left alone.
func TestCreateFileLeavesADirectoryAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if f, err := CreateFile(path, 0o644); err == nil {
		f.Close()
		t.Fatal("CreateFile succeeded on a directory")
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Fatalf("the directory was disturbed: %v", err)
	}
}

// A create that fails for any reason other than a refusal leaves the old file
// where it is. Deleting it and then failing the second create too, as running
// out of file descriptors would, loses the previous run's log for nothing.
func TestCreateFileKeepsTheFileOnOtherFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	if err := os.WriteFile(path, []byte("previous run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := openFile
	defer func() { openFile = saved }()
	openFile = func(string, int, os.FileMode) (*os.File, error) {
		return nil, &os.PathError{Op: "open", Path: path, Err: errors.New("too many open files")}
	}

	if err := WriteFile(path, []byte("this run\n"), 0o644); err == nil {
		t.Fatal("WriteFile succeeded with every open failing")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the previous file is gone: %v", err)
	}
	if string(got) != "previous run\n" {
		t.Fatalf("file holds %q, want the previous contents", got)
	}
}
