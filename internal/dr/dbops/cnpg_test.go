package dbops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

func downSteps(t *testing.T, te *testEnv, p *Planner, plan *dr.Plan) []dr.Step {
	t.Helper()
	steps, err := p.Steps(context.Background(), te.env, plan, dr.DirDown, dr.Scope{})
	if err != nil {
		t.Fatalf("Steps(down): %v", err)
	}
	return steps
}

func TestCNPGHibernateSetsTheAnnotationAndWaitsForTheOperator(t *testing.T) {
	pods := []runtime.Object{instancePod("shop", "pg-1", "pg"), instancePod("shop", "pg-2", "pg")}
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", withLastBackup(epoch.Add(-time.Hour)))}, pods...)
	plan := planWith(cnpgDB("pg", 0, "24h"))
	steps := downSteps(t, te, NewPlanner(Options{}), plan)
	if got := stepIDs(steps); len(got) != 2 || got[0] != "databases/shop/pg:preflight" || got[1] != "databases/shop/pg:hibernate" {
		t.Fatalf("steps = %v", got)
	}

	// The operator needs a few polls: first the condition, then the pods go away.
	te.afterSleep = func(n int) {
		switch n {
		case 2:
			te.mutateCluster("shop", "pg", hibernated)
		case 3:
			ctx := context.Background()
			_ = te.client.CoreV1().Pods("shop").Delete(ctx, "pg-1", metav1.DeleteOptions{})
			_ = te.client.CoreV1().Pods("shop").Delete(ctx, "pg-2", metav1.DeleteOptions{})
		}
	}

	ctx := context.Background()
	if skipped, err := runStep(ctx, steps[0]); err != nil || skipped {
		t.Fatalf("preflight: skipped=%v err=%v", skipped, err)
	}
	hibernate := steps[1]
	if done, err := hibernate.Action.Done(ctx); err != nil || done {
		t.Fatalf("Done before = %v, %v", done, err)
	}
	if skipped, err := runStep(ctx, hibernate); err != nil || skipped {
		t.Fatalf("hibernate: skipped=%v err=%v", skipped, err)
	}

	if got := te.cluster("shop", "pg").GetAnnotations()[hibernationAnnotation]; got != "on" {
		t.Errorf("annotation = %q; want on", got)
	}
	if te.pollCount() < 3 {
		t.Errorf("the gate returned after %d polls, before the operator finished", te.pollCount())
	}
	if done, err := hibernate.Action.Done(ctx); err != nil || !done {
		t.Errorf("Done after = %v, %v", done, err)
	}
	if hibernate.Action.Equivalent() != "kubectl annotate cluster pg -n shop --overwrite cnpg.io/hibernation=on" {
		t.Errorf("equivalent = %q", hibernate.Action.Equivalent())
	}
}

func TestCNPGHibernateIsIdempotent(t *testing.T) {
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", hibernated)})
	steps := downSteps(t, te, NewPlanner(Options{}), planWith(cnpgDB("pg", 0, "")))

	for _, step := range steps {
		if skipped, err := runStep(context.Background(), step); err != nil || !skipped {
			t.Errorf("%s on an already hibernated cluster: skipped=%v err=%v", step.Action.ID(), skipped, err)
		}
	}
	if n := countVerb(te.dyn.Actions(), "patch"); n != 0 {
		t.Errorf("%d patches were sent to a cluster that was already hibernated", n)
	}

	// Asked but not finished yet: Apply must not patch again, the gate just waits.
	pending := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", withAnnotation(hibernationAnnotation, "on"))})
	hibernate := stepByID(t, downSteps(t, pending, NewPlanner(Options{}), planWith(cnpgDB("pg", 0, ""))), "databases/shop/pg:hibernate")
	if err := hibernate.Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countVerb(pending.dyn.Actions(), "patch"); n != 0 {
		t.Errorf("Apply patched an annotation that was already on (%d patches)", n)
	}
}

