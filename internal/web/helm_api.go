package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---- catalog ----------------------------------------------------------------------------

func (s *Server) handleHelmCatalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.helm.catalog)
}

// ---- releases (read) ---------------------------------------------------------------------

type helmReleaseJSON struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Chart        string `json:"chart"`
	ChartVersion string `json:"chartVersion"`
	AppVersion   string `json:"appVersion"`
	Status       string `json:"status"`
	Health       string `json:"health"`
	Revision     int    `json:"revision"`
	Updated      string `json:"updated"`
	Managed      bool   `json:"managed"`
	CatalogID    string `json:"catalogId"`
	// Source is "catalog" or "hub" for releases the console installed, and "other" for the rest.
	Source    string `json:"source"`
	PackageID string `json:"packageId,omitempty"`
}

func (s *Server) toHelmReleaseJSON(rel *release.Release) helmReleaseJSON {
	out := helmReleaseJSON{
		Name: rel.Name, Namespace: rel.Namespace, Revision: rel.Version,
		Managed: isManaged(rel), CatalogID: rel.Labels[helmCatalogLabel],
		Source: releaseSource(rel), PackageID: rel.Labels[helmPackageLabel],
	}
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		out.Chart, out.ChartVersion, out.AppVersion = rel.Chart.Metadata.Name, rel.Chart.Metadata.Version, rel.Chart.Metadata.AppVersion
	}
	if rel.Info != nil {
		out.Status = rel.Info.Status.String()
		out.Health = releaseHealth(rel.Info.Status)
		out.Updated = s.age(metav1.NewTime(rel.Info.LastDeployed.Time))
	}
	return out
}

