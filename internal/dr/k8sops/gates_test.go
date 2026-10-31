package k8sops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

var apiRef = WorkloadRef{KindDeployment, "shop", "api"}

func setReady(t *testing.T, te *testEnv, name string, ready int32) {
	t.Helper()
	d, err := te.client.AppsV1().Deployments("shop").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d.Status.ReadyReplicas = ready
	if _, err := te.client.AppsV1().Deployments("shop").UpdateStatus(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestPodsReadyGateWaitsForReadyReplicas(t *testing.T) {
	d := deploy("shop", "api", 3)
	d.Status.ReadyReplicas = 0
	te := newEnv(t, d)
	te.clock.onSleep = func(total time.Duration) {
		if total >= 4*time.Second {
			setReady(t, te, "api", 3)
		}
	}
	gate := NewPodsReadyGate(te.env, []WorkloadRef{apiRef}, time.Minute)
	if err := gate.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if te.clock.slept < 4*time.Second {
		t.Errorf("returned too early (slept %s)", te.clock.slept)
	}
	if !strings.Contains(gate.Name(), "shop/api") {
		t.Errorf("name = %q", gate.Name())
	}
}

func TestPodsReadyGateTimesOutAndSaysWhatIsMissing(t *testing.T) {
	d := deploy("shop", "api", 3)
	d.Status.ReadyReplicas = 1
	te := newEnv(t, d, statefulSet("shop", "db", 1))
	gate := NewPodsReadyGate(te.env, []WorkloadRef{apiRef, {KindStatefulSet, "shop", "db"}}, 30*time.Second)

	err := gate.Wait(context.Background())
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !strings.Contains(err.Error(), "timed out after 30s") || !strings.Contains(err.Error(), "shop/api 1/3 ready") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(err.Error(), "shop/db 1/1") {
		t.Errorf("the StatefulSet that is ready must not be listed as waiting: %v", err)
	}
	if te.clock.slept < 30*time.Second {
		t.Errorf("gave up after %s", te.clock.slept)
	}
}

func TestPodsReadyGateWaitsForTheControllerToObserveTheSpec(t *testing.T) {
	d := deploy("shop", "api", 1)
	d.Generation = 5
	d.Status.ObservedGeneration = 4
	te := newEnv(t, d)
	te.clock.onSleep = func(time.Duration) {
		cur, _ := te.client.AppsV1().Deployments("shop").Get(context.Background(), "api", metav1.GetOptions{})
		cur.Status.ObservedGeneration = 5
		_, _ = te.client.AppsV1().Deployments("shop").UpdateStatus(context.Background(), cur, metav1.UpdateOptions{})
	}
	if err := NewPodsReadyGate(te.env, []WorkloadRef{apiRef}, time.Minute).Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if te.clock.slept == 0 {
		t.Error("a stale status must not pass the gate")
	}
}

func TestPodsReadyGateErrors(t *testing.T) {
	te := newEnv(t)
	err := NewPodsReadyGate(te.env, []WorkloadRef{apiRef}, 10*time.Second).Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "shop/api") || !strings.Contains(err.Error(), "not found") {
		t.Errorf("a missing workload is reported: %v", err)
	}

	noClient := newEnv(t)
	noClient.env.Client = nil
	if err := NewPodsReadyGate(noClient.env, []WorkloadRef{apiRef}, time.Minute).Wait(context.Background()); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("nil client = %v; want ErrUnreachable", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := deploy("shop", "api", 2)
	d.Status.ReadyReplicas = 0
	if err := NewPodsReadyGate(newEnv(t, d).env, []WorkloadRef{apiRef}, time.Hour).Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled = %v", err)
	}
}

func TestPodsGoneGateFallsBackToStatusWithoutASelector(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}, Status: appsv1.DeploymentStatus{Replicas: 2}}
	te := newEnv(t, d)
	te.clock.onSleep = func(time.Duration) {
		cur, _ := te.client.AppsV1().Deployments("shop").Get(context.Background(), "api", metav1.GetOptions{})
		cur.Status.Replicas = 0
		_, _ = te.client.AppsV1().Deployments("shop").UpdateStatus(context.Background(), cur, metav1.UpdateOptions{})
	}
	if err := NewPodsGoneGate(te.env, []WorkloadRef{apiRef}, time.Minute).Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPodsGoneGateIgnoresFinishedPodsAndMissingWorkloads(t *testing.T) {
	done := nodePod("shop", "api", "n1")
	done.Status.Phase = corev1.PodSucceeded
	te := newEnv(t, deploy("shop", "api", 0), done)
	gate := NewPodsGoneGate(te.env, []WorkloadRef{apiRef, {KindDeployment, "shop", "ghost"}}, time.Minute)
	if err := gate.Wait(context.Background()); err != nil {
		t.Errorf("finished pods and deleted workloads do not block: %v", err)
	}

	running := newEnv(t, deploy("shop", "api", 1), nodePod("shop", "api", "n1"))
	err := NewPodsGoneGate(running.env, []WorkloadRef{apiRef}, 10*time.Second).Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "still has 1 pod(s)") {
		t.Errorf("err = %v", err)
	}
	noClient := newEnv(t)
	noClient.env.Client = nil
	if err := NewPodsGoneGate(noClient.env, []WorkloadRef{apiRef}, time.Minute).Wait(context.Background()); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("nil client = %v", err)
	}
}