func TestCNPGHibernateGateTimesOutWhenTheOperatorNeverReports(t *testing.T) {
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg")})
	p := NewPlanner(Options{GateTimeout: time.Minute, GatePoll: 10 * time.Second})
	hibernate := stepByID(t, downSteps(t, te, p, planWith(cnpgDB("pg", 0, ""))), "databases/shop/pg:hibernate")

	if err := hibernate.Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := hibernate.Gate.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out after 1m0s") || !strings.Contains(err.Error(), "has not reported the hibernation") {
		t.Errorf("err = %v", err)
	}
}

func TestCNPGHibernateGateWaitsForTheInstancePodsToBeGone(t *testing.T) {
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", hibernated)}, instancePod("shop", "pg-1", "pg"))
	p := NewPlanner(Options{GateTimeout: 30 * time.Second, GatePoll: 5 * time.Second})
	gate := stepByID(t, downSteps(t, te, p, planWith(cnpgDB("pg", 0, ""))), "databases/shop/pg:hibernate").Gate

	err := gate.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "still terminating") {
		t.Errorf("the gate passed while a pod was still there: %v", err)
	}
}

// ---- pre-flight ------------------------------------------------------------------------------------------------

func preflightOf(t *testing.T, te *testEnv, p *Planner, plan *dr.Plan, ref string) (dr.Action, error) {
	t.Helper()
	steps := downSteps(t, te, p, plan)
	pre := stepByID(t, steps, "databases/shop/"+ref+":preflight").Action
	return pre, pre.Apply(context.Background())
}

func TestCNPGPreflightBlocksOldBackupsAndAllowsRecentOnes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		options   []func(*unstructured.Unstructured)
		limit     string
		finder    BackupFinder
		safety    string
		wantErr   string // substring; "" means the stop is allowed
		wantInOK  string
		wantBlock bool
	}{
		{name: "status backup too old", options: []func(*unstructured.Unstructured){withLastBackup(epoch.Add(-25 * time.Hour))}, limit: "24h",
			wantErr: "older than the 24h0m0s limit", wantBlock: true},
		{name: "status backup recent", options: []func(*unstructured.Unstructured){withLastBackup(epoch.Add(-3 * time.Hour))}, limit: "24h",
			wantInOK: "within the 24h0m0s limit"},
		{name: "no limit means no check", limit: "",
			wantInOK: "backups are not checked"},
		{name: "no backup anywhere", limit: "24h",
			wantErr: "no successful backup was found", wantBlock: true},
		{name: "newest of the per-method backups wins", options: []func(*unstructured.Unstructured){
			withBackupByMethod("barmanObjectStore", epoch.Add(-90*time.Hour)),
			withBackupByMethod("volumeSnapshot", epoch.Add(-2*time.Hour)),
		}, limit: "24h", wantInOK: "within"},
		{name: "plugin backups are found through the finder", limit: "24h",
			finder: finderAt(epoch.Add(-5 * time.Hour)), wantInOK: "backup-finder"},
		{name: "a stale finder blocks", limit: "24h",
			finder: finderAt(epoch.Add(-48 * time.Hour)), wantErr: "older than", wantBlock: true},
		{name: "a finder with nothing blocks", limit: "24h",
			finder: finderNone(), wantErr: "no successful backup", wantBlock: true},
		{name: "a failing finder blocks (fail closed)", limit: "24h",
			finder: finderError(errBoom), wantErr: "cannot verify the backups", wantBlock: true},
		{name: "the plan-wide limit applies when the database has none", options: []func(*unstructured.Unstructured){withLastBackup(epoch.Add(-10 * time.Hour))},
			limit: "", safety: "6h", wantErr: "older than the 6h0m0s limit", wantBlock: true},
		{name: "the database limit beats the plan-wide one", options: []func(*unstructured.Unstructured){withLastBackup(epoch.Add(-10 * time.Hour))},
			limit: "12h", safety: "6h", wantInOK: "within the 12h0m0s limit"},
		{name: "an invalid limit blocks", limit: "soon", wantErr: "invalid maxBackupAge", wantBlock: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", tc.options...)})
			plan := planWith(cnpgDB("pg", 0, tc.limit))
			plan.Safety.RequireBackupMaxAge = tc.safety

			action, err := preflightOf(t, te, NewPlanner(Options{Backups: tc.finder}), plan, "pg")

			if tc.wantBlock {
				if !errors.Is(err, dr.ErrPreflight) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v; want dr.ErrPreflight containing %q", err, tc.wantErr)
				}
				if !strings.Contains(action.Describe(), "BLOCKED") {
					t.Errorf("a blocked stop must say so in the dry run: %q", action.Describe())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected block: %v", err)
			}
			if !strings.Contains(action.Describe(), tc.wantInOK) {
				t.Errorf("Describe = %q; want it to contain %q", action.Describe(), tc.wantInOK)
			}
		})
	}
}