func (s *Server) handleHelmReleases(w http.ResponseWriter, r *http.Request) {
	namespace, err := namespaceParam(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	contextName, err := s.resolveContext(r.URL.Query().Get("context"))
	if err != nil {
		writeErr(w, err)
		return
	}
	cfg, err := s.helm.configFor(contextName, namespace)
	if err != nil {
		writeErr(w, err)
		return
	}

	list := action.NewList(cfg)
	list.AllNamespaces = namespace == metav1.NamespaceAll
	list.StateMask = action.ListAll
	releases, err := list.Run()
	if err != nil {
		writeHelmErr(w, err, nil)
		return
	}

	out := make([]helmReleaseJSON, 0, len(releases))
	for _, rel := range releases {
		out = append(out, s.toHelmReleaseJSON(rel))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, out)
}

type helmHistoryJSON struct {
	Revision    int    `json:"revision"`
	Status      string `json:"status"`
	Chart       string `json:"chart"`
	Updated     string `json:"updated"`
	Description string `json:"description"`
}

type helmReleaseDetail struct {
	helmReleaseJSON
	Notes  string            `json:"notes"`
	Fields map[string]string `json:"fields"`
	// Settings is what a release installed from search was given, with secret-looking values hidden.
	Settings  map[string]string `json:"settings,omitempty"`
	Resources []helmResource    `json:"resources"`
	History   []helmHistoryJSON `json:"history"`
}

func (s *Server) handleHelmRelease(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	namespace, err := validNamespace(query.Get("namespace"))
	if err != nil {
		writeErr(w, err)
		return
	}
	name := query.Get("name")
	if err := validReleaseName(name); err != nil {
		writeErr(w, err)
		return
	}
	contextName, err := s.resolveContext(query.Get("context"))
	if err != nil {
		writeErr(w, err)
		return
	}
	cfg, err := s.helm.configFor(contextName, namespace)
	if err != nil {
		writeErr(w, err)
		return
	}
	rel, err := latest(cfg, name)
	if err != nil {
		writeHelmErr(w, err, nil)
		return
	}

	detail := helmReleaseDetail{
		helmReleaseJSON: s.toHelmReleaseJSON(rel),
		Fields:          map[string]string{},
		Resources:       resourcesOf(parseManifest(rel.Manifest, rel.Namespace)),
		History:         []helmHistoryJSON{},
	}

	// Secret fields are left out, and their values are scrubbed from the notes.
	var secrets redactor
	if entry := catalogEntry(s.helm.catalog, rel.Labels[helmCatalogLabel]); entry != nil && isManaged(rel) {
		secrets = entry.secretsOf(nil, rel.Config)
		for _, f := range entry.Fields {
			if !f.Secret {
				detail.Fields[f.Key], _ = f.get(rel.Config)
			}
		}
	}
	if isManaged(rel) && rel.Labels[helmSourceLabel] == sourceHub {
		secrets = hubSecrets(rel.Config)
		detail.Settings = maskedSettings(rel.Config)
	}
	if rel.Info != nil {
		detail.Notes = secrets.clean(rel.Info.Notes)
	}

	history := action.NewHistory(cfg)
	history.Max = 20
	if revisions, err := history.Run(name); err == nil {
		sort.Slice(revisions, func(i, j int) bool { return revisions[i].Version > revisions[j].Version })
		for _, rev := range revisions {
			item := helmHistoryJSON{Revision: rev.Version}
			if rev.Chart != nil && rev.Chart.Metadata != nil {
				item.Chart = rev.Chart.Metadata.Name + "-" + rev.Chart.Metadata.Version
			}
			if rev.Info != nil {
				item.Status = rev.Info.Status.String()
				item.Updated = s.age(metav1.NewTime(rev.Info.LastDeployed.Time))
				item.Description = secrets.clean(rev.Info.Description)
			}
			detail.History = append(detail.History, item)
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

// ---- shared write helpers ---------------------------------------------------------------------

// helmCancelGrace is how long a real write's context outlives the request. Helm runs a goroutine
// per install or upgrade that chooses between "finished" and "context cancelled"; when both are
// ready it picks at random, and the cancelled branch marks the release as failed (in the cluster)
// even though it succeeded. Cancelling the context the moment the handler returns made that
// happen now and then, so the cancel waits until that goroutine has long since seen "finished".
var helmCancelGrace = 5 * time.Second

// writeContext returns the context a Helm write runs in. A real change keeps
// going when the browser closes (a half-finished install is worse than a
// finished one) and is bounded by a timeout; a dry run follows the request.
func writeContext(r *http.Request, dryRun bool) (context.Context, context.CancelFunc) {
	if dryRun {
		return context.WithTimeout(r.Context(), helmDryRunTimeout)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), helmWriteTimeout)
	grace := helmCancelGrace
	return ctx, func() { time.AfterFunc(grace, cancel) }
}

// isMissingCRDs reports a server dry run that failed only because the chart's
// CRDs are not installed yet, which a dry run never does.
func isMissingCRDs(err error) bool {
	message := err.Error()
	return strings.Contains(message, "no matches for kind") ||
		strings.Contains(message, "ensure CRDs are installed first") ||
		strings.Contains(message, "resource mapping not found")
}

// existing returns the latest revision of a release, or nil when there is none.
func existing(cfg *action.Configuration, name string) (*release.Release, error) {
	history, err := cfg.Releases.History(name)
	if errors.Is(err, driver.ErrReleaseNotFound) || (err == nil && len(history) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	newest := history[0]
	for _, rel := range history {
		if rel.Version > newest.Version {
			newest = rel
		}
	}
	return newest, nil
}

// Modes of a dry run. "server" lets the API server validate every object. Both
// Helm's "client" and "server" options build the objects against the API, which
// fails while the chart's CRDs are not in the cluster yet, so the fallback is
// "offline": render only, like helm template.
const (
	dryServer  = "server"
	dryOffline = "offline"
)

// withServerDryRun runs op as a server side dry run. When the chart needs CRDs
// that are not installed yet, it falls back to an offline render and says so.
// op loads its own copy of the chart, because Helm edits the chart it is given.
func withServerDryRun(op func(mode string) (*release.Release, error)) (*release.Release, string, error) {
	rel, err := op(dryServer)
	if err != nil && isMissingCRDs(err) {
		rel, err = op(dryOffline)
		return rel, "warning: the API server could not validate every object because this app's CRDs are not installed yet; what is listed comes from the rendered chart", err
	}
	return rel, "", err
}

// clusterKubeVersion reads the cluster's version for an offline render, so charts
// that require a minimum kubeVersion still render. It returns nil when the cluster
// cannot be asked, and Helm's default version applies.
func clusterKubeVersion(cfg *action.Configuration) *chartutil.KubeVersion {
	if cfg.RESTClientGetter == nil {
		return nil
	}
	discovery, err := cfg.RESTClientGetter.ToDiscoveryClient()
	if err != nil {
		return nil
	}
	info, err := discovery.ServerVersion()
	if err != nil {
		return nil
	}
	return &chartutil.KubeVersion{Version: info.GitVersion, Major: info.Major, Minor: info.Minor}
}

func labelsFor(e *helmEntry) map[string]string {
	return map[string]string{helmManagedLabel: "true", helmCatalogLabel: e.ID}
}

func describe(e *helmEntry) string { return e.Name + " " + e.Version }

// ---- install ------------------------------------------------------------------------------------

type helmInstallRequest struct {
	mutation
	// Source is "catalog" (the default) or "hub". The fields of the other source are ignored.
	Source          string         `json:"source"`
	CatalogID       string         `json:"catalogId"`
	PackageID       string         `json:"packageId"`
	Version         string         `json:"version"`
	Name            string         `json:"name"`
	Namespace       string         `json:"namespace"`
	CreateNamespace bool           `json:"createNamespace"`
	Fields          map[string]any `json:"fields"`
	Settings        map[string]any `json:"settings"`
	// Digest and ConfirmPhrase come back from the preview of an install from search.
	Digest        string `json:"digest"`
	ConfirmPhrase string `json:"confirmPhrase"`
}

func (s *Server) handleHelmInstall(w http.ResponseWriter, r *http.Request) {
	var req helmInstallRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	switch req.Source {
	case "", sourceCatalog:
		s.catalogInstall(w, r, &req)
	case sourceHub:
		s.hubInstall(w, r, &req)
	default:
		writeErr(w, badRequest("source must be %q or %q", sourceCatalog, sourceHub))
	}
}

// installRun is what installing a chart needs, whichever way the chart came.
type installRun struct {
	contextName, name, namespace string
	createNamespace              bool
	labels                       map[string]string
	load                         func() (*chart.Chart, error) // a fresh copy of the chart for every try
	values                       map[string]any
	dryRun                       bool
}

// execInstall runs the install, or its preview. A preview is a server side dry
// run; when the chart needs CRDs that are not in the cluster yet it falls back
// to an offline render and says so in note.
func (s *Server) execInstall(ctx context.Context, cfg *action.Configuration, run installRun) (rel *release.Release, loaded *chart.Chart, note string, err error) {
	attempt := func(mode string) (*release.Release, error) {
		fresh, err := run.load()
		if err != nil {
			return nil, err
		}
		loaded = fresh
		runCfg := cfg
		if mode == dryOffline {
			// ClientOnly swaps parts of the configuration for fakes, so it gets its own.
			if runCfg, err = s.helm.configFor(run.contextName, run.namespace); err != nil {
				return nil, err
			}
		}
		install := action.NewInstall(runCfg)
		install.ReleaseName = run.name
		install.Namespace = run.namespace
		install.CreateNamespace = run.createNamespace
		install.Labels = run.labels
		install.Timeout = helmReleaseTimeout
		switch mode {
		case dryServer:
			install.DryRun, install.DryRunOption = true, "server"
		case dryOffline:
			install.DryRun, install.DryRunOption = true, "client"
			install.ClientOnly = true
			install.KubeVersion = clusterKubeVersion(runCfg)
		}
		return install.RunWithContext(ctx, fresh, deepCopyMap(run.values))
	}

	if run.dryRun {
		rel, note, err = withServerDryRun(attempt)
	} else {
		rel, err = attempt("")
	}
	return rel, loaded, note, err
}

// takeName refuses a release name that is in use. Helm skips this check on a
// dry run, but the preview should already say that the name is taken.
func takeName(cfg *action.Configuration, name string) error {
	rel, err := existing(cfg, name)
	if err != nil {
		return err
	}
	if rel != nil {
		return fmt.Errorf("cannot re-use a name that is still in use")
	}
	return nil
}

func (s *Server) catalogInstall(w http.ResponseWriter, r *http.Request, req *helmInstallRequest) {
	entry := catalogEntry(s.helm.catalog, req.CatalogID)
	if entry == nil {
		writeErr(w, badRequest("%q is not an app of the catalog", req.CatalogID))
		return
	}
	if req.Name == "" {
		req.Name = entry.DefaultName
	}
	if req.Namespace == "" {
		req.Namespace = entry.DefaultNamespace
	}
	if err := validReleaseName(req.Name); err != nil {
		writeErr(w, err)
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
	given, err := entry.checkFields(req.Fields)
	if err != nil {
		writeErr(w, err)
		return
	}
	fields := entry.fieldDefaults()
	for key, value := range given {
		fields[key] = value
	}
	secrets := entry.secretsOf(fields, nil)

	contextName, err := s.authorize(req.mutation)
	if err != nil {
		writeErr(w, err)
		return
	}

	ctx, cancel := writeContext(r, req.DryRun)
	defer cancel()

	cfg, err := s.helm.configFor(contextName, namespace)
	if err != nil {
		writeHelmErr(w, err, secrets)
		return
	}
	if err := takeName(cfg, req.Name); err != nil {
		writeHelmErr(w, err, secrets)
		return
	}
	if !req.DryRun {
		done, err := s.helm.begin(contextName, namespace, req.Name)
		if err != nil {
			writeHelmErr(w, err, secrets)
			return
		}
		defer done()
	}

	values := entry.buildValues(nil, fields, false, false)
	rel, loaded, note, err := s.execInstall(ctx, cfg, installRun{
		contextName: contextName, name: req.Name, namespace: namespace, createNamespace: req.CreateNamespace,
		labels: labelsFor(entry), values: values, dryRun: req.DryRun,
		load: func() (*chart.Chart, error) { return s.helm.loadChart(ctx, entry) },
	})
	if err != nil {
		writeHelmErr(w, err, secrets)
		return
	}

	docs := withExtras(parseManifest(rel.Manifest, namespace), rel, loaded, namespace)
	crds := crdsOf(loaded)
	changes := warningsFor(docs, crds)
	if note != "" {
		changes = append(changes, note)
	}
	changes = append(changes, resourceLines("", append(resourcesOf(docs), crds...))...)

	summary := fmt.Sprintf("%s installed as release %q in namespace %q; pods may take a few minutes to start", describe(entry), req.Name, namespace)
	if req.DryRun {
		summary = fmt.Sprintf("%s would be installed as release %q in namespace %q", describe(entry), req.Name, namespace)
	}
	display := entry.buildValues(nil, fields, false, true)
	writeJSON(w, http.StatusOK, changeResponse{
		Context: contextName, Kind: "HelmRelease", Namespace: namespace, Name: req.Name,
		Action: "created", DryRun: req.DryRun, Summary: summary, Changes: changes,
		Equivalent: installCommand("install", entry, req.Name, namespace, req.CreateNamespace, helmSetFlags(display)),
	})
}

// ---- upgrade ------------------------------------------------------------------------------------

type helmUpgradeRequest struct {
	mutation
	Source    string         `json:"source"`
	Namespace string         `json:"namespace"`
	Name      string         `json:"name"`
	Version   string         `json:"version"`
	Fields    map[string]any `json:"fields"`
	Settings  map[string]any `json:"settings"`
	// Digest and ConfirmPhrase come back from the preview of an upgrade of an app installed from search.
	Digest        string `json:"digest"`
	ConfirmPhrase string `json:"confirmPhrase"`
}

// managedRelease loads a release and refuses it unless the console installed it.
// For a release installed from search there is no catalog entry, so entry is nil.
func (s *Server) managedRelease(cfg *action.Configuration, name string) (*release.Release, *helmEntry, error) {
	rel, err := latest(cfg, name)
	if err != nil {
		return nil, nil, err
	}
	if !isManaged(rel) {
		return nil, nil, &apiError{status: http.StatusForbidden, code: "not_managed",
			msg:  fmt.Sprintf("release %q was not installed by the console, so the console does not change it", name),
			hint: "Use the helm command line for releases installed elsewhere."}
	}
	if rel.Labels[helmSourceLabel] == sourceHub {
		return rel, nil, nil
	}
	entry := catalogEntry(s.helm.catalog, rel.Labels[helmCatalogLabel])
	if entry == nil {
		return nil, nil, &apiError{status: http.StatusConflict, code: "unknown_app",
			msg: fmt.Sprintf("release %q comes from an app that is no longer in the catalog", name)}
	}
	return rel, entry, nil
}

// upgradeRun is what upgrading a release needs, whichever way its chart came.
type upgradeRun struct {
	name, namespace string
	labels          map[string]string
	load            func() (*chart.Chart, error)
	values          map[string]any
	dryRun          bool
}

// execUpgrade runs the upgrade or its preview. The CRDs of an installed app are
// in the cluster, so the preview can be validated by the API server.
func (s *Server) execUpgrade(ctx context.Context, cfg *action.Configuration, run upgradeRun) (*release.Release, error) {
	loaded, err := run.load()
	if err != nil {
		return nil, err
	}
	upgrade := action.NewUpgrade(cfg)
	upgrade.Namespace = run.namespace
	upgrade.Labels = run.labels
	upgrade.Timeout = helmReleaseTimeout
	if run.dryRun {
		upgrade.DryRun, upgrade.DryRunOption = true, "server"
	}
	return upgrade.RunWithContext(ctx, run.name, loaded, deepCopyMap(run.values))
}

func (s *Server) handleHelmUpgrade(w http.ResponseWriter, r *http.Request) {
	var req helmUpgradeRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	namespace, err := validNamespace(req.Namespace)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := validReleaseName(req.Name); err != nil {
		writeErr(w, err)
		return
	}
	contextName, err := s.authorize(req.mutation)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := writeContext(r, req.DryRun)
	defer cancel()

	cfg, err := s.helm.configFor(contextName, namespace)
	if err != nil {
		writeHelmErr(w, err, nil)
		return
	}
	rel, entry, err := s.managedRelease(cfg, req.Name)
	if err != nil {
		writeHelmErr(w, err, nil)
		return
	}
	if entry == nil {
		s.hubUpgrade(ctx, w, &req, cfg, rel, contextName)
		return
	}

	// Everything below can see secrets, so everything that is sent goes through cleaned.
	given, err := entry.checkFields(req.Fields)
	if err != nil {
		writeErr(w, err)
		return
	}
	secrets := entry.secretsOf(given, rel.Config)

	// Fields the request leaves out keep the value the release has; an empty secret keeps the stored one.
	current := map[string]string{}
	apply := map[string]string{}
	var changes []string
	for _, f := range entry.Fields {
		old, _ := f.get(rel.Config)
		current[f.Key] = old
		value, provided := given[f.Key]
		if !provided || (f.Secret && value == "") {
			continue
		}
		apply[f.Key] = value
		switch {
		case f.Secret:
			changes = append(changes, f.Label+": changed")
		case value != old:
			changes = append(changes, fmt.Sprintf("%s: %q -> %q", f.Label, short(old), short(value)))
		}
	}
	// A secret that was sent is a change, whatever it was before: it cannot be compared without showing it.
	changed := map[string]string{}
	for key, value := range apply {
		if f := entry.field(key); f.Secret || value != current[key] {
			changed[key] = value
		}
	}

	installedVersion := ""
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		installedVersion = rel.Chart.Metadata.Version
	}
	if installedVersion != entry.Version {
		changes = append(changes, fmt.Sprintf("chart: %s -> %s", installedVersion, entry.Version))
	}
	equivalent := installCommand("upgrade", entry, req.Name, namespace, false, helmSetFlags(entry.buildValues(nil, changed, true, true)))

	if len(changed) == 0 && installedVersion == entry.Version {
		writeJSON(w, http.StatusOK, changeResponse{
			Context: contextName, Kind: "HelmRelease", Namespace: namespace, Name: req.Name, Action: "unchanged", DryRun: req.DryRun,
			Summary: fmt.Sprintf("%s already has these settings", req.Name), Equivalent: equivalent,
		})
		return
	}

	if !req.DryRun {
		done, err := s.helm.begin(contextName, namespace, req.Name)
		if err != nil {
			writeHelmErr(w, err, secrets)
			return
		}
		defer done()
	}

	// The values are the ones the release was installed with plus the fields that were sent,
	// so there is nothing to reuse and nothing outside the form can change.
	updated, err := s.execUpgrade(ctx, cfg, upgradeRun{
		name: req.Name, namespace: namespace, labels: labelsFor(entry), dryRun: req.DryRun,
		values: entry.buildValues(rel.Config, apply, true, false),
		load:   func() (*chart.Chart, error) { return s.helm.loadChart(ctx, entry) },
	})
	if err != nil {
		writeHelmErr(w, err, secrets)
		return
	}

	added, removed := diffResources(parseManifest(rel.Manifest, namespace), parseManifest(updated.Manifest, namespace))
	changes = append(changes, resourceLines("+ ", added)...)
	changes = append(changes, resourceLines("- ", removed)...)

	summary := fmt.Sprintf("Release %q updated; pods roll out one by one", req.Name)
	if req.DryRun {
		summary = fmt.Sprintf("Release %q would be updated", req.Name)
	}
	writeJSON(w, http.StatusOK, changeResponse{
		Context: contextName, Kind: "HelmRelease", Namespace: namespace, Name: req.Name,
		Action: "updated", DryRun: req.DryRun, Summary: summary, Changes: changes, Equivalent: equivalent,
	})
}

// diffResources lists what an upgrade adds and what it removes.
func diffResources(before, after []manifestDoc) (added, removed []helmResource) {
	index := func(docs []manifestDoc) map[string]helmResource {
		out := make(map[string]helmResource, len(docs))
		for _, r := range resourcesOf(docs) {
			out[r.String()] = r
		}
		return out
	}
	was, now := index(before), index(after)
	for key, r := range now {
		if _, ok := was[key]; !ok {
			added = append(added, r)
		}
	}
	for key, r := range was {
		if _, ok := now[key]; !ok {
			removed = append(removed, r)
		}
	}
	byName := func(list []helmResource) {
		sort.Slice(list, func(i, j int) bool { return list[i].String() < list[j].String() })
	}
	byName(added)
	byName(removed)
	return added, removed
}

// ---- uninstall -----------------------------------------------------------------------------------

type helmUninstallRequest struct {
	mutation
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// The browser sends these back on every apply; an uninstall has no use for them.
	Source        string         `json:"source"`
	Version       string         `json:"version"`
	Settings      map[string]any `json:"settings"`
	Digest        string         `json:"digest"`
	ConfirmPhrase string         `json:"confirmPhrase"`
}

func (s *Server) handleHelmUninstall(w http.ResponseWriter, r *http.Request) {
	var req helmUninstallRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	namespace, err := validNamespace(req.Namespace)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := validReleaseName(req.Name); err != nil {
		writeErr(w, err)
		return
	}
	contextName, err := s.authorize(req.mutation)
	if err != nil {
		writeErr(w, err)
		return
	}

	cfg, err := s.helm.configFor(contextName, namespace)
	if err != nil {
		writeHelmErr(w, err, nil)
		return
	}
	rel, entry, err := s.managedRelease(cfg, req.Name)
	if err != nil {
		writeHelmErr(w, err, nil)
		return
	}
	secrets := hubSecrets(rel.Config)
	if entry != nil {
		secrets = entry.secretsOf(nil, rel.Config)
	}

	docs := parseManifest(rel.Manifest, namespace)
	var changes []string
	for _, hook := range hookDocs(rel, namespace) {
		if strings.Contains(hook.hook, "delete") {
			changes = append(changes, "warning: runs Helm hooks while the release is removed: "+safeLine(hook.Kind+"/"+hook.Metadata.Name)+" ("+safeLine(hook.hook)+")")
		}
	}
	if crds := crdsOf(rel.Chart); len(crds) > 0 {
		names := make([]string, 0, len(crds))
		for _, c := range crds {
			names = append(names, c.Name)
		}
		changes = append(changes, fmt.Sprintf("warning: Helm does not delete the %d CustomResourceDefinitions this app installed (%s); remove them yourself if you do not need them",
			len(crds), strings.Join(names, ", ")))
	}
	for _, d := range docs {
		if d.Kind == "StatefulSet" || d.Kind == "PersistentVolumeClaim" {
			changes = append(changes, "warning: volumes created by StatefulSets are kept when the release is removed")
			break
		}
	}
	changes = append(changes, resourceLines("- ", resourcesOf(docs))...)

	if !req.DryRun {
		done, err := s.helm.begin(contextName, namespace, req.Name)
		if err != nil {
			writeHelmErr(w, err, secrets)
			return
		}
		defer done()

		uninstall := action.NewUninstall(cfg)
		uninstall.Timeout = helmReleaseTimeout
		uninstall.KeepHistory = false
		if _, err := uninstall.Run(req.Name); err != nil {
			writeHelmErr(w, err, secrets)
			return
		}
	}

	summary := fmt.Sprintf("Release %q removed from namespace %q", req.Name, namespace)
	if req.DryRun {
		summary = fmt.Sprintf("Release %q would be removed from namespace %q", req.Name, namespace)
	}
	writeJSON(w, http.StatusOK, changeResponse{
		Context: contextName, Kind: "HelmRelease", Namespace: namespace, Name: req.Name,
		Action: "deleted", DryRun: req.DryRun, Summary: summary, Changes: changes,
		Equivalent: fmt.Sprintf("helm uninstall %s -n %s", kubectlQuote(req.Name), kubectlQuote(namespace)),
	})
}
