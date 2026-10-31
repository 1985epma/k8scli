package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

// runIDPattern is the only shape of run id the engine creates and accepts, so an id from outside
// (a URL, a flag) can never name a path outside the ledger directory.
var runIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)

// defaultListLimit applies when List is called with limit <= 0.
const defaultListLimit = 100

// runSeq orders the ids created by this process. The timestamp has second precision, so two runs started in
// the same second would otherwise tie and sort by chance.
var runSeq atomic.Uint32

// newRunID returns a time-sortable id: 20261031T015900Z-0007a1b2. The shape is fixed (the web console
// validates it too): a UTC timestamp, then 4 hex digits of a per-process counter that grows with every id,
// then 4 random hex digits so ids from different processes in the same second do not collide.
func newRunID(now time.Time) (string, error) {
	random := make([]byte, 2)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("cannot generate a run id: %w", err)
	}
	seq := runSeq.Add(1) & 0xffff // 4 hex digits; wraps after 65536 ids, and ids in one second never get close
	return fmt.Sprintf("%s-%04x%s", now.UTC().Format("20060102T150405Z"), seq, hex.EncodeToString(random)), nil
}

func (e *Engine) runsDir() string { return filepath.Join(e.dir, "runs") }

func (e *Engine) ledgerPath(id string) string { return filepath.Join(e.runsDir(), id+".json") }

// writeLedger stores the run atomically. It is called after every step, so a crash at any moment
// leaves a ledger that Resume can continue from.
func (e *Engine) writeLedger(res *dr.RunResult) error {
	if !runIDPattern.MatchString(res.ID) {
		return fmt.Errorf("invalid run id %q", res.ID)
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(e.ledgerPath(res.ID), data)
}

// readLedger loads one run. The second result is false when there is no such run (or the id is not
// one the engine could have created).
func (e *Engine) readLedger(id string) (*dr.RunResult, bool, error) {
	if !runIDPattern.MatchString(id) {
		return nil, false, nil
	}
	// #nosec G304 -- id matches runIDPattern, so the path stays inside the runs directory.
	data, err := os.ReadFile(e.ledgerPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var res dr.RunResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, false, fmt.Errorf("the ledger of run %s is corrupt: %w", id, err)
	}
	if res.ID != id {
		return nil, false, fmt.Errorf("the ledger of run %s names another run (%q)", id, res.ID)
	}
	if res.Steps == nil {
		res.Steps = []dr.StepResult{}
	}
	return &res, true, nil
}

// Status returns a stored run.
func (e *Engine) Status(_ context.Context, id string) (*dr.RunResult, bool, error) {
	return e.readLedger(id)
}

// List returns stored runs, newest first. Files that cannot be read or parsed are skipped: one
// damaged ledger must not hide the others.
func (e *Engine) List(_ context.Context, limit int) ([]dr.RunResult, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	entries, err := os.ReadDir(e.runsDir())
	if errors.Is(err, os.ErrNotExist) {
		return []dr.RunResult{}, nil
	}
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if ok && !entry.IsDir() && runIDPattern.MatchString(id) {
			ids = append(ids, id)
		}
	}
	// Ids start with a UTC timestamp, so a descending sort is newest first, except among ids of the same
	// second. Take every id up to the limit plus those that share the second of the last one, then order
	// what was read by start time and id, which is exact.
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	const stamp = len("20060102T150405Z")
	end := min(limit, len(ids))
	for end < len(ids) && ids[end][:stamp] == ids[end-1][:stamp] {
		end++
	}

	found := make([]dr.RunResult, 0, end)
	for _, id := range ids[:end] {
		res, ok, err := e.readLedger(id)
		if err != nil || !ok {
			continue
		}
		found = append(found, *res)
	}
	sort.SliceStable(found, func(i, j int) bool {
		if !found[i].Started.Equal(found[j].Started) {
			return found[i].Started.After(found[j].Started)
		}
		return found[i].ID > found[j].ID
	})
	return found[:min(limit, len(found))], nil
}
