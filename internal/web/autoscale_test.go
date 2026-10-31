package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func hpaFor(name, deployment string, min, max, cpu int32) *autoscalingv2.HorizontalPodAutoscaler {
	metrics := []autoscalingv2.MetricSpec{}
	if cpu > 0 {
		metrics = append(metrics, resourceMetric(corev1.ResourceCPU, cpu))
	}
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", ResourceVersion: "3"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment},
			MinReplicas:    &min, MaxReplicas: max, Metrics: metrics,
		},
	}
}

func storedHPA(t *testing.T, e *env, name string) *autoscalingv2.HorizontalPodAutoscaler {
	t.Helper()
	hpa, err := e.client.AutoscalingV2().HorizontalPodAutoscalers("team-a").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get hpa %s: %v", name, err)
	}
	return hpa
}

func apiObjects() []runtime.Object {
	return []runtime.Object{wlDeployment("team-a", "web", 3, withCPURequest(corev1.Container{Name: "app", Image: "nginx"}, "100m"))}
}

func autoscaleBody() map[string]any {
	return map[string]any{"namespace": "team-a", "deployment": "web", "min": 2, "max": 10, "cpuPercent": 80}
}

// ---- list ------------------------------------------------------------------------

func TestAutoscalersList(t *testing.T) {
	cpu, memory, current := int32(80), int32(70), int32(65)
	hpa := hpaFor("web", "web", 2, 10, 0)
	hpa.Spec.Metrics = []autoscalingv2.MetricSpec{resourceMetric(corev1.ResourceCPU, cpu), resourceMetric(corev1.ResourceMemory, memory)}
	hpa.Status = autoscalingv2.HorizontalPodAutoscalerStatus{
		CurrentReplicas: 4, DesiredReplicas: 5,
		CurrentMetrics: []autoscalingv2.MetricStatus{{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricStatus{
			Name: corev1.ResourceCPU, Current: autoscalingv2.MetricValueStatus{AverageUtilization: &current}}}},
	}
	broken := hpaFor("api", "api", 1, 3, 50)
	broken.Status.Conditions = []autoscalingv2.HorizontalPodAutoscalerCondition{{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionFalse}}
	pending := hpaFor("worker", "worker", 1, 3, 50)
	e := newEnv(t, Options{}, hpa, broken, pending)

	list := decode[[]autoscalerJSON](t, e.do(http.MethodGet, "/api/autoscalers?namespace=team-a", nil))
	byName := map[string]autoscalerJSON{}
	for _, a := range list {
		byName[a.Name] = a
	}
	web := byName["web"]
	if web.Target != "Deployment/web" || web.Min != 2 || web.Max != 10 || web.CPUPercent != 80 || web.MemoryPercent != 70 ||
		web.Current != 4 || web.Desired != 5 || web.CurrentCPU != 65 || web.Health != "ok" {
		t.Errorf("web = %+v", web)
	}
	if byName["api"].Health != "error" {
		t.Errorf("an autoscaler that cannot scale must be an error: %+v", byName["api"])
	}
	if byName["worker"].Health != "warn" || byName["worker"].CurrentCPU != -1 {
		t.Errorf("no measurement yet should warn and report -1: %+v", byName["worker"])
	}

	if empty := decode[[]autoscalerJSON](t, e.do(http.MethodGet, "/api/autoscalers?namespace=other", nil)); empty == nil || len(empty) != 0 {
		t.Errorf("an empty namespace must be [] and not null: %v", empty)
	}
	if rec := e.do(http.MethodGet, "/api/autoscalers?namespace=Bad_NS", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad namespace = %d", rec.Code)
	}
}

func TestAutoscalerHealthWhenPinnedAtTheMaximum(t *testing.T) {
	hpa := hpaFor("web", "web", 1, 3, 50)
	hpa.Status.CurrentReplicas, hpa.Status.DesiredReplicas = 3, 3
	cpu := int32(95)
	hpa.Status.CurrentMetrics = []autoscalingv2.MetricStatus{{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricStatus{
		Name: corev1.ResourceCPU, Current: autoscalingv2.MetricValueStatus{AverageUtilization: &cpu}}}}
	if got := autoscalerHealth(hpa); got != "warn" {
		t.Errorf("health = %q; want warn at the maximum", got)
	}
}

// ---- create / update ----------------------------------------------------------------

