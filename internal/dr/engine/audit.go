package engine

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/1985epma/k8scli/internal/dr"
)

const (
	auditFile = "audit.jsonl"
	// maxAuditRead bounds how much of the log Audit reads (the newest part).
	maxAuditRead = 8 << 20
	// maxAuditLine bounds one record when reading.
	maxAuditLine = 1 << 20
)

// AuditRecord is one line of audit.jsonl: the event plus enough context to read it on its own.
type AuditRecord struct {
	dr.Event
	Plan      string       `json:"plan,omitempty"`
	Context   string       `json:"context,omitempty"`
	Direction dr.Direction `json:"direction,omitempty"`
	// PlanHash and ScopeHash identify exactly what was run (sha256 of the plan and of the scope).
	PlanHash  string `json:"planHash,omitempty"`
	ScopeHash string `json:"scopeHash,omitempty"`
}

func (e *Engine) auditPath() string { return filepath.Join(e.dir, auditFile) }

// appendAudit adds one record. The file is only ever appended to, each record is written with a
// single write and flushed to disk, so a crash cannot leave a record half-written in the middle.
func (e *Engine) appendAudit(rec AuditRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	e.auditMu.Lock()
	defer e.auditMu.Unlock()
	if err := os.MkdirAll(e.dir, dirMode); err != nil {
		return err
	}
	// #nosec G304 -- the path is under the engine directory.
	f, err := os.OpenFile(e.auditPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Audit returns the newest records, oldest first, at most limit (limit <= 0 means 200). Lines that
// do not parse are skipped.
func (e *Engine) Audit(_ context.Context, limit int) ([]AuditRecord, error) {
	if limit <= 0 {
		limit = 200
	}
	e.auditMu.Lock()
	defer e.auditMu.Unlock()

	// #nosec G304 -- the path is under the engine directory.
	f, err := os.Open(e.auditPath())
	if errors.Is(err, os.ErrNotExist) {
		return []AuditRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	trimmed := false
	if info.Size() > maxAuditRead {
		if _, err := f.Seek(info.Size()-maxAuditRead, io.SeekStart); err != nil {
			return nil, err
		}
		trimmed = true
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), maxAuditLine)
	var records []AuditRecord
	first := true
	for scanner.Scan() {
		if first && trimmed { // the first line of a trimmed read is usually cut in half
			first = false
			continue
		}
		first = false
		var rec AuditRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return nil, err
	}
	if len(records) > limit {
		records = records[len(records)-limit:]
	}
	if records == nil {
		records = []AuditRecord{}
	}
	return records, nil
}

// hashJSON returns the sha256 of the canonical JSON of v.
func hashJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// scopeHash hashes a scope independently of the order its lists were written in.
func scopeHash(scope dr.Scope) string {
	sorted := func(in []string) []string {
		out := append([]string{}, in...)
		sort.Strings(out)
		return out
	}
	phases := make([]string, len(scope.Phases))
	for i, p := range scope.Phases {
		phases[i] = string(p)
	}
	return hashJSON(struct {
		Apps      []string `json:"apps"`
		Databases []string `json:"databases"`
		Phases    []string `json:"phases"`
	}{sorted(scope.Apps), sorted(scope.Databases), sorted(phases)})
}
