package checks

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/1985epma/k8scli/internal/dr"
)

func crd(name string, versions ...string) *unstructured.Unstructured {
	list := make([]any, 0, len(versions))
	for _, v := range versions {
		list = append(list, map[string]any{"name": v, "served": true})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": name}, "spec": map[string]any{"versions": list},
	}}
}

func withCRDs(env *dr.Env, crds ...runtime.Object) *dr.Env {
	env.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{crdResource: "CustomResourceDefinitionList"}, crds...)
	return env
}

func image(img string) func(*appsv1.Deployment) {
	return func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Image = img }
}

func findItem(items []DiffItem, kind, name string) (DiffItem, bool) {
	for _, i := range items {
		if i.Kind == kind && i.Name == name {
			return i, true
		}
	}
	return DiffItem{}, false
}

func namespaceDiff(t *testing.T, r *DiffReport, name string) NamespaceDiff {
	t.Helper()
	for _, n := range r.Namespaces {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("namespace %s not in the diff", name)
	return NamespaceDiff{}
}

func TestDiffOfIdenticalClustersIsEmpty(t *testing.T) {
	build := func(name string) *dr.Env {
		return withCRDs(envOf(name, namespace("shop"), deployment("shop", "api", 2), service("shop", "api", "api"),
			configMap("shop", "cfg", map[string]string{"A": "1"}), ingressTo("shop", "web", "shop.example.com", "api")), crd("widgets.example.com", "v1"))
	}
	report, err := Diff(context.Background(), build("prod"), build("dr"), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Summary.Identical || report.Summary.High+report.Summary.Medium+report.Summary.Low != 0 {
		t.Errorf("summary = %+v", report.Summary)
	}
	if report.Primary != "prod" || report.Standby != "dr" {
		t.Errorf("contexts = %s, %s", report.Primary, report.Standby)
	}
}

func TestDiffFindsWorkloadDrift(t *testing.T) {
	primary := envOf("prod", namespace("shop"),
		deployment("shop", "api", 3, image("app:2.0")),
		deployment("shop", "web", 2),
		deployment("shop", "worker", 1),
		statefulSet("shop", "db", 1),
	)
	standby := envOf("dr", namespace("shop"),
		deployment("shop", "api", 1, image("app:1.9")), // image and replicas differ
		deployment("shop", "web", 2),                   // identical
		statefulSet("shop", "db", 1),
		deployment("shop", "extra", 1), // only on the standby
		// worker is missing
	)

	report, err := Diff(context.Background(), primary, standby, DiffOptions{Namespaces: []string{"shop"}})
	if err != nil {
		t.Fatal(err)
	}
	items := namespaceDiff(t, report, "shop").Items

	if item, ok := findItem(items, "Deployment", "worker"); !ok || item.Change != DiffMissingInStandby || item.Severity != SeverityHigh {
		t.Errorf("worker = %+v ok=%v", item, ok)
	}
	var api []DiffItem
	for _, i := range items {
		if i.Name == "api" {
			api = append(api, i)
		}
	}
	if len(api) != 2 {
		t.Fatalf("api items = %+v", api)
	}
	if api[0].Severity != SeverityMedium || !strings.Contains(api[0].Detail, "container app: app:2.0 (primary) vs app:1.9 (standby)") {
		t.Errorf("image diff = %+v", api[0])
	}
	if api[1].Severity != SeverityLow || !strings.Contains(api[1].Detail, "replicas: 3 on the primary, 1 on the standby") {
		t.Errorf("replicas diff = %+v", api[1])
	}
	if item, ok := findItem(items, "Deployment", "extra"); !ok || item.Change != DiffExtraInStandby || item.Severity != SeverityLow {
		t.Errorf("extra = %+v ok=%v", item, ok)
	}
	if _, ok := findItem(items, "Deployment", "web"); ok {
		t.Error("an identical workload must not be reported")
	}
	if report.Summary.High != 1 || report.Summary.Medium != 1 || report.Summary.Low != 2 || report.Summary.Identical {
		t.Errorf("summary = %+v", report.Summary)
	}
	// Order: high first.
	if items[0].Severity != SeverityHigh {
		t.Errorf("first item = %+v; high severity comes first", items[0])
	}
}

func TestDiffMissingNamespaceShortCircuits(t *testing.T) {
	primary := envOf("prod", namespace("shop"), namespace("billing"), deployment("billing", "inv", 1))
	standby := envOf("dr", namespace("shop"))
	report, _ := Diff(context.Background(), primary, standby, DiffOptions{})
	billing := namespaceDiff(t, report, "billing")
	if !billing.MissingInStandby || len(billing.Items) != 1 || billing.Items[0].Severity != SeverityHigh {
		t.Errorf("billing = %+v; a missing namespace is one high item, not a list of its contents", billing)
	}
}

func TestDiffServices(t *testing.T) {
	lb := service("shop", "api", "api")
	lb.Spec.Type = corev1.ServiceTypeLoadBalancer
	lb.Spec.Ports = []corev1.ServicePort{{Port: 443}, {Port: 80}}
	plain := service("shop", "api", "api")
	plain.Spec.Type = corev1.ServiceTypeClusterIP
	plain.Spec.Ports = []corev1.ServicePort{{Port: 80}}

	primary := envOf("prod", namespace("shop"), lb, service("shop", "gone", "x"))
	standby := envOf("dr", namespace("shop"), plain, service("shop", "extra", "x"))

	items := namespaceDiff(t, mustDiff(t, primary, standby), "shop").Items
	api, ok := findItem(items, "Service", "api")
	if !ok || !strings.Contains(api.Detail, "type: LoadBalancer vs ClusterIP") || !strings.Contains(api.Detail, "ports: 443/TCP,80/TCP vs 80/TCP") {
		t.Errorf("api = %+v", api)
	}
	if gone, ok := findItem(items, "Service", "gone"); !ok || gone.Severity != SeverityHigh {
		t.Errorf("gone = %+v", gone)
	}
	if extra, ok := findItem(items, "Service", "extra"); !ok || extra.Change != DiffExtraInStandby {
		t.Errorf("extra = %+v", extra)
	}
}

func mustDiff(t *testing.T, primary, standby *dr.Env) *DiffReport {
	t.Helper()
	report, err := Diff(context.Background(), primary, standby, DiffOptions{Namespaces: []string{"shop"}})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestDiffConfigMapsCompareKeyNamesNeverValues(t *testing.T) {
	primary := envOf("prod", namespace("shop"),
		configMap("shop", "app", map[string]string{"A": "primary-secret-value-1", "B": "x", "SAME": "1"}),
		configMap("shop", "only-primary", map[string]string{"K": "v"}))
	standby := envOf("dr", namespace("shop"),
		configMap("shop", "app", map[string]string{"A": "a-different-value", "C": "y", "SAME": "1"}),
		configMap("shop", "only-standby", map[string]string{"K": "v"}))

	report := mustDiff(t, primary, standby)
	items := namespaceDiff(t, report, "shop").Items

	var details []string
	for _, i := range items {
		if i.Kind == "ConfigMap" && i.Name == "app" {
			details = append(details, string(i.Severity)+": "+i.Detail)
		}
	}
	if len(details) != 2 || details[0] != "medium: keys missing on the standby: B" || details[1] != "low: keys only on the standby: C" {
		t.Errorf("app details = %v; a different value for the same key is not reported (values are never compared)", details)
	}
	if item, ok := findItem(items, "ConfigMap", "only-primary"); !ok || item.Severity != SeverityMedium {
		t.Errorf("only-primary = %+v", item)
	}
	if item, ok := findItem(items, "ConfigMap", "only-standby"); !ok || item.Change != DiffExtraInStandby {
		t.Errorf("only-standby = %+v", item)
	}
	for _, i := range items {
		if strings.Contains(i.Detail, "secret-value") || strings.Contains(i.Detail, "a-different-value") {
			t.Errorf("a ConfigMap value leaked into the diff: %+v", i)
		}
	}
}

func TestDiffIngressHosts(t *testing.T) {
	primary := envOf("prod", namespace("shop"), ingressTo("shop", "web", "shop.example.com", "web"), ingressTo("shop", "admin", "admin.example.com", "web"))
	standby := envOf("dr", namespace("shop"), ingressTo("shop", "web", "shop.dr.example.com", "web"))

	items := namespaceDiff(t, mustDiff(t, primary, standby), "shop").Items
	var web []string
	for _, i := range items {
		if i.Kind == "Ingress" && i.Name == "web" {
			web = append(web, i.Detail)
		}
	}
	if len(web) != 2 || web[0] != "hosts missing on the standby: shop.example.com" || web[1] != "hosts only on the standby: shop.dr.example.com" {
		t.Errorf("web = %v", web)
	}
	if admin, ok := findItem(items, "Ingress", "admin"); !ok || admin.Change != DiffMissingInStandby {
		t.Errorf("admin = %+v", admin)
	}
}

func TestDiffCRDs(t *testing.T) {
	primary := withCRDs(envOf("prod", namespace("shop")), crd("widgets.example.com", "v1", "v1beta1"), crd("gadgets.example.com", "v1"), crd("same.example.com", "v1"))
	standby := withCRDs(envOf("dr", namespace("shop")), crd("widgets.example.com", "v1"), crd("same.example.com", "v1"), crd("extra.example.com", "v1"))

	report := mustDiff(t, primary, standby)
	if item, ok := findItem(report.Cluster, "CustomResourceDefinition", "gadgets.example.com"); !ok || item.Severity != SeverityHigh || item.Change != DiffMissingInStandby {
		t.Errorf("gadgets = %+v", item)
	}
	if item, ok := findItem(report.Cluster, "CustomResourceDefinition", "widgets.example.com"); !ok || item.Severity != SeverityMedium ||
		!strings.Contains(item.Detail, "[v1, v1beta1] (primary) vs [v1] (standby)") {
		t.Errorf("widgets = %+v", item)
	}
	if item, ok := findItem(report.Cluster, "CustomResourceDefinition", "extra.example.com"); !ok || item.Change != DiffExtraInStandby {
		t.Errorf("extra = %+v", item)
	}
	if _, ok := findItem(report.Cluster, "CustomResourceDefinition", "same.example.com"); ok {
		t.Error("identical CRDs are not reported")
	}
}

func TestDiffWithoutDynamicClientsNotes(t *testing.T) {
	report := mustDiff(t, envOf("prod", namespace("shop")), envOf("dr", namespace("shop")))
	if len(report.Cluster) != 0 || len(report.Notices) != 1 || !strings.Contains(report.Notices[0], "not compared") {
		t.Errorf("notices = %v", report.Notices)
	}
	if report.Summary.Identical {
		t.Error("a partial comparison must not claim the clusters are identical")
	}
}

func TestDiffOutputIsBounded(t *testing.T) {
	var objects []runtime.Object
	objects = append(objects, namespace("shop"))
	for i := 0; i < 20; i++ {
		objects = append(objects, deployment("shop", "w"+itoa(i), 1))
	}
	report, err := Diff(context.Background(), envOf("prod", objects...), envOf("dr", namespace("shop")), DiffOptions{Namespaces: []string{"shop"}, MaxItems: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Truncated || len(namespaceDiff(t, report, "shop").Items) != 5 {
		t.Errorf("truncated=%v items=%d", report.Truncated, len(namespaceDiff(t, report, "shop").Items))
	}
}

func TestDiffIgnoresAutomaticObjects(t *testing.T) {
	primary := envOf("prod", namespace("shop"),
		configMap("shop", "kube-root-ca.crt", map[string]string{"ca.crt": "x"}))
	standby := envOf("dr", namespace("shop"))
	if items := namespaceDiff(t, mustDiff(t, primary, standby), "shop").Items; len(items) != 0 {
		t.Errorf("kube-root-ca.crt exists everywhere: %+v", items)
	}
}

func TestDiffNeedsBothClients(t *testing.T) {
	ok := envOf("x")
	if _, err := Diff(context.Background(), &dr.Env{}, ok, DiffOptions{}); err == nil {
		t.Error("expected an error without a primary client")
	}
	if _, err := Diff(context.Background(), ok, &dr.Env{}, DiffOptions{}); err == nil {
		t.Error("expected an error without a standby client")
	}
}

func TestDiffIsDeterministic(t *testing.T) {
	build := func() *DiffReport {
		return mustDiff(t,
			envOf("prod", namespace("shop"), deployment("shop", "a", 2, image("x:1")), deployment("shop", "b", 1), service("shop", "s", "a")),
			envOf("dr", namespace("shop"), deployment("shop", "a", 1, image("x:2")), deployment("shop", "c", 1)))
	}
	first := build()
	for i := 0; i < 5; i++ {
		got := build()
		if len(got.Namespaces[0].Items) != len(first.Namespaces[0].Items) {
			t.Fatal("different item counts")
		}
		for j := range got.Namespaces[0].Items {
			if got.Namespaces[0].Items[j] != first.Namespaces[0].Items[j] {
				t.Fatalf("run %d item %d differs", i, j)
			}
		}
	}
}
