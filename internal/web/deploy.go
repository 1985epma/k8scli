package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const (
	// fieldManager shows up in managedFields, so people can tell what the console changed.
	fieldManager = "k8scli-web"

	maxCreateReplicas = 100
	maxImageLength    = 255
	maxEnvVars        = 50
	maxEnvValueLength = 8192
	maxPort           = 65535
	restartedAtKey    = "kubectl.kubernetes.io/restartedAt"
)

// ---- shared helpers -----------------------------------------------------------

// writeWorkloadErr adds the answers a write can get that a read cannot, then
// falls back to writeErr.
func writeWorkloadErr(w http.ResponseWriter, err error) {
	switch {
	case apierrors.IsAlreadyExists(err):
		writeJSON(w, http.StatusConflict, errorResponse{Code: "already_exists", Error: err.Error()})
	case apierrors.IsConflict(err):
		writeJSON(w, http.StatusConflict, errorResponse{Code: "conflict",
			Error: "the object changed while you were editing it", Hint: "Reload it and try again."})
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		// The API server's own validation message is the useful part, in a dry run especially.
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "rejected_by_cluster", Error: err.Error()})
	default:
		writeErr(w, err)
	}
}

// validLabelName is for names that become a label value (app=<name>), which is
// stricter than the DNS subdomain a Deployment itself may have.
func validLabelName(kind, value string) (string, error) {
	if problems := validation.IsDNS1123Label(value); len(problems) > 0 {
		return "", badRequest("invalid %s name %q: use lowercase letters, digits and '-' (at most 63 characters)", kind, value)
	}
	return value, nil
}

// imageReference is the characters of an image reference: registry host and
// port, repository path, tag and digest. It leaves out spaces, control and
// invisible characters (zero width, right-to-left overrides) that could make
// the review screen show a different image than the one that is deployed.
var imageReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)

func validImage(image string) error {
	if image == "" {
		return badRequest("image is required")
	}
	if len(image) > maxImageLength {
		return badRequest("image is too long (at most %d characters)", maxImageLength)
	}
	if !imageReference.MatchString(image) {
		return badRequest("image may only contain letters, digits and . _ : / @ - (for example nginx:1.27 or ghcr.io/org/app@sha256:...)")
	}
	return nil
}

func validPort(kind string, port int32) error {
	if port < 0 || port > maxPort {
		return badRequest("%s must be between 1 and %d", kind, maxPort)
	}
	return nil
}

var safeShellWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// kubectlQuote quotes a value for the equivalent command shown in the UI.
func kubectlQuote(value string) string {
	if safeShellWord.MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func kubectlNamespace(namespace string) string { return " -n " + namespace }

// short keeps change lines readable when a value is long. Characters that
// are not printable (newlines, zero width, bidirectional overrides) are shown
// as escapes, so a value cannot disguise itself in the review screen.
func short(value string) string {
	const limit = 40
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '\n':
			b.WriteByte(' ')
		case unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "\\u%04x", r)
		}
	}
	out := b.String()
	if runes := []rune(out); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return out
}

// containerIndex picks the container to change. With one container the name is optional.
func containerIndex(containers []corev1.Container, requested string) (int, error) {
	names := make([]string, 0, len(containers))
	for i, c := range containers {
		if requested == c.Name {
			return i, nil
		}
		names = append(names, c.Name)
	}
	switch {
	case requested != "":
		return 0, badRequest("container %q not found (available: %s)", requested, strings.Join(names, ", "))
	case len(containers) == 1:
		return 0, nil
	case len(containers) == 0:
		return 0, badRequest("the deployment has no containers")
	default:
		return 0, badRequest("container is required (available: %s)", strings.Join(names, ", "))
	}
}

// deploymentTarget is the (context, namespace, name) every deployment action works on.
type deploymentTarget struct {
	ctx       string
	namespace string
	name      string
	client    kubernetes.Interface
}

