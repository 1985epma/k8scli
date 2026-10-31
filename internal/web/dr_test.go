package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/backups"
	"github.com/1985epma/k8scli/internal/dr/checks"
	"github.com/1985epma/k8scli/internal/dr/measure"
)

// ---- fakes -------------------------------------------------------------------------------------------

const (
	fakeRunID    = "20261031T015900Z-1a2b3c4d"
	fakeRunIDTwo = "20261031T020000Z-5e6f7a8b"
)

type runCall struct {
	Plan   string
	Dir    dr.Direction
	Scope  dr.Scope
	Opts   dr.RunOptions
	Dry    bool
	Resume string
}

// fakeDR is the cluster side of the DR endpoints: it records what it was asked and lets a test decide
// when a "real" run ends.
type fakeDR struct {
	mu      sync.Mutex
	calls   []runCall
	ledger  *fakeLedger
	nextID  string
	release chan struct{}
	// refuse makes Run fail before announcing itself (a confirmation, a lock...).
	refuse error
	// dryFails makes a dry run fail to plan, the way the engine reports a missing workload.
	dryFails error
	steps    []dr.StepResult

	drills    []measure.DrillOptions
	drillPlan []string
	drillRep  *measure.Report
	drillErr  error
	drillHold chan struct{}

	exportErr   error
	exportBytes string
	exportCalls []string
	checkCalls  [][]string
	diffCalls   []string
	checkErr    error
}

func newFakeDR(ledger *fakeLedger) *fakeDR {
	return &fakeDR{ledger: ledger, nextID: fakeRunID, release: make(chan struct{}), steps: []dr.StepResult{
		{ID: "apps/shop/api:down", Title: "Scale shop/api to 0", Phase: dr.PhaseApps, Risk: "high", Status: dr.StepPending},
	}}
}

func (f *fakeDR) Run(ctx context.Context, plan *dr.Plan, dir dr.Direction, scope dr.Scope, opts dr.RunOptions) (*dr.RunResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, runCall{Plan: plan.Name, Dir: dir, Scope: scope, Opts: opts, Dry: opts.DryRun, Resume: opts.Resume})
	refuse, id, steps := f.refuse, f.nextID, append([]dr.StepResult{}, f.steps...)
	release := f.release
	f.mu.Unlock()

	if opts.DryRun {
		if f.dryFails != nil {
			failed := []dr.StepResult{{ID: "phase/apps", Title: "Plan the apps phase", Phase: dr.PhaseApps, Status: dr.StepFailed, Err: f.dryFails.Error()}}
			return &dr.RunResult{Plan: plan.Name, Context: plan.Context, Direction: dir, Status: dr.StatusFailed, Steps: failed}, f.dryFails
		}
		return &dr.RunResult{Plan: plan.Name, Context: plan.Context, Direction: dir, Status: dr.StatusDryRun, Steps: steps}, nil
	}
	if refuse != nil {
		return nil, refuse
	}
	if opts.Resume != "" {
		id = opts.Resume
	}
	running := &dr.RunResult{ID: id, Plan: plan.Name, Context: plan.Context, Direction: dir, Status: dr.StatusRunning, Actor: opts.Actor, Started: time.Now(), Steps: steps}
	f.ledger.put(running)
	if opts.OnEvent != nil {
		opts.OnEvent(dr.Event{RunID: id, Kind: "run-start", Actor: opts.Actor})
	}
	select {
	case <-release:
		done := *running
		done.Status, done.Finished = dr.StatusSucceeded, time.Now()
		f.ledger.put(&done)
		return &done, nil
	case <-ctx.Done():
		aborted := *running
		aborted.Status, aborted.Finished = dr.StatusAborted, time.Now()
		f.ledger.put(&aborted)
		return &aborted, fmt.Errorf("the run was aborted: %w", ctx.Err())
	}
}

