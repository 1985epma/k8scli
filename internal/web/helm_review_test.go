package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
)

// Regression tests for the adversarial review of the Helm backend (ADR 0004). Each one fails when the
// defense it covers is taken out.

// ---- a release is not marked failed because the handler returned ------------------------------------------

func TestARealWriteKeepsItsContextAliveBrieflyAfterTheHandlerReturns(t *testing.T) {
	previous := helmCancelGrace
	helmCancelGrace = 40 * time.Millisecond
	defer func() { helmCancelGrace = previous }()

	request := httptest.NewRequest(http.MethodPost, "/api/helm/install", http.NoBody)
	ctx, cancel := writeContext(request, false)
	cancel() // what the handler's defer does the moment the answer is written

	// Helm's goroutine that picks between "finished" and "cancelled" must not see a cancelled
	// context right away: when it did, it sometimes recorded a successful release as failed.
	if err := ctx.Err(); err != nil {
		t.Fatalf("the context was cancelled at once (%v); Helm can mark a finished release as failed", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the context was never released")
	}

	// A preview follows the request: nothing keeps running for it.
	dry, cancelDry := writeContext(request, true)
	cancelDry()
	if dry.Err() == nil {
		t.Error("a preview's context should end with the request")
	}
}

// ---- Artifact Hub rate limit -------------------------------------------------------------------------------------

func TestArtifactHubRateLimitIsA429WithItsOwnCode(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.hub.mu.Lock()
	h.hub.forceStatus = http.StatusTooManyRequests
	h.hub.mu.Unlock()

	rec := h.do(http.MethodGet, "/api/helm/search?q=demo", nil)
	if got := decode[errorResponse](t, rec); rec.Code != http.StatusTooManyRequests || got.Code != "hub_rate_limited" {
		t.Errorf("search while rate limited: %d %+v", rec.Code, got)
	}

	// A real upstream failure stays a 502 hub_unavailable.
	h.hub.mu.Lock()
	h.hub.forceStatus = http.StatusServiceUnavailable
	h.hub.mu.Unlock()
	rec = h.do(http.MethodGet, "/api/helm/search?q=other", nil)
	if got := decode[errorResponse](t, rec); rec.Code != http.StatusBadGateway || got.Code != "hub_unavailable" {
		t.Errorf("search while Artifact Hub is down: %d %+v", rec.Code, got)
	}
}

func TestArtifactHubRateLimitOnAPackageLookupAndItsRetryHint(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	h.hub.mu.Lock()
	h.hub.forceStatus = http.StatusTooManyRequests
	h.hub.mu.Unlock()

	body := hubBody(map[string]any{"dryRun": true})
	rec := h.do(http.MethodPost, "/api/helm/install", body)
	if got := decode[errorResponse](t, rec); rec.Code != http.StatusTooManyRequests || got.Code != "hub_rate_limited" {
		t.Errorf("package lookup while rate limited: %d %+v", rec.Code, got)
	}

	for header, want := range map[string]string{"30": "30 seconds", "": "limiting requests", "soon": "limiting requests", "999999": "limiting requests"} {
		got := hubRateLimited(header)
		if got.status != http.StatusTooManyRequests || got.code != "hub_rate_limited" || !strings.Contains(got.msg, want) {
			t.Errorf("hubRateLimited(%q) = %+v; want a message with %q", header, got, want)
		}
	}
	if strings.Contains(hubRateLimited("soon").msg, "retry in") {
		t.Error("an unreadable Retry-After must not invent a delay")
	}
}

// ---- where a chart download may connect ---------------------------------------------------------------------------

func TestBlockedIPCoversTheAddressesADownloadMustNeverReach(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.9", "192.168.1.1", "fc00::1", "fd12:3456::1",
		"169.254.169.254", "fe80::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1", "100.64.0.1", "198.18.0.1", "240.0.0.1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::a00:1", "2002:a00:1::",
	}
	for _, text := range blocked {
		if !blockedIP(netip.MustParseAddr(text)) {
			t.Errorf("%s must be blocked", text)
		}
	}
	for _, text := range []string{"93.184.216.34", "140.82.112.3", "2606:4700:4700::1111", "185.199.108.153"} {
		if blockedIP(netip.MustParseAddr(text)) {
			t.Errorf("%s is a public address and must be allowed", text)
		}
	}
}

