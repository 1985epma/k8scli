package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

// Defenses for charts that come from outside the catalog (ADR 0004, amendment), found by an
// adversarial review of the Helm backend. They are small, separate and each one has a test that
// fails when the defense is removed.

const (
	// maxDecompressedChart is the most a chart archive may expand to. Helm's own default is 100 MiB
	// for every archive, and subcharts are archives inside the archive, so a 16 MiB download could
	// otherwise cost gigabytes. The biggest chart of the catalog is far below this.
	maxDecompressedChart = 48 << 20

	// maxHubWork is how many charts from search are fetched and rendered at the same time.
	maxHubWork = 3

	maxReviewLine  = 300 // characters of one line of a review
	maxListedKinds = 12  // names listed in one warning before "and N more"
)

func init() {
	loader.MaxDecompressedChartSize = maxDecompressedChart
}

// ---- network: where a chart download may connect ----------------------------------------------

var (
	errBlockedAddress = errors.New("the address is not a public one")

	// reservedPrefixes are ranges netip does not call private or loopback but that are not the open internet.
	reservedPrefixes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can reach IPv4 private ranges
		netip.MustParsePrefix("2002::/16"),    // 6to4 embeds an IPv4 address
	}
)

// blockedIP reports an address a chart download must never connect to: loopback, private, link-local
// (cloud metadata lives at 169.254.169.254), multicast, unspecified or reserved ranges.
func blockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, prefix := range reservedPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// guardedControl runs after DNS, on the address that is about to be connected, so a host name
// that resolves to an internal address (DNS rebinding included) is refused. allowed lists
// proxies the user configured: the connection goes to the proxy, which is the user's choice.
func guardedControl(allowed map[string]bool) func(network, address string, _ syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if allowed[address] {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%w (%s)", errBlockedAddress, address)
		}
		ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
		if err != nil || blockedIP(ip) {
			return fmt.Errorf("%w (%s)", errBlockedAddress, host)
		}
		return nil
	}
}

// proxyAddresses resolves the proxies named in the environment to "ip:port".
func proxyAddresses() map[string]bool {
	out := map[string]bool{}
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		raw := os.Getenv(name)
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Hostname() == "" {
			continue
		}
		port := parsed.Port()
		if port == "" {
			port = map[string]string{"https": "443", "socks5": "1080", "socks5h": "1080"}[parsed.Scheme]
			if port == "" {
				port = "80"
			}
		}
		ips, err := net.LookupIP(parsed.Hostname())
		if err != nil {
			continue
		}
		for _, ip := range ips {
			out[net.JoinHostPort(ip.String(), port)] = true
		}
	}
	return out
}

// guardedTransport is the transport every download from the internet uses.
func guardedTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   guardedControl(proxyAddresses()),
	}).DialContext
	return transport
}

// ---- cluster: charts from search may not read Secrets while they render ----------------------

var errSecretsBlind = errors.New("this chart tried to read Secrets while it was rendering, which the console does not allow for charts from outside the catalog")

// secretBlind refuses every request that is not a plain read, and every read of a Secret. Helm's
// `lookup` template function reads cluster objects with the credentials of whoever installs, so
// a chart (or a value run through `tpl`) could otherwise copy a Secret into an object name, an
// image or an annotation of what it installs.
type secretBlind struct{ next http.RoundTripper }

func (t secretBlind) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return nil, fmt.Errorf("%w: only reads are allowed while rendering", errSecretsBlind)
	}
	clean := path.Clean("/" + strings.ToLower(req.URL.Path))
	for _, segment := range strings.Split(clean, "/") {
		if segment == "secrets" {
			return nil, errSecretsBlind
		}
	}
	return t.next.RoundTrip(req)
}

// secretBlindGetter hands Helm's renderer a client configuration that cannot read Secrets. The
// rest of Helm (the release storage, the objects it creates) keeps the original client.
type secretBlindGetter struct{ action.RESTClientGetter }

func (g secretBlindGetter) ToRESTConfig() (*rest.Config, error) {
	config, err := g.RESTClientGetter.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	config = rest.CopyConfig(config)
	config.Wrap(func(next http.RoundTripper) http.RoundTripper { return secretBlind{next: next} })
	return config, nil
}

// blindToSecrets applies secretBlind to an action configuration. Charts from the catalog are
// reviewed and pinned; only charts from outside it get this.
func blindToSecrets(cfg *action.Configuration) *action.Configuration {
	if cfg != nil && cfg.RESTClientGetter != nil {
		cfg.RESTClientGetter = secretBlindGetter{cfg.RESTClientGetter}
	}
	return cfg
}

// lookupCall finds uses of `lookup` in a chart: its templates, its other files and the strings of
// its default values, because values can be rendered with `tpl`. It is a hint for the review, not
// the defense (the defense is secretBlind), so obfuscation only costs the warning.
var lookupCall = regexp.MustCompile(`\{\{[^\n]{0,400}?\blookup\b`)