// openDeployment validates the target of a write, authorizes it and loads the deployment.
func (s *Server) openDeployment(r *http.Request, m mutation, namespace, name string) (*deploymentTarget, *appsv1.Deployment, context.Context, context.CancelFunc, error) {
	ns, err := validNamespace(namespace)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	n, err := validName("deployment", name)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	contextName, err := s.authorize(m)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	_, client, err := s.cluster(contextName, 0)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	deployment, err := client.AppsV1().Deployments(ns).Get(ctx, n, metav1.GetOptions{})
	if err != nil {
		cancel()
		return nil, nil, nil, nil, err
	}
	return &deploymentTarget{ctx: contextName, namespace: ns, name: n, client: client}, deployment, ctx, cancel, nil
}

func (t *deploymentTarget) response(action string, dryRun bool, summary string, changes []string, equivalent string) changeResponse {
	if changes == nil {
		changes = []string{}
	}
	return changeResponse{
		Context: t.ctx, Kind: "Deployment", Namespace: t.namespace, Name: t.name,
		Action: action, DryRun: dryRun, Summary: summary, Changes: changes, Equivalent: equivalent,
	}
}

// ---- detail --------------------------------------------------------------------

type quantitiesJSON struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type resourcesJSON struct {
	Requests quantitiesJSON `json:"requests"`
	Limits   quantitiesJSON `json:"limits"`
}

type envVarJSON struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// From describes a variable that is not a plain value, e.g. "secret db/password". It is read-only.
	From string `json:"from"`
}

type containerJSON struct {
	Name      string        `json:"name"`
	Image     string        `json:"image"`
	Ports     []int32       `json:"ports"`
	Env       []envVarJSON  `json:"env"`
	Resources resourcesJSON `json:"resources"`
}

type deploymentDetailJSON struct {
	Namespace  string            `json:"namespace"`
	Name       string            `json:"name"`
	Replicas   int32             `json:"replicas"`
	Ready      int32             `json:"ready"`
	Strategy   string            `json:"strategy"`
	Labels     map[string]string `json:"labels"`
	Autoscaler string            `json:"autoscaler"`
	Containers []containerJSON   `json:"containers"`
}

