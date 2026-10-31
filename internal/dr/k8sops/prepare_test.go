package k8sops

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/1985epma/k8scli/internal/dr"
)

const cronStateKey = testContext + "/cronjobs"

func suspended(t *testing.T, te *testEnv, namespace, name string) bool {
	t.Helper()
	cj, err := te.client.BatchV1().CronJobs(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return isSuspended(cj)
}

func TestPrepareSuspendsOnlyRunningCronJobsAndResumesExactlyThose(t *testing.T) {
	kept := cronJob("shop", "backup", false)
	kept.Annotations = map[string]string{AnnotationKeepRunning: "true"}
	te := newEnv(t,
		cronJob("shop", "report", false),
		cronJob("shop", "cleanup", false),
		cronJob("shop", "paused-by-hand", true),
		kept,
		cronJob("shop", "billing", false),
		cronJob("other", "unrelated", false),
	)
	plan := testPlan(dr.App{Name: "shop", Namespace: "shop"})
	p := NewPreparePlanner(Options{ExcludeCronJobs: []string{"shop/billing"}})
	ctx := context.Background()

	down := stepsFor(t, te, p, plan, dr.DirDown, dr.Scope{})
	if got := strings.Join(ids(down), ","); got != "prepare/shop/cleanup:suspend-cronjob,prepare/shop/report:suspend-cronjob" {
		t.Fatalf("down steps = %s", got)
	}
	for _, s := range down {
		if s.Phase != dr.PhasePrepare || s.Action.Risk() != dr.RiskLow {
			t.Errorf("%s: phase %s risk %s", s.Action.ID(), s.Phase, s.Action.Risk())
		}
	}
	if err := runSteps(ctx, down); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"report": true, "cleanup": true, "paused-by-hand": true, "backup": false, "billing": false} {
		if got := suspended(t, te, "shop", name); got != want {
			t.Errorf("%s suspended = %v; want %v", name, got, want)
		}
	}
	if suspended(t, te, "other", "unrelated") {
		t.Error("CronJobs of a namespace outside the plan must not be touched")
	}

	var recorded []cronJobRef
	if found, _ := te.state.Load(ctx, cronStateKey, &recorded); !found || len(recorded) != 2 {
		t.Fatalf("state = %v", recorded)
	}

	// Up resumes exactly the two it suspended; the one paused by hand stays paused.
	up := stepsFor(t, te, p, plan, dr.DirUp, dr.Scope{})
	if len(up) != 2 {
		t.Fatalf("up steps = %v", ids(up))
	}
	if err := runSteps(ctx, up); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"report": false, "cleanup": false, "paused-by-hand": true} {
		if got := suspended(t, te, "shop", name); got != want {
			t.Errorf("after up: %s suspended = %v; want %v", name, got, want)
		}
	}
	if te.state.has(cronStateKey) {
		t.Error("the state must be empty after everything is resumed")
	}
	// A second up has nothing to do.
	if again := stepsFor(t, te, p, plan, dr.DirUp, dr.Scope{}); len(again) != 0 {
		t.Errorf("second up = %v", ids(again))
	}
}

func TestPrepareDownIsIdempotent(t *testing.T) {
	te := newEnv(t, cronJob("shop", "report", false))
	plan := testPlan(dr.App{Name: "shop", Namespace: "shop"})
	p := NewPreparePlanner(Options{})
	ctx := context.Background()

	if err := runSteps(ctx, stepsFor(t, te, p, plan, dr.DirDown, dr.Scope{})); err != nil {
		t.Fatal(err)
	}
	// Planning again finds it suspended: nothing new, and the record stays.
	if again := stepsFor(t, te, p, plan, dr.DirDown, dr.Scope{}); len(again) != 0 {
		t.Errorf("second down = %v", ids(again))
	}
	var recorded []cronJobRef
	if found, _ := te.state.Load(ctx, cronStateKey, &recorded); !found || len(recorded) != 1 {
		t.Errorf("the record was lost: %v", recorded)
	}
}

