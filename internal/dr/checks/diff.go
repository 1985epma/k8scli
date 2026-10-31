package checks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/1985epma/k8scli/internal/dr"
)

const defaultDiffItems = 500

var crdResource = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// Kinds of difference.
const (
	DiffMissingInStandby = "missing-in-standby"
	DiffExtraInStandby   = "extra-in-standby"
	DiffDiffers          = "differs"
)

// DiffOptions tune Diff.
type DiffOptions struct {
	// Namespaces to compare; empty means the namespaces of the primary except the system ones.
	Namespaces    []string
	IncludeSystem bool
	// MaxItems bounds the output (default 500).
	MaxItems int
}

// DiffItem is one difference.
type DiffItem struct {
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	// Change is missing-in-standby, extra-in-standby or differs.
	Change   string   `json:"change"`
	Severity Severity `json:"severity"`
	Detail   string   `json:"detail,omitempty"`
}

// NamespaceDiff groups the differences of one namespace.
type NamespaceDiff struct {
	Name             string     `json:"name"`
	MissingInStandby bool       `json:"missingInStandby,omitempty"`
	Items            []DiffItem `json:"items"`
}

// DiffSummary counts the differences by severity.
type DiffSummary struct {
	Identical bool `json:"identical"`
	High      int  `json:"high"`
	Medium    int  `json:"medium"`
	Low       int  `json:"low"`
}

