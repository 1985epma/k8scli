package checks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

// findingIDs returns "RULE:severity" for every finding of a workload.
func ruleSet(findings []Finding) map[string]Severity {
	out := map[string]Severity{}
	for _, f := range findings {
		out[f.Rule] = f.Severity
	}
	return out
}

func workloadOf(t *testing.T, r *Report, ns, name string) WorkloadResult {
	t.Helper()
	for _, n := range r.Namespaces {
		if n.Name != ns {
			continue
		}
		for _, w := range n.Workloads {
			if w.Name == name {
				return w
			}
		}
	}
	t.Fatalf("workload %s/%s not in the report", ns, name)
	return WorkloadResult{}
}

func namespaceOf(t *testing.T, r *Report, ns string) NamespaceResult {
	t.Helper()
	for _, n := range r.Namespaces {
		if n.Name == ns {
			return n
		}
	}
	t.Fatalf("namespace %s not in the report", ns)
	return NamespaceResult{}
}

func healthyObjects() []runtime.Object {
	return with(twoNodes(),
		namespace("shop"),
		deployment("shop", "api", 2),
		pdbFor("shop", "api", "api"),
		pod("shop", "api-1", "api", "n1"), pod("shop", "api-2", "api", "n2"),
	)
}

func TestHealthyWorkloadHasNoFindingsAndScores100(t *testing.T) {
	report, err := Readiness(context.Background(), newEnv(healthyObjects()...), ReadinessOptions{Backups: noBackups})
	if err != nil {
		t.Fatal(err)
	}
	w := workloadOf(t, report, "shop", "api")
	if len(w.Findings) != 0 || w.Score != 100 {
		t.Errorf("findings = %+v score = %d", w.Findings, w.Score)
	}
	if report.Score != 100 || len(report.Cluster) != 0 {
		t.Errorf("overall = %d cluster = %+v", report.Score, report.Cluster)
	}
	if report.Disclaimer != Disclaimer || !strings.Contains(report.Disclaimer, "SIGNALS") {
		t.Errorf("disclaimer = %q", report.Disclaimer)
	}
}

func TestDR001SingleReplica(t *testing.T) {
	objects := with(twoNodes(), namespace("shop"), deployment("shop", "api", 1), statefulSet("shop", "db", 1))
	report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: backupAt(fixedNow)})

	for _, name := range []string{"api", "db"} {
		w := workloadOf(t, report, "shop", name)
		if got := ruleSet(w.Findings)["DR001"]; got != SeverityHigh {
			t.Errorf("%s: DR001 = %q; want high", name, got)
		}
	}
	// The fix is a concrete command.
	for _, f := range workloadOf(t, report, "shop", "api").Findings {
		if f.Rule == "DR001" && !strings.Contains(f.Fix, "kubectl scale deployment/api -n shop --replicas=2") {
			t.Errorf("fix = %q", f.Fix)
		}
	}
}

func TestScaledToZeroWorkloadsAreStoppedAndNotJudged(t *testing.T) {
	objects := with(twoNodes(), namespace("shop"), deployment("shop", "api", 0, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].ReadinessProbe = nil
	}))
	report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
	w := workloadOf(t, report, "shop", "api")
	if !w.Stopped || len(w.Findings) != 0 || w.Score != 100 {
		t.Errorf("workload = %+v", w)
	}
	if report.Summary.Stopped != 1 || report.Score != 100 {
		t.Errorf("summary = %+v score = %d", report.Summary, report.Score)
	}
}

func TestDR002PodDisruptionBudget(t *testing.T) {
	cases := map[string]struct {
		pdbs []runtime.Object
		want bool
	}{
		"none":              {nil, true},
		"matching":          {[]runtime.Object{pdbFor("shop", "p", "api")}, false},
		"other selector":    {[]runtime.Object{pdbFor("shop", "p", "other")}, true},
		"other namespace":   {[]runtime.Object{pdbFor("billing", "p", "api")}, true},
		"matching + others": {[]runtime.Object{pdbFor("shop", "x", "other"), pdbFor("shop", "p", "api")}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			objects := with(twoNodes(), namespace("shop"), deployment("shop", "api", 2))
			objects = append(objects, tc.pdbs...)
			report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
			_, fired := ruleSet(workloadOf(t, report, "shop", "api").Findings)["DR002"]
			if fired != tc.want {
				t.Errorf("DR002 fired = %v; want %v", fired, tc.want)
			}
		})
	}
}

