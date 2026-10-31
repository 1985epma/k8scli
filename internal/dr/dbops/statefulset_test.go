package dbops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/1985epma/k8scli/internal/dr"
)

const stateKey = "lab/databases"

func TestStatefulSetStopRecordsTheSizeAndScalesToZero(t *testing.T) {
	te := newTestEnv(t, nil, statefulSet("shop", "mysql", 3, 3, nil))
	p := NewPlanner(Options{Backups: finderAt(epoch.Add(-2 * time.Hour))})
	steps := downSteps(t, te, p, planWith(stsDB("mysql", 0, "24h")))
	if got := stepIDs(steps); len(got) != 2 || got[0] != "databases/shop/mysql:preflight" || got[1] != "databases/shop/mysql:stop" {
		t.Fatalf("steps = %v", got)
	}

	te.afterSleep = func(n int) {
		if n == 2 {
			te.settle("shop", "mysql") // the controller finishes terminating the pods
		}
	}
	ctx := context.Background()
	for _, step := range steps {
		if skipped, err := runStep(ctx, step); err != nil || skipped {
			t.Fatalf("%s: skipped=%v err=%v", step.Action.ID(), skipped, err)
		}
	}

	sts := te.sts("shop", "mysql")
	if *sts.Spec.Replicas != 0 {
		t.Errorf("replicas = %d; want 0", *sts.Spec.Replicas)
	}
	if got := sts.Annotations[replicasAnnotation]; got != "3" {
		t.Errorf("annotation = %q; want 3", got)
	}
	var doc savedDocument
	if found, err := te.state.Load(ctx, stateKey, &doc); err != nil || !found || doc["shop/mysql"].Replicas != 3 {
		t.Errorf("saved state = %+v (found=%v, err=%v)", doc, found, err)
	}
	if te.pollCount() < 2 {
		t.Errorf("the gate returned after %d polls, before the pods were gone", te.pollCount())
	}

	stop := steps[1].Action
	if stop.Risk() != dr.RiskHigh {
		t.Errorf("risk = %v", stop.Risk())
	}
	if !strings.Contains(stop.Describe(), "WARNING") || !strings.Contains(stop.Describe(), "logical backup") ||
		!strings.Contains(stop.Describe(), "from 3 replicas to 0") {
		t.Errorf("Describe must warn about checkpointing and name the size: %q", stop.Describe())
	}
	if stop.Equivalent() != "kubectl scale statefulset mysql --replicas=0 -n shop" {
		t.Errorf("equivalent = %q", stop.Equivalent())
	}
}

func TestStatefulSetPreflightRefusesAnUnhealthyOrUnprotectedDatabase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ready   int32
		limit   string
		finder  BackupFinder
		wantErr string
	}{
		{"not every replica is ready", 2, "", nil, "2 of 3 replicas Ready"},
		{"backup too old", 3, "6h", finderAt(epoch.Add(-9 * time.Hour)), "older than the 6h0m0s limit"},
		{"no backup known", 3, "6h", nil, "no successful backup was found"},
		{"backup lookup fails", 3, "6h", finderError(errBoom), "cannot verify the backups"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t, nil, statefulSet("shop", "mysql", 3, tc.ready, nil))
			action, err := preflightOf(t, te, NewPlanner(Options{Backups: tc.finder}), planWith(stsDB("mysql", 0, tc.limit)), "mysql")
			if !errors.Is(err, dr.ErrPreflight) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v; want dr.ErrPreflight containing %q", err, tc.wantErr)
			}
			if !strings.Contains(action.Describe(), "BLOCKED") {
				t.Errorf("the dry run must show the block: %q", action.Describe())
			}
			if got := te.sts("shop", "mysql"); *got.Spec.Replicas != 3 {
				t.Error("a blocked pre-flight must not change anything")
			}
		})
	}
}