func (s *Server) handleDeploymentDetail(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	namespace, err := validNamespace(query.Get("namespace"))
	if err != nil {
		writeErr(w, err)
		return
	}
	name, err := validName("deployment", query.Get("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	_, client, err := s.cluster(query.Get("context"), 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	defer cancel()

	d, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}

	detail := deploymentDetailJSON{
		Namespace: d.Namespace, Name: d.Name, Replicas: 1, Ready: d.Status.ReadyReplicas,
		Strategy: string(d.Spec.Strategy.Type), Labels: d.Labels, Containers: []containerJSON{},
	}
	if detail.Labels == nil {
		detail.Labels = map[string]string{}
	}
	if detail.Strategy == "" {
		detail.Strategy = string(appsv1.RollingUpdateDeploymentStrategyType)
	}
	if d.Spec.Replicas != nil {
		detail.Replicas = *d.Spec.Replicas
	}
	for i := range d.Spec.Template.Spec.Containers {
		detail.Containers = append(detail.Containers, toContainerJSON(&d.Spec.Template.Spec.Containers[i]))
	}

	// An HPA that cannot be listed (RBAC) is not worth failing the whole page.
	if hpas, err := autoscalersTargeting(ctx, client, namespace, name); err == nil && len(hpas) > 0 {
		detail.Autoscaler = hpas[0].Name
	}
	writeJSON(w, http.StatusOK, detail)
}

func toContainerJSON(c *corev1.Container) containerJSON {
	out := containerJSON{Name: c.Name, Image: c.Image, Ports: []int32{}, Env: []envVarJSON{}}
	for _, p := range c.Ports {
		out.Ports = append(out.Ports, p.ContainerPort)
	}
	for _, e := range c.Env {
		out.Env = append(out.Env, envVarJSON{Name: e.Name, Value: e.Value, From: describeEnvSource(e.ValueFrom)})
	}
	out.Resources = resourcesJSON{
		Requests: quantitiesJSON{CPU: quantity(c.Resources.Requests, corev1.ResourceCPU), Memory: quantity(c.Resources.Requests, corev1.ResourceMemory)},
		Limits:   quantitiesJSON{CPU: quantity(c.Resources.Limits, corev1.ResourceCPU), Memory: quantity(c.Resources.Limits, corev1.ResourceMemory)},
	}
	return out
}

func quantity(list corev1.ResourceList, name corev1.ResourceName) string {
	if q, ok := list[name]; ok {
		return q.String()
	}
	return ""
}

// describeEnvSource names where a variable comes from without ever reading it.
func describeEnvSource(source *corev1.EnvVarSource) string {
	switch {
	case source == nil:
		return ""
	case source.SecretKeyRef != nil:
		return "secret " + source.SecretKeyRef.Name + "/" + source.SecretKeyRef.Key
	case source.ConfigMapKeyRef != nil:
		return "configmap " + source.ConfigMapKeyRef.Name + "/" + source.ConfigMapKeyRef.Key
	case source.FieldRef != nil:
		return "field " + source.FieldRef.FieldPath
	case source.ResourceFieldRef != nil:
		return "resource " + source.ResourceFieldRef.Resource
	default:
		return "reference"
	}
}

// ---- create --------------------------------------------------------------------

type envPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type exposeRequest struct {
	Enabled bool   `json:"enabled"`
	Type    string `json:"type"`
	Port    int32  `json:"port"`
}

type createDeploymentRequest struct {
	mutation
	Namespace     string        `json:"namespace"`
	Name          string        `json:"name"`
	Image         string        `json:"image"`
	Replicas      *int32        `json:"replicas"`
	Port          int32         `json:"port"`
	Env           []envPair     `json:"env"`
	CPURequest    string        `json:"cpuRequest"`
	MemoryRequest string        `json:"memoryRequest"`
	CPULimit      string        `json:"cpuLimit"`
	MemoryLimit   string        `json:"memoryLimit"`
	Expose        exposeRequest `json:"expose"`
}

// Upper bounds for a single container's resources. ParseQuantity accepts things
// like 1e9999 and silently saturates 9999999999999999999Ei; the cluster would
// take them and then never schedule the pod.
var (
	maxCPUQuantity    = resource.MustParse("1024")
	maxMemoryQuantity = resource.MustParse("16Ti")
)

// parseQuantity returns nil for an empty string.
func parseQuantity(field, value string, max resource.Quantity) (*resource.Quantity, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	q, err := resource.ParseQuantity(strings.TrimSpace(value))
	if err != nil {
		return nil, badRequest("%s %q is not a valid quantity (for example 100m or 128Mi)", field, value)
	}
	if q.Sign() < 0 {
		return nil, badRequest("%s must not be negative", field)
	}
	if q.Cmp(max) > 0 {
		return nil, badRequest("%s is too large (at most %s)", field, max.String())
	}
	return &q, nil
}

// buildResources validates the four optional quantities and returns the requirements.
func buildResources(req *createDeploymentRequest) (corev1.ResourceRequirements, error) {
	var out corev1.ResourceRequirements
	for _, pair := range []struct {
		field, value string
		resource     corev1.ResourceName
		into         *corev1.ResourceList
	}{
		{"cpuRequest", req.CPURequest, corev1.ResourceCPU, &out.Requests},
		{"memoryRequest", req.MemoryRequest, corev1.ResourceMemory, &out.Requests},
		{"cpuLimit", req.CPULimit, corev1.ResourceCPU, &out.Limits},
		{"memoryLimit", req.MemoryLimit, corev1.ResourceMemory, &out.Limits},
	} {
		max := maxCPUQuantity
		if pair.resource == corev1.ResourceMemory {
			max = maxMemoryQuantity
		}
		q, err := parseQuantity(pair.field, pair.value, max)
		if err != nil {
			return out, err
		}
		if q == nil {
			continue
		}
		if *pair.into == nil {
			*pair.into = corev1.ResourceList{}
		}
		(*pair.into)[pair.resource] = *q
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request, hasRequest := out.Requests[name]
		limit, hasLimit := out.Limits[name]
		if hasRequest && hasLimit && limit.Cmp(request) < 0 {
			return out, badRequest("the %s limit must not be lower than the %s request", name, name)
		}
	}
	return out, nil
}

func buildEnv(pairs []envPair) ([]corev1.EnvVar, error) {
	if len(pairs) > maxEnvVars {
		return nil, badRequest("too many environment variables (at most %d)", maxEnvVars)
	}
	seen := map[string]bool{}
	env := make([]corev1.EnvVar, 0, len(pairs))
	for _, p := range pairs {
		if problems := validation.IsEnvVarName(p.Name); len(problems) > 0 {
			return nil, badRequest("invalid environment variable name %q", p.Name)
		}
		if seen[p.Name] {
			return nil, badRequest("environment variable %q is listed twice", p.Name)
		}
		if len(p.Value) > maxEnvValueLength {
			return nil, badRequest("the value of %s is too long (at most %d characters)", p.Name, maxEnvValueLength)
		}
		seen[p.Name] = true
		env = append(env, corev1.EnvVar{Name: p.Name, Value: p.Value})
	}
	return env, nil
}

// validateCreate checks every field and returns the objects to create.
func validateCreate(req *createDeploymentRequest) (*appsv1.Deployment, *corev1.Service, error) {
	if _, err := validNamespace(req.Namespace); err != nil {
		return nil, nil, err
	}
	if _, err := validLabelName("deployment", req.Name); err != nil {
		return nil, nil, err
	}
	if err := validImage(req.Image); err != nil {
		return nil, nil, err
	}
	replicas := int32(1)
	if req.Replicas != nil {
		replicas = *req.Replicas
	}
	if replicas < 0 || replicas > maxCreateReplicas {
		return nil, nil, badRequest("replicas must be between 0 and %d", maxCreateReplicas)
	}
	if err := validPort("port", req.Port); err != nil {
		return nil, nil, err
	}
	env, err := buildEnv(req.Env)
	if err != nil {
		return nil, nil, err
	}
	resources, err := buildResources(req)
	if err != nil {
		return nil, nil, err
	}

	noEscalation := false
	container := corev1.Container{
		Name: req.Name, Image: req.Image, Env: env, Resources: resources,
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &noEscalation},
	}
	if req.Port > 0 {
		container.Ports = []corev1.ContainerPort{{ContainerPort: req.Port}}
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: req.Namespace, Labels: managedLabels(req.Name)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(req.Name)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: managedLabels(req.Name)},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container}},
			},
		},
	}

	if !req.Expose.Enabled {
		return deployment, nil, nil
	}
	if req.Port == 0 {
		return nil, nil, badRequest("a container port is required to expose the deployment")
	}
	serviceType := corev1.ServiceType(req.Expose.Type)
	switch serviceType {
	case "":
		serviceType = corev1.ServiceTypeClusterIP
	case corev1.ServiceTypeClusterIP, corev1.ServiceTypeLoadBalancer:
	default:
		return nil, nil, badRequest("expose type must be ClusterIP or LoadBalancer")
	}
	servicePort := req.Expose.Port
	if servicePort == 0 {
		servicePort = req.Port
	}
	if err := validPort("expose port", servicePort); err != nil {
		return nil, nil, err
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: req.Namespace, Labels: managedLabels(req.Name)},
		Spec: corev1.ServiceSpec{
			Type:     serviceType,
			Selector: selectorLabels(req.Name),
			Ports:    []corev1.ServicePort{{Port: servicePort, TargetPort: intstr.FromInt32(req.Port), Protocol: corev1.ProtocolTCP}},
		},
	}
	return deployment, service, nil
}