func chartLooksUp(c *chart.Chart) bool {
	if c == nil {
		return false
	}
	for _, f := range c.Templates {
		if lookupCall.Match(f.Data) {
			return true
		}
	}
	for _, f := range c.Files {
		if len(f.Data) <= 1<<20 && lookupCall.Match(f.Data) {
			return true
		}
	}
	if valuesLookUp(c.Values) {
		return true
	}
	for _, dep := range c.Dependencies() {
		if chartLooksUp(dep) {
			return true
		}
	}
	return false
}

func valuesLookUp(node any) bool {
	switch t := node.(type) {
	case string:
		return lookupCall.MatchString(t)
	case map[string]any:
		for _, v := range t {
			if valuesLookUp(v) {
				return true
			}
		}
	case []any:
		for _, v := range t {
			if valuesLookUp(v) {
				return true
			}
		}
	}
	return false
}

// ---- values: plain text only --------------------------------------------------------------------

// plainValue refuses a setting that could do more than be a value. `{{` starts a template action
// when the chart runs the value through `tpl`, and a value that parses as a YAML or JSON object
// or list can be turned into whole objects by `fromYaml` or `fromJson`.
func plainValue(value string, allowStructure bool) error {
	if strings.Contains(value, "{{") || strings.Contains(value, "}}") {
		return badRequest("a value may not contain {{ or }}: charts can run values as templates")
	}
	if allowStructure {
		return nil
	}
	var parsed any
	if err := yaml.Unmarshal([]byte(strings.TrimSpace(value)), &parsed); err == nil {
		switch parsed.(type) {
		case map[string]any, []any:
			return badRequest("a value may not be a list or a group of values (YAML or JSON): give a plain value")
		}
	}
	return nil
}

// ---- review text ------------------------------------------------------------------------------------

// safeLine makes text from a chart fit for a review line: invisible and direction-changing
// characters (a right-to-left override can make one name read as another) become "?", and a
// line has a length.
func safeLine(text string) string {
	var b strings.Builder
	n := 0
	for _, r := range text {
		if n == maxReviewLine {
			b.WriteString("…")
			break
		}
		if !unicode.IsPrint(r) {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// listNames joins names for a warning, keeping the line short when a chart creates hundreds.
func listNames(names []string) string {
	if len(names) > maxListedKinds {
		return strings.Join(names[:maxListedKinds], ", ") + fmt.Sprintf(" and %d more", len(names)-maxListedKinds)
	}
	return strings.Join(names, ", ")
}

// ---- hooks and crds/ objects --------------------------------------------------------------------------

// hookDocs lists the objects Helm runs as hooks (a pre-install Job, say). They are not in the
// release manifest, so without this a chart could hide an image or a privileged pod in a hook.
func hookDocs(rel *release.Release, namespace string) []manifestDoc {
	if rel == nil {
		return nil
	}
	var docs []manifestDoc
	for _, hook := range rel.Hooks {
		events := make([]string, 0, len(hook.Events))
		for _, e := range hook.Events {
			events = append(events, string(e))
		}
		for _, doc := range parseManifest(hook.Manifest, namespace) {
			doc.hook = strings.Join(events, ", ")
			docs = append(docs, doc)
		}
	}
	return docs
}

// crdDirObjects lists what the crds/ directories hold that is not a CustomResourceDefinition.
// Helm applies every object of crds/ before anything else, so a ClusterRoleBinding there would
// otherwise never be listed or analyzed.
func crdDirObjects(c *chart.Chart) []manifestDoc {
	if c == nil {
		return nil
	}
	var docs []manifestDoc
	for _, crd := range c.CRDObjects() {
		for _, doc := range parseManifest(string(crd.File.Data), "") {
			if doc.Kind != "CustomResourceDefinition" {
				doc.fromCRDDir = true
				docs = append(docs, doc)
			}
		}
	}
	return docs
}

// withExtras is what a review analyzes: the release manifest plus its hooks and the odd objects of crds/.
func withExtras(docs []manifestDoc, rel *release.Release, c *chart.Chart, namespace string) []manifestDoc {
	out := append([]manifestDoc{}, docs...)
	out = append(out, hookDocs(rel, namespace)...)
	return append(out, crdDirObjects(c)...)
}

// ---- how many at once -------------------------------------------------------------------------------------

// slotWait is how long a request waits for a slot before it is told the console is busy.
var slotWait = 10 * time.Second

// acquire takes one of the slots for fetching and rendering charts from search.
func (h *helmEngine) acquire(ctx context.Context) (func(), error) {
	timer := time.NewTimer(slotWait)
	defer timer.Stop()
	select {
	case h.slots <- struct{}{}:
		return func() { <-h.slots }, nil
	case <-ctx.Done():
	case <-timer.C:
	}
	return nil, &apiError{status: http.StatusServiceUnavailable, code: "busy",
		msg: "the console is busy with other charts right now", hint: "Try again in a moment."}
}

// charCount is the length of text in characters.
func charCount(text string) int { return utf8.RuneCountInString(text) }
