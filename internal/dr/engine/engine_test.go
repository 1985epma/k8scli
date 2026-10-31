package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/provider"
)

// ---- ordering and scope ------------------------------------------------------------------------------------

func TestDownAndUpFollowThePhaseOrder(t *testing.T) {
	e := newTestEngine(t)

	down := newWorld()
	res, err := e.run(t, testPlan("dev"), dr.DirDown, down.allPhases(), dr.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != dr.StatusSucceeded {
		t.Fatalf("status = %s", res.Status)
	}
	// Each phase is planned just before it runs, not all up front.
	want := []string{
		"plan:prepare", "apply:prepare", "plan:apps", "apply:apps", "plan:databases", "apply:databases",
		"plan:nodes", "apply:nodes", "plan:power", "apply:power",
	}
	if got := down.log.with("plan:", "apply:"); !reflect.DeepEqual(got, want) {
		t.Errorf("down order:\n got %v\nwant %v", got, want)
	}

	up := newWorld()
	res, err = e.run(t, testPlan("dev"), dr.DirUp, up.allPhases(), dr.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		"plan:power", "apply:power", "plan:nodes", "apply:nodes", "plan:databases", "apply:databases",
		"plan:apps", "apply:apps", "plan:prepare", "apply:prepare",
	}
	if got := up.log.with("plan:", "apply:"); !reflect.DeepEqual(got, want) {
		t.Errorf("up order:\n got %v\nwant %v", got, want)
	}
	for _, s := range res.Steps {
		if s.Status != dr.StepDone || s.Started.IsZero() || s.Finished.IsZero() {
			t.Errorf("step %s: %+v", s.ID, s)
		}
	}
}

func TestScopeSelectsPhasesAndIsPassedToPlanners(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()

	var seen dr.Scope
	apps := &fakePlanner{phase: dr.PhaseApps, steps: func(_ *dr.Env, _ dr.Direction, scope dr.Scope) ([]dr.Step, error) {
		seen = scope
		return []dr.Step{{Phase: dr.PhaseApps, Action: w.action("apps")}}, nil
	}}
	planners := []dr.Planner{w.planner(dr.PhasePrepare, "prepare"), apps, w.planner(dr.PhaseNodes, "nodes")}

	scope := dr.Scope{Apps: []string{"api"}, Phases: []dr.Phase{dr.PhaseApps, dr.PhaseNodes}}
	res, err := e.Run(context.Background(), testEnv("dev"), testPlan("dev"), dr.DirDown, scope, planners, dr.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.log.with("apply:"); !reflect.DeepEqual(got, []string{"apply:apps", "apply:nodes"}) {
		t.Errorf("applied %v; prepare must be left out", got)
	}
	if len(res.Steps) != 2 || !reflect.DeepEqual(seen.Apps, []string{"api"}) {
		t.Errorf("steps %v, scope seen by the planner %+v", stepsOf(res), seen)
	}

	_, err = e.Run(context.Background(), testEnv("dev"), testPlan("dev"), dr.DirDown, dr.Scope{Apps: []string{"ghost"}}, planners, dr.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "unknown app") {
		t.Errorf("unknown app in scope: %v", err)
	}
}

func TestSeveralPlannersInOnePhaseKeepTheirOrder(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	planners := []dr.Planner{w.planner(dr.PhaseApps, "a1", "a2"), w.planner(dr.PhaseApps, "b1")}
	if _, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := w.log.with("apply:"); !reflect.DeepEqual(got, []string{"apply:a1", "apply:a2", "apply:b1"}) {
		t.Errorf("applied %v", got)
	}
}

// ---- dry run ---------------------------------------------------------------------------------------------------

func TestDryRunChangesNothingAndWritesNothing(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.action("apps").done.Store(true) // already satisfied

	var events []dr.Event
	res, err := e.run(t, testPlan("dev"), dr.DirDown, w.allPhases(), dr.RunOptions{
		DryRun: true, Actor: "alice", OnEvent: func(ev dr.Event) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := w.log.count("apply:"); n != 0 {
		t.Fatalf("a dry run applied %d steps", n)
	}
	if w.log.count("done:") != 5 {
		t.Errorf("Done must be evaluated for every step: %v", w.log.all())
	}
	if res.Status != dr.StatusDryRun || len(res.Steps) != 5 {
		t.Fatalf("status %s, steps %v", res.Status, stepsOf(res))
	}
	for _, s := range res.Steps {
		if s.Title == "" || s.Describe == "" || s.Risk != "low" || s.Equivalent == "" {
			t.Errorf("step %s lacks its description: %+v", s.ID, s)
		}
		wantStatus := dr.StepPending
		if s.ID == "apps" {
			wantStatus = dr.StepSkipped
		}
		if s.Status != wantStatus {
			t.Errorf("step %s: %s; want %s", s.ID, s.Status, wantStatus)
		}
	}

	if len(events) < 3 || events[0].Kind != "run-start" || events[len(events)-1].Kind != "run-done" {
		t.Errorf("events = %+v", events)
	}
	if entries, _ := os.ReadDir(e.dir); len(entries) != 0 {
		t.Errorf("a dry run wrote files: %v", entries)
	}
	if _, found, _ := e.Status(context.Background(), res.ID); found {
		t.Error("a dry run must not be stored")
	}
}

func TestDryRunRunsWithoutConfirmationOrLockAndInReadOnlyMode(t *testing.T) {
	e := newTestEngine(t, func(o *Options) { o.ReadOnly = true })
	w := newWorld()
	plan := testPlan("prod-eu")
	plan.Provider = &dr.Provider{Kind: "manual"}

	if _, err := e.run(t, plan, dr.DirDown, w.allPhases(), dr.RunOptions{DryRun: true}); err != nil {
		t.Errorf("dry run on a production plan in a read-only engine: %v", err)
	}
}

func TestDryRunShowsAnUnreachablePhaseAsDeferred(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	nodes := &fakePlanner{phase: dr.PhaseNodes, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		return nil, dr.ErrUnreachable
	}}
	planners := []dr.Planner{w.planner(dr.PhasePower, "power"), nodes, w.planner(dr.PhaseApps, "apps")}

	res, err := e.run(t, testPlan("dev"), dr.DirUp, planners, dr.RunOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := stepsOf(res); !reflect.DeepEqual(got, []string{"power=pending", "phase/nodes=deferred", "apps=pending"}) {
		t.Errorf("steps = %v", got)
	}
	if nodes.calls.Load() != 1 {
		t.Errorf("a dry run must not retry an unreachable planner, it asked %d times", nodes.calls.Load())
	}
}

func TestRealRunWaitsForAnUnreachableClusterThenContinues(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	asked := 0
	nodes := &fakePlanner{phase: dr.PhaseNodes, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		asked++
		if asked <= 3 {
			return nil, dr.ErrUnreachable
		}
		return []dr.Step{{Phase: dr.PhaseNodes, Action: w.action("uncordon")}}, nil
	}}
	var events []dr.Event
	started := e.clock.Now()
	res, err := e.run(t, testPlan("dev"), dr.DirUp, []dr.Planner{nodes}, dr.RunOptions{
		OnEvent: func(ev dr.Event) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != dr.StatusSucceeded || w.log.count("apply:uncordon") != 1 {
		t.Errorf("status %s, log %v", res.Status, w.log.all())
	}
	if waited := e.clock.Now().Sub(started); waited != 7*time.Second { // 1s + 2s + 4s
		t.Errorf("waited %s; want the backoff 1s, 2s, 4s", waited)
	}
	waits := 0
	for _, ev := range events {
		if ev.Kind == "phase-wait" {
			waits++
		}
	}
	if waits != 3 {
		t.Errorf("%d phase-wait events; want 3", waits)
	}
}

func TestRealRunGivesUpOnAClusterThatNeverComes(t *testing.T) {
	e := newTestEngine(t, func(o *Options) { o.UnreachableTimeout = time.Minute })
	nodes := &fakePlanner{phase: dr.PhaseNodes, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		return nil, dr.ErrUnreachable
	}}
	started := e.clock.Now()
	res, err := e.run(t, testPlan("dev"), dr.DirUp, []dr.Planner{nodes}, dr.RunOptions{})
	if !errors.Is(err, dr.ErrUnreachable) {
		t.Fatalf("err = %v; want ErrUnreachable", err)
	}
	if res == nil || res.Status != dr.StatusFailed || len(res.Steps) != 1 || res.Steps[0].ID != "phase/nodes" || res.Steps[0].Status != dr.StepFailed {
		t.Errorf("result = %+v", res)
	}
	if waited := e.clock.Now().Sub(started); waited < time.Minute || waited > 2*time.Minute {
		t.Errorf("waited %s; the timeout was 1m", waited)
	}
}

func TestAPlannerErrorFailsTheRun(t *testing.T) {
	e := newTestEngine(t)
	bad := &fakePlanner{phase: dr.PhaseApps, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		return nil, errBoom
	}}
	res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{bad}, dr.RunOptions{})
	if !errors.Is(err, errBoom) || res == nil || res.Status != dr.StatusFailed {
		t.Errorf("err %v, result %+v", err, res)
	}
}

// ---- failure, optional steps, gates ------------------------------------------------------------------------

func TestFailureStopsTheRunAndKeepsAResumableLedger(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.action("a2").failWith(errBoom)
	planners := []dr.Planner{w.planner(dr.PhaseApps, "a1", "a2", "a3")}

	res, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{})
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "Do a2") {
		t.Fatalf("err = %v; want the cause and the step title", err)
	}
	if res.Status != dr.StatusFailed {
		t.Errorf("status = %s", res.Status)
	}
	if got := stepsOf(res); !reflect.DeepEqual(got, []string{"a1=done", "a2=failed", "a3=pending"}) {
		t.Errorf("steps = %v", got)
	}
	if w.log.count("apply:a3") != 0 {
		t.Error("a step after the failure ran")
	}
	if failed := statusOf(t, res, "a2"); failed.Err == "" || failed.Finished.IsZero() {
		t.Errorf("failed step = %+v", failed)
	}

	stored, found, err := e.Status(context.Background(), res.ID)
	if err != nil || !found || stored.Status != dr.StatusFailed || len(stored.Steps) != 3 {
		t.Errorf("stored ledger: %+v %v %v", stored, found, err)
	}
}

