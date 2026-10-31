package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/yaml"
)

// The Helm engine of the console (ADR 0004). It installs only apps of the
// catalog, from archives whose sha256 is pinned, into the context the request
// names, and keeps release state where Helm does: Secrets in the release
// namespace, so `helm list` shows the same releases.

const (
	// labels the console puts on the releases it installs. Upgrade and uninstall
	// act on those releases only.
	helmManagedLabel = "k8scli.io/managed"
	helmCatalogLabel = "k8scli.io/catalog"

	maxChartArchive = 16 << 20 // bytes; the biggest chart of the catalog is under 1 MiB
	chartFetchLimit = 2 * time.Minute

	helmDryRunTimeout = 3 * time.Minute
	helmWriteTimeout  = 6 * time.Minute // the release itself times out earlier, at helmReleaseTimeout
	// helmReleaseTimeout bounds the hooks of the release; Helm is not asked to wait for readiness.
	helmReleaseTimeout = 5 * time.Minute

	maxReleaseNameLength = 53 // Helm's own limit
	maxListedResources   = 200
	minRedacted          = 4 // shorter secrets would turn ordinary words into placeholders
)

// redirectSuffixes are the hosts a chart download may be redirected to: the
// project pages and release assets of GitHub, where most chart archives live.
// The sha256 check makes the host irrelevant for trust; this only keeps a
// download from wandering.
var redirectSuffixes = []string{"github.com", "githubusercontent.com"}

// helmEngine holds what the Helm endpoints need. Tests replace the fields.
type helmEngine struct {
	catalog []helmEntry

	// configFor builds the Helm action configuration for a context and a
	// namespace ("" means all namespaces, for listing).
	configFor func(contextName, namespace string) (*action.Configuration, error)
	// httpClient downloads chart archives; it only follows HTTPS redirects to allowed hosts.
	httpClient *http.Client

	// hub talks to Artifact Hub (search and install from search).
	hub *hubClient

	// slots bounds the charts from search being fetched and rendered at once.
	slots chan struct{}

	mu       sync.Mutex
	archives map[string][]byte // id -> verified archive bytes
	busy     map[string]bool   // releases with a write in progress
}

func newHelmEngine(s *Server) *helmEngine {
	h := &helmEngine{
		catalog:  helmCatalog,
		hub:      newHubClient(),
		slots:    make(chan struct{}, maxHubWork),
		archives: map[string][]byte{},
		busy:     map[string]bool{},
	}
	h.configFor = s.newHelmConfig
	h.httpClient = &http.Client{
		Timeout:   chartFetchLimit,
		Transport: guardedTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects while downloading the chart")
			}
			if req.URL.Scheme != "https" || req.URL.User != nil {
				return errors.New("the chart download was redirected away from plain HTTPS")
			}
			if !hostAllowed(req.URL.Hostname(), via[0].URL.Hostname()) {
				return fmt.Errorf("the chart download was redirected to %s, which is not allowed", req.URL.Hostname())
			}
			return nil
		},
	}
	return h
}

// hostAllowed reports whether a redirect may go to host: the host the download
// started on, or a GitHub host.
func hostAllowed(host, origin string) bool {
	host = strings.ToLower(host)
	if host == strings.ToLower(origin) {
		return true
	}
	for _, suffix := range redirectSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// newHelmConfig is the production configFor. It is built per request from the
// same kubeconfig and context the other endpoints use, and never reads Helm's
// own repository or settings files.
func (s *Server) newHelmConfig(contextName, namespace string) (*action.Configuration, error) {
	flags := genericclioptions.NewConfigFlags(false)
	if s.opts.Kubeconfig != "" {
		kubeconfigPath := s.opts.Kubeconfig
		flags.KubeConfig = &kubeconfigPath
	}
	flags.Context = &contextName
	flags.Namespace = &namespace
	cache := helmDiscoveryCache()
	flags.CacheDir = &cache

	cfg := new(action.Configuration)
	if err := cfg.Init(flags, namespace, "secret", func(string, ...any) {}); err != nil {
		return nil, fmt.Errorf("failed to prepare Helm for context %q: %w", contextName, err)
	}
	return cfg, nil
}

// helmDiscoveryCache keeps API discovery files out of ~/.kube/cache.
func helmDiscoveryCache() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "k8scli", "discovery")
}