func TestTheDialGuardChecksTheResolvedAddressAndHonorsConfiguredProxies(t *testing.T) {
	control := guardedControl(map[string]bool{"10.9.9.9:3128": true})
	for address, wantErr := range map[string]bool{
		"169.254.169.254:80": true, "127.0.0.1:8443": true, "[::1]:443": true, "10.0.0.5:443": true, "not-an-address": true,
		"93.184.216.34:443": false, "10.9.9.9:3128": false, // the proxy the user configured
	} {
		err := control("tcp", address, nil)
		if (err != nil) != wantErr {
			t.Errorf("control(%q) = %v; want an error: %v", address, err, wantErr)
		}
		if err != nil && !errors.Is(err, errBlockedAddress) {
			t.Errorf("control(%q): the error is not errBlockedAddress: %v", address, err)
		}
	}
}

func TestTheProductionDownloadClientRefusesAnInternalServer(t *testing.T) {
	internal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("chart")) }))
	defer internal.Close()

	engine := newHelmEngine(&Server{})
	_, _, err := engine.download(context.Background(), internal.URL+"/chart.tgz", "demo")
	if err == nil || !strings.Contains(err.Error(), "not a public one") {
		t.Fatalf("a download from a loopback server must be refused at connect time, got %v", err)
	}

	// The Artifact Hub client uses the same transport.
	if _, ok := newHubClient().http.Transport.(*http.Transport); !ok {
		t.Error("the Artifact Hub client lost the guarded transport")
	}
}

func TestDownloadAndRedirectsRefuseUserInfo(t *testing.T) {
	engine := newHelmEngine(&Server{})
	if _, _, err := engine.download(context.Background(), "https://user:pw@example.test/chart.tgz", "demo"); err == nil {
		t.Error("an address with a user and password must be refused")
	}

	first := httptest.NewRequest(http.MethodGet, "https://charts.example.test/a.tgz", http.NoBody)
	hop := httptest.NewRequest(http.MethodGet, "https://user:pw@github.com/org/repo/a.tgz", http.NoBody)
	if err := engine.httpClient.CheckRedirect(hop, []*http.Request{first}); err == nil {
		t.Error("a redirect that carries credentials must be refused")
	}
	ok := httptest.NewRequest(http.MethodGet, "https://github.com/org/repo/a.tgz", http.NoBody)
	if err := engine.httpClient.CheckRedirect(ok, []*http.Request{first}); err != nil {
		t.Errorf("a plain redirect to GitHub must still work: %v", err)
	}
}