func TestResumeContinuesFromTheFailureWithoutRepeatingWork(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.action("a3").failWith(errBoom)
	gate := w.gate("a2-ready")
	build := func() []dr.Planner {
		return []dr.Planner{&fakePlanner{phase: dr.PhaseApps, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
			return []dr.Step{
				{Phase: dr.PhaseApps, Action: w.action("a1")},
				{Phase: dr.PhaseApps, Action: w.action("a2"), Gate: gate},
				{Phase: dr.PhaseApps, Action: w.action("a3")},
			}, nil
		}}}
	}

	first, err := e.run(t, testPlan("dev"), dr.DirDown, build(), dr.RunOptions{Actor: "alice"})
	if err == nil {
		t.Fatal("the first run should fail")
	}

	w.action("a3").failWith(nil)
	second, err := e.run(t, testPlan("dev"), dr.DirDown, build(), dr.RunOptions{Resume: first.ID, Actor: "bob"})
	if err != nil {
		t.Fatal(err)
	}

	if second.ID != first.ID || second.Status != dr.StatusSucceeded {
		t.Errorf("resumed run %s status %s", second.ID, second.Status)
	}
	if got := stepsOf(second); !reflect.DeepEqual(got, []string{"a1=done", "a2=done", "a3=done"}) {
		t.Errorf("steps = %v", got)
	}
	for _, id := range []string{"a1", "a2"} {
		if n := w.log.count("apply:" + id); n != 1 {
			t.Errorf("%s was applied %d times; a resumed run must not repeat it", id, n)
		}
	}
	if n := w.log.count("gate:a2-ready"); n != 1 {
		t.Errorf("the gate of a2 was waited %d times; it had already passed", n)
	}
	if w.log.count("apply:a3") != 2 {
		t.Errorf("a3 should have been tried twice: %v", w.log.all())
	}
	if !second.Started.Equal(first.Started) {
		t.Error("a resumed run keeps its original start time")
	}

	if _, err := e.run(t, testPlan("dev"), dr.DirDown, build(), dr.RunOptions{Resume: first.ID}); err == nil || !strings.Contains(err.Error(), "already succeeded") {
		t.Errorf("resuming a finished run: %v", err)
	}
}

