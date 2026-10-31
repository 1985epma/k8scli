package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// dryRunSpy makes the fake clientset behave like a real API server for dry
// runs: the request is accepted but nothing is stored. It counts the requests
// that carried DryRun, so tests can prove the console really asked for one.
type dryRunSpy struct{ creates, patches int }

func spyOnDryRuns(e *env) *dryRunSpy {
	spy := &dryRunSpy{}
	e.client.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		switch a := action.(type) {
		case k8stesting.CreateActionImpl:
			if len(a.CreateOptions.DryRun) > 0 {
				spy.creates++
				return true, a.GetObject(), nil
			}
		case k8stesting.PatchActionImpl:
			if len(a.PatchOptions.DryRun) > 0 {
				spy.patches++
				return true, &corev1.ConfigMap{}, nil
			}
		}
		return false, nil, nil
	})
	return spy
}

func namespaceExists(e *env, name string) bool {
	_, err := e.client.CoreV1().Namespaces().Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

func TestNamespacesList(t *testing.T) {
	terminating := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "old", CreationTimestamp: metav1.NewTime(fixedNow.Add(-48 * 60 * 60 * 1e9))},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
	}
	e := newEnv(t, Options{}, terminating,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "empty"}},
		crashingPod("shop", "web-1"), podWithStatus("shop", "web-2", corev1.PodRunning, 0),
		deployment("shop", "web", 2, 2))

	rows := decode[[]namespaceRow](t, e.do(http.MethodGet, "/api/namespaces", nil))
	if len(rows) != 3 || rows[0].Name != "empty" || rows[1].Name != "old" || rows[2].Name != "shop" {
		t.Fatalf("rows (sorted by name) = %+v", rows)
	}
	if rows[0].Status != "Active" || rows[0].Pods != 0 {
		t.Errorf("empty = %+v (status defaults to Active)", rows[0])
	}
	if rows[1].Status != "Terminating" || rows[1].Age != "2d" {
		t.Errorf("old = %+v", rows[1])
	}
	if rows[2].Pods != 2 || rows[2].Running != 1 || rows[2].Attention != 1 || rows[2].Deployments != 1 {
		t.Errorf("shop = %+v", rows[2])
	}
}

func TestNamespacesListErrors(t *testing.T) {
	e := newEnv(t, Options{})
	e.client.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("namespaces is forbidden")
	})
	if rec := e.do(http.MethodGet, "/api/namespaces", nil); rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d", rec.Code)
	}

	// An empty cluster is [] and not null.
	if body := newEnv(t, Options{}).do(http.MethodGet, "/api/namespaces", nil).Body.String(); strings.TrimSpace(body) != "[]" {
		t.Errorf("body = %q", body)
	}
}

