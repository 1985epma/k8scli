package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Bounds that keep the dashboard cheap on big clusters. It makes one pass over
// the data and only keeps counters and a handful of rows in memory.
const (
	dashMaxPods        = 20000
	dashMaxDeployments = 5000
	dashMaxEvents      = 5000
	dashAttentionRows  = 10
	dashRestartRows    = 5
	dashWarningRows    = 10
	dashMetricRows     = 5
	dashMessageLimit   = 300
	dashMetricsLimit   = 32 << 20 // bytes read from metrics-server
	bytesPerMiB        = 1 << 20
)

// errMetricsUnavailable means metrics-server is not installed or not reachable.
// It is a normal situation, not a failure.
var errMetricsUnavailable = errors.New("metrics are not available")

// podUsage is one pod's current resource usage, summed over its containers.
type podUsage struct {
	namespace   string
	name        string
	cpuMilli    int64
	memoryBytes int64
}

// fetchPodMetrics reads pod usage from metrics-server. Tests replace it, because
// the fake clientset has no REST client to talk to.
var fetchPodMetrics = defaultFetchPodMetrics

type dashNodes struct {
	Ready int `json:"ready"`
	Total int `json:"total"`
}

type dashTotals struct {
	Namespaces          int   `json:"namespaces"`
	Pods                int   `json:"pods"`
	Running             int   `json:"running"`
	Attention           int   `json:"attention"`
	Restarts            int64 `json:"restarts"`
	Deployments         int   `json:"deployments"`
	DeploymentsNotReady int   `json:"deploymentsNotReady"`
}

type dashNamespace struct {
	Name        string `json:"name"`
	Pods        int    `json:"pods"`
	Running     int    `json:"running"`
	Attention   int    `json:"attention"`
	Restarts    int64  `json:"restarts"`
	Deployments int    `json:"deployments"`
}