func TestDR002SuggestsAConcretePDBCommand(t *testing.T) {
	report, _ := Readiness(context.Background(), newEnv(with(twoNodes(), namespace("shop"), deployment("shop", "api", 2))...), ReadinessOptions{Backups: noBackups})
	for _, f := range workloadOf(t, report, "shop", "api").Findings {
		if f.Rule == "DR002" && f.Fix != "kubectl create poddisruptionbudget api -n shop --selector=app=api --min-available=1" {
			t.Errorf("fix = %q", f.Fix)
		}
	}
}

func TestDR002AndDR003DoNotApplyToSingleReplicaWorkloads(t *testing.T) {
	objects := with(twoNodes(), namespace("shop"), deployment("shop", "api", 1, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.TopologySpreadConstraints = nil
	}))
	report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
	rules := ruleSet(workloadOf(t, report, "shop", "api").Findings)
	if _, ok := rules["DR002"]; ok {
		t.Error("DR002 must not fire for one replica")
	}
	if _, ok := rules["DR003"]; ok {
		t.Error("DR003 must not fire for one replica")
	}
}

func TestDR003Spread(t *testing.T) {
	required := &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "kubernetes.io/hostname"}},
	}}
	preferred := &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{Weight: 10}},
	}}
	nodeAffinityOnly := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}

	cases := map[string]struct {
		mutate func(*appsv1.Deployment)
		want   bool
	}{
		"spread constraints": {func(*appsv1.Deployment) {}, false},
		"required anti-affinity": {func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.TopologySpreadConstraints = nil
			d.Spec.Template.Spec.Affinity = required
		}, false},
		"preferred anti-affinity": {func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.TopologySpreadConstraints = nil
			d.Spec.Template.Spec.Affinity = preferred
		}, false},
		"nothing": {func(d *appsv1.Deployment) { d.Spec.Template.Spec.TopologySpreadConstraints = nil }, true},
		"node affinity is not spreading": {func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.TopologySpreadConstraints = nil
			d.Spec.Template.Spec.Affinity = nodeAffinityOnly
		}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			objects := with(twoNodes(), namespace("shop"), deployment("shop", "api", 2, tc.mutate), pdbFor("shop", "p", "api"))
			report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
			_, fired := ruleSet(workloadOf(t, report, "shop", "api").Findings)["DR003"]
			if fired != tc.want {
				t.Errorf("DR003 fired = %v; want %v", fired, tc.want)
			}
		})
	}
}

func TestDR004Probes(t *testing.T) {
	cases := map[string]struct {
		mutate func(*appsv1.Deployment)
		want   map[Severity]bool
	}{
		"both present":      {func(*appsv1.Deployment) {}, map[Severity]bool{}},
		"no readiness":      {func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].ReadinessProbe = nil }, map[Severity]bool{SeverityMedium: true}},
		"liveness only gap": {func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].LivenessProbe = nil }, map[Severity]bool{SeverityLow: true}},
		"neither": {func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].ReadinessProbe = nil
			d.Spec.Template.Spec.Containers[0].LivenessProbe = nil
		}, map[Severity]bool{SeverityMedium: true, SeverityLow: true}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			objects := with(twoNodes(), namespace("shop"), deployment("shop", "api", 2, tc.mutate), pdbFor("shop", "p", "api"))
			report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
			got := map[Severity]bool{}
			for _, f := range workloadOf(t, report, "shop", "api").Findings {
				if f.Rule == "DR004" {
					got[f.Severity] = true
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("DR004 severities = %v; want %v", got, tc.want)
			}
			for sev := range tc.want {
				if !got[sev] {
					t.Errorf("missing DR004 %s: %v", sev, got)
				}
			}
		})
	}
}