func TestNamespaceCreate(t *testing.T) {
	e := newEnv(t, Options{})

	rec := e.do(http.MethodPost, "/api/namespaces", map[string]any{"name": "team-b", "labels": map[string]string{"team": "payments"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := decode[changeResponse](t, rec)
	if got.Kind != "Namespace" || got.Name != "team-b" || got.Action != "created" || got.DryRun || got.Summary != "Namespace team-b created" {
		t.Errorf("response = %+v", got)
	}
	if got.Equivalent != "kubectl create namespace team-b && kubectl label namespace team-b team=payments" {
		t.Errorf("equivalent = %q", got.Equivalent)
	}
	if len(got.Changes) != 3 || got.Changes[1] != "label app.kubernetes.io/managed-by=k8scli" || got.Changes[2] != "label team=payments" {
		t.Errorf("changes = %v", got.Changes)
	}

	stored, err := e.client.CoreV1().Namespaces().Get(context.Background(), "team-b", metav1.GetOptions{})
	if err != nil || stored.Labels[managedByLabel] != managedByValue || stored.Labels["team"] != "payments" {
		t.Errorf("stored = %+v, %v", stored, err)
	}
}

func TestNamespaceCreateDryRunStoresNothing(t *testing.T) {
	e := newEnv(t, Options{})
	spy := spyOnDryRuns(e)

	rec := e.do(http.MethodPost, "/api/namespaces", map[string]any{"name": "team-b", "dryRun": true})
	got := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || !got.DryRun || got.Summary != "Namespace team-b would be created" {
		t.Errorf("status %d: %+v", rec.Code, got)
	}
	if spy.creates != 1 || namespaceExists(e, "team-b") {
		t.Errorf("the dry run must reach the API with DryRun set and store nothing (creates=%d)", spy.creates)
	}
}

func TestNamespaceCreateAlreadyExists(t *testing.T) {
	e := newEnv(t, Options{}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}})
	rec := e.do(http.MethodPost, "/api/namespaces", map[string]any{"name": "shop"})
	if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "already_exists" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestNamespaceCreateReadOnly(t *testing.T) {
	e := newEnv(t, Options{ReadOnly: true})
	spy := spyOnDryRuns(e)

	rec := e.do(http.MethodPost, "/api/namespaces", map[string]any{"name": "team-b"})
	if rec.Code != http.StatusForbidden || decode[errorResponse](t, rec).Code != "read_only" || namespaceExists(e, "team-b") {
		t.Errorf("real change in read-only mode: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodPost, "/api/namespaces", map[string]any{"name": "team-b", "dryRun": true}); rec.Code != http.StatusOK || spy.creates != 1 {
		t.Errorf("a dry run is allowed in read-only mode: %d", rec.Code)
	}
}

func TestNamespaceCreateInProductionNeedsTheContextName(t *testing.T) {
	e := newEnv(t, Options{})
	spyOnDryRuns(e)
	body := map[string]any{"name": "team-b", "context": "prod-eu"}

	rec := e.do(http.MethodPost, "/api/namespaces", body)
	if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "confirmation_required" || namespaceExists(e, "team-b") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body["confirm"] = "dev"
	if rec := e.do(http.MethodPost, "/api/namespaces", body); rec.Code != http.StatusConflict {
		t.Errorf("a wrong confirmation = %d; want 409", rec.Code)
	}

	body["dryRun"] = true
	body["confirm"] = ""
	if rec := e.do(http.MethodPost, "/api/namespaces", body); rec.Code != http.StatusOK {
		t.Errorf("a dry run needs no confirmation: %d", rec.Code)
	}

	body["dryRun"] = false
	body["confirm"] = "prod-eu"
	if rec := e.do(http.MethodPost, "/api/namespaces", body); rec.Code != http.StatusOK || !namespaceExists(e, "team-b") {
		t.Errorf("confirmed create failed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestNamespaceCreateValidation(t *testing.T) {
	e := newEnv(t, Options{})
	many := map[string]string{}
	for i := 0; i < nsMaxLabels+1; i++ {
		many["k"+strings.Repeat("x", i)] = "v"
	}

	for name, body := range map[string]any{
		"missing name":      map[string]any{},
		"upper case":        map[string]any{"name": "Team"},
		"underscore":        map[string]any{"name": "team_b"},
		"too long":          map[string]any{"name": strings.Repeat("a", 64)},
		"path":              map[string]any{"name": "../x"},
		"reserved prefix":   map[string]any{"name": "kube-mine"},
		"too many labels":   map[string]any{"name": "ok", "labels": many},
		"bad label key":     map[string]any{"name": "ok", "labels": map[string]string{"bad key": "v"}},
		"bad label value":   map[string]any{"name": "ok", "labels": map[string]string{"k": "has space"}},
		"managed-by label":  map[string]any{"name": "ok", "labels": map[string]string{managedByLabel: "me"}},
		"unknown field":     map[string]any{"name": "ok", "extra": 1},
		"shell metachar":    map[string]any{"name": "ok", "labels": map[string]string{"k": "a;rm -rf"}},
		"label key is slug": map[string]any{"name": "ok", "labels": map[string]string{"-bad": "v"}},
	} {
		if rec := e.do(http.MethodPost, "/api/namespaces", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d; want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
	if namespaceExists(e, "ok") {
		t.Error("an invalid request created something")
	}
}

func TestNamespaceCreateUnknownContext(t *testing.T) {
	rec := newEnv(t, Options{}).do(http.MethodPost, "/api/namespaces", map[string]any{"name": "ok", "context": "nope"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}
