package k8sops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

const nodesStateKey = testContext + "/nodes"

func withProvider() *dr.Plan {
	p := testPlan(dr.App{Name: "shop", Namespace: "shop"})
	p.Provider = &dr.Provider{Kind: "kind"}
	return p
}

func unschedulable(t *testing.T, te *testEnv, name string) bool {
	t.Helper()
	n, err := te.client.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return n.Spec.Unschedulable
}

func labLike(extra ...runtime.Object) []runtime.Object {
	return append([]runtime.Object{controlPlane("cp"), node("w1", nil), node("w2", nil)}, extra...)
}

func TestNodesOnlyApplyWithAProviderOrAnExplicitScope(t *testing.T) {
	te := newEnv(t, labLike()...)
	p := NewNodesPlanner(Options{})
	plain := testPlan(dr.App{Name: "shop", Namespace: "shop"})

	if steps := stepsFor(t, te, p, plain, dr.DirDown, dr.Scope{}); len(steps) != 0 {
		t.Errorf("a plan without a provider must not drain nodes by surprise: %v", ids(steps))
	}
	if steps := stepsFor(t, te, p, plain, dr.DirDown, dr.Scope{Phases: []dr.Phase{dr.PhaseNodes}}); len(steps) == 0 {
		t.Error("asking for the nodes phase explicitly must work without a provider")
	}
	if steps := stepsFor(t, te, p, withProvider(), dr.DirDown, dr.Scope{}); len(steps) == 0 {
		t.Error("a plan with a provider drains its nodes")
	}
}

func TestNodesDownCordonsEveryWorkerFirstThenDrainsAndSparesTheControlPlane(t *testing.T) {
	te := newEnv(t, labLike(nodePod("shop", "api-1", "w1"), nodePod("shop", "api-2", "w2"))...)
	p := NewNodesPlanner(Options{})
	steps := stepsFor(t, te, p, withProvider(), dr.DirDown, dr.Scope{})

	want := "nodes/w1:cordon,nodes/w2:cordon,nodes/w1:drain,nodes/w2:drain"
	if got := strings.Join(ids(steps), ","); got != want {
		t.Fatalf("order = %s; want %s", got, want)
	}
	for _, s := range steps {
		if s.Phase != dr.PhaseNodes || s.Action.Risk() != dr.RiskHigh {
			t.Errorf("%s: phase %s risk %s", s.Action.ID(), s.Phase, s.Action.Risk())
		}
	}

	if err := runSteps(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	if !unschedulable(t, te, "w1") || !unschedulable(t, te, "w2") {
		t.Error("both workers must be cordoned")
	}
	if unschedulable(t, te, "cp") {
		t.Error("the control plane must never be cordoned by default")
	}
	pods, _ := te.client.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("pods left after the drain: %d", len(pods.Items))
	}
}

func TestNodesRecordOnlyWhatThisRunCordonedAndUpUncordonsExactlyThose(t *testing.T) {
	already := node("w2", nil)
	already.Spec.Unschedulable = true
	te := newEnv(t, controlPlane("cp"), node("w1", nil), already)
	p := NewNodesPlanner(Options{})
	ctx := context.Background()

	down := stepsFor(t, te, p, withProvider(), dr.DirDown, dr.Scope{})
	if !strings.Contains(down[1].Action.Describe(), "already cordoned") {
		t.Errorf("describe = %q", down[1].Action.Describe())
	}
	if err := runSteps(ctx, down); err != nil {
		t.Fatal(err)
	}
	var recorded []string
	if found, _ := te.state.Load(ctx, nodesStateKey, &recorded); !found || strings.Join(recorded, ",") != "w1" {
		t.Fatalf("state = %v; only w1 was cordoned by this run", recorded)
	}

	up := stepsFor(t, te, p, withProvider(), dr.DirUp, dr.Scope{})
	if got := strings.Join(ids(up), ","); got != "nodes/w1:uncordon" {
		t.Fatalf("up = %s", got)
	}
	gate, ok := up[0].Gate.(*NodesReadyGate)
	if !ok || strings.Join(gate.Nodes, ",") != "w1" {
		t.Errorf("the last uncordon must carry a nodes-ready gate: %T", up[0].Gate)
	}
	if err := runSteps(ctx, up); err != nil {
		t.Fatal(err)
	}
	if unschedulable(t, te, "w1") {
		t.Error("w1 must be schedulable again")
	}
	if !unschedulable(t, te, "w2") {
		t.Error("w2 was cordoned by someone else and must stay cordoned")
	}
	if te.state.has(nodesStateKey) {
		t.Error("the state must be empty after up")
	}
}

