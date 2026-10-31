package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/1985epma/k8scli/internal/kubeconfig"
	"github.com/1985epma/k8scli/internal/podstatus"
)

const (
	listPageSize = 500
	maxBodyBytes = 1 << 16
	// maxScaleReplicas keeps a typo (or a hijacked session) from asking a
	// cluster for billions of pods; kubectl has no such limit, the console does.
	maxScaleReplicas = 1000
	defaultTail      = 100
	maxTail          = 10000
	maxLogSince      = 30 * 24 * time.Hour
	allNamespaces    = "_all"
	checkTimeout     = 10 * time.Second
	logFlushBuffer   = 4096
	bodyReadTimeout  = 10 * time.Second
)

// apiError is an error the handlers turn into a JSON response.
type apiError struct {
	status int
	code   string
	msg    string
	hint   string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, code: "bad_request", msg: fmt.Sprintf(format, args...)}
}

type errorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Hint  string `json:"hint,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResponse{Error: msg, Code: code})
}

// writeErr maps any error to a JSON response. Errors from the cluster keep the
// API server's own message and get a hint when the cause is a known one.
func writeErr(w http.ResponseWriter, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		writeJSON(w, apiErr.status, errorResponse{Error: apiErr.msg, Code: apiErr.code, Hint: apiErr.hint})
		return
	}

	status, code := http.StatusBadGateway, "cluster_error"
	switch {
	case apierrors.IsNotFound(err):
		status, code = http.StatusNotFound, "not_found"
	case apierrors.IsForbidden(err):
		status, code = http.StatusForbidden, "forbidden"
	case apierrors.IsUnauthorized(err):
		code = "cluster_unauthorized"
	case errors.Is(err, context.DeadlineExceeded):
		status, code = http.StatusGatewayTimeout, "timeout"
	}
	writeJSON(w, status, errorResponse{Error: err.Error(), Code: code, Hint: diagnose(err)})
}

func decodeBody(w http.ResponseWriter, r *http.Request, into any) error {
	return decodeBodyLimit(w, r, into, maxBodyBytes)
}

// decodeBodyLimit is decodeBody with a larger limit, for the few requests that
// legitimately carry a lot of text (ConfigMap values).
func decodeBodyLimit(w http.ResponseWriter, r *http.Request, into any, limit int64) error {
	// A client that trickles the body must not hold the connection open.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout))

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return badRequest("invalid request body: %v", err)
	}
	if decoder.More() {
		return badRequest("invalid request body: unexpected data after the JSON object")
	}
	return nil
}

// resolveContext validates the requested context against the kubeconfig. An
// empty name means the kubeconfig's current context.
func (s *Server) resolveContext(name string) (string, error) {
	config, err := s.loader.Load()
	if err != nil {
		return "", &apiError{status: http.StatusBadRequest, code: "kubeconfig_error", msg: err.Error(),
			hint: "Open Setup > Connect a cluster to add one."}
	}
	if name == "" {
		name = config.CurrentContext
	}
	if name == "" {
		return "", &apiError{status: http.StatusBadRequest, code: "no_context", msg: "no context selected",
			hint: "Open Setup > Connect a cluster to add one."}
	}
	if _, ok := config.Contexts[name]; !ok {
		return "", &apiError{status: http.StatusNotFound, code: "unknown_context", msg: fmt.Sprintf("context %q not found in the kubeconfig", name)}
	}
	return name, nil
}

// cluster resolves the request's context and returns a client for it.
func (s *Server) cluster(contextName string, timeout time.Duration) (string, kubernetes.Interface, error) {
	name, err := s.resolveContext(contextName)
	if err != nil {
		return "", nil, err
	}
	client, err := s.clientFor(name, timeout)
	if err != nil {
		return "", nil, &apiError{status: http.StatusBadGateway, code: "client_error", msg: err.Error(), hint: diagnose(err)}
	}
	return name, client, nil
}

// namespaceParam returns "" for all namespaces, or a validated namespace.
func namespaceParam(r *http.Request) (string, error) {
	value := r.URL.Query().Get("namespace")
	if value == "" || value == allNamespaces {
		return metav1.NamespaceAll, nil
	}
	return validNamespace(value)
}

func validNamespace(value string) (string, error) {
	if problems := validation.IsDNS1123Label(value); len(problems) > 0 {
		return "", badRequest("invalid namespace %q", value)
	}
	return value, nil
}

func validName(kind, value string) (string, error) {
	if problems := validation.IsDNS1123Subdomain(value); len(problems) > 0 {
		return "", badRequest("invalid %s name %q", kind, value)
	}
	return value, nil
}

func looksLikeProduction(contextName string) bool {
	return kubeconfig.LooksLikeProduction(contextName)
}

// ---- state ----------------------------------------------------------------

type contextJSON struct {
	kubeconfig.ContextInfo
	Production bool `json:"production"`
}

type stateResponse struct {
	Version         string        `json:"version"`
	ReadOnly        bool          `json:"readOnly"`
	Contexts        []contextJSON `json:"contexts"`
	CurrentContext  string        `json:"currentContext"`
	KubeconfigPaths []string      `json:"kubeconfigPaths"`
	KubeconfigError string        `json:"kubeconfigError,omitempty"`
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	state := stateResponse{
		Version:         s.opts.Version,
		ReadOnly:        s.opts.ReadOnly,
		Contexts:        []contextJSON{},
		KubeconfigPaths: s.loader.Paths(),
	}

	contexts, err := s.loader.Contexts()
	if err != nil {
		state.KubeconfigError = err.Error()
	}
	for _, info := range contexts {
		state.Contexts = append(state.Contexts, contextJSON{ContextInfo: info, Production: looksLikeProduction(info.Name)})
		if info.Current {
			state.CurrentContext = info.Name
		}
	}
	writeJSON(w, http.StatusOK, state)
}

// ---- overview -------------------------------------------------------------

type nodeJSON struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
}

type overviewResponse struct {
	Context          string     `json:"context"`
	Version          string     `json:"version"`
	Platform         string     `json:"platform"`
	Nodes            []nodeJSON `json:"nodes"`
	Namespaces       []string   `json:"namespaces"`
	DefaultNamespace string     `json:"defaultNamespace"`
	Warnings         []string   `json:"warnings"`
}

// serverVersion asks for /version. discovery's ServerVersion takes no context,
// so with a real client the request goes through its REST client and is
// cancelled with ctx; a hung API server must not leave a goroutine behind on
// every refresh. Fake clientsets have no REST client and use the plain call.
func serverVersion(ctx context.Context, client kubernetes.Interface) (*version.Info, error) {
	if rc, ok := client.Discovery().RESTClient().(*rest.RESTClient); ok && rc != nil {
		raw, err := rc.Get().AbsPath("/version").DoRaw(ctx)
		if err != nil {
			return nil, err
		}
		info := &version.Info{}
		if err := json.Unmarshal(raw, info); err != nil {
			return nil, fmt.Errorf("unexpected /version response: %w", err)
		}
		return info, nil
	}
	return client.Discovery().ServerVersion()
}

func (s *Server) overview(ctx context.Context, contextName string, client kubernetes.Interface) (*overviewResponse, error) {
	info, err := serverVersion(ctx, client)
	if err != nil {
		return nil, err
	}

	overview := &overviewResponse{
		Context:          contextName,
		Version:          info.GitVersion,
		Platform:         info.Platform,
		Nodes:            []nodeJSON{},
		Namespaces:       []string{},
		DefaultNamespace: s.contextNamespace(contextName),
		Warnings:         []string{},
	}

	// Nodes and namespaces may be off limits for a namespaced user; the rest of
	// the console still works, so report instead of failing.
	if nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err != nil {
		overview.Warnings = append(overview.Warnings, "Could not list nodes: "+err.Error())
	} else {
		for i := range nodes.Items {
			overview.Nodes = append(overview.Nodes, nodeJSON{Name: nodes.Items[i].Name, Ready: nodeReady(&nodes.Items[i])})
		}
	}
	if namespaces, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err != nil {
		overview.Warnings = append(overview.Warnings, "Could not list namespaces: "+err.Error())
	} else {
		for i := range namespaces.Items {
			overview.Namespaces = append(overview.Namespaces, namespaces.Items[i].Name)
		}
	}
	return overview, nil
}

func (s *Server) contextNamespace(contextName string) string {
	contexts, err := s.loader.Contexts()
	if err == nil {
		for _, info := range contexts {
			if info.Name == contextName && info.Namespace != "" {
				return info.Namespace
			}
		}
	}
	return kubeconfig.DefaultNamespace
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	name, client, err := s.cluster(r.URL.Query().Get("context"), 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	defer cancel()

	overview, err := s.overview(ctx, name, client)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

// ---- pods -----------------------------------------------------------------

type podJSON struct {
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	Ready      string   `json:"ready"`
	Status     string   `json:"status"`
	Health     string   `json:"health"`
	Restarts   int32    `json:"restarts"`
	Age        string   `json:"age"`
	IP         string   `json:"ip"`
	Node       string   `json:"node"`
	Containers []string `json:"containers"`
}

func (s *Server) handlePods(w http.ResponseWriter, r *http.Request) {
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

	pods := []podJSON{}
	options := metav1.ListOptions{Limit: listPageSize}
	for {
		list, err := client.CoreV1().Pods(namespace).List(ctx, options)
		if err != nil {
			writeErr(w, err)
			return
		}
		for i := range list.Items {
			pods = append(pods, s.toPodJSON(&list.Items[i]))
		}
		if list.Continue == "" {
			break
		}
		options.Continue = list.Continue
	}
	writeJSON(w, http.StatusOK, pods)
}

func (s *Server) toPodJSON(pod *corev1.Pod) podJSON {
	status, ready := podstatus.Status(pod), podstatus.Ready(pod)
	containers := make([]string, 0, len(pod.Spec.Containers))
	for _, container := range pod.Spec.Containers {
		containers = append(containers, container.Name)
	}
	return podJSON{
		Namespace:  pod.Namespace,
		Name:       pod.Name,
		Ready:      ready,
		Status:     status,
		Health:     podHealth(status, ready),
		Restarts:   podstatus.Restarts(pod),
		Age:        s.age(pod.CreationTimestamp),
		IP:         pod.Status.PodIP,
		Node:       pod.Spec.NodeName,
		Containers: containers,
	}
}

func (s *Server) age(created metav1.Time) string {
	if created.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(s.now().Sub(created.Time))
}

// ---- deployments ----------------------------------------------------------

type deploymentJSON struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Desired   int32  `json:"desired"`
	Ready     int32  `json:"ready"`
	Updated   int32  `json:"updated"`
	Health    string `json:"health"`
	Age       string `json:"age"`
}

func (s *Server) handleDeployments(w http.ResponseWriter, r *http.Request) {
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

	deployments := []deploymentJSON{}
	options := metav1.ListOptions{Limit: listPageSize}
	for {
		list, err := client.AppsV1().Deployments(namespace).List(ctx, options)
		if err != nil {
			writeErr(w, err)
			return
		}
		for i := range list.Items {
			deployments = append(deployments, s.toDeploymentJSON(&list.Items[i]))
		}
		if list.Continue == "" {
			break
		}
		options.Continue = list.Continue
	}
	writeJSON(w, http.StatusOK, deployments)
}

func (s *Server) toDeploymentJSON(d *appsv1.Deployment) deploymentJSON {
	desired := int32(1) // the API default when spec.replicas is unset
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	return deploymentJSON{
		Namespace: d.Namespace,
		Name:      d.Name,
		Desired:   desired,
		Ready:     d.Status.ReadyReplicas,
		Updated:   d.Status.UpdatedReplicas,
		Health:    deploymentHealth(desired, d.Status.ReadyReplicas),
		Age:       s.age(d.CreationTimestamp),
	}
}

// ---- scale ----------------------------------------------------------------

type scaleRequest struct {
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Replicas  *int32 `json:"replicas"`
	DryRun    bool   `json:"dryRun"`
	// Confirm must equal the context name when the context looks like production.
	Confirm string `json:"confirm"`
}

type scaleResponse struct {
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	From      int32  `json:"from"`
	To        int32  `json:"to"`
	DryRun    bool   `json:"dryRun"`
	Changed   bool   `json:"changed"`
}

func (s *Server) handleScale(w http.ResponseWriter, r *http.Request) {
	var req scaleRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if s.opts.ReadOnly && !req.DryRun {
		writeError(w, http.StatusForbidden, "read_only", "the console is running in read-only mode")
		return
	}
	namespace, err := validNamespace(req.Namespace)
	if err != nil {
		writeErr(w, err)
		return
	}
	name, err := validName("deployment", req.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	if req.Replicas == nil || *req.Replicas < 0 || *req.Replicas > maxScaleReplicas {
		writeErr(w, badRequest("replicas must be a whole number between 0 and %d", maxScaleReplicas))
		return
	}

	contextName, client, err := s.cluster(req.Context, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	defer cancel()

	deployments := client.AppsV1().Deployments(namespace)
	scale, err := deployments.GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}

	response := scaleResponse{
		Context: contextName, Namespace: namespace, Name: name,
		From: scale.Spec.Replicas, To: *req.Replicas, DryRun: req.DryRun,
	}
	if response.From == response.To || req.DryRun {
		writeJSON(w, http.StatusOK, response)
		return
	}

	if looksLikeProduction(contextName) && req.Confirm != contextName {
		writeJSON(w, http.StatusConflict, errorResponse{
			Code:  "confirmation_required",
			Error: fmt.Sprintf("context %q looks like production: type its name to confirm", contextName),
		})
		return
	}

	updated := scale.DeepCopy()
	updated.Spec.Replicas = response.To
	if _, err := deployments.UpdateScale(ctx, name, updated, metav1.UpdateOptions{}); err != nil {
		writeErr(w, err)
		return
	}
	response.Changed = true
	writeJSON(w, http.StatusOK, response)
}

// ---- default context ------------------------------------------------------

func (s *Server) handleSetDefault(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if s.opts.ReadOnly {
		writeError(w, http.StatusForbidden, "read_only", "the console is running in read-only mode")
		return
	}
	name, err := s.resolveContext(req.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.loader.UseContext(name); err != nil {
		writeErr(w, badRequest("%v", err))
		return
	}
	s.dropClients()
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

// ---- connection check -----------------------------------------------------

type checkResponse struct {
	OK         bool     `json:"ok"`
	Context    string   `json:"context"`
	Version    string   `json:"version,omitempty"`
	Platform   string   `json:"platform,omitempty"`
	NodesReady int      `json:"nodesReady"`
	NodesTotal int      `json:"nodesTotal"`
	Namespaces []string `json:"namespaces"`
	Error      string   `json:"error,omitempty"`
	Hint       string   `json:"hint,omitempty"`
}

// handleCheck backs the setup wizard. A failed connection is an expected
// result, so it answers 200 with ok=false and the reason.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	contextName, err := s.resolveContext(r.URL.Query().Get("context"))
	if err != nil {
		writeErr(w, err)
		return
	}

	// The wizard runs right after credentials were refreshed or a context was
	// rewritten, so do not keep serving the previous client for that context.
	s.dropClients()

	result := checkResponse{Context: contextName, Namespaces: []string{}}
	fail := func(err error) {
		result.Error, result.Hint = err.Error(), diagnose(err)
		writeJSON(w, http.StatusOK, result)
	}

	// A dedicated, short-lived client: the wizard should fail fast, and it must
	// pick up credentials that were refreshed a moment ago.
	client, err := s.clientFor(contextName, checkTimeout)
	if err != nil {
		fail(err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()

	overview, err := s.overview(ctx, contextName, client)
	if err != nil {
		fail(err)
		return
	}
	result.OK = true
	result.Version, result.Platform = overview.Version, overview.Platform
	result.Namespaces = overview.Namespaces
	result.NodesTotal = len(overview.Nodes)
	for _, node := range overview.Nodes {
		if node.Ready {
			result.NodesReady++
		}
	}
	writeJSON(w, http.StatusOK, result)
}

// ---- logs -----------------------------------------------------------------

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	namespace, err := validNamespace(query.Get("namespace"))
	if err != nil {
		writeErr(w, err)
		return
	}
	podName, err := validName("pod", query.Get("pod"))
	if err != nil {
		writeErr(w, err)
		return
	}
	options, err := logOptions(query.Get("tail"), query.Get("since"))
	if err != nil {
		writeErr(w, err)
		return
	}
	options.Follow = query.Get("follow") == "true"
	options.Previous = query.Get("previous") == "true"
	options.Timestamps = query.Get("timestamps") == "true"

	_, client, err := s.cluster(query.Get("context"), 0)
	if err != nil {
		writeErr(w, err)
		return
	}

	lookupCtx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	pod, err := client.CoreV1().Pods(namespace).Get(lookupCtx, podName, metav1.GetOptions{})
	cancel()
	if err != nil {
		writeErr(w, err)
		return
	}
	options.Container, err = pickContainer(pod, query.Get("container"))
	if err != nil {
		writeErr(w, err)
		return
	}

	// The stream is not bound by the request timeout: --follow must keep going.
	stream, err := client.CoreV1().Pods(namespace).GetLogs(podName, options).Stream(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	defer func() { _ = stream.Close() }()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	buffer := make([]byte, logFlushBuffer)
	for {
		n, readErr := stream.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				return
			}
			_ = controller.Flush()
		}
		if readErr != nil {
			// Headers are gone, so there is no way to report a late failure.
			if !errors.Is(readErr, io.EOF) {
				_, _ = fmt.Fprintf(w, "\n[log stream ended: %v]\n", readErr)
			}
			return
		}
	}
}

func logOptions(tail, since string) (*corev1.PodLogOptions, error) {
	options := &corev1.PodLogOptions{}

	switch tail {
	case "all":
	case "":
		lines := int64(defaultTail)
		options.TailLines = &lines
	default:
		lines, err := strconv.ParseInt(tail, 10, 64)
		if err != nil || lines < 0 || lines > maxTail {
			return nil, badRequest("tail must be \"all\" or a number between 0 and %d", maxTail)
		}
		options.TailLines = &lines
	}

	if since != "" {
		window, err := time.ParseDuration(since)
		if err != nil || window <= 0 || window > maxLogSince {
			return nil, badRequest("since must be a duration such as 5m or 1h (at most 30 days)")
		}
		seconds := int64(window.Seconds())
		options.SinceSeconds = &seconds
	}
	return options, nil
}

// pickContainer validates an explicit container or defaults to the first one.
func pickContainer(pod *corev1.Pod, requested string) (string, error) {
	if len(pod.Spec.Containers) == 0 {
		return "", badRequest("no containers found in pod %s", pod.Name)
	}
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, container := range pod.Spec.Containers {
		names = append(names, container.Name)
	}
	if requested == "" {
		return names[0], nil
	}
	for _, name := range names {
		if name == requested {
			return requested, nil
		}
	}
	return "", badRequest("container %q not found in pod %s (available: %s)", requested, pod.Name, strings.Join(names, ", "))
}
