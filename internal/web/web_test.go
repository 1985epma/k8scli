package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

const testKubeconfig = `apiVersion: v1
kind: Config
current-context: dev
clusters:
- name: c1
  cluster: {server: "https://c1.example.com"}
contexts:
- name: dev
  context: {cluster: c1, user: u1, namespace: team-a}
- name: prod-eu
  context: {cluster: c1, user: u1}
users:
- name: u1
  user: {token: abc}
`

var fixedNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// env is a Server wired to a fake clientset and a temporary kubeconfig.
type env struct {
	t          *testing.T
	server     *Server
	client     *fake.Clientset
	handler    http.Handler
	kubeconfig string
}

func newEnv(t *testing.T, opts Options, objects ...runtime.Object) *env {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	opts.Kubeconfig = path
	opts.Addr = "127.0.0.1:8484"

	server, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset(objects...)
	registerScaleReactors(client)
	server.newClient = func(string, time.Duration) (kubernetes.Interface, error) { return client, nil }
	server.now = func() time.Time { return fixedNow }

	return &env{t: t, server: server, client: client, handler: server.Handler(), kubeconfig: path}
}

// do sends an authenticated same-origin request.
func (e *env) do(method, target string, body any) *httptest.ResponseRecorder {
	e.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Host = "127.0.0.1:8484"
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.server.Token()})
	if method != http.MethodGet {
		req.Header.Set(csrfHeader, "1")
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
		t.Fatalf("invalid JSON (status %d): %v\n%s", rec.Code, err, rec.Body.String())
	}
	return value
}

func registerScaleReactors(client *fake.Clientset) {
	client.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		obj, err := client.Tracker().Get(action.GetResource(), action.GetNamespace(), action.(k8stesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		d := obj.(*appsv1.Deployment)
		return true, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace},
			Spec:       autoscalingv1.ScaleSpec{Replicas: *d.Spec.Replicas},
		}, nil
	})
	client.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		scale := action.(k8stesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
		obj, err := client.Tracker().Get(action.GetResource(), action.GetNamespace(), scale.Name)
		if err != nil {
			return true, nil, err
		}
		d := obj.(*appsv1.Deployment).DeepCopy()
		d.Spec.Replicas = &scale.Spec.Replicas
		if err := client.Tracker().Update(action.GetResource(), d, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, scale, nil
	})
}

func deployment(namespace, name string, replicas, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: metav1.NewTime(fixedNow.Add(-48 * time.Hour))},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready, UpdatedReplicas: ready},
	}
}

func crashingPod(namespace, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: metav1.NewTime(fixedNow.Add(-2 * time.Hour))},
		Spec:       corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.7",
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: true, RestartCount: 1, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "sidecar", Ready: false, RestartCount: 2, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
			},
		},
	}
}

func replicasOf(t *testing.T, client *fake.Clientset, namespace, name string) int32 {
	t.Helper()
	got, err := client.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return *got.Spec.Replicas
}

// ---- construction and security --------------------------------------------

func TestNewRefusesNonLoopbackUnlessAllowed(t *testing.T) {
	if _, err := New(Options{Addr: "0.0.0.0:8484"}); err == nil || !strings.Contains(err.Error(), "--allow-remote") {
		t.Errorf("err = %v; want a refusal that mentions --allow-remote", err)
	}
	if _, err := New(Options{Addr: "192.168.1.5:8484"}); err == nil {
		t.Error("a LAN address must be refused")
	}
	if _, err := New(Options{Addr: ":8484"}); err == nil {
		t.Error("an empty host listens on every interface and must be refused")
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:8484", "[::1]:8484"} {
		if _, err := New(Options{Addr: addr}); err != nil {
			t.Errorf("New(%q) = %v", addr, err)
		}
	}
	if _, err := New(Options{Addr: "0.0.0.0:8484", AllowRemote: true}); err != nil {
		t.Errorf("--allow-remote should be accepted: %v", err)
	}
	if _, err := New(Options{Addr: "not-an-address"}); err == nil {
		t.Error("a malformed address must be refused")
	}
}

func TestTokensAreRandomAndLong(t *testing.T) {
	a, _ := New(Options{})
	b, _ := New(Options{})
	if a.Token() == b.Token() || len(a.Token()) < 40 {
		t.Errorf("weak tokens: %q %q", a.Token(), b.Token())
	}
}

