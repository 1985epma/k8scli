package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func warningEvent(namespace, name, object, reason string, ago time.Duration) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:           corev1.EventTypeWarning,
		Reason:         reason,
		Message:        reason + " happened",
		Count:          3,
		LastTimestamp:  metav1.NewTime(fixedNow.Add(-ago)),
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: object, Namespace: namespace},
	}
}

func podWithStatus(namespace, name string, phase corev1.PodPhase, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: metav1.NewTime(fixedNow.Add(-time.Hour))},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{
			Phase: phase,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Ready: phase == corev1.PodRunning, RestartCount: restarts,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

func readyNode(name string, ready bool) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}},
	}
}

func useMetrics(t *testing.T, fn func(context.Context, kubernetes.Interface) ([]podUsage, error)) {
	t.Helper()
	previous := fetchPodMetrics
	fetchPodMetrics = fn
	t.Cleanup(func() { fetchPodMetrics = previous })
}

func TestDashboardSummarisesTheCluster(t *testing.T) {
	notReady := deployment("shop", "web", 3, 1)
	e := newEnv(t, Options{},
		readyNode("n1", true), readyNode("n2", false),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tools"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "quiet"}},
		crashingPod("shop", "web-1"),
		podWithStatus("shop", "web-2", corev1.PodRunning, 0),
		podWithStatus("shop", "web-3", corev1.PodPending, 0),
		podWithStatus("tools", "ci-1", corev1.PodRunning, 7),
		podWithStatus("tools", "job-1", corev1.PodSucceeded, 0),
		notReady, deployment("tools", "ci", 1, 1),
		warningEvent("shop", "e1", "web-1", "BackOff", 5*time.Minute),
		warningEvent("shop", "e2", "web-3", "FailedScheduling", 1*time.Minute),
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "normal", Namespace: "shop"}, Type: corev1.EventTypeNormal, Reason: "Started"},
	)

	rec := e.do(http.MethodGet, "/api/dashboard?context=dev", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	d := decode[dashboardResponse](t, rec)

	if d.Context != "dev" || d.Nodes.Ready != 1 || d.Nodes.Total != 2 {
		t.Errorf("context/nodes = %q %+v", d.Context, d.Nodes)
	}
	want := dashTotals{Namespaces: 3, Pods: 5, Running: 2, Attention: 2, Restarts: 10, Deployments: 2, DeploymentsNotReady: 1}
	if d.Totals != want {
		t.Errorf("totals = %+v; want %+v", d.Totals, want)
	}

	// attention desc, then name; the quiet namespace is still listed
	names := []string{}
	for _, row := range d.Namespaces {
		names = append(names, fmt.Sprintf("%s:%d/%d/%d", row.Name, row.Pods, row.Attention, row.Deployments))
	}
	if strings.Join(names, " ") != "shop:3/2/1 quiet:0/0/0 tools:2/0/1" {
		t.Errorf("namespaces = %v", names)
	}

	if len(d.AttentionPods) != 2 || d.AttentionPods[0].Name != "web-1" || d.AttentionPods[0].Health != "error" || d.AttentionPods[1].Name != "web-3" {
		t.Errorf("attention (errors first) = %+v", d.AttentionPods)
	}
	if len(d.TopRestarts) != 2 || d.TopRestarts[0].Name != "ci-1" || d.TopRestarts[0].Restarts != 7 || d.TopRestarts[1].Name != "web-1" {
		t.Errorf("top restarts = %+v", d.TopRestarts)
	}

	if len(d.Warnings) != 2 || d.Warnings[0].Reason != "FailedScheduling" || d.Warnings[0].Age != "60s" ||
		d.Warnings[0].Object != "Pod/web-3" || d.Warnings[0].Count != 3 || d.Warnings[1].Reason != "BackOff" {
		t.Errorf("warnings (newest first, no Normal events) = %+v", d.Warnings)
	}

	if d.Metrics.Available || len(d.Metrics.TopCPU) != 0 || len(d.Notices) != 0 {
		t.Errorf("without metrics-server there must be no error: metrics=%+v notices=%v", d.Metrics, d.Notices)
	}
}

func TestDashboardEmptyClusterHasEmptyListsNotNull(t *testing.T) {
	e := newEnv(t, Options{})
	body := e.do(http.MethodGet, "/api/dashboard", nil).Body.String()
	for _, field := range []string{"namespaces", "attentionPods", "topRestarts", "warnings", "notices", "topCPU", "topMemory"} {
		if !strings.Contains(body, `"`+field+`":[]`) {
			t.Errorf("%s should be [] in %s", field, body)
		}
	}
}

