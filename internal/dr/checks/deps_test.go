package checks

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/1985epma/k8scli/internal/dr"
)

func service(ns, name, app string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": app}},
	}
}

func withEnv(env ...corev1.EnvVar) func(*appsv1.Deployment) {
	return func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Env = env }
}

func configMap(ns, name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: data}
}

func ingressTo(ns, name, host, svc string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: svc}}}},
			}},
		}}},
	}
}

func threeTier() []runtime.Object {
	return []runtime.Object{
		namespace("shop"),
		// data tier
		statefulSet("shop", "postgres", 1), service("shop", "postgres", "postgres"),
		deployment("shop", "redis", 1), service("shop", "redis", "redis"),
		// application tier
		deployment("shop", "api", 2, withEnv(
			corev1.EnvVar{Name: "DATABASE_URL", Value: "postgres://app:s3cret-password@postgres:5432/shop"},
			corev1.EnvVar{Name: "CACHE_HOST", Value: "redis.shop.svc.cluster.local"},
			corev1.EnvVar{Name: "LOG_LEVEL", Value: "info"},
		)), service("shop", "api", "api"),
		// edge tier
		deployment("shop", "web", 2, func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "API_URL", Value: "http://api.shop.svc:8080"}}
		}), service("shop", "web", "web"),
		ingressTo("shop", "public", "shop.example.com", "web"),
		// background worker: database through an argument, api through a ConfigMap
		deployment("shop", "worker", 1, func(d *appsv1.Deployment) {
			c := &d.Spec.Template.Spec.Containers[0]
			c.Args = []string{"--db=postgres.shop", "--mode=queue"}
			c.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "worker-config"}}}}
		}),
		configMap("shop", "worker-config", map[string]string{"BACKEND": "http://api:8080/v1", "COLOR": "blue"}),
		// nothing detected
		deployment("shop", "orphan", 1),
	}
}

func nodeByName(t *testing.T, g *Graph, key string) DepNode {
	t.Helper()
	for _, n := range g.Nodes {
		if n.Key == key {
			return n
		}
	}
	t.Fatalf("node %s not in the graph: %+v", key, g.Nodes)
	return DepNode{}
}

func hasEdge(g *Graph, from, to string) (DepEdge, bool) {
	for _, e := range g.Edges {
		if e.From == from && e.To == to {
			return e, true
		}
	}
	return DepEdge{}, false
}