func TestRequestsNeedASession(t *testing.T) {
	e := newEnv(t, Options{})

	for _, target := range []string{"/", "/app.js", "/api/state"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = "127.0.0.1:8484"
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d; want 401", target, rec.Code)
		}
	}
}

func TestTokenExchangeSetsAStrictCookieAndHidesTheToken(t *testing.T) {
	e := newEnv(t, Options{})

	req := httptest.NewRequest(http.MethodGet, "/?token="+e.server.Token(), nil)
	req.Host = "127.0.0.1:8484"
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d, Location %q; want a redirect to /", rec.Code, rec.Header().Get("Location"))
	}
	cookie := rec.Result().Cookies()
	if len(cookie) != 1 || !cookie[0].HttpOnly || cookie[0].SameSite != http.SameSiteStrictMode || cookie[0].Value != e.server.Token() {
		t.Errorf("cookie = %+v", cookie)
	}

	bad := httptest.NewRequest(http.MethodGet, "/?token=wrong", nil)
	bad.Host = "127.0.0.1:8484"
	badRec := httptest.NewRecorder()
	e.handler.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusUnauthorized || len(badRec.Result().Cookies()) != 0 {
		t.Errorf("a wrong token must not open a session: %d", badRec.Code)
	}
}

func TestBearerTokenWorksForScripts(t *testing.T) {
	e := newEnv(t, Options{})

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Host = "127.0.0.1:8484"
	req.Header.Set("Authorization", "Bearer "+e.server.Token())
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestUnknownHostIsRejected(t *testing.T) {
	e := newEnv(t, Options{})

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Host = "attacker.example:8484" // DNS rebinding
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.server.Token()})
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rec.Code)
	}
}

func TestStateChangingRequestsNeedTheCSRFHeaderAndSameOrigin(t *testing.T) {
	e := newEnv(t, Options{}, deployment("team-a", "api", 2, 2))
	body := scaleRequest{Namespace: "team-a", Name: "api", Replicas: ptr(int32(4))}

	post := func(mutate func(*http.Request)) int {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/scale", bytes.NewReader(raw))
		req.Host = "127.0.0.1:8484"
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.server.Token()})
		mutate(req)
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post(func(*http.Request) {}); code != http.StatusForbidden {
		t.Errorf("no CSRF header = %d; want 403", code)
	}
	if code := post(func(r *http.Request) { r.Header.Set(csrfHeader, "1"); r.Header.Set("Origin", "https://evil.example") }); code != http.StatusForbidden {
		t.Errorf("foreign Origin = %d; want 403", code)
	}
	if code := post(func(r *http.Request) { r.Header.Set(csrfHeader, "1"); r.Header.Set("Origin", "http://127.0.0.1:8484") }); code != http.StatusOK {
		t.Errorf("same-origin request = %d; want 200", code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, Options{})
	rec := e.do(http.MethodGet, "/api/state", nil)

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q; want %q", header, got, want)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP too loose: %q", csp)
	}
}

func TestStaticIndexIsServed(t *testing.T) {
	e := newEnv(t, Options{})
	rec := e.do(http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>") {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
}

func ptr[T any](v T) *T { return &v }

// ---- state, overview, lists -----------------------------------------------

func TestState(t *testing.T) {
	e := newEnv(t, Options{Version: "v1.2.3", ReadOnly: true})
	state := decode[stateResponse](t, e.do(http.MethodGet, "/api/state", nil))

	if state.Version != "v1.2.3" || !state.ReadOnly || state.CurrentContext != "dev" || len(state.Contexts) != 2 {
		t.Fatalf("state = %+v", state)
	}
	if state.Contexts[0].Name != "dev" || state.Contexts[0].Production || state.Contexts[1].Name != "prod-eu" || !state.Contexts[1].Production {
		t.Errorf("contexts = %+v", state.Contexts)
	}
	if len(state.KubeconfigPaths) != 1 || state.KubeconfigPaths[0] != e.kubeconfig {
		t.Errorf("paths = %v", state.KubeconfigPaths)
	}
}

func TestStateWithoutKubeconfig(t *testing.T) {
	server, err := New(Options{Kubeconfig: filepath.Join(t.TempDir(), "missing")})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Host = "127.0.0.1:8484"
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: server.Token()})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	state := decode[stateResponse](t, rec)
	if rec.Code != http.StatusOK || state.KubeconfigError == "" || len(state.Contexts) != 0 {
		t.Errorf("the first run must still answer, with the reason: %d %+v", rec.Code, state)
	}
}