func TestDashboardMetrics(t *testing.T) {
	useMetrics(t, func(context.Context, kubernetes.Interface) ([]podUsage, error) {
		return []podUsage{
			{namespace: "a", name: "idle"},
			{namespace: "a", name: "cpu-hog", cpuMilli: 1500, memoryBytes: 64 << 20},
			{namespace: "b", name: "mem-hog", cpuMilli: 10, memoryBytes: 2048 << 20},
			{namespace: "b", name: "mid", cpuMilli: 200, memoryBytes: 128 << 20},
		}, nil
	})
	e := newEnv(t, Options{})

	d := decode[dashboardResponse](t, e.do(http.MethodGet, "/api/dashboard", nil))
	if !d.Metrics.Available {
		t.Fatal("metrics should be available")
	}
	if len(d.Metrics.TopCPU) != 3 || d.Metrics.TopCPU[0].Name != "cpu-hog" || d.Metrics.TopCPU[0].CPUMilli != 1500 || d.Metrics.TopCPU[1].Name != "mid" {
		t.Errorf("top CPU = %+v (idle pods are skipped)", d.Metrics.TopCPU)
	}
	if len(d.Metrics.TopMemory) != 3 || d.Metrics.TopMemory[0].Name != "mem-hog" || d.Metrics.TopMemory[0].MemoryMiB != 2048 {
		t.Errorf("top memory = %+v", d.Metrics.TopMemory)
	}
}

func TestDashboardMetricsFailureIsOnlyANotice(t *testing.T) {
	useMetrics(t, func(context.Context, kubernetes.Interface) ([]podUsage, error) {
		return nil, errors.New("metrics-server is forbidden")
	})
	e := newEnv(t, Options{}, crashingPod("shop", "web-1"))

	rec := e.do(http.MethodGet, "/api/dashboard", nil)
	d := decode[dashboardResponse](t, rec)
	if rec.Code != http.StatusOK || d.Metrics.Available || d.Totals.Pods != 1 {
		t.Errorf("status %d, metrics %+v, pods %d", rec.Code, d.Metrics, d.Totals.Pods)
	}
	if len(d.Notices) != 1 || !strings.Contains(d.Notices[0], "Could not read metrics") {
		t.Errorf("notices = %v", d.Notices)
	}
}

func TestDashboardKeepsWorkingWhenPartsFail(t *testing.T) {
	e := newEnv(t, Options{}, crashingPod("shop", "web-1"), deployment("shop", "web", 1, 1))
	e.client.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("events is forbidden")
	})
	e.client.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("deployments is forbidden")
	})

	rec := e.do(http.MethodGet, "/api/dashboard", nil)
	d := decode[dashboardResponse](t, rec)
	if rec.Code != http.StatusOK || d.Totals.Pods != 1 || len(d.AttentionPods) != 1 {
		t.Fatalf("the pods must still be reported: %d %+v", rec.Code, d.Totals)
	}
	joined := strings.Join(d.Notices, "|")
	if !strings.Contains(joined, "Could not list events") || !strings.Contains(joined, "Could not list deployments") {
		t.Errorf("notices = %v", d.Notices)
	}

	e2 := newEnv(t, Options{}, deployment("shop", "web", 1, 1))
	e2.client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pods is forbidden")
	})
	d2 := decode[dashboardResponse](t, e2.do(http.MethodGet, "/api/dashboard", nil))
	if d2.Totals.Deployments != 1 || len(d2.Notices) != 1 || !strings.Contains(d2.Notices[0], "Could not list pods") {
		t.Errorf("deployments must still be reported: %+v %v", d2.Totals, d2.Notices)
	}
}

func TestDashboardRowsAreBounded(t *testing.T) {
	var objects []runtime.Object
	for i := 0; i < 15; i++ {
		objects = append(objects, podWithStatus("shop", fmt.Sprintf("pending-%02d", i), corev1.PodPending, int32(i+1)))
		objects = append(objects, warningEvent("shop", fmt.Sprintf("ev-%02d", i), "x", "BackOff", time.Duration(i+1)*time.Minute))
	}
	d := decode[dashboardResponse](t, newEnv(t, Options{}, objects...).do(http.MethodGet, "/api/dashboard", nil))

	if len(d.AttentionPods) != dashAttentionRows || len(d.TopRestarts) != dashRestartRows || len(d.Warnings) != dashWarningRows {
		t.Errorf("rows = %d attention, %d restarts, %d warnings", len(d.AttentionPods), len(d.TopRestarts), len(d.Warnings))
	}
	if d.AttentionPods[0].Restarts != 15 || d.TopRestarts[0].Restarts != 15 || d.Warnings[0].Age != "60s" {
		t.Errorf("not the worst/newest: %+v %+v %+v", d.AttentionPods[0], d.TopRestarts[0], d.Warnings[0])
	}
	if d.Totals.Attention != 15 {
		t.Errorf("totals must count every pod, not only the listed ones: %d", d.Totals.Attention)
	}
}