func TestDR005ResourceRequests(t *testing.T) {
	none := func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Resources.Requests = nil }
	cpuOnly := func(d *appsv1.Deployment) {
		delete(d.Spec.Template.Spec.Containers[0].Resources.Requests, corev1.ResourceMemory)
	}
	cases := map[string]struct {
		mutate func(*appsv1.Deployment)
		want   Severity
	}{"present": {func(*appsv1.Deployment) {}, ""}, "none": {none, SeverityMedium}, "partial": {cpuOnly, SeverityLow}}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			objects := with(twoNodes(), namespace("shop"), deployment("shop", "api", 2, tc.mutate), pdbFor("shop", "p", "api"))
			report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
			if got := ruleSet(workloadOf(t, report, "shop", "api").Findings)["DR005"]; got != tc.want {
				t.Errorf("DR005 = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestDR007AllReplicasOnOneNode(t *testing.T) {
	cases := map[string]struct {
		pods []runtime.Object
		want bool
	}{
		"same node":      {[]runtime.Object{pod("shop", "a1", "api", "n1"), pod("shop", "a2", "api", "n1")}, true},
		"two nodes":      {[]runtime.Object{pod("shop", "a1", "api", "n1"), pod("shop", "a2", "api", "n2")}, false},
		"one pod only":   {[]runtime.Object{pod("shop", "a1", "api", "n1")}, false},
		"other app pods": {[]runtime.Object{pod("shop", "x1", "other", "n1"), pod("shop", "x2", "other", "n1")}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			objects := with(twoNodes(), namespace("shop"), deployment("shop", "api", 2), pdbFor("shop", "p", "api"))
			objects = append(objects, tc.pods...)
			report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
			_, fired := ruleSet(workloadOf(t, report, "shop", "api").Findings)["DR007"]
			if fired != tc.want {
				t.Errorf("DR007 fired = %v; want %v", fired, tc.want)
			}
		})
	}
}

func TestDR007IsSilentOnASingleNodeCluster(t *testing.T) {
	objects := []runtime.Object{node("only"), namespace("shop"), deployment("shop", "api", 2), pdbFor("shop", "p", "api"),
		pod("shop", "a1", "api", "only"), pod("shop", "a2", "api", "only")}
	report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
	if _, fired := ruleSet(workloadOf(t, report, "shop", "api").Findings)["DR007"]; fired {
		t.Error("with one node DR009 already says it: DR007 must stay silent")
	}
	if len(report.Cluster) != 1 || report.Cluster[0].Rule != "DR009" {
		t.Errorf("cluster findings = %+v", report.Cluster)
	}
}

func TestDR007IgnoresPodsThatAreNotRunning(t *testing.T) {
	pending := pod("shop", "a2", "api", "n1")
	pending.Status.Phase = corev1.PodPending
	objects := with(twoNodes(), namespace("shop"), deployment("shop", "api", 2), pdbFor("shop", "p", "api"), pod("shop", "a1", "api", "n1"), pending)
	report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
	if _, fired := ruleSet(workloadOf(t, report, "shop", "api").Findings)["DR007"]; fired {
		t.Error("a pending pod does not count as running on the node")
	}
}

func TestDR011LatestTags(t *testing.T) {
	for image, want := range map[string]bool{
		"nginx":                       true,
		"nginx:latest":                true,
		"registry.local:5000/app":     true,
		"registry.local:5000/app:1.2": false,
		"nginx:1.27":                  false,
		"app@sha256:abc":              false,
		"app:latest@sha256:abc":       false,
	} {
		if got := floatingImage(image); got != want {
			t.Errorf("floatingImage(%q) = %v; want %v", image, got, want)
		}
	}

	objects := with(twoNodes(), namespace("shop"), deployment("shop", "api", 2, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Image = "nginx"
	}), pdbFor("shop", "p", "api"))
	report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
	if got := ruleSet(workloadOf(t, report, "shop", "api").Findings)["DR011"]; got != SeverityLow {
		t.Errorf("DR011 = %q; want low", got)
	}
}