// ---- chart download ---------------------------------------------------------------

// loadChart returns a fresh copy of the catalog entry's chart. The archive is
// downloaded once, checked against the pinned sha256, and kept in memory; the
// chart is decoded again for every use because Helm edits the chart it is given.
func (h *helmEngine) loadChart(ctx context.Context, e *helmEntry) (*chart.Chart, error) {
	data, err := h.archive(ctx, e)
	if err != nil {
		return nil, err
	}
	loaded, err := loader.LoadArchive(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("the %s chart could not be read: %w", e.Name, err)
	}
	if loaded.Metadata == nil || loaded.Metadata.Name != e.Chart || loaded.Metadata.Version != e.Version {
		return nil, fmt.Errorf("the downloaded archive is not %s %s", e.Chart, e.Version)
	}
	return loaded, nil
}

func (h *helmEngine) archive(ctx context.Context, e *helmEntry) ([]byte, error) {
	h.mu.Lock()
	cached, ok := h.archives[e.ID]
	h.mu.Unlock()
	if ok {
		return cached, nil
	}

	data, sum, err := h.download(ctx, e.archiveURL, e.Name)
	if err != nil {
		return nil, err
	}
	if sum != e.digest {
		return nil, &apiError{status: http.StatusBadGateway, code: "digest_mismatch",
			msg:  fmt.Sprintf("the %s chart does not match the digest pinned in the catalog, so it was not installed", e.Name),
			hint: "The upstream archive changed or the download was tampered with. Nothing was installed."}
	}

	h.mu.Lock()
	h.archives[e.ID] = data
	h.mu.Unlock()
	return data, nil
}

// download fetches a chart archive over HTTPS (HTTPS redirects stay on the same
// host or a GitHub host), refuses anything over the size cap, and returns the
// bytes with their sha256. It does not decide whether that sha256 is acceptable.
func (h *helmEngine) download(ctx context.Context, address, label string) ([]byte, string, error) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return nil, "", fmt.Errorf("the chart address of %s is not a plain HTTPS URL", label)
	}

	ctx, cancel := context.WithTimeout(ctx, chartFetchLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, http.NoBody)
	if err != nil {
		return nil, "", err
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("could not download the %s chart: %w", label, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("could not download the %s chart: the server answered %s", label, resp.Status)
	}
	if resp.ContentLength > maxChartArchive {
		return nil, "", fmt.Errorf("the %s chart is larger than the %d MiB limit", label, maxChartArchive>>20)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxChartArchive+1))
	if err != nil {
		return nil, "", fmt.Errorf("could not download the %s chart: %w", label, err)
	}
	if len(data) > maxChartArchive {
		return nil, "", fmt.Errorf("the %s chart is larger than the %d MiB limit", label, maxChartArchive>>20)
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// ---- guards ------------------------------------------------------------------------

// begin marks a release as being written. A second write to the same release
// is refused instead of racing the first.
func (h *helmEngine) begin(contextName, namespace, name string) (func(), error) {
	key := contextName + "/" + namespace + "/" + name
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.busy[key] {
		return nil, &apiError{status: http.StatusConflict, code: "busy", msg: fmt.Sprintf("release %q is being changed right now", name),
			hint: "Wait for it to finish, then reload."}
	}
	h.busy[key] = true
	return func() {
		h.mu.Lock()
		delete(h.busy, key)
		h.mu.Unlock()
	}, nil
}

func validReleaseName(name string) error {
	if name == "" {
		return badRequest("a release name is required")
	}
	if len(name) > maxReleaseNameLength {
		return badRequest("the release name is too long (at most %d characters)", maxReleaseNameLength)
	}
	if _, err := validLabelName("release", name); err != nil {
		return err
	}
	return nil
}

// releaseSource says where a release came from: "hub" (installed from an
// Artifact Hub search), "catalog", or "other" for everything the console did not install.
func releaseSource(rel *release.Release) string {
	switch {
	case !isManaged(rel):
		return "other"
	case rel.Labels[helmSourceLabel] == sourceHub:
		return sourceHub
	default:
		return sourceCatalog
	}
}

// isManaged reports whether the console installed a release.
func isManaged(rel *release.Release) bool {
	return rel.Labels[helmManagedLabel] == "true"
}

// ---- manifests -----------------------------------------------------------------------

// helmResource is one object of a rendered release.
type helmResource struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

func (r helmResource) String() string {
	if r.Namespace == "" {
		return safeLine(r.Kind + "/" + r.Name)
	}
	return safeLine(fmt.Sprintf("%s/%s (%s)", r.Kind, r.Name, r.Namespace))
}

// clusterScopedKinds are the kinds that live outside a namespace and so can
// reach beyond where the app is installed.
var clusterScopedKinds = map[string]bool{
	"ClusterRole": true, "ClusterRoleBinding": true, "CustomResourceDefinition": true,
	"MutatingWebhookConfiguration": true, "ValidatingWebhookConfiguration": true,
	"ValidatingAdmissionPolicy": true, "ValidatingAdmissionPolicyBinding": true,
	"APIService": true, "PriorityClass": true, "StorageClass": true, "IngressClass": true,
	"PersistentVolume": true, "Namespace": true, "ClusterIssuer": true, "RuntimeClass": true,
}

// manifestDoc is the part of an object the preview needs.
type manifestDoc struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	raw map[string]any

	hook       string // events of a hook object ("pre-install"), empty for a normal object
	fromCRDDir bool   // an object of a crds/ directory that is not a CustomResourceDefinition
	releaseNS  string // the namespace of the release, to spot objects placed elsewhere
}