func createChanges(d *appsv1.Deployment, s *corev1.Service) []string {
	c := d.Spec.Template.Spec.Containers[0]
	changes := []string{"image: " + c.Image, fmt.Sprintf("replicas: %d", *d.Spec.Replicas)}
	if len(c.Ports) > 0 {
		changes = append(changes, fmt.Sprintf("container port: %d", c.Ports[0].ContainerPort))
	}
	if len(c.Env) > 0 {
		changes = append(changes, fmt.Sprintf("environment variables: %d", len(c.Env)))
	}
	if r := c.Resources; len(r.Requests) > 0 || len(r.Limits) > 0 {
		changes = append(changes, "requests: "+formatResources(r.Requests), "limits: "+formatResources(r.Limits))
	}
	if s != nil {
		changes = append(changes, fmt.Sprintf("service: %s, port %d -> %d", s.Spec.Type, s.Spec.Ports[0].Port, s.Spec.Ports[0].TargetPort.IntValue()))
	}
	return changes
}

func formatResources(list corev1.ResourceList) string {
	if len(list) == 0 {
		return "none"
	}
	parts := []string{}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if q, ok := list[name]; ok {
			parts = append(parts, fmt.Sprintf("%s %s", name, q.String()))
		}
	}
	return strings.Join(parts, ", ")
}