func TestDR008NoBackupAtAll(t *testing.T) {
	objects := with(twoNodes(), namespace("shop"), statefulSet("shop", "db", 2), pdbFor("shop", "p", "db"), pvc("shop", "data-db-0", nil))
	report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})

	if got := ruleSet(workloadOf(t, report, "shop", "db").Findings)["DR008"]; got != SeverityHigh {
		t.Errorf("workload DR008 = %q; want high", got)
	}
	ns := namespaceOf(t, report, "shop")
	found := false
	for _, f := range ns.Findings {
		if f.Rule == "DR008" && strings.Contains(f.Message, "data-db-0") && strings.Contains(f.Fix, "velero schedule create shop-daily") {
			found = true
		}
	}
	if !found {
		t.Errorf("namespace findings = %+v", ns.Findings)
	}
}

func TestDR006StaleBackup(t *testing.T) {
	objects := with(twoNodes(), namespace("shop"), deployment("shop", "web", 2), pdbFor("shop", "p", "web"), pvc("shop", "data", nil))

	stale, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: backupAt(fixedNow.Add(-72 * time.Hour))})
	ns := namespaceOf(t, stale, "shop")
	if got := ruleSet(ns.Findings)["DR006"]; got != SeverityMedium {
		t.Errorf("DR006 = %q; findings = %+v", got, ns.Findings)
	}
	if !strings.Contains(ns.Findings[0].Message, "3 days") {
		t.Errorf("message = %q", ns.Findings[0].Message)
	}

	fresh, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: backupAt(fixedNow.Add(-2 * time.Hour))})
	if len(namespaceOf(t, fresh, "shop").Findings) != 0 {
		t.Errorf("a fresh backup must be quiet: %+v", namespaceOf(t, fresh, "shop").Findings)
	}

	// The limit is configurable.
	custom, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: backupAt(fixedNow.Add(-2 * time.Hour)), BackupMaxAge: time.Hour})
	if _, ok := ruleSet(namespaceOf(t, custom, "shop").Findings)["DR006"]; !ok {
		t.Error("a 1h limit must flag a 2h old backup")
	}
}

func TestBackupFinderErrorsBecomeNotices(t *testing.T) {
	finder := func(context.Context, *dr.Env, string) (time.Time, bool, error) {
		return time.Time{}, false, errors.New("forbidden: no access to velero")
	}
	objects := with(twoNodes(), namespace("shop"), statefulSet("shop", "db", 2), pdbFor("shop", "p", "db"))
	report, err := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: finder})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ruleSet(workloadOf(t, report, "shop", "db").Findings)["DR008"]; ok {
		t.Error("an unknown backup state must not be reported as 'no backup'")
	}
	if len(report.Notices) == 0 || !strings.Contains(report.Notices[0], "forbidden") {
		t.Errorf("notices = %v", report.Notices)
	}
}

func TestNoBackupRulesWithoutStatefulData(t *testing.T) {
	called := false
	finder := func(context.Context, *dr.Env, string) (time.Time, bool, error) {
		called = true
		return time.Time{}, false, nil
	}
	objects := with(twoNodes(), namespace("shop"), deployment("shop", "api", 2), pdbFor("shop", "p", "api"))
	report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: finder})
	if called || len(namespaceOf(t, report, "shop").Findings) != 0 {
		t.Errorf("a stateless namespace needs no backup check (called=%v)", called)
	}
}