// ---- charts from search cannot read Secrets while they render ----------------------------------------------------------

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okTransport(calls *int) http.RoundTripper {
	return roundTripFunc(func(*http.Request) (*http.Response, error) {
		*calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
}

func TestSecretBlindRefusesEverySecretReadAndEveryWrite(t *testing.T) {
	calls := 0
	rt := secretBlind{next: okTransport(&calls)}

	refused := []struct{ method, target string }{
		{http.MethodGet, "https://k/api/v1/namespaces/kube-system/secrets/admin"},
		{http.MethodGet, "https://k/api/v1/secrets"},
		{http.MethodGet, "https://k/api/v1/namespaces/a/secrets?limit=500"},
		{http.MethodGet, "https://k/api/v1/namespaces/a/../b/SECRETS"},
		{http.MethodGet, "https://k/api/v1//namespaces/a/./secrets/x"},
		{http.MethodGet, "https://k/api/v1/namespaces/a/%73ecrets/x"},
		{http.MethodPost, "https://k/api/v1/namespaces/a/pods"},
		{http.MethodDelete, "https://k/api/v1/namespaces/a/pods/p"},
		{http.MethodPut, "https://k/api/v1/namespaces/a/configmaps/c"},
		{http.MethodPatch, "https://k/api/v1/namespaces/a/configmaps/c"},
	}
	for _, c := range refused {
		req := httptest.NewRequest(c.method, c.target, http.NoBody)
		if _, err := rt.RoundTrip(req); !errors.Is(err, errSecretsBlind) {
			t.Errorf("%s %s was not refused: %v", c.method, c.target, err)
		}
	}
	if calls != 0 {
		t.Fatalf("%d refused requests reached the cluster", calls)
	}

	for _, target := range []string{"https://k/api/v1", "https://k/api/v1/namespaces/a/pods", "https://k/api/v1/namespaces/a/configmaps/c", "https://k/apis/apps/v1/deployments"} {
		resp, err := rt.RoundTrip(httptest.NewRequest(http.MethodGet, target, http.NoBody))
		if err != nil {
			t.Errorf("GET %s must still work: %v", target, err)
			continue
		}
		_ = resp.Body.Close()
	}
	if calls != 4 {
		t.Errorf("expected the 4 plain reads to go through, saw %d", calls)
	}
}

func TestBlindToSecretsOnlyChangesWhatTheRendererSees(t *testing.T) {
	inner := &rest.Config{Host: "https://cluster.test"}
	getter := genericclioptions.NewConfigFlags(false)
	cfg := &action.Configuration{RESTClientGetter: getter}
	blindToSecrets(cfg)

	wrapped, ok := cfg.RESTClientGetter.(secretBlindGetter)
	if !ok {
		t.Fatalf("the getter was not wrapped: %T", cfg.RESTClientGetter)
	}
	if wrapped.RESTClientGetter != action.RESTClientGetter(getter) {
		t.Error("the wrapper must keep the original getter for everything else")
	}

	// The copy the renderer gets refuses Secrets; the original configuration is untouched.
	copied := rest.CopyConfig(inner)
	copied.Wrap(func(next http.RoundTripper) http.RoundTripper { return secretBlind{next: next} })
	calls := 0
	if _, err := copied.WrapTransport(okTransport(&calls)).RoundTrip(httptest.NewRequest(http.MethodGet, "https://k/api/v1/namespaces/a/secrets", http.NoBody)); !errors.Is(err, errSecretsBlind) {
		t.Errorf("the wrapped transport let a Secret read through: %v", err)
	}
	if inner.WrapTransport != nil {
		t.Error("the original configuration was changed")
	}

	// Nothing to wrap, nothing to break.
	blindToSecrets(nil)
	blindToSecrets(&action.Configuration{})
}

func TestChartLooksUpFindsLookupInTemplatesFilesAndValues(t *testing.T) {
	file := func(name, data string) *chart.File { return &chart.File{Name: name, Data: []byte(data)} }
	cases := map[string]*chart.Chart{
		"template": {Templates: []*chart.File{file("templates/a.yaml", `{{ $x := lookup "v1" "ConfigMap" "ns" "n" }}`)}},
		"file":     {Files: []*chart.File{file("extra.txt", `{{- lookup "v1" "Secret" "kube-system" "s" -}}`)}},
		"values":   {Values: map[string]any{"extra": map[string]any{"name": `{{ (lookup "v1" "Secret" "a" "b").data.x }}`}}},
		"subchart": {Metadata: &chart.Metadata{Name: "p"}},
	}
	sub := &chart.Chart{Metadata: &chart.Metadata{Name: "s"}, Templates: []*chart.File{file("templates/s.yaml", `{{ lookup "v1" "Namespace" "" "x" }}`)}}
	cases["subchart"].AddDependency(sub)

	for name, c := range cases {
		if !chartLooksUp(c) {
			t.Errorf("%s: a lookup was not found", name)
		}
	}
	plain := &chart.Chart{Templates: []*chart.File{file("templates/a.yaml", "kind: ConfigMap # nothing looked up, just the word in a comment? no")}, Values: map[string]any{"note": "lookup tables are fine"}}
	if chartLooksUp(plain) {
		t.Error("the word lookup outside a template action is not a lookup call")
	}
	if chartLooksUp(nil) {
		t.Error("a nil chart looks nothing up")
	}
}

// ---- settings: plain values only -----------------------------------------------------------------------------------------

func settingsChart() *chart.Chart {
	return &chart.Chart{Values: map[string]any{
		"message": "", "replicas": float64(1), "enabled": false, "ports": []any{float64(80)},
		"extra-objects": nil, "sub": map[string]any{"extra_objects": nil, "ok": ""},
		"auth": map[string]any{"password": "", "pwd": ""},
	}}
}

func TestSettingsRefuseTemplatesAndStructuredValues(t *testing.T) {
	c := settingsChart()
	refused := map[string]string{
		"{{ lookup \"v1\" \"Secret\" \"kube-system\" \"x\" }}": "{{ or }}",
		"{{ .Values.x }}":    "{{ or }}",
		"before }} after":    "{{ or }}",
		"a: b":               "group of values",
		"{a: b}":             "group of values",
		`{"kind":"Pod"}`:     "group of values",
		"[1, 2]":             "group of values",
		"- one":              "group of values",
		"kind: Pod\nname: x": "control or invisible",
	}
	for value, want := range refused {
		_, _, err := checkSettings(c, map[string]any{"message": value})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("message=%q: want a refusal mentioning %q, got %v", value, want, err)
		}
	}
	for _, value := range []string{"hello world", "ClusterIP", "10.0.0.1:8080", "v1.2.3-rc1", "100%", "a=b"} {
		if _, _, err := checkSettings(c, map[string]any{"message": value}); err != nil {
			t.Errorf("message=%q is a plain value and must be accepted: %v", value, err)
		}
	}

	// Random-looking values for passwords may look like YAML by chance, but never as a template.
	if _, _, err := checkSettings(c, map[string]any{"auth.password": "{a: b}"}); err != nil {
		t.Errorf("a password that looks like YAML must be accepted: %v", err)
	}
	if _, _, err := checkSettings(c, map[string]any{"auth.password": "{{ lookup }}"}); err == nil {
		t.Error("a password may not carry a template action")
	}
}

func TestSettingPathsCannotReachObjectKeysByAnySpelling(t *testing.T) {
	c := settingsChart()
	for _, path := range []string{
		"extra-objects", "sub.extra_objects", "extraObjects", "EXTRA_OBJECTS", "sub.Extra-Objects",
		"ports[0]", `a\.b`, "message..x", "mess\u0430ge", "message ", "../message", "message.", ".message", "ports.0",
		"sub", "ports", // groups and lists, not plain values
	} {
		if _, _, err := checkSettings(c, map[string]any{path: "x"}); err == nil {
			t.Errorf("the path %q must be refused", path)
		}
	}
	if _, _, err := checkSettings(c, map[string]any{"sub.ok": "x"}); err != nil {
		t.Errorf("a nested plain value must work: %v", err)
	}
	if normalizeSegment("Extra-Objects") != "extraobjects" || normalizeSegment("extra_objects") != "extraobjects" {
		t.Error("normalizeSegment must fold case, - and _")
	}
}

func TestCatalogFieldsRefuseTemplateActions(t *testing.T) {
	h := newHelmEnv(t, Options{})
	body := installBody(map[string]any{"fields": map[string]any{"message": "{{ lookup }}"}})
	body["dryRun"] = true
	rec := h.do(http.MethodPost, "/api/helm/install", body)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "{{") {
		t.Errorf("a template action in a catalog field: %d %s", rec.Code, rec.Body.String())
	}
}