func TestResumeIsCheckedAgainstTheLedger(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.action("a1").failWith(errBoom)
	planners := []dr.Planner{w.planner(dr.PhaseApps, "a1")}
	first, _ := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{})

	other := testPlan("dev")
	other.Name = "other"
	otherContext := testPlan("stage")
	for name, tc := range map[string]struct {
		plan *dr.Plan
		dir  dr.Direction
		id   string
		want string
	}{
		"another plan":    {other, dr.DirDown, first.ID, "belongs to plan"},
		"another context": {otherContext, dr.DirDown, first.ID, "acted on context"},
		"another way":     {testPlan("dev"), dr.DirUp, first.ID, "went down"},
		"unknown id":      {testPlan("dev"), dr.DirDown, "20260101T000000Z-deadbeef", "no run"},
		"bad id":          {testPlan("dev"), dr.DirDown, "../../etc/passwd", "no run"},
	} {
		_, err := e.run(t, tc.plan, tc.dir, planners, dr.RunOptions{Resume: tc.id})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v; want %q", name, err, tc.want)
		}
	}
	if _, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{Resume: first.ID, DryRun: true}); err == nil {
		t.Error("a dry run cannot resume")
	}
}

func TestOptionalStepFailureIsSkippedWithItsError(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.action("nice-to-have").failWith(errBoom)
	planner := &fakePlanner{phase: dr.PhaseApps, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		return []dr.Step{
			{Phase: dr.PhaseApps, Action: w.action("a1")},
			{Phase: dr.PhaseApps, Action: w.action("nice-to-have"), Optional: true},
			{Phase: dr.PhaseApps, Action: w.action("a3")},
		}, nil
	}}
	res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{planner}, dr.RunOptions{})
	if err != nil || res.Status != dr.StatusSucceeded {
		t.Fatalf("status %v err %v", res, err)
	}
	skipped := statusOf(t, res, "nice-to-have")
	if skipped.Status != dr.StepSkipped || !strings.Contains(skipped.Err, "boom") {
		t.Errorf("optional step = %+v", skipped)
	}
	if w.log.count("apply:a3") != 1 {
		t.Error("the run must go on after an optional failure")
	}
}

