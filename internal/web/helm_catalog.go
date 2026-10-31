package web

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// The catalog of apps the console can install (ADR 0004). Each entry pins the
// archive URL, the exact chart version and the sha256 of the archive; a
// download that does not match is refused. Run scripts/update-helm-catalog.sh
// to see newer versions and digests, review them, then edit this file by hand.

const (
	fieldBool   = "bool"
	fieldString = "string"
	fieldInt    = "int"
	fieldSelect = "select"

	// defaultStringMax is the longest string a field accepts unless it says otherwise.
	defaultStringMax = 100
	// placeholderSecret is what stands in for a secret in every command shown.
	placeholderSecret = "<secret>"
)

// helmField is one setting people may change when they install an app.
type helmField struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Type    string   `json:"type"`
	Default string   `json:"default"`
	Help    string   `json:"help,omitempty"`
	Options []string `json:"options,omitempty"`
	// Secret fields are never returned by the API, never written to a log and
	// never shown in a command.
	Secret bool `json:"secret"`
	// Min and Max bound an int; for a string, Max is the longest it may be.
	Min int `json:"min"`
	Max int `json:"max"`

	// pattern restricts a string field; nil means any printable text.
	pattern *regexp.Regexp
	// path is where the value goes in the chart's values; dots nest. apply and
	// read replace it for the few fields that need another shape.
	path  string
	apply func(values map[string]any, value string)
	read  func(values map[string]any) (string, bool)
}

// helmEntry is one app of the catalog.
type helmEntry struct {
	ID               string      `json:"id"`
	Name             string      `json:"name"`
	Description      string      `json:"description"`
	Category         string      `json:"category"`
	Chart            string      `json:"chart"`
	Version          string      `json:"version"`
	AppVersion       string      `json:"appVersion"`
	Repo             string      `json:"repo"`
	DefaultName      string      `json:"defaultName"`
	DefaultNamespace string      `json:"defaultNamespace"`
	CreateNamespace  bool        `json:"createNamespace"`
	ClusterScoped    bool        `json:"clusterScoped"`
	Docs             string      `json:"docs"`
	Fields           []helmField `json:"fields"`

	// archiveURL and digest identify the exact chart archive. They are not
	// part of the API: the browser never sees or chooses them.
	archiveURL string
	digest     string
}

func (e *helmEntry) field(key string) *helmField {
	for i := range e.Fields {
		if e.Fields[i].Key == key {
			return &e.Fields[i]
		}
	}
	return nil
}

var durationPattern = regexp.MustCompile(`^[0-9]{1,4}(ms|s|m|h|d|w|y)$`)

func serviceTypeField(path, def string, options ...string) helmField {
	return helmField{
		Key: "serviceType", Label: "Service type", Type: fieldSelect, Default: def, Options: options, path: path,
		Help: "ClusterIP is reachable only inside the cluster. LoadBalancer is how Docker Desktop publishes it on localhost.",
	}
}

func replicasField(path string, max int, label string) helmField {
	return helmField{Key: "replicas", Label: label, Type: fieldInt, Default: "1", Min: 1, Max: max, path: path}
}

// helmCatalog is the list of installable apps, in the order the UI shows them:
// the first group below, then helmCatalogMore (helm_catalog_more.go).
var helmCatalog = slices.Concat(helmCatalogCore, helmCatalogMore)