func TestDiscoverThreeTierApplication(t *testing.T) {
	g, err := DiscoverDependencies(context.Background(), newEnv(threeTier()...), DiscoverOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, edge := range []struct{ from, to, via string }{
		{"shop/Deployment/api", "shop/StatefulSet/postgres", "env DATABASE_URL"},
		{"shop/Deployment/api", "shop/Deployment/redis", "env CACHE_HOST"},
		{"shop/Deployment/web", "shop/Deployment/api", "env API_URL"},
		{"shop/Deployment/worker", "shop/StatefulSet/postgres", "args of app"},
		{"shop/Deployment/worker", "shop/Deployment/api", "configmap worker-config/BACKEND"},
	} {
		e, ok := hasEdge(g, edge.from, edge.to)
		if !ok {
			t.Errorf("missing edge %s -> %s", edge.from, edge.to)
			continue
		}
		if !strings.Contains(e.Via, edge.via) {
			t.Errorf("edge %s -> %s via %q; want it to mention %q", edge.from, edge.to, e.Via, edge.via)
		}
	}
	if len(g.Edges) != 5 {
		t.Errorf("edges = %d; want 5 (no false positives): %+v", len(g.Edges), g.Edges)
	}

	// Tiers: data stores first, then api, then its clients.
	for key, want := range map[string]int{
		"shop/StatefulSet/postgres": 0,
		"shop/Deployment/redis":     0,
		"shop/Deployment/api":       1,
		"shop/Deployment/web":       2,
		"shop/Deployment/worker":    2,
		"shop/Deployment/orphan":    1, // nothing detected: after the data stores
	} {
		if got := nodeByName(t, g, key).Tier; got != want {
			t.Errorf("tier of %s = %d; want %d", key, got, want)
		}
	}

	if !nodeByName(t, g, "shop/StatefulSet/postgres").Stateful || nodeByName(t, g, "shop/Deployment/api").Stateful {
		t.Error("only the StatefulSet is stateful")
	}
	web := nodeByName(t, g, "shop/Deployment/web")
	if !web.EntryPoint || len(web.Hosts) != 1 || web.Hosts[0] != "shop.example.com" {
		t.Errorf("web = %+v; the Ingress makes it an entry point", web)
	}
	if nodeByName(t, g, "shop/Deployment/api").EntryPoint {
		t.Error("api is not behind an Ingress")
	}
}

func TestDiscoverNeverLeaksTheScannedText(t *testing.T) {
	g, _ := DiscoverDependencies(context.Background(), newEnv(threeTier()...), DiscoverOptions{})
	raw, _ := yaml.Marshal(g)
	draft, _ := g.DraftYAML()
	for _, leaked := range []string{"s3cret-password", "postgres://app", "http://api:8080/v1"} {
		if strings.Contains(string(raw), leaked) || strings.Contains(string(draft), leaked) {
			t.Errorf("the output contains %q: only variable and key names may appear", leaked)
		}
	}
}

func TestDiscoverIgnoresVariableNamesAndUnknownWords(t *testing.T) {
	objects := []runtime.Object{
		namespace("shop"),
		deployment("shop", "db", 1), service("shop", "db", "db"),
		// the variable NAME mentions db, the value does not; "user" is not a service.
		deployment("shop", "app", 1, withEnv(corev1.EnvVar{Name: "DB_HOST_NOTE", Value: "user"})),
	}
	g, _ := DiscoverDependencies(context.Background(), newEnv(objects...), DiscoverOptions{})
	if len(g.Edges) != 0 {
		t.Errorf("edges = %+v; names of variables must not count", g.Edges)
	}
}

func TestDiscoverBareNamesOnlyMatchWithinTheNamespace(t *testing.T) {
	objects := []runtime.Object{
		namespace("a"), namespace("b"),
		deployment("a", "db", 1), service("a", "db", "db"),
		deployment("b", "app", 1, withEnv(corev1.EnvVar{Name: "HOST", Value: "db"})),             // "db" in namespace b: no such service
		deployment("b", "app2", 1, withEnv(corev1.EnvVar{Name: "HOST", Value: "db.a.svc"})),      // explicit namespace
		deployment("b", "app3", 1, withEnv(corev1.EnvVar{Name: "HOST", Value: "db.a"})),          // name.namespace
		deployment("b", "app4", 1, withEnv(corev1.EnvVar{Name: "HOST", Value: "db.a.svc.corp"})), // other cluster domain
	}
	g, _ := DiscoverDependencies(context.Background(), newEnv(objects...), DiscoverOptions{})
	if _, ok := hasEdge(g, "b/Deployment/app", "a/Deployment/db"); ok {
		t.Error("a bare name must not cross namespaces")
	}
	for _, app := range []string{"app2", "app3", "app4"} {
		if _, ok := hasEdge(g, "b/Deployment/"+app, "a/Deployment/db"); !ok {
			t.Errorf("%s should depend on a/db", app)
		}
	}
}

func TestDiscoverFromNetworkPolicies(t *testing.T) {
	selector := func(app string) metav1.LabelSelector {
		return metav1.LabelSelector{MatchLabels: map[string]string{"app": app}}
	}
	clientPeer := selector("client")
	objects := []runtime.Object{
		namespace("shop"),
		deployment("shop", "client", 1), deployment("shop", "db", 1), deployment("shop", "cache", 1),
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "db-from-client", Namespace: "shop"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: selector("db"),
				Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &clientPeer}}}},
			},
		},
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "client-to-cache", Namespace: "shop"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: selector("client"),
				Egress:      []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{PodSelector: ptr(selector("cache"))}}}},
			},
		},
		// "allow everything in the namespace" and a namespace selector say nothing about a dependency.
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "noisy", Namespace: "shop"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: selector("db"),
				Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{
					{PodSelector: &metav1.LabelSelector{}}, {PodSelector: &clientPeer, NamespaceSelector: &metav1.LabelSelector{}},
				}}},
			},
		},
	}
	g, _ := DiscoverDependencies(context.Background(), newEnv(objects...), DiscoverOptions{})
	if e, ok := hasEdge(g, "shop/Deployment/client", "shop/Deployment/db"); !ok || e.Via != "networkpolicy db-from-client" {
		t.Errorf("ingress peer edge = %+v ok=%v", e, ok)
	}
	if e, ok := hasEdge(g, "shop/Deployment/client", "shop/Deployment/cache"); !ok || e.Via != "networkpolicy client-to-cache" {
		t.Errorf("egress peer edge = %+v ok=%v", e, ok)
	}
	if len(g.Edges) != 2 {
		t.Errorf("edges = %+v; the noisy policy adds nothing", g.Edges)
	}
}