func TestNodesDownResumesAfterACrashBetweenCordonAndDrain(t *testing.T) {
	te := newEnv(t, controlPlane("cp"), node("w1", nil), nodePod("shop", "api", "w1"))
	p := NewNodesPlanner(Options{})
	ctx := context.Background()

	first := stepsFor(t, te, p, withProvider(), dr.DirDown, dr.Scope{})
	if err := first[0].Action.Apply(ctx); err != nil { // cordon, then "crash"
		t.Fatal(err)
	}

	// The resumed run plans again: the node is now cordoned, but it was ours, so it must still be recorded.
	second := stepsFor(t, te, p, withProvider(), dr.DirDown, dr.Scope{})
	if err := runSteps(ctx, second); err != nil {
		t.Fatal(err)
	}
	up := stepsFor(t, te, p, withProvider(), dr.DirUp, dr.Scope{})
	if len(up) != 1 {
		t.Fatalf("the node cordoned before the crash must still be uncordoned on up: %v", ids(up))
	}
}

func TestNodesControlPlaneOnlyWithOptInAndNodeFilter(t *testing.T) {
	te := newEnv(t, labLike()...)
	got := strings.Join(ids(stepsFor(t, te, NewNodesPlanner(Options{IncludeControlPlane: true}), withProvider(), dr.DirDown, dr.Scope{})), ",")
	if !strings.Contains(got, "nodes/cp:cordon") || !strings.Contains(got, "nodes/cp:drain") {
		t.Errorf("IncludeControlPlane must include it: %s", got)
	}

	only := strings.Join(ids(stepsFor(t, te, NewNodesPlanner(Options{Nodes: []string{"w2"}}), withProvider(), dr.DirDown, dr.Scope{})), ",")
	if only != "nodes/w2:cordon,nodes/w2:drain" {
		t.Errorf("Nodes filter: %s", only)
	}

	if _, err := NewNodesPlanner(Options{Nodes: []string{"cp"}}).Steps(context.Background(), te.env, withProvider(), dr.DirDown, dr.Scope{}); err == nil ||
		!strings.Contains(err.Error(), "control-plane") {
		t.Errorf("naming a control-plane node without opting in must fail: %v", err)
	}
	if _, err := NewNodesPlanner(Options{Nodes: []string{"ghost"}}).Steps(context.Background(), te.env, withProvider(), dr.DirDown, dr.Scope{}); err == nil {
		t.Error("an unknown node must fail")
	}
}

func TestNodesControlPlaneIsRecognizedByLabelOrTaint(t *testing.T) {
	tainted := node("old-master", nil)
	tainted.Spec.Taints = []corev1.Taint{{Key: "node-role.kubernetes.io/master", Effect: corev1.TaintEffectNoSchedule}}
	if !isControlPlane(tainted) || !isControlPlane(controlPlane("cp")) || isControlPlane(node("w", nil)) {
		t.Error("control-plane detection is wrong")
	}
}

func TestNodesUpSkipsNodesThatNoLongerExist(t *testing.T) {
	te := newEnv(t, controlPlane("cp"), node("w1", nil), node("w2", nil))
	p := NewNodesPlanner(Options{})
	ctx := context.Background()
	if err := runSteps(ctx, stepsFor(t, te, p, withProvider(), dr.DirDown, dr.Scope{})); err != nil {
		t.Fatal(err)
	}
	if err := te.client.CoreV1().Nodes().Delete(ctx, "w2", metav1.DeleteOptions{}); err != nil { // an autoscaler took it away
		t.Fatal(err)
	}

	up := stepsFor(t, te, p, withProvider(), dr.DirUp, dr.Scope{})
	if err := runSteps(ctx, up); err != nil {
		t.Fatalf("a node that vanished must not fail the run: %v", err)
	}
	if te.state.has(nodesStateKey) {
		t.Error("the vanished node must be dropped from the state")
	}
	if !containsLog(te, "no longer exists") {
		t.Errorf("the skip must be logged: %v", te.logs)
	}
	if unschedulable(t, te, "w1") {
		t.Error("w1 must be uncordoned")
	}
}

func TestNodesUpIsPlannedFromTheStateAloneAndNoStateMeansNoSteps(t *testing.T) {
	te := newEnv(t, controlPlane("cp"), node("w1", nil))
	p := NewNodesPlanner(Options{})
	if err := runSteps(context.Background(), stepsFor(t, te, p, withProvider(), dr.DirDown, dr.Scope{})); err != nil {
		t.Fatal(err)
	}
	te.env.Client = nil // the cluster is off
	up, err := p.Steps(context.Background(), te.env, withProvider(), dr.DirUp, dr.Scope{})
	if err != nil || len(up) != 1 {
		t.Fatalf("%d steps, %v", len(up), err)
	}
	if err := up[0].Gate.Wait(context.Background()); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("the gate without a client = %v; want ErrUnreachable", err)
	}

	empty := newEnv(t, labLike()...)
	if steps := stepsFor(t, empty, p, withProvider(), dr.DirUp, dr.Scope{}); len(steps) != 0 {
		t.Errorf("nothing recorded, nothing to uncordon: %v", ids(steps))
	}
}