// helmCatalogCore holds the first group of apps.
var helmCatalogCore = []helmEntry{
	{
		ID: "podinfo", Name: "Podinfo", Category: "Demo",
		Description: "A tiny web app that is ideal to try the feature: install it, scale it, remove it.",
		Chart:       "podinfo", Version: "6.15.0", AppVersion: "6.15.0",
		Repo:        "https://stefanprodan.github.io/podinfo",
		archiveURL:  "https://stefanprodan.github.io/podinfo/podinfo-6.15.0.tgz",
		digest:      "5ca7896889b539e04cdad4df2093ff1ff7576295e7e3ef90a1e1845e3a334d75",
		DefaultName: "podinfo", DefaultNamespace: "podinfo", CreateNamespace: true,
		Docs: "https://github.com/stefanprodan/podinfo",
		Fields: []helmField{
			replicasField("replicaCount", 5, "Replicas"),
			{Key: "message", Label: "Greeting", Type: fieldString, Max: 100, path: "ui.message", Help: "Shown on the page of the app."},
			serviceTypeField("service.type", "ClusterIP", "ClusterIP", "NodePort", "LoadBalancer"),
		},
	},
	{
		ID: "metrics-server", Name: "Metrics Server", Category: "Observability",
		Description: "Gives the cluster CPU and memory usage. It is what makes `kubectl top`, the dashboard usage bars and CPU autoscaling work.",
		Chart:       "metrics-server", Version: "3.14.0", AppVersion: "0.9.0",
		Repo:        "https://kubernetes-sigs.github.io/metrics-server/",
		archiveURL:  "https://github.com/kubernetes-sigs/metrics-server/releases/download/metrics-server-helm-chart-3.14.0/metrics-server-3.14.0.tgz",
		digest:      "c2ca1185c01e6e7f53dd1b7d131f0c9b3fa50e003ed068b784563a1b5a3422a1",
		DefaultName: "metrics-server", DefaultNamespace: "kube-system", ClusterScoped: true,
		Docs: "https://github.com/kubernetes-sigs/metrics-server/tree/master/charts/metrics-server",
		Fields: []helmField{
			{
				Key: "insecureTLS", Label: "Local cluster: accept the kubelet certificate", Type: fieldBool, Default: "false",
				Help: "Turn it on for Docker Desktop, kind and minikube, whose kubelets use self-signed certificates. Leave it off on a real cluster.",
				apply: func(values map[string]any, value string) {
					if value == "true" {
						values["args"] = []any{"--kubelet-insecure-tls"}
					} else {
						delete(values, "args")
					}
				},
				read: func(values map[string]any) (string, bool) {
					args, ok := values["args"].([]any)
					if !ok {
						return "false", false
					}
					for _, arg := range args {
						if arg == "--kubelet-insecure-tls" {
							return "true", true
						}
					}
					return "false", true
				},
			},
			replicasField("replicas", 3, "Replicas"),
		},
	},
	{
		ID: "cert-manager", Name: "cert-manager", Category: "Security",
		Description: "Issues and renews TLS certificates (for example from Let's Encrypt) for your ingresses.",
		Chart:       "cert-manager", Version: "v1.21.3", AppVersion: "v1.21.3",
		Repo:        "https://charts.jetstack.io",
		archiveURL:  "https://charts.jetstack.io/charts/cert-manager-v1.21.3.tgz",
		digest:      "9ec67938b2981d8b333af61b3fcf53e26b4f6f5deb7ccda3de4ea116bb373a17",
		DefaultName: "cert-manager", DefaultNamespace: "cert-manager", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://cert-manager.io/docs/installation/helm/",
		Fields: []helmField{
			{
				Key: "installCRDs", Label: "Install the CRDs with the chart", Type: fieldBool, Default: "true", path: "crds.enabled",
				Help: "cert-manager does not work without its CustomResourceDefinitions. Helm never deletes them when you uninstall.",
			},
			replicasField("replicaCount", 3, "Replicas"),
		},
	},
	{
		ID: "traefik", Name: "Traefik", Category: "Networking",
		Description: "An ingress controller and reverse proxy that routes outside traffic to your services.",
		Chart:       "traefik", Version: "42.1.3", AppVersion: "v3.7.16",
		Repo:        "https://traefik.github.io/charts",
		archiveURL:  "https://traefik.github.io/charts/traefik/traefik-42.1.3.tgz",
		digest:      "10ec2b032fd7ec9fa04a8816872e20ba0fafc47231f6394b5cb5d522f3b0ef50",
		DefaultName: "traefik", DefaultNamespace: "traefik", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://github.com/traefik/traefik-helm-chart",
		Fields: []helmField{
			serviceTypeField("service.spec.type", "LoadBalancer", "LoadBalancer", "ClusterIP", "NodePort"),
			replicasField("deployment.replicas", 5, "Replicas"),
		},
	},
	{
		ID: "argo-cd", Name: "Argo CD", Category: "Delivery",
		Description: "GitOps continuous delivery: keeps the cluster in sync with what is in Git.",
		Chart:       "argo-cd", Version: "10.11.1", AppVersion: "v3.5.6",
		Repo:        "https://argoproj.github.io/argo-helm",
		archiveURL:  "https://github.com/argoproj/argo-helm/releases/download/argo-cd-10.11.1/argo-cd-10.11.1.tgz",
		digest:      "dbb92a8c7855d4c564b87ab70e70bc36f5b17fd2bd30fa041a31e80dbea99c38",
		DefaultName: "argocd", DefaultNamespace: "argocd", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://github.com/argoproj/argo-helm/tree/main/charts/argo-cd",
		Fields: []helmField{
			serviceTypeField("server.service.type", "ClusterIP", "ClusterIP", "NodePort", "LoadBalancer"),
			replicasField("server.replicas", 3, "Server replicas"),
		},
	},
	{
		ID: "grafana", Name: "Grafana", Category: "Observability",
		Description: "Dashboards for metrics, logs and traces. Add a data source after it starts.",
		Chart:       "grafana", Version: "14.0.1", AppVersion: "13.2.4",
		Repo:        "https://grafana-community.github.io/helm-charts",
		archiveURL:  "https://github.com/grafana-community/helm-charts/releases/download/grafana-14.0.1/grafana-14.0.1.tgz",
		digest:      "4dbc6ed68ee29e78edd4db47d10aad72f8fc473e4169501a2bdaf149463e74df",
		DefaultName: "grafana", DefaultNamespace: "monitoring", CreateNamespace: true,
		Docs: "https://github.com/grafana-community/helm-charts/tree/main/charts/grafana",
		Fields: []helmField{
			{
				Key: "adminPassword", Label: "Admin password", Type: fieldString, Max: 128, Secret: true, path: "adminPassword",
				Help: "Optional. Leave it empty and the chart generates one and keeps it in a Secret of the release.",
			},
			serviceTypeField("service.type", "ClusterIP", "ClusterIP", "NodePort", "LoadBalancer"),
			{Key: "persistence", Label: "Keep data on a volume", Type: fieldBool, Default: "false", path: "persistence.enabled",
				Help: "Without it, dashboards you create are lost when the pod restarts."},
		},
	},
	{
		ID: "kube-prometheus-stack", Name: "Prometheus stack", Category: "Observability",
		Description: "Prometheus, Alertmanager, Grafana and exporters, with ready-made dashboards for Kubernetes. A big install.",
		Chart:       "kube-prometheus-stack", Version: "92.11.0", AppVersion: "v0.94.1",
		Repo:        "https://prometheus-community.github.io/helm-charts",
		archiveURL:  "https://github.com/prometheus-community/helm-charts/releases/download/kube-prometheus-stack-92.11.0/kube-prometheus-stack-92.11.0.tgz",
		digest:      "c357a259965187edcbed86591461bfbd56997591afba09be3a49a09b2bec8a2c",
		DefaultName: "monitoring", DefaultNamespace: "monitoring", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack",
		Fields: []helmField{
			{Key: "grafana", Label: "Include Grafana", Type: fieldBool, Default: "true", path: "grafana.enabled"},
			{Key: "alertmanager", Label: "Include Alertmanager", Type: fieldBool, Default: "true", path: "alertmanager.enabled"},
			{
				Key: "retention", Label: "Keep metrics for", Type: fieldString, Default: "10d", Max: 8, path: "prometheus.prometheusSpec.retention",
				pattern: durationPattern, Help: "A number and a unit: 12h, 10d, 4w.",
			},
			{
				Key: "grafanaPassword", Label: "Grafana admin password", Type: fieldString, Max: 128, Secret: true, path: "grafana.adminPassword",
				Help: "Optional. Leave it empty and the chart generates one.",
			},
		},
	},
}

