package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ---- switches -------------------------------------------------------------------------------------

// hubAllowed refuses everything that talks to Artifact Hub when the console runs with --catalog-only.
func (s *Server) hubAllowed() error {
	if s.opts.CatalogOnly {
		return &apiError{status: http.StatusForbidden, code: "catalog_only",
			msg:  "the console runs with --catalog-only, so it installs only the apps of the catalog",
			hint: "Restart without --catalog-only to search Artifact Hub."}
	}
	return nil
}

type helmCapabilities struct {
	Search     bool `json:"search"`
	HubInstall bool `json:"hubInstall"`
}

func (s *Server) handleHelmCapabilities(w http.ResponseWriter, _ *http.Request) {
	enabled := !s.opts.CatalogOnly
	writeJSON(w, http.StatusOK, helmCapabilities{Search: enabled, HubInstall: enabled})
}

// ---- search ----------------------------------------------------------------------------------------

func (s *Server) handleHelmSearch(w http.ResponseWriter, r *http.Request) {
	if err := s.hubAllowed(); err != nil {
		writeErr(w, err)
		return
	}
	query, err := validQuery(r.URL.Query().Get("q"))
	if err != nil {
		writeErr(w, err)
		return
	}
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > hubMaxPage {
			writeErr(w, badRequest("page must be a number from 1 to %d", hubMaxPage))
			return
		}
		page = n
	}

	// The request's own context goes down to the HTTP call, so a search the browser
	// abandoned (a new keystroke aborts the old one) stops with it.
	result, err := s.helm.hub.search(r.Context(), s.helm.catalog, query, page)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ---- install from search -----------------------------------------------------------------------------

// hubChangeResponse is the answer of an install or upgrade from search: a
// changeResponse plus what the person has to review and send back.
type hubChangeResponse struct {
	Context    string   `json:"context"`
	Kind       string   `json:"kind"`
	Namespace  string   `json:"namespace,omitempty"`
	Name       string   `json:"name"`
	Action     string   `json:"action"`
	DryRun     bool     `json:"dryRun"`
	Summary    string   `json:"summary"`
	Changes    []string `json:"changes"`
	Equivalent string   `json:"equivalent"`

	Source    string `json:"source"`
	PackageID string `json:"packageId"`
	Chart     string `json:"chart"`
	Version   string `json:"version"`
	// Digest is the sha256 of the archive that was reviewed; the apply has to send it back.
	Digest string `json:"digest"`
	// ConfirmPhrase is what has to be typed to apply: the chart name.
	ConfirmPhrase string   `json:"confirmPhrase"`
	Images        []string `json:"images"`
}

func (r hubChangeResponse) withDefaults() hubChangeResponse {
	if r.Changes == nil {
		r.Changes = []string{}
	}
	if r.Images == nil {
		r.Images = []string{}
	}
	return r
}

// hubChart is a package of Artifact Hub, fetched again, checked and downloaded.
type hubChart struct {
	ref    hubRef
	detail *hubDetail
	data   []byte
	digest string
}

// fresh decodes a new copy of the chart, because Helm edits the chart it is given.
func (h *hubChart) fresh() (*chart.Chart, error) {
	return loader.LoadArchive(bytes.NewReader(h.data))
}

// resolveHub asks Artifact Hub for the package by its id, applies the rules of
// who can be installed, downloads the archive and works out its sha256. Nothing
// the browser sent but the id and a version is used.
func (s *Server) resolveHub(ctx context.Context, ref hubRef, packageID, version string) (*hubChart, *chart.Chart, error) {
	detail, err := s.helm.hub.detail(ctx, ref, version)
	if err != nil {
		return nil, nil, err
	}
	if detail.PackageID != packageID {
		return nil, nil, &apiError{status: http.StatusConflict, code: "package_changed",
			msg: "Artifact Hub returned a different package for this id", hint: "Search for it again."}
	}
	if version != "" && detail.Version != version {
		return nil, nil, badRequest("Artifact Hub has no version %q of this chart", version)
	}
	if ok, reason := detailEligibility(detail); !ok {
		return nil, nil, &apiError{status: http.StatusBadRequest, code: "not_installable", msg: "this chart cannot be installed here: " + reason}
	}
	for _, label := range []string{ref.repo, ref.name} {
		if problems := validation.IsValidLabelValue(label); len(problems) > 0 {
			return nil, nil, &apiError{status: http.StatusBadRequest, code: "not_installable",
				msg: "this chart cannot be installed here: its repository or chart name is too long to be recorded on the release"}
		}
	}

	data, sum, err := s.helm.download(ctx, detail.ContentURL, detail.Name)
	if err != nil {
		return nil, nil, hubUnavailable("%v", err)
	}
	if detail.Digest != "" && !strings.EqualFold(detail.Digest, sum) {
		return nil, nil, &apiError{status: http.StatusBadGateway, code: "digest_mismatch",
			msg:  "the chart archive does not match the digest that Artifact Hub reports, so it was not used",
			hint: "The repository changed the file or the download was tampered with. Nothing was installed."}
	}
	hc := &hubChart{ref: ref, detail: detail, data: data, digest: sum}

	loaded, err := hc.fresh()
	if err != nil {
		return nil, nil, fmt.Errorf("the chart could not be read: %w", err)
	}
	if loaded.Metadata == nil || loaded.Metadata.Version != detail.Version {
		return nil, nil, &apiError{status: http.StatusBadGateway, code: "digest_mismatch", msg: "the archive is not the version Artifact Hub describes"}
	}
	if loaded.Metadata.Type == "library" {
		return nil, nil, &apiError{status: http.StatusBadRequest, code: "not_installable", msg: "this chart cannot be installed here: library charts cannot be installed"}
	}
	return hc, loaded, nil
}