func TestCNPGPreflightReadsTheBackupObjectsOfTheRightCluster(t *testing.T) {
	objects := []runtime.Object{
		cnpgCluster("shop", "pg"),
		backupObject("shop", "pg-old", "pg", "completed", epoch.Add(-30*time.Hour)),
		backupObject("shop", "pg-good", "pg", "completed", epoch.Add(-2*time.Hour)),
		backupObject("shop", "pg-failed", "pg", "failed", epoch.Add(-time.Minute)),
		backupObject("shop", "pg-running", "pg", "started", time.Time{}),
		backupObject("shop", "other-fresh", "other", "completed", epoch.Add(-time.Minute)),
	}
	te := newTestEnv(t, objects)
	action, err := preflightOf(t, te, NewPlanner(Options{}), planWith(cnpgDB("pg", 0, "6h")), "pg")
	if err != nil {
		t.Fatalf("the 2h-old completed backup must satisfy a 6h limit: %v", err)
	}
	if !strings.Contains(action.Describe(), "2.0h") && !strings.Contains(action.Describe(), "120m") {
		t.Errorf("Describe = %q; want the 2h backup to be the one used", action.Describe())
	}

	// With only a failed backup and another cluster's fresh one, nothing qualifies.
	none := newTestEnv(t, []runtime.Object{
		cnpgCluster("shop", "pg"),
		backupObject("shop", "pg-failed", "pg", "failed", epoch.Add(-time.Minute)),
		backupObject("shop", "other-fresh", "other", "completed", epoch.Add(-time.Minute)),
	})
	if _, err := preflightOf(t, none, NewPlanner(Options{}), planWith(cnpgDB("pg", 0, "6h")), "pg"); !errors.Is(err, dr.ErrPreflight) {
		t.Errorf("err = %v; want dr.ErrPreflight", err)
	}
}

func TestCNPGPreflightIsSkippedOnceTheClusterIsHibernated(t *testing.T) {
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", hibernated, withLastBackup(epoch.Add(-90*time.Hour)))})
	steps := downSteps(t, te, NewPlanner(Options{}), planWith(cnpgDB("pg", 0, "1h")))
	if skipped, err := runStep(context.Background(), stepByID(t, steps, "databases/shop/pg:preflight")); err != nil || !skipped {
		t.Errorf("a resumed run must not be blocked by an old backup after the hibernation: skipped=%v err=%v", skipped, err)
	}
}

// ---- up -------------------------------------------------------------------------------------------------------------

func TestCNPGStartSwitchesTheAnnotationOffAndWaitsForHealth(t *testing.T) {
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", hibernated)})
	steps, err := NewPlanner(Options{}).Steps(context.Background(), te.env, planWith(cnpgDB("pg", 0, "")), dr.DirUp, dr.Scope{})
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps = %v, %v", stepIDs(steps), err)
	}
	start := steps[0]

	if done, err := start.Action.Done(context.Background()); err != nil || done {
		t.Fatalf("Done on a hibernated cluster = %v, %v", done, err)
	}

	te.afterSleep = func(n int) {
		if n == 2 {
			te.startPods("shop", "pg", 3)
			te.mutateCluster("shop", "pg", func(o *unstructured.Unstructured) {
				withStatus("instances", int64(3))(o)
				withStatus("readyInstances", int64(3))(o)
				withStatus("phase", "Cluster in healthy state")(o)
				withConditions(cond("Ready", "True", "ClusterIsReady", epoch))(o)
			})
		}
	}
	if skipped, err := runStep(context.Background(), start); err != nil || skipped {
		t.Fatalf("start: skipped=%v err=%v", skipped, err)
	}

	if got := te.cluster("shop", "pg").GetAnnotations()[hibernationAnnotation]; got != "off" {
		t.Errorf("annotation = %q; want off", got)
	}
	if te.pollCount() < 2 {
		t.Errorf("the gate did not wait for the instances (%d polls)", te.pollCount())
	}
	if done, _ := start.Action.Done(context.Background()); !done {
		t.Error("Done must be true once the cluster is healthy")
	}
	if start.Action.Risk() != dr.RiskLow {
		t.Errorf("risk = %v", start.Action.Risk())
	}
}