// DiffReport compares a primary cluster with its standby. Only names, images, replicas, ports and key
// NAMES are compared: ConfigMap values are never read into the report.
type DiffReport struct {
	Primary    string          `json:"primary"`
	Standby    string          `json:"standby"`
	Namespaces []NamespaceDiff `json:"namespaces"`
	// Cluster holds the cluster-wide differences (CustomResourceDefinitions).
	Cluster   []DiffItem  `json:"cluster"`
	Summary   DiffSummary `json:"summary"`
	Notices   []string    `json:"notices,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
}

type differ struct {
	options DiffOptions
	report  *DiffReport
	count   int
}

func (d *differ) full() bool { return d.count >= d.options.MaxItems }

// add records an item into a list unless the output limit was reached.
func (d *differ) add(list *[]DiffItem, item DiffItem) {
	if d.full() {
		d.report.Truncated = true
		return
	}
	d.count++
	*list = append(*list, item)
	switch item.Severity {
	case SeverityHigh:
		d.report.Summary.High++
	case SeverityMedium:
		d.report.Summary.Medium++
	default:
		d.report.Summary.Low++
	}
}

// Diff compares the workloads, Services, ConfigMap key sets, Ingress hosts and CRDs of two clusters,
// which is how drift of a standby is found. It only reads.
func Diff(ctx context.Context, primary, standby *dr.Env, options DiffOptions) (*DiffReport, error) {
	if err := requireClient(primary); err != nil {
		return nil, fmt.Errorf("primary: %w", err)
	}
	if err := requireClient(standby); err != nil {
		return nil, fmt.Errorf("standby: %w", err)
	}
	if options.MaxItems <= 0 {
		options.MaxItems = defaultDiffItems
	}
	d := &differ{options: options, report: &DiffReport{
		Primary: primary.Context, Standby: standby.Context, Namespaces: []NamespaceDiff{}, Cluster: []DiffItem{},
	}}

	namespaces := append([]string(nil), options.Namespaces...)
	if len(namespaces) == 0 {
		var err error
		if namespaces, err = namespaceNames(ctx, primary.Client, options.IncludeSystem); err != nil {
			return nil, err
		}
	}
	sort.Strings(namespaces)

	for _, namespace := range namespaces {
		result := NamespaceDiff{Name: namespace, Items: []DiffItem{}}
		if _, err := standby.Client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			result.MissingInStandby = true
			d.add(&result.Items, DiffItem{Namespace: namespace, Kind: "Namespace", Name: namespace, Change: DiffMissingInStandby, Severity: SeverityHigh,
				Detail: "The namespace does not exist on the standby."})
			d.report.Namespaces = append(d.report.Namespaces, result)
			continue
		} else if err != nil {
			d.report.Notices = append(d.report.Notices, fmt.Sprintf("Could not check namespace %s on the standby: %v", namespace, err))
			continue
		}

		if err := d.workloads(ctx, primary, standby, namespace, &result); err != nil {
			return nil, err
		}
		d.services(ctx, primary, standby, namespace, &result)
		d.configMaps(ctx, primary, standby, namespace, &result)
		d.ingresses(ctx, primary, standby, namespace, &result)
		d.report.Namespaces = append(d.report.Namespaces, result)
	}

	d.crds(ctx, primary, standby)
	d.report.Summary.Identical = d.count == 0 && len(d.report.Notices) == 0
	sort.Strings(d.report.Notices)
	return d.report, nil
}

func sortItems(items []DiffItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch {
		case a.Severity.rank() != b.Severity.rank():
			return a.Severity.rank() > b.Severity.rank()
		case a.Kind != b.Kind:
			return a.Kind < b.Kind
		default:
			return a.Name < b.Name
		}
	})
}

// workloads compares Deployments and StatefulSets: presence, container images and replicas.
func (d *differ) workloads(ctx context.Context, primary, standby *dr.Env, namespace string, out *NamespaceDiff) error {
	p, err := listWorkloads(ctx, primary.Client, namespace)
	if err != nil {
		return err
	}
	s, err := listWorkloads(ctx, standby.Client, namespace)
	if err != nil {
		return err
	}
	standbyBy := map[string]workload{}
	for _, w := range s {
		standbyBy[w.Kind+"/"+w.Name] = w
	}
	seen := map[string]bool{}

	var items []DiffItem
	for _, w := range p {
		key := w.Kind + "/" + w.Name
		seen[key] = true
		other, ok := standbyBy[key]
		if !ok {
			items = append(items, DiffItem{Namespace: namespace, Kind: w.Kind, Name: w.Name, Change: DiffMissingInStandby, Severity: SeverityHigh,
				Detail: fmt.Sprintf("%d replica(s) on the primary, nothing on the standby.", w.Replicas)})
			continue
		}
		if detail := imageDiff(w, other); detail != "" {
			items = append(items, DiffItem{Namespace: namespace, Kind: w.Kind, Name: w.Name, Change: DiffDiffers, Severity: SeverityMedium, Detail: detail})
		}
		if w.Replicas != other.Replicas {
			items = append(items, DiffItem{Namespace: namespace, Kind: w.Kind, Name: w.Name, Change: DiffDiffers, Severity: SeverityLow,
				Detail: fmt.Sprintf("replicas: %d on the primary, %d on the standby.", w.Replicas, other.Replicas)})
		}
	}
	for _, w := range s {
		if !seen[w.Kind+"/"+w.Name] {
			items = append(items, DiffItem{Namespace: namespace, Kind: w.Kind, Name: w.Name, Change: DiffExtraInStandby, Severity: SeverityLow,
				Detail: "Only on the standby."})
		}
	}
	sortItems(items)
	for _, item := range items {
		d.add(&out.Items, item)
	}
	return nil
}

// imageDiff describes the container images that differ, "" when they match.
func imageDiff(a, b workload) string {
	images := func(w workload) map[string]string {
		out := map[string]string{}
		for _, c := range w.Template.Spec.Containers {
			out[c.Name] = c.Image
		}
		return out
	}
	pa, pb := images(a), images(b)
	names := map[string]bool{}
	for n := range pa {
		names[n] = true
	}
	for n := range pb {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var parts []string
	for _, n := range sorted {
		switch {
		case pa[n] == "":
			parts = append(parts, fmt.Sprintf("container %s exists only on the standby", n))
		case pb[n] == "":
			parts = append(parts, fmt.Sprintf("container %s is missing on the standby", n))
		case pa[n] != pb[n]:
			parts = append(parts, fmt.Sprintf("container %s: %s (primary) vs %s (standby)", n, pa[n], pb[n]))
		}
	}
	return strings.Join(parts, "; ")
}

func (d *differ) services(ctx context.Context, primary, standby *dr.Env, namespace string, out *NamespaceDiff) {
	list := func(env *dr.Env) (map[string]corev1.Service, error) {
		items, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]corev1.Service, string, error) {
			page, err := env.Client.CoreV1().Services(namespace).List(ctx, o)
			if err != nil {
				return nil, "", err
			}
			return page.Items, page.Continue, nil
		})
		m := map[string]corev1.Service{}
		for _, s := range items {
			if !ownedByController(&s) && (s.Name != "kubernetes" || namespace != "default") {
				m[s.Name] = s
			}
		}
		return m, err
	}
	p, err := list(primary)
	if err != nil {
		d.report.Notices = append(d.report.Notices, fmt.Sprintf("Could not read Services in %s on the primary: %v", namespace, err))
		return
	}
	s, err := list(standby)
	if err != nil {
		d.report.Notices = append(d.report.Notices, fmt.Sprintf("Could not read Services in %s on the standby: %v", namespace, err))
		return
	}

	var items []DiffItem
	for name, a := range p {
		b, ok := s[name]
		if !ok {
			items = append(items, DiffItem{Namespace: namespace, Kind: "Service", Name: name, Change: DiffMissingInStandby, Severity: SeverityHigh})
			continue
		}
		var parts []string
		if a.Spec.Type != b.Spec.Type {
			parts = append(parts, fmt.Sprintf("type: %s vs %s", a.Spec.Type, b.Spec.Type))
		}
		if pa, pb := servicePorts(a), servicePorts(b); pa != pb {
			parts = append(parts, fmt.Sprintf("ports: %s vs %s", pa, pb))
		}
		if len(parts) > 0 {
			items = append(items, DiffItem{Namespace: namespace, Kind: "Service", Name: name, Change: DiffDiffers, Severity: SeverityMedium, Detail: strings.Join(parts, "; ")})
		}
	}
	for name := range s {
		if _, ok := p[name]; !ok {
			items = append(items, DiffItem{Namespace: namespace, Kind: "Service", Name: name, Change: DiffExtraInStandby, Severity: SeverityLow})
		}
	}
	sortItems(items)
	for _, item := range items {
		d.add(&out.Items, item)
	}
}

func servicePorts(s corev1.Service) string {
	ports := make([]string, 0, len(s.Spec.Ports))
	for _, p := range s.Spec.Ports {
		protocol := p.Protocol
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		ports = append(ports, fmt.Sprintf("%d/%s", p.Port, protocol))
	}
	sort.Strings(ports)
	return strings.Join(ports, ",")
}

// configMaps compares KEY NAMES only: values are never read into the report.
func (d *differ) configMaps(ctx context.Context, primary, standby *dr.Env, namespace string, out *NamespaceDiff) {
	list := func(env *dr.Env) (map[string][]string, error) {
		items, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]corev1.ConfigMap, string, error) {
			page, err := env.Client.CoreV1().ConfigMaps(namespace).List(ctx, o)
			if err != nil {
				return nil, "", err
			}
			return page.Items, page.Continue, nil
		})
		m := map[string][]string{}
		for i := range items {
			cm := &items[i]
			if cm.Name == "kube-root-ca.crt" || ownedByController(cm) {
				continue
			}
			keys := make([]string, 0, len(cm.Data)+len(cm.BinaryData))
			for k := range cm.Data {
				keys = append(keys, k)
			}
			for k := range cm.BinaryData {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			m[cm.Name] = keys
		}
		return m, err
	}
	p, err := list(primary)
	if err != nil {
		d.report.Notices = append(d.report.Notices, fmt.Sprintf("Could not read ConfigMaps in %s on the primary: %v", namespace, err))
		return
	}
	s, err := list(standby)
	if err != nil {
		d.report.Notices = append(d.report.Notices, fmt.Sprintf("Could not read ConfigMaps in %s on the standby: %v", namespace, err))
		return
	}

	var items []DiffItem
	for name, keys := range p {
		other, ok := s[name]
		if !ok {
			items = append(items, DiffItem{Namespace: namespace, Kind: "ConfigMap", Name: name, Change: DiffMissingInStandby, Severity: SeverityMedium})
			continue
		}
		missing, extra := setDifference(keys, other), setDifference(other, keys)
		if len(missing) > 0 {
			items = append(items, DiffItem{Namespace: namespace, Kind: "ConfigMap", Name: name, Change: DiffDiffers, Severity: SeverityMedium,
				Detail: "keys missing on the standby: " + strings.Join(missing, ", ")})
		}
		if len(extra) > 0 {
			items = append(items, DiffItem{Namespace: namespace, Kind: "ConfigMap", Name: name, Change: DiffDiffers, Severity: SeverityLow,
				Detail: "keys only on the standby: " + strings.Join(extra, ", ")})
		}
	}
	for name := range s {
		if _, ok := p[name]; !ok {
			items = append(items, DiffItem{Namespace: namespace, Kind: "ConfigMap", Name: name, Change: DiffExtraInStandby, Severity: SeverityLow})
		}
	}
	sortItems(items)
	for _, item := range items {
		d.add(&out.Items, item)
	}
}

// setDifference returns the elements of a that are not in b.
func setDifference(a, b []string) []string {
	in := map[string]bool{}
	for _, v := range b {
		in[v] = true
	}
	var out []string
	for _, v := range a {
		if !in[v] {
			out = append(out, v)
		}
	}
	return out
}

func (d *differ) ingresses(ctx context.Context, primary, standby *dr.Env, namespace string, out *NamespaceDiff) {
	list := func(env *dr.Env) (map[string][]string, error) {
		page, err := env.Client.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		m := map[string][]string{}
		for i := range page.Items {
			m[page.Items[i].Name] = ingressHosts(&page.Items[i])
		}
		return m, nil
	}
	p, err := list(primary)
	if err != nil {
		d.report.Notices = append(d.report.Notices, fmt.Sprintf("Could not read Ingresses in %s on the primary: %v", namespace, err))
		return
	}
	s, err := list(standby)
	if err != nil {
		d.report.Notices = append(d.report.Notices, fmt.Sprintf("Could not read Ingresses in %s on the standby: %v", namespace, err))
		return
	}

	var items []DiffItem
	for name, hosts := range p {
		other, ok := s[name]
		if !ok {
			items = append(items, DiffItem{Namespace: namespace, Kind: "Ingress", Name: name, Change: DiffMissingInStandby, Severity: SeverityMedium,
				Detail: "hosts: " + strings.Join(hosts, ", ")})
			continue
		}
		if missing := setDifference(hosts, other); len(missing) > 0 {
			items = append(items, DiffItem{Namespace: namespace, Kind: "Ingress", Name: name, Change: DiffDiffers, Severity: SeverityMedium,
				Detail: "hosts missing on the standby: " + strings.Join(missing, ", ")})
		}
		if extra := setDifference(other, hosts); len(extra) > 0 {
			items = append(items, DiffItem{Namespace: namespace, Kind: "Ingress", Name: name, Change: DiffDiffers, Severity: SeverityLow,
				Detail: "hosts only on the standby: " + strings.Join(extra, ", ")})
		}
	}
	for name := range s {
		if _, ok := p[name]; !ok {
			items = append(items, DiffItem{Namespace: namespace, Kind: "Ingress", Name: name, Change: DiffExtraInStandby, Severity: SeverityLow})
		}
	}
	sortItems(items)
	for _, item := range items {
		d.add(&out.Items, item)
	}
}

func ingressHosts(ing *networkingv1.Ingress) []string {
	set := map[string]bool{}
	for _, rule := range ing.Spec.Rules {
		if rule.Host != "" {
			set[rule.Host] = true
		}
	}
	hosts := make([]string, 0, len(set))
	for h := range set {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}

// crdVersions lists CustomResourceDefinitions as name -> served versions.
func crdVersions(ctx context.Context, env *dr.Env) (map[string][]string, bool, error) {
	if env.Dynamic == nil {
		return nil, false, nil
	}
	items, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]unstructured.Unstructured, string, error) {
		page, err := env.Dynamic.Resource(crdResource).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.GetContinue(), nil
	})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	out := map[string][]string{}
	for i := range items {
		versions, _, _ := unstructured.NestedSlice(items[i].Object, "spec", "versions")
		var names []string
		for _, v := range versions {
			if m, ok := v.(map[string]any); ok {
				if name, _ := m["name"].(string); name != "" {
					if served, found := m["served"].(bool); !found || served {
						names = append(names, name)
					}
				}
			}
		}
		sort.Strings(names)
		out[items[i].GetName()] = names
	}
	return out, true, nil
}

func (d *differ) crds(ctx context.Context, primary, standby *dr.Env) {
	p, okP, err := crdVersions(ctx, primary)
	if err != nil {
		d.report.Notices = append(d.report.Notices, fmt.Sprintf("Could not read CRDs on the primary: %v", err))
		return
	}
	s, okS, err := crdVersions(ctx, standby)
	if err != nil {
		d.report.Notices = append(d.report.Notices, fmt.Sprintf("Could not read CRDs on the standby: %v", err))
		return
	}
	if !okP || !okS {
		d.report.Notices = append(d.report.Notices, "CustomResourceDefinitions were not compared: the dynamic client or the CRD API is not available on one side.")
		return
	}

	var items []DiffItem
	for name, versions := range p {
		other, ok := s[name]
		switch {
		case !ok:
			items = append(items, DiffItem{Kind: "CustomResourceDefinition", Name: name, Change: DiffMissingInStandby, Severity: SeverityHigh,
				Detail: "versions " + strings.Join(versions, ", ")})
		case strings.Join(versions, ",") != strings.Join(other, ","):
			items = append(items, DiffItem{Kind: "CustomResourceDefinition", Name: name, Change: DiffDiffers, Severity: SeverityMedium,
				Detail: fmt.Sprintf("served versions: [%s] (primary) vs [%s] (standby)", strings.Join(versions, ", "), strings.Join(other, ", "))})
		}
	}
	for name := range s {
		if _, ok := p[name]; !ok {
			items = append(items, DiffItem{Kind: "CustomResourceDefinition", Name: name, Change: DiffExtraInStandby, Severity: SeverityLow})
		}
	}
	sortItems(items)
	for _, item := range items {
		d.add(&d.report.Cluster, item)
	}
}