func createCommand(req *createDeploymentRequest, d *appsv1.Deployment, s *corev1.Service) string {
	c := d.Spec.Template.Spec.Containers[0]
	ns := kubectlNamespace(req.Namespace)
	cmd := fmt.Sprintf("kubectl create deployment %s --image=%s --replicas=%d", req.Name, kubectlQuote(req.Image), *d.Spec.Replicas)
	if len(c.Ports) > 0 {
		cmd += fmt.Sprintf(" --port=%d", c.Ports[0].ContainerPort)
	}
	commands := []string{cmd + ns}
	if len(c.Env) > 0 {
		set := "kubectl set env deployment/" + req.Name
		for _, e := range c.Env {
			set += " " + kubectlQuote(e.Name+"="+e.Value)
		}
		commands = append(commands, set+ns)
	}
	if r := c.Resources; len(r.Requests) > 0 || len(r.Limits) > 0 {
		set := "kubectl set resources deployment/" + req.Name
		if len(r.Requests) > 0 {
			set += " --requests=" + kubectlQuote(strings.ReplaceAll(formatResources(r.Requests), " ", "="))
		}
		if len(r.Limits) > 0 {
			set += " --limits=" + kubectlQuote(strings.ReplaceAll(formatResources(r.Limits), " ", "="))
		}
		commands = append(commands, set+ns)
	}
	if s != nil {
		commands = append(commands, fmt.Sprintf("kubectl expose deployment %s --port=%d --target-port=%d --type=%s%s",
			req.Name, s.Spec.Ports[0].Port, s.Spec.Ports[0].TargetPort.IntValue(), s.Spec.Type, ns))
	}
	return strings.Join(commands, " && ")
}

func (s *Server) handleDeploymentCreate(w http.ResponseWriter, r *http.Request) {
	var req createDeploymentRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	deployment, service, err := validateCreate(&req)
	if err != nil {
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

	options := metav1.CreateOptions{DryRun: dryRunOption(req.DryRun), FieldManager: fieldManager}
	created, err := client.AppsV1().Deployments(req.Namespace).Create(ctx, deployment, options)
	if err != nil {
		writeWorkloadErr(w, err)
		return
	}
	if service != nil {
		if _, err := client.CoreV1().Services(req.Namespace).Create(ctx, service, options); err != nil {
			if !req.DryRun {
				// Do not leave half of what the form promised: remove the deployment this call just made.
				// The UID precondition makes sure it is that very object and not a newer one with the same name.
				remove := metav1.DeleteOptions{}
				if created != nil && created.UID != "" {
					remove.Preconditions = &metav1.Preconditions{UID: &created.UID}
				}
				_ = client.AppsV1().Deployments(req.Namespace).Delete(ctx, req.Name, remove)
			}
			writeWorkloadErr(w, err)
			return
		}
	}

	replicas := *deployment.Spec.Replicas
	summary := fmt.Sprintf("Deployment %s created with %d replicas", req.Name, replicas)
	if req.DryRun {
		summary = fmt.Sprintf("Deployment %s would be created with %d replicas", req.Name, replicas)
	}
	if service != nil {
		summary += " and a " + string(service.Spec.Type) + " service"
	}
	writeJSON(w, http.StatusOK, changeResponse{
		Context: contextName, Kind: "Deployment", Namespace: req.Namespace, Name: req.Name,
		Action: "created", DryRun: req.DryRun, Summary: summary,
		Changes: createChanges(deployment, service), Equivalent: createCommand(&req, deployment, service),
	})
}

// ---- update image --------------------------------------------------------------

type updateImageRequest struct {
	mutation
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Container string `json:"container"`
	Image     string `json:"image"`
}

// conflictRetries is how many times an update is retried after a conflict.
const conflictRetries = 4

// staleEdit is the conflict answered when somebody else changed the field the
// user is editing between the preview and the apply.
func staleEdit(deployment, what string) error {
	return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, deployment,
		errors.New(what+" was changed by someone else"))
}