func TestDiscoverDetectsAndBreaksCycles(t *testing.T) {
	objects := []runtime.Object{
		namespace("shop"),
		deployment("shop", "a", 1, withEnv(corev1.EnvVar{Name: "X", Value: "b"})), service("shop", "a", "a"),
		deployment("shop", "b", 1, withEnv(corev1.EnvVar{Name: "X", Value: "a"})), service("shop", "b", "b"),
		deployment("shop", "c", 1, withEnv(corev1.EnvVar{Name: "X", Value: "a"})),
	}
	g, _ := DiscoverDependencies(context.Background(), newEnv(objects...), DiscoverOptions{})
	if len(g.Cycles) != 1 {
		t.Fatalf("cycles = %v", g.Cycles)
	}
	cyclic := 0
	for _, e := range g.Edges {
		if e.Cycle {
			cyclic++
		}
	}
	if cyclic != 1 {
		t.Errorf("%d cycle-closing edges; want 1", cyclic)
	}
	// The order still exists: c depends on a, so c starts after a.
	if nodeByName(t, g, "shop/Deployment/c").Tier <= nodeByName(t, g, "shop/Deployment/a").Tier {
		t.Errorf("tiers: %+v", g.Nodes)
	}
	if len(g.Notices) == 0 {
		t.Error("a cycle must produce a notice")
	}
}

func TestPVCMountMakesAWorkloadStateful(t *testing.T) {
	objects := []runtime.Object{namespace("shop"), deployment("shop", "files", 1, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}
	})}
	g, _ := DiscoverDependencies(context.Background(), newEnv(objects...), DiscoverOptions{})
	n := nodeByName(t, g, "shop/Deployment/files")
	if !n.Stateful || n.Tier != 0 {
		t.Errorf("node = %+v; a stateful workload with no dependencies starts first", n)
	}
}

func TestDraftApps(t *testing.T) {
	g, _ := DiscoverDependencies(context.Background(), newEnv(threeTier()...), DiscoverOptions{})
	apps := g.DraftApps()

	byName := map[string]dr.App{}
	for _, a := range apps {
		byName[a.Name] = a
	}
	if len(apps) != 6 {
		t.Fatalf("apps = %d", len(apps))
	}
	if apps[0].Tier != 0 || apps[len(apps)-1].Tier != 2 {
		t.Errorf("apps are ordered by tier: first %d last %d", apps[0].Tier, apps[len(apps)-1].Tier)
	}
	worker := byName["worker"]
	if worker.Namespace != "shop" || worker.Tier != 2 || len(worker.Workloads) != 1 || worker.Workloads[0] != "worker" {
		t.Errorf("worker = %+v", worker)
	}
	if got := strings.Join(worker.DependsOn, ","); got != "api,postgres" {
		t.Errorf("worker dependsOn = %q; want api,postgres (sorted)", got)
	}
	if len(byName["postgres"].DependsOn) != 0 {
		t.Errorf("postgres dependsOn = %v", byName["postgres"].DependsOn)
	}
}

func TestDraftYAMLRoundTrips(t *testing.T) {
	g, _ := DiscoverDependencies(context.Background(), newEnv(threeTier()...), DiscoverOptions{})
	raw, err := g.DraftYAML()
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, "# Draft generated") || !strings.Contains(text, "shop/postgres") {
		t.Errorf("header missing or the stateful workload is not listed:\n%s", text)
	}
	var parsed struct {
		Apps []dr.App `json:"apps"`
	}
	if err := yaml.UnmarshalStrict(raw, &parsed); err != nil {
		t.Fatalf("the draft must be valid YAML for the plan types: %v\n%s", err, text)
	}
	if len(parsed.Apps) != 6 {
		t.Errorf("apps = %d", len(parsed.Apps))
	}
}

func TestDraftNamesStayUniqueAcrossNamespaces(t *testing.T) {
	objects := []runtime.Object{namespace("a"), namespace("b"), deployment("a", "api", 1), deployment("b", "api", 1), deployment("b", "web", 1)}
	g, _ := DiscoverDependencies(context.Background(), newEnv(objects...), DiscoverOptions{})
	var names []string
	for _, a := range g.DraftApps() {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "a-api,b-api,web" {
		t.Errorf("names = %v; colliding names get the namespace as a prefix", names)
	}
}

func TestDiscoverIsDeterministic(t *testing.T) {
	var first string
	for i := 0; i < 5; i++ {
		g, _ := DiscoverDependencies(context.Background(), newEnv(threeTier()...), DiscoverOptions{})
		raw, _ := g.DraftYAML()
		edges, _ := yaml.Marshal(g)
		got := string(raw) + string(edges)
		if first == "" {
			first = got
		} else if got != first {
			t.Fatalf("run %d differs", i)
		}
	}
}

func TestDiscoverNamespaceSelectionAndClient(t *testing.T) {
	if _, err := DiscoverDependencies(context.Background(), &dr.Env{}, DiscoverOptions{}); err == nil {
		t.Error("expected an error without a client")
	}
	objects := append(threeTier(), namespace("other"), deployment("other", "x", 1))
	g, _ := DiscoverDependencies(context.Background(), newEnv(objects...), DiscoverOptions{Namespaces: []string{"other"}})
	if len(g.Nodes) != 1 || g.Nodes[0].Key != "other/Deployment/x" {
		t.Errorf("nodes = %+v", g.Nodes)
	}
}
