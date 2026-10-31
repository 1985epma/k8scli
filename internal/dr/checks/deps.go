package checks

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"

	"github.com/1985epma/k8scli/internal/dr"
)

const maxReferenceText = 4096

// DepNode is a workload in the dependency graph.
type DepNode struct {
	Key       string `json:"key"` // namespace/Kind/name
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Stateful is true for StatefulSets and for workloads that mount a PVC.
	Stateful bool `json:"stateful"`
	// EntryPoint is true when an Ingress routes traffic to it (nothing is expected to depend on it).
	EntryPoint bool     `json:"entryPoint,omitempty"`
	Hosts      []string `json:"hosts,omitempty"`
	// Tier is the suggested start order: 0 starts first.
	Tier int `json:"tier"`
}

// DepEdge says From depends on To.
type DepEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Via explains how the dependency was inferred ("env DB_HOST", "configmap app/DB_URL", "networkpolicy db-allow").
	Via string `json:"via"`
	// Cycle marks an edge that closes a cycle; it is ignored when tiers are computed.
	Cycle bool `json:"cycle,omitempty"`
}

// Graph is the inferred dependency graph.
type Graph struct {
	Nodes   []DepNode  `json:"nodes"`
	Edges   []DepEdge  `json:"edges"`
	Cycles  [][]string `json:"cycles,omitempty"`
	Notices []string   `json:"notices,omitempty"`
}

// DiscoverOptions select the namespaces to analyse.
type DiscoverOptions struct {
	// Namespaces; empty means every namespace except the system ones.
	Namespaces    []string
	IncludeSystem bool
}

var tokenPattern = regexp.MustCompile(`[a-z0-9][a-z0-9.\-]*`)

type depWorkload struct {
	workload
	key      string
	stateful bool
	pvc      bool
	texts    []referenceText
	// ingressHosts are the hosts of the Ingress rules that route to this workload.
	ingressHosts []string
}

// referenceText is a string that may mention a Service, and where it came from.
type referenceText struct {
	value string
	via   string
}