func TestNodesDownUnreachable(t *testing.T) {
	te := newEnv(t, labLike()...)
	te.client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp: i/o timeout")
	})
	if _, err := NewNodesPlanner(Options{}).Steps(context.Background(), te.env, withProvider(), dr.DirDown, dr.Scope{}); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("err = %v; want ErrUnreachable", err)
	}
	te.env.Client = nil
	if _, err := NewNodesPlanner(Options{}).Steps(context.Background(), te.env, withProvider(), dr.DirDown, dr.Scope{}); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("nil client err = %v; want ErrUnreachable", err)
	}
}

func TestNodesDescriptionsAndEquivalents(t *testing.T) {
	te := newEnv(t, labLike()...)
	steps := stepsFor(t, te, NewNodesPlanner(Options{AllowEmptyDir: true, AllowUnmanagedPods: true, DrainTimeout: 3 * time.Minute}), withProvider(), dr.DirDown, dr.Scope{})
	byID := map[string]dr.Action{}
	for _, s := range steps {
		byID[s.Action.ID()] = s.Action
	}
	if got := byID["nodes/w1:cordon"].Equivalent(); got != "kubectl cordon w1" {
		t.Errorf("cordon = %q", got)
	}
	drain := byID["nodes/w1:drain"]
	if got := drain.Equivalent(); got != "kubectl drain w1 --ignore-daemonsets --delete-emptydir-data --force --timeout=3m0s" {
		t.Errorf("drain = %q", got)
	}
	if !strings.Contains(drain.Describe(), "PodDisruptionBudgets are honored") || drain.Title() != "Drain node w1" {
		t.Errorf("drain describe/title = %q / %q", drain.Describe(), drain.Title())
	}
	plain := stepsFor(t, te, NewNodesPlanner(Options{}), withProvider(), dr.DirDown, dr.Scope{})
	for _, s := range plain {
		if s.Action.ID() == "nodes/w1:drain" && s.Action.Equivalent() != "kubectl drain w1 --ignore-daemonsets --timeout=10m0s" {
			t.Errorf("default drain = %q", s.Action.Equivalent())
		}
	}
}

func TestPlannersListsTheThreePhases(t *testing.T) {
	phases := []dr.Phase{}
	for _, p := range Planners(Options{}) {
		phases = append(phases, p.Phase())
	}
	if len(phases) != 3 || phases[0] != dr.PhasePrepare || phases[1] != dr.PhaseApps || phases[2] != dr.PhaseNodes {
		t.Errorf("phases = %v", phases)
	}
}

func TestCordonApplyNeverClaimsANodeSomeoneElseCordoned(t *testing.T) {
	already := node("w1", nil)
	already.Spec.Unschedulable = true
	te := newEnv(t, controlPlane("cp"), already)
	ctx := context.Background()

	// Even when Apply is called directly (a race, or a caller that skips Done), it must not record a node it did not cordon.
	cordon := &cordonNode{nodeAction: nodeAction{client: te.client, name: "w1", state: newLedger(te.env, newMemStore(), "nodes"), c: clockOf(te.env)}}
	cordon.state = newLedger(te.env, nil, "nodes")
	if err := cordon.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if te.state.has(nodesStateKey) {
		t.Error("a node cordoned by someone else must not end up in the DR state")
	}

	// A node this run cordoned and recorded, then crashed: Apply on the resumed run keeps the record.
	fresh := newEnv(t, controlPlane("cp"), node("w2", nil))
	c2 := &cordonNode{nodeAction: nodeAction{client: fresh.client, name: "w2", state: newLedger(fresh.env, nil, "nodes"), c: clockOf(fresh.env)}}
	if err := c2.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c2.Apply(ctx); err != nil { // second time: already cordoned and already recorded
		t.Fatal(err)
	}
	var recorded []string
	if found, _ := fresh.state.Load(ctx, nodesStateKey, &recorded); !found || strings.Join(recorded, ",") != "w2" {
		t.Errorf("state = %v; the record must survive a second Apply", recorded)
	}

	// A node that vanished is not an error.
	gone := &cordonNode{nodeAction: nodeAction{client: fresh.client, name: "missing", state: newLedger(fresh.env, nil, "nodes"), c: clockOf(fresh.env)}}
	if err := gone.Apply(ctx); err != nil {
		t.Errorf("a vanished node: %v", err)
	}
}