func TestDashboardTruncatesLongEventMessages(t *testing.T) {
	event := warningEvent("shop", "e", "web-1", "BackOff", time.Minute)
	event.Message = strings.Repeat("é", 1000)
	d := decode[dashboardResponse](t, newEnv(t, Options{}, event).do(http.MethodGet, "/api/dashboard", nil))
	if got := []rune(d.Warnings[0].Message); len(got) != dashMessageLimit+1 || got[len(got)-1] != '…' {
		t.Errorf("message has %d runes", len(got))
	}
}

func TestDashboardFallsBackToOtherEventTimes(t *testing.T) {
	onlyEventTime := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "x"}, Type: corev1.EventTypeWarning, Reason: "A",
		EventTime: metav1.NewMicroTime(fixedNow.Add(-10 * time.Minute)),
	}
	onlyCreated := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "x", CreationTimestamp: metav1.NewTime(fixedNow.Add(-20 * time.Minute))},
		Type:       corev1.EventTypeWarning, Reason: "B",
	}
	d := decode[dashboardResponse](t, newEnv(t, Options{}, onlyCreated, onlyEventTime).do(http.MethodGet, "/api/dashboard", nil))
	if len(d.Warnings) != 2 || d.Warnings[0].Reason != "A" || d.Warnings[0].Age != "10m" || d.Warnings[1].Age != "20m" {
		t.Errorf("warnings = %+v", d.Warnings)
	}
}

func TestDashboardUnknownContext(t *testing.T) {
	if rec := newEnv(t, Options{}).do(http.MethodGet, "/api/dashboard?context=nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestDashboardDeploymentWithoutReplicasCountsAsOne(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "x"}}
	if !deploymentNotReady(d) {
		t.Error("spec.replicas unset means 1 desired, 0 ready")
	}
	d.Status.ReadyReplicas = 1
	if deploymentNotReady(d) {
		t.Error("1 of 1 is ready")
	}
}

func TestDefaultFetchPodMetricsWithoutARESTClient(t *testing.T) {
	e := newEnv(t, Options{})
	if _, err := defaultFetchPodMetrics(context.Background(), e.client); !errors.Is(err, errMetricsUnavailable) {
		t.Errorf("err = %v; a fake client has no metrics API", err)
	}
}

// metricsClient builds a real clientset against a stub API server.
func metricsClient(t *testing.T, handler http.HandlerFunc) kubernetes.Interface {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestDefaultFetchPodMetricsParsesMetricsServer(t *testing.T) {
	client := metricsClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/metrics.k8s.io/v1beta1/pods" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"api-1","namespace":"shop"},"containers":[
				{"usage":{"cpu":"250m","memory":"128Mi"}},{"usage":{"cpu":"1","memory":"64Mi"}}]},
			{"metadata":{"name":"odd","namespace":"shop"},"containers":[{"usage":{"cpu":"bogus","memory":""}}]}]}`))
	})

	usage, err := defaultFetchPodMetrics(context.Background(), client)
	if err != nil || len(usage) != 2 {
		t.Fatalf("usage = %+v, err = %v", usage, err)
	}
	if usage[0].name != "api-1" || usage[0].cpuMilli != 1250 || usage[0].memoryBytes != 192<<20 {
		t.Errorf("containers must be summed: %+v", usage[0])
	}
	if usage[1].cpuMilli != 0 || usage[1].memoryBytes != 0 {
		t.Errorf("unparsable quantities count as zero: %+v", usage[1])
	}
}

func TestDefaultFetchPodMetricsWhenMetricsServerIsMissing(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusServiceUnavailable} {
		client := metricsClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"x","code":` + fmt.Sprint(status) + `}`))
		})
		if _, err := defaultFetchPodMetrics(context.Background(), client); !errors.Is(err, errMetricsUnavailable) {
			t.Errorf("HTTP %d: err = %v; want errMetricsUnavailable", status, err)
		}
	}

	garbage := metricsClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) })
	if _, err := defaultFetchPodMetrics(context.Background(), garbage); err == nil || errors.Is(err, errMetricsUnavailable) {
		t.Errorf("a garbled answer is a real error, got %v", err)
	}
}

func TestPushHelpersKeepTheBestRows(t *testing.T) {
	var attention []podJSON
	for i := 0; i < 20; i++ {
		health := healthWarn
		if i == 19 {
			health = healthError
		}
		attention = pushAttention(attention, podJSON{Namespace: "x", Name: fmt.Sprintf("p%02d", i), Health: health, Restarts: int32(i)})
	}
	if len(attention) != dashAttentionRows || attention[0].Name != "p19" || attention[1].Name != "p18" {
		t.Errorf("attention = %+v", attention)
	}
}