func TestAutoscalerCreate(t *testing.T) {
	e := newEnv(t, Options{}, apiObjects()...)
	rec := e.do(http.MethodPost, "/api/autoscalers", autoscaleBody())
	resp := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || resp.Action != "created" || resp.Kind != "HorizontalPodAutoscaler" || resp.Name != "web" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if resp.Equivalent != "kubectl autoscale deployment web --min=2 --max=10 --cpu-percent=80 -n team-a" {
		t.Errorf("equivalent = %q", resp.Equivalent)
	}
	for _, warning := range resp.Changes {
		if strings.HasPrefix(warning, "warning:") {
			t.Errorf("unexpected warning %q: the deployment has a CPU request and 3 replicas", warning)
		}
	}

	hpa := storedHPA(t, e, "web")
	if *hpa.Spec.MinReplicas != 2 || hpa.Spec.MaxReplicas != 10 || hpa.Spec.ScaleTargetRef.Name != "web" || hpa.Spec.ScaleTargetRef.Kind != "Deployment" ||
		utilizationTarget(hpa, corev1.ResourceCPU) != 80 || hpa.Labels[managedByLabel] != managedByValue {
		t.Errorf("hpa = %+v", hpa)
	}
}

func TestAutoscalerGuards(t *testing.T) {
	assertWriteGuards(t, "/api/autoscalers", apiObjects, autoscaleBody())
}

func TestAutoscalerUpdateKeepsMetricsItDoesNotOwn(t *testing.T) {
	existing := hpaFor("web", "web", 1, 5, 50)
	pods := int32(1)
	_ = pods
	existing.Spec.Metrics = append(existing.Spec.Metrics, autoscalingv2.MetricSpec{
		Type: autoscalingv2.PodsMetricSourceType,
		Pods: &autoscalingv2.PodsMetricSource{Metric: autoscalingv2.MetricIdentifier{Name: "requests_per_second"}, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.AverageValueMetricType}},
	})
	e := newEnv(t, Options{}, append(apiObjects(), existing)...)

	body := withFields(autoscaleBody(), map[string]any{"memoryPercent": 60})
	rec := e.do(http.MethodPost, "/api/autoscalers", body)
	resp := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || resp.Action != "updated" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	want := []string{"min replicas: 1 -> 2", "max replicas: 5 -> 10", "cpu target: 50% -> 80%", "memory target: none -> 60%"}
	if got := resp.Changes[:4]; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("changes = %v; want %v", got, want)
	}

	hpa := storedHPA(t, e, "web")
	var pod, cpu, memory bool
	for _, m := range hpa.Spec.Metrics {
		switch {
		case m.Type == autoscalingv2.PodsMetricSourceType:
			pod = true
		case m.Resource != nil && m.Resource.Name == corev1.ResourceCPU:
			cpu = *m.Resource.Target.AverageUtilization == 80
		case m.Resource != nil && m.Resource.Name == corev1.ResourceMemory:
			memory = *m.Resource.Target.AverageUtilization == 60
		}
	}
	if !pod || !cpu || !memory {
		t.Errorf("metrics = %+v (custom metric kept %v, cpu %v, memory %v)", hpa.Spec.Metrics, pod, cpu, memory)
	}
	if hpa.ResourceVersion != "3" {
		// The object stays the one that was read; the apiserver compares the version.
		t.Logf("resourceVersion after update = %q", hpa.ResourceVersion)
	}
}

func TestAutoscalerUpdateGuards(t *testing.T) {
	objects := func() []runtime.Object { return append(apiObjects(), hpaFor("web", "web", 1, 5, 50)) }
	assertWriteGuards(t, "/api/autoscalers", objects, autoscaleBody())
}

func TestAutoscalerUnchanged(t *testing.T) {
	e := newEnv(t, Options{}, append(apiObjects(), hpaFor("web", "web", 2, 10, 80))...)
	log := trackWrites(e)
	resp := decode[changeResponse](t, e.do(http.MethodPost, "/api/autoscalers", autoscaleBody()))
	if resp.Action != "unchanged" || len(log.reals())+len(log.dries()) != 0 {
		t.Errorf("same settings must be a no-op: %+v %v", resp, log.reals())
	}
}

