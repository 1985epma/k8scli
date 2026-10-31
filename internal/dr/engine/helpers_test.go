package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

// ---- fakes -----------------------------------------------------------------------------------------------------

type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.calls...)
}

// count returns how many calls start with prefix.
func (l *callLog) count(prefix string) int {
	n := 0
	for _, c := range l.all() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// with returns the calls that start with one of the prefixes, in order.
func (l *callLog) with(prefixes ...string) []string {
	var out []string
	for _, c := range l.all() {
		for _, p := range prefixes {
			if strings.HasPrefix(c, p) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// fakeAction is a step whose "world" is its own done flag, so state survives from run to run.
type fakeAction struct {
	id   string
	risk dr.Risk
	log  *callLog
	done atomic.Bool

	mu       sync.Mutex
	applyErr error
	doneErr  error
	onApply  func(ctx context.Context) error
}

func (a *fakeAction) ID() string         { return a.id }
func (a *fakeAction) Title() string      { return "Do " + a.id }
func (a *fakeAction) Describe() string   { return "Describe " + a.id }
func (a *fakeAction) Risk() dr.Risk      { return a.risk }
func (a *fakeAction) Equivalent() string { return "cmd " + a.id }

func (a *fakeAction) Done(context.Context) (bool, error) {
	a.log.add("done:" + a.id)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doneErr != nil {
		return false, a.doneErr
	}
	return a.done.Load(), nil
}

func (a *fakeAction) Apply(ctx context.Context) error {
	a.log.add("apply:" + a.id)
	a.mu.Lock()
	hook, err := a.onApply, a.applyErr
	a.mu.Unlock()
	if hook != nil {
		if herr := hook(ctx); herr != nil {
			return herr
		}
	}
	if err != nil {
		return err
	}
	a.done.Store(true)
	return nil
}

func (a *fakeAction) failWith(err error) {
	a.mu.Lock()
	a.applyErr = err
	a.mu.Unlock()
}

func (a *fakeAction) setOnApply(f func(ctx context.Context) error) {
	a.mu.Lock()
	a.onApply = f
	a.mu.Unlock()
}

func (a *fakeAction) failDone(err error) {
	a.mu.Lock()
	a.doneErr = err
	a.mu.Unlock()
}

type fakeGate struct {
	name string
	log  *callLog
	mu   sync.Mutex
	err  error
}

func (g *fakeGate) Name() string { return g.name }
func (g *fakeGate) Wait(context.Context) error {
	g.log.add("gate:" + g.name)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

// world holds the actions of one simulated cluster.
type world struct {
	log     callLog
	actions map[string]*fakeAction
	gates   map[string]*fakeGate
}

func newWorld() *world {
	return &world{actions: map[string]*fakeAction{}, gates: map[string]*fakeGate{}}
}

func (w *world) action(id string) *fakeAction {
	if a, ok := w.actions[id]; ok {
		return a
	}
	a := &fakeAction{id: id, log: &w.log}
	w.actions[id] = a
	return a
}

func (w *world) gate(name string) *fakeGate {
	if g, ok := w.gates[name]; ok {
		return g
	}
	g := &fakeGate{name: name, log: &w.log}
	w.gates[name] = g
	return g
}

type fakePlanner struct {
	phase dr.Phase
	steps func(env *dr.Env, dir dr.Direction, scope dr.Scope) ([]dr.Step, error)
	calls atomic.Int32
}

func (p *fakePlanner) Phase() dr.Phase { return p.phase }
func (p *fakePlanner) Steps(_ context.Context, env *dr.Env, _ *dr.Plan, dir dr.Direction, scope dr.Scope) ([]dr.Step, error) {
	p.calls.Add(1)
	return p.steps(env, dir, scope)
}

// planner makes a planner for phase whose steps are the given action ids, planned lazily (each plan
// call is logged as "plan:<phase>").
func (w *world) planner(phase dr.Phase, ids ...string) *fakePlanner {
	return &fakePlanner{phase: phase, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		w.log.add("plan:" + string(phase))
		steps := make([]dr.Step, len(ids))
		for i, id := range ids {
			steps[i] = dr.Step{Phase: phase, Action: w.action(id)}
		}
		return steps, nil
	}}
}

// allPhases returns one planner per phase with one step "<phase>".
func (w *world) allPhases() []dr.Planner {
	var out []dr.Planner
	for _, phase := range dr.DownOrder {
		out = append(out, w.planner(phase, string(phase)))
	}
	return out
}

// clock is a fake clock; Sleep only moves it forward.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Now().UTC().Truncate(time.Second)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.advance(d)
	return nil
}

// ---- builders --------------------------------------------------------------------------------------------------

type testEngine struct {
	*Engine
	clock *clock
	dir   string
}

func newTestEngine(t *testing.T, mods ...func(*Options)) *testEngine {
	t.Helper()
	c := newClock()
	opts := Options{Dir: t.TempDir(), Now: c.Now, Sleep: c.Sleep}
	for _, mod := range mods {
		mod(&opts)
	}
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return &testEngine{Engine: e, clock: c, dir: opts.Dir}
}

func testPlan(contextName string) *dr.Plan {
	return &dr.Plan{
		Version: dr.PlanVersion, Name: "demo", Context: contextName,
		Apps: []dr.App{
			{Name: "api", Namespace: "shop", Workloads: []string{"api"}},
			{Name: "web", Namespace: "shop", Workloads: []string{"web"}},
		},
	}
}

func testEnv(contextName string) *dr.Env { return &dr.Env{Context: contextName} }

func (e *testEngine) run(t *testing.T, plan *dr.Plan, dir dr.Direction, planners []dr.Planner, opts dr.RunOptions) (*dr.RunResult, error) {
	t.Helper()
	return e.Run(context.Background(), testEnv(plan.Context), plan, dir, dr.Scope{}, planners, opts)
}

func stepsOf(res *dr.RunResult) []string {
	out := make([]string, len(res.Steps))
	for i, s := range res.Steps {
		out[i] = s.ID + "=" + string(s.Status)
	}
	return out
}

func statusOf(t *testing.T, res *dr.RunResult, id string) dr.StepResult {
	t.Helper()
	for _, s := range res.Steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no step %q in %v", id, stepsOf(res))
	return dr.StepResult{}
}

var errBoom = errors.New("boom")
