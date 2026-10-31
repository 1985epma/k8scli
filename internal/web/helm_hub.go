package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"helm.sh/helm/v3/pkg/chart"
)

// Artifact Hub search and the install-from-search flow (ADR 0004, amendment).
// The browser never talks to Artifact Hub: this server does, on a fixed host,
// and what comes back is checked again at install time, never taken from the
// browser.

const (
	hubDefaultBase = "https://artifacthub.io"
	hubTimeout     = 10 * time.Second
	hubMaxBody     = 1 << 20 // bytes
	hubPageSize    = 20
	hubMaxPage     = 50
	hubMinQuery    = 2
	hubMaxQuery    = 80

	hubSearchTTL = 5 * time.Minute
	hubSearchMax = 100
	hubRefTTL    = time.Hour
	hubRefMax    = 2000

	maxHubSettings    = 20
	maxSettingValue   = 256
	maxSettingPath    = 128
	maxSettingDepth   = 8
	maxDescriptionLen = 200

	helmSourceLabel  = "k8scli.io/source"
	helmPackageLabel = "k8scli.io/package"
	helmHubRepoLabel = "k8scli.io/hub-repo"
	helmHubNameLabel = "k8scli.io/hub-chart"
	sourceHub        = "hub"
	sourceCatalog    = "catalog"
)

var (
	packageIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hubNamePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	versionPattern   = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)
	settingPath      = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)
	// secretishPath names the values that are hidden from responses and equivalent commands.
	secretishPath = regexp.MustCompile(`(?i)(pass(word|wd|phrase)?|pwd|secret|token|api[-_]?key|private[-_]?key|credential|auth|cert|salt|jwt|bearer|signing|encrypt|license|dsn|connection[-_]?string|(^|[._-])keys?($|[._-])|key$)`)
	// randomValuePath names settings whose value may be random text (so it may look like YAML by chance);
	// narrower than secretishPath, which only decides what is hidden.
	randomValuePath = regexp.MustCompile(`(?i)(pass(word|wd|phrase)?|pwd|secret|token|api[-_]?key|private[-_]?key|credential)`)
	// urlCredentials matches user:password@ inside a URL value.
	urlCredentials = regexp.MustCompile(`://[^/@\s:]+:[^@\s]+@`)
)

// hubDenyCharts are charts whose whole purpose is to build arbitrary objects out
// of values, which would turn "a few settings" into a free-form manifest.
var hubDenyCharts = map[string]bool{
	"raw": true, "app-template": true, "common": true, "base": true, "generic": true, "generic-chart": true,
	"universal-chart": true, "kubernetes-manifests": true, "kubernetes-manifest": true, "manifests": true,
	"raw-manifests": true, "any-resource": true, "multi-resource": true, "resources": true, "helm-raw": true,
}

// hubDenySegments are value keys that take whole objects; a setting may not go through them.
var hubDenySegments = map[string]bool{
	"extraobjects": true, "extramanifests": true, "extradeploy": true, "extraresources": true,
	"rawresources": true, "additionalobjects": true, "additionalmanifests": true, "manifests": true, "objects": true,
}

// ---- Artifact Hub documents ----------------------------------------------------------------

type hubRepository struct {
	Name                    string `json:"name"`
	URL                     string `json:"url"`
	Kind                    int    `json:"kind"`
	Official                bool   `json:"official"`
	VerifiedPublisher       bool   `json:"verified_publisher"`
	OrganizationName        string `json:"organization_name"`
	OrganizationDisplayName string `json:"organization_display_name"`
	UserAlias               string `json:"user_alias"`
}

type hubSecurity struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
}

type hubPackage struct {
	PackageID      string        `json:"package_id"`
	Name           string        `json:"name"`
	NormalizedName string        `json:"normalized_name"`
	DisplayName    string        `json:"display_name"`
	Description    string        `json:"description"`
	Version        string        `json:"version"`
	AppVersion     string        `json:"app_version"`
	License        string        `json:"license"`
	Stars          int           `json:"stars"`
	Official       bool          `json:"official"`
	Deprecated     bool          `json:"deprecated"`
	Signed         bool          `json:"signed"`
	Security       *hubSecurity  `json:"security_report_summary"`
	Repository     hubRepository `json:"repository"`
}