func TestGatesAreWaitedAndTheirWaitIsRecorded(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	planner := &fakePlanner{phase: dr.PhaseApps, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		return []dr.Step{{Phase: dr.PhaseApps, Action: w.action("a1"), Gate: w.gate("ready")}}, nil
	}}
	res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{planner}, dr.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := statusOf(t, res, "a1")
	if s.Gate != "ready" || s.GateWaited == "" {
		t.Errorf("gate not recorded: %+v", s)
	}
	if got := w.log.with("apply:", "gate:"); !reflect.DeepEqual(got, []string{"apply:a1", "gate:ready"}) {
		t.Errorf("the gate must run after its action: %v", got)
	}
}

func TestAFailedGateStopsTheRun(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.gate("ready").err = errors.New("never became ready")
	planner := &fakePlanner{phase: dr.PhaseApps, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		return []dr.Step{
			{Phase: dr.PhaseApps, Action: w.action("a1"), Gate: w.gate("ready")},
			{Phase: dr.PhaseApps, Action: w.action("a2")},
		}, nil
	}}
	res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{planner}, dr.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), `gate "ready"`) {
		t.Fatalf("err = %v", err)
	}
	if got := stepsOf(res); !reflect.DeepEqual(got, []string{"a1=failed", "a2=pending"}) {
		t.Errorf("steps = %v", got)
	}
}

func TestACheckThatCannotRunFailsTheStep(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.action("a1").failDone(errBoom)
	res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{w.planner(dr.PhaseApps, "a1")}, dr.RunOptions{})
	if !errors.Is(err, errBoom) || !strings.Contains(res.Steps[0].Err, "cannot check the current state") {
		t.Errorf("err %v, step %+v", err, res.Steps)
	}
	if w.log.count("apply:") != 0 {
		t.Error("a step whose state is unknown must not be applied")
	}
}

func TestAlreadySatisfiedStepsAreSkipped(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.action("a1").done.Store(true)
	res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{w.planner(dr.PhaseApps, "a1", "a2")}, dr.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := stepsOf(res); !reflect.DeepEqual(got, []string{"a1=skipped", "a2=done"}) {
		t.Errorf("steps = %v", got)
	}
	if w.log.count("apply:a1") != 0 {
		t.Error("a satisfied step was applied")
	}
}

// ---- abort -----------------------------------------------------------------------------------------------------

func TestAbortLeavesAConsistentLedgerAndReleasesTheLock(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	started := make(chan struct{})
	w.action("a2").setOnApply(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	planners := []dr.Planner{w.planner(dr.PhaseApps, "a1", "a2", "a3")}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	res, err := e.Run(ctx, testEnv("dev"), testPlan("dev"), dr.DirDown, dr.Scope{}, planners, dr.RunOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
	if res.Status != dr.StatusAborted {
		t.Errorf("status = %s", res.Status)
	}
	if got := stepsOf(res); !reflect.DeepEqual(got, []string{"a1=done", "a2=aborted", "a3=pending"}) {
		t.Errorf("steps = %v", got)
	}

	stored, found, err := e.Status(context.Background(), res.ID)
	if err != nil || !found || stored.Status != dr.StatusAborted || len(stored.Steps) != 3 {
		t.Fatalf("stored = %+v %v %v", stored, found, err)
	}
	if _, err := os.Stat(e.lockPath("dev")); !os.IsNotExist(err) {
		t.Error("the lock was not released after the abort")
	}

	// The aborted run can be resumed: the blocked step now goes through.
	w.action("a2").setOnApply(nil)
	again, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{Resume: res.ID})
	if err != nil || again.Status != dr.StatusSucceeded {
		t.Fatalf("resume after abort: %v %v", again, err)
	}
	if w.log.count("apply:a1") != 1 {
		t.Error("a1 had finished before the abort and must not repeat")
	}
}

func TestAbortWhileWaitingForTheClusterIsAnAbort(t *testing.T) {
	e := newTestEngine(t)
	nodes := &fakePlanner{phase: dr.PhaseNodes, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		return nil, dr.ErrUnreachable
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := e.Run(ctx, testEnv("dev"), testPlan("dev"), dr.DirUp, dr.Scope{}, []dr.Planner{nodes}, dr.RunOptions{})
	if !errors.Is(err, context.Canceled) || res.Status != dr.StatusAborted {
		t.Errorf("err %v status %v", err, res)
	}
}

// ---- locking ---------------------------------------------------------------------------------------------------

func TestOneRealRunPerContextAtATime(t *testing.T) {
	e := newTestEngine(t)
	block := newWorld()
	hold := make(chan struct{})
	entered := make(chan struct{})
	block.action("a1").setOnApply(func(context.Context) error {
		close(entered)
		<-hold
		return nil
	})

	var wg sync.WaitGroup
	var firstErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, firstErr = e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{block.planner(dr.PhaseApps, "a1")}, dr.RunOptions{})
	}()
	<-entered

	// Same context: refused, and nothing was applied.
	other := newWorld()
	res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{other.planner(dr.PhaseApps, "x")}, dr.RunOptions{})
	if !errors.Is(err, dr.ErrRunInProgress) || res != nil {
		t.Errorf("second run on the same context: %v %v", res, err)
	}
	if other.log.count("apply:") != 0 {
		t.Error("the refused run applied something")
	}

	// Another context is independent.
	third := newWorld()
	if _, err := e.run(t, testPlan("stage"), dr.DirDown, []dr.Planner{third.planner(dr.PhaseApps, "y")}, dr.RunOptions{}); err != nil {
		t.Errorf("a run on another context: %v", err)
	}

	// A dry run on the busy context is fine: it reads only.
	if _, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{other.planner(dr.PhaseApps, "x")}, dr.RunOptions{DryRun: true}); err != nil {
		t.Errorf("dry run during a real run: %v", err)
	}

	close(hold)
	wg.Wait()
	if firstErr != nil {
		t.Errorf("first run: %v", firstErr)
	}
	if _, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{other.planner(dr.PhaseApps, "x")}, dr.RunOptions{}); err != nil {
		t.Errorf("after the first run ended: %v", err)
	}
}

