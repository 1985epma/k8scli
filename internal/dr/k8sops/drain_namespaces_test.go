package k8sops

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/1985epma/k8scli/internal/dr"
)

// A shutdown that powers the machines off must not insist on moving the pods of kube-system or of an
// operator: they stop with the machine, and many of them use emptyDir or have no controller, so a full drain
// would refuse them for good and no real cluster could ever be shut down.

func withEmptyDir(pod *corev1.Pod) *corev1.Pod {
	pod.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	return pod
}

func unmanaged(pod *corev1.Pod) *corev1.Pod {
	pod.OwnerReferences = nil
	return pod
}

func fullShutdownPlan() *dr.Plan {
	plan := testPlan(dr.App{Name: "shop", Namespace: "shop"})
	plan.Databases = []dr.Database{{Name: "db", Namespace: "data", Kind: "statefulset", Ref: "db"}}
	plan.Provider = &dr.Provider{Kind: "kind"}
	return plan
}

func shutdownOptions() Options { return Options{DrainPlanNamespacesBeforePower: true} }

func podNames(t *testing.T, te *testEnv) []string {
	t.Helper()
	list, err := te.client.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range list.Items {
		out = append(out, p.Namespace+"/"+p.Name)
	}
	return out
}

func TestShutdownIgnoresAnEmptyDirPodOfAnotherNamespace(t *testing.T) {
	te := newEnv(t, labLike(
		nodePod("shop", "api-1", "w1"),
		nodePod("data", "db-0", "w2"),
		withEmptyDir(nodePod("cnpg-system", "cnpg-controller-manager", "w1")),
		unmanaged(nodePod("kube-system", "static-thing", "w2")),
	)...)
	steps := stepsFor(t, te, NewNodesPlanner(shutdownOptions()), fullShutdownPlan(), dr.DirDown, dr.Scope{})

	if err := runSteps(context.Background(), steps); err != nil {
		t.Fatalf("the operator's emptyDir pod must not block a shutdown: %v", err)
	}
	left := strings.Join(podNames(t, te), ",")
	if strings.Contains(left, "shop/api-1") || strings.Contains(left, "data/db-0") {
		t.Errorf("the plan's own pods must be evicted, left: %s", left)
	}
	if !strings.Contains(left, "cnpg-system/cnpg-controller-manager") || !strings.Contains(left, "kube-system/static-thing") {
		t.Errorf("pods of other namespaces must be left alone, left: %s", left)
	}
	if !unschedulable(t, te, "w1") || !unschedulable(t, te, "w2") {
		t.Error("both workers are still cordoned")
	}
}

func TestShutdownStillRefusesAnEmptyDirPodInsideThePlansNamespaces(t *testing.T) {
	te := newEnv(t, labLike(
		nodePod("shop", "api-1", "w1"),
		withEmptyDir(nodePod("shop", "cache-0", "w1")),
		withEmptyDir(nodePod("kube-system", "dns", "w1")), // foreign: ignored
	)...)
	steps := stepsFor(t, te, NewNodesPlanner(shutdownOptions()), fullShutdownPlan(), dr.DirDown, dr.Scope{})

	err := runSteps(context.Background(), steps)
	if err == nil || !strings.Contains(err.Error(), "shop/cache-0 uses an emptyDir") || strings.Contains(err.Error(), "kube-system") {
		t.Fatalf("err = %v; want a refusal that names only the plan's pod", err)
	}
	if len(podNames(t, te)) != 3 {
		t.Errorf("nothing may be evicted when a pod inside the plan is refused: %v", podNames(t, te))
	}
}

func TestShutdownStillRefusesUnmanagedPodsInsideThePlansNamespaces(t *testing.T) {
	te := newEnv(t, labLike(unmanaged(nodePod("data", "stray", "w1")))...)
	steps := stepsFor(t, te, NewNodesPlanner(shutdownOptions()), fullShutdownPlan(), dr.DirDown, dr.Scope{})
	if err := runSteps(context.Background(), steps); err == nil || !strings.Contains(err.Error(), "data/stray has no controller") {
		t.Fatalf("err = %v", err)
	}
}