func TestNodesReadyGate(t *testing.T) {
	notReady := node("w1", nil)
	notReady.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	te := newEnv(t, notReady, node("w2", nil))
	te.clock.onSleep = func(total time.Duration) {
		if total >= 8*time.Second {
			n, _ := te.client.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
			n.Status.Conditions[0].Status = corev1.ConditionTrue
			_, _ = te.client.CoreV1().Nodes().UpdateStatus(context.Background(), n, metav1.UpdateOptions{})
		}
	}
	// "gone" no longer exists: it is not waited for.
	gate := NewNodesReadyGate(te.env, []string{"w1", "w2", "gone"}, time.Minute)
	if err := gate.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if te.clock.slept < 8*time.Second {
		t.Errorf("w1 was not Ready yet (slept %s)", te.clock.slept)
	}
	if gate.Name() != "nodes ready: w1, w2, gone" {
		t.Errorf("name = %q", gate.Name())
	}

	stuck := newEnv(t, notReady)
	err := NewNodesReadyGate(stuck.env, []string{"w1"}, 20*time.Second).Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "w1 is not Ready") {
		t.Errorf("err = %v", err)
	}
}

func TestAPIReadyGate(t *testing.T) {
	te := newEnv(t)
	attempts := 0
	te.client.PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts <= 3 {
			return true, nil, errors.New("dial tcp 127.0.0.1:6443: connect: connection refused")
		}
		return false, nil, nil
	})
	if err := NewAPIReadyGate(te.env, time.Minute).Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 4 {
		t.Errorf("attempts = %d; want 3 failures then success", attempts)
	}

	down := newEnv(t)
	down.client.PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	err := NewAPIReadyGate(down.env, 30*time.Second).Wait(context.Background())
	if !errors.Is(err, dr.ErrUnreachable) || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("a cluster that never answers = %v; want ErrUnreachable with the cause", err)
	}

	noClient := newEnv(t)
	noClient.env.Client = nil
	if err := NewAPIReadyGate(noClient.env, time.Minute).Wait(context.Background()); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("nil client = %v; want ErrUnreachable", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewAPIReadyGate(down.env, time.Hour).Wait(ctx); errors.Is(err, dr.ErrUnreachable) || !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait is not 'unreachable': %v", err)
	}
	if NewAPIReadyGate(te.env, 0).Timeout != defaultAPIReadyTimeout {
		t.Error("a zero timeout means the default")
	}
}

type fakeHTTP struct {
	statuses []int
	err      error
	calls    int
}

func (f *fakeHTTP) Check(context.Context, string) (int, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	i := f.calls - 1
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	return f.statuses[i], nil
}