func TestConcurrentRunsOnDifferentContextsDoNotInterfere(t *testing.T) {
	e := newTestEngine(t)
	contexts := []string{"alpha", "beta", "gamma", "delta"}

	var wg sync.WaitGroup
	errs := make([]error, len(contexts))
	results := make([]*dr.RunResult, len(contexts))
	for i, name := range contexts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := newWorld()
			results[i], errs[i] = e.Run(context.Background(), testEnv(name), testPlan(name), dr.DirDown, dr.Scope{}, w.allPhases(), dr.RunOptions{Actor: name})
		}()
	}
	wg.Wait()

	ids := map[string]bool{}
	for i := range contexts {
		if errs[i] != nil || results[i].Status != dr.StatusSucceeded {
			t.Errorf("%s: %v %v", contexts[i], results[i], errs[i])
			continue
		}
		ids[results[i].ID] = true
	}
	if len(ids) != len(contexts) {
		t.Errorf("run ids collided: %v", ids)
	}

	// Every audit line is intact even though four runs wrote at the same time.
	records, err := e.Audit(context.Background(), 10000)
	if err != nil {
		t.Fatal(err)
	}
	perRun := map[string]int{}
	for _, rec := range records {
		perRun[rec.RunID]++
	}
	for id := range ids {
		if perRun[id] < 12 { // run-start, 5 x (start+done), run-done
			t.Errorf("run %s has %d audit records", id, perRun[id])
		}
	}
	raw, _ := os.ReadFile(e.auditPath())
	if got := strings.Count(string(raw), "\n"); got != len(records) {
		t.Errorf("%d lines but %d parsed records: a line is damaged", got, len(records))
	}
}

// ---- confirmation, read-only, destructive steps ------------------------------------------------------------

func TestConfirmPhrase(t *testing.T) {
	withProvider := func(name string) *dr.Plan {
		p := testPlan(name)
		p.Provider = &dr.Provider{Kind: "kind"}
		return p
	}
	for _, tc := range []struct {
		name  string
		plan  *dr.Plan
		dir   dr.Direction
		scope dr.Scope
		want  string
	}{
		{"dev down no provider", testPlan("dev"), dr.DirDown, dr.Scope{}, ""},
		{"dev up", testPlan("dev"), dr.DirUp, dr.Scope{}, ""},
		{"prod down no provider", testPlan("prod-eu"), dr.DirDown, dr.Scope{}, "prod-eu"},
		{"prod up", testPlan("prod-eu"), dr.DirUp, dr.Scope{}, "prod-eu"},
		{"dev down with provider", withProvider("dev"), dr.DirDown, dr.Scope{}, "stop dev"},
		{"prod down with provider", withProvider("prod-eu"), dr.DirDown, dr.Scope{}, "stop prod-eu"},
		{"down with provider, only apps", withProvider("dev"), dr.DirDown, dr.Scope{Phases: []dr.Phase{dr.PhaseApps}}, ""},
		{"prod down with provider, only apps", withProvider("prod-eu"), dr.DirDown, dr.Scope{Phases: []dr.Phase{dr.PhaseApps}}, "prod-eu"},
		{"explicit power phase", testPlan("dev"), dr.DirDown, dr.Scope{Phases: []dr.Phase{dr.PhasePower}}, "stop dev"},
		{"up with provider", withProvider("dev"), dr.DirUp, dr.Scope{}, ""},
		{"prod up with provider", withProvider("prod-eu"), dr.DirUp, dr.Scope{}, "prod-eu"},
	} {
		if got := ConfirmPhrase(tc.plan, tc.dir, tc.scope); got != tc.want {
			t.Errorf("%s: ConfirmPhrase = %q; want %q", tc.name, got, tc.want)
		}
	}
	if ConfirmPhrase(nil, dr.DirDown, dr.Scope{}) != "" {
		t.Error("a nil plan needs no phrase")
	}
}