func TestShutdownKeepsThePodDisruptionBudgetsAndTheControlPlane(t *testing.T) {
	te := newEnv(t, labLike(nodePod("shop", "api-1", "w1"), nodePod("shop", "cp-pod", "cp"))...)
	steps := stepsFor(t, te, NewNodesPlanner(shutdownOptions()), fullShutdownPlan(), dr.DirDown, dr.Scope{})
	if got := strings.Join(ids(steps), ","); got != "nodes/w1:cordon,nodes/w2:cordon,nodes/w1:drain,nodes/w2:drain" {
		t.Fatalf("steps = %s; the control plane is still never touched", got)
	}
	if err := runSteps(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	if left := strings.Join(podNames(t, te), ","); !strings.Contains(left, "shop/cp-pod") {
		t.Errorf("a pod on the control plane stays: %s", left)
	}
}

// Without the power phase nothing is about to stop the other pods, so everything is drained as before.
func TestScopeWithoutPowerDrainsEveryNamespaceAsBefore(t *testing.T) {
	for name, scope := range map[string]dr.Scope{
		"nodes only":   {Phases: []dr.Phase{dr.PhaseNodes}},
		"apps and dbs": {Phases: []dr.Phase{dr.PhaseApps, dr.PhaseDatabases, dr.PhaseNodes}},
	} {
		t.Run(name, func(t *testing.T) {
			te := newEnv(t, labLike(nodePod("shop", "api-1", "w1"), withEmptyDir(nodePod("cnpg-system", "mgr", "w1")))...)
			steps := stepsFor(t, te, NewNodesPlanner(shutdownOptions()), fullShutdownPlan(), dr.DirDown, scope)
			err := runSteps(context.Background(), steps)
			if err == nil || !strings.Contains(err.Error(), "cnpg-system/mgr uses an emptyDir") {
				t.Fatalf("err = %v; the full drain semantics must be unchanged", err)
			}
		})
	}
}

func TestScopeThatListsThePowerPhaseIsLimited(t *testing.T) {
	te := newEnv(t, labLike(withEmptyDir(nodePod("cnpg-system", "mgr", "w1")))...)
	scope := dr.Scope{Phases: []dr.Phase{dr.PhaseNodes, dr.PhasePower}}
	steps := stepsFor(t, te, NewNodesPlanner(shutdownOptions()), fullShutdownPlan(), dr.DirDown, scope)
	if err := runSteps(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
}

func TestAPlanWithoutAProviderNeverLimitsTheDrain(t *testing.T) {
	te := newEnv(t, labLike(withEmptyDir(nodePod("cnpg-system", "mgr", "w1")))...)
	plan := testPlan(dr.App{Name: "shop", Namespace: "shop"}) // no provider: nothing will power anything off
	steps := stepsFor(t, te, NewNodesPlanner(shutdownOptions()), plan, dr.DirDown, dr.Scope{Phases: []dr.Phase{dr.PhaseNodes}})
	if err := runSteps(context.Background(), steps); err == nil {
		t.Fatal("without a power phase the foreign emptyDir pod must still be refused")
	}
}

func TestExplicitNodeDrainKeepsTheFullKubectlSemantics(t *testing.T) {
	// `dr nodes drain` passes no DrainPlanNamespacesBeforePower: a node named explicitly is drained fully.
	te := newEnv(t, labLike(withEmptyDir(nodePod("kube-system", "dns", "w1")))...)
	steps := stepsFor(t, te, NewNodesPlanner(Options{Nodes: []string{"w1"}}), fullShutdownPlan(), dr.DirDown, dr.Scope{})
	if err := runSteps(context.Background(), steps); err == nil || !strings.Contains(err.Error(), "kube-system/dns uses an emptyDir") {
		t.Fatalf("err = %v", err)
	}
	te = newEnv(t, labLike(withEmptyDir(nodePod("kube-system", "dns", "w1")))...)
	steps = stepsFor(t, te, NewNodesPlanner(Options{Nodes: []string{"w1"}, AllowEmptyDir: true}), fullShutdownPlan(), dr.DirDown, dr.Scope{})
	if err := runSteps(context.Background(), steps); err != nil {
		t.Fatalf("AllowEmptyDir still works: %v", err)
	}
}

func TestExplicitDrainNamespacesWinAndAnEmptyListDrainsNothing(t *testing.T) {
	te := newEnv(t, labLike(nodePod("shop", "api-1", "w1"), nodePod("other", "o-1", "w1"))...)
	steps := stepsFor(t, te, NewNodesPlanner(Options{DrainNamespaces: []string{"other"}}), fullShutdownPlan(), dr.DirDown, dr.Scope{})
	if err := runSteps(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(podNames(t, te), ","); got != "shop/api-1" {
		t.Errorf("left = %s; only namespace `other` is drained", got)
	}

	te = newEnv(t, labLike(nodePod("shop", "api-1", "w1"))...)
	noNamespaces := testPlan()
	noNamespaces.Provider = &dr.Provider{Kind: "kind"}
	steps = stepsFor(t, te, NewNodesPlanner(shutdownOptions()), noNamespaces, dr.DirDown, dr.Scope{})
	if err := runSteps(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	if len(podNames(t, te)) != 1 {
		t.Error("a plan that manages no namespace drains nothing (empty means none, not all)")
	}
}

func TestDescribeAndEquivalentSayWhichNamespacesAreDrained(t *testing.T) {
	te := newEnv(t, labLike()...)
	limited := stepsFor(t, te, NewNodesPlanner(shutdownOptions()), fullShutdownPlan(), dr.DirDown, dr.Scope{})
	var drain dr.Action
	for _, s := range limited {
		if strings.HasSuffix(s.Action.ID(), ":drain") {
			drain = s.Action
			break
		}
	}
	text := drain.Describe()
	for _, want := range []string{"data, shop", "other namespaces", "stop with the node"} {
		if !strings.Contains(text, want) {
			t.Errorf("Describe() = %q; missing %q", text, want)
		}
	}
	if eq := drain.Equivalent(); !strings.Contains(eq, "kubectl drain w1 --ignore-daemonsets") || !strings.Contains(eq, "only the pods of: data, shop") {
		t.Errorf("Equivalent() = %q", eq)
	}

	full := stepsFor(t, te, NewNodesPlanner(Options{}), fullShutdownPlan(), dr.DirDown, dr.Scope{Phases: []dr.Phase{dr.PhaseNodes}})
	for _, s := range full {
		if strings.HasSuffix(s.Action.ID(), ":drain") && (strings.Contains(s.Action.Describe(), "namespaces the plan manages") || strings.Contains(s.Action.Equivalent(), "only the pods of")) {
			t.Errorf("a full drain must keep its old wording: %q / %q", s.Action.Describe(), s.Action.Equivalent())
		}
	}
}
