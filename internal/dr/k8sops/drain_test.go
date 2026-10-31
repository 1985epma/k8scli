package k8sops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

func drainer(te *testEnv, opts Options) *drainNode {
	return &drainNode{
		nodeAction: nodeAction{client: te.client, name: "w1", state: newLedger(te.env, newMemStore(), "nodes"), c: clockOf(te.env)},
		opts:       opts.withDefaults(),
	}
}

func evictions(te *testEnv) []string {
	var out []string
	for _, a := range te.client.Actions() {
		if a.GetVerb() == "create" && a.GetSubresource() == "eviction" {
			out = append(out, a.(k8stesting.CreateAction).GetObject().(interface{ GetName() string }).GetName())
		}
	}
	return out
}

func podsOnNode(te *testEnv) int {
	list, _ := te.client.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	return len(list.Items)
}

func tooMany() error {
	return apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 1)
}

func TestDrainLeavesDaemonSetMirrorAndFinishedPods(t *testing.T) {
	controller := true
	daemon := nodePod("kube-system", "proxy", "w1")
	daemon.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "proxy", Controller: &controller}}
	mirror := nodePod("kube-system", "etcd", "w1")
	mirror.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "hash"}
	finished := nodePod("jobs", "done", "w1")
	finished.Status.Phase = corev1.PodSucceeded
	elsewhere := nodePod("shop", "other-node", "w2")
	te := newEnv(t, node("w1", nil), nodePod("shop", "api", "w1"), daemon, mirror, finished, elsewhere)

	if err := drainer(te, Options{}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(evictions(te), ","); got != "api" {
		t.Errorf("evicted = %q; only the regular pod of this node", got)
	}
	if podsOnNode(te) != 4 {
		t.Errorf("pods left = %d; the DaemonSet, mirror, finished and other-node pods stay", podsOnNode(te))
	}
}

func TestDrainRefusesEmptyDirAndUnmanagedPodsBeforeEvictingAnything(t *testing.T) {
	withDisk := nodePod("shop", "cache", "w1")
	withDisk.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	lonely := nodePod("shop", "lonely", "w1")
	lonely.OwnerReferences = nil
	te := newEnv(t, node("w1", nil), nodePod("shop", "api", "w1"), withDisk, lonely)

	err := drainer(te, Options{}).Apply(context.Background())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"shop/cache uses an emptyDir", "shop/lonely has no controller", "AllowEmptyDir", "AllowUnmanagedPods", "nothing was evicted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}
	if got := evictions(te); len(got) != 0 {
		t.Errorf("nothing may be evicted when the drain is refused, got %v", got)
	}
	if done, _ := drainer(te, Options{}).Done(context.Background()); done {
		t.Error("a node with blocking pods is not drained")
	}

	if err := drainer(te, Options{AllowEmptyDir: true, AllowUnmanagedPods: true}).Apply(context.Background()); err != nil {
		t.Fatalf("with both options the drain proceeds: %v", err)
	}
	if podsOnNode(te) != 0 {
		t.Errorf("pods left = %d", podsOnNode(te))
	}
}

func TestDrainRetriesWhileAPodDisruptionBudgetBlocksThenSucceeds(t *testing.T) {
	te := newEnv(t, node("w1", nil), nodePod("shop", "api", "w1"))
	calls := 0
	te.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		calls++
		if calls <= 3 {
			return true, nil, tooMany()
		}
		return false, nil, nil // the budget has room now: fall through to the normal eviction
	})

	if err := drainer(te, Options{}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Errorf("eviction attempts = %d; want 3 refused + 1 accepted", calls)
	}
	// Backoff doubles from 2s: 2 + 4 + 8.
	if te.clock.slept != 14*time.Second {
		t.Errorf("slept %s; want 14s of exponential backoff", te.clock.slept)
	}
}

