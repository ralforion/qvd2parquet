package convert

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FolderSchemaName is the schema override a QVD's own folder may hold. A tree
// of table folders converts in one run, and one --schema cannot pin the same
// column differently per table, so each folder can carry its own.
//
// It is looked up next to the input rather than the output: the output folder
// is the one a query engine promotes as a dataset, and a stray JSON file there
// is read as data.
const FolderSchemaName = "qvd2parquet-schema.json"

// folderSchema is one folder's schema as read. Path is empty when the folder
// holds none, and Err is kept rather than retried, so every file of a folder
// with a broken schema fails the same way.
type folderSchema struct {
	Path     string
	Override *SchemaOverride
	Hash     string
	Err      error
}

// FolderSchemas reads each folder's schema once and keeps it for the run. A
// folder of daily deltas holds hundreds of QVDs, and both the up-to-date check
// and the conversion want the schema of each, so without it the same file is
// read and parsed twice per input. Holding one reading also means the check
// and the conversion cannot see two different versions of a file edited
// mid-run. Safe for concurrent use.
type FolderSchemas struct {
	mu   sync.Mutex
	dirs map[string]*folderSchema
}

// NewFolderSchemas starts an empty cache.
func NewFolderSchemas() *FolderSchemas {
	return &FolderSchemas{dirs: map[string]*folderSchema{}}
}

// For is the schema of the input's folder. A nil cache reads it every time.
func (c *FolderSchemas) For(input string) *folderSchema {
	dir := filepath.Dir(input)
	if c == nil {
		return readFolderSchema(dir)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fs, ok := c.dirs[dir]
	if !ok {
		fs = readFolderSchema(dir)
		c.dirs[dir] = fs
	}
	return fs
}

// readFolderSchema loads a folder's schema. Only a file that is not there
// counts as none: one that cannot be read is an error, so the folder fails
// rather than converting without the pins it was given.
func readFolderSchema(dir string) *folderSchema {
	p := filepath.Join(dir, FolderSchemaName)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &folderSchema{}
	}
	if err != nil {
		return &folderSchema{Path: p, Err: fmt.Errorf("read schema override %s: %w", p, err)}
	}
	so, err := ParseSchemaOverride(p, b)
	if err != nil {
		// A pin that fails validation is reported by column alone, which is
		// enough for the one --schema of a run but not for one file among
		// many folders.
		if !strings.Contains(err.Error(), p) {
			err = fmt.Errorf("%s: %w", p, err)
		}
		return &folderSchema{Path: p, Err: err}
	}
	for name, co := range so.Columns {
		co.from = p
		so.Columns[name] = co
	}
	return &folderSchema{Path: p, Override: so, Hash: fmt.Sprintf("%x", sha256.Sum256(b))}
}

// LoadOverrides combines --schema with the input's folder schema. A column
// pinned in both takes the folder's pin, being the more specific of the two,
// and the path of the folder schema is returned so the run can say it used it.
func LoadOverrides(input string, opts *Options) (so *SchemaOverride, folder string, err error) {
	if opts.SchemaOverridePath != "" {
		if so, err = LoadSchemaOverride(opts.SchemaOverridePath); err != nil {
			return nil, "", err
		}
	}
	local := opts.FolderSchemas.For(input)
	if local.Err != nil {
		return nil, "", local.Err
	}
	if local.Override == nil {
		return so, "", nil
	}
	if so == nil {
		return local.Override, local.Path, nil
	}
	merged := &SchemaOverride{Columns: make(map[string]ColumnOverride, len(so.Columns)+len(local.Override.Columns))}
	for name, co := range so.Columns {
		merged.Columns[name] = co
	}
	for name, co := range local.Override.Columns {
		// lookup matches names case-insensitively, so a global pin spelled
		// with other capitals would otherwise survive beside the folder's
		// and win or lose on map order.
		for g := range merged.Columns {
			if strings.EqualFold(g, name) {
				delete(merged.Columns, g)
			}
		}
		merged.Columns[name] = co
	}
	return merged, local.Path, nil
}

// folderFingerprint extends the run's fingerprint with the input's folder
// schema. A folder without one keeps the run's fingerprint unchanged, so
// adding this lookup reconverts nothing that has no schema file, and adding a
// schema file reconverts that folder alone.
func folderFingerprint(run string, c *FolderSchemas, input string) (string, error) {
	local := c.For(input)
	if local.Err != nil {
		return "", local.Err
	}
	if local.Hash == "" {
		return run, nil
	}
	h := sha256.New()
	fmt.Fprint(h, tagged(run), tagged("folderSchema"), tagged(local.Hash))
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
