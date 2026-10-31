package checks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

func rpoPlan() *dr.Plan {
	return &dr.Plan{
		Name: "p", Context: "prod",
		Targets: dr.Targets{RPO: "24h"},
		Apps: []dr.App{
			{Name: "web", Namespace: "shop"},                 // default 24h
			{Name: "api", Namespace: "shop", RPO: "1h"},      // own target
			{Name: "batch", Namespace: "jobs", RPO: "bogus"}, // invalid: falls back to the default
			{Name: "lonely", Namespace: "empty"},
		},
		Databases: []dr.Database{{Name: "pg", Namespace: "shop", Kind: "cnpg", Ref: "pg", RPO: "15m"}},
	}
}

func TestEstimateBackupRPO(t *testing.T) {
	calls := map[string]int{}
	finder := func(_ context.Context, _ *dr.Env, namespace string) (time.Time, bool, error) {
		calls[namespace]++
		switch namespace {
		case "shop":
			return fixedNow.Add(-2 * time.Hour), true, nil
		case "jobs":
			return fixedNow.Add(-30 * time.Hour), true, nil
		}
		return time.Time{}, false, nil
	}

	got, err := EstimateBackupRPOWith(context.Background(), newEnv(), rpoPlan(), finder)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]RPOEstimate{}
	for _, e := range got {
		by[e.Kind+"/"+e.Name] = e
	}

	if e := by["app/web"]; e.Status != RPOOK || e.Age != 2*time.Hour || e.Target != 24*time.Hour || !e.Found {
		t.Errorf("web = %+v", e)
	}
	if e := by["app/api"]; e.Status != RPOBreach || e.Target != time.Hour || !strings.Contains(e.Note, "2 hours old") {
		t.Errorf("api = %+v", e)
	}
	if e := by["database/pg"]; e.Status != RPOBreach || e.Target != 15*time.Minute {
		t.Errorf("pg = %+v", e)
	}
	if e := by["app/batch"]; e.Status != RPOBreach || e.Target != 24*time.Hour || !strings.Contains(e.Note, `"bogus"`) {
		t.Errorf("batch = %+v; an invalid target is reported and the default is used", e)
	}
	if e := by["app/lonely"]; e.Status != RPONoBackup || e.Found {
		t.Errorf("lonely = %+v", e)
	}
	if calls["shop"] != 1 {
		t.Errorf("the finder was called %d times for one namespace; want 1 (cached)", calls["shop"])
	}
	// Sorted: apps before databases (by kind), then by name.
	if got[0].Kind != "app" || got[0].Name != "api" || got[len(got)-1].Kind != "database" {
		t.Errorf("order = %+v", got)
	}
}

func TestEstimateWithoutATargetIsUnknownButShowsTheAge(t *testing.T) {
	plan := &dr.Plan{Apps: []dr.App{{Name: "web", Namespace: "shop"}}}
	got, _ := EstimateBackupRPOWith(context.Background(), newEnv(), plan, backupAt(fixedNow.Add(-3*time.Hour)))
	if got[0].Status != RPOUnknown || got[0].Age != 3*time.Hour || !strings.Contains(got[0].Note, "No RPO target") {
		t.Errorf("estimate = %+v", got[0])
	}
}

func TestEstimateFinderErrorIsReportedPerEntry(t *testing.T) {
	finder := func(context.Context, *dr.Env, string) (time.Time, bool, error) {
		return time.Time{}, false, errors.New("forbidden")
	}
	got, err := EstimateBackupRPOWith(context.Background(), newEnv(), rpoPlan(), finder)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		if e.Status != RPOUnknown || !strings.Contains(e.Note, "forbidden") {
			t.Errorf("%s = %+v", e.Name, e)
		}
	}
}

func TestEstimateDefaultFinderNeedsTheDynamicClient(t *testing.T) {
	env := newEnv()
	env.Dynamic = nil
	got, err := EstimateBackupRPO(context.Background(), env, rpoPlan())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		if e.Status != RPOUnknown || !strings.Contains(e.Note, "dynamic client") {
			t.Errorf("%s = %+v", e.Name, e)
		}
	}
	if _, err := EstimateBackupRPO(context.Background(), nil, rpoPlan()); err == nil {
		t.Error("expected an error without an environment")
	}
	if _, err := EstimateBackupRPO(context.Background(), env, nil); err == nil {
		t.Error("expected an error without a plan")
	}
}