func TestDR009SingleNodeCluster(t *testing.T) {
	one := Readiness
	report, _ := one(context.Background(), newEnv(node("only"), namespace("shop")), ReadinessOptions{Backups: noBackups})
	if len(report.Cluster) != 1 || report.Cluster[0].Rule != "DR009" || report.Cluster[0].Severity != SeverityMedium {
		t.Errorf("cluster findings = %+v", report.Cluster)
	}
	if report.Score != 100-6 {
		t.Errorf("score = %d; a medium cluster finding costs 6", report.Score)
	}

	report, _ = one(context.Background(), newEnv(twoNodes()...), ReadinessOptions{Backups: noBackups})
	if len(report.Cluster) != 0 {
		t.Errorf("two nodes must be quiet: %+v", report.Cluster)
	}
}

func TestDR010StorageClassSnapshotSupport(t *testing.T) {
	base := with(twoNodes(), namespace("shop"), deployment("shop", "web", 2), pdbFor("shop", "p", "web"),
		storageClass("fast", "ebs.csi.aws.com", false), storageClass("standard", "rancher.io/local-path", true),
		pvc("shop", "a", ptr("fast")), pvc("shop", "b", nil))

	classes := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), snapshotKinds, snapshotClass("ebs", "ebs.csi.aws.com"))
	env := newEnv(base...)
	env.Dynamic = classes
	report, _ := Readiness(context.Background(), env, ReadinessOptions{Backups: backupAt(fixedNow)})

	// "fast" has a matching class; the default class "standard" (used by pvc b) does not.
	var names []string
	for _, f := range namespaceOf(t, report, "shop").Findings {
		if f.Rule == "DR010" {
			names = append(names, f.Name)
		}
	}
	if len(names) != 1 || names[0] != "standard" {
		t.Errorf("DR010 on %v; want only the default class \"standard\"", names)
	}
}

func TestDR010WhenTheSnapshotCRDsAreMissing(t *testing.T) {
	base := with(twoNodes(), namespace("shop"), storageClass("fast", "ebs.csi.aws.com", false), pvc("shop", "a", ptr("fast")))
	env := newEnv(base...)
	missing := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), snapshotKinds)
	missing.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "volumesnapshotclasses"}, "")
	})
	env.Dynamic = missing

	report, _ := Readiness(context.Background(), env, ReadinessOptions{Backups: backupAt(fixedNow)})
	var message string
	for _, f := range namespaceOf(t, report, "shop").Findings {
		if f.Rule == "DR010" {
			message = f.Message
		}
	}
	if !strings.Contains(message, "CRDs are not installed") {
		t.Errorf("message = %q", message)
	}
}

func TestDR010IsSkippedWithoutADynamicClient(t *testing.T) {
	env := newEnv(with(twoNodes(), namespace("shop"), storageClass("fast", "x", false), pvc("shop", "a", ptr("fast")))...)
	env.Dynamic = nil
	report, err := Readiness(context.Background(), env, ReadinessOptions{Backups: backupAt(fixedNow)})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range namespaceOf(t, report, "shop").Findings {
		if f.Rule == "DR010" {
			t.Errorf("unexpected %+v", f)
		}
	}
}

// ---- scoring ------------------------------------------------------------------------------------------------

func fs(severity Severity, n int) []Finding {
	out := make([]Finding, n)
	for i := range out {
		out[i] = Finding{Severity: severity}
	}
	return out
}

func TestScoreArithmetic(t *testing.T) {
	for name, tc := range map[string]struct {
		findings []Finding
		want     int
	}{
		"none":                 {nil, 100},
		"one high":             {fs(SeverityHigh, 1), 85},
		"two high":             {fs(SeverityHigh, 2), 70},
		"high is capped at 60": {fs(SeverityHigh, 9), 40},
		"one medium":           {fs(SeverityMedium, 1), 94},
		"medium capped at 30":  {fs(SeverityMedium, 20), 70},
		"low capped at 10":     {fs(SeverityLow, 40), 90},
		"everything capped":    {append(append(fs(SeverityHigh, 9), fs(SeverityMedium, 9)...), fs(SeverityLow, 9)...), 0},
		"mixed":                {append(append(fs(SeverityHigh, 1), fs(SeverityMedium, 2)...), fs(SeverityLow, 3)...), 100 - 15 - 12 - 6},
	} {
		if got := workloadScore(tc.findings); got != tc.want {
			t.Errorf("%s: score = %d; want %d", name, got, tc.want)
		}
	}
}

