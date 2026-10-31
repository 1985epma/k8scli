package chaos

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/k8sops"
)

type fakeHTTP struct {
	status int
	err    error
	urls   []string
}

func (f *fakeHTTP) Check(_ context.Context, url string) (int, error) {
	f.urls = append(f.urls, url)
	return f.status, f.err
}

func TestPodsReadyProbe(t *testing.T) {
	broken := deployment("shop", "web", 3)
	broken.Status.ReadyReplicas = 1
	te := newTestEnv(t, deployment("shop", "api", 2), broken, statefulSet("shop", "redis", 2))
	refs := func(names ...string) []k8sops.WorkloadRef {
		var out []k8sops.WorkloadRef
		for _, n := range names {
			kind := "Deployment"
			if n == "redis" {
				kind = "StatefulSet"
			}
			out = append(out, k8sops.WorkloadRef{Kind: kind, Namespace: "shop", Name: n})
		}
		return out
	}

	ok, detail, err := PodsReadyProbe(te.env, "healthy", refs("api", "redis")).Check(context.Background())
	if err != nil || !ok || detail != "" {
		t.Errorf("healthy probe: %v %q %v", ok, detail, err)
	}
	ok, detail, err = PodsReadyProbe(te.env, "broken", refs("api", "web")).Check(context.Background())
	if err != nil || ok || !strings.Contains(detail, "shop/web 1/3 ready") || strings.Contains(detail, "api") {
		t.Errorf("broken probe: %v %q %v", ok, detail, err)
	}
	if _, _, err := PodsReadyProbe(te.env, "x", refs("ghost")).Check(context.Background()); err == nil {
		t.Error("an unknown workload is an error, not a healthy answer")
	}
	if _, _, err := PodsReadyProbe(&dr.Env{}, "x", refs("api")).Check(context.Background()); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("no client: %v", err)
	}
	if got := PodsReadyProbe(te.env, "named", nil).Name(); got != "named" {
		t.Errorf("Name() = %q", got)
	}
}

func TestHTTPProbe(t *testing.T) {
	te := newTestEnv(t)
	h := &fakeHTTP{status: 200}
	te.env.HTTP = h
	ok, _, err := HTTPProbe(te.env, "web", "http://localhost/healthz", 0).Check(context.Background())
	if err != nil || !ok {
		t.Errorf("200 by default: %v %v", ok, err)
	}
	h.status = 503
	ok, detail, _ := HTTPProbe(te.env, "web", "http://localhost/healthz", 204).Check(context.Background())
	if ok || !strings.Contains(detail, "answered 503, want 204") {
		t.Errorf("wrong status: %v %q", ok, detail)
	}
	h.err = errors.New("connection refused")
	ok, detail, err = HTTPProbe(te.env, "web", "http://localhost/healthz", 200).Check(context.Background())
	if ok || err != nil || !strings.Contains(detail, "connection refused") {
		t.Errorf("a request error is unhealthy, not fatal: %v %q %v", ok, detail, err)
	}
	te.env.HTTP = nil
	if _, _, err := HTTPProbe(te.env, "web", "http://x", 200).Check(context.Background()); err == nil {
		t.Error("no checker is an error")
	}
}

func planWithApps() *dr.Plan {
	return &dr.Plan{
		Version: 1, Name: "lab", Context: testContext, Safety: dr.Safety{LabOnly: true},
		Apps: []dr.App{
			{Name: "api", Namespace: "shop", Tier: 1},
			{Name: "web", Namespace: "shop", Tier: 2, Health: &dr.Health{Kind: "http", URL: "http://localhost:8080/", Status: 200}},
			{Name: "named", Namespace: "other", Workloads: []string{"worker", "queue"}},
		},
		Databases: []dr.Database{{Name: "db", Namespace: "shop", Kind: "statefulset", Ref: "legacy-db"}},
	}
}

func TestProbesFromPlan(t *testing.T) {
	objects := []runtime.Object{
		deployment("shop", "api", 2), deployment("shop", "web", 2), statefulSet("shop", "legacy-db", 2),
		deployment("other", "worker", 1), statefulSet("other", "queue", 1),
	}
	te := newTestEnv(t, objects...)
	te.env.HTTP = &fakeHTTP{status: 200}
	ctx := context.Background()

	probes, err := ProbesFromPlan(ctx, te.env, planWithApps(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range probes {
		names = append(names, p.Name())
	}
	if got := strings.Join(names, ","); got != "app api,app web,app named" {
		t.Errorf("probes = %s", got)
	}
	// The api probe covers its namespace's workloads except the plan's database.
	ok, _, err := probes[0].Check(ctx)
	if err != nil || !ok {
		t.Errorf("api probe: %v %v", ok, err)
	}
	if err := te.client.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("statefulsets"), func() *appsv1.StatefulSet {
		s := statefulSet("shop", "legacy-db", 2)
		s.Status.ReadyReplicas = 0
		return s
	}(), "shop"); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := probes[0].Check(ctx); !ok {
		t.Error("a broken database is not part of the app probe")
	}

	// Filters: by namespace and by app name.
	probes, err = ProbesFromPlan(ctx, te.env, planWithApps(), "other", nil)
	if err != nil || len(probes) != 1 || probes[0].Name() != "app named" {
		t.Errorf("namespace filter: %v %v", probes, err)
	}
	probes, err = ProbesFromPlan(ctx, te.env, planWithApps(), "", []string{"web"})
	if err != nil || len(probes) != 1 || probes[0].Name() != "app web" {
		t.Errorf("name filter: %v %v", probes, err)
	}

	// A named workload that does not exist is an error, not a silently empty probe.
	bad := planWithApps()
	bad.Apps[2].Workloads = []string{"ghost"}
	if _, err := ProbesFromPlan(ctx, te.env, bad, "other", nil); err == nil {
		t.Error("unknown named workload")
	}
	if _, err := ProbesFromPlan(ctx, nil, planWithApps(), "", nil); err == nil {
		t.Error("nil environment")
	}
}