func TestDrainTimesOutWhenTheBudgetNeverAllowsIt(t *testing.T) {
	te := newEnv(t, node("w1", nil), nodePod("shop", "api", "w1"))
	te.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "eviction" {
			return true, nil, tooMany()
		}
		return false, nil, nil
	})

	err := drainer(te, Options{DrainTimeout: 90 * time.Second}).Apply(context.Background())
	if err == nil {
		t.Fatal("expected a timeout")
	}
	for _, want := range []string{"timed out after 1m30s", "node w1", "PodDisruptionBudget refused", "shop/api"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}
	if te.clock.slept < 90*time.Second {
		t.Errorf("gave up after %s, before the timeout", te.clock.slept)
	}
}

func TestDrainWaitsForTerminatingPods(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	stuck := nodePod("shop", "slow", "w1")
	stuck.DeletionTimestamp = &now
	te := newEnv(t, node("w1", nil), stuck)
	te.clock.onSleep = func(total time.Duration) {
		if total >= 6*time.Second {
			_ = te.client.Tracker().Delete(podsGVR, "shop", "slow")
		}
	}
	if err := drainer(te, Options{}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := evictions(te); len(got) != 0 {
		t.Errorf("a pod that is already terminating is waited for, not evicted again: %v", got)
	}
	if te.clock.slept < 6*time.Second {
		t.Errorf("returned before the pod was gone (slept %s)", te.clock.slept)
	}
}

func TestDrainToleratesPodsThatDisappearAndStopsOnOtherErrors(t *testing.T) {
	te := newEnv(t, node("w1", nil), nodePod("shop", "api", "w1"))
	te.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		_ = te.client.Tracker().Delete(podsGVR, "shop", "api")
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "api")
	})
	if err := drainer(te, Options{}).Apply(context.Background()); err != nil {
		t.Errorf("a pod that is already gone is fine: %v", err)
	}

	broken := newEnv(t, node("w1", nil), nodePod("shop", "api", "w1"))
	broken.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "eviction" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods/eviction"}, "api", errors.New("rbac"))
		}
		return false, nil, nil
	})
	if err := drainer(broken, Options{}).Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "evict shop/api") {
		t.Errorf("an error other than a budget refusal must stop the drain: %v", err)
	}
}

func TestDrainDoneAndCancellation(t *testing.T) {
	te := newEnv(t, node("w1", nil), nodePod("shop", "api", "w1"))
	d := drainer(te, Options{})
	ctx := context.Background()

	if done, err := d.Done(ctx); err != nil || done {
		t.Errorf("a node with a pod is not drained: %v %v", done, err)
	}
	if err := d.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if done, err := d.Done(ctx); err != nil || !done {
		t.Errorf("a drained node is done: %v %v", done, err)
	}

	gone := drainer(newEnv(t), Options{})
	if done, err := gone.Done(ctx); err != nil || !done {
		t.Errorf("a node that no longer exists counts as done: %v %v", done, err)
	}

	// Cancelling while the budget blocks returns promptly.
	blocked := newEnv(t, node("w1", nil), nodePod("shop", "api", "w1"))
	blocked.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "eviction" {
			return true, nil, tooMany()
		}
		return false, nil, nil
	})
	cancelCtx, cancel := context.WithCancel(ctx)
	blocked.clock.onSleep = func(time.Duration) { cancel() }
	blockedDrain := drainer(blocked, Options{})
	blockedDrain.c.sleep = func(c context.Context, d time.Duration) error {
		cancel()
		return c.Err()
	}
	if err := blockedDrain.Apply(cancelCtx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want context.Canceled", err)
	}
}

func TestDrainStepPlanIntegration(t *testing.T) {
	// Planner + engine-like loop: cordon every worker, drain each, with a budget that lets go midway.
	te := newEnv(t, controlPlane("cp"), node("w1", nil), node("w2", nil), nodePod("shop", "a", "w1"), nodePod("shop", "b", "w2"))
	refusals := 2
	te.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "eviction" && refusals > 0 {
			refusals--
			return true, nil, tooMany()
		}
		return false, nil, nil
	})
	steps := stepsFor(t, te, NewNodesPlanner(Options{}), withProvider(), dr.DirDown, dr.Scope{})
	if err := runSteps(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	if podsOnNode(te) != 0 || !unschedulable(t, te, "w1") || !unschedulable(t, te, "w2") {
		t.Error("both workers must be cordoned and empty")
	}
}