// reviewedDigest enforces rule 3: the apply has to send back the digest that was previewed.
func reviewedDigest(sent, actual string) error {
	if strings.EqualFold(sent, actual) && sent != "" {
		return nil
	}
	return &apiError{status: http.StatusConflict, code: "digest_changed",
		msg:  "the chart archive is not the one that was previewed, so nothing was applied",
		hint: "Preview again and review what changed."}
}

// typedPhrase enforces rule 5: for charts outside the catalog the chart name is typed.
func typedPhrase(sent, chartName string) error {
	if sent == chartName {
		return nil
	}
	return &apiError{status: http.StatusConflict, code: "phrase_required",
		msg: fmt.Sprintf("type the chart name (%s) to apply a chart that is not in the catalog", chartName)}
}

func hubLabels(ref hubRef, packageID string) map[string]string {
	return map[string]string{
		helmManagedLabel: "true", helmSourceLabel: sourceHub, helmPackageLabel: packageID,
		helmHubRepoLabel: ref.repo, helmHubNameLabel: ref.name,
	}
}

// hubChartCommand is the equivalent helm command of an install or upgrade from search.
func hubChartCommand(verb string, hc *hubChart, loaded *chart.Chart, name, namespace string, createNamespace bool, flags []string) string {
	parts := []string{"helm", verb, kubectlQuote(name), kubectlQuote(loaded.Metadata.Name)}
	if repo := hc.detail.Repository.URL; strings.HasPrefix(repo, "https://") {
		parts = append(parts, "--repo", kubectlQuote(repo))
	}
	parts = append(parts, "--version", kubectlQuote(hc.detail.Version), "-n", kubectlQuote(namespace))
	if createNamespace {
		parts = append(parts, "--create-namespace")
	}
	if verb == "upgrade" {
		parts = append(parts, "--reset-then-reuse-values")
	}
	return strings.Join(append(parts, flags...), " ")
}

// publisherLine says who published the chart, for the first line of the review.
func publisherLine(hc *hubChart) string {
	flags := []string{}
	if hc.detail.Repository.VerifiedPublisher {
		flags = append(flags, "verified publisher")
	}
	if hc.detail.Repository.Official || hc.detail.Official {
		flags = append(flags, "official")
	}
	if hc.detail.Signed {
		flags = append(flags, "signed")
	}
	return fmt.Sprintf("chart: %s %s from repository %s (%s)", hc.detail.Name, hc.detail.Version, hc.ref.repo, strings.Join(flags, ", "))
}