// DiscoverDependencies infers which workloads depend on which, from the Services their environment
// variables, ConfigMaps and arguments mention, the Ingress backends and the NetworkPolicy peers, and
// suggests a start order. It is a best-effort hint: review it before trusting it. It only reads, and it
// never prints the text it scans (only the name of the variable or key it found a reference in).
func DiscoverDependencies(ctx context.Context, env *dr.Env, options DiscoverOptions) (*Graph, error) {
	if err := requireClient(env); err != nil {
		return nil, err
	}
	client := env.Client
	namespaces := append([]string(nil), options.Namespaces...)
	if len(namespaces) == 0 {
		var err error
		if namespaces, err = namespaceNames(ctx, client, options.IncludeSystem); err != nil {
			return nil, err
		}
	}
	sort.Strings(namespaces)

	graph := &Graph{Nodes: []DepNode{}, Edges: []DepEdge{}}
	byKey := map[string]*depWorkload{}
	services := map[string]*corev1.Service{} // namespace/name
	var order []string
	notice := func(format string, args ...any) { graph.Notices = append(graph.Notices, fmt.Sprintf(format, args...)) }

	type namespaceData struct {
		policies  []networkingv1.NetworkPolicy
		ingresses []networkingv1.Ingress
	}
	data := map[string]*namespaceData{}

	for _, namespace := range namespaces {
		workloads, err := listWorkloads(ctx, client, namespace)
		if err != nil {
			return nil, err
		}
		serviceList, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]corev1.Service, string, error) {
			page, err := client.CoreV1().Services(namespace).List(ctx, o)
			if err != nil {
				return nil, "", err
			}
			return page.Items, page.Continue, nil
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list services in %s: %w", namespace, err)
		}
		for i := range serviceList {
			services[namespace+"/"+serviceList[i].Name] = &serviceList[i]
		}

		configMaps := map[string]map[string]string{}
		cms, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]corev1.ConfigMap, string, error) {
			page, err := client.CoreV1().ConfigMaps(namespace).List(ctx, o)
			if err != nil {
				return nil, "", err
			}
			return page.Items, page.Continue, nil
		})
		if err != nil {
			notice("Could not read ConfigMaps in %s: %v", namespace, err)
		}
		for i := range cms {
			configMaps[cms[i].Name] = cms[i].Data
		}

		d := &namespaceData{}
		if policies, err := client.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{}); err == nil {
			d.policies = policies.Items
		} else {
			notice("Could not read NetworkPolicies in %s: %v", namespace, err)
		}
		if ingresses, err := client.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{}); err == nil {
			d.ingresses = ingresses.Items
		} else {
			notice("Could not read Ingresses in %s: %v", namespace, err)
		}
		data[namespace] = d

		for _, w := range workloads {
			dw := &depWorkload{workload: w, key: namespace + "/" + w.Kind + "/" + w.Name, stateful: w.Kind == "StatefulSet"}
			for _, v := range w.Template.Spec.Volumes {
				if v.PersistentVolumeClaim != nil {
					dw.pvc, dw.stateful = true, true
				}
			}
			dw.texts = collectTexts(&w.Template.Spec, configMaps)
			byKey[dw.key] = dw
			order = append(order, dw.key)
		}
	}

	// Which workloads sit behind each Service.
	targets := map[string][]string{}
	for key, svc := range services {
		if len(svc.Spec.Selector) == 0 {
			continue
		}
		namespace := strings.SplitN(key, "/", 2)[0]
		selector := labels.SelectorFromSet(svc.Spec.Selector)
		for _, wkey := range order {
			w := byKey[wkey]
			if w.Namespace == namespace && selector.Matches(labels.Set(w.Template.Labels)) {
				targets[key] = append(targets[key], wkey)
			}
		}
	}

	edges := map[[2]string][]string{}
	addEdge := func(from, to, via string) {
		if from == to {
			return
		}
		pair := [2]string{from, to}
		for _, existing := range edges[pair] {
			if existing == via {
				return
			}
		}
		if len(edges[pair]) < 3 {
			edges[pair] = append(edges[pair], via)
		}
	}

	// Services mentioned in env vars, ConfigMaps and arguments.
	for _, wkey := range order {
		w := byKey[wkey]
		for _, text := range w.texts {
			value := text.value
			if len(value) > maxReferenceText {
				value = value[:maxReferenceText]
			}
			for _, token := range tokenPattern.FindAllString(strings.ToLower(value), -1) {
				svcKey := resolveService(token, w.Namespace, services)
				if svcKey == "" {
					continue
				}
				for _, target := range targets[svcKey] {
					addEdge(wkey, target, text.via)
				}
			}
		}
	}

	// Ingress: entry points.
	for _, namespace := range namespaces {
		for _, ing := range data[namespace].ingresses {
			backends := map[string][]string{} // service -> hosts
			if ing.Spec.DefaultBackend != nil && ing.Spec.DefaultBackend.Service != nil {
				backends[ing.Spec.DefaultBackend.Service.Name] = append(backends[ing.Spec.DefaultBackend.Service.Name], "*")
			}
			for _, rule := range ing.Spec.Rules {
				if rule.HTTP == nil {
					continue
				}
				host := rule.Host
				if host == "" {
					host = "*"
				}
				for _, p := range rule.HTTP.Paths {
					if p.Backend.Service != nil {
						backends[p.Backend.Service.Name] = append(backends[p.Backend.Service.Name], host)
					}
				}
			}
			for service, hosts := range backends {
				for _, target := range targets[namespace+"/"+service] {
					byKey[target].entry(hosts)
				}
			}
		}
	}

	// NetworkPolicy peers: clients depend on the pods they are allowed to reach, and a policy's own pods
	// depend on the egress peers.
	for _, namespace := range namespaces {
		for _, policy := range data[namespace].policies {
			selected := matchWorkloads(byKey, order, namespace, &policy.Spec.PodSelector)
			if len(selected) == 0 {
				continue
			}
			via := "networkpolicy " + policy.Name
			for _, rule := range policy.Spec.Ingress {
				for _, peer := range rule.From {
					if peer.PodSelector == nil || peer.NamespaceSelector != nil {
						continue
					}
					for _, client := range matchWorkloads(byKey, order, namespace, peer.PodSelector) {
						for _, target := range selected {
							addEdge(client, target, via)
						}
					}
				}
			}
			for _, rule := range policy.Spec.Egress {
				for _, peer := range rule.To {
					if peer.PodSelector == nil || peer.NamespaceSelector != nil {
						continue
					}
					for _, target := range matchWorkloads(byKey, order, namespace, peer.PodSelector) {
						for _, from := range selected {
							addEdge(from, target, via)
						}
					}
				}
			}
		}
	}

	// Materialise nodes and edges deterministically.
	sort.Strings(order)
	index := map[string]int{}
	for _, key := range order {
		w := byKey[key]
		hosts := append([]string(nil), w.hosts()...)
		sort.Strings(hosts)
		index[key] = len(graph.Nodes)
		graph.Nodes = append(graph.Nodes, DepNode{Key: key, Kind: w.Kind, Name: w.Name, Namespace: w.Namespace,
			Stateful: w.stateful, EntryPoint: len(hosts) > 0, Hosts: dedupe(hosts)})
	}
	pairs := make([][2]string, 0, len(edges))
	for pair := range edges {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	for _, pair := range pairs {
		graph.Edges = append(graph.Edges, DepEdge{From: pair[0], To: pair[1], Via: strings.Join(edges[pair], "; ")})
	}

	markCycles(graph, index)
	assignTiers(graph, index)
	return graph, nil
}