func TestCNPGHealthRules(t *testing.T) {
	for name, tc := range map[string]struct {
		obj  func() *unstructured.Unstructured
		want bool
	}{
		"ready and all instances up": {func() *unstructured.Unstructured { return cnpgCluster("s", "c") }, true},
		"annotation still on": {func() *unstructured.Unstructured {
			return cnpgCluster("s", "c", withAnnotation(hibernationAnnotation, "on"))
		}, false},
		"annotation off": {func() *unstructured.Unstructured {
			return cnpgCluster("s", "c", withAnnotation(hibernationAnnotation, "off"))
		}, true},
		"ready false": {func() *unstructured.Unstructured {
			return cnpgCluster("s", "c", withConditions(cond("Ready", "False", "x", epoch)))
		}, false},
		"fewer ready than instances": {func() *unstructured.Unstructured {
			return cnpgCluster("s", "c", withStatus("readyInstances", int64(2)))
		}, false},
		"zero instances": {func() *unstructured.Unstructured {
			return cnpgCluster("s", "c", withStatus("instances", int64(0)), withStatus("readyInstances", int64(0)))
		}, false},
		"old operator: phase text": {func() *unstructured.Unstructured {
			return cnpgCluster("s", "c", withConditions())
		}, true},
		"old operator: not healthy": {func() *unstructured.Unstructured {
			return cnpgCluster("s", "c", withConditions(), withStatus("phase", "Setting up primary"))
		}, false},
		"counters omitted by the API (they are 0)": {func() *unstructured.Unstructured {
			return cnpgCluster("s", "c", withoutStatus("instances", "readyInstances"))
		}, false},
		"stale Ready True while still hibernated": {func() *unstructured.Unstructured {
			return cnpgCluster("s", "c", withConditions(cond("Ready", "True", "ClusterIsReady", epoch), cond(hibernationCondition, "True", "Hibernated", epoch)))
		}, false},
		"hibernation condition gone after rehydration": {func() *unstructured.Unstructured {
			return cnpgCluster("s", "c", withConditions(cond("Ready", "True", "ClusterIsReady", epoch)))
		}, true},
		"spec asks for more instances than the status shows": {func() *unstructured.Unstructured {
			return cnpgCluster("s", "c", withStatus("instances", int64(2)), withStatus("readyInstances", int64(2)))
		}, false},
		"counters as floats (JSON decoding)": {func() *unstructured.Unstructured {
			return cnpgCluster("s", "c", withStatus("instances", float64(3)), withStatus("readyInstances", float64(3)))
		}, true},
	} {
		if got := isHealthy(tc.obj()); got != tc.want {
			t.Errorf("%s: isHealthy = %v; want %v", name, got, tc.want)
		}
	}
}

func TestCNPGStartIsIdempotentOnAHealthyCluster(t *testing.T) {
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg")},
		runningPod("shop", "pg-1", "pg"), runningPod("shop", "pg-2", "pg"), runningPod("shop", "pg-3", "pg"))
	steps, _ := NewPlanner(Options{}).Steps(context.Background(), te.env, planWith(cnpgDB("pg", 0, "")), dr.DirUp, dr.Scope{})
	if skipped, err := runStep(context.Background(), steps[0]); err != nil || !skipped {
		t.Errorf("skipped=%v err=%v", skipped, err)
	}
}