func (s *Server) hubInstall(w http.ResponseWriter, r *http.Request, req *helmInstallRequest) {
	if err := s.hubAllowed(); err != nil {
		writeErr(w, err)
		return
	}
	if !packageIDPattern.MatchString(req.PackageID) {
		writeErr(w, badRequest("packageId is not a package id"))
		return
	}
	if req.Version != "" && !versionPattern.MatchString(req.Version) {
		writeErr(w, badRequest("version is not a chart version"))
		return
	}
	if req.Namespace == "" {
		writeErr(w, badRequest("a namespace is required"))
		return
	}
	namespace, err := validNamespace(req.Namespace)
	if err != nil {
		writeErr(w, err)
		return
	}
	if req.CreateNamespace && strings.HasPrefix(namespace, "kube-") {
		writeErr(w, badRequest("namespaces starting with kube- are reserved for Kubernetes"))
		return
	}
	// Settings are checked against the chart's own values below, but their shape is
	// bounded here, before anything is downloaded.
	if len(req.Settings) > maxHubSettings {
		writeErr(w, badRequest("too many settings (at most %d)", maxHubSettings))
		return
	}

	contextName, err := s.authorize(req.mutation)
	if err != nil {
		writeErr(w, err)
		return
	}
	ref, ok := s.helm.hub.lookup(req.PackageID)
	if !ok {
		writeErr(w, &apiError{status: http.StatusNotFound, code: "unknown_package",
			msg: "this package is not known to the console", hint: "Search for it again, then install from the result."})
		return
	}
	if !req.DryRun {
		if err := typedPhrase(req.ConfirmPhrase, ref.name); err != nil {
			writeErr(w, err)
			return
		}
	}

	ctx, cancel := writeContext(r, req.DryRun)
	defer cancel()
	free, err := s.helm.acquire(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer free()

	hc, loaded, err := s.resolveHub(ctx, ref, req.PackageID, req.Version)
	if err != nil {
		writeHelmErr(w, err, nil)
		return
	}
	if !req.DryRun {
		if err := reviewedDigest(req.Digest, hc.digest); err != nil {
			writeErr(w, err)
			return
		}
	}

	values, _, err := checkSettings(loaded, req.Settings)
	if err != nil {
		writeErr(w, err)
		return
	}
	secrets := hubSecrets(values)

	name := req.Name
	if name == "" {
		name = strings.ToLower(loaded.Metadata.Name)
	}
	if err := validReleaseName(name); err != nil {
		writeErr(w, err)
		return
	}

	cfg, err := s.helm.configFor(contextName, namespace)
	if err != nil {
		writeHelmErr(w, err, secrets)
		return
	}
	if err := takeName(cfg, name); err != nil {
		writeHelmErr(w, err, secrets)
		return
	}
	// From here the chart renders: it may not read Secrets while it does.
	blindToSecrets(cfg)
	if !req.DryRun {
		done, err := s.helm.begin(contextName, namespace, name)
		if err != nil {
			writeHelmErr(w, err, secrets)
			return
		}
		defer done()
	}

	rel, withCRDs, note, err := s.execInstall(ctx, cfg, installRun{
		contextName: contextName, name: name, namespace: namespace, createNamespace: req.CreateNamespace,
		labels: hubLabels(ref, req.PackageID), values: values, dryRun: req.DryRun, load: hc.fresh,
	})
	if err != nil {
		writeHelmErr(w, err, secrets)
		return
	}

	docs := withExtras(parseManifest(rel.Manifest, namespace), rel, withCRDs, namespace)
	crds := crdsOf(withCRDs)
	warnings, images := hubReview(docs, crds)
	if chartLooksUp(loaded) {
		warnings = append(warnings, "warning: the chart reads existing objects of the cluster while it renders (`lookup`); reading Secrets is blocked for charts from outside the catalog")
	}
	changes := append([]string{publisherLine(hc)}, warnings...)
	if note != "" {
		changes = append(changes, note)
	}
	for _, image := range images {
		changes = append(changes, "image: "+image)
	}
	changes = append(changes, resourceLines("", append(resourcesOf(docs), crds...))...)

	summary := fmt.Sprintf("%s %s would be installed as release %q in namespace %q", hc.detail.Name, hc.detail.Version, name, namespace)
	if !req.DryRun {
		summary = fmt.Sprintf("%s %s installed as release %q in namespace %q; pods may take a few minutes to start", hc.detail.Name, hc.detail.Version, name, namespace)
	}
	writeJSON(w, http.StatusOK, hubChangeResponse{
		Context: contextName, Kind: "HelmRelease", Namespace: namespace, Name: name, Action: "created", DryRun: req.DryRun,
		Summary: summary, Changes: changes,
		Equivalent: hubChartCommand("install", hc, loaded, name, namespace, req.CreateNamespace, helmSetFlags(displayValues(values))),
		Source:     sourceHub, PackageID: req.PackageID, Chart: loaded.Metadata.Name, Version: hc.detail.Version,
		Digest: hc.digest, ConfirmPhrase: ref.name, Images: images,
	}.withDefaults())
}

// ---- upgrade of a release installed from search -----------------------------------------------------------

// hubUpgrade upgrades a release that came from a search. It goes through the same
// review as an install: the package is fetched again by id, the new version is
// downloaded, its digest is previewed and has to be sent back, and the chart name
// has to be typed.
func (s *Server) hubUpgrade(ctx context.Context, w http.ResponseWriter, req *helmUpgradeRequest, cfg *action.Configuration, rel *release.Release, contextName string) {
	if err := s.hubAllowed(); err != nil {
		writeErr(w, err)
		return
	}
	packageID := rel.Labels[helmPackageLabel]
	ref := hubRef{repo: rel.Labels[helmHubRepoLabel], name: rel.Labels[helmHubNameLabel]}
	if !packageIDPattern.MatchString(packageID) || !hubNamePattern.MatchString(ref.repo) || !hubNamePattern.MatchString(ref.name) {
		writeErr(w, &apiError{status: http.StatusConflict, code: "unknown_app", msg: "this release does not record where its chart came from"})
		return
	}
	if req.Version != "" && !versionPattern.MatchString(req.Version) {
		writeErr(w, badRequest("version is not a chart version"))
		return
	}
	if len(req.Settings) > maxHubSettings {
		writeErr(w, badRequest("too many settings (at most %d)", maxHubSettings))
		return
	}
	secrets := hubSecrets(rel.Config)

	free, err := s.helm.acquire(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer free()
	blindToSecrets(cfg)

	hc, loaded, err := s.resolveHub(ctx, ref, packageID, req.Version)
	if err != nil {
		writeHelmErr(w, err, secrets)
		return
	}

	over, paths, err := checkSettings(loaded, req.Settings)
	if err != nil {
		writeErr(w, err)
		return
	}
	secrets = append(secrets, hubSecrets(over)...)

	oldFlat := map[string]any{}
	flatten("", rel.Config, oldFlat)
	newFlat := map[string]any{}
	flatten("", over, newFlat)
	var changes []string
	changedValues := map[string]any{}
	for _, path := range paths {
		old, had := oldFlat[path]
		if !had && rel.Chart != nil {
			// Never set: the value in effect is the default of the chart that is installed.
			old, had = getPath(rel.Chart.Values, path)
		}
		now := newFlat[path]
		secret := secretishPath.MatchString(path)
		if had && !secret && fmt.Sprint(old) == fmt.Sprint(now) {
			continue
		}
		setPath(changedValues, path, now)
		switch {
		case secret:
			changes = append(changes, path+": changed")
		case !had:
			changes = append(changes, fmt.Sprintf("%s: (not set) -> %q", path, short(fmt.Sprint(now))))
		default:
			changes = append(changes, fmt.Sprintf("%s: %q -> %q", path, short(fmt.Sprint(old)), short(fmt.Sprint(now))))
		}
	}

	installedVersion := ""
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		installedVersion = rel.Chart.Metadata.Version
	}
	if installedVersion != hc.detail.Version {
		changes = append([]string{fmt.Sprintf("chart: %s -> %s", installedVersion, hc.detail.Version)}, changes...)
	}
	equivalent := hubChartCommand("upgrade", hc, loaded, req.Name, rel.Namespace, false, helmSetFlags(displayValues(changedValues)))

	answer := hubChangeResponse{
		Context: contextName, Kind: "HelmRelease", Namespace: rel.Namespace, Name: req.Name, DryRun: req.DryRun, Equivalent: equivalent,
		Source: sourceHub, PackageID: packageID, Chart: loaded.Metadata.Name, Version: hc.detail.Version,
		Digest: hc.digest, ConfirmPhrase: ref.name,
	}
	if len(changedValues) == 0 && installedVersion == hc.detail.Version {
		answer.Action, answer.Summary = "unchanged", fmt.Sprintf("%s already has these settings", req.Name)
		writeJSON(w, http.StatusOK, answer.withDefaults())
		return
	}

	// Applying needs what the preview showed: the chart name typed and the digest reviewed.
	if !req.DryRun {
		if err := typedPhrase(req.ConfirmPhrase, ref.name); err != nil {
			writeErr(w, err)
			return
		}
		if err := reviewedDigest(req.Digest, hc.digest); err != nil {
			writeErr(w, err)
			return
		}
		done, err := s.helm.begin(contextName, rel.Namespace, req.Name)
		if err != nil {
			writeHelmErr(w, err, secrets)
			return
		}
		defer done()
	}

	updated, err := s.execUpgrade(ctx, cfg, upgradeRun{
		name: req.Name, namespace: rel.Namespace, labels: hubLabels(ref, packageID), dryRun: req.DryRun,
		values: mergeValues(rel.Config, over), load: hc.fresh,
	})
	if err != nil {
		writeHelmErr(w, err, secrets)
		return
	}

	docs := parseManifest(updated.Manifest, rel.Namespace)
	warnings, images := hubReview(withExtras(docs, updated, loaded, rel.Namespace), crdsOf(loaded))
	if chartLooksUp(loaded) {
		warnings = append(warnings, "warning: the chart reads existing objects of the cluster while it renders (`lookup`); reading Secrets is blocked for charts from outside the catalog")
	}
	added, removed := diffResources(parseManifest(rel.Manifest, rel.Namespace), docs)
	all := append([]string{publisherLine(hc)}, warnings...)
	for _, image := range images {
		all = append(all, "image: "+image)
	}
	all = append(all, changes...)
	all = append(all, resourceLines("+ ", added)...)
	all = append(all, resourceLines("- ", removed)...)

	answer.Action, answer.Changes, answer.Images = "updated", all, images
	answer.Summary = fmt.Sprintf("Release %q updated; pods roll out one by one", req.Name)
	if req.DryRun {
		answer.Summary = fmt.Sprintf("Release %q would be updated", req.Name)
	}
	writeJSON(w, http.StatusOK, answer.withDefaults())
}

// ---- the review ---------------------------------------------------------------------------------------------

// hubReview is rule 5: what a chart from outside the catalog does, as lines a
// person can read before applying. warnings come first and start with "warning:".
func hubReview(docs []manifestDoc, crds []helmResource) (warnings, images []string) {
	warnings = warningsFor(docs, crds)

	var webhooks, adminBindings, wildcardRoles []string
	seen := map[string]bool{}
	for _, d := range docs {
		label := safeLine(d.Kind + "/" + d.Metadata.Name)
		switch d.Kind {
		case "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration":
			webhooks = append(webhooks, label)
		case "ClusterRoleBinding", "RoleBinding":
			if role, ok := d.raw["roleRef"].(map[string]any); ok && role["name"] == "cluster-admin" {
				adminBindings = append(adminBindings, label)
			}
		case "ClusterRole", "Role":
			if grantsEverything(d.raw["rules"]) {
				wildcardRoles = append(wildcardRoles, label)
			}
		}
		for _, image := range imagesOf(d.raw) {
			seen[safeLine(image)] = true
		}
	}

	if len(webhooks) > 0 {
		sort.Strings(webhooks)
		warnings = append(warnings, "warning: registers admission webhooks ("+listNames(webhooks)+"): if their service is down the API server can delay or reject changes")
	}
	if len(adminBindings) > 0 {
		sort.Strings(adminBindings)
		warnings = append(warnings, "warning: binds the cluster-admin role, which is full control of the cluster ("+listNames(adminBindings)+")")
	}
	if len(wildcardRoles) > 0 {
		sort.Strings(wildcardRoles)
		warnings = append(warnings, "warning: grants every verb on every resource ("+listNames(wildcardRoles)+")")
	}

	images = make([]string, 0, len(seen))
	for image := range seen {
		images = append(images, image)
	}
	sort.Strings(images)
	var latest []string
	for _, image := range images {
		if usesLatest(image) {
			latest = append(latest, image)
		}
	}
	if len(latest) > 0 {
		warnings = append(warnings, "warning: runs images that are not pinned to a version, so they can change under you (:latest or no tag): "+listNames(latest))
	}
	return warnings, images
}

// grantsEverything reports a role rule with a wildcard verb and a wildcard resource.
func grantsEverything(rules any) bool {
	list, _ := rules.([]any)
	for _, item := range list {
		rule, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if contains(rule["verbs"], "*") && contains(rule["resources"], "*") {
			return true
		}
	}
	return false
}

func contains(list any, want string) bool {
	items, _ := list.([]any)
	for _, item := range items {
		if text, ok := item.(string); ok && text == want {
			return true
		}
	}
	return false
}

// imagesOf collects the images of every container list in an object.
func imagesOf(node any) []string {
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			for key, value := range t {
				if key == "containers" || key == "initContainers" || key == "ephemeralContainers" {
					if list, ok := value.([]any); ok {
						for _, item := range list {
							if container, ok := item.(map[string]any); ok {
								if image, ok := container["image"].(string); ok && image != "" {
									out = append(out, image)
								}
							}
						}
					}
				}
				walk(value)
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	walk(node)
	return out
}

// usesLatest reports an image that is not pinned: no tag, or the tag "latest", and no digest.
func usesLatest(image string) bool {
	if strings.Contains(image, "@sha256:") {
		return false
	}
	last := image[strings.LastIndex(image, "/")+1:] // the tag colon is after the last slash
	i := strings.LastIndex(last, ":")
	return i < 0 || last[i+1:] == "latest"
}
