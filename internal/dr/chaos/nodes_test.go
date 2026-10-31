package chaos

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/1985epma/k8scli/internal/dr"
)

func labNodes(extra ...runtime.Object) []runtime.Object {
	base := []runtime.Object{
		controlPlane("cp"), worker("w1"), worker("w2"),
		allowedNS("chaos-test"),
		pod("chaos-test", "api-1", "api", "w1", true),
		pod("chaos-test", "api-2", "api", "w2", true),
	}
	return append(base, extra...)
}

func unschedulable(t *testing.T, te *testEnv, name string) bool {
	t.Helper()
	n, err := te.client.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return n.Spec.Unschedulable
}

func TestNodesNeverTouchTheControlPlane(t *testing.T) {
	te := newTestEnv(t, labNodes()...)
	_, err := NewDrainNode(context.Background(), te.guard(), NodeOptions{Node: "cp"})
	mustErrorIs(t, err, ErrControlPlane)
	_, err = NewCordonNode(context.Background(), te.guard(), NodeOptions{Node: "cp"})
	mustErrorIs(t, err, ErrControlPlane)

	// A taint alone is enough to make a node a control-plane node.
	tainted := worker("t1")
	tainted.Spec.Taints = []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}
	te = newTestEnv(t, append(labNodes(), tainted)...)
	_, err = NewCordonNode(context.Background(), te.guard(), NodeOptions{Node: "t1"})
	mustErrorIs(t, err, ErrControlPlane)

	_, err = NewDrainNode(context.Background(), te.guard(), NodeOptions{Node: "ghost"})
	mustErrorIs(t, err, dr.ErrPreflight)
	_, err = NewDrainNode(context.Background(), te.guard(), NodeOptions{})
	mustErrorIs(t, err, dr.ErrPreflight)
}

func TestNodesNeedAnotherSchedulableWorker(t *testing.T) {
	te := newTestEnv(t, controlPlane("cp"), worker("w1"), allowedNS("chaos-test"))
	_, err := NewCordonNode(context.Background(), te.guard(), NodeOptions{Node: "w1"})
	mustErrorIs(t, err, ErrBlastRadius)

	// Another worker that is already cordoned or not ready does not count.
	cordoned := worker("w2")
	cordoned.Spec.Unschedulable = true
	down := worker("w3")
	down.Status.Conditions[0].Status = corev1.ConditionFalse
	te = newTestEnv(t, controlPlane("cp"), worker("w1"), cordoned, down, allowedNS("chaos-test"))
	_, err = NewCordonNode(context.Background(), te.guard(), NodeOptions{Node: "w1"})
	mustErrorIs(t, err, ErrBlastRadius)
}

func TestDrainRefusesToEvictPodsOfNamespacesThatAreNotMarked(t *testing.T) {
	shop := ns("shop", nil)
	te := newTestEnv(t, labNodes(shop, pod("shop", "web-1", "web", "w1", true))...)

	_, err := NewDrainNode(context.Background(), te.guard(), NodeOptions{Node: "w1"})
	mustErrorIs(t, err, ErrBlastRadius)
	if !strings.Contains(err.Error(), "shop") {
		t.Errorf("the namespace should be named: %v", err)
	}
	// Cordon evicts nothing, so it is allowed.
	if _, err := NewCordonNode(context.Background(), te.guard(), NodeOptions{Node: "w1"}); err != nil {
		t.Errorf("cordon must be allowed: %v", err)
	}
	// An explicit option accepts the node-wide effect, and Describe warns about it.
	a, err := NewDrainNode(context.Background(), te.guard(), NodeOptions{Node: "w1", AllowOtherNamespaces: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.Describe(), "WARNING") || !strings.Contains(a.Describe(), "shop") {
		t.Errorf("Describe should warn: %s", a.Describe())
	}
}

func TestDrainIgnoresDaemonSetAndMirrorPods(t *testing.T) {
	ds := pod("kube-system", "proxy-w1", "proxy", "w1", true)
	ds.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "proxy"}}
	mirror := pod("kube-system", "etcd-w1", "etcd", "w1", true)
	mirror.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}
	done := pod("shop", "job-1", "job", "w1", true)
	done.Status.Phase = corev1.PodSucceeded

	te := newTestEnv(t, labNodes(ds, mirror, done)...)
	a, err := NewDrainNode(context.Background(), te.guard(), NodeOptions{Node: "w1"})
	if err != nil {
		t.Fatalf("system, DaemonSet and finished pods must not block the drain: %v", err)
	}
	if !strings.Contains(a.Describe(), "chaos-test: 1") || strings.Contains(a.Describe(), "kube-system") {
		t.Errorf("Describe() = %s", a.Describe())
	}
}

