package cmd

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/1985epma/k8scli/internal/dr/chaos"
	"github.com/1985epma/k8scli/internal/dr/wire/wiretest"
)

const labPlanYAML = `version: 1
name: lab
context: dev
safety:
  labOnly: true
apps:
  - name: api
    namespace: chaos-test
    workloads: [api]
    tier: 1
`

func chaosPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "chaos-test", UID: k8stypes.UID("uid-" + name), Labels: map[string]string{"app": "api"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func chaosCluster(allowed bool, extra ...runtime.Object) *wiretest.Cluster {
	namespace := wiretest.Namespace("chaos-test")
	if allowed {
		namespace.Labels = map[string]string{chaos.LabelAllowed: "true"}
	}
	objects := []runtime.Object{namespace, wiretest.Deployment("chaos-test", "api", 3), chaosPod("api-1"), chaosPod("api-2"), chaosPod("api-3")}
	return wiretest.NewCluster(append(objects, extra...)...)
}

func killArgs(plan string, more ...string) []string {
	return append([]string{"dr", "chaos", "kill-pods", "-n", "chaos-test", "--workload", "api", "--plan", plan, "--window", "5s", "--poll", "1s"}, more...)
}

func podsLeft(t *testing.T, c *wiretest.Cluster) int {
	t.Helper()
	list, err := c.Client.CoreV1().Pods("chaos-test").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(list.Items)
}

func TestChaosIsListedUnderDR(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": chaosCluster(true)})
	if err := h.run("dr", "--help"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"chaos", "extcheck"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("dr --help does not list %q:\n%s", want, h.out.String())
		}
	}
}

func TestChaosKillPodsIsADryRunByDefault(t *testing.T) {
	c := chaosCluster(true)
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})
	plan := writeFile(t, "lab.yaml", labPlanYAML)

	if err := h.run(killArgs(plan)...); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"dry run", "Kill 1 pod(s) of chaos-test/api", "Watching:   app api", "Healthy before: yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The victim is picked at random, so assert the property, not the pod: it is one of the three
	// candidates and the equivalent command names exactly that one.
	if !regexp.MustCompile(`equivalent: kubectl delete pod api-[123] -n chaos-test`).MatchString(out) {
		t.Errorf("the equivalent command must name one of the candidate pods:\n%s", out)
	}
	if !strings.Contains(h.errOut.String(), "nothing was injected") {
		t.Errorf("stderr = %q", h.errOut.String())
	}
	if podsLeft(t, c) != 3 {
		t.Error("a dry run must not evict anything")
	}
}

func TestChaosKillPodsExecuteInjectsAndReports(t *testing.T) {
	c := chaosCluster(true)
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})
	plan := writeFile(t, "lab.yaml", labPlanYAML)

	if err := h.run(killArgs(plan, "--execute")...); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if podsLeft(t, c) != 2 {
		t.Errorf("one pod must be evicted, %d remain", podsLeft(t, c))
	}
	out := h.out.String()
	if !strings.Contains(out, "executed") || !strings.Contains(out, "absorbed the failure") {
		t.Errorf("output:\n%s", out)
	}
	if !strings.Contains(h.errOut.String(), "injecting") {
		t.Errorf("progress events belong on stderr: %q", h.errOut.String())
	}
}

func TestChaosJSONOutput(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": chaosCluster(true)})
	plan := writeFile(t, "lab.yaml", labPlanYAML)
	if err := h.run(killArgs(plan, "-o", "json")...); err != nil {
		t.Fatal(err)
	}
	var result chaos.Result
	if err := json.Unmarshal(h.out.Bytes(), &result); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, h.out.String())
	}
	if result.Mode != "dry-run" || result.Injection == nil || result.Injection.ID != "chaos/chaos-test/api:kill-pods" {
		t.Errorf("result = %+v", result)
	}
}