// parseManifest splits rendered YAML into the objects it contains.
func parseManifest(manifest, defaultNamespace string) []manifestDoc {
	var docs []manifestDoc
	parts := releaseutil.SplitManifests(manifest)
	keys := make([]string, 0, len(parts))
	for key := range parts {
		keys = append(keys, key)
	}
	// The keys are manifest-0, manifest-1, ...; numeric order keeps Helm's install order.
	sort.Slice(keys, func(i, j int) bool { return manifestIndex(keys[i]) < manifestIndex(keys[j]) })
	for _, key := range keys {
		var doc manifestDoc
		if err := yaml.Unmarshal([]byte(parts[key]), &doc); err != nil || doc.Kind == "" || doc.Metadata.Name == "" {
			continue
		}
		_ = yaml.Unmarshal([]byte(parts[key]), &doc.raw)
		if doc.Metadata.Namespace == "" && !clusterScopedKinds[doc.Kind] {
			doc.Metadata.Namespace = defaultNamespace
		}
		doc.releaseNS = defaultNamespace
		docs = append(docs, doc)
	}
	return docs
}

func manifestIndex(key string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(key, "manifest-"))
	if err != nil {
		return 0
	}
	return n
}

func resourcesOf(docs []manifestDoc) []helmResource {
	out := make([]helmResource, 0, len(docs))
	for _, d := range docs {
		out = append(out, helmResource{Kind: d.Kind, Name: d.Metadata.Name, Namespace: d.Metadata.Namespace})
	}
	return out
}

// crdsOf lists the CustomResourceDefinitions in the chart's crds/ directory.
// Helm installs them first and never deletes them, and they are not in the manifest.
func crdsOf(c *chart.Chart) []helmResource {
	var out []helmResource
	if c == nil {
		return out
	}
	for _, crd := range c.CRDObjects() {
		for _, doc := range parseManifest(string(crd.File.Data), "") {
			if doc.Kind == "CustomResourceDefinition" {
				out = append(out, helmResource{Kind: doc.Kind, Name: doc.Metadata.Name})
			}
		}
	}
	return out
}