func TestOverview(t *testing.T) {
	ready := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	notReady := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}},
	}
	e := newEnv(t, Options{}, ready, notReady, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}})

	overview := decode[overviewResponse](t, e.do(http.MethodGet, "/api/overview?context=dev", nil))
	if overview.Context != "dev" || overview.DefaultNamespace != "team-a" || len(overview.Nodes) != 2 || !overview.Nodes[0].Ready || overview.Nodes[1].Ready {
		t.Errorf("overview = %+v", overview)
	}
	if len(overview.Namespaces) != 1 || overview.Namespaces[0] != "team-a" {
		t.Errorf("namespaces = %v", overview.Namespaces)
	}

	// No context parameter means the kubeconfig's current one.
	if got := decode[overviewResponse](t, e.do(http.MethodGet, "/api/overview", nil)); got.Context != "dev" {
		t.Errorf("default context = %q", got.Context)
	}
}

func TestUnknownContext(t *testing.T) {
	e := newEnv(t, Options{})
	rec := e.do(http.MethodGet, "/api/pods?context=nope", nil)
	if rec.Code != http.StatusNotFound || decode[errorResponse](t, rec).Code != "unknown_context" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPods(t *testing.T) {
	e := newEnv(t, Options{}, crashingPod("team-a", "api-1"), crashingPod("other", "api-2"))

	pods := decode[[]podJSON](t, e.do(http.MethodGet, "/api/pods?namespace=team-a", nil))
	if len(pods) != 1 {
		t.Fatalf("pods = %+v", pods)
	}
	pod := pods[0]
	if pod.Name != "api-1" || pod.Ready != "1/2" || pod.Status != "CrashLoopBackOff" || pod.Health != "error" ||
		pod.Restarts != 3 || pod.Age != "120m" || pod.IP != "10.0.0.7" || pod.Node != "node-1" || len(pod.Containers) != 2 {
		t.Errorf("pod = %+v", pod)
	}

	all := decode[[]podJSON](t, e.do(http.MethodGet, "/api/pods?namespace=_all", nil))
	if len(all) != 2 {
		t.Errorf("all namespaces returned %d pods", len(all))
	}
	if empty := decode[[]podJSON](t, e.do(http.MethodGet, "/api/pods?namespace=empty", nil)); empty == nil || len(empty) != 0 {
		t.Errorf("an empty namespace must be [] and not null: %v", empty)
	}
}

func TestPodsRejectBadNamespaces(t *testing.T) {
	e := newEnv(t, Options{})
	for _, ns := range []string{"Bad_NS", "../etc", "a%2Fb"} {
		if rec := e.do(http.MethodGet, "/api/pods?namespace="+ns, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("namespace %q = %d; want 400", ns, rec.Code)
		}
	}
}

func TestClusterErrorsCarryAHint(t *testing.T) {
	e := newEnv(t, Options{})
	e.client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New(`Get "https://c1.example.com/api/v1/pods": dial tcp 10.0.0.1:443: connect: connection refused`)
	})

	rec := e.do(http.MethodGet, "/api/pods", nil)
	resp := decode[errorResponse](t, rec)
	if rec.Code != http.StatusBadGateway || !strings.Contains(resp.Error, "connection refused") || !strings.Contains(resp.Hint, "refused the connection") {
		t.Errorf("status %d: %+v", rec.Code, resp)
	}
}

func TestDeployments(t *testing.T) {
	e := newEnv(t, Options{}, deployment("team-a", "api", 3, 2), deployment("team-a", "worker", 0, 0))

	list := decode[[]deploymentJSON](t, e.do(http.MethodGet, "/api/deployments?namespace=team-a", nil))
	if len(list) != 2 {
		t.Fatalf("deployments = %+v", list)
	}
	if list[0].Name != "api" || list[0].Desired != 3 || list[0].Ready != 2 || list[0].Health != "warn" || list[0].Age != "2d" {
		t.Errorf("api = %+v", list[0])
	}
	if list[1].Health != "done" {
		t.Errorf("a deployment scaled to zero should be \"done\": %+v", list[1])
	}
}