func TestARealRunNeedsItsConfirmationPhrase(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	planners := []dr.Planner{w.planner(dr.PhaseApps, "a1")}
	plan := testPlan("prod-eu")

	for _, confirm := range []string{"", "prod", "PROD-EU", "prod-eu ", "stop prod-eu"} {
		res, err := e.run(t, plan, dr.DirDown, planners, dr.RunOptions{Confirm: confirm})
		if !errors.Is(err, dr.ErrConfirmationRequired) || res != nil {
			t.Errorf("confirm %q: %v %v", confirm, res, err)
		}
	}
	if w.log.count("apply:") != 0 || w.log.count("done:") != 0 {
		t.Error("nothing may be touched before the confirmation is right")
	}
	if _, err := os.Stat(e.auditPath()); !os.IsNotExist(err) {
		t.Error("a refused run must not write an audit record")
	}

	_, err := e.run(t, plan, dr.DirDown, planners, dr.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), `"prod-eu"`) {
		t.Errorf("the error must tell what to type: %v", err)
	}
	if res, err := e.run(t, plan, dr.DirDown, planners, dr.RunOptions{Confirm: "prod-eu"}); err != nil || res.Status != dr.StatusSucceeded {
		t.Errorf("with the right phrase: %v %v", res, err)
	}
}

func TestADestructiveStepAlwaysNeedsTheStopPhrase(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.action("poweroff").risk = dr.RiskDestructive
	// The plan has no provider, so no phrase is announced up front, but a planner still produced a
	// destructive step: the engine must catch it when it gets there.
	planners := []dr.Planner{w.planner(dr.PhaseApps, "a1", "poweroff", "a3")}

	res, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{})
	if !errors.Is(err, dr.ErrConfirmationRequired) || !strings.Contains(err.Error(), `"stop dev"`) {
		t.Fatalf("err = %v", err)
	}
	if got := stepsOf(res); !reflect.DeepEqual(got, []string{"a1=done", "poweroff=failed", "a3=pending"}) {
		t.Errorf("steps = %v", got)
	}
	if w.log.count("apply:poweroff") != 0 {
		t.Fatal("the destructive step ran without confirmation")
	}

	// A production confirmation (the context name) is not enough for a destructive step either.
	prod := newWorld()
	prod.action("poweroff").risk = dr.RiskDestructive
	_, err = e.run(t, testPlan("prod-eu"), dr.DirDown, []dr.Planner{prod.planner(dr.PhaseApps, "poweroff")}, dr.RunOptions{Confirm: "prod-eu"})
	if !errors.Is(err, dr.ErrConfirmationRequired) || prod.log.count("apply:") != 0 {
		t.Errorf("prod with the context name only: %v", err)
	}

	// With the stop phrase it goes ahead.
	ok := newWorld()
	ok.action("poweroff").risk = dr.RiskDestructive
	if _, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{ok.planner(dr.PhaseApps, "poweroff")}, dr.RunOptions{Confirm: "stop dev"}); err != nil {
		t.Errorf("with the stop phrase: %v", err)
	}

	// A dry run describes it without asking.
	dry := newWorld()
	dry.action("poweroff").risk = dr.RiskDestructive
	res, err = e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{dry.planner(dr.PhaseApps, "poweroff")}, dr.RunOptions{DryRun: true})
	if err != nil || res.Steps[0].Risk != "destructive" {
		t.Errorf("dry run: %v %+v", err, res)
	}
}

func TestAStepAlreadySatisfiedNeedsNoDestructiveConfirmation(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	off := w.action("poweroff")
	off.risk = dr.RiskDestructive
	off.done.Store(true) // already off: nothing is going to happen
	if _, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{w.planner(dr.PhaseApps, "poweroff")}, dr.RunOptions{}); err != nil {
		t.Errorf("an already satisfied destructive step must not ask: %v", err)
	}
}

func TestReadOnlyEngineAndOptionRefuseRealRuns(t *testing.T) {
	for name, tc := range map[string]struct {
		engineReadOnly bool
		optReadOnly    bool
	}{"engine": {true, false}, "option": {false, true}} {
		e := newTestEngine(t, func(o *Options) { o.ReadOnly = tc.engineReadOnly })
		w := newWorld()
		planners := []dr.Planner{w.planner(dr.PhaseApps, "a1")}

		res, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{ReadOnly: tc.optReadOnly})
		if !errors.Is(err, dr.ErrReadOnly) || res != nil || w.log.count("apply:") != 0 {
			t.Errorf("%s: real run %v %v", name, res, err)
		}
		if _, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{ReadOnly: tc.optReadOnly, DryRun: true}); err != nil {
			t.Errorf("%s: dry run: %v", name, err)
		}
	}
}

// ---- input checks --------------------------------------------------------------------------------------------