// ---- the review sees hooks, crds/ objects and other namespaces ------------------------------------------------------------

func hostileChart() *chart.Chart {
	file := func(name, data string) *chart.File { return &chart.File{Name: name, Data: []byte(data)} }
	return &chart.Chart{
		Metadata: &chart.Metadata{APIVersion: "v2", Name: "demo", Version: "1.0.0", AppVersion: "1"},
		Templates: []*chart.File{
			file("templates/ok.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-ok\n"),
			file("templates/elsewhere.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: planted\n  namespace: kube-system\n"),
			file("templates/hook.yaml", `apiVersion: batch/v1
kind: Job
metadata:
  name: {{ .Release.Name }}-bootstrap
  annotations:
    "helm.sh/hook": pre-install
spec:
  template:
    spec:
      hostNetwork: true
      restartPolicy: Never
      containers:
      - name: miner
        image: evil.example/miner:latest
        securityContext:
          privileged: true
`),
			file("templates/lookup.yaml", "{{ if false }}{{ lookup \"v1\" \"ConfigMap\" .Release.Namespace \"x\" }}{{ end }}"),
		},
		Files: []*chart.File{file("crds/rbac.yaml", `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: everything
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
- kind: ServiceAccount
  name: default
  namespace: default
`)},
		Raw:    []*chart.File{file("values.yaml", "message: \"\"\nlicense:\n  key: \"\"\ndb:\n  url: \"\"\n")},
		Values: map[string]any{"message": "", "license": map[string]any{"key": ""}, "db": map[string]any{"url": ""}},
	}
}

func (h *hubEnv) useHostileChart(t *testing.T) {
	t.Helper()
	data, sum := packChart(t, hostileChart())
	h.archives["/hostile.tgz"] = data
	h.setDetail("acme/demo", "1.0.0", sum, "/hostile.tgz")
}

func TestTheReviewListsHooksCRDDirObjectsAndObjectsInOtherNamespaces(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.useHostileChart(t)
	h.searched(t)

	got := h.preview(t, nil)
	review := strings.Join(got.Changes, "\n")
	for _, want := range []string{
		"warning: runs Helm hooks", "Job/web-bootstrap (pre-install)",
		"warning: the chart's crds/ directory holds objects that are not CustomResourceDefinitions", "ClusterRoleBinding/everything",
		"warning: creates objects in other namespaces than the one chosen: kube-system (1)",
		"warning: creates cluster-wide objects", "ClusterRoleBinding (1)",
		"warning: runs with elevated privileges", "Job/web-bootstrap",
		"warning: binds the cluster-admin role", "warning: runs images that are not pinned", "evil.example/miner:latest",
		"image: evil.example/miner:latest",
		"warning: the chart reads existing objects of the cluster while it renders",
	} {
		if !strings.Contains(review, want) {
			t.Errorf("the review hides %q:\n%s", want, review)
		}
	}
	if len(got.Images) != 1 || got.Images[0] != "evil.example/miner:latest" {
		t.Errorf("an image that only a hook runs must be listed: %v", got.Images)
	}
}