// entry records that an Ingress reaches the workload.
func (w *depWorkload) entry(hosts []string) {
	w.ingressHosts = append(w.ingressHosts, hosts...)
}

func (w *depWorkload) hosts() []string { return w.ingressHosts }

// collectTexts gathers every string of a pod spec that may name a Service.
func collectTexts(spec *corev1.PodSpec, configMaps map[string]map[string]string) []referenceText {
	var out []referenceText
	containers := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
	for _, c := range containers {
		for _, e := range c.Env {
			switch {
			case e.Value != "":
				out = append(out, referenceText{e.Value, "env " + e.Name})
			case e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil:
				ref := e.ValueFrom.ConfigMapKeyRef
				if value, ok := configMaps[ref.Name][ref.Key]; ok {
					out = append(out, referenceText{value, "env " + e.Name + " (configmap " + ref.Name + "/" + ref.Key + ")"})
				}
			}
		}
		for _, from := range c.EnvFrom {
			if from.ConfigMapRef == nil {
				continue
			}
			keys := make([]string, 0, len(configMaps[from.ConfigMapRef.Name]))
			for key := range configMaps[from.ConfigMapRef.Name] {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				out = append(out, referenceText{configMaps[from.ConfigMapRef.Name][key], "configmap " + from.ConfigMapRef.Name + "/" + key})
			}
		}
		for _, arg := range append(append([]string{}, c.Command...), c.Args...) {
			out = append(out, referenceText{arg, "args of " + c.Name})
		}
	}
	return out
}

// resolveService maps a token to a known Service: "name" (same namespace), "name.namespace",
// "name.namespace.svc" and "name.namespace.svc.<cluster domain>".
func resolveService(token, namespace string, services map[string]*corev1.Service) string {
	token = strings.Trim(token, ".-")
	parts := strings.Split(token, ".")
	var key string
	switch {
	case len(parts) == 1:
		key = namespace + "/" + parts[0]
	case len(parts) == 2, len(parts) >= 3 && parts[2] == "svc":
		key = parts[1] + "/" + parts[0]
	default:
		return ""
	}
	if _, ok := services[key]; ok {
		return key
	}
	return ""
}