func TestEstimateWithTheRealBackupFinder(t *testing.T) {
	gvr := func(group, version, resource string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
	}
	backup := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": "nightly", "namespace": "velero"},
		"spec":     map[string]any{"includedNamespaces": []any{"shop"}},
		"status":   map[string]any{"phase": "Completed", "completionTimestamp": fixedNow.Add(-5 * time.Hour).Format(time.RFC3339)},
	}}
	env := newEnv()
	env.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gvr("velero.io", "v1", "backups"):                       "BackupList",
		gvr("snapshot.storage.k8s.io", "v1", "volumesnapshots"): "VolumeSnapshotList",
	}, backup)

	plan := &dr.Plan{Targets: dr.Targets{RPO: "12h"}, Apps: []dr.App{{Name: "web", Namespace: "shop"}, {Name: "other", Namespace: "billing"}}}
	got, err := EstimateBackupRPO(context.Background(), env, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Name != "other" || got[0].Status != RPONoBackup {
		t.Errorf("other = %+v", got[0])
	}
	if got[1].Name != "web" || got[1].Status != RPOOK || got[1].Age != 5*time.Hour {
		t.Errorf("web = %+v; the Velero backup of 5h is within the 12h target", got[1])
	}
}

// ---- pagination and read-only guarantees -----------------------------------------------------------------------

func TestPageAllFollowsContinueTokens(t *testing.T) {
	pages := map[string]struct {
		items []string
		next  string
	}{"": {[]string{"a", "b"}, "t1"}, "t1": {[]string{"c"}, "t2"}, "t2": {[]string{"d", "e"}, ""}}
	var tokens []string
	got, truncated, err := pageAll(100, func(o metav1.ListOptions) ([]string, string, error) {
		tokens = append(tokens, o.Continue)
		if o.Limit != pageSize {
			t.Errorf("limit = %d; want %d", o.Limit, pageSize)
		}
		p := pages[o.Continue]
		return p.items, p.next, nil
	})
	if err != nil || truncated || strings.Join(got, "") != "abcde" || strings.Join(tokens, ",") != ",t1,t2" {
		t.Errorf("got %v truncated=%v err=%v tokens=%v", got, truncated, err, tokens)
	}

	capped, truncated, _ := pageAll(3, func(o metav1.ListOptions) ([]string, string, error) {
		p := pages[o.Continue]
		return p.items, p.next, nil
	})
	if !truncated || strings.Join(capped, "") != "abc" {
		t.Errorf("capped = %v truncated=%v", capped, truncated)
	}

	if _, _, err := pageAll(3, func(metav1.ListOptions) ([]string, string, error) { return nil, "", errors.New("boom") }); err == nil {
		t.Error("an error must be returned")
	}
}

// TestEverythingInThisPackageIsReadOnly runs every analysis against a fake clientset and checks that no
// write verb was ever issued.
func TestEverythingInThisPackageIsReadOnly(t *testing.T) {
	client := fake.NewSimpleClientset(append(exportFixture(), twoNodes()...)...)
	tiers := fake.NewSimpleClientset(threeTier()...)
	standby := fake.NewSimpleClientset(namespace("shop"), readyNode("n1", "8", "16Gi"))
	ctx := context.Background()

	dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		snapshotClasses: "VolumeSnapshotClassList", crdResource: "CustomResourceDefinitionList",
	})
	env := &dr.Env{Context: "prod", Client: client, Dynamic: dynamic, Now: func() time.Time { return fixedNow }}
	other := &dr.Env{Context: "dr", Client: standby, Dynamic: dynamic, Now: func() time.Time { return fixedNow }}

	if _, err := Readiness(ctx, env, ReadinessOptions{Backups: noBackups}); err != nil {
		t.Fatal(err)
	}
	if _, err := CapacityFit(ctx, env, other, CapacityOptions{Namespaces: []string{"shop"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverDependencies(ctx, &dr.Env{Context: "prod", Client: tiers}, DiscoverOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Diff(ctx, env, other, DiffOptions{Namespaces: []string{"shop"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportZip(ctx, env, "shop", &strings.Builder{}, ExportOptions{IncludeSecrets: true}); err != nil {
		t.Fatal(err)
	}
	assertReadOnly(t, client)
	assertReadOnly(t, tiers)
	assertReadOnly(t, standby)
}

func TestDynamicClientIsOnlyListed(t *testing.T) {
	env := newEnv(with(twoNodes(), namespace("shop"), storageClass("fast", "x", false), pvc("shop", "a", ptr("fast")))...)
	dyn, ok := env.Dynamic.(interface{ Actions() []k8stesting.Action })
	if !ok {
		t.Skip("the fake dynamic client does not expose its actions")
	}
	if _, err := Readiness(context.Background(), env, ReadinessOptions{Backups: backupAt(fixedNow)}); err != nil {
		t.Fatal(err)
	}
	if len(dyn.Actions()) == 0 {
		t.Fatal("the storage check should have listed snapshot classes")
	}
	for _, a := range dyn.Actions() {
		if a.GetVerb() != "list" && a.GetVerb() != "get" {
			t.Errorf("dynamic %s on %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}