func TestWarningsForCountsHooksExtrasAndForeignNamespaces(t *testing.T) {
	docs := parseManifest("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: kube-system\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n", "apps")
	docs[1].hook = "post-install"
	docs = append(docs, manifestDoc{Kind: "Role", fromCRDDir: true, releaseNS: "apps"})
	docs[2].Metadata.Name = "r"
	text := strings.Join(warningsFor(docs, nil), "\n")
	for _, want := range []string{"other namespaces than the one chosen: kube-system (1)", "ConfigMap/b (post-install)", "Role/r"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if got := warningsFor(parseManifest("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n", "apps"), nil); len(got) != 0 {
		t.Errorf("an ordinary object in the release namespace needs no warning: %v", got)
	}
}

func TestUninstallWarnsAboutHooksThatRunOnRemoval(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.useHostileChart(t)
	h.searched(t)
	h.apply(t, nil)

	rec := h.do(http.MethodPost, "/api/helm/uninstall", map[string]any{"namespace": "apps", "name": "web", "dryRun": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall preview: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "while the release is removed") {
		t.Error("a pre-install hook does not run on removal")
	}

	engine := h.server.helm
	cfg, _ := engine.configFor("dev", "apps")
	stored, err := latest(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	stored.Hooks = append(stored.Hooks, &release.Hook{Kind: "Job", Name: "cleanup", Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: cleanup\n", Events: []release.HookEvent{release.HookPreDelete}})
	if err := cfg.Releases.Update(stored); err != nil {
		t.Fatal(err)
	}
	rec = h.do(http.MethodPost, "/api/helm/uninstall", map[string]any{"namespace": "apps", "name": "web", "dryRun": true})
	if !strings.Contains(rec.Body.String(), "while the release is removed: Job/cleanup (pre-delete)") {
		t.Errorf("a pre-delete hook must be flagged: %s", rec.Body.String())
	}
}

func TestReviewTextIsSafeAndShort(t *testing.T) {
	if got := safeLine("name\u202egnp.exe"); strings.ContainsRune(got, '\u202e') || !strings.Contains(got, "name?gnp.exe") {
		t.Errorf("a right-to-left override must be replaced: %q", got)
	}
	if got := safeLine("a\x00b\nc"); got != "a?b?c" {
		t.Errorf("control characters: %q", got)
	}
	long := safeLine(strings.Repeat("x", 5000))
	if charCount(long) > maxReviewLine+1 || !strings.HasSuffix(long, "…") {
		t.Errorf("a line must be cut: %d characters", charCount(long))
	}

	names := make([]string, 40)
	for i := range names {
		names[i] = "n"
	}
	if got := listNames(names); !strings.HasSuffix(got, "and 28 more") {
		t.Errorf("a long list must be cut: %q", got)
	}
	if got := listNames([]string{"a", "b"}); got != "a, b" {
		t.Errorf("a short list is kept: %q", got)
	}
}

// ---- secrets in responses --------------------------------------------------------------------------------------------------

func TestSecretLookingSettingsAreHiddenWhateverTheyAreCalled(t *testing.T) {
	for path, secret := range map[string]bool{
		"license.key": true, "auth.pwd": true, "admin.password": true, "db.connectionString": true, "x.apiKey": true,
		"tls.cert": true, "session.salt": true, "oidc.clientSecret": true, "jwt.signing": true, "encryption.key": true,
		"service.type": false, "replicaCount": false, "image.tag": false, "monkey.mode": false,
	} {
		if got := looksSecret(path, "value"); got != secret {
			t.Errorf("looksSecret(%q) = %v; want %v", path, got, secret)
		}
	}
	if !looksSecret("db.url", "postgres://app:hunter22@db.internal/prod") {
		t.Error("a URL with a user and password in it must be hidden")
	}
	if looksSecret("db.url", "postgres://db.internal/prod") {
		t.Error("a URL without credentials is not a secret")
	}
}

func TestSettingsWithSecretsNeverLeaveInAnyResponse(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.useHostileChart(t)
	h.searched(t)

	settings := map[string]any{"license.key": "LIC-4a8c91f2", "db.url": "postgres://app:hunter22@db.internal/prod"}
	preview := h.do(http.MethodPost, "/api/helm/install", hubBody(map[string]any{"dryRun": true, "settings": settings}))
	if preview.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
	}
	reply := decode[hubReply](t, preview)
	body := hubBody(map[string]any{"settings": settings})
	body["digest"], body["confirmPhrase"] = reply.Digest, reply.ConfirmPhrase
	applied := h.do(http.MethodPost, "/api/helm/install", body)
	if applied.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", applied.Code, applied.Body.String())
	}
	detail := h.do(http.MethodGet, "/api/helm/release?namespace=apps&name=web", nil)
	list := h.do(http.MethodGet, "/api/helm/releases?namespace=apps", nil)

	for where, text := range map[string]string{"preview": preview.Body.String(), "apply": applied.Body.String(), "detail": detail.Body.String(), "list": list.Body.String()} {
		for _, secret := range []string{"LIC-4a8c91f2", "hunter22"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s leaks %q: %s", where, secret, text)
			}
		}
	}
	if !strings.Contains(reply.Equivalent, "--set license.key=<secret>") || !strings.Contains(reply.Equivalent, "--set db.url=<secret>") {
		t.Errorf("the equivalent command should show the placeholder: %s", reply.Equivalent)
	}
}

// ---- how much a chart may cost to open ----------------------------------------------------------------------------------------

func TestADecompressionBombIsRefusedBeforeItIsRendered(t *testing.T) {
	if loader.MaxDecompressedChartSize != maxDecompressedChart {
		t.Fatalf("the decompression cap is %d, want %d", loader.MaxDecompressedChartSize, maxDecompressedChart)
	}
	zeros := bytes.Repeat([]byte{0}, 4<<20) // under Helm's own 5 MiB per file
	bomb := &chart.Chart{Metadata: &chart.Metadata{APIVersion: "v2", Name: "bomb", Version: "1.0.0"}}
	for i := 0; i < 13; i++ { // 52 MiB in all: well over the cap, and a few KiB when compressed
		bomb.Files = append(bomb.Files, &chart.File{Name: "blob" + string(rune('a'+i)) + ".bin", Data: zeros})
	}
	data, _ := packChart(t, bomb)
	if len(data) > maxChartArchive {
		t.Fatalf("the test archive is %d bytes; it should be tiny", len(data))
	}
	if _, err := loader.LoadArchive(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("a chart that expands past the cap must be refused, got %v", err)
	}
}

func TestOnlyAFewChartsFromSearchAreHandledAtOnce(t *testing.T) {
	previous := slotWait
	slotWait = 30 * time.Millisecond
	defer func() { slotWait = previous }()

	engine := newHelmEngine(&Server{})
	var frees []func()
	for i := 0; i < maxHubWork; i++ {
		free, err := engine.acquire(context.Background())
		if err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
		frees = append(frees, free)
	}
	_, err := engine.acquire(context.Background())
	var api *apiError
	if !errors.As(err, &api) || api.status != http.StatusServiceUnavailable || api.code != "busy" {
		t.Fatalf("a fourth chart must be told the console is busy, got %v", err)
	}
	frees[0]()
	free, err := engine.acquire(context.Background())
	if err != nil {
		t.Fatalf("a freed slot must be usable: %v", err)
	}
	free()
}

func TestAPreviewIsTurnedAwayWhenTheConsoleIsBusyWithCharts(t *testing.T) {
	previous := slotWait
	slotWait = 30 * time.Millisecond
	defer func() { slotWait = previous }()

	h := newHubEnv(t, Options{})
	h.searched(t)
	for i := 0; i < maxHubWork; i++ {
		h.server.helm.slots <- struct{}{}
	}
	rec := h.do(http.MethodPost, "/api/helm/install", hubBody(map[string]any{"dryRun": true}))
	if got := decode[errorResponse](t, rec); rec.Code != http.StatusServiceUnavailable || got.Code != "busy" {
		t.Errorf("busy console: %d %+v", rec.Code, got)
	}
}

// ---- gates come before the network ------------------------------------------------------------------------------------------------

func TestAuthorizationAndTheTypedPhraseComeBeforeAnyNetworkCall(t *testing.T) {
	for name, tc := range map[string]struct {
		opts   Options
		body   map[string]any
		status int
		code   string
	}{
		"read-only":                           {Options{ReadOnly: true}, hubBody(map[string]any{"confirmPhrase": "demo", "digest": "x"}), http.StatusForbidden, "read_only"},
		"production without the context name": {Options{}, hubBody(map[string]any{"context": "prod-eu", "confirmPhrase": "demo", "digest": "x"}), http.StatusConflict, "confirmation_required"},
		"production with a wrong-case name":   {Options{}, hubBody(map[string]any{"context": "prod-eu", "confirm": "PROD-EU", "confirmPhrase": "demo", "digest": "x"}), http.StatusConflict, "confirmation_required"},
		"production with padding":             {Options{}, hubBody(map[string]any{"context": "prod-eu", "confirm": " prod-eu ", "confirmPhrase": "demo", "digest": "x"}), http.StatusConflict, "confirmation_required"},
		"no typed phrase":                     {Options{}, hubBody(nil), http.StatusConflict, "phrase_required"},
		"a phrase in another case":            {Options{}, hubBody(map[string]any{"confirmPhrase": "DEMO", "digest": "x"}), http.StatusConflict, "phrase_required"},
		"a phrase with padding":               {Options{}, hubBody(map[string]any{"confirmPhrase": "demo ", "digest": "x"}), http.StatusConflict, "phrase_required"},
	} {
		h := newHubEnv(t, tc.opts)
		h.searched(t)
		h.srv.Close() // any call to Artifact Hub or the chart host from here on would fail with a 502
		h.archive.Close()

		rec := h.do(http.MethodPost, "/api/helm/install", tc.body)
		if got := decode[errorResponse](t, rec); rec.Code != tc.status || got.Code != tc.code {
			t.Errorf("%s: %d %+v; want %d %s (a 502 would mean the network was used first)", name, rec.Code, got, tc.status, tc.code)
		}
	}
}

func TestCatalogOnlyAlsoBlocksUpgradingAReleaseFromSearchButNotRemovingIt(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	h.apply(t, nil)

	h.server.opts.CatalogOnly = true
	if rec := h.upgrade(map[string]any{"dryRun": true}); rec.Code != http.StatusForbidden || decode[errorResponse](t, rec).Code != "catalog_only" {
		t.Errorf("upgrade in catalog-only mode: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodGet, "/api/helm/search?q=demo", nil); rec.Code != http.StatusForbidden {
		t.Errorf("search in catalog-only mode: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/helm/install", hubBody(map[string]any{"dryRun": true})); rec.Code != http.StatusForbidden {
		t.Errorf("hub install in catalog-only mode: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/helm/uninstall", map[string]any{"namespace": "apps", "name": "web"}); rec.Code != http.StatusOK {
		t.Errorf("removing what the console installed must still work: %d %s", rec.Code, rec.Body.String())
	}
}

// ---- managed-ness comes from the stored release, never from the request ----------------------------------------------------------------

func TestAReleaseIsManagedOnlyByItsStoredLabels(t *testing.T) {
	h := newHubEnv(t, Options{})
	cfg, _ := h.server.helm.configFor("dev", "apps")
	store := func(name string, labels map[string]string) {
		t.Helper()
		rel := &release.Release{
			Name: name, Namespace: "apps", Version: 1, Labels: labels,
			Info: &release.Info{Status: release.StatusDeployed}, Chart: testChart("1.0.0", false),
			Manifest: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n",
		}
		if err := cfg.Releases.Create(rel); err != nil {
			t.Fatal(err)
		}
	}
	store("plain", nil)
	store("half", map[string]string{helmManagedLabel: "true"})                                  // managed, but from nowhere
	store("nopackage", map[string]string{helmManagedLabel: "true", helmSourceLabel: sourceHub}) // hub, but no record of the package
	store("badrepo", map[string]string{helmManagedLabel: "true", helmSourceLabel: sourceHub, helmPackageLabel: idDemo, helmHubRepoLabel: "../x", helmHubNameLabel: "demo"})

	for name, want := range map[string]struct {
		status int
		code   string
	}{
		"plain": {http.StatusForbidden, "not_managed"}, "half": {http.StatusConflict, "unknown_app"},
		"nopackage": {http.StatusConflict, "unknown_app"}, "badrepo": {http.StatusConflict, "unknown_app"},
	} {
		// Whatever the request claims about its source, the stored release decides.
		for _, source := range []string{"hub", "catalog", ""} {
			rec := h.upgrade(map[string]any{"name": name, "dryRun": true, "source": source})
			if got := decode[errorResponse](t, rec); rec.Code != want.status || got.Code != want.code {
				t.Errorf("upgrade of %s claiming %q: %d %+v; want %d %s", name, source, rec.Code, got, want.status, want.code)
			}
		}
	}
	if rec := h.do(http.MethodPost, "/api/helm/uninstall", map[string]any{"namespace": "apps", "name": "plain"}); rec.Code != http.StatusForbidden {
		t.Errorf("uninstall of a release the console did not install: %d", rec.Code)
	}
}

func TestInstallCannotInjectLabelsThroughItsInputs(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)

	for name, extra := range map[string]map[string]any{
		"name":     {"name": "web,k8scli.io/managed=true"},
		"settings": {"settings": map[string]any{"labels.k8scli.io/managed": "true"}},
		"unknown":  {"labels": map[string]any{"k8scli.io/managed": "false"}},
	} {
		body := hubBody(extra)
		body["dryRun"] = true
		if rec := h.do(http.MethodPost, "/api/helm/install", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}

	reply := h.apply(t, nil)
	cfg, _ := h.server.helm.configFor("dev", "apps")
	stored, err := latest(cfg, reply.Name)
	if err != nil {
		t.Fatal(err)
	}
	want := hubLabels(hubRef{repo: "acme", name: "demo"}, idDemo)
	for key, value := range want {
		if stored.Labels[key] != value {
			t.Errorf("label %s = %q, want %q", key, stored.Labels[key], value)
		}
	}
}

// ---- the archive that is hashed is the archive that is installed -----------------------------------------------------------------------

func TestTheInstalledChartIsTheOneThatWasHashed(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	preview := h.preview(t, nil)

	// The chart host starts serving something else after the preview: the digest the person
	// reviewed no longer matches, so nothing is installed.
	other, _ := packChart(t, hostileChart())
	h.archives["/demo-1.0.0.tgz"] = other
	h.hub.mu.Lock()
	delete(h.hub.details["acme/demo"], "digest") // Artifact Hub is silent, so only the reviewed digest protects
	h.hub.mu.Unlock()

	body := hubBody(nil)
	body["digest"], body["confirmPhrase"] = preview.Digest, preview.ConfirmPhrase
	rec := h.do(http.MethodPost, "/api/helm/install", body)
	if got := decode[errorResponse](t, rec); rec.Code != http.StatusConflict || got.Code != "digest_changed" {
		t.Fatalf("a changed archive: %d %+v", rec.Code, got)
	}
	if rels := h.releases(t, "apps"); len(rels) != 0 {
		t.Errorf("nothing may be installed: %v", rels)
	}
}

// ---- catalog integrity ------------------------------------------------------------------------------------------------------------------------

func TestEveryCatalogFieldRefusesTemplateActions(t *testing.T) {
	for _, e := range helmCatalog {
		for _, f := range e.Fields {
			if f.Type != fieldString {
				continue
			}
			if err := f.validate("{{ lookup }}"); err == nil {
				t.Errorf("%s/%s accepts a template action", e.ID, f.Key)
			}
		}
	}
}

// The transport test above proves the filter works; this proves the hub flows actually use it, and
// that the reviewed, pinned catalog does not pay for it.
func TestOnlyChartsFromSearchRenderBlindToSecrets(t *testing.T) {
	h := newHubEnv(t, Options{})
	engine := h.server.helm
	inner := engine.configFor
	var made []*action.Configuration
	engine.configFor = func(contextName, namespace string) (*action.Configuration, error) {
		cfg, err := inner(contextName, namespace)
		made = append(made, cfg)
		return cfg, err
	}
	blind := func(cfg *action.Configuration) bool {
		_, ok := cfg.RESTClientGetter.(secretBlindGetter)
		return ok
	}

	h.searched(t)
	h.apply(t, nil)
	if len(made) == 0 || !blind(made[0]) {
		t.Fatal("an install from search must render with the Secret-blind client")
	}

	made = nil
	h.setDetail("acme/demo/1.1.0", "1.1.0", h.digest11, "/demo-1.1.0.tgz")
	if rec := h.upgrade(map[string]any{"version": "1.1.0", "dryRun": true}); rec.Code != http.StatusOK {
		t.Fatalf("upgrade preview: %d %s", rec.Code, rec.Body.String())
	}
	if len(made) == 0 || !blind(made[0]) {
		t.Error("an upgrade of a release from search must render with the Secret-blind client")
	}

	made = nil
	catalog := newHelmEnv(t, Options{})
	engine = catalog.server.helm
	inner = engine.configFor
	engine.configFor = func(contextName, namespace string) (*action.Configuration, error) {
		cfg, err := inner(contextName, namespace)
		made = append(made, cfg)
		return cfg, err
	}
	catalog.install(t, nil)
	for _, cfg := range made {
		if blind(cfg) {
			t.Error("the catalog is reviewed and pinned: it keeps the normal client")
		}
	}
}
