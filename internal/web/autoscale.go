package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	maxAutoscaleReplicas = 500
	maxUtilizationTarget = 200
	unknownCPU           = -1
	maxListedAutoscalers = 5000
)

// ---- list ------------------------------------------------------------------------

type autoscalerJSON struct {
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	Target        string `json:"target"`
	Min           int32  `json:"min"`
	Max           int32  `json:"max"`
	CPUPercent    int32  `json:"cpuPercent"`
	MemoryPercent int32  `json:"memoryPercent"`
	Current       int32  `json:"current"`
	Desired       int32  `json:"desired"`
	// CurrentCPU is the measured average utilization in percent, or -1 when unknown.
	CurrentCPU int32  `json:"currentCPU"`
	Age        string `json:"age"`
	Health     string `json:"health"`
}

// utilizationTarget reads the average-utilization target of a resource metric, or 0.
func utilizationTarget(hpa *autoscalingv2.HorizontalPodAutoscaler, name corev1.ResourceName) int32 {
	for _, m := range hpa.Spec.Metrics {
		if m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil && m.Resource.Name == name &&
			m.Resource.Target.AverageUtilization != nil {
			return *m.Resource.Target.AverageUtilization
		}
	}
	return 0
}

func currentCPU(hpa *autoscalingv2.HorizontalPodAutoscaler) int32 {
	for _, m := range hpa.Status.CurrentMetrics {
		if m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil && m.Resource.Name == corev1.ResourceCPU &&
			m.Resource.Current.AverageUtilization != nil {
			return *m.Resource.Current.AverageUtilization
		}
	}
	return unknownCPU
}

func minReplicas(hpa *autoscalingv2.HorizontalPodAutoscaler) int32 {
	if hpa.Spec.MinReplicas != nil {
		return *hpa.Spec.MinReplicas
	}
	return 1 // the API default
}

// autoscalerHealth: error when the controller cannot scale at all (typically no
// metrics), warn when it is pinned at the maximum or has no measurement yet.
func autoscalerHealth(hpa *autoscalingv2.HorizontalPodAutoscaler) string {
	for _, c := range hpa.Status.Conditions {
		if c.Type == autoscalingv2.ScalingActive && c.Status == corev1.ConditionFalse {
			return healthError
		}
	}
	if hpa.Status.CurrentReplicas >= hpa.Spec.MaxReplicas && hpa.Status.DesiredReplicas >= hpa.Spec.MaxReplicas {
		return healthWarn
	}
	if len(hpa.Status.CurrentMetrics) == 0 {
		return healthWarn
	}
	return healthOK
}

func (s *Server) toAutoscalerJSON(hpa *autoscalingv2.HorizontalPodAutoscaler) autoscalerJSON {
	return autoscalerJSON{
		Namespace: hpa.Namespace, Name: hpa.Name,
		Target: hpa.Spec.ScaleTargetRef.Kind + "/" + hpa.Spec.ScaleTargetRef.Name,
		Min:    minReplicas(hpa), Max: hpa.Spec.MaxReplicas,
		CPUPercent:    utilizationTarget(hpa, corev1.ResourceCPU),
		MemoryPercent: utilizationTarget(hpa, corev1.ResourceMemory),
		Current:       hpa.Status.CurrentReplicas, Desired: hpa.Status.DesiredReplicas,
		CurrentCPU: currentCPU(hpa), Age: s.age(hpa.CreationTimestamp), Health: autoscalerHealth(hpa),
	}
}

func listAutoscalers(ctx context.Context, client kubernetes.Interface, namespace string) ([]autoscalingv2.HorizontalPodAutoscaler, error) {
	var all []autoscalingv2.HorizontalPodAutoscaler
	options := metav1.ListOptions{Limit: listPageSize}
	for {
		list, err := client.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, options)
		if err != nil {
			return nil, err
		}
		all = append(all, list.Items...)
		if list.Continue == "" || len(all) >= maxListedAutoscalers {
			return all, nil
		}
		options.Continue = list.Continue
	}
}

