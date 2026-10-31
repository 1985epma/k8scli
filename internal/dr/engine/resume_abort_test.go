package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

// eventLog collects the events of a run (OnEvent must not block, so it only appends).
type eventLog struct {
	mu     sync.Mutex
	events []dr.Event
}

func (l *eventLog) add(e dr.Event) { l.mu.Lock(); l.events = append(l.events, e); l.mu.Unlock() }

func (l *eventLog) of(kind, step string) []dr.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []dr.Event
	for _, e := range l.events {
		if e.Kind == kind && (step == "" || e.Step == step) {
			out = append(out, e)
		}
	}
	return out
}

// blockingGate waits until the context ends, like a gate that is still waiting when the person aborts.
type blockingGate struct{ entered chan struct{} }

func (g *blockingGate) Name() string { return "pods-ready" }
func (g *blockingGate) Wait(ctx context.Context) error {
	close(g.entered)
	<-ctx.Done()
	return ctx.Err()
}

func TestResumeSaysWhichStepsWereAlreadyDoneInThePreviousRun(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.action("a3").failWith(errBoom)
	planners := []dr.Planner{w.planner(dr.PhaseApps, "a1", "a2", "a3")}

	first, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{})
	if err == nil {
		t.Fatal("the first run should fail")
	}
	w.action("a3").failWith(nil)

	var log eventLog
	second, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{Resume: first.ID, OnEvent: log.add})
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"a1", "a2"} {
		skips := log.of("step-skip", id)
		if len(skips) != 1 || skips[0].Msg != "already done in the previous run" {
			t.Errorf("%s: events %+v; a step finished by the previous run must be reported as already done, not applied again", id, skips)
		}
		if len(log.of("step-done", id)) != 0 {
			t.Errorf("%s was announced as done by the resumed run, but it did nothing", id)
		}
		// The ledger keeps the truth that the step is done; only the wording of the event changes.
		if got := statusOf(t, second, id).Status; got != dr.StepDone {
			t.Errorf("%s status = %s; the ledger must keep done", id, got)
		}
	}
	if done := log.of("step-done", "a3"); len(done) != 1 {
		t.Errorf("a3 was applied by the resumed run and must be announced as done: %+v", log.of("step-done", ""))
	}
}

func TestAbortWhileWaitingOnAGateIsAnAbortAndNotAFailedGate(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	gate := &blockingGate{entered: make(chan struct{})}
	planners := []dr.Planner{&fakePlanner{phase: dr.PhaseApps, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		return []dr.Step{{Phase: dr.PhaseApps, Action: w.action("a1"), Gate: gate}, {Phase: dr.PhaseApps, Action: w.action("a2")}}, nil
	}}}

	var log eventLog
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-gate.entered; cancel() }()
	res, err := e.Run(ctx, testEnv("dev"), testPlan("dev"), dr.DirDown, dr.Scope{}, planners, dr.RunOptions{OnEvent: log.add})

	if !errors.Is(err, context.Canceled) || res == nil || res.Status != dr.StatusAborted {
		t.Fatalf("err = %v, status = %v; want an aborted run", err, res)
	}
	if failed := log.of("gate-fail", ""); len(failed) != 0 {
		t.Errorf("cancelling is not a gate failure, but got %+v", failed)
	}
	if aborted := log.of("run-abort", ""); len(aborted) != 1 {
		t.Errorf("run-abort events = %+v; want exactly one", aborted)
	}
	if failed := log.of("run-fail", ""); len(failed) != 0 {
		t.Errorf("an abort must not also be reported as a failure: %+v", failed)
	}
	step := statusOf(t, res, "a1")
	if step.Status != dr.StepAborted || !strings.HasPrefix(step.Err, "aborted") || strings.Contains(step.Err, "gate") {
		t.Errorf("step = %s / %q; want aborted without gate wording", step.Status, step.Err)
	}
	if got := statusOf(t, res, "a2").Status; got != dr.StepPending {
		t.Errorf("a2 = %s; what never ran stays pending", got)
	}
}

// A real gate failure (not a cancellation) is still reported as one.
func TestARealGateFailureIsStillAGateFailure(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.gate("a1-ready").err = errBoom
	planners := []dr.Planner{&fakePlanner{phase: dr.PhaseApps, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		return []dr.Step{{Phase: dr.PhaseApps, Action: w.action("a1"), Gate: w.gate("a1-ready")}}, nil
	}}}

	var log eventLog
	res, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{OnEvent: log.add})
	if err == nil || res.Status != dr.StatusFailed {
		t.Fatalf("err = %v, res = %v", err, res)
	}
	if failed := log.of("gate-fail", "a1"); len(failed) != 1 {
		t.Errorf("gate-fail events = %+v", failed)
	}
}

func TestRunIDsOfTheSameSecondKeepTheirOrder(t *testing.T) {
	c := newClock()
	now := c.Now()
	var prev string
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id, err := newRunID(now) // the clock never moves: only the counter tells them apart
		if err != nil || !runIDPattern.MatchString(id) {
			t.Fatalf("id %q: %v", id, err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
		if prev != "" && id <= prev {
			t.Fatalf("id %s does not sort after %s although it was created later", id, prev)
		}
		prev = id
	}
}

func TestListIsNewestFirstEvenWhenRunsShareASecond(t *testing.T) {
	e := newTestEngine(t) // a fixed clock: every run starts at the same instant
	w := newWorld()
	planners := []dr.Planner{w.planner(dr.PhaseApps, "a1")}

	var ids []string
	for i := 0; i < 5; i++ {
		res, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.ID)
	}

	for _, limit := range []int{0, 1, 2, 5} {
		got, err := e.List(context.Background(), limit)
		if err != nil {
			t.Fatal(err)
		}
		want := limit
		if want == 0 {
			want = 5
		}
		if len(got) != want {
			t.Fatalf("limit %d: %d runs", limit, len(got))
		}
		for i, run := range got {
			if wantID := ids[len(ids)-1-i]; run.ID != wantID {
				t.Errorf("limit %d, position %d: %s; want %s (newest first)", limit, i, run.ID, wantID)
			}
		}
	}
}

func TestListOrdersByStartTimeBeforeTheId(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	planners := []dr.Planner{w.planner(dr.PhaseApps, "a1")}

	older, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e.clock.advance(250 * time.Millisecond) // same second, later start
	newer, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Give the older run the larger id: the start time must still win.
	stored, _, _ := e.Status(context.Background(), older.ID)
	if !newer.Started.After(stored.Started) {
		t.Fatalf("test setup: %s is not after %s", newer.Started, stored.Started)
	}
	got, err := e.List(context.Background(), 10)
	if err != nil || len(got) != 2 || got[0].ID != newer.ID {
		t.Errorf("List = %v, %v; want the later start first", got, err)
	}
}
