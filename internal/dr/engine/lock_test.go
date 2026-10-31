package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

func writeLockFile(t *testing.T, e *testEngine, contextName string, info lockInfo) {
	t.Helper()
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(e.lockPath(contextName)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.lockPath(contextName), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runOnce(t *testing.T, e *testEngine) error {
	t.Helper()
	w := newWorld()
	_, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{w.planner(dr.PhaseApps, "a1")}, dr.RunOptions{})
	return err
}

func TestStaleLocks(t *testing.T) {
	for name, tc := range map[string]struct {
		alive   bool
		lock    func(e *testEngine) lockInfo
		corrupt string // raw content instead of JSON
		age     time.Duration
		wantErr bool
	}{
		"owner process is gone": {
			alive: false, lock: func(e *testEngine) lockInfo {
				return lockInfo{PID: 99999, Host: e.host, RunID: "x", Time: e.clock.Now()}
			},
		},
		"owner alive on this host": {
			alive: true, lock: func(e *testEngine) lockInfo {
				return lockInfo{PID: 99999, Host: e.host, RunID: "x", Time: e.clock.Now()}
			}, wantErr: true,
		},
		"another host, recent (pid cannot be checked)": {
			alive: false, lock: func(e *testEngine) lockInfo {
				return lockInfo{PID: 1, Host: "other-machine", RunID: "x", Time: e.clock.Now()}
			}, wantErr: true,
		},
		"another host, older than the max age": {
			alive: true, lock: func(e *testEngine) lockInfo {
				return lockInfo{PID: 1, Host: "other-machine", RunID: "x", Time: e.clock.Now().Add(-13 * time.Hour)}
			},
		},
		"alive but older than the max age": {
			alive: true, lock: func(e *testEngine) lockInfo {
				return lockInfo{PID: 99999, Host: e.host, RunID: "x", Time: e.clock.Now().Add(-13 * time.Hour)}
			},
		},
		"unreadable and fresh (a writer may be creating it)": {
			corrupt: "{not json", wantErr: true,
		},
		"unreadable and old": {
			corrupt: "{not json", age: time.Hour,
		},
		"empty file, old": {
			corrupt: "", age: time.Hour,
		},
	} {
		e := newTestEngine(t, func(o *Options) { o.ProcessAlive = func(int) bool { return tc.alive } })
		if tc.corrupt != "" || tc.lock == nil {
			if err := os.MkdirAll(filepath.Dir(e.lockPath("dev")), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(e.lockPath("dev"), []byte(tc.corrupt), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.age > 0 {
				old := e.clock.Now().Add(-tc.age)
				if err := os.Chtimes(e.lockPath("dev"), old, old); err != nil {
					t.Fatal(err)
				}
			} else {
				// The lock check compares with the fake clock, so make the file look just written.
				if err := os.Chtimes(e.lockPath("dev"), e.clock.Now(), e.clock.Now()); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			writeLockFile(t, e, "dev", tc.lock(e))
		}

		err := runOnce(t, e)
		if tc.wantErr != errors.Is(err, dr.ErrRunInProgress) {
			t.Errorf("%s: err = %v; wantErr %v", name, err, tc.wantErr)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTheLockNamesItsOwnerAndIsReleasedAfterwards(t *testing.T) {
	e := newTestEngine(t, func(o *Options) {
		o.Pid = func() int { return 4242 }
		o.ProcessAlive = func(int) bool { return true } // pid 4242 is only a label here
	})

	release, err := e.acquireLock("dev", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	info, err := readLock(e.lockPath("dev"))
	if err != nil || info.PID != 4242 || info.RunID != "run-1" || info.Host != e.host {
		t.Errorf("lock = %+v, %v", info, err)
	}
	if st, _ := os.Stat(e.lockPath("dev")); permissionBitsMatter() && st.Mode().Perm() != 0o600 {
		t.Errorf("lock mode = %v", st.Mode().Perm())
	}

	_, err = e.acquireLock("dev", "run-2")
	if !errors.Is(err, dr.ErrRunInProgress) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"run-1", "4242"} {
		if !contains(err.Error(), want) {
			t.Errorf("the refusal %q should name the owner (%s)", err, want)
		}
	}

	release()
	if _, err := os.Stat(e.lockPath("dev")); !os.IsNotExist(err) {
		t.Error("lock not removed")
	}
	if release2, err := e.acquireLock("dev", "run-3"); err != nil {
		t.Errorf("after release: %v", err)
	} else {
		release2()
	}
}

func TestReleaseLeavesAnotherRunsLockAlone(t *testing.T) {
	e := newTestEngine(t)
	writeLockFile(t, e, "dev", lockInfo{PID: 1, Host: e.host, RunID: "someone-else", Time: e.clock.Now()})
	e.release(e.lockPath("dev"), "me")
	if _, err := os.Stat(e.lockPath("dev")); err != nil {
		t.Error("a run removed a lock it does not own")
	}
}

func TestLockFilesNeverEscapeTheirDirectory(t *testing.T) {
	e := newTestEngine(t)
	for _, name := range []string{"../../evil", "a/b", "..", "arn:aws:eks:us-east-1:123:cluster/prod"} {
		p := e.lockPath(name)
		if filepath.Dir(p) != filepath.Join(e.dir, "locks") {
			t.Errorf("lock for %q lands in %s", name, p)
		}
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("this process is alive")
	}
	if processAlive(0) || processAlive(-5) {
		t.Error("pid 0 and negative pids are not processes")
	}
	if processAlive(2147483646) {
		t.Error("an unused pid reported alive")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