func TestBadInputIsRefusedBeforeAnythingRuns(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	ok := w.planner(dr.PhaseApps, "a1")
	run := func(plan *dr.Plan, env *dr.Env, dir dr.Direction, planners ...dr.Planner) error {
		_, err := e.Run(context.Background(), env, plan, dir, dr.Scope{}, planners, dr.RunOptions{})
		return err
	}

	invalid := testPlan("dev")
	invalid.Name = ""
	for name, err := range map[string]error{
		"nil plan":         run(nil, testEnv("dev"), dr.DirDown, ok),
		"nil env":          run(testPlan("dev"), nil, dr.DirDown, ok),
		"bad direction":    run(testPlan("dev"), testEnv("dev"), "sideways", ok),
		"invalid plan":     run(invalid, testEnv("dev"), dr.DirDown, ok),
		"context mismatch": run(testPlan("dev"), testEnv("prod-eu"), dr.DirDown, ok),
		"nil planner":      run(testPlan("dev"), testEnv("dev"), dr.DirDown, nil),
		"unknown phase":    run(testPlan("dev"), testEnv("dev"), dr.DirDown, &fakePlanner{phase: "teleport"}),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if w.log.count("apply:") != 0 {
		t.Error("something ran")
	}
}

func TestMalformedPlannerOutputIsRejected(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	for name, steps := range map[string][]dr.Step{
		"no action":     {{Phase: dr.PhaseApps}},
		"wrong phase":   {{Phase: dr.PhaseNodes, Action: w.action("x")}},
		"duplicate ids": {{Phase: dr.PhaseApps, Action: w.action("d")}, {Phase: dr.PhaseApps, Action: w.action("d")}},
	} {
		planner := &fakePlanner{phase: dr.PhaseApps, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) { return steps, nil }}
		res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{planner}, dr.RunOptions{})
		if err == nil || res == nil || res.Status != dr.StatusFailed {
			t.Errorf("%s: %v %v", name, res, err)
		}
	}
	if w.log.count("apply:") != 0 {
		t.Error("a malformed phase must run nothing")
	}

	// An empty phase on the step means "the planner's phase".
	planner := &fakePlanner{phase: dr.PhaseApps, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		return []dr.Step{{Action: w.action("inherits")}}, nil
	}}
	res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{planner}, dr.RunOptions{})
	if err != nil || res.Steps[0].Phase != dr.PhaseApps {
		t.Errorf("inherited phase: %v %v", res, err)
	}
}

func TestCallbackPanicsDoNotTakeTheRunDown(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	var logged []string
	env := testEnv("dev")
	env.Logf = func(format string, args ...any) { logged = append(logged, format) }

	res, err := e.Run(context.Background(), env, testPlan("dev"), dr.DirDown, dr.Scope{}, []dr.Planner{w.planner(dr.PhaseApps, "a1")},
		dr.RunOptions{OnEvent: func(dr.Event) { panic("callback bug") }})
	if err != nil || res.Status != dr.StatusSucceeded {
		t.Fatalf("%v %v", res, err)
	}
	if len(logged) == 0 {
		t.Error("the panic should have been logged")
	}
}

func TestEventsAreNumberedAndCarryTheActor(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	var events []dr.Event
	if _, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{w.planner(dr.PhaseApps, "a1")}, dr.RunOptions{
		Actor: "alice", OnEvent: func(ev dr.Event) { events = append(events, ev) },
	}); err != nil {
		t.Fatal(err)
	}
	for i, ev := range events {
		if ev.Seq != i+1 || ev.Actor != "alice" || ev.RunID == "" || ev.Time.IsZero() {
			t.Errorf("event %d = %+v", i, ev)
		}
	}
	kinds := make([]string, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	if !reflect.DeepEqual(kinds, []string{"run-start", "step-start", "step-done", "run-done"}) {
		t.Errorf("kinds = %v", kinds)
	}
}

func TestEnvDefaultsAreFilledOnACopy(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	var seen *dr.Env
	planner := &fakePlanner{phase: dr.PhaseApps, steps: func(env *dr.Env, _ dr.Direction, _ dr.Scope) ([]dr.Step, error) {
		seen = env
		return nil, nil
	}}
	env := &dr.Env{}
	if _, err := e.Run(context.Background(), env, testPlan("dev"), dr.DirDown, dr.Scope{}, []dr.Planner{planner, w.planner(dr.PhaseNodes, "n")}, dr.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if seen == nil || seen.Context != "dev" || seen.Now == nil || seen.Sleep == nil || seen.Logf == nil {
		t.Errorf("planner env = %+v", seen)
	}
	if env.Now != nil || env.Context != "" {
		t.Error("the caller's env was modified")
	}
}

func TestNewDefaultsToTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	e, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if e.Dir() != filepath.Join(home, ".k8scli", "dr") || e.StateDir() != filepath.Join(home, ".k8scli", "dr", "state") {
		t.Errorf("dir %s state %s", e.Dir(), e.StateDir())
	}
	if _, err := os.Stat(e.Dir()); !os.IsNotExist(err) {
		t.Error("New must not touch the disk")
	}
}