func (f *fakeDR) Drill(ctx context.Context, plan *dr.Plan, opts measure.DrillOptions) (*measure.Report, error) {
	f.mu.Lock()
	f.drills = append(f.drills, opts)
	f.drillPlan = append(f.drillPlan, plan.Name)
	report, err, hold := f.drillRep, f.drillErr, f.drillHold
	f.mu.Unlock()
	if opts.OnEvent != nil && err == nil {
		opts.OnEvent(dr.Event{RunID: fakeRunID, Kind: "run-start"})
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return report, err
}

func (f *fakeDR) Check(_ context.Context, contextName string, namespaces []string) (*checks.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCalls = append(f.checkCalls, append([]string{contextName}, namespaces...))
	if f.checkErr != nil {
		return nil, f.checkErr
	}
	return &checks.Report{}, nil
}

func (f *fakeDR) Backups(_ context.Context, contextName, namespace string) (*backups.Listing, error) {
	return &backups.Listing{Items: []backups.Item{}, VeleroInstalled: contextName == "dev"}, nil
}

func (f *fakeDR) Diff(_ context.Context, primary, standby string, namespaces []string) (*checks.DiffReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diffCalls = append(f.diffCalls, primary+">"+standby+":"+strings.Join(namespaces, ","))
	return &checks.DiffReport{}, nil
}

func (f *fakeDR) Export(_ context.Context, contextName, namespace string, w ioWriter) (*checks.ExportResult, error) {
	f.mu.Lock()
	f.exportCalls = append(f.exportCalls, contextName+"/"+namespace)
	err, body := f.exportErr, f.exportBytes
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if body == "" {
		body = "PK\x03\x04fake-zip"
	}
	_, _ = w.Write([]byte(body))
	return &checks.ExportResult{Namespace: namespace, Context: contextName}, nil
}

func (f *fakeDR) runCalls() []runCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runCall{}, f.calls...)
}

func (f *fakeDR) realCalls() []runCall {
	var out []runCall
	for _, c := range f.runCalls() {
		if !c.Dry {
			out = append(out, c)
		}
	}
	return out
}

type fakeLedger struct {
	mu   sync.Mutex
	runs map[string]dr.RunResult
}

func (l *fakeLedger) put(r *dr.RunResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.runs == nil {
		l.runs = map[string]dr.RunResult{}
	}
	l.runs[r.ID] = *r
}

func (l *fakeLedger) Status(_ context.Context, id string) (*dr.RunResult, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.runs[id]
	if !ok {
		return nil, false, nil
	}
	return &r, true, nil
}

func (l *fakeLedger) List(_ context.Context, limit int) ([]dr.RunResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []dr.RunResult
	for _, r := range l.runs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---- environment -------------------------------------------------------------------------------------

type drEnv struct {
	*env
	fake   *fakeDR
	ledger *fakeLedger
	dir    string
}

func newDREnv(t *testing.T, opts Options) *drEnv {
	t.Helper()
	dir := t.TempDir()
	opts.DRDir = dir
	e := newEnv(t, opts)
	ledger := &fakeLedger{}
	fake := newFakeDR(ledger)
	e.server.dr.newBackend = func() (drBackend, error) { return fake, nil }
	e.server.dr.newLedger = func() (drLedger, error) { return ledger, nil }
	t.Cleanup(func() { e.server.dr.shutdown(5 * time.Second) })
	return &drEnv{env: e, fake: fake, ledger: ledger, dir: dir}
}

// planYAML builds a valid plan for the test kubeconfig (contexts dev and prod-eu).
func planYAML(name, contextName string, labOnly, provider bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "version: 1\nname: %s\ncontext: %s\ntargets:\n  rto: 30m\n  rpo: 1h\napps:\n  - name: api\n    namespace: team-a\n    workloads: [api]\n    tier: 1\n  - name: web\n    namespace: team-a\n    workloads: [web]\n    tier: 2\n    dependsOn: [api]\n", name, contextName)
	b.WriteString("databases:\n  - name: db\n    namespace: team-a\n    kind: statefulset\n    ref: db\n    tier: 0\n")
	if provider {
		b.WriteString("provider:\n  kind: kind\n  params:\n    name: lab\n")
	}
	if labOnly {
		b.WriteString("safety:\n  labOnly: true\n")
	}
	return b.String()
}

func (d *drEnv) writePlan(t *testing.T, name, yaml string) {
	t.Helper()
	dir := filepath.Join(d.dir, "plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (d *drEnv) dryRun(t *testing.T, plan, direction string, scope drScope) drDryRunResponse {
	t.Helper()
	rec := d.do(http.MethodPost, "/api/dr/runs", drRunRequest{Plan: plan, Direction: direction, Scope: scope, DryRun: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run: status %d: %s", rec.Code, rec.Body.String())
	}
	return decode[drDryRunResponse](t, rec)
}

// waitFor polls a condition for a short time.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func jsonUnmarshal(raw []byte, into any) error { return json.Unmarshal(raw, into) }