func TestPrepareScopeLimitsNamespaces(t *testing.T) {
	te := newEnv(t, cronJob("a", "job", false), cronJob("b", "job", false))
	plan := testPlan(dr.App{Name: "a", Namespace: "a"}, dr.App{Name: "b", Namespace: "b"})
	p := NewPreparePlanner(Options{})

	got := strings.Join(ids(stepsFor(t, te, p, plan, dr.DirDown, dr.Scope{Apps: []string{"b"}})), ",")
	if got != "prepare/b/job:suspend-cronjob" {
		t.Errorf("steps = %s", got)
	}
	if _, err := p.Steps(context.Background(), te.env, plan, dr.DirDown, dr.Scope{Apps: []string{"ghost"}}); err == nil {
		t.Error("an unknown app in the scope must fail")
	}

	// Up with a scope resumes only the CronJobs of the scoped namespaces.
	if err := runSteps(context.Background(), stepsFor(t, te, p, plan, dr.DirDown, dr.Scope{})); err != nil {
		t.Fatal(err)
	}
	up := strings.Join(ids(stepsFor(t, te, p, plan, dr.DirUp, dr.Scope{Apps: []string{"a"}})), ",")
	if up != "prepare/a/job:resume-cronjob" {
		t.Errorf("scoped up = %s", up)
	}
}

func TestPrepareResumeSurvivesAMissingCronJobAndAManualResume(t *testing.T) {
	te := newEnv(t, cronJob("shop", "gone", false), cronJob("shop", "manual", false))
	plan := testPlan(dr.App{Name: "shop", Namespace: "shop"})
	p := NewPreparePlanner(Options{})
	ctx := context.Background()
	if err := runSteps(ctx, stepsFor(t, te, p, plan, dr.DirDown, dr.Scope{})); err != nil {
		t.Fatal(err)
	}

	if err := te.client.BatchV1().CronJobs("shop").Delete(ctx, "gone", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f := false
	cj, _ := te.client.BatchV1().CronJobs("shop").Get(ctx, "manual", metav1.GetOptions{})
	cj.Spec.Suspend = &f
	if _, err := te.client.BatchV1().CronJobs("shop").Update(ctx, cj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	up := stepsFor(t, te, p, plan, dr.DirUp, dr.Scope{})
	if len(up) != 2 {
		t.Fatalf("up = %v", ids(up))
	}
	if err := runSteps(ctx, up); err != nil {
		t.Fatalf("a missing or already resumed CronJob must not fail the run: %v", err)
	}
	if te.state.has(cronStateKey) {
		t.Error("both records must be cleared")
	}
}

func TestPrepareDescriptions(t *testing.T) {
	te := newEnv(t, cronJob("shop", "report", false))
	plan := testPlan(dr.App{Name: "shop", Namespace: "shop"})
	down := stepsFor(t, te, NewPreparePlanner(Options{}), plan, dr.DirDown, dr.Scope{})
	a := down[0].Action
	if a.Title() != "Suspend CronJob shop/report" || !strings.Contains(a.Describe(), "records it") ||
		a.Equivalent() != `kubectl patch cronjob report -n shop -p '{"spec":{"suspend":true}}'` {
		t.Errorf("%q | %q | %q", a.Title(), a.Describe(), a.Equivalent())
	}
	if err := runSteps(context.Background(), down); err != nil {
		t.Fatal(err)
	}
	up := stepsFor(t, te, NewPreparePlanner(Options{}), plan, dr.DirUp, dr.Scope{})
	if up[0].Action.Title() != "Resume CronJob shop/report" || !strings.Contains(up[0].Action.Equivalent(), `"suspend":false`) {
		t.Errorf("%q | %q", up[0].Action.Title(), up[0].Action.Equivalent())
	}
}

func TestPrepareNeedsAClientToGoDownButNotToPlanUp(t *testing.T) {
	te := newEnv(t, cronJob("shop", "report", false))
	plan := testPlan(dr.App{Name: "shop", Namespace: "shop"})
	p := NewPreparePlanner(Options{})
	if err := runSteps(context.Background(), stepsFor(t, te, p, plan, dr.DirDown, dr.Scope{})); err != nil {
		t.Fatal(err)
	}

	te.env.Client = nil
	if _, err := p.Steps(context.Background(), te.env, plan, dr.DirDown, dr.Scope{}); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("down without a client: %v", err)
	}
	up, err := p.Steps(context.Background(), te.env, plan, dr.DirUp, dr.Scope{})
	if err != nil || len(up) != 1 {
		t.Errorf("up is planned from the state alone: %d steps, %v", len(up), err)
	}
}