type hubDetail struct {
	hubPackage
	ContentURL string `json:"content_url"`
	Digest     string `json:"digest"`
	Prerelease bool   `json:"prerelease"`
	Data       struct {
		Type string `json:"type"`
	} `json:"data"`
	AvailableVersions []struct {
		Version    string `json:"version"`
		Prerelease bool   `json:"prerelease"`
	} `json:"available_versions"`
}

// ---- what the UI gets ------------------------------------------------------------------------------

type hubRepositoryJSON struct {
	Name              string `json:"name"`
	URL               string `json:"url"`
	VerifiedPublisher bool   `json:"verifiedPublisher"`
	Official          bool   `json:"official"`
	Organization      string `json:"organization"`
}

type hubItemJSON struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	DisplayName string            `json:"displayName,omitempty"`
	Version     string            `json:"version"`
	AppVersion  string            `json:"appVersion"`
	Description string            `json:"description"`
	Repository  hubRepositoryJSON `json:"repository"`
	Stars       int               `json:"stars"`
	Signed      bool              `json:"signed"`
	Deprecated  bool              `json:"deprecated"`
	License     string            `json:"license"`
	Security    *hubSecurity      `json:"security,omitempty"`
	InCatalog   string            `json:"inCatalog"`
	Installable bool              `json:"installable"`
	Reason      string            `json:"reason"`
}

type hubSearchJSON struct {
	Query    string        `json:"query"`
	Page     int           `json:"page"`
	PageSize int           `json:"pageSize"`
	Total    int           `json:"total"`
	HasMore  bool          `json:"hasMore"`
	Items    []hubItemJSON `json:"items"`
}

// ---- the client ---------------------------------------------------------------------------------------

type hubRef struct {
	repo, name string
	at         time.Time
}

type hubCached struct {
	at     time.Time
	result hubSearchJSON
}

// hubClient talks to Artifact Hub. base and http are replaced in tests.
type hubClient struct {
	base string
	http *http.Client
	now  func() time.Time

	mu       sync.Mutex
	searches map[string]hubCached
	order    []string // oldest first, for eviction
	refs     map[string]hubRef
}

func newHubClient() *hubClient {
	c := &hubClient{
		base:     hubDefaultBase,
		now:      time.Now,
		searches: map[string]hubCached{},
		refs:     map[string]hubRef{},
	}
	c.http = &http.Client{Timeout: hubTimeout, Transport: guardedTransport(), CheckRedirect: c.checkRedirect}
	return c
}

// checkRedirect keeps Artifact Hub on its own host: a redirect elsewhere would
// move the trust to a place nobody reviewed.
func (c *hubClient) checkRedirect(req *http.Request, via []*http.Request) error {
	base, err := url.Parse(c.base)
	if err != nil || len(via) >= 3 || req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, base.Host) {
		return errors.New("the Artifact Hub request was redirected away from its own host")
	}
	return nil
}

func hubUnavailable(format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadGateway, code: "hub_unavailable", msg: fmt.Sprintf(format, args...),
		hint: "Artifact Hub did not answer. Try again in a moment; the catalog still works."}
}

// hubRateLimited is Artifact Hub saying "slow down". It is a 429 for the browser too, so the UI can
// show a wait-and-retry state instead of a failure.
func hubRateLimited(retryAfter string) *apiError {
	msg := "Artifact Hub is limiting requests right now"
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds > 0 && seconds <= 3600 {
		msg += fmt.Sprintf(" (it asks to retry in %d seconds)", seconds)
	}
	return &apiError{status: http.StatusTooManyRequests, code: "hub_rate_limited", msg: msg,
		hint: "Wait a moment and try again. The catalog still works."}
}