func TestNamespaceScoreFormula(t *testing.T) {
	ns := &NamespaceResult{Workloads: []WorkloadResult{{Score: 100}, {Score: 40}}}
	// 0.7 x mean(70) + 0.3 x worst(40) = 49 + 12 = 61
	if got := namespaceScore(ns); got != 61 {
		t.Errorf("namespace score = %d; want 61", got)
	}
	// Namespace-level findings are deducted afterwards (one high = 15).
	ns.Findings = fs(SeverityHigh, 1)
	if got := namespaceScore(ns); got != 46 {
		t.Errorf("with a namespace finding = %d; want 46", got)
	}
	// Stopped workloads are ignored; an empty namespace starts at 100.
	if got := namespaceScore(&NamespaceResult{Workloads: []WorkloadResult{{Stopped: true, Score: 100}}}); got != 100 {
		t.Errorf("only stopped workloads = %d", got)
	}
	if got := namespaceScore(&NamespaceResult{}); got != 100 {
		t.Errorf("empty = %d", got)
	}
	// The floor is 0.
	if got := namespaceScore(&NamespaceResult{Workloads: []WorkloadResult{{Score: 5}}, Findings: fs(SeverityHigh, 9)}); got != 0 {
		t.Errorf("floor = %d", got)
	}
}

func TestOverallScoreWeightsNamespacesByRunningWorkloads(t *testing.T) {
	// "big" has three single-replica workloads (each 85 minus other gaps), "small" is healthy.
	objects := with(twoNodes(), namespace("big"), namespace("small"),
		deployment("big", "a", 1), deployment("big", "b", 1), deployment("big", "c", 1),
		deployment("small", "ok", 2), pdbFor("small", "p", "ok"),
		pod("small", "ok-1", "ok", "n1"), pod("small", "ok-2", "ok", "n2"))
	report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})

	big, small := namespaceOf(t, report, "big"), namespaceOf(t, report, "small")
	if big.Score != 85 || small.Score != 100 {
		t.Fatalf("namespace scores = %d, %d", big.Score, small.Score)
	}
	// (85*3 + 100*1) / 4 = 88.75 -> 89
	if report.Score != 89 {
		t.Errorf("overall = %d; want 89", report.Score)
	}
	if report.Summary.High != 3 || report.Summary.Workloads != 4 || report.Summary.Namespaces != 2 {
		t.Errorf("summary = %+v", report.Summary)
	}
}

// ---- options ------------------------------------------------------------------------------------------------

func TestSeverityFilterHidesFindingsButKeepsTheScore(t *testing.T) {
	objects := with(twoNodes(), namespace("shop"), deployment("shop", "api", 1, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Image = "nginx"      // low
		d.Spec.Template.Spec.Containers[0].LivenessProbe = nil  // low
		d.Spec.Template.Spec.Containers[0].ReadinessProbe = nil // medium
	}))
	all, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
	high, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups, MinSeverity: SeverityHigh})

	if len(workloadOf(t, high, "shop", "api").Findings) != 1 {
		t.Errorf("high filter shows %+v", workloadOf(t, high, "shop", "api").Findings)
	}
	if len(workloadOf(t, all, "shop", "api").Findings) != 4 {
		t.Errorf("unfiltered shows %d findings", len(workloadOf(t, all, "shop", "api").Findings))
	}
	if all.Score != high.Score || all.Summary != high.Summary {
		t.Errorf("filter changed the score or the summary: %d/%+v vs %d/%+v", all.Score, all.Summary, high.Score, high.Summary)
	}
	if got, err := ParseSeverity("medium"); err != nil || got != SeverityMedium {
		t.Errorf("ParseSeverity = %q, %v", got, err)
	}
	if _, err := ParseSeverity("critical"); err == nil {
		t.Error("an unknown severity must be rejected")
	}
}