// ---- steps only a person can do ------------------------------------------------------------------------------

func manualError() error {
	return fmt.Errorf("%w: turn Kubernetes off in Docker Desktop (Settings > Kubernetes); then run `k8scli dr resume <id>` to continue", provider.ErrManualStep)
}

func TestAManualStepWaitsForThePersonAndResumes(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	manual := w.action("power-off")
	manual.failWith(manualError())
	planners := []dr.Planner{w.planner(dr.PhaseNodes, "drain"), w.planner(dr.PhasePower, "power-off")}

	var events []dr.Event
	res, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{
		Actor: "alice", OnEvent: func(ev dr.Event) { events = append(events, ev) },
	})
	if !errors.Is(err, provider.ErrManualStep) || !strings.Contains(err.Error(), "waiting for a person") {
		t.Fatalf("err = %v", err)
	}
	if res.Status != dr.StatusFailed {
		t.Errorf("status = %s; a manual stop is failed-like and resumable", res.Status)
	}
	step := statusOf(t, res, "power-off")
	if step.Status != dr.StepFailed || !strings.Contains(step.Err, "turn Kubernetes off in Docker Desktop (Settings > Kubernetes); then run `k8scli dr resume <id>`") {
		t.Errorf("the instructions must be kept verbatim: %+v", step)
	}
	if got := stepsOf(res); !reflect.DeepEqual(got, []string{"drain=done", "power-off=failed"}) {
		t.Errorf("steps = %v", got)
	}

	var manualEvents []dr.Event
	for _, ev := range events {
		if ev.Kind == "step-manual" {
			manualEvents = append(manualEvents, ev)
		}
	}
	if len(manualEvents) != 1 || manualEvents[0].Step != "power-off" || !strings.Contains(manualEvents[0].Err, "Docker Desktop") {
		t.Errorf("step-manual events = %+v", manualEvents)
	}
	if last := events[len(events)-1]; last.Kind != "run-fail" || last.Msg != "waiting for a person" {
		t.Errorf("last event = %+v", last)
	}
	if w.log.count("apply:power-off") != 1 {
		t.Errorf("the manual step is tried once: %v", w.log.all())
	}
	if stored, found, _ := e.Status(context.Background(), res.ID); !found || stored.Status != dr.StatusFailed {
		t.Errorf("the ledger must be stored and resumable: %+v", stored)
	}

	// Resuming before the person acted just waits again, without looping.
	again, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{Resume: res.ID})
	if !errors.Is(err, provider.ErrManualStep) || again.Status != dr.StatusFailed {
		t.Fatalf("resume before the person acted: %v %v", again, err)
	}
	if w.log.count("apply:power-off") != 2 {
		t.Errorf("one try per resume, not a retry loop: %d", w.log.count("apply:power-off"))
	}

	// The person acts: Done now reports true, so resume goes on.
	manual.done.Store(true)
	manual.failWith(nil)
	final, err := e.run(t, testPlan("dev"), dr.DirDown, planners, dr.RunOptions{Resume: res.ID})
	if err != nil || final.Status != dr.StatusSucceeded {
		t.Fatalf("resume after the person acted: %v %v", final, err)
	}
	if got := stepsOf(final); !reflect.DeepEqual(got, []string{"drain=done", "power-off=skipped"}) {
		t.Errorf("steps = %v", got)
	}
	if w.log.count("apply:drain") != 1 {
		t.Error("finished steps must not repeat")
	}
}

func TestAManualStepEndsTheRunEvenWhenOptional(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.action("by-hand").failWith(manualError())
	planner := &fakePlanner{phase: dr.PhasePower, steps: func(*dr.Env, dr.Direction, dr.Scope) ([]dr.Step, error) {
		return []dr.Step{
			{Phase: dr.PhasePower, Action: w.action("by-hand"), Optional: true},
			{Phase: dr.PhasePower, Action: w.action("next")},
		}, nil
	}}
	res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{planner}, dr.RunOptions{})
	if !errors.Is(err, provider.ErrManualStep) || w.log.count("apply:next") != 0 {
		t.Errorf("err %v log %v", err, w.log.all())
	}
	if got := stepsOf(res); !reflect.DeepEqual(got, []string{"by-hand=failed", "next=pending"}) {
		t.Errorf("steps = %v", got)
	}
}

func TestACancelledRunIsAnAbortEvenIfTheStepWasManual(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	w.action("by-hand").setOnApply(func(context.Context) error { return manualError() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := e.Run(ctx, testEnv("dev"), testPlan("dev"), dr.DirDown, dr.Scope{}, []dr.Planner{w.planner(dr.PhasePower, "by-hand")}, dr.RunOptions{})
	if !errors.Is(err, context.Canceled) || res == nil || res.Status != dr.StatusAborted {
		t.Errorf("err %v result %v", err, res)
	}
}