// autoscalersTargeting finds the HPAs that scale one deployment.
func autoscalersTargeting(ctx context.Context, client kubernetes.Interface, namespace, deployment string) ([]autoscalingv2.HorizontalPodAutoscaler, error) {
	all, err := listAutoscalers(ctx, client, namespace)
	if err != nil {
		return nil, err
	}
	var matching []autoscalingv2.HorizontalPodAutoscaler
	for _, hpa := range all {
		if hpa.Spec.ScaleTargetRef.Kind == "Deployment" && hpa.Spec.ScaleTargetRef.Name == deployment {
			matching = append(matching, hpa)
		}
	}
	return matching, nil
}

func (s *Server) handleAutoscalers(w http.ResponseWriter, r *http.Request) {
	namespace, err := namespaceParam(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	_, client, err := s.cluster(r.URL.Query().Get("context"), 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	defer cancel()

	hpas, err := listAutoscalers(ctx, client, namespace)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]autoscalerJSON, 0, len(hpas))
	for i := range hpas {
		out = append(out, s.toAutoscalerJSON(&hpas[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- create or update -------------------------------------------------------------

type saveAutoscalerRequest struct {
	mutation
	Namespace     string `json:"namespace"`
	Deployment    string `json:"deployment"`
	Min           int32  `json:"min"`
	Max           int32  `json:"max"`
	CPUPercent    int32  `json:"cpuPercent"`
	MemoryPercent int32  `json:"memoryPercent"`
}

func validateAutoscaler(req *saveAutoscalerRequest) error {
	if _, err := validNamespace(req.Namespace); err != nil {
		return err
	}
	if _, err := validName("deployment", req.Deployment); err != nil {
		return err
	}
	if req.Min < 1 || req.Max > maxAutoscaleReplicas || req.Min > req.Max {
		return badRequest("replicas must satisfy 1 <= min <= max <= %d", maxAutoscaleReplicas)
	}
	for _, target := range []struct {
		field string
		value int32
	}{{"cpuPercent", req.CPUPercent}, {"memoryPercent", req.MemoryPercent}} {
		if target.value < 0 || target.value > maxUtilizationTarget {
			return badRequest("%s must be between 1 and %d (or 0 to leave it out)", target.field, maxUtilizationTarget)
		}
	}
	if req.CPUPercent == 0 && req.MemoryPercent == 0 {
		return badRequest("set a CPU or a memory target")
	}
	return nil
}

func resourceMetric(name corev1.ResourceName, percent int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name:   name,
			Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: &percent},
		},
	}
}

// isUtilizationMetric tells the CPU and memory targets this form owns from any
// other metric someone added by hand, which an update must keep.
func isUtilizationMetric(m autoscalingv2.MetricSpec) bool {
	return m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil &&
		(m.Resource.Name == corev1.ResourceCPU || m.Resource.Name == corev1.ResourceMemory) &&
		m.Resource.Target.Type == autoscalingv2.UtilizationMetricType
}

func mergeMetrics(existing []autoscalingv2.MetricSpec, cpu, memory int32) []autoscalingv2.MetricSpec {
	var metrics []autoscalingv2.MetricSpec
	for _, m := range existing {
		if !isUtilizationMetric(m) {
			metrics = append(metrics, m)
		}
	}
	if cpu > 0 {
		metrics = append(metrics, resourceMetric(corev1.ResourceCPU, cpu))
	}
	if memory > 0 {
		metrics = append(metrics, resourceMetric(corev1.ResourceMemory, memory))
	}
	return metrics
}

func percentText(value int32) string {
	if value == 0 {
		return "none"
	}
	return fmt.Sprintf("%d%%", value)
}

// diffAutoscaler lists what an update changes.
func diffAutoscaler(hpa *autoscalingv2.HorizontalPodAutoscaler, req *saveAutoscalerRequest) []string {
	var changes []string
	if old := minReplicas(hpa); old != req.Min {
		changes = append(changes, fmt.Sprintf("min replicas: %d -> %d", old, req.Min))
	}
	if hpa.Spec.MaxReplicas != req.Max {
		changes = append(changes, fmt.Sprintf("max replicas: %d -> %d", hpa.Spec.MaxReplicas, req.Max))
	}
	if old := utilizationTarget(hpa, corev1.ResourceCPU); old != req.CPUPercent {
		changes = append(changes, fmt.Sprintf("cpu target: %s -> %s", percentText(old), percentText(req.CPUPercent)))
	}
	if old := utilizationTarget(hpa, corev1.ResourceMemory); old != req.MemoryPercent {
		changes = append(changes, fmt.Sprintf("memory target: %s -> %s", percentText(old), percentText(req.MemoryPercent)))
	}
	return changes
}

// autoscaleWarnings catches the setups that look fine and then do nothing.
func autoscaleWarnings(d *appsv1.Deployment, req *saveAutoscalerRequest) []string {
	var warnings []string
	for _, c := range d.Spec.Template.Spec.Containers {
		if req.CPUPercent > 0 && c.Resources.Requests.Cpu().IsZero() {
			warnings = append(warnings, fmt.Sprintf("warning: container %s has no CPU request, so CPU-based scaling will not work until you set one", c.Name))
		}
		if req.MemoryPercent > 0 && c.Resources.Requests.Memory().IsZero() {
			warnings = append(warnings, fmt.Sprintf("warning: container %s has no memory request, so memory-based scaling will not work until you set one", c.Name))
		}
	}
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	if replicas < req.Min || replicas > req.Max {
		warnings = append(warnings, fmt.Sprintf("warning: the deployment has %d replicas now, outside %d-%d; the autoscaler will change it", replicas, req.Min, req.Max))
	}
	return warnings
}

func autoscaleCommand(req *saveAutoscalerRequest) string {
	base := fmt.Sprintf("kubectl autoscale deployment %s --min=%d --max=%d", req.Deployment, req.Min, req.Max)
	ns := kubectlNamespace(req.Namespace)
	switch {
	case req.CPUPercent > 0 && req.MemoryPercent > 0:
		return fmt.Sprintf("%s --cpu-percent=%d%s  # the memory target needs a HorizontalPodAutoscaler manifest", base, req.CPUPercent, ns)
	case req.CPUPercent > 0:
		return fmt.Sprintf("%s --cpu-percent=%d%s", base, req.CPUPercent, ns)
	default:
		return fmt.Sprintf("kubectl apply -f hpa.yaml  # autoscaling/v2 HorizontalPodAutoscaler with a %d%% memory target, %d-%d replicas%s", req.MemoryPercent, req.Min, req.Max, ns)
	}
}

func (s *Server) handleAutoscalerSave(w http.ResponseWriter, r *http.Request) {
	var req saveAutoscalerRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := validateAutoscaler(&req); err != nil {
		writeErr(w, err)
		return
	}
	contextName, err := s.authorize(req.mutation)
	if err != nil {
		writeErr(w, err)
		return
	}
	_, client, err := s.cluster(contextName, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	defer cancel()

	deployment, err := client.AppsV1().Deployments(req.Namespace).Get(ctx, req.Deployment, metav1.GetOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}
	warnings := autoscaleWarnings(deployment, &req)

	targeting, err := autoscalersTargeting(ctx, client, req.Namespace, req.Deployment)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Two autoscalers on one deployment fight each other.
	for _, other := range targeting {
		if other.Name != req.Deployment {
			writeJSON(w, http.StatusConflict, errorResponse{Code: "already_autoscaled",
				Error: fmt.Sprintf("deployment %s is already scaled by autoscaler %s", req.Deployment, other.Name),
				Hint:  "Edit or remove that autoscaler first."})
			return
		}
	}

	response := changeResponse{
		Context: contextName, Kind: "HorizontalPodAutoscaler", Namespace: req.Namespace, Name: req.Deployment,
		DryRun: req.DryRun, Equivalent: autoscaleCommand(&req),
	}
	hpas := client.AutoscalingV2().HorizontalPodAutoscalers(req.Namespace)

	existing, err := hpas.Get(ctx, req.Deployment, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		created := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: req.Deployment, Namespace: req.Namespace, Labels: managedLabels(req.Deployment)},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: req.Deployment},
				MinReplicas:    &req.Min,
				MaxReplicas:    req.Max,
				Metrics:        mergeMetrics(nil, req.CPUPercent, req.MemoryPercent),
			},
		}
		if _, err := hpas.Create(ctx, created, metav1.CreateOptions{DryRun: dryRunOption(req.DryRun), FieldManager: fieldManager}); err != nil {
			writeWorkloadErr(w, err)
			return
		}
		response.Action = "created"
		response.Summary = fmt.Sprintf("Autoscaler for %s created: %d to %d replicas", req.Deployment, req.Min, req.Max)
		response.Changes = []string{
			fmt.Sprintf("min replicas: %d", req.Min), fmt.Sprintf("max replicas: %d", req.Max),
		}
		if req.CPUPercent > 0 {
			response.Changes = append(response.Changes, "cpu target: "+percentText(req.CPUPercent))
		}
		if req.MemoryPercent > 0 {
			response.Changes = append(response.Changes, "memory target: "+percentText(req.MemoryPercent))
		}
	case err != nil:
		writeErr(w, err)
		return
	default:
		if ref := existing.Spec.ScaleTargetRef; ref.Kind != "Deployment" || ref.Name != req.Deployment {
			writeJSON(w, http.StatusConflict, errorResponse{Code: "already_exists",
				Error: fmt.Sprintf("an autoscaler named %s already targets %s/%s", existing.Name, ref.Kind, ref.Name)})
			return
		}
		response.Changes = diffAutoscaler(existing, &req)
		if len(response.Changes) == 0 {
			response.Action = "unchanged"
			response.Summary = fmt.Sprintf("Autoscaler for %s already has these settings", req.Deployment)
			break
		}
		// resourceVersion rides along in the copy, so a concurrent edit is refused instead of overwritten.
		updated := existing.DeepCopy()
		updated.Spec.MinReplicas = &req.Min
		updated.Spec.MaxReplicas = req.Max
		updated.Spec.Metrics = mergeMetrics(existing.Spec.Metrics, req.CPUPercent, req.MemoryPercent)
		if _, err := hpas.Update(ctx, updated, metav1.UpdateOptions{DryRun: dryRunOption(req.DryRun), FieldManager: fieldManager}); err != nil {
			writeWorkloadErr(w, err)
			return
		}
		response.Action = "updated"
		response.Summary = fmt.Sprintf("Autoscaler for %s updated: %d to %d replicas", req.Deployment, req.Min, req.Max)
	}

	if req.DryRun && response.Action != "unchanged" {
		response.Summary = strings.Replace(response.Summary, " created", " would be created", 1)
		response.Summary = strings.Replace(response.Summary, " updated", " would be updated", 1)
	}
	response.Changes = append(response.Changes, warnings...)
	writeJSON(w, http.StatusOK, response)
}