// resourceLines lists objects one per line, capped so a big chart stays readable.
func resourceLines(prefix string, resources []helmResource) []string {
	lines := make([]string, 0, len(resources)+1)
	for i, r := range resources {
		if i == maxListedResources {
			lines = append(lines, fmt.Sprintf("%s... and %d more", prefix, len(resources)-maxListedResources))
			break
		}
		lines = append(lines, prefix+r.String())
	}
	return lines
}

// warningsFor flags what deserves a second look before someone applies the
// install: cluster-wide objects, objects in other namespaces, Secrets, hooks and
// workloads that ask for privileges. docs include the hooks and the odd objects of crds/.
func warningsFor(docs []manifestDoc, crds []helmResource) []string {
	cluster := map[string]int{}
	for _, c := range crds {
		cluster[c.Kind]++
	}
	secrets := 0
	var privileged, hooks, crdDir []string
	elsewhere := map[string]int{}
	for _, d := range docs {
		label := safeLine(d.Kind + "/" + d.Metadata.Name)
		if clusterScopedKinds[d.Kind] {
			cluster[d.Kind]++
		} else if d.releaseNS != "" && d.Metadata.Namespace != "" && d.Metadata.Namespace != d.releaseNS {
			elsewhere[safeLine(d.Metadata.Namespace)]++
		}
		if d.Kind == "Secret" {
			secrets++
		}
		if asksForPrivileges(d.raw) {
			privileged = append(privileged, label)
		}
		if d.hook != "" {
			hooks = append(hooks, label+" ("+safeLine(d.hook)+")")
		}
		if d.fromCRDDir {
			crdDir = append(crdDir, label)
		}
	}

	var warnings []string
	if len(cluster) > 0 {
		kinds := make([]string, 0, len(cluster))
		for kind, n := range cluster {
			kinds = append(kinds, fmt.Sprintf("%s (%d)", safeLine(kind), n))
		}
		sort.Strings(kinds)
		warnings = append(warnings, "warning: creates cluster-wide objects that live outside the namespace: "+listNames(kinds))
	}
	if len(elsewhere) > 0 {
		places := make([]string, 0, len(elsewhere))
		for ns, n := range elsewhere {
			places = append(places, fmt.Sprintf("%s (%d)", ns, n))
		}
		sort.Strings(places)
		warnings = append(warnings, "warning: creates objects in other namespaces than the one chosen: "+listNames(places))
	}
	if len(crdDir) > 0 {
		sort.Strings(crdDir)
		warnings = append(warnings, "warning: the chart's crds/ directory holds objects that are not CustomResourceDefinitions, and Helm applies them first: "+listNames(crdDir))
	}
	if len(hooks) > 0 {
		sort.Strings(hooks)
		warnings = append(warnings, "warning: runs Helm hooks, extra objects that run while the release is installed, upgraded or removed: "+listNames(hooks))
	}
	if secrets > 0 {
		warnings = append(warnings, fmt.Sprintf("warning: creates %d Secret(s)", secrets))
	}
	if len(privileged) > 0 {
		sort.Strings(privileged)
		warnings = append(warnings, "warning: runs with elevated privileges (privileged, hostNetwork, hostPID or hostPath): "+listNames(privileged))
	}
	return warnings
}

// asksForPrivileges walks an object looking for the settings that give a pod
// more access to the node than usual.
func asksForPrivileges(node any) bool {
	switch t := node.(type) {
	case map[string]any:
		for key, value := range t {
			switch key {
			case "privileged", "hostNetwork", "hostPID", "hostIPC":
				if on, ok := value.(bool); ok && on {
					return true
				}
			case "hostPath":
				if _, ok := value.(map[string]any); ok {
					return true
				}
			}
			if asksForPrivileges(value) {
				return true
			}
		}
	case []any:
		for _, item := range t {
			if asksForPrivileges(item) {
				return true
			}
		}
	}
	return false
}

// ---- secrets ---------------------------------------------------------------------------