func TestStatefulSetPreflightPassesAndSaysWhenBackupsAreNotChecked(t *testing.T) {
	te := newTestEnv(t, nil, statefulSet("shop", "mysql", 3, 3, nil))
	action, err := preflightOf(t, te, NewPlanner(Options{}), planWith(stsDB("mysql", 0, "")), "mysql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(action.Describe(), "backups are not checked") {
		t.Errorf("Describe = %q", action.Describe())
	}
}

func TestStatefulSetStopIsIdempotentAndKeepsTheRecordedSize(t *testing.T) {
	te := newTestEnv(t, nil, statefulSet("shop", "mysql", 0, 0, map[string]string{replicasAnnotation: "3"}))
	if err := te.state.Save(context.Background(), stateKey, savedDocument{"shop/mysql": {Replicas: 3}}); err != nil {
		t.Fatal(err)
	}
	steps := downSteps(t, te, NewPlanner(Options{}), planWith(stsDB("mysql", 0, "1h")))

	for _, step := range steps {
		if skipped, err := runStep(context.Background(), step); err != nil || !skipped {
			t.Errorf("%s on a stopped database: skipped=%v err=%v", step.Action.ID(), skipped, err)
		}
	}
	// Even a forced Apply must not overwrite the size with 0.
	if err := steps[1].Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := te.sts("shop", "mysql").Annotations[replicasAnnotation]; got != "3" {
		t.Errorf("annotation = %q; the recorded size was lost", got)
	}
	var doc savedDocument
	_, _ = te.state.Load(context.Background(), stateKey, &doc)
	if doc["shop/mysql"].Replicas != 3 {
		t.Errorf("state = %+v", doc)
	}
}

func TestStatefulSetStopDoesNothingWhenTheSizeCannotBeSaved(t *testing.T) {
	te := newTestEnv(t, nil, statefulSet("shop", "mysql", 3, 3, nil))
	te.state.failSave = errBoom
	steps := downSteps(t, te, NewPlanner(Options{}), planWith(stsDB("mysql", 0, "")))

	err := steps[1].Action.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	if got := te.sts("shop", "mysql"); *got.Spec.Replicas != 3 || got.Annotations[replicasAnnotation] != "" {
		t.Error("the StatefulSet was modified although the size could not be saved")
	}
}

func TestStatefulSetStopWorksWithoutAStateStore(t *testing.T) {
	te := newTestEnv(t, nil, statefulSet("shop", "mysql", 2, 2, nil))
	te.env.State = nil
	steps := downSteps(t, te, NewPlanner(Options{}), planWith(stsDB("mysql", 0, "")))

	if err := steps[1].Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := te.sts("shop", "mysql").Annotations[replicasAnnotation]; got != "2" {
		t.Errorf("without a state store the annotation is the only record, got %q", got)
	}
}

// ---- up ---------------------------------------------------------------------------------------------------------

func upSteps(t *testing.T, te *testEnv, p *Planner, db dr.Database) []dr.Step {
	t.Helper()
	steps, err := p.Steps(context.Background(), te.env, planWith(db), dr.DirUp, dr.Scope{})
	if err != nil || len(steps) != 1 {
		t.Fatalf("Steps(up) = %v, %v", stepIDs(steps), err)
	}
	return steps
}

func TestStatefulSetStartRestoresFromTheAnnotationAndClearsIt(t *testing.T) {
	te := newTestEnv(t, nil, statefulSet("shop", "mysql", 0, 0, map[string]string{replicasAnnotation: "3", "keep": "me"}))
	if err := te.state.Save(context.Background(), stateKey, savedDocument{"shop/mysql": {Replicas: 3}}); err != nil {
		t.Fatal(err)
	}
	steps := upSteps(t, te, NewPlanner(Options{}), stsDB("mysql", 0, ""))

	te.afterSleep = func(n int) {
		if n == 2 {
			te.settle("shop", "mysql")
		}
	}
	if skipped, err := runStep(context.Background(), steps[0]); err != nil || skipped {
		t.Fatalf("start: skipped=%v err=%v", skipped, err)
	}

	sts := te.sts("shop", "mysql")
	if *sts.Spec.Replicas != 3 {
		t.Errorf("replicas = %d; want 3", *sts.Spec.Replicas)
	}
	if _, left := sts.Annotations[replicasAnnotation]; left || sts.Annotations["keep"] != "me" {
		t.Errorf("annotations = %v; only %s may be removed", sts.Annotations, replicasAnnotation)
	}
	if te.state.has(stateKey) {
		t.Error("the saved run state must be forgotten once the database is started")
	}
	if te.pollCount() < 2 {
		t.Errorf("the gate did not wait for readiness (%d polls)", te.pollCount())
	}
}