// ---- scale ------------------------------------------------------------------

func TestScale(t *testing.T) {
	e := newEnv(t, Options{}, deployment("team-a", "api", 2, 2))

	rec := e.do(http.MethodPost, "/api/scale", scaleRequest{Context: "dev", Namespace: "team-a", Name: "api", Replicas: ptr(int32(5))})
	got := decode[scaleResponse](t, rec)
	if rec.Code != http.StatusOK || !got.Changed || got.From != 2 || got.To != 5 {
		t.Fatalf("status %d: %+v", rec.Code, got)
	}
	if replicasOf(t, e.client, "team-a", "api") != 5 {
		t.Error("the deployment was not scaled")
	}
}

func TestScaleNoOpAndDryRun(t *testing.T) {
	e := newEnv(t, Options{}, deployment("team-a", "api", 2, 2))

	same := decode[scaleResponse](t, e.do(http.MethodPost, "/api/scale", scaleRequest{Namespace: "team-a", Name: "api", Replicas: ptr(int32(2))}))
	if same.Changed {
		t.Error("scaling to the current value must be a no-op")
	}

	dry := decode[scaleResponse](t, e.do(http.MethodPost, "/api/scale", scaleRequest{Namespace: "team-a", Name: "api", Replicas: ptr(int32(8)), DryRun: true}))
	if dry.Changed || !dry.DryRun || dry.From != 2 || dry.To != 8 || replicasOf(t, e.client, "team-a", "api") != 2 {
		t.Errorf("dry run changed something: %+v", dry)
	}
}

func TestScaleInProductionNeedsTheContextName(t *testing.T) {
	e := newEnv(t, Options{}, deployment("team-a", "api", 2, 2))
	req := scaleRequest{Context: "prod-eu", Namespace: "team-a", Name: "api", Replicas: ptr(int32(4))}

	rec := e.do(http.MethodPost, "/api/scale", req)
	if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "confirmation_required" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if replicasOf(t, e.client, "team-a", "api") != 2 {
		t.Fatal("scaled without confirmation")
	}

	req.Confirm = "wrong"
	if rec := e.do(http.MethodPost, "/api/scale", req); rec.Code != http.StatusConflict {
		t.Errorf("a wrong confirmation = %d; want 409", rec.Code)
	}

	req.Confirm = "prod-eu"
	if rec := e.do(http.MethodPost, "/api/scale", req); rec.Code != http.StatusOK || replicasOf(t, e.client, "team-a", "api") != 4 {
		t.Errorf("confirmed scale failed: %d %s", rec.Code, rec.Body.String())
	}

	// A dry run never needs confirmation.
	req.Confirm, req.Replicas, req.DryRun = "", ptr(int32(9)), true
	if rec := e.do(http.MethodPost, "/api/scale", req); rec.Code != http.StatusOK {
		t.Errorf("dry run in production = %d", rec.Code)
	}
}

func TestScaleValidation(t *testing.T) {
	e := newEnv(t, Options{}, deployment("team-a", "api", 2, 2))

	cases := map[string]any{
		"negative replicas": scaleRequest{Namespace: "team-a", Name: "api", Replicas: ptr(int32(-1))},
		"missing replicas":  scaleRequest{Namespace: "team-a", Name: "api"},
		"bad namespace":     scaleRequest{Namespace: "Bad NS", Name: "api", Replicas: ptr(int32(1))},
		"bad name":          scaleRequest{Namespace: "team-a", Name: "../x", Replicas: ptr(int32(1))},
		"unknown field":     map[string]any{"namespace": "team-a", "name": "api", "replicas": 1, "extra": true},
	}
	for name, body := range cases {
		if rec := e.do(http.MethodPost, "/api/scale", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d; want 400", name, rec.Code)
		}
	}

	rec := e.do(http.MethodPost, "/api/scale", scaleRequest{Namespace: "team-a", Name: "missing", Replicas: ptr(int32(1))})
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown deployment = %d; want 404", rec.Code)
	}
}