// get fetches one JSON document, with a body cap. total is the Pagination-Total-Count header, or -1.
func (c *hubClient) get(ctx context.Context, path string, query url.Values, into any) (int, error) {
	address := c.base + path
	if len(query) > 0 {
		address += "?" + query.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, hubTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, http.NoBody)
	if err != nil {
		return -1, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "k8scli-web")

	resp, err := c.http.Do(req)
	if err != nil {
		return -1, hubUnavailable("could not reach Artifact Hub: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return -1, hubRateLimited(resp.Header.Get("Retry-After"))
	case resp.StatusCode == http.StatusNotFound:
		return -1, &apiError{status: http.StatusNotFound, code: "unknown_package", msg: "Artifact Hub does not know this package",
			hint: "Search for it again."}
	case resp.StatusCode != http.StatusOK:
		return -1, hubUnavailable("Artifact Hub answered %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, hubMaxBody+1))
	if err != nil {
		return -1, hubUnavailable("could not read the answer of Artifact Hub: %v", err)
	}
	if len(body) > hubMaxBody {
		return -1, hubUnavailable("the answer of Artifact Hub is larger than %d KiB", hubMaxBody>>10)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return -1, hubUnavailable("the answer of Artifact Hub could not be read")
	}
	total := -1
	if header := resp.Header.Get("Pagination-Total-Count"); header != "" {
		if n, err := strconv.Atoi(header); err == nil && n >= 0 {
			total = n
		}
	}
	return total, nil
}

// validQuery checks and normalizes a search text.
func validQuery(raw string) (string, error) {
	q := strings.Join(strings.Fields(raw), " ")
	switch n := utf8.RuneCountInString(q); {
	case n < hubMinQuery:
		return "", badRequest("type at least %d characters to search", hubMinQuery)
	case n > hubMaxQuery:
		return "", badRequest("the search is too long (at most %d characters)", hubMaxQuery)
	}
	for _, r := range q {
		if !unicode.IsPrint(r) {
			return "", badRequest("the search may not contain control characters")
		}
	}
	return q, nil
}

// search returns one page of Helm charts matching q.
func (c *hubClient) search(ctx context.Context, catalog []helmEntry, q string, page int) (*hubSearchJSON, error) {
	key := strings.ToLower(q) + "|" + strconv.Itoa(page)
	c.mu.Lock()
	if hit, ok := c.searches[key]; ok && c.now().Sub(hit.at) < hubSearchTTL {
		c.mu.Unlock()
		result := hit.result
		return &result, nil
	}
	c.mu.Unlock()

	query := url.Values{}
	query.Set("ts_query_web", q)
	query.Set("kind", "0") // Helm charts
	query.Set("sort", "relevance")
	query.Set("limit", strconv.Itoa(hubPageSize))
	query.Set("offset", strconv.Itoa((page-1)*hubPageSize))
	var answer struct {
		Packages []hubPackage `json:"packages"`
	}
	total, err := c.get(ctx, "/api/v1/packages/search", query, &answer)
	if err != nil {
		return nil, err
	}

	result := hubSearchJSON{Query: q, Page: page, PageSize: hubPageSize, Items: make([]hubItemJSON, 0, len(answer.Packages))}
	for i := range answer.Packages {
		p := &answer.Packages[i]
		if len(result.Items) == hubPageSize {
			break
		}
		result.Items = append(result.Items, toHubItem(p, catalog))
	}
	result.Total = total
	if total < 0 {
		result.Total = (page-1)*hubPageSize + len(result.Items)
	}
	result.HasMore = page*hubPageSize < result.Total && page < hubMaxPage

	c.remember(answer.Packages)
	c.mu.Lock()
	if _, exists := c.searches[key]; !exists {
		c.order = append(c.order, key)
	}
	c.searches[key] = hubCached{at: c.now(), result: result}
	for len(c.order) > hubSearchMax {
		delete(c.searches, c.order[0])
		c.order = c.order[1:]
	}
	c.mu.Unlock()
	return &result, nil
}

// remember keeps what Artifact Hub said about a package id, so an install can
// find the package again from the id alone. Only Artifact Hub's own answers get here.
func (c *hubClient) remember(packages []hubPackage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, p := range packages {
		if !packageIDPattern.MatchString(p.PackageID) || !hubNamePattern.MatchString(p.Repository.Name) || !hubNamePattern.MatchString(p.NormalizedName) {
			continue
		}
		c.refs[p.PackageID] = hubRef{repo: p.Repository.Name, name: p.NormalizedName, at: now}
	}
	if len(c.refs) <= hubRefMax {
		return
	}
	// Over the limit: drop the oldest ones, and a tenth more, so this does not run on every call.
	type aged struct {
		id string
		at time.Time
	}
	all := make([]aged, 0, len(c.refs))
	for id, ref := range c.refs {
		all = append(all, aged{id, ref.at})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for _, old := range all[:len(c.refs)-hubRefMax+hubRefMax/10] {
		delete(c.refs, old.id)
	}
}

// lookup finds the repository and chart of a package id that a search returned.
func (c *hubClient) lookup(id string) (hubRef, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ref, ok := c.refs[id]
	if ok && c.now().Sub(ref.at) > hubRefTTL {
		delete(c.refs, id)
		return hubRef{}, false
	}
	return ref, ok
}

// detail fetches a package (a given version, or the latest) from Artifact Hub.
func (c *hubClient) detail(ctx context.Context, ref hubRef, version string) (*hubDetail, error) {
	path := "/api/v1/packages/helm/" + url.PathEscape(ref.repo) + "/" + url.PathEscape(ref.name)
	if version != "" {
		path += "/" + url.PathEscape(version)
	}
	var detail hubDetail
	if _, err := c.get(ctx, path, nil, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// ---- eligibility ----------------------------------------------------------------------------------------

// hubEligibility applies rule 2 of the amendment to what a search or a detail says.
func hubEligibility(p *hubPackage) (bool, string) {
	switch {
	case p.Repository.Kind != 0:
		return false, "it is not a Helm chart"
	case p.Deprecated:
		return false, "the chart is deprecated"
	case hubDenyCharts[strings.ToLower(p.NormalizedName)] || hubDenyCharts[strings.ToLower(p.Name)]:
		return false, "this chart builds arbitrary objects from values, so it cannot be installed here"
	case !p.Repository.VerifiedPublisher && !p.Repository.Official && !p.Official:
		return false, "the publisher is not verified and the repository is not official"
	}
	return true, ""
}

// detailEligibility adds the checks only the full record allows.
func detailEligibility(d *hubDetail) (bool, string) {
	if ok, reason := hubEligibility(&d.hubPackage); !ok {
		return false, reason
	}
	switch {
	case d.Prerelease:
		return false, "this version is a pre-release"
	case strings.EqualFold(d.Data.Type, "library"):
		return false, "library charts cannot be installed"
	case !strings.HasPrefix(d.ContentURL, "https://"):
		return false, "the archive is not served over HTTPS"
	}
	return true, ""
}

func normalizeRepo(address string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(address), "/"))
}

func toHubItem(p *hubPackage, catalog []helmEntry) hubItemJSON {
	item := hubItemJSON{
		ID: p.PackageID, Name: p.Name, DisplayName: p.DisplayName, Version: p.Version, AppVersion: p.AppVersion,
		Description: truncate(p.Description, maxDescriptionLen), Stars: p.Stars, Signed: p.Signed, Deprecated: p.Deprecated,
		License: p.License, Security: p.Security,
		Repository: hubRepositoryJSON{
			Name: p.Repository.Name, URL: p.Repository.URL, VerifiedPublisher: p.Repository.VerifiedPublisher,
			Official:     p.Repository.Official || p.Official,
			Organization: firstNonEmpty(p.Repository.OrganizationDisplayName, p.Repository.OrganizationName, p.Repository.UserAlias),
		},
	}
	for i := range catalog {
		if strings.EqualFold(catalog[i].Chart, p.NormalizedName) && normalizeRepo(catalog[i].Repo) == normalizeRepo(p.Repository.URL) {
			item.InCatalog = catalog[i].ID
			break
		}
	}
	item.Installable, item.Reason = hubEligibility(p)
	return item
}

func truncate(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// normalizeSegment lowercases a value key and drops - and _, so extra-objects, extra_objects and
// extraObjects are one name.
func normalizeSegment(segment string) string {
	return strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(segment))
}

// ---- settings ------------------------------------------------------------------------------------------

// settingValue normalizes a scalar setting.
func settingValue(raw any) (any, error) {
	switch v := raw.(type) {
	case string:
		if utf8.RuneCountInString(v) > maxSettingValue {
			return nil, badRequest("a value is too long (at most %d characters)", maxSettingValue)
		}
		for _, r := range v {
			if !unicode.IsPrint(r) {
				return nil, badRequest("a value may not contain control or invisible characters")
			}
		}
		return v, nil
	case bool:
		return v, nil
	case float64:
		if v == float64(int64(v)) {
			return int64(v), nil
		}
		return v, nil
	default:
		return nil, badRequest("a setting must be text, a number or true/false")
	}
}

// defaultKind says what the chart's own default at a path is.
func defaultKind(node any) string {
	switch node.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case string:
		return "string"
	case float64, int, int64, float32:
		return "number"
	default:
		return "object"
	}
}

// checkSettings validates settings against the chart's default values (rule 4)
// and returns them as nested values, in a stable order.
func checkSettings(c *chart.Chart, given map[string]any) (map[string]any, []string, error) {
	if len(given) > maxHubSettings {
		return nil, nil, badRequest("too many settings (at most %d)", maxHubSettings)
	}
	paths := make([]string, 0, len(given))
	for p := range given {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	values := map[string]any{}
	for _, path := range paths {
		if len(path) > maxSettingPath || !settingPath.MatchString(path) {
			return nil, nil, badRequest("%q is not a setting path: use names joined by dots, for example service.type", path)
		}
		segments := strings.Split(path, ".")
		if len(segments) > maxSettingDepth {
			return nil, nil, badRequest("%q is nested too deeply", path)
		}
		for _, segment := range segments {
			if hubDenySegments[normalizeSegment(segment)] {
				return nil, nil, badRequest("%q goes through %s, which takes whole objects and is not allowed", path, segment)
			}
		}
		node, ok := getPath(c.Values, path)
		if !ok {
			return nil, nil, badRequest("%q is not a value this chart has", path)
		}
		kind := defaultKind(node)
		if kind == "object" {
			return nil, nil, badRequest("%q is a list or a group of values, not a simple one", path)
		}
		value, err := settingValue(given[path])
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		if text, isText := value.(string); isText {
			// A secret may look like anything, so only the template check applies to it.
			if err := plainValue(text, randomValuePath.MatchString(path)); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", path, err)
			}
		}
		switch got := defaultKind(value); {
		case kind == "null":
		case kind != got:
			return nil, nil, badRequest("%q expects a %s, not a %s", path, kind, got)
		}
		setPath(values, path, value)
	}
	return values, paths, nil
}

// mergeValues overlays over on top of base, in place on a copy.
func mergeValues(base, over map[string]any) map[string]any {
	out := deepCopyMap(base)
	for key, value := range over {
		if next, ok := value.(map[string]any); ok {
			if current, ok := out[key].(map[string]any); ok {
				out[key] = mergeValues(current, next)
				continue
			}
			out[key] = deepCopyMap(next)
			continue
		}
		out[key] = value
	}
	return out
}

// flatten lists scalar leaves of nested values as dotted paths.
func flatten(prefix string, node any, out map[string]any) {
	if m, ok := node.(map[string]any); ok {
		for key, value := range m {
			next := key
			if prefix != "" {
				next = prefix + "." + key
			}
			flatten(next, value, out)
		}
		return
	}
	if prefix != "" {
		out[prefix] = node
	}
}

// looksSecret says whether a setting is hidden: by the name of its path, or because its value is a
// URL with a user and password in it.
func looksSecret(path string, value any) bool {
	if secretishPath.MatchString(path) {
		return true
	}
	text, ok := value.(string)
	return ok && urlCredentials.MatchString(text)
}

// hubSecrets finds the values of an installed release that look like secrets.
func hubSecrets(config map[string]any) redactor {
	flat := map[string]any{}
	flatten("", config, flat)
	var secrets redactor
	for path, value := range flat {
		if text, ok := value.(string); ok && len(text) >= 4 && looksSecret(path, text) {
			secrets = append(secrets, text)
		}
	}
	return secrets
}

// maskedSettings is what the API may say about the settings of a release: the
// values at secret-looking paths are hidden.
func maskedSettings(config map[string]any) map[string]string {
	flat := map[string]any{}
	flatten("", config, flat)
	out := make(map[string]string, len(flat))
	for path, value := range flat {
		text := fmt.Sprint(value)
		if looksSecret(path, value) {
			text = placeholderSecret
		}
		out[path] = text
	}
	return out
}

// displayValues is the settings as --set arguments with secrets hidden.
func displayValues(values map[string]any) map[string]any {
	flat := map[string]any{}
	flatten("", values, flat)
	out := map[string]any{}
	for path, value := range flat {
		if looksSecret(path, value) {
			value = placeholderSecret
		}
		setPath(out, path, value)
	}
	return out
}