// redactor removes secret values from text that is about to leave the server.
type redactor []string

func (r redactor) clean(text string) string {
	for _, secret := range r {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, placeholderSecret)
		}
	}
	return text
}

// secretsOf collects the values of an entry's secret fields from given fields
// and from the values a release was installed with.
func (e *helmEntry) secretsOf(fields map[string]string, installed map[string]any) redactor {
	var secrets redactor
	for _, f := range e.Fields {
		if !f.Secret {
			continue
		}
		if v := fields[f.Key]; v != "" {
			secrets = append(secrets, v)
		}
		if v, ok := f.get(installed); ok && v != "" {
			secrets = append(secrets, v)
		}
	}
	return secrets
}

// ---- errors ------------------------------------------------------------------------------

// writeHelmErr answers a failed Helm call. The message is cleaned of secrets
// before it is sent.
func writeHelmErr(w http.ResponseWriter, err error, secrets redactor) {
	message := secrets.clean(err.Error())

	var apiErr *apiError
	if errors.As(err, &apiErr) {
		writeJSON(w, apiErr.status, errorResponse{Error: secrets.clean(apiErr.msg), Code: apiErr.code, Hint: apiErr.hint})
		return
	}
	switch {
	case errors.Is(err, driver.ErrReleaseNotFound), strings.Contains(message, "release: not found"):
		writeJSON(w, http.StatusNotFound, errorResponse{Code: "not_found", Error: "release not found"})
	case strings.Contains(message, "cannot re-use a name that is still in use"):
		writeJSON(w, http.StatusConflict, errorResponse{Code: "already_exists", Error: "a release with this name already exists in the namespace",
			Hint: "Pick another name, or upgrade the release that exists."})
	case strings.Contains(message, "exists and cannot be imported into the current release"):
		writeJSON(w, http.StatusConflict, errorResponse{Code: "already_exists", Error: message,
			Hint: "An object this app creates already exists in the cluster. Remove it or install into another namespace."})
	case isMissingCRDs(err):
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "missing_crds", Error: message,
			Hint: "The CustomResourceDefinitions of this app are not in the cluster. Reinstall the app to bring them back."})
	case strings.Contains(message, "chart requires kubeVersion"):
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "incompatible_cluster", Error: message})
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusGatewayTimeout, errorResponse{Code: "timeout", Error: "Helm did not finish in time",
			Hint: "The release may be half installed. Check it in the releases list and uninstall it if needed."})
	default:
		// Cluster errors keep their usual mapping and hints, with the message cleaned.
		writeWorkloadErr(w, redactedError{err: err, message: message})
	}
}

// redactedError keeps an error's type, so the usual mapping still applies,
// but says the cleaned message.
type redactedError struct {
	err     error
	message string
}

func (e redactedError) Error() string { return e.message }
func (e redactedError) Unwrap() error { return e.err }

// ---- release helpers -------------------------------------------------------------------------

// releaseHealth maps a Helm status to the health the UI colors.
func releaseHealth(status release.Status) string {
	switch status {
	case release.StatusDeployed:
		return healthOK
	case release.StatusFailed:
		return healthError
	case release.StatusSuperseded, release.StatusUninstalled:
		return healthDone
	default:
		return healthWarn
	}
}

// latest returns the newest revision of a release.
func latest(cfg *action.Configuration, name string) (*release.Release, error) {
	return action.NewGet(cfg).Run(name)
}

// installCommand is the equivalent `helm` command of an install or an upgrade.
func installCommand(verb string, e *helmEntry, name, namespace string, createNamespace bool, flags []string) string {
	parts := []string{"helm", verb, kubectlQuote(name), e.Chart, "--repo", e.Repo, "--version", e.Version, "-n", kubectlQuote(namespace)}
	if createNamespace {
		parts = append(parts, "--create-namespace")
	}
	if verb == "upgrade" {
		parts = append(parts, "--reset-then-reuse-values")
	}
	return strings.Join(append(parts, flags...), " ")
}