// ---- the stale-status pitfall (found live on CloudNativePG v1.30.1) ----------------------------------------------

func TestCNPGStartIsNotFooledByAStaleReadyCondition(t *testing.T) {
	// One second after "off": the operator still shows the Ready value from before the hibernation, and
	// the API omits the instance counters because they are 0. Nothing exists yet.
	stale := cnpgCluster("shop", "pg", withAnnotation(hibernationAnnotation, "off"), withoutStatus("instances", "readyInstances"),
		withConditions(cond("Ready", "True", "ClusterIsReady", epoch.Add(-time.Hour))))
	te := newTestEnv(t, []runtime.Object{stale})
	p := NewPlanner(Options{GateTimeout: 30 * time.Second, GatePoll: 5 * time.Second})
	steps, _ := p.Steps(context.Background(), te.env, planWith(cnpgDB("pg", 0, "")), dr.DirUp, dr.Scope{})

	if done, err := steps[0].Action.Done(context.Background()); err != nil || done {
		t.Fatalf("Done = %v, %v; a stale Ready must not count as started", done, err)
	}
	err := steps[0].Gate.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "0 of 3 instances ready") {
		t.Fatalf("the gate must keep waiting while no instance exists, got %v", err)
	}

	// It passes only once the instances exist, the counters agree and the pods are Running and Ready.
	base := te.pollCount()
	te.afterSleep = func(n int) {
		switch n - base {
		case 2:
			te.mutateCluster("shop", "pg", func(o *unstructured.Unstructured) {
				withStatus("instances", int64(3))(o)
				withStatus("readyInstances", int64(1))(o)
			})
		case 4:
			te.startPods("shop", "pg", 3)
			te.mutateCluster("shop", "pg", withStatus("readyInstances", int64(3)))
		}
	}
	if err := steps[0].Gate.Wait(context.Background()); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if te.pollCount()-base < 4 {
		t.Errorf("the gate passed after %d polls, before the instances were ready", te.pollCount()-base)
	}
}

func TestCNPGHealthyAlsoNeedsTheInstancePodsToBeRunning(t *testing.T) {
	cluster := cnpgCluster("shop", "pg") // counters say 3/3 and Ready is True
	c := func(te *testEnv) *cnpgTarget {
		return &cnpgTarget{p: NewPlanner(Options{}), env: te.env, db: cnpgDB("pg", 0, "")}
	}

	none := newTestEnv(t, nil)
	if ok, why, err := c(none).healthy(context.Background(), cluster); err != nil || ok || !strings.Contains(why, "0 of 3 instance pods") {
		t.Errorf("no pods: ok=%v why=%q err=%v", ok, why, err)
	}

	notReady := instancePod("shop", "pg-3", "pg") // Pending, not Ready
	terminating := runningPod("shop", "pg-4", "pg")
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	partial := newTestEnv(t, nil, runningPod("shop", "pg-1", "pg"), runningPod("shop", "pg-2", "pg"), notReady, terminating)
	if ok, why, _ := c(partial).healthy(context.Background(), cluster); ok || !strings.Contains(why, "2 of 3 instance pods") {
		t.Errorf("pods that are not Ready or are terminating must not count: ok=%v why=%q", ok, why)
	}

	all := newTestEnv(t, nil, runningPod("shop", "pg-1", "pg"), runningPod("shop", "pg-2", "pg"), runningPod("shop", "pg-3", "pg"))
	if ok, why, err := c(all).healthy(context.Background(), cluster); err != nil || !ok {
		t.Errorf("all pods Running and Ready: ok=%v why=%q err=%v", ok, why, err)
	}

	// Without a Kubernetes client only the object can be judged.
	all.env.Client = nil
	if ok, _, err := c(all).healthy(context.Background(), cluster); err != nil || !ok {
		t.Errorf("no client: ok=%v err=%v", ok, err)
	}
}

