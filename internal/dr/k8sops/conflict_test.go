package k8sops

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

// Controllers (an HPA, a rollout, an operator) rewrite objects all the time, so the API server answers a
// write with "409 Conflict: the object has been modified". A live `dr down` failed on exactly that at
// "Scale shop/web to 0". Every write k8sops makes has to ride such a conflict out.

func conflictError(group, resource string) error {
	return apierrors.NewConflict(schema.GroupResource{Group: group, Resource: resource}, "x",
		errors.New("the object has been modified; please apply your changes to the latest version and try again"))
}

// conflictFirst makes the first n matching writes fail with a Conflict (it returns how many it saw).
func conflictFirst(te *testEnv, verb, resource, subresource string, n int32) *atomic.Int32 {
	var seen atomic.Int32
	te.client.PrependReactor(verb, resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != subresource {
			return false, nil, nil
		}
		if seen.Add(1) <= n {
			return true, nil, conflictError("apps", resource)
		}
		return false, nil, nil
	})
	return &seen
}

func downSteps(t *testing.T, te *testEnv) []dr.Step {
	t.Helper()
	return onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), threeTierPlan(), dr.DirDown, dr.Scope{}))
}

func TestScaleDownRidesOutConflictsOnTheScaleSubresource(t *testing.T) {
	te := newEnv(t, threeTierObjects()...)
	seen := conflictFirst(te, "update", "deployments", "scale", 4)

	if err := runSteps(context.Background(), downSteps(t, te)); err != nil {
		t.Fatalf("a conflict that clears must not fail the step: %v", err)
	}
	if seen.Load() < 5 {
		t.Errorf("update attempts = %d; the conflicts were not retried", seen.Load())
	}
	for name, before := range map[string]string{"web": "2", "api": "3", "cache": "1"} {
		if got := te.replicas(KindDeployment, "shop", name); got != 0 {
			t.Errorf("%s replicas = %d; want 0", name, got)
		}
		if got, _ := te.annotation(KindDeployment, "shop", name); got != before {
			t.Errorf("%s annotation = %q; want the real pre-stop count %q", name, got, before)
		}
	}
}

func TestScaleDownRetriesAnnotationPatchConflictsToo(t *testing.T) {
	te := newEnv(t, threeTierObjects()...)
	seen := conflictFirst(te, "patch", "deployments", "", 3)

	if err := runSteps(context.Background(), downSteps(t, te)); err != nil {
		t.Fatal(err)
	}
	if seen.Load() < 4 {
		t.Errorf("patch attempts = %d", seen.Load())
	}
	if got, _ := te.annotation(KindDeployment, "shop", "api"); got != "3" {
		t.Errorf("annotation = %q; want 3", got)
	}
}

// The write lands, but the answer is a conflict anyway (a retry by a proxy, a lost response): the retry
// finds the workload already at 0 and must neither fail nor record 0 over the real count.
func TestScaleDownAfterAWriteThatLandedKeepsTheRealCount(t *testing.T) {
	te := newEnv(t, deploy("shop", "web", 2))
	var landed atomic.Bool
	te.client.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" || landed.Swap(true) {
			return false, nil, nil
		}
		// Apply the write for real, then answer with a conflict anyway.
		scale := action.(k8stesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
		obj, err := te.client.Tracker().Get(deploymentsGVR, action.GetNamespace(), scale.Name)
		if err != nil {
			return true, nil, err
		}
		updated := obj.(*appsv1.Deployment).DeepCopy()
		updated.Spec.Replicas = &scale.Spec.Replicas
		if err := te.client.Tracker().Update(deploymentsGVR, updated, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, nil, conflictError("apps", "deployments")
	})
	plan := testPlan(dr.App{Name: "web", Namespace: "shop", Workloads: []string{"web"}})
	steps := onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{}))

	if err := runSteps(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	if got := te.replicas(KindDeployment, "shop", "web"); got != 0 {
		t.Errorf("replicas = %d; want 0", got)
	}
	if got, _ := te.annotation(KindDeployment, "shop", "web"); got != "2" {
		t.Errorf("annotation = %q; the real count (2) must survive a retry", got)
	}
	var recorded map[string]int32
	if _, err := te.state.Load(context.Background(), appsStateKey, &recorded); err != nil || recorded["Deployment/shop/web"] != 2 {
		t.Errorf("state = %v, %v; want web=2", recorded, err)
	}
}

func TestAConflictThatNeverClearsEndsAsAClearErrorAndTheNextRunStillKnowsTheCount(t *testing.T) {
	te := newEnv(t, deploy("shop", "web", 2))
	var forever atomic.Bool
	forever.Store(true)
	var attempts atomic.Int32
	te.client.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" || !forever.Load() {
			return false, nil, nil
		}
		attempts.Add(1)
		return true, nil, conflictError("apps", "deployments")
	})
	plan := testPlan(dr.App{Name: "web", Namespace: "shop", Workloads: []string{"web"}})
	steps := onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{}))

	err := runSteps(context.Background(), steps)
	if err == nil {
		t.Fatal("a conflict that never clears must fail the step")
	}
	if !strings.Contains(err.Error(), "kept changing") || !strings.Contains(err.Error(), "scale shop/web to 0") || !apierrors.IsConflict(err) {
		t.Errorf("error = %v; want a clear message that still wraps the conflict", err)
	}
	if want := int32(conflictRetries + 1); attempts.Load() != want {
		t.Errorf("attempts = %d; want %d (bounded)", attempts.Load(), want)
	}
	if got := te.replicas(KindDeployment, "shop", "web"); got != 2 {
		t.Errorf("replicas = %d; a failed scale must change nothing", got)
	}
	if got, _ := te.annotation(KindDeployment, "shop", "web"); got != "2" {
		t.Errorf("annotation = %q; the count was recorded before the write", got)
	}

	// The engine resumes: the conflict is gone and the second run must record the same real count.
	forever.Store(false)
	if err := runSteps(context.Background(), onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{}))); err != nil {
		t.Fatal(err)
	}
	if got, _ := te.annotation(KindDeployment, "shop", "web"); got != "2" {
		t.Errorf("annotation after the resume = %q; want 2, never 0", got)
	}
	if got := te.replicas(KindDeployment, "shop", "web"); got != 0 {
		t.Errorf("replicas after the resume = %d", got)
	}
}

