package convert

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LogWriter records one JSON object per line: a "file" record for each input
// and a final "summary". JSON Lines is used so a finished run can be queried
// directly -- DuckDB's read_json_auto, jq, pandas -- rather than parsed out of
// prose, which is the point of keeping a log at all.
type LogWriter struct {
	mu   sync.Mutex
	f    *os.File
	enc  *json.Encoder
	path string
}

// NewLogWriter creates or truncates the log file.
func NewLogWriter(path string) (*LogWriter, error) {
	// The log commonly sits inside --out-dir, which may not exist yet.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create log directory %s: %w", dir, err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create log %s: %w", path, err)
	}
	return &LogWriter{f: f, enc: json.NewEncoder(f), path: path}, nil
}

// Close flushes and closes the log.
func (w *LogWriter) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	return w.f.Close()
}

// Path is where the log is being written.
func (w *LogWriter) Path() string {
	if w == nil {
		return ""
	}
	return w.path
}

// fileRecord is one converted input.
//
// Every field is emitted on every record, including the empty ones. Omitting
// them would make the column absent whenever no record happened to carry it,
// so `where status='failed'` would fail to bind on a run with no failures --
// the monitoring query breaking exactly when everything worked. A stable
// schema is the point of writing a machine-readable log.
type fileRecord struct {
	Type      string `json:"type"`
	Time      string `json:"time"`
	Input     string `json:"input"`
	Output    string `json:"output"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	ElapsedMs int64  `json:"elapsedMs"`

	Table       string `json:"table"`
	Rows        int64  `json:"rows"`
	Columns     int    `json:"columns"`
	OutputBytes int64  `json:"outputBytes"`
	SymbolsRead int64  `json:"symbolsRead"`
	RowsPerSec  int64  `json:"rowsPerSecond"`

	// Quality carries the gate's verdict when one ran, so a batch can be
	// audited without opening every per-file report. QualityPassed is null
	// when no gate ran, which is distinct from a gate that failed.
	// DecimalsNearLimit names decimal columns whose widest value already fills
	// most of the type's range. The full per-column detail belongs in
	// --schema-report; a record here is one line per file and stays that way,
	// so it carries only the signal a scheduled run would want to alert on.
	DecimalsNearLimit []string `json:"decimalsNearLimit"`

	QualityMode   string   `json:"qualityMode"`
	QualityPassed *bool    `json:"qualityPassed"`
	QualityErrors []string `json:"qualityErrors"`
}

// File appends a record for one input.
func (w *LogWriter) File(r FileResult) {
	if w == nil {
		return
	}
	rec := fileRecord{
		Type: "file",
		// Both start empty rather than absent: a record's key set has to be
		// the same on every line, or a query binding the field fails on the
		// clean runs.
		QualityErrors:     []string{},
		DecimalsNearLimit: []string{},
		Time:              r.Started.UTC().Format(time.RFC3339Nano),
		Input:             r.Input,
		Output:            r.Output,
		Status:            "ok",
		ElapsedMs:         r.Elapsed.Milliseconds(),
	}
	switch {
	case r.Err != nil:
		rec.Status, rec.Error, rec.Output = "failed", r.Err.Error(), ""
	case r.Skipped:
		rec.Status = "skipped"
	}
	if r.Stats != nil {
		rec.Rows = r.Stats.Rows
		rec.Columns = r.Stats.Columns
		rec.OutputBytes = r.Stats.OutputBytes
		rec.SymbolsRead = r.Stats.SymbolsRead
		rec.RowsPerSec = int64(r.Stats.RowsPerSecond())
		if len(r.Stats.DecimalsNearLimit) > 0 {
			rec.DecimalsNearLimit = r.Stats.DecimalsNearLimit
		}
	}
	if r.Quality != nil {
		passed := r.Quality.Passed
		rec.QualityMode, rec.QualityPassed = r.Quality.Mode, &passed
		rec.QualityErrors = append(rec.QualityErrors, r.Quality.Errors...)
		for _, c := range r.Quality.Columns {
			for _, e := range c.Errors {
				rec.QualityErrors = append(rec.QualityErrors, c.Name+": "+e)
			}
		}
	}
	w.write(rec)
}

// summaryRecord closes the log with the run's totals.
type summaryRecord struct {
	Type        string `json:"type"`
	Time        string `json:"time"`
	Files       int    `json:"files"`
	Converted   int    `json:"converted"`
	Failed      int    `json:"failed"`
	Skipped     int    `json:"skipped"`
	Rows        int64  `json:"rows"`
	OutputBytes int64  `json:"outputBytes"`
	ElapsedMs   int64  `json:"elapsedMs"`
}

// Summary appends the closing record.
func (w *LogWriter) Summary(b *BatchResult) {
	if w == nil {
		return
	}
	w.write(summaryRecord{
		Type:        "summary",
		Time:        time.Now().UTC().Format(time.RFC3339Nano),
		Files:       len(b.Results),
		Converted:   b.Converted,
		Failed:      b.Failed,
		Skipped:     b.Skipped,
		Rows:        b.Rows,
		OutputBytes: b.Bytes,
		ElapsedMs:   b.Elapsed.Milliseconds(),
	})
}

// write serializes one record. Files convert concurrently, so the encoder is
// guarded; a log write failure must not abort a conversion that succeeded, so
// it is reported to stderr and the run continues.
func (w *LogWriter) write(v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "qvd2parquet: write log %s: %v\n", w.path, err)
	}
}