func TestReadOnlyBlocksChangesButAllowsDryRun(t *testing.T) {
	e := newEnv(t, Options{ReadOnly: true}, deployment("team-a", "api", 2, 2))

	rec := e.do(http.MethodPost, "/api/scale", scaleRequest{Namespace: "team-a", Name: "api", Replicas: ptr(int32(4))})
	if rec.Code != http.StatusForbidden || decode[errorResponse](t, rec).Code != "read_only" {
		t.Errorf("scale in read-only mode = %d", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/api/scale", scaleRequest{Namespace: "team-a", Name: "api", Replicas: ptr(int32(4)), DryRun: true}); rec.Code != http.StatusOK {
		t.Errorf("dry run in read-only mode = %d; want 200", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/api/contexts/default", map[string]string{"name": "prod-eu"}); rec.Code != http.StatusForbidden {
		t.Errorf("set default in read-only mode = %d; want 403", rec.Code)
	}
}

// ---- default context ----------------------------------------------------------

func TestSetDefaultContextWritesTheKubeconfig(t *testing.T) {
	e := newEnv(t, Options{})

	rec := e.do(http.MethodPost, "/api/contexts/default", map[string]string{"name": "prod-eu"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(e.kubeconfig)
	if err != nil || !strings.Contains(string(data), "current-context: prod-eu") {
		t.Errorf("kubeconfig not updated (%v):\n%s", err, data)
	}
	if rec := e.do(http.MethodPost, "/api/contexts/default", map[string]string{"name": "nope"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown context = %d; want 404", rec.Code)
	}
}

// ---- connection check ---------------------------------------------------------

func TestCheckSucceeds(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	e := newEnv(t, Options{}, node, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}})

	check := decode[checkResponse](t, e.do(http.MethodGet, "/api/check?context=dev", nil))
	if !check.OK || check.NodesReady != 1 || check.NodesTotal != 1 || len(check.Namespaces) != 1 {
		t.Errorf("check = %+v", check)
	}
}

func TestCheckReportsAFailureAsAResult(t *testing.T) {
	e := newEnv(t, Options{})
	e.server.newClient = func(string, time.Duration) (kubernetes.Interface, error) {
		return nil, errors.New(`getting credentials: exec: executable file not found in $PATH: "aws"`)
	}

	rec := e.do(http.MethodGet, "/api/check?context=dev", nil)
	check := decode[checkResponse](t, rec)
	if rec.Code != http.StatusOK || check.OK || !strings.Contains(check.Hint, "credential plugin") {
		t.Errorf("status %d: %+v", rec.Code, check)
	}
}

// ---- logs ---------------------------------------------------------------------

func TestLogs(t *testing.T) {
	e := newEnv(t, Options{}, crashingPod("team-a", "api-1"))

	rec := e.do(http.MethodGet, "/api/logs?namespace=team-a&pod=api-1&container=sidecar&tail=20&since=5m&timestamps=true", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/plain") || rec.Body.Len() == 0 {
		t.Errorf("status %d, type %q, body %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestLogsValidation(t *testing.T) {
	e := newEnv(t, Options{}, crashingPod("team-a", "api-1"))

	for name, target := range map[string]string{
		"unknown container": "/api/logs?namespace=team-a&pod=api-1&container=nope",
		"bad tail":          "/api/logs?namespace=team-a&pod=api-1&tail=-5",
		"huge tail":         "/api/logs?namespace=team-a&pod=api-1&tail=99999999",
		"bad since":         "/api/logs?namespace=team-a&pod=api-1&since=yesterday",
		"bad pod":           "/api/logs?namespace=team-a&pod=..%2Fsecret",
		"missing pod":       "/api/logs?namespace=team-a",
	} {
		if rec := e.do(http.MethodGet, target, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d; want 400", name, rec.Code)
		}
	}
	if rec := e.do(http.MethodGet, "/api/logs?namespace=team-a&pod=gone", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown pod = %d; want 404", rec.Code)
	}
}

func TestLogOptions(t *testing.T) {
	options, err := logOptions("", "")
	if err != nil || options.TailLines == nil || *options.TailLines != defaultTail || options.SinceSeconds != nil {
		t.Errorf("defaults = %+v, %v", options, err)
	}
	options, err = logOptions("all", "2h")
	if err != nil || options.TailLines != nil || *options.SinceSeconds != 7200 {
		t.Errorf("all/2h = %+v, %v", options, err)
	}
}

// ---- health and hints ------------------------------------------------------------

func TestPodHealth(t *testing.T) {
	for _, tc := range []struct{ status, ready, want string }{
		{"Running", "2/2", "ok"},
		{"Running", "1/2", "warn"},
		{"Completed", "0/1", "done"},
		{"Pending", "0/1", "warn"},
		{"ContainerCreating", "0/1", "warn"},
		{"Init:0/2", "0/1", "warn"},
		{"Init:Error", "0/1", "error"},
		{"Init:CrashLoopBackOff", "0/1", "error"},
		{"CrashLoopBackOff", "1/2", "error"},
		{"ImagePullBackOff", "0/1", "error"},
		{"OOMKilled", "0/1", "error"},
	} {
		if got := podHealth(tc.status, tc.ready); got != tc.want {
			t.Errorf("podHealth(%q, %q) = %q; want %q", tc.status, tc.ready, got, tc.want)
		}
	}
}

func TestDeploymentHealth(t *testing.T) {
	for _, tc := range []struct {
		desired, ready int32
		want           string
	}{{3, 3, "ok"}, {3, 2, "warn"}, {3, 0, "error"}, {0, 0, "done"}} {
		if got := deploymentHealth(tc.desired, tc.ready); got != tc.want {
			t.Errorf("deploymentHealth(%d, %d) = %q; want %q", tc.desired, tc.ready, got, tc.want)
		}
	}
}

func TestDiagnose(t *testing.T) {
	for message, fragment := range map[string]string{
		"dial tcp: lookup api.example.com: no such host":                            "VPN or DNS",
		"Get https://x: x509: certificate signed by unknown":                        "certificate-authority-data",
		"the server has asked for the client to provide credentials (Unauthorized)": "expired",
		"pods is forbidden: User cannot list resource":                              "read access",
		"dial tcp 1.2.3.4:443: i/o timeout":                                         "did not answer in time",
	} {
		if hint := diagnose(errors.New(message)); !strings.Contains(hint, fragment) {
			t.Errorf("diagnose(%q) = %q; want it to mention %q", message, hint, fragment)
		}
	}
	if hint := diagnose(errors.New("something unusual")); hint != "" {
		t.Errorf("an unknown error should have no hint, got %q", hint)
	}
}

// ---- client cache ----------------------------------------------------------------

func TestClientsAreCachedUntilTheyExpire(t *testing.T) {
	e := newEnv(t, Options{})
	built := 0
	e.server.newClient = func(string, time.Duration) (kubernetes.Interface, error) {
		built++
		return e.client, nil
	}
	clock := fixedNow
	e.server.now = func() time.Time { return clock }

	for range 3 {
		if _, err := e.server.clientFor("dev", 0); err != nil {
			t.Fatal(err)
		}
	}
	if built != 1 {
		t.Errorf("built %d clients; want 1 (exec credential plugins are slow)", built)
	}

	clock = clock.Add(clientTTL + time.Second)
	if _, err := e.server.clientFor("dev", 0); err != nil || built != 2 {
		t.Errorf("expired client not rebuilt: built=%d err=%v", built, err)
	}

	// Clients with a timeout are never cached.
	_, _ = e.server.clientFor("dev", time.Second)
	_, _ = e.server.clientFor("dev", time.Second)
	if built != 4 {
		t.Errorf("timeout clients were cached: built=%d", built)
	}
}

// ---- run -------------------------------------------------------------------------

func TestRunServesAndStopsOnCancel(t *testing.T) {
	server, err := New(Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, out) }()

	var link string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && link == "" {
		for _, field := range strings.Fields(out.String()) {
			if strings.HasPrefix(field, "http://") {
				link = field
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if link == "" || !strings.Contains(link, "token="+server.Token()) {
		t.Fatalf("no link printed: %q", out.String())
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d; want the token redirect", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after the context was cancelled")
	}
}

// syncBuffer lets the test read what Run prints while Run is still writing.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---- adversarial regressions ------------------------------------------------------

func TestAPIRejectsCrossSiteAndSameSiteFetches(t *testing.T) {
	e := newEnv(t, Options{})

	get := func(target, site, origin string) int {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = "127.0.0.1:8484"
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.server.Token()})
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// Another web app on a different port of the same host is "same-site", so
	// SameSite=Strict lets its fetches carry the cookie. The API must refuse them.
	for _, site := range []string{"cross-site", "same-site"} {
		if code := get("/api/state", site, ""); code != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site %q = %d; want 403", site, code)
		}
	}
	if code := get("/api/state", "", "http://localhost:3000"); code != http.StatusForbidden {
		t.Errorf("a foreign Origin on a GET = %d; want 403", code)
	}
	for _, site := range []string{"same-origin", "none", ""} {
		if code := get("/api/state", site, ""); code != http.StatusOK {
			t.Errorf("Sec-Fetch-Site %q = %d; want 200", site, code)
		}
	}
	if code := get("/api/state", "same-origin", "http://127.0.0.1:8484"); code != http.StatusOK {
		t.Errorf("same-origin Origin = %d; want 200", code)
	}
	// Opening the page from a link in another app is a navigation, not an API call.
	if code := get("/", "cross-site", ""); code != http.StatusOK {
		t.Errorf("a cross-site navigation to the page = %d; want 200", code)
	}
}

func TestNullOriginIsRejected(t *testing.T) {
	e := newEnv(t, Options{}, deployment("team-a", "api", 2, 2))
	raw, _ := json.Marshal(scaleRequest{Namespace: "team-a", Name: "api", Replicas: ptr(int32(4))})
	req := httptest.NewRequest(http.MethodPost, "/api/scale", bytes.NewReader(raw))
	req.Host = "127.0.0.1:8484"
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.server.Token()})
	req.Header.Set(csrfHeader, "1")
	req.Header.Set("Origin", "null") // sandboxed iframes and some redirects
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || replicasOf(t, e.client, "team-a", "api") != 2 {
		t.Errorf("status %d; the deployment must not change", rec.Code)
	}
}

func TestHostHeaderEdgeCases(t *testing.T) {
	e := newEnv(t, Options{})
	for host, want := range map[string]int{
		"127.0.0.1:8484":              http.StatusOK,
		"LOCALHOST:8484":              http.StatusOK, // host names are case-insensitive
		"[::1]:8484":                  http.StatusOK,
		"127.0.0.1":                   http.StatusForbidden, // browsers always send the port
		"127.0.0.1:9999":              http.StatusForbidden,
		"localhost.:8484":             http.StatusForbidden,
		"127.0.0.1:8484.evil.example": http.StatusForbidden,
		"":                            http.StatusForbidden,
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
		req.Host = host
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.server.Token()})
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q = %d; want %d", host, rec.Code, want)
		}
	}
}

