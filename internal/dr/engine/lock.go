package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

// lockInfo is what a lock file says about its owner.
type lockInfo struct {
	PID   int       `json:"pid"`
	Host  string    `json:"host"`
	RunID string    `json:"runId"`
	Time  time.Time `json:"time"`
}

// corruptLockGrace is how long an unreadable lock file is trusted to be a writer in the middle of
// creating it. After that it is stale.
const corruptLockGrace = 10 * time.Second

func (e *Engine) lockPath(contextName string) string {
	return filepath.Join(e.dir, "locks", escapeName(contextName)+".lock")
}

// acquireLock takes the per-context run lock. It uses an exclusive create, so only one process can
// hold it. A lock whose owner is gone (same host, process dead) or that is older than LockMaxAge
// is stale and is taken over. The returned function releases it.
func (e *Engine) acquireLock(contextName, runID string) (func(), error) {
	target := e.lockPath(contextName)
	if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
		return nil, fmt.Errorf("cannot create the lock directory: %w", err)
	}

	mine := lockInfo{PID: e.opts.Pid(), Host: e.host, RunID: runID, Time: e.opts.Now()}
	body, err := json.Marshal(mine)
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < 3; attempt++ {
		// #nosec G304 -- the lock path is under the engine directory with an escaped name.
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
		if err == nil {
			_, werr := f.Write(body)
			if werr == nil {
				werr = f.Sync()
			}
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				_ = os.Remove(target)
				return nil, fmt.Errorf("cannot write the lock: %w", werr)
			}
			return func() { e.release(target, runID) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("cannot take the run lock: %w", err)
		}

		held, stale := e.inspectLock(target)
		if !stale {
			return nil, fmt.Errorf("%w (context %q, run %s, pid %d on %s, since %s)",
				dr.ErrRunInProgress, contextName, held.RunID, held.PID, held.Host, held.Time.Format(time.RFC3339))
		}
		// Re-read just before removing so a lock that was refreshed in the meantime survives.
		if again, _ := readLock(target); again == held {
			_ = os.Remove(target)
		}
	}
	return nil, fmt.Errorf("%w (context %q): the lock kept changing", dr.ErrRunInProgress, contextName)
}

// inspectLock returns what the lock file says and whether it is stale.
func (e *Engine) inspectLock(target string) (lockInfo, bool) {
	info, err := readLock(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return lockInfo{}, true // released while we looked
		}
		// Unreadable: a writer may be midway through creating it.
		st, serr := os.Stat(target)
		if serr != nil {
			return lockInfo{}, true
		}
		return lockInfo{}, e.opts.Now().Sub(st.ModTime()) > corruptLockGrace
	}
	if e.opts.Now().Sub(info.Time) > e.opts.LockMaxAge {
		return info, true
	}
	if info.Host == e.host && !e.opts.ProcessAlive(info.PID) {
		return info, true
	}
	return info, false
}

func readLock(target string) (lockInfo, error) {
	// #nosec G304 -- the lock path is under the engine directory with an escaped name.
	data, err := os.ReadFile(target)
	if err != nil {
		return lockInfo{}, err
	}
	var info lockInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return lockInfo{}, err
	}
	return info, nil
}

// release removes the lock only if this run still owns it.
func (e *Engine) release(target, runID string) {
	if info, err := readLock(target); err == nil && info.RunID != runID {
		return
	}
	_ = os.Remove(target)
}
