package extchecks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/1985epma/k8scli/internal/dr"
)

func sortedNames(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// The typed fallback copies names and nothing else: no value may become part of what it returns.
func TestListNamesReturnsExactlyTheNames(t *testing.T) {
	env := newEnv("c", []runtime.Object{
		secret("shop", "db-pass", map[string]string{"password": "VALUE-ONE"}),
		secret("shop", "tls", map[string]string{"tls.key": "VALUE-TWO"}),
		secret("other", "elsewhere", nil),
		configMap("shop", "cfg", map[string]string{"k": "VALUE-THREE"}),
	})
	for resource, want := range map[string]string{"secrets": "db-pass,tls", "configmaps": "cfg"} {
		got, err := listNames(context.Background(), env, resource, "shop")
		if err != nil {
			t.Fatal(err)
		}
		if sortedNames(got) != want {
			t.Errorf("listNames(%s) = %s; want exactly %s", resource, sortedNames(got), want)
		}
	}
	if _, err := listNames(context.Background(), env, "pods", "shop"); err == nil {
		t.Error("only secrets and configmaps are supported")
	}
}

// With a real REST client the request itself asks for metadata only, so values never leave the server.
func TestListNamesAsksTheServerForMetadataOnly(t *testing.T) {
	var (
		mu      sync.Mutex
		accepts []string
		paths   []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		accepts = append(accepts, r.Header.Get("Accept"))
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// A server that ignores the header would send values too; they must be ignored.
		if r.URL.Query().Get("continue") == "" {
			fmt.Fprint(w, `{"kind":"PartialObjectMetadataList","items":[{"metadata":{"name":"one"},"data":{"password":"LEAKED-1"}},{"metadata":{"name":"two"}}],"metadata":{"continue":"page2"}}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"metadata":{"name":"three"},"data":{"password":"LEAKED-2"}}],"metadata":{}}`)
	}))
	defer server.Close()

	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	env := &dr.Env{Client: client}
	got, err := listNames(context.Background(), env, "secrets", "shop")
	if err != nil {
		t.Fatal(err)
	}
	if sortedNames(got) != "one,three,two" {
		t.Errorf("names = %s (pagination must be followed)", sortedNames(got))
	}
	if out := fmt.Sprint(got); strings.Contains(out, "LEAKED") {
		t.Errorf("a value reached the result: %s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(accepts) != 2 {
		t.Fatalf("expected two page requests, got %d", len(accepts))
	}
	for i, accept := range accepts {
		if !strings.Contains(accept, "as=PartialObjectMetadataList") {
			t.Errorf("request %d does not ask for metadata only: Accept = %q", i, accept)
		}
		if paths[i] != "/api/v1/namespaces/shop/secrets" {
			t.Errorf("path = %q", paths[i])
		}
	}
}

func TestListNamesRefusesAHostileNamespaceAndReportsErrors(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "status": "Failure", "message": "forbidden", "reason": "Forbidden", "code": 403})
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	env := &dr.Env{Client: client}

	for _, bad := range []string{"a/../b", "../kube-system", "UPPER", "a b", "", "x%2Fy", strings.Repeat("a", 64)} {
		if _, err := listNames(context.Background(), env, "secrets", bad); err == nil {
			t.Errorf("namespace %q must be refused", bad)
		}
	}
	if requests != 0 {
		t.Errorf("a hostile namespace must never reach the network (%d requests)", requests)
	}

	if _, err := listNames(context.Background(), env, "configmaps", "shop"); err == nil {
		t.Error("a forbidden answer must be an error")
	}
	if requests != 1 {
		t.Errorf("requests = %d", requests)
	}
}