func TestNamespaceSelection(t *testing.T) {
	objects := with(twoNodes(), namespace("shop"), namespace("billing"), namespace("kube-system"),
		deployment("shop", "a", 2), deployment("billing", "b", 2), deployment("kube-system", "dns", 2))

	report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
	if len(report.Namespaces) != 2 || report.Namespaces[0].Name != "billing" || report.Namespaces[1].Name != "shop" {
		t.Errorf("default namespaces = %+v (system namespaces are skipped, order is alphabetical)", report.Namespaces)
	}

	withSystem, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups, IncludeSystem: true})
	if len(withSystem.Namespaces) != 3 {
		t.Errorf("IncludeSystem namespaces = %d", len(withSystem.Namespaces))
	}

	only, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups, Namespaces: []string{"shop"}})
	if len(only.Namespaces) != 1 || only.Namespaces[0].Name != "shop" {
		t.Errorf("explicit namespaces = %+v", only.Namespaces)
	}
}

func TestMaxWorkloadsBoundsTheWork(t *testing.T) {
	objects := with(twoNodes(), namespace("a"), namespace("b"),
		deployment("a", "w1", 2), deployment("a", "w2", 2), deployment("a", "w3", 2), deployment("b", "w4", 2))
	report, err := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups, MaxWorkloads: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Truncated || report.Summary.Workloads != 2 || len(report.Notices) == 0 {
		t.Errorf("truncated=%v workloads=%d notices=%v", report.Truncated, report.Summary.Workloads, report.Notices)
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	objects := with(twoNodes(), namespace("shop"),
		statefulSet("shop", "zdb", 1), deployment("shop", "api", 1), deployment("shop", "web", 1), pvc("shop", "d", nil))
	var first []string
	for i := 0; i < 5; i++ {
		report, _ := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups})
		var lines []string
		for _, ns := range report.Namespaces {
			for _, f := range ns.Findings {
				lines = append(lines, "ns:"+f.Rule+f.Name)
			}
			for _, w := range ns.Workloads {
				lines = append(lines, w.Kind+"/"+w.Name)
				for _, f := range w.Findings {
					lines = append(lines, string(f.Severity)+":"+f.Rule)
				}
			}
		}
		if first == nil {
			first = lines
			continue
		}
		if strings.Join(first, "|") != strings.Join(lines, "|") {
			t.Fatalf("run %d differs:\n%v\n%v", i, first, lines)
		}
	}
	if first[0] != "ns:DR008shop" {
		t.Errorf("namespace findings come first, high severity first: %v", first)
	}
}

func TestRulesAreUniqueAndDocumented(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Rules() {
		if seen[r.ID] || r.Title == "" || r.Why == "" || r.Severity == "" {
			t.Errorf("bad rule %+v", r)
		}
		seen[r.ID] = true
	}
	if len(seen) != 11 {
		t.Errorf("rules = %d; want 11", len(seen))
	}
}

func TestReadinessNeedsAClient(t *testing.T) {
	if _, err := Readiness(context.Background(), &dr.Env{}, ReadinessOptions{}); err == nil {
		t.Error("expected an error without a client")
	}
}

func TestPaginationReadsEveryObject(t *testing.T) {
	// 1200 deployments in one namespace come back through 500-item pages.
	var objects []runtime.Object
	objects = append(objects, twoNodes()...)
	objects = append(objects, namespace("big"))
	for i := 0; i < 1200; i++ {
		objects = append(objects, deployment("big", "d"+itoa(i), 2))
	}
	report, err := Readiness(context.Background(), newEnv(objects...), ReadinessOptions{Backups: noBackups, MaxWorkloads: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Workloads != 1200 {
		t.Errorf("workloads = %d; want 1200", report.Summary.Workloads)
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var out []byte
	for i > 0 {
		out = append([]byte{digits[i%10]}, out...)
		i /= 10
	}
	return string(out)
}