// catalogEntry looks an app up by id.
func catalogEntry(entries []helmEntry, id string) *helmEntry {
	for i := range entries {
		if entries[i].ID == id {
			return &entries[i]
		}
	}
	return nil
}

// ---- field values ---------------------------------------------------------------

// normalizeFieldValue turns what a JSON client may send (a string, a bool or a
// number) into the string form fields use.
func normalizeFieldValue(raw any) (string, error) {
	switch v := raw.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case float64:
		if v != float64(int64(v)) {
			return "", badRequest("numbers must be whole")
		}
		return strconv.FormatInt(int64(v), 10), nil
	case nil:
		return "", nil
	default:
		return "", badRequest("a field value must be text, a number or true/false")
	}
}

// validate checks a value against the field's type and bounds.
func (f *helmField) validate(value string) error {
	switch f.Type {
	case fieldBool:
		if value != "true" && value != "false" {
			return badRequest("%s must be true or false", f.Label)
		}
	case fieldInt:
		n, err := strconv.Atoi(value)
		if err != nil || n < f.Min || n > f.Max {
			return badRequest("%s must be a whole number from %d to %d", f.Label, f.Min, f.Max)
		}
	case fieldSelect:
		for _, option := range f.Options {
			if value == option {
				return nil
			}
		}
		return badRequest("%s must be one of: %s", f.Label, strings.Join(f.Options, ", "))
	case fieldString:
		limit := f.Max
		if limit == 0 {
			limit = defaultStringMax
		}
		if len(value) > limit {
			return badRequest("%s is too long (at most %d characters)", f.Label, limit)
		}
		for _, r := range value {
			if !unicode.IsPrint(r) {
				return badRequest("%s may not contain control or invisible characters", f.Label)
			}
		}
		if f.pattern != nil && value != "" && !f.pattern.MatchString(value) {
			return badRequest("%s is not in the expected format", f.Label)
		}
		// Charts may run a value as a template; secrets may look like anything else.
		if err := plainValue(value, true); err != nil {
			return badRequest("%s: %v", f.Label, err)
		}
	}
	return nil
}

