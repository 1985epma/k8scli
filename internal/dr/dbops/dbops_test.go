package dbops

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/1985epma/k8scli/internal/dr"
)

func TestPhaseAndDefaults(t *testing.T) {
	p := NewPlanner(Options{})
	if p.Phase() != dr.PhaseDatabases {
		t.Errorf("Phase = %q", p.Phase())
	}
	if p.opts.GateTimeout != defaultGateTimeout || p.opts.GatePoll != defaultGatePoll {
		t.Errorf("defaults not applied: %+v", p.opts)
	}
}

func TestDownStopsHigherTiersFirstAndUpStartsLowerTiersFirst(t *testing.T) {
	te := newTestEnv(t, nil)
	plan := planWith(
		stsDB("core", 0, ""),
		stsDB("orders", 2, ""),
		stsDB("cache", 1, ""),
		stsDB("search", 2, ""), // same tier as orders: plan order decides
	)
	p := NewPlanner(Options{})

	// Down reads the cluster, so give it the StatefulSets.
	for _, db := range plan.Databases {
		_, err := te.client.AppsV1().StatefulSets("shop").Create(context.Background(), statefulSet("shop", db.Ref, 1, 1, nil), metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}

	down, err := p.Steps(context.Background(), te.env, plan, dr.DirDown, dr.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	gotDown := onlyStops(stepIDs(down))
	wantDown := []string{"databases/shop/orders:stop", "databases/shop/search:stop", "databases/shop/cache:stop", "databases/shop/core:stop"}
	if strings.Join(gotDown, ",") != strings.Join(wantDown, ",") {
		t.Errorf("down order = %v; want %v", gotDown, wantDown)
	}

	up, err := p.Steps(context.Background(), te.env, plan, dr.DirUp, dr.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	wantUp := []string{"databases/shop/core:start", "databases/shop/cache:start", "databases/shop/orders:start", "databases/shop/search:start"}
	if strings.Join(stepIDs(up), ",") != strings.Join(wantUp, ",") {
		t.Errorf("up order = %v; want %v", stepIDs(up), wantUp)
	}
}

func onlyStops(ids []string) []string {
	var stops []string
	for _, id := range ids {
		if strings.HasSuffix(id, ":stop") {
			stops = append(stops, id)
		}
	}
	return stops
}

func TestEveryStepIsInThePhaseOfDatabasesAndRiskIsClassified(t *testing.T) {
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg")}, statefulSet("shop", "mysql", 2, 2, nil))
	plan := planWith(cnpgDB("pg", 0, ""), stsDB("mysql", 0, ""))
	steps, err := NewPlanner(Options{}).Steps(context.Background(), te.env, plan, dr.DirDown, dr.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 4 {
		t.Fatalf("steps = %v", stepIDs(steps))
	}
	for _, step := range steps {
		if step.Phase != dr.PhaseDatabases {
			t.Errorf("%s is in phase %q", step.Action.ID(), step.Phase)
		}
		wantRisk := dr.RiskLow
		if strings.HasSuffix(step.Action.ID(), ":hibernate") || strings.HasSuffix(step.Action.ID(), ":stop") {
			wantRisk = dr.RiskHigh
		}
		if step.Action.Risk() != wantRisk {
			t.Errorf("%s risk = %v; want %v", step.Action.ID(), step.Action.Risk(), wantRisk)
		}
		if step.Action.Title() == "" || step.Action.Describe() == "" {
			t.Errorf("%s has no title or description", step.Action.ID())
		}
	}
}

func TestScopeFiltersDatabasesAndPhases(t *testing.T) {
	te := newTestEnv(t, nil, statefulSet("shop", "a", 1, 1, nil), statefulSet("shop", "b", 1, 1, nil))
	plan := planWith(stsDB("a", 0, ""), stsDB("b", 0, ""))
	p := NewPlanner(Options{})

	steps, err := p.Steps(context.Background(), te.env, plan, dr.DirDown, dr.Scope{Databases: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range stepIDs(steps) {
		if !strings.Contains(id, "/b:") {
			t.Errorf("scope leaked %s", id)
		}
	}

	if _, err := p.Steps(context.Background(), te.env, plan, dr.DirDown, dr.Scope{Databases: []string{"ghost"}}); err == nil ||
		!strings.Contains(err.Error(), `"ghost"`) {
		t.Errorf("an unknown database in the scope must be an error, got %v", err)
	}

	none, err := p.Steps(context.Background(), te.env, plan, dr.DirDown, dr.Scope{Phases: []dr.Phase{dr.PhaseApps}})
	if err != nil || len(none) != 0 {
		t.Errorf("a scope without the databases phase must yield nothing, got %v, %v", stepIDs(none), err)
	}
	some, err := p.Steps(context.Background(), te.env, plan, dr.DirDown, dr.Scope{Phases: []dr.Phase{dr.PhaseDatabases}})
	if err != nil || len(some) == 0 {
		t.Errorf("the databases phase in scope must yield steps, got %v", err)
	}
}

func TestInvalidPlansAreRejected(t *testing.T) {
	te := newTestEnv(t, nil)
	p := NewPlanner(Options{})

	for name, db := range map[string]dr.Database{
		"unsupported kind": {Name: "x", Namespace: "shop", Kind: "mongo", Ref: "x"},
		"no ref":           {Name: "x", Namespace: "shop", Kind: KindStatefulSet},
		"no namespace":     {Name: "x", Kind: KindStatefulSet, Ref: "x"},
	} {
		if _, err := p.Steps(context.Background(), te.env, planWith(db), dr.DirUp, dr.Scope{}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := p.Steps(context.Background(), nil, planWith(), dr.DirDown, dr.Scope{}); err == nil {
		t.Error("a nil environment must be rejected")
	}
	if _, err := p.Steps(context.Background(), te.env, planWith(), dr.Direction("sideways"), dr.Scope{}); err == nil {
		t.Error("an unknown direction must be rejected")
	}
}

func TestDownNeedsAClusterConnectionButUpDoesNot(t *testing.T) {
	te := newTestEnv(t, nil)
	te.env.Client, te.env.Dynamic = nil, nil
	plan := planWith(stsDB("mysql", 0, ""), cnpgDB("pg", 0, ""))
	p := NewPlanner(Options{})

	if _, err := p.Steps(context.Background(), te.env, plan, dr.DirDown, dr.Scope{}); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("down without a client = %v; want dr.ErrUnreachable", err)
	}

	// "up" is planned while the cluster may still be off, so it must not need a connection to be listed.
	steps, err := p.Steps(context.Background(), te.env, plan, dr.DirUp, dr.Scope{})
	if err != nil || len(steps) != 2 {
		t.Fatalf("up without a client: %v, %v", stepIDs(steps), err)
	}
	// Applying them later does fail, with a typed error the engine can recognise.
	if _, err := steps[0].Action.Done(context.Background()); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("Done without a client = %v; want dr.ErrUnreachable", err)
	}
}

func TestGateTimesOutWithTheLastStatus(t *testing.T) {
	te := newTestEnv(t, nil)
	p := NewPlanner(Options{GateTimeout: 30e9, GatePoll: 5e9})
	gate := &pollGate{
		name: "something", env: te.env, timeout: p.opts.GateTimeout, poll: p.opts.GatePoll,
		check: func(context.Context) (bool, string, error) { return false, "2 of 3 ready", nil },
	}
	err := gate.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out after 30s") || !strings.Contains(err.Error(), "2 of 3 ready") {
		t.Errorf("err = %v", err)
	}
	if te.pollCount() != 6 {
		t.Errorf("polled %d times; want 6 (30s / 5s)", te.pollCount())
	}
}

func TestGateToleratesATransientReadErrorAndStopsOnCancel(t *testing.T) {
	te := newTestEnv(t, nil)
	calls := 0
	gate := &pollGate{
		name: "flaky", env: te.env, timeout: 60e9, poll: 5e9,
		check: func(context.Context) (bool, string, error) {
			calls++
			if calls < 3 {
				return false, "", errBoom
			}
			return true, "", nil
		},
	}
	if err := gate.Wait(context.Background()); err != nil {
		t.Errorf("a transient error must not fail the gate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stuck := &pollGate{name: "stuck", env: te.env, timeout: 60e9, poll: 5e9,
		check: func(context.Context) (bool, string, error) { return false, "still waiting", nil }}
	if err := stuck.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context must stop the gate, got %v", err)
	}

	never := &pollGate{name: "never", env: te.env, timeout: 10e9, poll: 5e9,
		check: func(context.Context) (bool, string, error) { return false, "", errBoom }}
	if err := never.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a gate that never reads must report the read error, got %v", err)
	}
}