// ---- delete ------------------------------------------------------------------------

type deleteAutoscalerRequest struct {
	mutation
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (s *Server) handleAutoscalerDelete(w http.ResponseWriter, r *http.Request) {
	var req deleteAutoscalerRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := validNamespace(req.Namespace); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := validName("autoscaler", req.Name); err != nil {
		writeErr(w, err)
		return
	}
	contextName, err := s.authorize(req.mutation)
	if err != nil {
		writeErr(w, err)
		return
	}
	_, client, err := s.cluster(contextName, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	defer cancel()

	hpas := client.AutoscalingV2().HorizontalPodAutoscalers(req.Namespace)
	existing, err := hpas.Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := hpas.Delete(ctx, req.Name, metav1.DeleteOptions{DryRun: dryRunOption(req.DryRun)}); err != nil {
		writeWorkloadErr(w, err)
		return
	}

	summary := fmt.Sprintf("Autoscaler %s removed; %s keeps its current replica count", req.Name, existing.Spec.ScaleTargetRef.Name)
	if req.DryRun {
		summary = fmt.Sprintf("Autoscaler %s would be removed; %s would keep its current replica count", req.Name, existing.Spec.ScaleTargetRef.Name)
	}
	writeJSON(w, http.StatusOK, changeResponse{
		Context: contextName, Kind: "HorizontalPodAutoscaler", Namespace: req.Namespace, Name: req.Name,
		Action: "deleted", DryRun: req.DryRun, Summary: summary,
		Changes:    []string{fmt.Sprintf("removes the %d-%d replica range on %s", minReplicas(existing), existing.Spec.MaxReplicas, existing.Spec.ScaleTargetRef.Name)},
		Equivalent: fmt.Sprintf("kubectl delete hpa %s%s", req.Name, kubectlNamespace(req.Namespace)),
	})
}
