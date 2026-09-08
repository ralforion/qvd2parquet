package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

// FindParquet expands the given files and directories into the .parquet files
// beneath them. A directory is read one level deep unless recursive is set,
// matching how --out-dir lays a batch out.
func FindParquet(paths []string, recursive bool) ([]string, error) {
	var found []string
	seen := map[string]bool{}
	add := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		found = append(found, p)
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", os.ErrNotExist, p)
		}
		if !info.IsDir() {
			add(p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != p && !recursive {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.EqualFold(filepath.Ext(path), ".parquet") {
				add(path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(found)
	return found, nil
}

// ScanFile reads one finished Parquet file's schema and returns a row per
// column.
//
// Only the footer is read, never a data page, so scanning a folder costs a
// seek and a few KiB per file however many rows it holds. The comment comes
// back because pqarrow decodes the ARROW:schema entry the writer stored; a
// file written by something else simply has no field metadata and yields rows
// with an empty comment rather than an error.
func ScanFile(path string) ([]Row, error) {
	rdr, err := file.OpenParquetFile(path, false)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer rdr.Close()

	fr, err := pqarrow.NewFileReader(rdr, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	schema, err := fr.Schema()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	rows := make([]Row, 0, len(schema.Fields()))
	for i, f := range schema.Fields() {
		md := f.Metadata
		comment, _ := lookup(md.Keys(), md.Values(), "comment")
		// The writer records the original QVD name only when it differs from
		// the output name, so its absence means the two are the same.
		source, ok := lookup(md.Keys(), md.Values(), "qvd.field")
		if !ok {
			source = f.Name
		}
		rows = append(rows, Row{
			Source:       SourceParquet,
			SourceFile:   path,
			SourceRows:   rdr.NumRows(),
			OutputFile:   path,
			Ordinal:      int32(i + 1),
			ColumnName:   f.Name,
			SourceColumn: source,
			Comment:      comment,
			ParquetType:  f.Type.String(),
			Nullable:     f.Nullable,
		})
	}
	return rows, nil
}

// lookup finds a key in Arrow's parallel metadata slices.
func lookup(keys, values []string, want string) (string, bool) {
	for i, k := range keys {
		if k == want && i < len(values) {
			return values[i], true
		}
	}
	return "", false
}