func TestScaleUpRidesOutConflicts(t *testing.T) {
	te := newEnv(t, threeTierObjects()...)
	ctx := context.Background()
	if err := runSteps(ctx, downSteps(t, te)); err != nil {
		t.Fatal(err)
	}
	conflictFirst(te, "update", "deployments", "scale", 5)
	conflictFirst(te, "patch", "deployments", "", 2)

	up := onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), threeTierPlan(), dr.DirUp, dr.Scope{}))
	if err := runSteps(ctx, up); err != nil {
		t.Fatalf("up: %v", err)
	}
	for name, want := range map[string]int32{"web": 2, "api": 3, "cache": 1} {
		if got := te.replicas(KindDeployment, "shop", name); got != want {
			t.Errorf("%s replicas = %d; want %d", name, got, want)
		}
	}
	if _, ok := te.annotation(KindDeployment, "shop", "web"); ok {
		t.Error("the annotation must be gone after a successful up")
	}
}

func TestStatefulSetScaleRidesOutConflicts(t *testing.T) {
	te := newEnv(t, statefulSet("shop", "db", 2))
	seen := conflictFirst(te, "update", "statefulsets", "scale", 2)
	plan := testPlan(dr.App{Name: "db", Namespace: "shop", Workloads: []string{"db"}})

	if err := runSteps(context.Background(), onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{}))); err != nil {
		t.Fatal(err)
	}
	if seen.Load() < 3 || te.replicas(KindStatefulSet, "shop", "db") != 0 {
		t.Errorf("attempts = %d, replicas = %d", seen.Load(), te.replicas(KindStatefulSet, "shop", "db"))
	}
	if got, _ := te.annotation(KindStatefulSet, "shop", "db"); got != "2" {
		t.Errorf("annotation = %q; want 2", got)
	}
}

func TestCronJobSuspendAndResumeRideOutConflicts(t *testing.T) {
	te := newEnv(t, cronJob("shop", "report", false))
	seen := conflictFirst(te, "patch", "cronjobs", "", 3)
	plan := testPlan(dr.App{Name: "shop", Namespace: "shop"})
	p := NewPreparePlanner(Options{})
	ctx := context.Background()

	if err := runSteps(ctx, stepsFor(t, te, p, plan, dr.DirDown, dr.Scope{})); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if seen.Load() < 4 {
		t.Errorf("suspend attempts = %d", seen.Load())
	}
	conflictFirst(te, "patch", "cronjobs", "", 2)
	if err := runSteps(ctx, stepsFor(t, te, p, plan, dr.DirUp, dr.Scope{})); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

func TestNodeCordonAndUncordonRideOutConflicts(t *testing.T) {
	te := newEnv(t, labLike()...)
	seen := conflictFirst(te, "patch", "nodes", "", 3)
	p := NewNodesPlanner(Options{})
	ctx := context.Background()

	down := stepsFor(t, te, p, withProvider(), dr.DirDown, dr.Scope{})
	if err := runSteps(ctx, onlyActions(down)); err != nil {
		t.Fatalf("down: %v", err)
	}
	if seen.Load() < 4 || !unschedulable(t, te, "w1") || !unschedulable(t, te, "w2") {
		t.Errorf("attempts = %d, w1 = %v, w2 = %v", seen.Load(), unschedulable(t, te, "w1"), unschedulable(t, te, "w2"))
	}
	conflictFirst(te, "patch", "nodes", "", 2)
	if err := runSteps(ctx, onlyActions(stepsFor(t, te, p, withProvider(), dr.DirUp, dr.Scope{}))); err != nil {
		t.Fatalf("up: %v", err)
	}
	if unschedulable(t, te, "w1") || unschedulable(t, te, "w2") {
		t.Error("both workers must be schedulable again")
	}
}

func TestRetryBacksOffAndStopsWhenTheContextEnds(t *testing.T) {
	var slept []time.Duration
	sleep := func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	calls := 0
	err := retryOnConflict(context.Background(), sleep, "test", func() error {
		calls++
		if calls < 4 {
			return conflictError("apps", "deployments")
		}
		return nil
	})
	if err != nil || calls != 4 || len(slept) != 3 || slept[0] >= slept[1] || slept[1] >= slept[2] {
		t.Errorf("err = %v, calls = %d, slept = %v; want 3 growing pauses", err, calls, slept)
	}

	stop := errors.New("context canceled")
	err = retryOnConflict(context.Background(), func(context.Context, time.Duration) error { return stop }, "test",
		func() error { return conflictError("apps", "deployments") })
	if !errors.Is(err, stop) {
		t.Errorf("err = %v; a cancelled wait must end the retries", err)
	}

	other := apiForbidden()
	calls = 0
	if err := retryOnConflict(context.Background(), sleep, "test", func() error { calls++; return other }); !errors.Is(err, other) || calls != 1 {
		t.Errorf("only conflicts are retried: err = %v, calls = %d", err, calls)
	}
}