func matchWorkloads(byKey map[string]*depWorkload, order []string, namespace string, selector *metav1.LabelSelector) []string {
	if selector == nil {
		return nil
	}
	parsed, err := metav1.LabelSelectorAsSelector(selector)
	// An empty selector matches every pod: too noisy to mean a dependency.
	if err != nil || parsed.Empty() {
		return nil
	}
	var out []string
	for _, key := range order {
		w := byKey[key]
		if w.Namespace == namespace && parsed.Matches(labels.Set(w.Template.Labels)) {
			out = append(out, key)
		}
	}
	return out
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// markCycles flags the edges that close a cycle (found by a depth-first search in sorted order) and
// records each cycle as a path of node keys.
func markCycles(g *Graph, index map[string]int) {
	adjacency := make([][]int, len(g.Nodes))
	edgeAt := map[[2]int]int{}
	for i, e := range g.Edges {
		from, to := index[e.From], index[e.To]
		adjacency[from] = append(adjacency[from], to)
		edgeAt[[2]int{from, to}] = i
	}

	const (
		white = iota
		grey
		black
	)
	color := make([]int, len(g.Nodes))
	var stack []int
	var visit func(n int)
	visit = func(n int) {
		color[n] = grey
		stack = append(stack, n)
		for _, next := range adjacency[n] {
			switch color[next] {
			case white:
				visit(next)
			case grey:
				g.Edges[edgeAt[[2]int{n, next}]].Cycle = true
				var cycle []string
				for i := len(stack) - 1; i >= 0; i-- {
					cycle = append([]string{g.Nodes[stack[i]].Key}, cycle...)
					if stack[i] == next {
						break
					}
				}
				g.Cycles = append(g.Cycles, append(cycle, g.Nodes[next].Key))
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
	}
	for n := range g.Nodes {
		if color[n] == white {
			visit(n)
		}
	}
}

// assignTiers sets Tier: dependencies start before their dependents (longest path layering over the
// edges that are not cycle-closing). A node without any edge starts at tier 0 when it is stateful and at
// tier 1 otherwise, so data stores come up before applications whose dependency could not be detected.
func assignTiers(g *Graph, index map[string]int) {
	deps := make([][]int, len(g.Nodes))
	connected := make([]bool, len(g.Nodes))
	for _, e := range g.Edges {
		from, to := index[e.From], index[e.To]
		connected[from], connected[to] = true, true
		if !e.Cycle {
			deps[from] = append(deps[from], to)
		}
	}
	tier := make([]int, len(g.Nodes))
	done := make([]bool, len(g.Nodes))
	var compute func(n int) int
	compute = func(n int) int {
		if done[n] {
			return tier[n]
		}
		done[n] = true // the graph is acyclic here, so this only guards against surprises
		best := 0
		for _, d := range deps[n] {
			if t := compute(d) + 1; t > best {
				best = t
			}
		}
		tier[n] = best
		return best
	}
	for n := range g.Nodes {
		t := compute(n)
		if !connected[n] && !g.Nodes[n].Stateful {
			t = 1
		}
		g.Nodes[n].Tier = t
	}
	if len(g.Cycles) > 0 {
		g.Notices = append(g.Notices, fmt.Sprintf("%d dependency cycle(s) found; the edge that closes each one was ignored for the order. Review them.", len(g.Cycles)))
	}
}

// DraftApps turns the graph into plan apps, one per workload: tier from the graph, dependsOn from the
// edges. Names are the workload names, or namespace-name when two namespaces share a name.
func (g *Graph) DraftApps() []dr.App {
	count := map[string]int{}
	for _, n := range g.Nodes {
		count[n.Name]++
	}
	names := map[string]string{}
	for _, n := range g.Nodes {
		name := n.Name
		if count[name] > 1 {
			name = n.Namespace + "-" + n.Name
		}
		names[n.Key] = strings.ToLower(name)
	}

	dependsOn := map[string][]string{}
	for _, e := range g.Edges {
		if !e.Cycle {
			dependsOn[e.From] = append(dependsOn[e.From], names[e.To])
		}
	}

	nodes := append([]DepNode(nil), g.Nodes...)
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Tier != nodes[j].Tier {
			return nodes[i].Tier < nodes[j].Tier
		}
		return nodes[i].Key < nodes[j].Key
	})
	apps := make([]dr.App, 0, len(nodes))
	for _, n := range nodes {
		deps := dependsOn[n.Key]
		sort.Strings(deps)
		apps = append(apps, dr.App{Name: names[n.Key], Namespace: n.Namespace, Workloads: []string{n.Name}, Tier: n.Tier, DependsOn: deps})
	}
	return apps
}

// DraftYAML renders DraftApps as the `apps:` section of k8scli-dr.yaml, with a header that says it is a
// draft and lists the stateful workloads (which usually belong under `databases:`).
func (g *Graph) DraftYAML() ([]byte, error) {
	body, err := yaml.Marshal(struct {
		Apps []dr.App `json:"apps"`
	}{Apps: g.DraftApps()})
	if err != nil {
		return nil, err
	}
	var header strings.Builder
	header.WriteString("# Draft generated from the cluster by dependency discovery. It is a hint, not a fact:\n")
	header.WriteString("# review every tier and dependsOn before you use it.\n")
	var stateful []string
	for _, n := range g.Nodes {
		if n.Stateful {
			stateful = append(stateful, n.Namespace+"/"+n.Name)
		}
	}
	if len(stateful) > 0 {
		header.WriteString("# Stateful workloads (move databases to the `databases:` section): " + strings.Join(stateful, ", ") + "\n")
	}
	return append([]byte(header.String()), body...), nil
}