// typed converts a validated string to the value Helm gets.
func (f *helmField) typed(value string) any {
	switch f.Type {
	case fieldBool:
		return value == "true"
	case fieldInt:
		n, _ := strconv.Atoi(value)
		return int64(n)
	default:
		return value
	}
}

// set writes value into the chart values.
func (f *helmField) set(values map[string]any, value string) {
	if f.apply != nil {
		f.apply(values, value)
		return
	}
	setPath(values, f.path, f.typed(value))
}

// get reads the field back from chart values. ok is false when the release never set it.
func (f *helmField) get(values map[string]any) (string, bool) {
	if f.read != nil {
		return f.read(values)
	}
	raw, ok := getPath(values, f.path)
	if !ok {
		return f.Default, false
	}
	switch v := raw.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		return strconv.FormatInt(int64(v), 10), true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	default:
		return f.Default, false
	}
}

func setPath(values map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	for _, part := range parts[:len(parts)-1] {
		next, ok := values[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			values[part] = next
		}
		values = next
	}
	values[parts[len(parts)-1]] = value
}

func getPath(values map[string]any, path string) (any, bool) {
	var node any = values
	for _, part := range strings.Split(path, ".") {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		node, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return node, true
}

// fieldDefaults is the value of every field before anyone changes it.
func (e *helmEntry) fieldDefaults() map[string]string {
	defaults := make(map[string]string, len(e.Fields))
	for _, f := range e.Fields {
		defaults[f.Key] = f.Default
	}
	return defaults
}

// checkFields validates a request's fields against the entry. Unknown keys are
// an error: nothing outside the declared form can reach the chart.
func (e *helmEntry) checkFields(given map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(given))
	for key, raw := range given {
		f := e.field(key)
		if f == nil {
			return nil, badRequest("%q is not a setting of %s", key, e.Name)
		}
		value, err := normalizeFieldValue(raw)
		if err != nil {
			return nil, badRequest("%s: %v", f.Label, err)
		}
		if err := f.validate(value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

// buildValues turns field values into chart values on top of base (which may be
// nil). Empty strings are left out on install so the chart's own default
// applies; on upgrade (clearEmpty) a non-secret field set to "" is cleared.
// display swaps secrets for a placeholder, for the command shown to people.
func (e *helmEntry) buildValues(base map[string]any, fields map[string]string, clearEmpty, display bool) map[string]any {
	values := deepCopyMap(base)
	for _, f := range e.Fields {
		value, ok := fields[f.Key]
		if !ok {
			continue
		}
		if value == "" && (f.Type == fieldString) && (f.Secret || !clearEmpty) {
			continue
		}
		if f.Secret && display {
			value = placeholderSecret
		}
		f.set(values, value)
	}
	return values
}

func deepCopyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		cp := make([]any, len(t))
		for i := range t {
			cp[i] = deepCopyValue(t[i])
		}
		return cp
	default:
		return v
	}
}

// helmSetFlags renders chart values as --set arguments for the equivalent
// helm command, sorted so the command is stable.
func helmSetFlags(values map[string]any) []string {
	var flags []string
	var walk func(prefix string, node any)
	walk = func(prefix string, node any) {
		switch t := node.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				key := strings.ReplaceAll(k, ".", `\.`)
				if prefix != "" {
					key = prefix + "." + key
				}
				walk(key, t[k])
			}
		case []any:
			for i, item := range t {
				walk(fmt.Sprintf("%s[%d]", prefix, i), item)
			}
		default:
			text := fmt.Sprint(t)
			if text != placeholderSecret {
				text = strings.ReplaceAll(text, ",", `\,`)
			}
			flags = append(flags, "--set "+shellWord(prefix+"="+text))
		}
	}
	walk("", values)
	return flags
}

// shellWord quotes a word for the equivalent command, leaving the secret
// placeholder readable.
func shellWord(word string) string {
	if key, ok := strings.CutSuffix(word, "="+placeholderSecret); ok {
		return kubectlQuote(key) + "=" + placeholderSecret
	}
	return kubectlQuote(word)
}