type dashWarning struct {
	Age       string `json:"age"`
	Namespace string `json:"namespace"`
	Object    string `json:"object"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
	Count     int32  `json:"count"`
}

type dashUsage struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	CPUMilli  int64  `json:"cpuMilli"`
	MemoryMiB int64  `json:"memoryMiB"`
}

type dashMetrics struct {
	Available bool        `json:"available"`
	TopCPU    []dashUsage `json:"topCPU"`
	TopMemory []dashUsage `json:"topMemory"`
}

type dashboardResponse struct {
	Context       string          `json:"context"`
	Version       string          `json:"version"`
	Platform      string          `json:"platform"`
	Nodes         dashNodes       `json:"nodes"`
	Totals        dashTotals      `json:"totals"`
	Namespaces    []dashNamespace `json:"namespaces"`
	AttentionPods []podJSON       `json:"attentionPods"`
	TopRestarts   []podJSON       `json:"topRestarts"`
	Warnings      []dashWarning   `json:"warnings"`
	Metrics       dashMetrics     `json:"metrics"`
	Notices       []string        `json:"notices"`
}

// podAggregate is everything the dashboard and the namespaces page need from
// the pods, collected in one pass.
type podAggregate struct {
	byNamespace map[string]*dashNamespace
	total       dashTotals
	attention   []podJSON
	restarts    []podJSON
	truncated   bool
}

func (a *podAggregate) namespace(name string) *dashNamespace {
	row, ok := a.byNamespace[name]
	if !ok {
		row = &dashNamespace{Name: name}
		a.byNamespace[name] = row
	}
	return row
}

// healthRank orders attention rows: errors before warnings.
func healthRank(health string) int {
	if health == healthError {
		return 0
	}
	return 1
}

// pushAttention keeps the dashAttentionRows worst pods: errors first, then the
// most restarts, then by name so the answer is stable.
func pushAttention(rows []podJSON, pod podJSON) []podJSON {
	rows = append(rows, pod)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if healthRank(a.Health) != healthRank(b.Health) {
			return healthRank(a.Health) < healthRank(b.Health)
		}
		if a.Restarts != b.Restarts {
			return a.Restarts > b.Restarts
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	if len(rows) > dashAttentionRows {
		rows = rows[:dashAttentionRows]
	}
	return rows
}

func pushRestarts(rows []podJSON, pod podJSON) []podJSON {
	rows = append(rows, pod)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Restarts != b.Restarts {
			return a.Restarts > b.Restarts
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	if len(rows) > dashRestartRows {
		rows = rows[:dashRestartRows]
	}
	return rows
}

// aggregatePods pages through every pod once. It never holds more than one page
// plus the counters and the few rows it reports.
func (s *Server) aggregatePods(ctx context.Context, client kubernetes.Interface) (*podAggregate, error) {
	agg := &podAggregate{byNamespace: map[string]*dashNamespace{}, attention: []podJSON{}, restarts: []podJSON{}}
	options := metav1.ListOptions{Limit: listPageSize}

	for {
		list, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, options)
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			if agg.total.Pods >= dashMaxPods {
				agg.truncated = true
				return agg, nil
			}
			pod := s.toPodJSON(&list.Items[i])
			row := agg.namespace(pod.Namespace)

			agg.total.Pods++
			row.Pods++
			agg.total.Restarts += int64(pod.Restarts)
			row.Restarts += int64(pod.Restarts)
			if pod.Status == "Running" {
				agg.total.Running++
				row.Running++
			}
			if pod.Health == healthWarn || pod.Health == healthError {
				agg.total.Attention++
				row.Attention++
				agg.attention = pushAttention(agg.attention, pod)
			}
			if pod.Restarts > 0 {
				agg.restarts = pushRestarts(agg.restarts, pod)
			}
		}
		if list.Continue == "" {
			return agg, nil
		}
		options.Continue = list.Continue
	}
}

// deploymentAggregate counts deployments per namespace.
type deploymentAggregate struct {
	perNamespace map[string]int
	total        int
	notReady     int
	truncated    bool
}

func (s *Server) aggregateDeployments(ctx context.Context, client kubernetes.Interface) (*deploymentAggregate, error) {
	agg := &deploymentAggregate{perNamespace: map[string]int{}}
	options := metav1.ListOptions{Limit: listPageSize}

	for {
		list, err := client.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, options)
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			if agg.total >= dashMaxDeployments {
				agg.truncated = true
				return agg, nil
			}
			d := &list.Items[i]
			agg.total++
			agg.perNamespace[d.Namespace]++
			if deploymentNotReady(d) {
				agg.notReady++
			}
		}
		if list.Continue == "" {
			return agg, nil
		}
		options.Continue = list.Continue
	}
}

func deploymentNotReady(d *appsv1.Deployment) bool {
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	return d.Status.ReadyReplicas < desired
}

// eventTime is the best timestamp an event has: events come from two APIs and
// fill different fields.
func eventTime(ev *corev1.Event) time.Time {
	switch {
	case !ev.LastTimestamp.IsZero():
		return ev.LastTimestamp.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	default:
		return ev.CreationTimestamp.Time
	}
}

func dashTruncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// recentWarnings returns the newest Warning events. The API does not sort
// events, so it pages through them and keeps only the newest rows.
func (s *Server) recentWarnings(ctx context.Context, client kubernetes.Interface) ([]dashWarning, error) {
	type stamped struct {
		at      time.Time
		warning dashWarning
	}
	var newest []stamped

	options := metav1.ListOptions{FieldSelector: "type=Warning", Limit: listPageSize}
	seen := 0
	for {
		list, err := client.CoreV1().Events(metav1.NamespaceAll).List(ctx, options)
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			ev := &list.Items[i]
			if seen++; seen > dashMaxEvents {
				break
			}
			// The fake clientset ignores field selectors, and so might an odd API server.
			if ev.Type != corev1.EventTypeWarning {
				continue
			}
			at := eventTime(ev)
			newest = append(newest, stamped{at: at, warning: dashWarning{
				Age:       duration.HumanDuration(s.now().Sub(at)),
				Namespace: ev.Namespace,
				Object:    ev.InvolvedObject.Kind + "/" + ev.InvolvedObject.Name,
				Reason:    ev.Reason,
				Message:   dashTruncate(ev.Message, dashMessageLimit),
				Count:     ev.Count,
			}})
			sort.SliceStable(newest, func(a, b int) bool { return newest[a].at.After(newest[b].at) })
			if len(newest) > dashWarningRows {
				newest = newest[:dashWarningRows]
			}
		}
		if list.Continue == "" || seen > dashMaxEvents {
			break
		}
		options.Continue = list.Continue
	}

	warnings := make([]dashWarning, 0, len(newest))
	for _, item := range newest {
		warnings = append(warnings, item.warning)
	}
	return warnings, nil
}

// defaultFetchPodMetrics asks metrics-server for every pod's usage.
func defaultFetchPodMetrics(ctx context.Context, client kubernetes.Interface) ([]podUsage, error) {
	rc, ok := client.Discovery().RESTClient().(*rest.RESTClient)
	if !ok || rc == nil {
		return nil, errMetricsUnavailable
	}

	stream, err := rc.Get().AbsPath("/apis/metrics.k8s.io/v1beta1/pods").Stream(ctx)
	if err != nil {
		// 404 or 503 mean metrics-server is not installed (or not ready).
		if apierrors.IsNotFound(err) || apierrors.IsServiceUnavailable(err) {
			return nil, errMetricsUnavailable
		}
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var body struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Containers []struct {
				Usage struct {
					CPU    string `json:"cpu"`
					Memory string `json:"memory"`
				} `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(stream, dashMetricsLimit)).Decode(&body); err != nil {
		return nil, fmt.Errorf("unexpected metrics response: %w", err)
	}

	usage := make([]podUsage, 0, len(body.Items))
	for _, item := range body.Items {
		pod := podUsage{namespace: item.Metadata.Namespace, name: item.Metadata.Name}
		for _, container := range item.Containers {
			if q, err := resource.ParseQuantity(container.Usage.CPU); err == nil {
				pod.cpuMilli += q.MilliValue()
			}
			if q, err := resource.ParseQuantity(container.Usage.Memory); err == nil {
				pod.memoryBytes += q.Value()
			}
		}
		usage = append(usage, pod)
	}
	return usage, nil
}