func TestChaosRequiresItsFlags(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": chaosCluster(true)})
	plan := writeFile(t, "lab.yaml", labPlanYAML)
	for name, args := range map[string][]string{
		"no namespace": {"dr", "chaos", "kill-pods", "--workload", "api", "--plan", plan},
		"no workload":  {"dr", "chaos", "kill-pods", "-n", "chaos-test", "--plan", plan},
		"no node":      {"dr", "chaos", "drain-node", "--plan", plan},
		"no plan file": {"dr", "chaos", "cordon-node", "--node", "w1", "--plan", "/nonexistent/plan.yaml"},
	} {
		if err := h.run(args...); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestChaosRefusesWhatTheGuardRefuses(t *testing.T) {
	plan := writeFile(t, "lab.yaml", labPlanYAML)
	notLab := writeFile(t, "notlab.yaml", strings.Replace(labPlanYAML, "labOnly: true", "labOnly: false", 1))

	c := chaosCluster(false)
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})
	err := h.run(killArgs(plan, "--execute")...)
	if err == nil || !strings.Contains(err.Error(), chaos.LabelAllowed) {
		t.Errorf("an unlabelled namespace: %v", err)
	}
	if podsLeft(t, c) != 3 {
		t.Error("nothing may be evicted after a refusal")
	}

	h = newDRHarness(t, map[string]*wiretest.Cluster{"dev": chaosCluster(true)})
	if err := h.run(killArgs(notLab)...); err == nil || !strings.Contains(err.Error(), "labOnly") {
		t.Errorf("a plan that is not a lab: %v", err)
	}
}

func TestChaosProductionNeedsTheTypedPhrase(t *testing.T) {
	prodPlan := writeFile(t, "prod.yaml", strings.Replace(labPlanYAML, "context: dev", "context: eks-prod", 1))
	c := chaosCluster(true)
	h := newDRHarness(t, map[string]*wiretest.Cluster{"eks-prod": c})

	// A dry run needs no phrase.
	if err := h.run(killArgs(prodPlan)...); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	// A real run without one, and without input to type it, fails and says what to pass.
	err := h.run(killArgs(prodPlan, "--execute")...)
	if err == nil || !strings.Contains(err.Error(), "--confirm 'chaos eks-prod'") {
		t.Fatalf("err = %v", err)
	}
	if podsLeft(t, c) != 3 {
		t.Fatal("nothing may be evicted without the phrase")
	}
	// The wrong phrase.
	if err := h.run(killArgs(prodPlan, "--execute", "--confirm", "eks-prod")...); err == nil {
		t.Error("the context name alone is not the chaos phrase")
	}
	// The right one.
	if err := h.run(killArgs(prodPlan, "--execute", "--confirm", "chaos eks-prod")...); err != nil {
		t.Fatalf("with the phrase: %v", err)
	}
	if podsLeft(t, c) != 2 {
		t.Error("the right phrase must let it run")
	}
}

func TestChaosCordonAndDrainDryRuns(t *testing.T) {
	objects := []runtime.Object{wiretest.ControlPlane("cp"), wiretest.Node("w1"), wiretest.Node("w2")}
	c := chaosCluster(true, objects...)
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})
	plan := writeFile(t, "lab.yaml", labPlanYAML)

	if err := h.run("dr", "chaos", "cordon-node", "--node", "w1", "--plan", plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "kubectl cordon w1") || !strings.Contains(h.out.String(), "Uncordon node w1") {
		t.Errorf("cordon output:\n%s", h.out.String())
	}
	if err := h.run("dr", "chaos", "drain-node", "--node", "w1", "--plan", plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "kubectl drain w1 --ignore-daemonsets") {
		t.Errorf("drain output:\n%s", h.out.String())
	}
	if err := h.run("dr", "chaos", "drain-node", "--node", "cp", "--plan", plan); err == nil || !strings.Contains(err.Error(), "control-plane") {
		t.Errorf("the control plane is never touched: %v", err)
	}
	// Cordon has no option to accept other namespaces: only a drain evicts.
	if err := h.run("dr", "chaos", "cordon-node", "--node", "w1", "--plan", plan, "--allow-other-namespaces"); err == nil {
		t.Error("--allow-other-namespaces belongs to drain-node only")
	}
	node, _ := c.Client.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if node.Spec.Unschedulable {
		t.Error("dry runs must not cordon")
	}
}