func TestCNPGHibernateIsNotDoneWhileAnInstancePodRemains(t *testing.T) {
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", hibernated)}, instancePod("shop", "pg-1", "pg"))
	steps := downSteps(t, te, NewPlanner(Options{}), planWith(cnpgDB("pg", 0, "")))
	hibernate := stepByID(t, steps, "databases/shop/pg:hibernate").Action
	if done, err := hibernate.Done(context.Background()); err != nil || done {
		t.Errorf("Done = %v, %v; the Hibernated condition alone is not enough while a pod exists", done, err)
	}
}

func TestCNPGStartDoesNotPatchWhenNothingIsHibernated(t *testing.T) {
	// Unhealthy but not hibernated: Apply has nothing to switch off, the gate waits for recovery.
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", withStatus("readyInstances", int64(1)))})
	steps, _ := NewPlanner(Options{GateTimeout: 20 * time.Second, GatePoll: 5 * time.Second}).
		Steps(context.Background(), te.env, planWith(cnpgDB("pg", 0, "")), dr.DirUp, dr.Scope{})
	if err := steps[0].Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countVerb(te.dyn.Actions(), "patch"); n != 0 {
		t.Errorf("%d unnecessary patches", n)
	}
	err := steps[0].Gate.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "1 of 3 instances ready") {
		t.Errorf("gate err = %v", err)
	}
}

// ---- missing operator -----------------------------------------------------------------------------------------------

func missingResourceError() error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: 404, Reason: metav1.StatusReasonNotFound,
		Message: "the server could not find the requested resource",
	}}
}

func TestMissingOperatorIsAClearErrorNotAPanic(t *testing.T) {
	te := newTestEnv(t, nil)
	te.dyn.PrependReactor("*", "clusters", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, missingResourceError()
	})

	steps := downSteps(t, te, NewPlanner(Options{}), planWith(cnpgDB("pg", 0, "24h")))
	for _, step := range steps {
		_, err := step.Action.Done(context.Background())
		if !errors.Is(err, ErrOperatorMissing) {
			t.Errorf("%s: err = %v; want ErrOperatorMissing", step.Action.ID(), err)
		}
	}
	if !strings.Contains(ErrOperatorMissing.Error(), "not installed") {
		t.Errorf("message = %q", ErrOperatorMissing)
	}

	up, _ := NewPlanner(Options{}).Steps(context.Background(), te.env, planWith(cnpgDB("pg", 0, "")), dr.DirUp, dr.Scope{})
	if _, err := up[0].Action.Done(context.Background()); !errors.Is(err, ErrOperatorMissing) {
		t.Errorf("up: err = %v", err)
	}
	// And patching a resource that vanished is reported the same way.
	te.dyn.PrependReactor("patch", "clusters", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, missingResourceError()
	})
	cluster := &cnpgTarget{p: NewPlanner(Options{}), env: te.env, db: cnpgDB("pg", 0, "")}
	if err := cluster.annotate(context.Background(), "on"); !errors.Is(err, ErrOperatorMissing) {
		t.Errorf("annotate: err = %v", err)
	}
}

func TestAMissingClusterObjectIsNotConfusedWithAMissingOperator(t *testing.T) {
	te := newTestEnv(t, nil)
	steps := downSteps(t, te, NewPlanner(Options{}), planWith(cnpgDB("pg", 0, "")))
	_, err := steps[1].Action.Done(context.Background())
	if err == nil || errors.Is(err, ErrOperatorMissing) || !strings.Contains(err.Error(), "shop/pg was not found") {
		t.Errorf("err = %v", err)
	}
}

func TestIsMissingResource(t *testing.T) {
	named := apierrors.NewNotFound(schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "clusters"}, "pg")
	if isMissingResource(named) {
		t.Error("a missing named object is not a missing resource type")
	}
	if !isMissingResource(missingResourceError()) {
		t.Error("the server's 'could not find the requested resource' must be detected")
	}
	if isMissingResource(errBoom) || isMissingResource(apierrors.NewForbidden(schema.GroupResource{}, "x", errBoom)) {
		t.Error("other errors must not be reported as a missing operator")
	}
}