func TestAutoscalerWarnings(t *testing.T) {
	// No CPU request, and 3 replicas outside 5-10.
	bare := wlDeployment("team-a", "web", 3, corev1.Container{Name: "app", Image: "nginx"})
	e := newEnv(t, Options{}, bare)
	body := withFields(autoscaleBody(), map[string]any{"min": 5, "memoryPercent": 70, "dryRun": true})

	resp := decode[changeResponse](t, e.do(http.MethodPost, "/api/autoscalers", body))
	var warnings []string
	for _, line := range resp.Changes {
		if strings.HasPrefix(line, "warning:") {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) != 3 {
		t.Fatalf("warnings = %v; want CPU request, memory request and replicas outside the range", warnings)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"no CPU request", "no memory request", "3 replicas now, outside 5-10"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings miss %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(resp.Equivalent, "memory target needs a HorizontalPodAutoscaler manifest") {
		t.Errorf("equivalent = %q", resp.Equivalent)
	}
}

func TestAutoscalerMemoryOnlyEquivalent(t *testing.T) {
	e := newEnv(t, Options{}, apiObjects()...)
	body := withFields(autoscaleBody(), map[string]any{"cpuPercent": 0, "memoryPercent": 75, "dryRun": true})
	resp := decode[changeResponse](t, e.do(http.MethodPost, "/api/autoscalers", body))
	if !strings.Contains(resp.Equivalent, "kubectl apply -f hpa.yaml") {
		t.Errorf("equivalent = %q", resp.Equivalent)
	}
}

func TestAutoscalerRefusesASecondAutoscalerOnTheSameDeployment(t *testing.T) {
	e := newEnv(t, Options{}, append(apiObjects(), hpaFor("web-old", "web", 1, 3, 50))...)
	log := trackWrites(e)
	rec := e.do(http.MethodPost, "/api/autoscalers", autoscaleBody())
	if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "already_autoscaled" || len(log.reals()) != 0 {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAutoscalerRefusesToHijackAnAutoscalerForAnotherTarget(t *testing.T) {
	e := newEnv(t, Options{}, append(apiObjects(), hpaFor("web", "other", 1, 3, 50))...)
	rec := e.do(http.MethodPost, "/api/autoscalers", autoscaleBody())
	if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "already_exists" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAutoscalerValidation(t *testing.T) {
	e := newEnv(t, Options{}, apiObjects()...)
	log := trackWrites(e)
	with := func(extra map[string]any) map[string]any { return withFields(autoscaleBody(), extra) }

	assertBadRequests(t, e, "/api/autoscalers", map[string]map[string]any{
		"min below 1":         with(map[string]any{"min": 0}),
		"max below min":       with(map[string]any{"min": 5, "max": 4}),
		"max above the cap":   with(map[string]any{"max": 501}),
		"negative cpu":        with(map[string]any{"cpuPercent": -1}),
		"cpu above 200":       with(map[string]any{"cpuPercent": 201}),
		"memory above 200":    with(map[string]any{"memoryPercent": 201}),
		"no metric at all":    with(map[string]any{"cpuPercent": 0, "memoryPercent": 0}),
		"bad deployment name": with(map[string]any{"deployment": "Web_1"}),
		"bad namespace":       with(map[string]any{"namespace": "Team A"}),
		"unknown field":       with(map[string]any{"behavior": "x"}),
	})
	if rec := e.do(http.MethodPost, "/api/autoscalers", with(map[string]any{"deployment": "gone"})); rec.Code != http.StatusNotFound {
		t.Errorf("unknown deployment = %d; want 404", rec.Code)
	}
	if len(log.reals())+len(log.dries()) != 0 {
		t.Errorf("invalid input reached the cluster: %v %v", log.reals(), log.dries())
	}

	// The documented limits themselves are accepted.
	for _, ok := range []map[string]any{
		with(map[string]any{"min": 1, "max": 1}),
		with(map[string]any{"max": 500}),
		with(map[string]any{"cpuPercent": 200}),
	} {
		if rec := e.do(http.MethodPost, "/api/autoscalers", withFields(ok, map[string]any{"dryRun": true})); rec.Code != http.StatusOK {
			t.Errorf("%v = %d: %s", ok, rec.Code, rec.Body.String())
		}
	}
}

// ---- delete ---------------------------------------------------------------------------

func deleteObjects() []runtime.Object { return []runtime.Object{hpaFor("web", "web", 2, 10, 80)} }
func deleteBody() map[string]any      { return map[string]any{"namespace": "team-a", "name": "web"} }

func TestAutoscalerDelete(t *testing.T) {
	e := newEnv(t, Options{}, deleteObjects()...)
	rec := e.do(http.MethodPost, "/api/autoscalers/delete", deleteBody())
	resp := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || resp.Action != "deleted" || resp.Equivalent != "kubectl delete hpa web -n team-a" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := e.client.AutoscalingV2().HorizontalPodAutoscalers("team-a").Get(context.Background(), "web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the autoscaler is still there (err=%v)", err)
	}
}

func TestAutoscalerDeleteGuards(t *testing.T) {
	assertWriteGuards(t, "/api/autoscalers/delete", deleteObjects, deleteBody())
}

func TestAutoscalerDeleteValidation(t *testing.T) {
	e := newEnv(t, Options{}, deleteObjects()...)
	assertBadRequests(t, e, "/api/autoscalers/delete", map[string]map[string]any{
		"bad name":      {"namespace": "team-a", "name": "../web"},
		"bad namespace": {"namespace": "", "name": "web"},
		"unknown field": {"namespace": "team-a", "name": "web", "cascade": true},
	})
	if rec := e.do(http.MethodPost, "/api/autoscalers/delete", map[string]any{"namespace": "team-a", "name": "gone"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown autoscaler = %d; want 404", rec.Code)
	}
}
