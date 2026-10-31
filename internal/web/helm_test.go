package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/kube"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	helmtime "helm.sh/helm/v3/pkg/time"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

const secretPassword = "s3cr3t-Passw0rd"

// testChart builds a chart that exercises what the console has to handle:
// a cluster-wide ClusterRole, a Secret, a CRD in crds/, a Deployment, notes that
// print the password, and a template that fails quoting it.
func testChart(version string, withExtra bool) *chart.Chart {
	file := func(name, data string) *chart.File { return &chart.File{Name: name, Data: []byte(data)} }
	templates := []*chart.File{
		file("templates/deployment.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
spec:
  replicas: {{ .Values.replicaCount }}
  selector:
    matchLabels:
      app: {{ .Release.Name }}
  template:
    metadata:
      labels:
        app: {{ .Release.Name }}
    spec:
      containers:
      - name: app
        image: {{ .Values.image.repository }}:{{ .Values.image.tag }}
        env:
        - name: MESSAGE
          value: {{ .Values.message | quote }}
`),
		file("templates/rbac.yaml", `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ .Release.Name }}-reader
rules: []
`),
		file("templates/secret.yaml", `apiVersion: v1
kind: Secret
metadata:
  name: {{ .Release.Name }}-auth
data:
  password: {{ .Values.auth.password | b64enc | quote }}
`),
		file("templates/fail.yaml", `{{ if .Values.feature.enabled }}{{ fail (printf "feature needs more than the password %s" .Values.auth.password) }}{{ end }}`),
		file("templates/NOTES.txt", `The admin password is {{ .Values.auth.password }}.`),
	}
	if withExtra {
		templates = append(templates, file("templates/extra.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-extra
`))
	}
	return &chart.Chart{
		Metadata:  &chart.Metadata{APIVersion: "v2", Name: "demo", Version: version, AppVersion: "9.9"},
		Templates: templates,
		Files: []*chart.File{file("crds/demo.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.demo.example.com
`)},
		// chartutil.Save writes the raw values.yaml, not the parsed map.
		Raw: []*chart.File{file("values.yaml", "replicaCount: 1\nmessage: \"\"\nauth:\n  password: \"\"\nfeature:\n  enabled: false\nimage:\n  repository: nginx\n  tag: latest\nservice:\n  type: ClusterIP\n  port: 80\nports:\n  - 80\nextraObjects: []\nnothing: null\n")},
		Values: map[string]any{
			"replicaCount": 1,
			"message":      "",
			"auth":         map[string]any{"password": ""},
			"feature":      map[string]any{"enabled": false},
			"image":        map[string]any{"repository": "nginx", "tag": "latest"},
			"service":      map[string]any{"type": "ClusterIP", "port": float64(80)},
			"ports":        []any{float64(80)},
			"extraObjects": []any{},
			"nothing":      nil,
		},
	}
}

func packChart(t *testing.T, c *chart.Chart) ([]byte, string) {
	t.Helper()
	path, err := chartutil.Save(c, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}

// helmEnv is an env whose Helm engine uses an in-memory release store, a
// printing kube client and a local HTTPS server that serves the test chart.
type helmEnv struct {
	*env
	mem      *driver.Memory
	archive  *httptest.Server  // serves the chart archives over HTTPS
	archives map[string][]byte // path -> archive
	digest11 string            // sha256 of the 1.1.0 archive
	entry    helmEntry
	// kubeClient is what the Helm configurations talk to; tests swap it to simulate a cluster.
	kubeClient func() kube.Interface
}

func newHelmEnv(t *testing.T, opts Options) *helmEnv {
	t.Helper()

	e := newEnv(t, opts)
	h := &helmEnv{env: e, mem: driver.NewMemory(), archives: map[string][]byte{}}
	h.kubeClient = func() kube.Interface { return &kubefake.PrintingKubeClient{Out: io.Discard} }

	v100, d100 := packChart(t, testChart("1.0.0", false))
	v110, d110 := packChart(t, testChart("1.1.0", true))
	h.archives["/demo-1.0.0.tgz"], h.archives["/demo-1.1.0.tgz"] = v100, v110
	h.digest11 = d110

	h.archive = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := h.archives[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(h.archive.Close)

	replicas := replicasField("replicaCount", 5, "Replicas")
	h.entry = helmEntry{
		ID: "demo", Name: "Demo", Category: "Test", Description: "A test app",
		Chart: "demo", Version: "1.0.0", AppVersion: "9.9", Repo: "https://example.test/charts",
		archiveURL: h.archive.URL + "/demo-1.0.0.tgz", digest: d100,
		DefaultName: "demo", DefaultNamespace: "apps", CreateNamespace: true, ClusterScoped: true,
		Fields: []helmField{
			replicas,
			{Key: "message", Label: "Greeting", Type: fieldString, Max: 20, path: "message"},
			{Key: "password", Label: "Password", Type: fieldString, Max: 64, Secret: true, path: "auth.password"},
			{Key: "feature", Label: "Feature", Type: fieldBool, Default: "false", path: "feature.enabled"},
			serviceTypeField("service.type", "ClusterIP", "ClusterIP", "NodePort"),
		},
	}

	getter := genericclioptions.NewConfigFlags(false)
	kubeconfigPath := e.kubeconfig
	getter.KubeConfig = &kubeconfigPath

	engine := e.server.helm
	engine.catalog = []helmEntry{h.entry}
	engine.httpClient = h.archive.Client()
	engine.configFor = func(_, namespace string) (*action.Configuration, error) {
		h.mem.SetNamespace(namespace) // "" means every namespace, for listing
		return &action.Configuration{
			RESTClientGetter: getter,
			Releases:         storage.Init(h.mem),
			KubeClient:       h.kubeClient(),
			Capabilities:     chartutil.DefaultCapabilities.Copy(),
			Log:              func(string, ...any) {},
		}, nil
	}
	return h
}

// catalogTo1_1 moves the catalog entry to chart 1.1.0, like a maintainer's update.
func (h *helmEnv) catalogTo1_1() {
	h.entry.Version = "1.1.0"
	h.entry.archiveURL = h.archive.URL + "/demo-1.1.0.tgz"
	h.entry.digest = h.digest11
	h.server.helm.catalog = []helmEntry{h.entry}
	h.server.helm.mu.Lock()
	h.server.helm.archives = map[string][]byte{}
	h.server.helm.mu.Unlock()
}

func installBody(extra map[string]any) map[string]any {
	body := map[string]any{"catalogId": "demo", "name": "web", "namespace": "apps", "createNamespace": true}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (h *helmEnv) releases(t *testing.T, namespace string) []helmReleaseJSON {
	t.Helper()
	rec := h.do(http.MethodGet, "/api/helm/releases?namespace="+namespace, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("releases: %d %s", rec.Code, rec.Body.String())
	}
	return decode[[]helmReleaseJSON](t, rec)
}

// install runs a real install and fails the test if it does not succeed.
func (h *helmEnv) install(t *testing.T, fields map[string]any) {
	t.Helper()
	rec := h.do(http.MethodPost, "/api/helm/install", installBody(map[string]any{"fields": fields}))
	if rec.Code != http.StatusOK {
		t.Fatalf("install: %d %s", rec.Code, rec.Body.String())
	}
}

func noSecret(t *testing.T, where string, text string) {
	t.Helper()
	if strings.Contains(text, secretPassword) {
		t.Errorf("%s contains the secret:\n%s", where, text)
	}
}

// ---- catalog ------------------------------------------------------------------------

func TestCatalogIsServedWithoutTheArchiveAddressOrDigest(t *testing.T) {
	h := newHelmEnv(t, Options{})
	rec := h.do(http.MethodGet, "/api/helm/catalog", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, leaked := range []string{h.entry.digest, h.archive.URL, "archiveURL", "digest"} {
		if strings.Contains(body, leaked) {
			t.Errorf("the catalog leaks %q:\n%s", leaked, body)
		}
	}
	entries := decode[[]helmEntry](t, rec)
	if len(entries) != 1 || entries[0].ID != "demo" || len(entries[0].Fields) != 5 {
		t.Errorf("entries = %+v", entries)
	}
}

func TestBuiltInCatalogIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	digest := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, e := range helmCatalog {
		if e.ID == "" || seen[e.ID] {
			t.Errorf("entry %q: empty or duplicated id", e.ID)
		}
		seen[e.ID] = true
		if !strings.HasPrefix(e.Repo, "https://") || !strings.HasPrefix(e.archiveURL, "https://") {
			t.Errorf("%s: the repository and the archive must be HTTPS (%s, %s)", e.ID, e.Repo, e.archiveURL)
		}
		if !digest.MatchString(e.digest) {
			t.Errorf("%s: digest %q is not a sha256", e.ID, e.digest)
		}
		if e.Chart == "" || e.Version == "" || e.Name == "" || e.DefaultName == "" || e.DefaultNamespace == "" {
			t.Errorf("%s: missing basic fields", e.ID)
		}
		if err := validReleaseName(e.DefaultName); err != nil {
			t.Errorf("%s: default name: %v", e.ID, err)
		}
		if _, err := validNamespace(e.DefaultNamespace); err != nil {
			t.Errorf("%s: default namespace: %v", e.ID, err)
		}
		if !strings.Contains(e.archiveURL, e.Version) {
			t.Errorf("%s: the archive URL %s does not mention version %s", e.ID, e.archiveURL, e.Version)
		}
		keys := map[string]bool{}
		for _, f := range e.Fields {
			if keys[f.Key] || f.Key == "" || f.Label == "" {
				t.Errorf("%s: field %q is empty or duplicated", e.ID, f.Key)
			}
			keys[f.Key] = true
			if f.path == "" && f.apply == nil {
				t.Errorf("%s.%s: no chart value to write", e.ID, f.Key)
			}
			if f.Type == fieldSelect && len(f.Options) == 0 {
				t.Errorf("%s.%s: a select without options", e.ID, f.Key)
			}
			if f.Secret && f.Type != fieldString {
				t.Errorf("%s.%s: only text can be secret", e.ID, f.Key)
			}
			if f.Default != "" || f.Type == fieldBool || f.Type == fieldInt || f.Type == fieldSelect {
				if err := f.validate(f.Default); err != nil {
					t.Errorf("%s.%s: the default %q is not valid: %v", e.ID, f.Key, f.Default, err)
				}
			}
		}
	}
	for _, required := range []string{"podinfo", "metrics-server"} {
		if !seen[required] {
			t.Errorf("the catalog has no %s entry", required)
		}
	}
}

// ---- install --------------------------------------------------------------------------

func TestInstallDryRunInstallsNothingAndListsWhatItWouldCreate(t *testing.T) {
	h := newHelmEnv(t, Options{})
	rec := h.do(http.MethodPost, "/api/helm/install", installBody(map[string]any{
		"dryRun": true, "fields": map[string]any{"replicas": 3, "password": secretPassword, "message": "hi"},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	noSecret(t, "the install preview", rec.Body.String())

	got := decode[changeResponse](t, rec)
	if got.Action != "created" || !got.DryRun || got.Kind != "HelmRelease" || got.Name != "web" || got.Namespace != "apps" {
		t.Errorf("response = %+v", got)
	}
	joined := strings.Join(got.Changes, "\n")
	for _, want := range []string{
		"Deployment/web (apps)", "Secret/web-auth (apps)", "ClusterRole/web-reader",
		"CustomResourceDefinition/widgets.demo.example.com",
		"warning: creates cluster-wide objects", "ClusterRole (1)", "CustomResourceDefinition (1)", "warning: creates 1 Secret",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("changes lack %q:\n%s", want, joined)
		}
	}
	for _, want := range []string{"helm install web demo --repo https://example.test/charts --version 1.0.0 -n apps --create-namespace",
		"--set replicaCount=3", "--set auth.password=<secret>"} {
		if !strings.Contains(got.Equivalent, want) {
			t.Errorf("equivalent lacks %q: %s", want, got.Equivalent)
		}
	}
	if n := len(h.releases(t, "_all")); n != 0 {
		t.Errorf("a dry run stored %d release(s)", n)
	}
}

func TestInstallCreatesAManagedRelease(t *testing.T) {
	h := newHelmEnv(t, Options{})
	rec := h.do(http.MethodPost, "/api/helm/install", installBody(map[string]any{
		"fields": map[string]any{"replicas": 2, "password": secretPassword},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	noSecret(t, "the install answer", rec.Body.String())
	if got := decode[changeResponse](t, rec); got.DryRun || got.Action != "created" || !strings.Contains(got.Summary, "installed") {
		t.Errorf("response = %+v", got)
	}

	releases := h.releases(t, "apps")
	if len(releases) != 1 {
		t.Fatalf("releases = %+v", releases)
	}
	rel := releases[0]
	if !rel.Managed || rel.CatalogID != "demo" || rel.Name != "web" || rel.Chart != "demo" || rel.ChartVersion != "1.0.0" ||
		rel.Status != "deployed" || rel.Health != "ok" || rel.Revision != 1 {
		t.Errorf("release = %+v", rel)
	}
}

func TestInstallRefusesATakenNameEvenInThePreview(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.install(t, nil)

	for _, dry := range []bool{true, false} {
		rec := h.do(http.MethodPost, "/api/helm/install", installBody(map[string]any{"dryRun": dry}))
		if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "already_exists" {
			t.Errorf("dryRun=%v: status %d: %s", dry, rec.Code, rec.Body.String())
		}
	}
}

func TestInstallDefaultsComeFromTheCatalog(t *testing.T) {
	h := newHelmEnv(t, Options{})
	rec := h.do(http.MethodPost, "/api/helm/install", map[string]any{"catalogId": "demo", "dryRun": true})
	got := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || got.Name != "demo" || got.Namespace != "apps" {
		t.Errorf("status %d: %+v", rec.Code, got)
	}
}

func TestInstallGuards(t *testing.T) {
	// Read-only blocks the real change, not the preview.
	h := newHelmEnv(t, Options{ReadOnly: true})
	if rec := h.do(http.MethodPost, "/api/helm/install", installBody(nil)); rec.Code != http.StatusForbidden || decode[errorResponse](t, rec).Code != "read_only" {
		t.Errorf("read-only install: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodPost, "/api/helm/install", installBody(map[string]any{"dryRun": true})); rec.Code != http.StatusOK {
		t.Errorf("read-only preview: %d %s", rec.Code, rec.Body.String())
	}
	if n := len(h.releases(t, "_all")); n != 0 {
		t.Errorf("read-only mode installed %d release(s)", n)
	}

	// A production-looking context needs the context name typed.
	p := newHelmEnv(t, Options{})
	prod := installBody(map[string]any{"context": "prod-eu"})
	if rec := p.do(http.MethodPost, "/api/helm/install", prod); rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "confirmation_required" {
		t.Errorf("no confirmation: %d %s", rec.Code, rec.Body.String())
	}
	prod["confirm"] = "wrong"
	if rec := p.do(http.MethodPost, "/api/helm/install", prod); rec.Code != http.StatusConflict {
		t.Errorf("wrong confirmation: %d", rec.Code)
	}
	if n := len(p.releases(t, "_all")); n != 0 {
		t.Fatalf("installed %d release(s) without confirmation", n)
	}
	prod["confirm"] = "prod-eu"
	if rec := p.do(http.MethodPost, "/api/helm/install", prod); rec.Code != http.StatusOK {
		t.Errorf("confirmed install: %d %s", rec.Code, rec.Body.String())
	}
	dry := installBody(map[string]any{"context": "prod-eu", "dryRun": true, "name": "other"})
	if rec := p.do(http.MethodPost, "/api/helm/install", dry); rec.Code != http.StatusOK {
		t.Errorf("a preview needs no confirmation: %d %s", rec.Code, rec.Body.String())
	}
}

func TestInstallValidation(t *testing.T) {
	h := newHelmEnv(t, Options{})
	cases := map[string]map[string]any{
		"unknown app":           {"catalogId": "evil"},
		"no app":                {"catalogId": ""},
		"unknown field":         {"fields": map[string]any{"values": "x"}},
		"a values file":         {"fields": map[string]any{"set": "image.repository=evil"}},
		"int too big":           {"fields": map[string]any{"replicas": 6}},
		"int too small":         {"fields": map[string]any{"replicas": 0}},
		"int not whole":         {"fields": map[string]any{"replicas": 1.5}},
		"int not a number":      {"fields": map[string]any{"replicas": "many"}},
		"bool not a bool":       {"fields": map[string]any{"feature": "maybe"}},
		"select not an option":  {"fields": map[string]any{"serviceType": "ExternalName"}},
		"string too long":       {"fields": map[string]any{"message": strings.Repeat("a", 21)}},
		"string with control":   {"fields": map[string]any{"message": "a\nb"}},
		"string invisible":      {"fields": map[string]any{"message": "a\u202eb"}},
		"secret too long":       {"fields": map[string]any{"password": strings.Repeat("a", 65)}},
		"field of a wrong type": {"fields": map[string]any{"message": []string{"a"}}},
		"bad release name":      {"name": "Web_App"},
		"name too long":         {"name": strings.Repeat("a", 54)},
		"bad namespace":         {"namespace": "../etc"},
		"reserved namespace":    {"namespace": "kube-system", "createNamespace": true},
		"unknown request key":   {"repo": "https://evil.example/charts"},
	}
	for name, extra := range cases {
		rec := h.do(http.MethodPost, "/api/helm/install", installBody(extra))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d; want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
	if n := len(h.releases(t, "_all")); n != 0 {
		t.Errorf("invalid requests installed %d release(s)", n)
	}
}

func TestInstallingIntoAnExistingKubeNamespaceIsAllowedWhenNotCreatingIt(t *testing.T) {
	h := newHelmEnv(t, Options{})
	rec := h.do(http.MethodPost, "/api/helm/install", installBody(map[string]any{"namespace": "kube-system", "createNamespace": false, "dryRun": true}))
	if rec.Code != http.StatusOK {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSecretsNeverAppearInAnyResponseOrError(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.install(t, map[string]any{"password": secretPassword})

	// Everything that reads.
	for _, target := range []string{
		"/api/helm/releases?namespace=apps", "/api/helm/releases", "/api/helm/release?namespace=apps&name=web", "/api/helm/catalog",
	} {
		rec := h.do(http.MethodGet, target, nil)
		noSecret(t, "GET "+target, rec.Body.String())
	}

	// The notes print the password; the detail must scrub it.
	detail := decode[helmReleaseDetail](t, h.do(http.MethodGet, "/api/helm/release?namespace=apps&name=web", nil))
	if !strings.Contains(detail.Notes, "<secret>") {
		t.Errorf("notes = %q; want the password replaced by <secret>", detail.Notes)
	}
	if _, ok := detail.Fields["password"]; ok {
		t.Errorf("the detail lists the secret field: %v", detail.Fields)
	}

	// A render that fails and quotes the password in its message.
	for _, dry := range []bool{true, false} {
		rec := h.do(http.MethodPost, "/api/helm/upgrade", map[string]any{
			"namespace": "apps", "name": "web", "dryRun": dry, "fields": map[string]any{"feature": true},
		})
		if rec.Code == http.StatusOK {
			t.Fatalf("the failing chart was accepted (dryRun=%v)", dry)
		}
		noSecret(t, "an upgrade error", rec.Body.String())
	}
	failing := installBody(map[string]any{"name": "other", "fields": map[string]any{"feature": true, "password": secretPassword}})
	for _, dry := range []bool{true, false} {
		failing["dryRun"] = dry
		rec := h.do(http.MethodPost, "/api/helm/install", failing)
		if rec.Code == http.StatusOK {
			t.Fatalf("the failing chart was accepted (dryRun=%v)", dry)
		}
		noSecret(t, "an install error", rec.Body.String())
	}

	// Changing the secret: the preview says "changed" and never the value.
	rec := h.do(http.MethodPost, "/api/helm/upgrade", map[string]any{
		"namespace": "apps", "name": "web", "dryRun": true, "fields": map[string]any{"password": secretPassword + "-new"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	noSecret(t, "the upgrade preview", rec.Body.String())
	if strings.Contains(rec.Body.String(), secretPassword+"-new") {
		t.Errorf("the new secret is in the preview: %s", rec.Body.String())
	}
	if got := decode[changeResponse](t, rec); !strings.Contains(strings.Join(got.Changes, "\n"), "Password: changed") ||
		!strings.Contains(got.Equivalent, "auth.password=<secret>") {
		t.Errorf("preview = %+v", got)
	}
}

// ---- chart download --------------------------------------------------------------------

func TestDigestMismatchIsRefusedAndNothingIsInstalled(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.entry.digest = strings.Repeat("0", 64)
	h.server.helm.catalog = []helmEntry{h.entry}

	for _, dry := range []bool{true, false} {
		rec := h.do(http.MethodPost, "/api/helm/install", installBody(map[string]any{"dryRun": dry}))
		if rec.Code != http.StatusBadGateway || decode[errorResponse](t, rec).Code != "digest_mismatch" {
			t.Errorf("dryRun=%v: status %d: %s", dry, rec.Code, rec.Body.String())
		}
	}
	if n := len(h.releases(t, "_all")); n != 0 {
		t.Errorf("a chart that failed the digest check was installed (%d)", n)
	}
}

func TestDownloadRules(t *testing.T) {
	h := newHelmEnv(t, Options{})
	engine := h.server.helm

	try := func(mutate func(*helmEntry)) error {
		entry := h.entry
		mutate(&entry)
		engine.mu.Lock()
		engine.archives = map[string][]byte{}
		engine.mu.Unlock()
		_, err := engine.archive(t.Context(), &entry)
		return err
	}

	if err := try(func(*helmEntry) {}); err != nil {
		t.Fatalf("a good download failed: %v", err)
	}
	if err := try(func(e *helmEntry) { e.archiveURL = strings.Replace(e.archiveURL, "https://", "http://", 1) }); err == nil {
		t.Error("an http:// archive must be refused")
	}
	if err := try(func(e *helmEntry) { e.archiveURL = "https:///nohost.tgz" }); err == nil {
		t.Error("an address without a host must be refused")
	}
	if err := try(func(e *helmEntry) { e.archiveURL = h.archive.URL + "/missing.tgz" }); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a 404 must be reported, got %v", err)
	}

	// A host that sends the client somewhere else is refused.
	evil := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://attacker.example/x.tgz", http.StatusFound)
	}))
	defer evil.Close()
	engine.httpClient = evil.Client()
	engine.httpClient.CheckRedirect = newHelmEngine(h.env.server).httpClient.CheckRedirect
	if err := try(func(e *helmEntry) { e.archiveURL = evil.URL + "/x.tgz" }); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("a redirect to another host must be refused, got %v", err)
	}
	downgrade := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/x.tgz", http.StatusFound)
	}))
	defer downgrade.Close()
	engine.httpClient = downgrade.Client()
	engine.httpClient.CheckRedirect = newHelmEngine(h.env.server).httpClient.CheckRedirect
	if err := try(func(e *helmEntry) { e.archiveURL = downgrade.URL + "/x.tgz" }); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Errorf("a redirect to http must be refused, got %v", err)
	}
}

func TestOversizedDownloadIsRefused(t *testing.T) {
	h := newHelmEnv(t, Options{})
	big := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 1<<20)
		for range maxChartArchive>>20 + 2 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer big.Close()
	engine := h.server.helm
	engine.httpClient = big.Client()
	entry := h.entry
	entry.archiveURL = big.URL + "/big.tgz"
	if _, err := engine.archive(t.Context(), &entry); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v", err)
	}
}

func TestChartMustBeTheOneTheCatalogNames(t *testing.T) {
	h := newHelmEnv(t, Options{})
	// A valid archive (digest matches) of another chart version than the entry says.
	h.entry.Version = "9.9.9"
	h.server.helm.catalog = []helmEntry{h.entry}
	rec := h.do(http.MethodPost, "/api/helm/install", installBody(map[string]any{"dryRun": true}))
	if rec.Code == http.StatusOK {
		t.Errorf("an archive that is not the pinned version was accepted: %s", rec.Body.String())
	}
}

func TestHostAllowedForRedirects(t *testing.T) {
	for host, want := range map[string]bool{
		"charts.example.com":                   true, // the origin itself
		"github.com":                           true,
		"objects.githubusercontent.com":        true,
		"release-assets.githubusercontent.com": true,
		"evilgithub.com":                       false,
		"github.com.evil.example":              false,
		"attacker.example":                     false,
	} {
		if got := hostAllowed(host, "charts.example.com"); got != want {
			t.Errorf("hostAllowed(%q) = %v; want %v", host, got, want)
		}
	}
}

// ---- releases (read) ---------------------------------------------------------------------

// foreignRelease stores a release the way the helm command line would: no k8scli labels.
func (h *helmEnv) foreignRelease(t *testing.T, name, namespace string) {
	t.Helper()
	cfg, _ := h.server.helm.configFor("dev", namespace)
	rel := &release.Release{
		Name: name, Namespace: namespace, Version: 1,
		Info:     &release.Info{Status: release.StatusDeployed, LastDeployed: helmtime.Now()},
		Chart:    testChart("1.0.0", false),
		Manifest: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: foreign\n",
	}
	if err := cfg.Releases.Create(rel); err != nil {
		t.Fatal(err)
	}
}

func TestReleasesListEveryReleaseAndFlagTheManagedOnes(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.install(t, nil)
	h.foreignRelease(t, "legacy", "tools")

	all := h.releases(t, "_all")
	if len(all) != 2 {
		t.Fatalf("releases = %+v", all)
	}
	// Sorted by namespace, then name.
	if all[0].Namespace != "apps" || all[0].Name != "web" || !all[0].Managed || all[1].Name != "legacy" || all[1].Managed {
		t.Errorf("releases = %+v", all)
	}
	if only := h.releases(t, "tools"); len(only) != 1 || only[0].Name != "legacy" {
		t.Errorf("the namespace filter returned %+v", only)
	}
	if rec := h.do(http.MethodGet, "/api/helm/releases?namespace=Bad_NS", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad namespace = %d", rec.Code)
	}
}

func TestReleaseDetail(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.install(t, map[string]any{"replicas": 3, "message": "hello"})

	detail := decode[helmReleaseDetail](t, h.do(http.MethodGet, "/api/helm/release?namespace=apps&name=web", nil))
	if detail.Name != "web" || !detail.Managed || detail.Status != "deployed" || detail.AppVersion != "9.9" {
		t.Errorf("detail = %+v", detail.helmReleaseJSON)
	}
	if detail.Fields["replicas"] != "3" || detail.Fields["message"] != "hello" || detail.Fields["feature"] != "false" {
		t.Errorf("fields = %v", detail.Fields)
	}
	kinds := map[string]bool{}
	for _, r := range detail.Resources {
		kinds[r.Kind] = true
	}
	if !kinds["Deployment"] || !kinds["Secret"] || !kinds["ClusterRole"] {
		t.Errorf("resources = %+v", detail.Resources)
	}
	if len(detail.History) != 1 || detail.History[0].Revision != 1 || detail.History[0].Chart != "demo-1.0.0" {
		t.Errorf("history = %+v", detail.History)
	}

	if rec := h.do(http.MethodGet, "/api/helm/release?namespace=apps&name=nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown release = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodGet, "/api/helm/release?namespace=apps&name=", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("no name = %d", rec.Code)
	}
}

func TestForeignReleasesCanBeReadButShowNoFields(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.foreignRelease(t, "legacy", "tools")
	detail := decode[helmReleaseDetail](t, h.do(http.MethodGet, "/api/helm/release?namespace=tools&name=legacy", nil))
	if detail.Managed || len(detail.Fields) != 0 {
		t.Errorf("detail = %+v", detail)
	}
}

// ---- upgrade -------------------------------------------------------------------------------

func TestUpgradeOnlyTouchesReleasesTheConsoleInstalled(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.foreignRelease(t, "legacy", "tools")

	for _, dry := range []bool{true, false} {
		rec := h.do(http.MethodPost, "/api/helm/upgrade", map[string]any{
			"namespace": "tools", "name": "legacy", "dryRun": dry, "fields": map[string]any{"replicas": 2},
		})
		if rec.Code != http.StatusForbidden || decode[errorResponse](t, rec).Code != "not_managed" {
			t.Errorf("dryRun=%v: status %d: %s", dry, rec.Code, rec.Body.String())
		}
	}
	if rec := h.do(http.MethodPost, "/api/helm/upgrade", map[string]any{"namespace": "apps", "name": "nope"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown release = %d", rec.Code)
	}
}

func TestUpgradeShowsTheFieldDiffAndKeepsEverythingElse(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.install(t, map[string]any{"replicas": 2, "message": "hello", "password": secretPassword})

	body := map[string]any{"namespace": "apps", "name": "web", "dryRun": true, "fields": map[string]any{"replicas": 4}}
	rec := h.do(http.MethodPost, "/api/helm/upgrade", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := decode[changeResponse](t, rec)
	if got.Action != "updated" || !got.DryRun || !strings.Contains(strings.Join(got.Changes, "\n"), `Replicas: "2" -> "4"`) {
		t.Errorf("preview = %+v", got)
	}
	if !strings.Contains(got.Equivalent, "helm upgrade web demo") || !strings.Contains(got.Equivalent, "--set replicaCount=4") || strings.Contains(got.Equivalent, "message") {
		t.Errorf("equivalent = %s", got.Equivalent)
	}
	if rel := h.releases(t, "apps")[0]; rel.Revision != 1 {
		t.Errorf("a preview created revision %d", rel.Revision)
	}

	// Real: the revision moves, the fields not mentioned (message, password) are kept.
	body["dryRun"] = false
	if rec := h.do(http.MethodPost, "/api/helm/upgrade", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rel := h.releases(t, "apps")[0]; rel.Revision != 2 || !rel.Managed || rel.CatalogID != "demo" {
		t.Errorf("release = %+v", rel)
	}
	detail := decode[helmReleaseDetail](t, h.do(http.MethodGet, "/api/helm/release?namespace=apps&name=web", nil))
	if detail.Fields["replicas"] != "4" || detail.Fields["message"] != "hello" {
		t.Errorf("fields = %v", detail.Fields)
	}
	if !strings.Contains(detail.Notes, "<secret>") {
		t.Errorf("the password was lost on upgrade: notes %q", detail.Notes)
	}

	// Setting a text field to nothing clears it.
	clear := map[string]any{"namespace": "apps", "name": "web", "fields": map[string]any{"message": ""}}
	if rec := h.do(http.MethodPost, "/api/helm/upgrade", clear); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if detail := decode[helmReleaseDetail](t, h.do(http.MethodGet, "/api/helm/release?namespace=apps&name=web", nil)); detail.Fields["message"] != "" {
		t.Errorf("message = %q", detail.Fields["message"])
	}
}

func TestUpgradeWithNothingNewIsUnchanged(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.install(t, map[string]any{"replicas": 2})

	rec := h.do(http.MethodPost, "/api/helm/upgrade", map[string]any{"namespace": "apps", "name": "web", "fields": map[string]any{"replicas": 2}})
	if got := decode[changeResponse](t, rec); rec.Code != http.StatusOK || got.Action != "unchanged" {
		t.Errorf("status %d: %+v", rec.Code, got)
	}
	if rel := h.releases(t, "apps")[0]; rel.Revision != 1 {
		t.Errorf("an unchanged upgrade created revision %d", rel.Revision)
	}
}

func TestUpgradeMovesToTheCatalogVersionAndShowsTheResourceDiff(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.install(t, nil)
	h.catalogTo1_1()

	rec := h.do(http.MethodPost, "/api/helm/upgrade", map[string]any{"namespace": "apps", "name": "web", "dryRun": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	joined := strings.Join(decode[changeResponse](t, rec).Changes, "\n")
	if !strings.Contains(joined, "chart: 1.0.0 -> 1.1.0") || !strings.Contains(joined, "+ ConfigMap/web-extra (apps)") {
		t.Errorf("changes:\n%s", joined)
	}

	if rec := h.do(http.MethodPost, "/api/helm/upgrade", map[string]any{"namespace": "apps", "name": "web"}); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rel := h.releases(t, "apps")[0]; rel.ChartVersion != "1.1.0" || rel.Revision != 2 {
		t.Errorf("release = %+v", rel)
	}
}

func TestUpgradeGuards(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.install(t, map[string]any{"replicas": 1})
	body := map[string]any{"namespace": "apps", "name": "web", "fields": map[string]any{"replicas": 3}}

	ro := newHelmEnv(t, Options{ReadOnly: true})
	if rec := ro.do(http.MethodPost, "/api/helm/upgrade", body); rec.Code != http.StatusForbidden || decode[errorResponse](t, rec).Code != "read_only" {
		t.Errorf("read-only upgrade: %d %s", rec.Code, rec.Body.String())
	}

	body["context"] = "prod-eu"
	if rec := h.do(http.MethodPost, "/api/helm/upgrade", body); rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "confirmation_required" {
		t.Errorf("no confirmation: %d %s", rec.Code, rec.Body.String())
	}

	for name, bad := range map[string]map[string]any{
		"unknown field": {"namespace": "apps", "name": "web", "fields": map[string]any{"image": "evil"}},
		"out of range":  {"namespace": "apps", "name": "web", "fields": map[string]any{"replicas": 99}},
		"bad namespace": {"namespace": "A b", "name": "web"},
		"bad name":      {"namespace": "apps", "name": "A b"},
	} {
		if rec := h.do(http.MethodPost, "/api/helm/upgrade", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
}

// ---- uninstall --------------------------------------------------------------------------------

func TestUninstallPreviewListsWhatGoesAndKeepsTheRelease(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.install(t, nil)

	rec := h.do(http.MethodPost, "/api/helm/uninstall", map[string]any{"namespace": "apps", "name": "web", "dryRun": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := decode[changeResponse](t, rec)
	joined := strings.Join(got.Changes, "\n")
	if got.Action != "deleted" || !got.DryRun || !strings.Contains(joined, "- Deployment/web (apps)") ||
		!strings.Contains(joined, "warning: Helm does not delete the 1 CustomResourceDefinitions") || !strings.Contains(joined, "widgets.demo.example.com") {
		t.Errorf("preview = %+v", got)
	}
	if got.Equivalent != "helm uninstall web -n apps" {
		t.Errorf("equivalent = %q", got.Equivalent)
	}
	if n := len(h.releases(t, "apps")); n != 1 {
		t.Errorf("the preview removed the release (%d left)", n)
	}
}

func TestUninstallRemovesTheRelease(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.install(t, nil)

	rec := h.do(http.MethodPost, "/api/helm/uninstall", map[string]any{"namespace": "apps", "name": "web"})
	if got := decode[changeResponse](t, rec); rec.Code != http.StatusOK || got.DryRun || !strings.Contains(got.Summary, "removed") {
		t.Fatalf("status %d: %+v", rec.Code, got)
	}
	if n := len(h.releases(t, "apps")); n != 0 {
		t.Errorf("%d release(s) left", n)
	}
	if rec := h.do(http.MethodPost, "/api/helm/uninstall", map[string]any{"namespace": "apps", "name": "web"}); rec.Code != http.StatusNotFound {
		t.Errorf("uninstalling twice = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUninstallOnlyTouchesReleasesTheConsoleInstalled(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.foreignRelease(t, "legacy", "tools")

	for _, dry := range []bool{true, false} {
		rec := h.do(http.MethodPost, "/api/helm/uninstall", map[string]any{"namespace": "tools", "name": "legacy", "dryRun": dry})
		if rec.Code != http.StatusForbidden || decode[errorResponse](t, rec).Code != "not_managed" {
			t.Errorf("dryRun=%v: status %d: %s", dry, rec.Code, rec.Body.String())
		}
	}
	if n := len(h.releases(t, "tools")); n != 1 {
		t.Errorf("a release the console did not install was removed")
	}
}

func TestUninstallGuards(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.install(t, nil)

	body := map[string]any{"namespace": "apps", "name": "web", "context": "prod-eu"}
	if rec := h.do(http.MethodPost, "/api/helm/uninstall", body); rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "confirmation_required" {
		t.Errorf("no confirmation: %d %s", rec.Code, rec.Body.String())
	}
	if n := len(h.releases(t, "apps")); n != 1 {
		t.Fatalf("the release was removed without confirmation")
	}

	ro := newHelmEnv(t, Options{ReadOnly: true})
	if rec := ro.do(http.MethodPost, "/api/helm/uninstall", map[string]any{"namespace": "apps", "name": "web"}); rec.Code != http.StatusForbidden {
		t.Errorf("read-only uninstall = %d", rec.Code)
	}
}

// ---- concurrency and plumbing -------------------------------------------------------------------

func TestOnlyOneWriteToAReleaseAtATime(t *testing.T) {
	h := newHelmEnv(t, Options{})
	engine := h.server.helm

	done, err := engine.begin("dev", "apps", "web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.begin("dev", "apps", "web"); err == nil || !strings.Contains(err.Error(), "being changed") {
		t.Errorf("a second write to the same release must be refused, got %v", err)
	}
	if _, err := engine.begin("dev", "apps", "other"); err != nil {
		t.Errorf("another release must not be blocked: %v", err)
	}
	done()
	if again, err := engine.begin("dev", "apps", "web"); err != nil {
		t.Errorf("the release stays blocked after it finished: %v", err)
	} else {
		again()
	}
}

func TestFieldHelpers(t *testing.T) {
	for in, want := range map[any]string{"x": "x", true: "true", float64(3): "3", nil: ""} {
		if got, err := normalizeFieldValue(in); err != nil || got != want {
			t.Errorf("normalizeFieldValue(%v) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []any{1.5, []any{"a"}, map[string]any{}} {
		if _, err := normalizeFieldValue(bad); err == nil {
			t.Errorf("normalizeFieldValue(%v) should fail", bad)
		}
	}

	values := map[string]any{}
	setPath(values, "a.b.c", "x")
	if v, ok := getPath(values, "a.b.c"); !ok || v != "x" {
		t.Errorf("getPath = %v, %v", v, ok)
	}
	if _, ok := getPath(values, "a.b.d"); ok {
		t.Error("getPath found a missing key")
	}

	flags := helmSetFlags(map[string]any{
		"auth":   map[string]any{"password": placeholderSecret},
		"args":   []any{"--kubelet-insecure-tls"},
		"labels": map[string]any{"a.b": "x,y"},
		"text":   "it's",
	})
	joined := strings.Join(flags, " | ")
	for _, want := range []string{"--set auth.password=<secret>", `--set 'args[0]=--kubelet-insecure-tls'`, `labels.a\.b=x\,y`, `'text=it'\''s'`} {
		if !strings.Contains(joined, want) {
			t.Errorf("flags lack %q:\n%s", want, joined)
		}
	}
}

func TestAsksForPrivileges(t *testing.T) {
	for doc, want := range map[string]bool{
		"kind: Pod\nspec:\n  containers:\n  - securityContext:\n      privileged: true\n":  true,
		"kind: Pod\nspec:\n  hostNetwork: true\n":                                          true,
		"kind: Pod\nspec:\n  volumes:\n  - hostPath:\n      path: /var/log\n":              true,
		"kind: Pod\nspec:\n  containers:\n  - securityContext:\n      privileged: false\n": false,
		"kind: ConfigMap\ndata:\n  privileged: \"true\"\n":                                 false,
	} {
		docs := parseManifest("---\n"+doc+"metadata:\n  name: x\n", "ns")
		if len(docs) != 1 {
			t.Fatalf("could not parse %q", doc)
		}
		if got := asksForPrivileges(docs[0].raw); got != want {
			t.Errorf("asksForPrivileges(%q) = %v; want %v", doc, got, want)
		}
	}
}

func TestRedactor(t *testing.T) {
	r := redactor{"abc", "", "xyz"}
	if got := r.clean("abc and xyz and nothing"); got != "<secret> and <secret> and nothing" {
		t.Errorf("clean = %q", got)
	}
}

func TestReleaseHealth(t *testing.T) {
	for status, want := range map[release.Status]string{
		release.StatusDeployed: "ok", release.StatusFailed: "error", release.StatusSuperseded: "done",
		release.StatusUninstalled: "done", release.StatusPendingInstall: "warn", release.StatusUninstalling: "warn",
	} {
		if got := releaseHealth(status); got != want {
			t.Errorf("releaseHealth(%s) = %q; want %q", status, got, want)
		}
	}
}

func TestNewHelmConfigNeedsNoNetwork(t *testing.T) {
	env := newEnv(t, Options{})
	cfg, err := env.server.newHelmConfig("dev", "apps")
	if err != nil || cfg == nil || cfg.Releases == nil {
		t.Errorf("cfg = %v, err = %v", cfg, err)
	}
}

// ---- apps whose CRDs are not in the cluster yet ----------------------------------------------------

// missingCRDClient is a cluster that does not know the kinds the chart uses, as a
// real one answers before the chart's CRDs are installed.
type missingCRDClient struct{ kubefake.PrintingKubeClient }

func (c *missingCRDClient) Build(io.Reader, bool) (kube.ResourceList, error) {
	return nil, errors.New(`unable to build kubernetes objects from release manifest: resource mapping not found for name: "w" namespace: "apps" from "": no matches for kind "Widget" in version "demo.example.com/v1"`)
}

func TestInstallPreviewFallsBackToAnOfflineRenderWhenCRDsAreMissing(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.kubeClient = func() kube.Interface {
		return &missingCRDClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}}
	}

	rec := h.do(http.MethodPost, "/api/helm/install", installBody(map[string]any{"dryRun": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	joined := strings.Join(decode[changeResponse](t, rec).Changes, "\n")
	if !strings.Contains(joined, "warning: the API server could not validate every object") || !strings.Contains(joined, "Deployment/web (apps)") {
		t.Errorf("changes:\n%s", joined)
	}
	if n := len(h.releases(t, "_all")); n != 0 {
		t.Errorf("the preview stored %d release(s)", n)
	}

	// A real install does not pretend: the cluster's answer is reported.
	rec = h.do(http.MethodPost, "/api/helm/install", installBody(nil))
	if rec.Code != http.StatusBadRequest || decode[errorResponse](t, rec).Code != "missing_crds" {
		t.Errorf("real install: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUpgradePreviewSaysWhenTheCRDsAreGone(t *testing.T) {
	h := newHelmEnv(t, Options{})
	h.install(t, nil)
	h.kubeClient = func() kube.Interface {
		return &missingCRDClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}}
	}

	rec := h.do(http.MethodPost, "/api/helm/upgrade", map[string]any{"namespace": "apps", "name": "web", "dryRun": true, "fields": map[string]any{"replicas": 2}})
	got := decode[errorResponse](t, rec)
	if rec.Code != http.StatusBadRequest || got.Code != "missing_crds" || !strings.Contains(got.Hint, "Reinstall") {
		t.Errorf("status %d: %+v", rec.Code, got)
	}
}