func TestStatefulSetStartFallsBackToTheSavedRunState(t *testing.T) {
	// A restored or recreated cluster has lost the annotation; the machine that ran "down" still knows.
	te := newTestEnv(t, nil, statefulSet("shop", "mysql", 0, 0, nil))
	if err := te.state.Save(context.Background(), stateKey, savedDocument{"shop/mysql": {Replicas: 5}}); err != nil {
		t.Fatal(err)
	}
	steps := upSteps(t, te, NewPlanner(Options{}), stsDB("mysql", 0, ""))
	if !strings.Contains(steps[0].Action.Describe(), "5, from the saved run state") {
		t.Errorf("Describe = %q", steps[0].Action.Describe())
	}
	if steps[0].Action.Equivalent() != "kubectl scale statefulset mysql --replicas=5 -n shop" {
		t.Errorf("equivalent = %q", steps[0].Action.Equivalent())
	}

	if err := steps[0].Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := *te.sts("shop", "mysql").Spec.Replicas; got != 5 {
		t.Errorf("replicas = %d; want 5", got)
	}
}

func TestStatefulSetStartRefusesWhenTheSizeIsUnknown(t *testing.T) {
	for name, annotations := range map[string]map[string]string{
		"no annotation":      nil,
		"garbage annotation": {replicasAnnotation: "many"},
		"zero annotation":    {replicasAnnotation: "0"},
	} {
		t.Run(name, func(t *testing.T) {
			te := newTestEnv(t, nil, statefulSet("shop", "mysql", 0, 0, annotations))
			steps := upSteps(t, te, NewPlanner(Options{}), stsDB("mysql", 0, ""))

			err := steps[0].Action.Apply(context.Background())
			if err == nil || !strings.Contains(err.Error(), "unknown") || !strings.Contains(err.Error(), "kubectl scale statefulset mysql --replicas=<N> -n shop") {
				t.Fatalf("err = %v", err)
			}
			if got := *te.sts("shop", "mysql").Spec.Replicas; got != 0 {
				t.Errorf("replicas = %d; it must not guess", got)
			}
			if n := countVerb(te.client.Actions(), "patch"); n != 0 {
				t.Errorf("%d patches were sent", n)
			}
		})
	}
}

func TestStatefulSetStartDoneAndGate(t *testing.T) {
	// Fully up: skipped.
	up := newTestEnv(t, nil, statefulSet("shop", "mysql", 3, 3, nil))
	if skipped, err := runStep(context.Background(), upSteps(t, up, NewPlanner(Options{}), stsDB("mysql", 0, ""))[0]); err != nil || !skipped {
		t.Errorf("a running database: skipped=%v err=%v", skipped, err)
	}

	// Scaled up but not ready: Done is false, Apply sends nothing, the gate waits and then times out.
	slow := newTestEnv(t, nil, statefulSet("shop", "mysql", 3, 1, nil))
	steps := upSteps(t, slow, NewPlanner(Options{GateTimeout: 20 * time.Second, GatePoll: 5 * time.Second}), stsDB("mysql", 0, ""))
	if done, _ := steps[0].Action.Done(context.Background()); done {
		t.Error("Done must be false while replicas are not Ready")
	}
	if err := steps[0].Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countVerb(slow.client.Actions(), "patch"); n != 0 {
		t.Errorf("%d patches for a StatefulSet that was already scaled up", n)
	}
	if err := steps[0].Gate.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "1 of 3 replicas Ready") {
		t.Errorf("gate err = %v", err)
	}
}