// topUsage returns the n pods with the highest value of key, skipping zeros.
func topUsage(usage []podUsage, n int, key func(podUsage) int64) []dashUsage {
	sorted := append([]podUsage(nil), usage...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if key(sorted[i]) != key(sorted[j]) {
			return key(sorted[i]) > key(sorted[j])
		}
		if sorted[i].namespace != sorted[j].namespace {
			return sorted[i].namespace < sorted[j].namespace
		}
		return sorted[i].name < sorted[j].name
	})

	rows := []dashUsage{}
	for _, pod := range sorted {
		if len(rows) == n || key(pod) == 0 {
			break
		}
		rows = append(rows, dashUsage{Namespace: pod.namespace, Name: pod.name, CPUMilli: pod.cpuMilli, MemoryMiB: pod.memoryBytes / bytesPerMiB})
	}
	return rows
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	contextName, client, err := s.cluster(r.URL.Query().Get("context"), 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	defer cancel()

	overview, err := s.overview(ctx, contextName, client)
	if err != nil {
		writeErr(w, err)
		return
	}

	// The four reads are independent; run them together and keep what works.
	var (
		wg       sync.WaitGroup
		pods     *podAggregate
		podsErr  error
		deps     *deploymentAggregate
		depsErr  error
		warnings []dashWarning
		warnErr  error
		usage    []podUsage
		usageErr error
	)
	wg.Add(4)
	go func() { defer wg.Done(); pods, podsErr = s.aggregatePods(ctx, client) }()
	go func() { defer wg.Done(); deps, depsErr = s.aggregateDeployments(ctx, client) }()
	go func() { defer wg.Done(); warnings, warnErr = s.recentWarnings(ctx, client) }()
	go func() { defer wg.Done(); usage, usageErr = fetchPodMetrics(ctx, client) }()
	wg.Wait()

	response := dashboardResponse{
		Context:       contextName,
		Version:       overview.Version,
		Platform:      overview.Platform,
		Namespaces:    []dashNamespace{},
		AttentionPods: []podJSON{},
		TopRestarts:   []podJSON{},
		Warnings:      []dashWarning{},
		Metrics:       dashMetrics{TopCPU: []dashUsage{}, TopMemory: []dashUsage{}},
		Notices:       append([]string{}, overview.Warnings...),
	}
	for _, node := range overview.Nodes {
		response.Nodes.Total++
		if node.Ready {
			response.Nodes.Ready++
		}
	}

	rows := map[string]*dashNamespace{}
	for _, name := range overview.Namespaces {
		rows[name] = &dashNamespace{Name: name}
	}

	if podsErr != nil {
		response.Notices = append(response.Notices, "Could not list pods: "+podsErr.Error())
	} else {
		response.Totals.Pods = pods.total.Pods
		response.Totals.Running = pods.total.Running
		response.Totals.Attention = pods.total.Attention
		response.Totals.Restarts = pods.total.Restarts
		response.AttentionPods = pods.attention
		response.TopRestarts = pods.restarts
		for name, row := range pods.byNamespace {
			if existing, ok := rows[name]; ok {
				existing.Pods, existing.Running, existing.Attention, existing.Restarts = row.Pods, row.Running, row.Attention, row.Restarts
			} else {
				rows[name] = row
			}
		}
		if pods.truncated {
			response.Notices = append(response.Notices, fmt.Sprintf("Counted the first %d pods only.", dashMaxPods))
		}
	}

	if depsErr != nil {
		response.Notices = append(response.Notices, "Could not list deployments: "+depsErr.Error())
	} else {
		response.Totals.Deployments = deps.total
		response.Totals.DeploymentsNotReady = deps.notReady
		for name, count := range deps.perNamespace {
			if _, ok := rows[name]; !ok {
				rows[name] = &dashNamespace{Name: name}
			}
			rows[name].Deployments = count
		}
		if deps.truncated {
			response.Notices = append(response.Notices, fmt.Sprintf("Counted the first %d deployments only.", dashMaxDeployments))
		}
	}

	if warnErr != nil {
		response.Notices = append(response.Notices, "Could not list events: "+warnErr.Error())
	} else {
		response.Warnings = warnings
	}

	switch {
	case usageErr == nil:
		response.Metrics = dashMetrics{
			Available: true,
			TopCPU:    topUsage(usage, dashMetricRows, func(p podUsage) int64 { return p.cpuMilli }),
			TopMemory: topUsage(usage, dashMetricRows, func(p podUsage) int64 { return p.memoryBytes }),
		}
	case errors.Is(usageErr, errMetricsUnavailable):
		// Not installed: the page shows a hint instead of an error.
	default:
		response.Notices = append(response.Notices, "Could not read metrics: "+usageErr.Error())
	}

	for _, row := range rows {
		response.Namespaces = append(response.Namespaces, *row)
	}
	sort.Slice(response.Namespaces, func(i, j int) bool {
		a, b := response.Namespaces[i], response.Namespaces[j]
		if a.Attention != b.Attention {
			return a.Attention > b.Attention
		}
		return a.Name < b.Name
	})
	response.Totals.Namespaces = len(response.Namespaces)

	writeJSON(w, http.StatusOK, response)
}
