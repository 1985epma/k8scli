package wire_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/chaos"
	"github.com/1985epma/k8scli/internal/dr/wire"
	"github.com/1985epma/k8scli/internal/dr/wire/wiretest"
)

func readyPod(namespace, name, app string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: k8stypes.UID("uid-" + name), Labels: map[string]string{"app": app}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func labNamespace(name string, allowed bool) *corev1.Namespace {
	n := wiretest.Namespace(name)
	if allowed {
		n.Labels = map[string]string{chaos.LabelAllowed: "true"}
	}
	return n
}

func chaosPlan(labOnly bool) *dr.Plan {
	return &dr.Plan{
		Version: dr.PlanVersion, Name: "lab", Context: "dev", Safety: dr.Safety{LabOnly: labOnly},
		Apps: []dr.App{{Name: "api", Namespace: "chaos-test", Workloads: []string{"api"}, Tier: 1}},
	}
}

func chaosCluster(allowed bool) *wiretest.Cluster {
	return wiretest.NewCluster(
		labNamespace("chaos-test", allowed), wiretest.Deployment("chaos-test", "api", 3),
		readyPod("chaos-test", "api-1", "api"), readyPod("chaos-test", "api-2", "api"), readyPod("chaos-test", "api-3", "api"),
	)
}

func killRequest(plan *dr.Plan) wire.ChaosRequest {
	return wire.ChaosRequest{
		Plan: plan, Kind: wire.ChaosKillPods, Namespace: "chaos-test", Workload: "api",
		Window: 5 * time.Second, Poll: time.Second, MaxDuration: 30 * time.Second,
	}
}

func podCount(t *testing.T, c *wiretest.Cluster) int {
	t.Helper()
	list, err := c.Client.CoreV1().Pods("chaos-test").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(list.Items)
}

func TestChaosKillPodsRunsEndToEndAndSavesTheResult(t *testing.T) {
	c := chaosCluster(true)
	dir := t.TempDir()
	stack := wiretest.NewStack(t, wire.Options{Dir: dir, Actor: "ana"}, map[string]*wiretest.Cluster{"dev": c}, nil)

	result, err := stack.Chaos(context.Background(), killRequest(chaosPlan(true)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != "executed" || !result.SteadyBefore || !result.Absorbed || !result.Recovered {
		t.Errorf("result = %+v", result)
	}
	if podCount(t, c) != 2 {
		t.Errorf("one pod must have been evicted, %d remain", podCount(t, c))
	}
	files, _ := filepath.Glob(filepath.Join(dir, "chaos", "chaos-*.json"))
	audit, _ := os.ReadFile(filepath.Join(dir, "chaos", "audit.jsonl"))
	if len(files) != 1 || !strings.Contains(string(audit), `"actor":"ana"`) {
		t.Errorf("the result and the audit line must be stored: %v %q", files, audit)
	}
}

func TestChaosDryRunInjectsAndStoresNothing(t *testing.T) {
	c := chaosCluster(true)
	dir := t.TempDir()
	stack := wiretest.NewStack(t, wire.Options{Dir: dir}, map[string]*wiretest.Cluster{"dev": c}, nil)
	req := killRequest(chaosPlan(true))
	req.DryRun = true

	result, err := stack.Chaos(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != "dry-run" || podCount(t, c) != 3 {
		t.Errorf("a dry run must change nothing: %+v, %d pods", result, podCount(t, c))
	}
	if _, err := os.Stat(filepath.Join(dir, "chaos")); !os.IsNotExist(err) {
		t.Errorf("a dry run must not write anything: %v", err)
	}
}

func TestChaosRefusals(t *testing.T) {
	ctx := context.Background()

	notLab := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": chaosCluster(true)}, nil)
	if _, err := notLab.Chaos(ctx, killRequest(chaosPlan(false))); err == nil || !strings.Contains(err.Error(), "labOnly") {
		t.Errorf("a plan that is not a lab: %v", err)
	}

	c := chaosCluster(false)
	unlabelled := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": c}, nil)
	if _, err := unlabelled.Chaos(ctx, killRequest(chaosPlan(true))); err == nil || !strings.Contains(err.Error(), chaos.LabelAllowed) {
		t.Errorf("an unlabelled namespace: %v", err)
	}
	if podCount(t, c) != 3 {
		t.Error("nothing may be evicted after a refusal")
	}

	c = chaosCluster(true)
	readOnly := wiretest.NewStack(t, wire.Options{ReadOnly: true}, map[string]*wiretest.Cluster{"dev": c}, nil)
	if _, err := readOnly.Chaos(ctx, killRequest(chaosPlan(true))); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("a read-only stack: %v", err)
	}
	dry := killRequest(chaosPlan(true))
	dry.DryRun = true
	if _, err := readOnly.Chaos(ctx, dry); err != nil {
		t.Errorf("a dry run is allowed in read-only mode: %v", err)
	}

	if _, err := notLab.Chaos(ctx, wire.ChaosRequest{Plan: chaosPlan(true), Kind: "burn"}); err == nil {
		t.Error("an unknown kind must be refused")
	}
	if _, err := notLab.Chaos(ctx, wire.ChaosRequest{}); err == nil {
		t.Error("a request without a plan must be refused")
	}
}

func TestChaosPhraseOnlyForProductionLookingContexts(t *testing.T) {
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": chaosCluster(true)}, nil)
	if got := stack.ChaosPhrase(chaosPlan(true)); got != "" {
		t.Errorf("phrase for a lab context = %q", got)
	}
	prod := chaosPlan(true)
	prod.Context = "eks-prod"
	if got := stack.ChaosPhrase(prod); got != "chaos eks-prod" {
		t.Errorf("phrase = %q", got)
	}
}

func TestChaosCordonNodeRoundTripsThroughTheStack(t *testing.T) {
	c := wiretest.NewCluster(wiretest.ControlPlane("cp"), wiretest.Node("w1"), wiretest.Node("w2"),
		labNamespace("chaos-test", true), wiretest.Deployment("chaos-test", "api", 2),
		readyPod("chaos-test", "api-1", "api"), readyPod("chaos-test", "api-2", "api"))
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": c}, nil)
	req := wire.ChaosRequest{Plan: chaosPlan(true), Kind: wire.ChaosCordonNode, Node: "w1", Window: 5 * time.Second, Poll: time.Second, MaxDuration: 30 * time.Second}

	result, err := stack.Chaos(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecoveryRun == nil || !result.RecoveryRun.Applied {
		t.Errorf("the node must be uncordoned again: %+v", result.RecoveryRun)
	}
	node, _ := c.Client.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if node.Spec.Unschedulable {
		t.Error("w1 must be schedulable after the experiment")
	}

	cp := req
	cp.Node = "cp"
	if _, err := stack.Chaos(context.Background(), cp); err == nil || !strings.Contains(err.Error(), "control-plane") {
		t.Errorf("the control plane is never touched: %v", err)
	}
}

func TestExtChecksThroughTheStack(t *testing.T) {
	kinds := map[schema.GroupVersionResource]string{
		{Group: "velero.io", Version: "v1", Resource: "backups"}:                       "BackupList",
		{Group: "velero.io", Version: "v1", Resource: "schedules"}:                     "ScheduleList",
		{Group: "velero.io", Version: "v1", Resource: "backupstoragelocations"}:        "BackupStorageLocationList",
		{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}: "VolumeSnapshotList",
		{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}:            "CertificateList",
	}
	withKinds := func(c *wiretest.Cluster) *wiretest.Cluster {
		c.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds)
		return c
	}
	primary := withKinds(wiretest.NewCluster(wiretest.Namespace("shop"), wiretest.Deployment("shop", "api", 2)))
	standby := withKinds(wiretest.NewCluster(wiretest.Namespace("other")))
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": primary, "dr": standby}, nil)

	plan := &dr.Plan{Version: dr.PlanVersion, Name: "p", Context: "dev", DRContext: "dr", Apps: []dr.App{{Name: "shop", Namespace: "shop"}}}
	report, err := stack.ExtChecks(context.Background(), plan, "", wire.ExtChecksOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Context != "dev" || report.Standby != "dr" {
		t.Errorf("context %q standby %q", report.Context, report.Standby)
	}
	var sawMissingNamespace bool
	for _, f := range report.Findings {
		if f.Rule == "DR125" && f.Name == "shop" {
			sawMissingNamespace = true
		}
	}
	if !sawMissingNamespace {
		t.Errorf("the plan's DR context is the standby, so shop is missing there: %+v", report.Findings)
	}

	// An explicit standby overrides the plan's, and a bad severity is refused.
	if r, err := stack.ExtChecks(context.Background(), plan, "dev", wire.ExtChecksOptions{Standby: "dev"}); err != nil || r.Standby != "dev" {
		t.Errorf("explicit standby: %v %+v", err, r)
	}
	if _, err := stack.ExtChecks(context.Background(), plan, "", wire.ExtChecksOptions{MinSeverity: "urgent"}); err == nil {
		t.Error("an unknown severity must be refused")
	}
	if _, err := stack.ExtChecks(context.Background(), plan, "ghost", wire.ExtChecksOptions{}); err == nil {
		t.Error("an unknown context must be refused")
	}
}