// ---- extcheck -----------------------------------------------------------------------------------------------

func withCertKinds(c *wiretest.Cluster) *wiretest.Cluster {
	c.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "velero.io", Version: "v1", Resource: "backups"}:                       "BackupList",
		{Group: "velero.io", Version: "v1", Resource: "schedules"}:                     "ScheduleList",
		{Group: "velero.io", Version: "v1", Resource: "backupstoragelocations"}:        "BackupStorageLocationList",
		{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}: "VolumeSnapshotList",
		{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}:            "CertificateList",
	})
	return c
}

func extCluster() *wiretest.Cluster {
	return withCertKinds(wiretest.NewCluster(wiretest.Namespace("shop"), wiretest.Deployment("shop", "api", 2)))
}

func TestExtCheckPrintsFindingsAndTheThreeTwoOneSummary(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": extCluster()})
	plan := writeFile(t, "shop.yaml", devPlan)
	if err := h.run("dr", "extcheck", "--plan", plan); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"External dependencies of dev, against itself", "Namespaces: shop", "DR140", "3-2-1", "Immutability:", "only a drill proves"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestExtCheckAgainstADRClusterAndAsJSON(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{
		"dev": extCluster(), "dr": withCertKinds(wiretest.NewCluster(wiretest.Namespace("other"))),
	})
	plan := writeFile(t, "shop.yaml", devPlan)
	if err := h.run("dr", "extcheck", "--plan", plan, "--to", "dr", "-o", "json"); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Standby  string `json:"standby"`
		Findings []struct {
			Rule string `json:"rule"`
			Name string `json:"name"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, h.out.String())
	}
	var missing bool
	for _, f := range report.Findings {
		if f.Rule == "DR125" && f.Name == "shop" {
			missing = true
		}
	}
	if report.Standby != "dr" || !missing {
		t.Errorf("standby %q, findings %+v", report.Standby, report.Findings)
	}
}

func TestExtCheckFailOnAndSeverityFlags(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": extCluster()})
	plan := writeFile(t, "shop.yaml", devPlan)

	// DR140 is high: the check fails for CI.
	if err := h.run("dr", "extcheck", "--plan", plan, "--fail-on", "high"); err == nil || !strings.Contains(err.Error(), "at or above high") {
		t.Errorf("--fail-on high: %v", err)
	}
	// Hiding findings does not hide the failure: the gate looks at what exists.
	if err := h.run("dr", "extcheck", "--plan", plan, "--fail-on", "high", "--min-severity", "high"); err == nil {
		t.Error("--fail-on must count every finding, not only the visible ones")
	}
	if err := h.run("dr", "extcheck", "--plan", plan); err != nil {
		t.Errorf("without --fail-on the exit code is 0: %v", err)
	}
	if err := h.run("dr", "extcheck", "--plan", plan, "--min-severity", "urgent"); err == nil {
		t.Error("an unknown severity must be refused")
	}
	if err := h.run("dr", "extcheck", "--plan", plan, "--fail-on", "urgent"); err == nil {
		t.Error("an unknown --fail-on must be refused")
	}
}

func TestExtCheckWithoutAPlanNeedsNamespacesAndAContext(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": extCluster()})
	if err := h.run("dr", "extcheck", "--context", "dev"); err == nil || !strings.Contains(err.Error(), "no namespace") {
		t.Errorf("no namespaces: %v", err)
	}
	if err := h.run("dr", "extcheck", "--context", "dev", "-n", "shop"); err != nil {
		t.Errorf("namespace and context given: %v", err)
	}
}