func TestStaticHandlerDoesNotEscapeTheEmbeddedDirectory(t *testing.T) {
	e := newEnv(t, Options{})
	for _, target := range []string{"/../../../etc/passwd", "/..%2f..%2f..%2fetc%2fpasswd", "/%2e%2e/%2e%2e/etc/passwd", "/static/../../go.mod", "/web_test.go", "/server.go"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = "127.0.0.1:8484"
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.server.Token()})
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		body := rec.Body.String()
		if strings.Contains(body, "root:") || strings.Contains(body, "package web") || strings.Contains(body, "module github.com") {
			t.Errorf("%s leaked a file outside static/ (status %d)", target, rec.Code)
		}
	}
}

func TestTokenIsNotAcceptedAnywhereButTheRoot(t *testing.T) {
	e := newEnv(t, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/state?token="+e.server.Token(), nil)
	req.Host = "127.0.0.1:8484"
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a token in an API query string = %d; want 401 (it would end up in logs)", rec.Code)
	}
}

func TestBodyWithTrailingDataIsRejected(t *testing.T) {
	e := newEnv(t, Options{}, deployment("team-a", "api", 2, 2))
	req := httptest.NewRequest(http.MethodPost, "/api/scale",
		strings.NewReader(`{"namespace":"team-a","name":"api","replicas":4}{"replicas":9}`))
	req.Host = "127.0.0.1:8484"
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.server.Token()})
	req.Header.Set(csrfHeader, "1")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || replicasOf(t, e.client, "team-a", "api") != 2 {
		t.Errorf("status %d; trailing JSON must be rejected without scaling", rec.Code)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	e := newEnv(t, Options{})
	big := `{"namespace":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/scale", strings.NewReader(big))
	req.Host = "127.0.0.1:8484"
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.server.Token()})
	req.Header.Set(csrfHeader, "1")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d; want 400", rec.Code)
	}
}

func TestEveryMutatingEndpointHonoursReadOnly(t *testing.T) {
	e := newEnv(t, Options{ReadOnly: true}, deployment("team-a", "api", 2, 2))

	// Walk the real routes so a new POST endpoint cannot ship without a decision.
	mutating := map[string]any{
		"/api/scale":            scaleRequest{Namespace: "team-a", Name: "api", Replicas: ptr(int32(9))},
		"/api/contexts/default": map[string]string{"name": "prod-eu"},
	}
	for path, body := range mutating {
		if rec := e.do(http.MethodPost, path, body); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s in read-only mode = %d; want 403", path, rec.Code)
		}
	}
	if replicasOf(t, e.client, "team-a", "api") != 2 {
		t.Error("read-only mode changed a deployment")
	}
	data, _ := os.ReadFile(e.kubeconfig)
	if !strings.Contains(string(data), "current-context: dev") {
		t.Error("read-only mode rewrote the kubeconfig")
	}
	// Methods other than GET/POST must not reach any handler either.
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if rec := e.do(method, "/api/scale", scaleRequest{Namespace: "team-a", Name: "api", Replicas: ptr(int32(9))}); rec.Code != http.StatusNotFound {
			t.Errorf("%s /api/scale = %d; want 404", method, rec.Code)
		}
	}
}

func TestCheckAndSetDefaultDropCachedClients(t *testing.T) {
	e := newEnv(t, Options{})
	built := 0
	e.server.newClient = func(string, time.Duration) (kubernetes.Interface, error) {
		built++
		return e.client, nil
	}

	for range 2 {
		_, _ = e.server.clientFor("dev", 0)
	}
	if built != 1 {
		t.Fatalf("built %d; want the second call to hit the cache", built)
	}

	e.do(http.MethodGet, "/api/check?context=dev", nil)
	before := built
	_, _ = e.server.clientFor("dev", 0)
	if built != before+1 {
		t.Errorf("a connection check must not leave a stale client behind (built %d -> %d)", before, built)
	}

	_, _ = e.server.clientFor("dev", 0)
	before = built
	e.do(http.MethodPost, "/api/contexts/default", map[string]string{"name": "prod-eu"})
	_, _ = e.server.clientFor("dev", 0)
	if built != before+1 {
		t.Errorf("changing the default context must drop cached clients (built %d -> %d)", before, built)
	}
}

func TestClientCacheIsBoundedByTheKubeconfig(t *testing.T) {
	e := newEnv(t, Options{})
	// Names that are not in the kubeconfig never reach clientFor.
	for i := range 20 {
		e.do(http.MethodGet, "/api/pods?context=ghost-"+strings.Repeat("x", i+1), nil)
	}
	e.server.mu.Lock()
	n := len(e.server.clients)
	e.server.mu.Unlock()
	if n != 0 {
		t.Errorf("%d clients cached for contexts that do not exist", n)
	}
}

func TestServerVersionIsCancelledWithTheRequest(t *testing.T) {
	cancelled := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answers
		close(cancelled)
	}))
	defer hung.Close()

	client, err := kubernetes.NewForConfig(&rest.Config{Host: hung.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := serverVersion(ctx, client); err == nil {
		t.Fatal("expected an error from a server that never answers")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("serverVersion ignored the context deadline")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Error("the request to the API server was left running (goroutine leak on every refresh)")
	}
}

func TestServerVersionParsesTheResponse(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"major":"1","minor":"31","gitVersion":"v1.31.4","platform":"linux/amd64"}`))
	}))
	defer api.Close()

	client, err := kubernetes.NewForConfig(&rest.Config{Host: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	info, err := serverVersion(context.Background(), client)
	if err != nil || info.GitVersion != "v1.31.4" || info.Platform != "linux/amd64" {
		t.Errorf("info = %+v, err = %v", info, err)
	}
}