// updateDeployment saves the modified copy with the resourceVersion read
// earlier. A conflict is usually not another person: while pods roll out, the
// controller rewrites the status every moment, which bumps the resourceVersion.
// So it reads the object again and applies the same edit (reapply) a few times
// before giving up with a 409. Fields the edit does not touch are kept as they
// are on the fresh object, never overwritten with stale values. reapply must
// refuse (see staleEdit) when the field it edits no longer has the value the
// user saw: that is a real concurrent edit and must not be overwritten.
func (t *deploymentTarget) updateDeployment(ctx context.Context, d *appsv1.Deployment, dryRun bool, reapply func(*appsv1.Deployment) error) error {
	deployments := t.client.AppsV1().Deployments(t.namespace)
	options := metav1.UpdateOptions{DryRun: dryRunOption(dryRun), FieldManager: fieldManager}

	current := d
	for attempt := 0; ; attempt++ {
		_, err := deployments.Update(ctx, current, options)
		if !apierrors.IsConflict(err) || attempt >= conflictRetries {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
		}
		fresh, getErr := deployments.Get(ctx, t.name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		if reapplyErr := reapply(fresh); reapplyErr != nil {
			return reapplyErr
		}
		current = fresh
	}
}

func (s *Server) handleDeploymentImage(w http.ResponseWriter, r *http.Request) {
	var req updateImageRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := validImage(req.Image); err != nil {
		writeErr(w, err)
		return
	}
	target, d, ctx, cancel, err := s.openDeployment(r, req.mutation, req.Namespace, req.Name)
	if err != nil {
		writeWorkloadErr(w, err)
		return
	}
	defer cancel()

	i, err := containerIndex(d.Spec.Template.Spec.Containers, req.Container)
	if err != nil {
		writeErr(w, err)
		return
	}
	container := &d.Spec.Template.Spec.Containers[i]
	equivalent := fmt.Sprintf("kubectl set image deployment/%s %s=%s%s", target.name, container.Name, kubectlQuote(req.Image), kubectlNamespace(target.namespace))

	if container.Image == req.Image {
		writeJSON(w, http.StatusOK, target.response("unchanged", req.DryRun,
			fmt.Sprintf("Container %s already runs %s", container.Name, req.Image), nil, equivalent))
		return
	}
	change := fmt.Sprintf("image: %s -> %s", container.Image, req.Image)
	seenImage := container.Image
	container.Image = req.Image
	setImage := func(fresh *appsv1.Deployment) error {
		j, err := containerIndex(fresh.Spec.Template.Spec.Containers, req.Container)
		if err != nil {
			return err
		}
		current := &fresh.Spec.Template.Spec.Containers[j]
		if current.Image != seenImage && current.Image != req.Image {
			return staleEdit(target.name, "the image of container "+current.Name)
		}
		current.Image = req.Image
		return nil
	}
	if err := target.updateDeployment(ctx, d, req.DryRun, setImage); err != nil {
		writeWorkloadErr(w, err)
		return
	}

	summary := fmt.Sprintf("Container %s of %s now uses the new image; pods roll out one by one", container.Name, target.name)
	if req.DryRun {
		summary = fmt.Sprintf("Container %s of %s would use the new image; pods would roll out one by one", container.Name, target.name)
	}
	writeJSON(w, http.StatusOK, target.response("updated", req.DryRun, summary, []string{change}, equivalent))
}

// ---- restart ---------------------------------------------------------------------

type restartRequest struct {
	mutation
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (s *Server) handleDeploymentRestart(w http.ResponseWriter, r *http.Request) {
	var req restartRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	target, d, ctx, cancel, err := s.openDeployment(r, req.mutation, req.Namespace, req.Name)
	if err != nil {
		writeWorkloadErr(w, err)
		return
	}
	defer cancel()

	stamp := s.now().UTC().Format("2006-01-02T15:04:05Z")
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`, restartedAtKey, stamp)
	options := metav1.PatchOptions{DryRun: dryRunOption(req.DryRun), FieldManager: fieldManager}
	if _, err := target.client.AppsV1().Deployments(target.namespace).Patch(ctx, target.name, types.StrategicMergePatchType, []byte(patch), options); err != nil {
		writeWorkloadErr(w, err)
		return
	}

	changes := []string{"pod template annotation " + restartedAtKey + ": " + stamp}
	if d.Spec.Replicas != nil && *d.Spec.Replicas == 0 {
		changes = append(changes, "warning: the deployment has 0 replicas, so there are no pods to restart")
	}
	summary := fmt.Sprintf("Deployment %s restarted; pods are replaced one by one", target.name)
	if req.DryRun {
		summary = fmt.Sprintf("Deployment %s would restart; pods would be replaced one by one", target.name)
	}
	writeJSON(w, http.StatusOK, target.response("restarted", req.DryRun, summary, changes,
		fmt.Sprintf("kubectl rollout restart deployment/%s%s", target.name, kubectlNamespace(target.namespace))))
}

// ---- environment -----------------------------------------------------------------

type updateEnvRequest struct {
	mutation
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Container string            `json:"container"`
	Set       map[string]string `json:"set"`
	Unset     []string          `json:"unset"`
}

func validateEnvRequest(req *updateEnvRequest) error {
	if len(req.Set) == 0 && len(req.Unset) == 0 {
		return badRequest("nothing to change: set or unset at least one variable")
	}
	if len(req.Set)+len(req.Unset) > maxEnvVars {
		return badRequest("too many changes at once (at most %d variables)", maxEnvVars)
	}
	for name, value := range req.Set {
		if problems := validation.IsEnvVarName(name); len(problems) > 0 {
			return badRequest("invalid environment variable name %q", name)
		}
		if len(value) > maxEnvValueLength {
			return badRequest("the value of %s is too long (at most %d characters)", name, maxEnvValueLength)
		}
	}
	for _, name := range req.Unset {
		if problems := validation.IsEnvVarName(name); len(problems) > 0 {
			return badRequest("invalid environment variable name %q", name)
		}
		if _, both := req.Set[name]; both {
			return badRequest("%s is both set and unset", name)
		}
	}
	return nil
}

func (s *Server) handleDeploymentEnv(w http.ResponseWriter, r *http.Request) {
	var req updateEnvRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := validateEnvRequest(&req); err != nil {
		writeErr(w, err)
		return
	}
	target, d, ctx, cancel, err := s.openDeployment(r, req.mutation, req.Namespace, req.Name)
	if err != nil {
		writeWorkloadErr(w, err)
		return
	}
	defer cancel()

	i, err := containerIndex(d.Spec.Template.Spec.Containers, req.Container)
	if err != nil {
		writeErr(w, err)
		return
	}
	container := &d.Spec.Template.Spec.Containers[i]
	seenEnv := snapshotEnv(container, &req)
	changes, args, err := applyEnvChanges(container, &req)
	if err != nil {
		writeErr(w, err)
		return
	}
	equivalent := fmt.Sprintf("kubectl set env deployment/%s -c %s %s%s", target.name, container.Name, strings.Join(args, " "), kubectlNamespace(target.namespace))

	if len(changes) == 0 {
		writeJSON(w, http.StatusOK, target.response("unchanged", req.DryRun, "The environment already matches", nil, equivalent))
		return
	}
	setEnv := func(fresh *appsv1.Deployment) error {
		j, err := containerIndex(fresh.Spec.Template.Spec.Containers, req.Container)
		if err != nil {
			return err
		}
		current := &fresh.Spec.Template.Spec.Containers[j]
		if changedMeanwhile(current, seenEnv) {
			return staleEdit(target.name, "an environment variable you are editing")
		}
		_, _, err = applyEnvChanges(current, &req)
		return err
	}
	if err := target.updateDeployment(ctx, d, req.DryRun, setEnv); err != nil {
		writeWorkloadErr(w, err)
		return
	}

	summary := fmt.Sprintf("Environment of %s updated; changing it rolls out new pods", target.name)
	if req.DryRun {
		summary = fmt.Sprintf("Environment of %s would be updated; changing it would roll out new pods", target.name)
	}
	writeJSON(w, http.StatusOK, target.response("updated", req.DryRun, summary, changes, equivalent))
}

// applyEnvChanges edits the container's plain variables in place. It returns
// the change lines and the kubectl arguments. Variables that come from a
// Secret, ConfigMap or field reference are never touched.
func applyEnvChanges(c *corev1.Container, req *updateEnvRequest) ([]string, []string, error) {
	position := map[string]int{}
	for i, e := range c.Env {
		position[e.Name] = i
	}
	refuse := func(name string) error {
		return badRequest("%s comes from %s and cannot be changed here", name, describeEnvSource(c.Env[position[name]].ValueFrom))
	}

	changes := []string{}
	args := []string{}

	setNames := make([]string, 0, len(req.Set))
	for name := range req.Set {
		setNames = append(setNames, name)
	}
	sort.Strings(setNames)
	for _, name := range setNames {
		value := req.Set[name]
		args = append(args, kubectlQuote(name+"="+value))
		i, exists := position[name]
		switch {
		case !exists:
			c.Env = append(c.Env, corev1.EnvVar{Name: name, Value: value})
			position[name] = len(c.Env) - 1
			changes = append(changes, fmt.Sprintf("+ %s=%s", name, short(value)))
		case c.Env[i].ValueFrom != nil:
			return nil, nil, refuse(name)
		case c.Env[i].Value != value:
			changes = append(changes, fmt.Sprintf("~ %s: %q -> %q", name, short(c.Env[i].Value), short(value)))
			c.Env[i].Value = value
		}
	}

	unset := append([]string(nil), req.Unset...)
	sort.Strings(unset)
	removed := map[string]bool{}
	for _, name := range unset {
		args = append(args, name+"-")
		i, exists := position[name]
		if !exists {
			continue
		}
		if c.Env[i].ValueFrom != nil {
			return nil, nil, refuse(name)
		}
		removed[name] = true
		changes = append(changes, "- "+name)
	}
	if len(removed) > 0 {
		kept := c.Env[:0:0]
		for _, e := range c.Env {
			if !removed[e.Name] {
				kept = append(kept, e)
			}
		}
		c.Env = kept
	}
	return changes, args, nil
}

// envState is what the user saw for one variable.
type envState struct {
	exists bool
	value  string
	source string
}

// snapshotEnv records the variables a request touches, as they are now.
func snapshotEnv(c *corev1.Container, req *updateEnvRequest) map[string]envState {
	touched := map[string]bool{}
	for name := range req.Set {
		touched[name] = true
	}
	for _, name := range req.Unset {
		touched[name] = true
	}
	seen := map[string]envState{}
	for name := range touched {
		seen[name] = envStateOf(c, name)
	}
	return seen
}

func envStateOf(c *corev1.Container, name string) envState {
	for _, e := range c.Env {
		if e.Name == name {
			return envState{exists: true, value: e.Value, source: describeEnvSource(e.ValueFrom)}
		}
	}
	return envState{}
}

// changedMeanwhile reports whether any touched variable differs from what the
// user saw. Changes to other variables or fields are not a conflict.
func changedMeanwhile(c *corev1.Container, seen map[string]envState) bool {
	for name, before := range seen {
		if envStateOf(c, name) != before {
			return true
		}
	}
	return false
}