func TestHTTPGate(t *testing.T) {
	te := newEnv(t)
	checker := &fakeHTTP{statuses: []int{503, 503, 200}}
	te.env.HTTP = checker
	gate := NewHTTPGate(te.env, "https://shop.example/health", 200, time.Minute)
	if err := gate.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if checker.calls != 3 || gate.Name() != "GET https://shop.example/health answers 200" {
		t.Errorf("calls = %d, name = %q", checker.calls, gate.Name())
	}

	// Status 0 accepts any 2xx.
	any2xx := &fakeHTTP{statuses: []int{204}}
	te.env.HTTP = any2xx
	g := NewHTTPGate(te.env, "https://x", 0, time.Minute)
	if err := g.Wait(context.Background()); err != nil || !strings.Contains(g.Name(), "2xx") {
		t.Errorf("2xx: %v / %q", err, g.Name())
	}

	// The wrong status times out and says what it saw.
	te.env.HTTP = &fakeHTTP{statuses: []int{404}}
	err := NewHTTPGate(te.env, "https://x", 200, 10*time.Second).Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "it answered 404") {
		t.Errorf("err = %v", err)
	}

	te.env.HTTP = &fakeHTTP{err: errors.New("no route to host")}
	err = NewHTTPGate(te.env, "https://x", 200, 10*time.Second).Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no route to host") {
		t.Errorf("err = %v", err)
	}

	te.env.HTTP = nil
	if err := NewHTTPGate(te.env, "https://x", 200, time.Minute).Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "no HTTP checker") {
		t.Errorf("no checker = %v", err)
	}
}

func TestCombinedGates(t *testing.T) {
	te := newEnv(t, deploy("shop", "api", 1))
	te.env.HTTP = &fakeHTTP{statuses: []int{200}}
	pods := NewPodsReadyGate(te.env, []WorkloadRef{apiRef}, time.Minute)
	web := NewHTTPGate(te.env, "https://x", 200, time.Minute)

	if combineGates(nil) != nil {
		t.Error("no gates combine to nil")
	}
	if g := combineGates([]dr.Gate{pods}); g != dr.Gate(pods) {
		t.Error("one gate stays as it is")
	}
	both := combineGates([]dr.Gate{pods, web})
	if !strings.Contains(both.Name(), "pods ready") || !strings.Contains(both.Name(), "https://x") {
		t.Errorf("name = %q", both.Name())
	}
	if err := both.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	failing := &fakeHTTP{statuses: []int{500}}
	te.env.HTTP = failing
	bad := combineGates([]dr.Gate{pods, NewHTTPGate(te.env, "https://x", 200, 4*time.Second)})
	if err := bad.Wait(context.Background()); err == nil {
		t.Error("the combination fails when one gate fails")
	}
}

func TestDescribeRefsShortensLongLists(t *testing.T) {
	refs := []WorkloadRef{{KindDeployment, "a", "1"}, {KindDeployment, "a", "2"}, {KindDeployment, "a", "3"}, {KindDeployment, "a", "4"}, {KindDeployment, "a", "5"}}
	if got := describeRefs(refs); got != "a/1, a/2, a/3 and 2 more" {
		t.Errorf("describeRefs = %q", got)
	}
}

func TestStateAndClockFallbacks(t *testing.T) {
	// Without an Env, the clock still works with real time.
	c := clockOf(nil)
	if c.now().IsZero() || c.sleep(context.Background(), time.Millisecond) != nil {
		t.Error("the default clock must work")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := realSleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("realSleep on a cancelled context = %v", err)
	}
	c.logf("no-op %d", 1) // must not panic

	// The memory store round-trips JSON and deletes.
	m := newMemStore()
	if found, _ := m.Load(context.Background(), "k", &struct{}{}); found {
		t.Error("an absent key is not found")
	}
	if err := m.Save(context.Background(), "k", map[string]int32{"a": 1}); err != nil {
		t.Fatal(err)
	}
	var got map[string]int32
	if found, err := m.Load(context.Background(), "k", &got); err != nil || !found || got["a"] != 1 {
		t.Errorf("round trip: %v %v %v", found, err, got)
	}
	_ = m.Delete(context.Background(), "k")
	if found, _ := m.Load(context.Background(), "k", &got); found {
		t.Error("deleted key still found")
	}

	if !isUnreachable(errors.New("connection refused")) || isUnreachable(nil) || isUnreachable(context.Canceled) || isUnreachable(apiForbidden()) {
		t.Error("isUnreachable misclassifies")
	}
	if !isUnreachable(dr.ErrUnreachable) {
		t.Error("ErrUnreachable is unreachable")
	}
}
