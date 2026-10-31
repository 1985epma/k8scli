package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	idDemo     = "11111111-1111-4111-8111-111111111111"
	idUnsigned = "22222222-2222-4222-8222-222222222222"
	idOther    = "33333333-3333-4333-8333-333333333333"
)

// fakeHub stands in for artifacthub.io: it serves search results and package
// details, and counts what it was asked.
type fakeHub struct {
	mu          sync.Mutex
	searchCalls int
	lastSearch  url.Values
	results     []map[string]any
	total       string
	details     map[string]map[string]any // "repo/name" or "repo/name/version"
	forceStatus int
	huge        bool
	badJSON     bool
	redirectTo  string
	block       chan struct{} // closed to release a blocked search
	entered     chan struct{} // signalled when a blocked search arrived
	cancelled   atomic.Bool
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.redirectTo != "" {
		http.Redirect(w, r, f.redirectTo, http.StatusFound)
		return
	}
	if f.forceStatus != 0 {
		http.Error(w, "boom", f.forceStatus)
		return
	}
	switch {
	case r.URL.Path == "/api/v1/packages/search":
		f.searchCalls++
		f.lastSearch = r.URL.Query()
		if f.block != nil {
			f.entered <- struct{}{}
			f.mu.Unlock()
			select {
			case <-r.Context().Done():
				f.cancelled.Store(true)
			case <-f.block:
			}
			f.mu.Lock()
			return
		}
		if f.huge {
			_, _ = w.Write([]byte(`{"packages":[],"pad":"` + strings.Repeat("x", hubMaxBody+10) + `"}`))
			return
		}
		if f.badJSON {
			_, _ = w.Write([]byte(`<html>not json</html>`))
			return
		}
		if f.total != "" {
			w.Header().Set("Pagination-Total-Count", f.total)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"packages": f.results})
	case strings.HasPrefix(r.URL.Path, "/api/v1/packages/helm/"):
		detail, ok := f.details[strings.TrimPrefix(r.URL.Path, "/api/v1/packages/helm/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(detail)
	default:
		http.NotFound(w, r)
	}
}

func fakePackage(id, repo, name string, mods ...func(map[string]any)) map[string]any {
	p := map[string]any{
		"package_id": id, "name": name, "normalized_name": name, "description": "A demo chart",
		"version": "1.0.0", "app_version": "9.9", "license": "MIT", "stars": 5, "official": false,
		"deprecated": false, "signed": true,
		"security_report_summary": map[string]any{"critical": 0, "high": 1, "medium": 2, "low": 3, "unknown": 4},
		"repository": map[string]any{
			"name": repo, "url": "https://example.test/charts/", "kind": 0, "official": false,
			"verified_publisher": true, "organization_display_name": "Acme",
		},
	}
	for _, mod := range mods {
		mod(p)
	}
	return p
}

func repoOf(p map[string]any) map[string]any { return p["repository"].(map[string]any) }

// hubEnv is a helmEnv whose Artifact Hub is a local server.
type hubEnv struct {
	*helmEnv
	hub *fakeHub
	srv *httptest.Server
}

func newHubEnv(t *testing.T, opts Options) *hubEnv {
	t.Helper()
	h := &hubEnv{helmEnv: newHelmEnv(t, opts), hub: &fakeHub{details: map[string]map[string]any{}}}
	h.srv = httptest.NewTLSServer(h.hub)
	t.Cleanup(h.srv.Close)

	client := h.server.helm.hub
	client.base = h.srv.URL
	client.http = h.srv.Client()
	client.http.CheckRedirect = client.checkRedirect

	// A verified package whose chart is the test archive.
	h.hub.results = []map[string]any{fakePackage(idDemo, "acme", "demo")}
	h.setDetail("acme/demo", "1.0.0", h.entry.digest, "/demo-1.0.0.tgz")
	return h
}

// setDetail registers what Artifact Hub says about a version of acme/demo.
func (h *hubEnv) setDetail(key, version, digest, archive string, mods ...func(map[string]any)) {
	detail := fakePackage(idDemo, "acme", "demo", func(p map[string]any) {
		p["version"] = version
		p["content_url"] = h.archive.URL + archive
		p["digest"] = digest
		p["prerelease"] = false
		p["data"] = map[string]any{"type": "application"}
	})
	for _, mod := range mods {
		mod(detail)
	}
	h.hub.mu.Lock()
	h.hub.details[key] = detail
	h.hub.mu.Unlock()
}

// searched makes the console remember the package id, like a person who searched first.
func (h *hubEnv) searched(t *testing.T) {
	t.Helper()
	if rec := h.do(http.MethodGet, "/api/helm/search?q=demo", nil); rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
}

func hubBody(extra map[string]any) map[string]any {
	body := map[string]any{"source": "hub", "packageId": idDemo, "name": "web", "namespace": "apps", "createNamespace": true, "settings": map[string]any{}}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

type hubReply struct {
	Action        string   `json:"action"`
	DryRun        bool     `json:"dryRun"`
	Name          string   `json:"name"`
	Summary       string   `json:"summary"`
	Changes       []string `json:"changes"`
	Equivalent    string   `json:"equivalent"`
	Source        string   `json:"source"`
	PackageID     string   `json:"packageId"`
	Chart         string   `json:"chart"`
	Version       string   `json:"version"`
	Digest        string   `json:"digest"`
	ConfirmPhrase string   `json:"confirmPhrase"`
	Images        []string `json:"images"`
}

// preview runs the dry run of an install from search and returns what it said.
func (h *hubEnv) preview(t *testing.T, extra map[string]any) hubReply {
	t.Helper()
	body := hubBody(extra)
	body["dryRun"] = true
	rec := h.do(http.MethodPost, "/api/helm/install", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	return decode[hubReply](t, rec)
}

// apply installs from search the way the UI does: with the digest and the phrase of the preview.
func (h *hubEnv) apply(t *testing.T, extra map[string]any) hubReply {
	t.Helper()
	preview := h.preview(t, extra)
	body := hubBody(extra)
	body["digest"], body["confirmPhrase"] = preview.Digest, preview.ConfirmPhrase
	rec := h.do(http.MethodPost, "/api/helm/install", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	return decode[hubReply](t, rec)
}

func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decode[errorResponse](t, rec).Code
}

// ---- search -------------------------------------------------------------------------------------------

func TestSearchMapsResultsAndMarksWhatIsInTheCatalog(t *testing.T) {
	h := newHubEnv(t, Options{})
	long := strings.Repeat("word ", 80)
	h.hub.total = "45"
	h.hub.results = []map[string]any{
		fakePackage(idDemo, "acme", "demo", func(p map[string]any) { p["description"] = long; p["display_name"] = "Demo App" }),
		fakePackage(idOther, "stranger", "demo", func(p map[string]any) {
			repoOf(p)["verified_publisher"] = false
			repoOf(p)["url"] = "https://other.example/charts"
		}),
	}

	rec := h.do(http.MethodGet, "/api/helm/search?q=%20%20demo%20%20app%20&page=2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := decode[hubSearchJSON](t, rec)
	if got.Query != "demo app" || got.Page != 2 || got.PageSize != 20 || got.Total != 45 || !got.HasMore || len(got.Items) != 2 {
		t.Fatalf("search = %+v", got)
	}
	// What was asked of Artifact Hub: Helm charts only, one page of 20, the right offset.
	q := h.hub.lastSearch
	if q.Get("kind") != "0" || q.Get("limit") != "20" || q.Get("offset") != "20" || q.Get("ts_query_web") != "demo app" {
		t.Errorf("upstream query = %v", q)
	}

	first := got.Items[0]
	if first.ID != idDemo || first.DisplayName != "Demo App" || first.Version != "1.0.0" || first.Stars != 5 || !first.Signed ||
		first.Repository.Name != "acme" || first.Repository.Organization != "Acme" || !first.Repository.VerifiedPublisher ||
		first.Security == nil || first.Security.High != 1 || first.License != "MIT" {
		t.Errorf("first = %+v", first)
	}
	if !strings.HasSuffix(first.Description, "…") || len([]rune(first.Description)) > maxDescriptionLen+1 {
		t.Errorf("description not truncated: %d runes", len([]rune(first.Description)))
	}
	if first.InCatalog != "demo" || !first.Installable || first.Reason != "" {
		t.Errorf("the chart of the catalog should be recognised (trailing slash and case ignored): %+v", first)
	}
	if second := got.Items[1]; second.InCatalog != "" || second.Installable || !strings.Contains(second.Reason, "not verified") {
		t.Errorf("second = %+v", second)
	}
}

func TestSearchEligibilityMatrix(t *testing.T) {
	h := newHubEnv(t, Options{})
	mk := func(id string, mod func(map[string]any)) map[string]any {
		return fakePackage(id, "r-"+id[:4], "chart", mod)
	}
	h.hub.results = []map[string]any{
		mk("a1111111-1111-4111-8111-111111111111", func(p map[string]any) {}), // verified
		mk("a2222222-2222-4222-8222-222222222222", func(p map[string]any) { repoOf(p)["verified_publisher"] = false; repoOf(p)["official"] = true }),
		mk("a3333333-3333-4333-8333-333333333333", func(p map[string]any) { repoOf(p)["verified_publisher"] = false; p["official"] = true }),
		mk("a4444444-4444-4444-8444-444444444444", func(p map[string]any) { repoOf(p)["verified_publisher"] = false }),
		mk("a5555555-5555-4555-8555-555555555555", func(p map[string]any) { p["deprecated"] = true }),
		mk("a6666666-6666-4666-8666-666666666666", func(p map[string]any) { repoOf(p)["kind"] = 3 }),
		fakePackage("a7777777-7777-4777-8777-777777777777", "r-7777", "raw"),
		fakePackage("a8888888-8888-4888-8888-888888888888", "r-8888", "app-template"),
	}
	got := decode[hubSearchJSON](t, h.do(http.MethodGet, "/api/helm/search?q=chart", nil)).Items
	want := []struct {
		ok     bool
		reason string
	}{
		{true, ""}, {true, ""}, {true, ""}, {false, "not verified"}, {false, "deprecated"},
		{false, "not a Helm chart"}, {false, "arbitrary objects"}, {false, "arbitrary objects"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d items", len(got))
	}
	for i, w := range want {
		if got[i].Installable != w.ok || !strings.Contains(got[i].Reason, w.reason) {
			t.Errorf("item %d: installable=%v reason=%q; want %v / %q", i, got[i].Installable, got[i].Reason, w.ok, w.reason)
		}
	}
}

func TestSearchValidation(t *testing.T) {
	h := newHubEnv(t, Options{})
	for name, target := range map[string]string{
		"too short":      "/api/helm/search?q=a",
		"blank":          "/api/helm/search?q=%20%20%20",
		"missing":        "/api/helm/search",
		"too long":       "/api/helm/search?q=" + strings.Repeat("a", hubMaxQuery+1),
		"control chars":  "/api/helm/search?q=ab%00cd",
		"page zero":      "/api/helm/search?q=demo&page=0",
		"page too big":   "/api/helm/search?q=demo&page=" + strconv.Itoa(hubMaxPage+1),
		"page not a num": "/api/helm/search?q=demo&page=two",
	} {
		if rec := h.do(http.MethodGet, target, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d; want 400", name, rec.Code)
		}
	}
	if h.hub.searchCalls != 0 {
		t.Errorf("invalid searches reached Artifact Hub (%d calls)", h.hub.searchCalls)
	}
	if rec := h.do(http.MethodGet, "/api/helm/search?q="+strings.Repeat("a", hubMaxQuery), nil); rec.Code != http.StatusOK {
		t.Errorf("a search at the limit = %d", rec.Code)
	}
}

func TestSearchIsCachedAndTheCacheIsBounded(t *testing.T) {
	h := newHubEnv(t, Options{})
	hub := h.server.helm.hub
	clock := time.Now()
	hub.now = func() time.Time { return clock }

	for range 3 {
		h.do(http.MethodGet, "/api/helm/search?q=Demo", nil)
	}
	h.do(http.MethodGet, "/api/helm/search?q=demo", nil) // same text, other case
	if h.hub.searchCalls != 1 {
		t.Errorf("upstream calls = %d; want 1 (the answer is cached)", h.hub.searchCalls)
	}
	h.do(http.MethodGet, "/api/helm/search?q=demo&page=2", nil)
	if h.hub.searchCalls != 2 {
		t.Errorf("another page must be another request: %d", h.hub.searchCalls)
	}
	clock = clock.Add(hubSearchTTL + time.Second)
	h.do(http.MethodGet, "/api/helm/search?q=demo", nil)
	if h.hub.searchCalls != 3 {
		t.Errorf("an expired answer must be fetched again: %d", h.hub.searchCalls)
	}

	for i := range hubSearchMax + 30 {
		h.do(http.MethodGet, "/api/helm/search?q=query"+strconv.Itoa(i), nil)
	}
	hub.mu.Lock()
	size := len(hub.searches)
	hub.mu.Unlock()
	if size > hubSearchMax {
		t.Errorf("the search cache holds %d entries; the limit is %d", size, hubSearchMax)
	}
}

func TestSearchFailuresBecomeCleanErrors(t *testing.T) {
	h := newHubEnv(t, Options{})
	for name, set := range map[string]func(){
		"server error": func() { h.hub.forceStatus = http.StatusInternalServerError },
		"too big":      func() { h.hub.forceStatus = 0; h.hub.huge = true },
		"not json":     func() { h.hub.huge = false; h.hub.badJSON = true },
		"other host":   func() { h.hub.badJSON = false; h.hub.redirectTo = "https://attacker.example/api/v1/packages/search" },
		"http redirect": func() {
			h.hub.redirectTo = "http://" + strings.TrimPrefix(h.srv.URL, "https://") + "/api/v1/packages/search"
		},
	} {
		set()
		rec := h.do(http.MethodGet, "/api/helm/search?q="+strings.ReplaceAll(name, " ", ""), nil)
		if rec.Code != http.StatusBadGateway || code(t, rec) != "hub_unavailable" {
			t.Errorf("%s: status %d: %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestSearchStopsWhenTheBrowserGivesUp(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.hub.block = make(chan struct{})
	h.hub.entered = make(chan struct{}, 1)
	t.Cleanup(func() { close(h.hub.block) })

	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/helm/search?q=demo", nil)
	req.Host = "127.0.0.1:8484"
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: h.server.Token()})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		done <- rec
	}()

	select {
	case <-h.hub.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the search never reached Artifact Hub")
	}
	cancel() // a new keystroke aborts the old search
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler kept working after the request was cancelled")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !h.hub.cancelled.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.hub.cancelled.Load() {
		t.Error("the call to Artifact Hub was not cancelled")
	}
}

func TestCatalogOnlyTurnsOffSearchAndInstallFromSearch(t *testing.T) {
	h := newHubEnv(t, Options{CatalogOnly: true})

	for _, target := range []string{"/api/helm/search?q=demo"} {
		if rec := h.do(http.MethodGet, target, nil); rec.Code != http.StatusForbidden || code(t, rec) != "catalog_only" {
			t.Errorf("GET %s = %d %s", target, rec.Code, rec.Body.String())
		}
	}
	for _, dry := range []bool{true, false} {
		if rec := h.do(http.MethodPost, "/api/helm/install", hubBody(map[string]any{"dryRun": dry})); rec.Code != http.StatusForbidden || code(t, rec) != "catalog_only" {
			t.Errorf("hub install dryRun=%v = %d %s", dry, rec.Code, rec.Body.String())
		}
	}
	if h.hub.searchCalls != 0 {
		t.Errorf("--catalog-only still reached Artifact Hub")
	}

	caps := decode[helmCapabilities](t, h.do(http.MethodGet, "/api/helm/capabilities", nil))
	if caps.Search || caps.HubInstall {
		t.Errorf("capabilities = %+v", caps)
	}
	// The catalog keeps working.
	if rec := h.do(http.MethodPost, "/api/helm/install", installBody(map[string]any{"dryRun": true})); rec.Code != http.StatusOK {
		t.Errorf("catalog install = %d %s", rec.Code, rec.Body.String())
	}

	open := newHubEnv(t, Options{})
	if caps := decode[helmCapabilities](t, open.do(http.MethodGet, "/api/helm/capabilities", nil)); !caps.Search || !caps.HubInstall {
		t.Errorf("default capabilities = %+v", caps)
	}
}

// ---- install from search -------------------------------------------------------------------------------

func TestHubInstallNeedsASearchFirstAndNeverTrustsTheBrowserForWhereToDownload(t *testing.T) {
	h := newHubEnv(t, Options{})

	// The id was never returned by a search: the console does not guess where it lives.
	rec := h.do(http.MethodPost, "/api/helm/install", hubBody(map[string]any{"dryRun": true}))
	if rec.Code != http.StatusNotFound || code(t, rec) != "unknown_package" {
		t.Errorf("unknown id: %d %s", rec.Code, rec.Body.String())
	}

	// Fields that would carry an address are not part of the request at all.
	h.searched(t)
	for _, key := range []string{"contentUrl", "repository", "url", "archive", "repo", "chart"} {
		if rec := h.do(http.MethodPost, "/api/helm/install", hubBody(map[string]any{key: "https://evil.example/x.tgz", "dryRun": true})); rec.Code != http.StatusBadRequest {
			t.Errorf("field %q was accepted: %d", key, rec.Code)
		}
	}
}

func TestHubInstallPreviewShowsTheDigestThePhraseAndWhatItDoes(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)

	got := h.preview(t, map[string]any{"settings": map[string]any{"replicaCount": 3, "message": "hello"}})
	if got.Action != "created" || !got.DryRun || got.Source != "hub" || got.PackageID != idDemo || got.Chart != "demo" || got.Version != "1.0.0" || got.Name != "web" {
		t.Errorf("reply = %+v", got)
	}
	if got.Digest != h.entry.digest || got.ConfirmPhrase != "demo" {
		t.Errorf("digest %q / phrase %q", got.Digest, got.ConfirmPhrase)
	}
	joined := strings.Join(got.Changes, "\n")
	for _, want := range []string{
		"chart: demo 1.0.0 from repository acme (verified publisher, signed)",
		"warning: creates cluster-wide objects", "warning: creates 1 Secret",
		"warning: runs images that are not pinned to a version", "nginx:latest",
		"image: nginx:latest", "Deployment/web (apps)", "ClusterRole/web-reader", "CustomResourceDefinition/widgets.demo.example.com",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("review lacks %q:\n%s", want, joined)
		}
	}
	if len(got.Images) != 1 || got.Images[0] != "nginx:latest" {
		t.Errorf("images = %v", got.Images)
	}
	for _, want := range []string{"helm install web demo --repo https://example.test/charts/ --version 1.0.0 -n apps --create-namespace", "--set replicaCount=3", "--set message=hello"} {
		if !strings.Contains(got.Equivalent, want) {
			t.Errorf("equivalent lacks %q: %s", want, got.Equivalent)
		}
	}
	if n := len(h.releases(t, "_all")); n != 0 {
		t.Errorf("the preview installed %d release(s)", n)
	}

	// Pinning the tag takes the warning away.
	pinned := h.preview(t, map[string]any{"settings": map[string]any{"image.tag": "1.27"}})
	if strings.Contains(strings.Join(pinned.Changes, "\n"), "not pinned") {
		t.Errorf("a pinned image still warns: %v", pinned.Changes)
	}
}

func TestHubInstallAppliesOnlyWhatWasReviewed(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	preview := h.preview(t, nil)

	attempt := func(mod func(map[string]any)) *httptest.ResponseRecorder {
		body := hubBody(nil)
		body["digest"], body["confirmPhrase"] = preview.Digest, preview.ConfirmPhrase
		mod(body)
		return h.do(http.MethodPost, "/api/helm/install", body)
	}

	for name, tc := range map[string]struct {
		mod  func(map[string]any)
		code string
	}{
		"no phrase":      {func(b map[string]any) { delete(b, "confirmPhrase") }, "phrase_required"},
		"wrong phrase":   {func(b map[string]any) { b["confirmPhrase"] = "Demo" }, "phrase_required"},
		"no digest":      {func(b map[string]any) { delete(b, "digest") }, "digest_changed"},
		"another digest": {func(b map[string]any) { b["digest"] = strings.Repeat("0", 64) }, "digest_changed"},
	} {
		rec := attempt(tc.mod)
		if rec.Code != http.StatusConflict || code(t, rec) != tc.code {
			t.Errorf("%s: status %d: %s", name, rec.Code, rec.Body.String())
		}
	}
	if n := len(h.releases(t, "_all")); n != 0 {
		t.Fatalf("something was installed without the review (%d)", n)
	}

	if rec := attempt(func(map[string]any) {}); rec.Code != http.StatusOK {
		t.Fatalf("the reviewed install failed: %d %s", rec.Code, rec.Body.String())
	}

	releases := h.releases(t, "apps")
	if len(releases) != 1 {
		t.Fatalf("releases = %+v", releases)
	}
	rel := releases[0]
	if !rel.Managed || rel.Source != "hub" || rel.PackageID != idDemo || rel.CatalogID != "" || rel.Chart != "demo" || rel.Status != "deployed" {
		t.Errorf("release = %+v", rel)
	}
	stored, err := h.mem.Get("sh.helm.release.v1.web.v1")
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"k8scli.io/managed": "true", "k8scli.io/source": "hub", "k8scli.io/package": idDemo,
		"k8scli.io/hub-repo": "acme", "k8scli.io/hub-chart": "demo",
	} {
		if stored.Labels[key] != want {
			t.Errorf("label %s = %q; want %q", key, stored.Labels[key], want)
		}
	}
}

func TestHubInstallRefusesAnArchiveThatChangedAfterThePreview(t *testing.T) {
	h := newHubEnv(t, Options{})
	// Artifact Hub reports no digest for this one, so the digest is derived on first use.
	h.setDetail("acme/demo", "1.0.0", "", "/demo-1.0.0.tgz")
	h.searched(t)
	preview := h.preview(t, nil)

	// The repository now serves a different archive for the same version.
	swapped, _ := packChart(t, testChart("1.0.0", true))
	h.archives["/demo-1.0.0.tgz"] = swapped

	body := hubBody(nil)
	body["digest"], body["confirmPhrase"] = preview.Digest, preview.ConfirmPhrase
	rec := h.do(http.MethodPost, "/api/helm/install", body)
	if rec.Code != http.StatusConflict || code(t, rec) != "digest_changed" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(h.releases(t, "_all")); n != 0 {
		t.Errorf("a changed archive was installed")
	}
}

func TestHubInstallRefusesADigestArtifactHubDoesNotConfirm(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.setDetail("acme/demo", "1.0.0", strings.Repeat("a", 64), "/demo-1.0.0.tgz")
	h.searched(t)

	for _, dry := range []bool{true, false} {
		rec := h.do(http.MethodPost, "/api/helm/install", hubBody(map[string]any{"dryRun": dry, "confirmPhrase": "demo", "digest": strings.Repeat("a", 64)}))
		if rec.Code != http.StatusBadGateway || code(t, rec) != "digest_mismatch" {
			t.Errorf("dryRun=%v: status %d: %s", dry, rec.Code, rec.Body.String())
		}
	}
	if n := len(h.releases(t, "_all")); n != 0 {
		t.Errorf("installed %d release(s)", n)
	}
}

func TestHubInstallEligibilityIsCheckedAgainOnTheFullRecord(t *testing.T) {
	cases := map[string]struct {
		mod    func(map[string]any)
		status int
		code   string
	}{
		"unverified":      {func(p map[string]any) { repoOf(p)["verified_publisher"] = false }, 400, "not_installable"},
		"deprecated":      {func(p map[string]any) { p["deprecated"] = true }, 400, "not_installable"},
		"library":         {func(p map[string]any) { p["data"] = map[string]any{"type": "library"} }, 400, "not_installable"},
		"prerelease":      {func(p map[string]any) { p["prerelease"] = true }, 400, "not_installable"},
		"raw chart":       {func(p map[string]any) { p["name"], p["normalized_name"] = "raw", "raw" }, 400, "not_installable"},
		"http archive":    {func(p map[string]any) { p["content_url"] = "http://example.test/demo.tgz" }, 400, "not_installable"},
		"not helm":        {func(p map[string]any) { repoOf(p)["kind"] = 1 }, 400, "not_installable"},
		"another package": {func(p map[string]any) { p["package_id"] = idOther }, 409, "package_changed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHubEnv(t, Options{})
			h.setDetail("acme/demo", "1.0.0", h.entry.digest, "/demo-1.0.0.tgz", tc.mod)
			h.searched(t)
			rec := h.do(http.MethodPost, "/api/helm/install", hubBody(map[string]any{"dryRun": true}))
			if rec.Code != tc.status || code(t, rec) != tc.code {
				t.Errorf("status %d: %s", rec.Code, rec.Body.String())
			}
		})
	}

	t.Run("library by its own metadata", func(t *testing.T) {
		h := newHubEnv(t, Options{})
		lib := testChart("1.0.0", false)
		lib.Metadata.Type = "library"
		data, digest := packChart(t, lib)
		h.archives["/lib.tgz"] = data
		h.setDetail("acme/demo", "1.0.0", digest, "/lib.tgz")
		h.searched(t)
		rec := h.do(http.MethodPost, "/api/helm/install", hubBody(map[string]any{"dryRun": true}))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "library") {
			t.Errorf("status %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestHubInstallVersionAndShapeValidation(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	h.setDetail("acme/demo/1.0.0", "1.0.0", h.entry.digest, "/demo-1.0.0.tgz")

	if rec := h.do(http.MethodPost, "/api/helm/install", hubBody(map[string]any{"dryRun": true, "version": "1.0.0"})); rec.Code != http.StatusOK {
		t.Errorf("an explicit version = %d %s", rec.Code, rec.Body.String())
	}
	for name, extra := range map[string]map[string]any{
		"unknown version": {"version": "9.9.9"},
		"bad version":     {"version": "1.0/../x"},
		"bad package id":  {"packageId": "../../etc"},
		"not hex":         {"packageId": "ZZZZZZZZ-1111-4111-8111-111111111111"},
		"no namespace":    {"namespace": ""},
		"bad namespace":   {"namespace": "A b"},
		"reserved ns":     {"namespace": "kube-system"},
		"bad name":        {"name": "Web App"},
		"bad source":      {"source": "other"},
	} {
		body := hubBody(extra)
		body["dryRun"] = true
		rec := h.do(http.MethodPost, "/api/helm/install", body)
		if rec.Code < 400 || rec.Code >= 500 {
			t.Errorf("%s: status %d; want a 4xx (%s)", name, rec.Code, rec.Body.String())
		}
	}
}

func TestHubInstallSettings(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)

	many := map[string]any{}
	for i := range maxHubSettings + 1 {
		many["image.tag"+strconv.Itoa(i)] = "x"
	}
	cases := map[string]map[string]any{
		"not a path":          {"a b": "x"},
		"empty segment":       {"image..tag": "x"},
		"leading dot":         {".image": "x"},
		"index syntax":        {"ports[0]": 80},
		"traversal":           {"../etc/passwd": "x"},
		"slash":               {"image/tag": "x"},
		"does not exist":      {"nope": "x"},
		"nested not existing": {"image.nope": "x"},
		"through a scalar":    {"message.deeper": "x"},
		"a group of values":   {"image": "x"},
		"a list":              {"ports": 1},
		"object value":        {"message": map[string]any{"a": 1}},
		"list value":          {"message": []any{"a"}},
		"wrong type string":   {"replicaCount": "three"},
		"wrong type number":   {"message": 3},
		"wrong type bool":     {"feature.enabled": "yes"},
		"denied key":          {"extraObjects": "x"},
		"too long":            {"message": strings.Repeat("a", maxSettingValue+1)},
		"control char":        {"message": "a\nb"},
		"invisible char":      {"message": "a\u202eb"},
		"too many":            many,
		"too deep":            {"a.b.c.d.e.f.g.h.i": "x"},
		"very long path":      {strings.Repeat("a", maxSettingPath+1): "x"},
	}
	for name, settings := range cases {
		body := hubBody(map[string]any{"settings": settings, "dryRun": true})
		rec := h.do(http.MethodPost, "/api/helm/install", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d; want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}

	// What is allowed: scalars of the same kind as the chart's default, and anything for a null default.
	ok := h.preview(t, map[string]any{"settings": map[string]any{
		"replicaCount": 2, "message": "hi there", "feature.enabled": false, "image.tag": "1.27", "service.port": 8080, "nothing": "any scalar",
	}})
	for _, want := range []string{"--set replicaCount=2", "--set feature.enabled=false", "--set image.tag=1.27", "--set service.port=8080"} {
		if !strings.Contains(ok.Equivalent, want) {
			t.Errorf("equivalent lacks %q: %s", want, ok.Equivalent)
		}
	}
}

func TestHubInstallSecretsNeverLeave(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	settings := map[string]any{"auth.password": secretPassword}

	rec := h.do(http.MethodPost, "/api/helm/install", hubBody(map[string]any{"settings": settings, "dryRun": true}))
	noSecret(t, "the preview", rec.Body.String())
	if got := decode[hubReply](t, rec); !strings.Contains(got.Equivalent, "auth.password=<secret>") {
		t.Errorf("equivalent = %s", got.Equivalent)
	}

	reply := h.apply(t, map[string]any{"settings": settings})
	b, _ := json.Marshal(reply)
	noSecret(t, "the apply", string(b))

	for _, target := range []string{"/api/helm/releases?namespace=apps", "/api/helm/release?namespace=apps&name=web"} {
		noSecret(t, target, h.do(http.MethodGet, target, nil).Body.String())
	}
	detail := decode[helmReleaseDetail](t, h.do(http.MethodGet, "/api/helm/release?namespace=apps&name=web", nil))
	if detail.Source != "hub" || detail.Settings["auth.password"] != "<secret>" || !strings.Contains(detail.Notes, "<secret>") {
		t.Errorf("detail = %+v", detail)
	}

	// A chart that fails while quoting the secret must not echo it.
	body := hubBody(map[string]any{"name": "again", "settings": map[string]any{"auth.password": secretPassword, "feature.enabled": true}, "dryRun": true})
	if rec := h.do(http.MethodPost, "/api/helm/install", body); rec.Code == http.StatusOK {
		t.Fatalf("the failing chart was accepted")
	} else {
		noSecret(t, "an install error", rec.Body.String())
	}
}

func TestHubInstallGuards(t *testing.T) {
	// Read-only: the preview works, the apply is blocked before anything else.
	ro := newHubEnv(t, Options{ReadOnly: true})
	ro.searched(t)
	prev := ro.preview(t, nil)
	body := hubBody(map[string]any{"digest": prev.Digest, "confirmPhrase": prev.ConfirmPhrase})
	if rec := ro.do(http.MethodPost, "/api/helm/install", body); rec.Code != http.StatusForbidden || code(t, rec) != "read_only" {
		t.Errorf("read-only apply: %d %s", rec.Code, rec.Body.String())
	}

	// Production: the context name is needed on top of the chart name.
	p := newHubEnv(t, Options{})
	p.searched(t)
	prod := hubBody(map[string]any{"context": "prod-eu"})
	prod["dryRun"] = true
	prevProd := decode[hubReply](t, p.do(http.MethodPost, "/api/helm/install", prod))
	if prevProd.Digest == "" {
		t.Fatal("the preview in a production context must work without confirmation")
	}
	delete(prod, "dryRun")
	prod["digest"], prod["confirmPhrase"] = prevProd.Digest, prevProd.ConfirmPhrase
	if rec := p.do(http.MethodPost, "/api/helm/install", prod); rec.Code != http.StatusConflict || code(t, rec) != "confirmation_required" {
		t.Errorf("no context name: %d %s", rec.Code, rec.Body.String())
	}
	prod["confirm"] = "prod-eu"
	prod["confirmPhrase"] = ""
	if rec := p.do(http.MethodPost, "/api/helm/install", prod); rec.Code != http.StatusConflict || code(t, rec) != "phrase_required" {
		t.Errorf("context name but no chart name: %d %s", rec.Code, rec.Body.String())
	}
	if n := len(p.releases(t, "_all")); n != 0 {
		t.Fatalf("installed %d release(s) without both confirmations", n)
	}
	prod["confirmPhrase"] = prevProd.ConfirmPhrase
	if rec := p.do(http.MethodPost, "/api/helm/install", prod); rec.Code != http.StatusOK {
		t.Errorf("both confirmations: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHubInstallRefusesATakenName(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	h.apply(t, nil)
	rec := h.do(http.MethodPost, "/api/helm/install", hubBody(map[string]any{"dryRun": true}))
	if rec.Code != http.StatusConflict || code(t, rec) != "already_exists" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHubReleaseNameDefaultsToTheChartName(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	got := h.preview(t, map[string]any{"name": ""})
	if got.Name != "demo" {
		t.Errorf("name = %q", got.Name)
	}
}

// ---- upgrade and uninstall of releases installed from search ------------------------------------------------

func (h *hubEnv) upgrade(extra map[string]any) *httptest.ResponseRecorder {
	body := map[string]any{"namespace": "apps", "name": "web", "settings": map[string]any{}}
	for k, v := range extra {
		body[k] = v
	}
	return h.do(http.MethodPost, "/api/helm/upgrade", body)
}

func TestHubUpgradeGoesThroughTheSameReview(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	h.apply(t, nil)

	// New version 1.1.0 with an extra object; Artifact Hub reports its digest.
	h.setDetail("acme/demo/1.1.0", "1.1.0", h.digest11, "/demo-1.1.0.tgz")

	prev := h.upgrade(map[string]any{"version": "1.1.0", "dryRun": true, "settings": map[string]any{"replicaCount": 3}})
	if prev.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", prev.Code, prev.Body.String())
	}
	got := decode[hubReply](t, prev)
	joined := strings.Join(got.Changes, "\n")
	for _, want := range []string{"chart: 1.0.0 -> 1.1.0", `replicaCount: "1" -> "3"`, "+ ConfigMap/web-extra (apps)", "image: nginx:latest", "from repository acme"} {
		if !strings.Contains(joined, want) {
			t.Errorf("review lacks %q:\n%s", want, joined)
		}
	}
	if got.Action != "updated" || got.Digest != h.digest11 || got.ConfirmPhrase != "demo" || got.Version != "1.1.0" {
		t.Errorf("reply = %+v", got)
	}
	if rel := h.releases(t, "apps")[0]; rel.ChartVersion != "1.0.0" || rel.Revision != 1 {
		t.Errorf("a preview changed the release: %+v", rel)
	}

	// The apply needs the phrase and the digest of the preview.
	real := map[string]any{"version": "1.1.0", "settings": map[string]any{"replicaCount": 3}}
	if rec := h.upgrade(real); rec.Code != http.StatusConflict || code(t, rec) != "phrase_required" {
		t.Errorf("no phrase: %d %s", rec.Code, rec.Body.String())
	}
	real["confirmPhrase"] = "demo"
	if rec := h.upgrade(real); rec.Code != http.StatusConflict || code(t, rec) != "digest_changed" {
		t.Errorf("no digest: %d %s", rec.Code, rec.Body.String())
	}
	real["digest"] = got.Digest
	if rec := h.upgrade(real); rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}

	rel := h.releases(t, "apps")[0]
	if rel.ChartVersion != "1.1.0" || rel.Revision != 2 || rel.Source != "hub" || rel.PackageID != idDemo || !rel.Managed {
		t.Errorf("release = %+v", rel)
	}
	detail := decode[helmReleaseDetail](t, h.do(http.MethodGet, "/api/helm/release?namespace=apps&name=web", nil))
	if detail.Settings["replicaCount"] != "3" {
		t.Errorf("settings = %v", detail.Settings)
	}

	// Nothing new: nothing to do.
	again := map[string]any{"version": "1.1.0", "settings": map[string]any{"replicaCount": 3}}
	if got := decode[hubReply](t, h.upgrade(again)); got.Action != "unchanged" {
		t.Errorf("action = %q", got.Action)
	}
}

func TestHubUpgradeKeepsItsSecretsAndChecksItsSettings(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	h.apply(t, map[string]any{"settings": map[string]any{"auth.password": secretPassword}})

	rec := h.upgrade(map[string]any{"dryRun": true, "settings": map[string]any{"auth.password": secretPassword + "-new", "message": "hi"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	noSecret(t, "the upgrade preview", rec.Body.String())
	if strings.Contains(rec.Body.String(), secretPassword+"-new") {
		t.Error("the new secret is in the preview")
	}
	if got := decode[hubReply](t, rec); !strings.Contains(strings.Join(got.Changes, "\n"), "auth.password: changed") {
		t.Errorf("changes = %v", got.Changes)
	}

	for name, settings := range map[string]map[string]any{
		"unknown path": {"nope": 1},
		"wrong type":   {"replicaCount": "x"},
		"group":        {"image": "x"},
	} {
		if rec := h.upgrade(map[string]any{"dryRun": true, "settings": settings}); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
}

func TestHubUpgradeGuardsAndCatalogOnly(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	h.apply(t, nil)

	body := map[string]any{"settings": map[string]any{"replicaCount": 2}, "context": "prod-eu"}
	if rec := h.upgrade(body); rec.Code != http.StatusConflict || code(t, rec) != "confirmation_required" {
		t.Errorf("production: %d %s", rec.Code, rec.Body.String())
	}

	h.server.opts.CatalogOnly = true
	if rec := h.upgrade(map[string]any{"dryRun": true}); rec.Code != http.StatusForbidden || code(t, rec) != "catalog_only" {
		t.Errorf("catalog-only: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHubReleaseCanBeUninstalledEvenInCatalogOnlyMode(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	h.apply(t, nil)
	h.server.opts.CatalogOnly = true // removing what is installed never needs Artifact Hub

	prev := h.do(http.MethodPost, "/api/helm/uninstall", map[string]any{"namespace": "apps", "name": "web", "dryRun": true, "digest": "ignored", "confirmPhrase": "ignored"})
	if rec := prev; rec.Code != http.StatusOK || !strings.Contains(strings.Join(decode[changeResponse](t, rec).Changes, "\n"), "- Deployment/web (apps)") {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	if n := len(h.releases(t, "apps")); n != 1 {
		t.Fatalf("the preview removed the release")
	}

	// The production gate applies as for every release.
	if rec := h.do(http.MethodPost, "/api/helm/uninstall", map[string]any{"namespace": "apps", "name": "web", "context": "prod-eu"}); rec.Code != http.StatusConflict {
		t.Errorf("production: %d", rec.Code)
	}

	rec := h.do(http.MethodPost, "/api/helm/uninstall", map[string]any{"namespace": "apps", "name": "web"})
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall: %d %s", rec.Code, rec.Body.String())
	}
	if n := len(h.releases(t, "apps")); n != 0 {
		t.Errorf("%d release(s) left", n)
	}
}

func TestOptionalFieldsAreAcceptedEverywhere(t *testing.T) {
	h := newHubEnv(t, Options{})

	// A catalog install that carries the fields of the other source: they are ignored.
	rec := h.do(http.MethodPost, "/api/helm/install", installBody(map[string]any{
		"dryRun": true, "source": "catalog", "settings": map[string]any{}, "version": "", "digest": "", "confirmPhrase": "", "packageId": "",
	}))
	if rec.Code != http.StatusOK {
		t.Errorf("catalog install with the hub fields = %d %s", rec.Code, rec.Body.String())
	}
	h.install(t, nil)

	for name, target := range map[string]string{"upgrade": "/api/helm/upgrade", "uninstall": "/api/helm/uninstall"} {
		rec := h.do(http.MethodPost, target, map[string]any{
			"namespace": "apps", "name": "web", "dryRun": true, "source": "catalog", "digest": "x", "confirmPhrase": "y", "settings": map[string]any{}, "version": "",
		})
		if rec.Code != http.StatusOK {
			t.Errorf("%s with digest and confirmPhrase = %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestReleasesTellWhereEachOneCameFrom(t *testing.T) {
	h := newHubEnv(t, Options{})
	h.searched(t)
	h.install(t, nil)                      // catalog
	h.foreignRelease(t, "legacy", "tools") // installed with the helm command line
	h.mem.SetNamespace("apps")
	h.apply(t, map[string]any{"name": "fromhub"})

	bySource := map[string]string{}
	for _, rel := range h.releases(t, "_all") {
		bySource[rel.Name] = rel.Source
	}
	if bySource["web"] != "catalog" || bySource["fromhub"] != "hub" || bySource["legacy"] != "other" {
		t.Errorf("sources = %v", bySource)
	}
}

// ---- the review and the settings, in isolation -----------------------------------------------------------------

func TestHubReviewFlagsWhatIsRisky(t *testing.T) {
	manifest := `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: admin-everything}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-admin}
subjects: [{kind: ServiceAccount, name: app, namespace: apps}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: everything}
rules:
- apiGroups: ["*"]
  resources: ["*"]
  verbs: ["*"]
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata: {name: policy}
---
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: agent}
spec:
  template:
    spec:
      hostNetwork: true
      hostPID: true
      volumes:
      - name: logs
        hostPath: {path: /var/log}
      initContainers:
      - {name: init, image: "busybox"}
      containers:
      - name: agent
        image: ghcr.io/acme/agent:latest
        securityContext: {privileged: true}
      - name: pinned
        image: ghcr.io/acme/pinned@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
      - name: versioned
        image: registry.local:5000/team/app:1.2.3
---
apiVersion: v1
kind: Secret
metadata: {name: creds}
`
	docs := parseManifest(manifest, "apps")
	if len(docs) != 5 {
		t.Fatalf("parsed %d documents", len(docs))
	}
	warnings, images := hubReview(docs, []helmResource{{Kind: "CustomResourceDefinition", Name: "things.acme.io"}})
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{
		"cluster-wide objects", "ClusterRole (1)", "ClusterRoleBinding (1)", "CustomResourceDefinition (1)", "ValidatingWebhookConfiguration (1)",
		"creates 1 Secret", "elevated privileges", "DaemonSet/agent",
		"admission webhooks (ValidatingWebhookConfiguration/policy)",
		"cluster-admin role", "ClusterRoleBinding/admin-everything",
		"every verb on every resource", "ClusterRole/everything",
		"not pinned", "busybox", "ghcr.io/acme/agent:latest",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "registry.local:5000/team/app:1.2.3") || strings.Contains(joined, "pinned@sha256") {
		t.Errorf("a pinned image is flagged:\n%s", joined)
	}
	if len(images) != 4 {
		t.Errorf("images = %v", images)
	}
	for _, w := range warnings {
		if !strings.HasPrefix(w, "warning:") {
			t.Errorf("a warning without the prefix: %q", w)
		}
	}
}

func TestUsesLatest(t *testing.T) {
	for image, want := range map[string]bool{
		"nginx": true, "nginx:latest": true, "nginx:1.27": false, "ghcr.io/a/b": true, "ghcr.io/a/b:latest": true,
		"registry:5000/a": true, "registry:5000/a:v1": false, "a/b@sha256:abc": false, "a/b:latest@sha256:abc": false,
	} {
		if got := usesLatest(image); got != want {
			t.Errorf("usesLatest(%q) = %v; want %v", image, got, want)
		}
	}
}

func TestQueryAndTextHelpers(t *testing.T) {
	if got, err := validQuery("  Cert   Manager "); err != nil || got != "Cert Manager" {
		t.Errorf("validQuery = %q, %v", got, err)
	}
	if got := truncate("one   two\nthree", 7); got != "one two…" {
		t.Errorf("truncate = %q", got)
	}
	if got := firstNonEmpty("", "b", "c"); got != "b" {
		t.Errorf("firstNonEmpty = %q", got)
	}
	merged := mergeValues(map[string]any{"a": map[string]any{"b": 1, "c": 2}, "d": 3}, map[string]any{"a": map[string]any{"b": 9}, "e": 4})
	flat := map[string]any{}
	flatten("", merged, flat)
	if flat["a.b"] != 9 || flat["a.c"] != 2 || flat["d"] != 3 || flat["e"] != 4 {
		t.Errorf("merged = %v", flat)
	}
	masked := maskedSettings(map[string]any{"db": map[string]any{"password": "hunter2hunter2"}, "replicas": 3})
	if masked["db.password"] != "<secret>" || masked["replicas"] != "3" {
		t.Errorf("masked = %v", masked)
	}
	if secrets := hubSecrets(map[string]any{"db": map[string]any{"password": "hunter2hunter2", "enabled": true}, "token": "abc"}); len(secrets) != 1 || secrets[0] != "hunter2hunter2" {
		t.Errorf("secrets = %v (short values and non-strings are not treated as secrets)", secrets)
	}
}

func TestHubRefsAreBoundedAndExpire(t *testing.T) {
	c := newHubClient()
	clock := time.Now()
	c.now = func() time.Time { return clock }

	// A search brings at most one page of packages, so the references arrive in small batches.
	var packages []hubPackage
	for i := range hubRefMax + 500 {
		id := "00000000-0000-4000-8000-" + strings.Repeat("0", 12-len(strconv.Itoa(i))) + strconv.Itoa(i)
		packages = append(packages, hubPackage{PackageID: id, NormalizedName: "demo", Repository: hubRepository{Name: "acme"}})
		if len(packages)%hubPageSize == 0 {
			clock = clock.Add(time.Second)
			c.remember(packages[len(packages)-hubPageSize:])
		}
	}
	c.mu.Lock()
	n := len(c.refs)
	c.mu.Unlock()
	if n > hubRefMax {
		t.Errorf("%d refs; the limit is %d", n, hubRefMax)
	}

	last := packages[len(packages)-1].PackageID
	if _, ok := c.lookup(last); !ok {
		t.Error("the newest package should be known")
	}
	clock = clock.Add(hubRefTTL + time.Minute)
	if _, ok := c.lookup(last); ok {
		t.Error("an old reference should expire")
	}

	// Names that could not be a path segment or a label are never stored.
	c.remember([]hubPackage{{PackageID: idDemo, NormalizedName: "../x", Repository: hubRepository{Name: "acme"}}})
	if _, ok := c.lookup(idDemo); ok {
		t.Error("a malformed name was stored")
	}
}