func TestDrainAndRecoveryRoundTrip(t *testing.T) {
	te := newTestEnv(t, labNodes()...)
	ctx := context.Background()
	a, err := NewDrainNode(ctx, te.guard(), NodeOptions{Node: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode() != ModeDrain || a.Risk() != dr.RiskHigh || a.ID() != "chaos/node/w1:drain" {
		t.Errorf("mode %v risk %v id %s", a.Mode(), a.Risk(), a.ID())
	}
	if got := a.Equivalent(); !strings.HasPrefix(got, "kubectl drain w1 --ignore-daemonsets --timeout=5m") {
		t.Errorf("Equivalent() = %q", got)
	}
	if done, _ := a.Done(ctx); done {
		t.Fatal("Done before applying")
	}

	if err := a.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if !unschedulable(t, te, "w1") || unschedulable(t, te, "w2") {
		t.Error("only w1 must be cordoned")
	}
	if len(te.evicted) != 1 || te.evicted[0] != "chaos-test/api-1" {
		t.Errorf("evicted = %v", te.evicted)
	}
	if done, err := a.Done(ctx); err != nil || !done {
		t.Errorf("Done after apply = %v, %v", done, err)
	}

	// The record lives under its own key, so a DR "up" never uncordons what chaos cordoned.
	keys := strings.Join(te.state.keys(), ",")
	if !strings.Contains(keys, testContext+".chaos/nodes") || strings.Contains(keys, testContext+"/nodes") {
		t.Errorf("state keys = %s", keys)
	}

	recovery := a.Recovery()
	if recovery.Risk() != dr.RiskLow || recovery.Equivalent() != "kubectl uncordon w1" {
		t.Errorf("recovery: %v %q", recovery.Risk(), recovery.Equivalent())
	}
	if done, _ := recovery.Done(ctx); done {
		t.Error("recovery is pending while the node is cordoned")
	}
	if err := recovery.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if unschedulable(t, te, "w1") {
		t.Error("the node must be schedulable again")
	}
	if done, err := recovery.Done(ctx); err != nil || !done {
		t.Errorf("recovery Done = %v, %v", done, err)
	}
	if len(te.state.keys()) != 0 {
		t.Errorf("the state record should be cleaned: %v", te.state.keys())
	}
}

func TestCordonOnlyDoesNotEvict(t *testing.T) {
	te := newTestEnv(t, labNodes()...)
	ctx := context.Background()
	a, err := NewCordonNode(ctx, te.guard(), NodeOptions{Node: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Equivalent() != "kubectl cordon w1" || a.Title() != "Cordon node w1" {
		t.Errorf("%q %q", a.Equivalent(), a.Title())
	}
	if err := a.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if !unschedulable(t, te, "w1") || len(te.evicted) != 0 {
		t.Errorf("cordon must not evict: evicted %v", te.evicted)
	}
	if err := a.Recovery().Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if unschedulable(t, te, "w1") {
		t.Error("recovery must uncordon")
	}
}

func TestRecoveryLeavesANodeCordonedByAPersonAlone(t *testing.T) {
	already := worker("w1")
	already.Spec.Unschedulable = true
	te := newTestEnv(t, controlPlane("cp"), already, worker("w2"), allowedNS("chaos-test"))
	ctx := context.Background()
	a, err := NewCordonNode(ctx, te.guard(), NodeOptions{Node: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Recovery().Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if !unschedulable(t, te, "w1") {
		t.Error("a node somebody else cordoned must stay cordoned")
	}
}

func TestNodeApplyChecksTheGuardAgain(t *testing.T) {
	te := newTestEnv(t, labNodes()...)
	ctx := context.Background()
	a, err := NewDrainNode(ctx, te.guard(), NodeOptions{Node: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	a.guard.ReadOnly = true
	mustErrorIs(t, a.Apply(ctx), dr.ErrReadOnly)
	a.guard.ReadOnly = false

	// Another worker went away after planning: draining w1 would now leave nowhere to schedule.
	if err := te.client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("nodes"), "", "w2"); err != nil {
		t.Fatal(err)
	}
	mustErrorIs(t, a.Apply(ctx), ErrBlastRadius)
	if unschedulable(t, te, "w1") {
		t.Error("nothing may be cordoned when the guard fails")
	}
}

func TestPause(t *testing.T) {
	if _, err := NewPause(nil, 100*time.Millisecond); err == nil {
		t.Error("a pause under one second must be refused")
	}
	if _, err := NewPause(nil, MaxPause+time.Second); err == nil {
		t.Error("a pause over the maximum must be refused")
	}
	te := newTestEnv(t)
	p, err := NewPause(te.env, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title() != "Wait 1m30s" || p.Risk() != dr.RiskLow || p.Equivalent() != "sleep 90" {
		t.Errorf("%q %v %q", p.Title(), p.Risk(), p.Equivalent())
	}
	start := te.clock.Now()
	if done, _ := p.Done(context.Background()); done {
		t.Error("not done before it ran")
	}
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := te.clock.Now().Sub(start); got != 90*time.Second {
		t.Errorf("slept %s", got)
	}
	if done, _ := p.Done(context.Background()); !done {
		t.Error("done after it ran")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p2, _ := NewPause(te.env, time.Second)
	if err := p2.Apply(ctx); err == nil {
		t.Error("a cancelled pause must report it")
	}
}

func TestRecoveryActionDescribesItself(t *testing.T) {
	te := newTestEnv(t, labNodes()...)
	a, err := NewCordonNode(context.Background(), te.guard(), NodeOptions{Node: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	r := a.Recovery()
	if r.ID() != "chaos/node/w1:uncordon" || r.Title() != "Uncordon node w1" ||
		!strings.Contains(r.Describe(), "only if the chaos injection is what cordoned it") {
		t.Errorf("%q %q %q", r.ID(), r.Title(), r.Describe())
	}
	p, _ := NewPause(te.env, 5*time.Second)
	if p.ID() != "chaos/pause:5s" || !strings.Contains(p.Describe(), "5s") {
		t.Errorf("%q %q", p.ID(), p.Describe())
	}
}