func TestStatefulSetDefaultsToOneReplicaWhenUnset(t *testing.T) {
	sts := statefulSet("shop", "mysql", 0, 0, nil)
	sts.Spec.Replicas = nil
	if desired(sts) != 1 {
		t.Errorf("desired = %d; the API defaults spec.replicas to 1", desired(sts))
	}
}

func TestStatefulSetNotFound(t *testing.T) {
	te := newTestEnv(t, nil)
	steps := downSteps(t, te, NewPlanner(Options{}), planWith(stsDB("mysql", 0, "")))
	_, err := steps[1].Action.Done(context.Background())
	if err == nil || !strings.Contains(err.Error(), "StatefulSet shop/mysql was not found") {
		t.Errorf("err = %v", err)
	}
}

func TestADownAndUpCycleRestoresTheOriginalSize(t *testing.T) {
	te := newTestEnv(t, nil, statefulSet("shop", "mysql", 3, 3, nil), statefulSet("shop", "redis", 2, 2, nil))
	te.afterSleep = func(int) { te.settle("shop", "mysql"); te.settle("shop", "redis") }
	plan := planWith(stsDB("mysql", 1, ""), stsDB("redis", 0, ""))
	p := NewPlanner(Options{})
	ctx := context.Background()

	for _, step := range downSteps(t, te, p, plan) {
		if _, err := runStep(ctx, step); err != nil {
			t.Fatalf("down %s: %v", step.Action.ID(), err)
		}
	}
	for _, name := range []string{"mysql", "redis"} {
		if got := *te.sts("shop", name).Spec.Replicas; got != 0 {
			t.Fatalf("%s replicas after down = %d", name, got)
		}
	}
	var doc savedDocument
	if _, err := te.state.Load(ctx, stateKey, &doc); err != nil || len(doc) != 2 {
		t.Fatalf("one document must hold both databases, got %+v (%v)", doc, err)
	}

	up, err := p.Steps(ctx, te.env, plan, dr.DirUp, dr.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := stepIDs(up); ids[0] != "databases/shop/redis:start" {
		t.Fatalf("the lower tier must start first, got %v", ids)
	}
	// Starting the first database forgets only its own entry.
	if _, err := runStep(ctx, up[0]); err != nil {
		t.Fatal(err)
	}
	doc = nil
	if _, err := te.state.Load(ctx, stateKey, &doc); err != nil || len(doc) != 1 || doc["shop/mysql"].Replicas != 3 {
		t.Errorf("after the first start the state = %+v (%v)", doc, err)
	}
	if _, err := runStep(ctx, up[1]); err != nil {
		t.Fatal(err)
	}
	if te.state.has(stateKey) {
		t.Error("the document must disappear with its last entry")
	}

	if m, r := *te.sts("shop", "mysql").Spec.Replicas, *te.sts("shop", "redis").Spec.Replicas; m != 3 || r != 2 {
		t.Errorf("sizes after the cycle: mysql=%d redis=%d; want 3 and 2", m, r)
	}
}

func TestStatefulSetObjectsAreUntouchedByDryRunStyleReads(t *testing.T) {
	// Planning (what a dry run does) and Done must be read-only.
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg")}, statefulSet("shop", "mysql", 3, 3, nil))
	plan := planWith(stsDB("mysql", 0, ""), cnpgDB("pg", 0, ""))
	for _, dir := range []dr.Direction{dr.DirDown, dr.DirUp} {
		steps, err := NewPlanner(Options{}).Steps(context.Background(), te.env, plan, dir, dr.Scope{})
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range steps {
			if _, err := step.Action.Done(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, verb := range []string{"patch", "update", "create", "delete"} {
		if n := countVerb(te.client.Actions(), verb) + countVerb(te.dyn.Actions(), verb); n != 0 {
			t.Errorf("planning issued %d %s call(s)", n, verb)
		}
	}
}
