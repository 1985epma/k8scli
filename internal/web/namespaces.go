package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const (
	nsMaxLabels      = 20
	nsReservedPrefix = "kube-"
	nsMaxListed      = 5000
)

type namespaceRow struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Age         string `json:"age"`
	Pods        int    `json:"pods"`
	Running     int    `json:"running"`
	Attention   int    `json:"attention"`
	Deployments int    `json:"deployments"`
}

func (s *Server) handleNamespaces(w http.ResponseWriter, r *http.Request) {
	_, client, err := s.cluster(r.URL.Query().Get("context"), 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	defer cancel()

	namespaces, err := listNamespaces(ctx, client)
	if err != nil {
		writeErr(w, err)
		return
	}
	pods, err := s.aggregatePods(ctx, client)
	if err != nil {
		writeErr(w, err)
		return
	}
	deployments, err := s.aggregateDeployments(ctx, client)
	if err != nil {
		writeErr(w, err)
		return
	}

	rows := make([]namespaceRow, 0, len(namespaces))
	for i := range namespaces {
		ns := &namespaces[i]
		row := namespaceRow{
			Name:        ns.Name,
			Status:      string(ns.Status.Phase),
			Age:         s.age(ns.CreationTimestamp),
			Deployments: deployments.perNamespace[ns.Name],
		}
		if row.Status == "" {
			row.Status = string(corev1.NamespaceActive)
		}
		if counts, ok := pods.byNamespace[ns.Name]; ok {
			row.Pods, row.Running, row.Attention = counts.Pods, counts.Running, counts.Attention
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	writeJSON(w, http.StatusOK, rows)
}

func listNamespaces(ctx context.Context, client kubernetes.Interface) ([]corev1.Namespace, error) {
	var all []corev1.Namespace
	options := metav1.ListOptions{Limit: listPageSize}
	for {
		list, err := client.CoreV1().Namespaces().List(ctx, options)
		if err != nil {
			return nil, err
		}
		all = append(all, list.Items...)
		if list.Continue == "" || len(all) >= nsMaxListed {
			return all, nil
		}
		options.Continue = list.Continue
	}
}

type namespaceCreateRequest struct {
	mutation
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

// clusterExistsError turns the API server's AlreadyExists into the console's 409.
func clusterExistsError(kind, name string, err error) error {
	if apierrors.IsAlreadyExists(err) {
		return &apiError{status: http.StatusConflict, code: "already_exists", msg: fmt.Sprintf("%s %q already exists", kind, name)}
	}
	return err
}

func (s *Server) handleNamespaceCreate(w http.ResponseWriter, r *http.Request) {
	var req namespaceCreateRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	name, err := validNamespace(req.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	if strings.HasPrefix(name, nsReservedPrefix) {
		writeErr(w, badRequest("names starting with %q are reserved for Kubernetes itself", nsReservedPrefix))
		return
	}
	if err := validateNamespaceLabels(req.Labels); err != nil {
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

	labels := map[string]string{managedByLabel: managedByValue}
	for key, value := range req.Labels {
		labels[key] = value
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	if _, err := client.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{DryRun: dryRunOption(req.DryRun)}); err != nil {
		writeErr(w, clusterExistsError("namespace", name, err))
		return
	}

	userKeys := make([]string, 0, len(req.Labels))
	for key := range req.Labels {
		userKeys = append(userKeys, key)
	}
	sort.Strings(userKeys)

	changes := []string{"name: " + name, fmt.Sprintf("label %s=%s", managedByLabel, managedByValue)}
	equivalent := "kubectl create namespace " + name
	for _, key := range userKeys {
		changes = append(changes, fmt.Sprintf("label %s=%s", key, req.Labels[key]))
		equivalent += fmt.Sprintf(" && kubectl label namespace %s %s=%s", name, key, req.Labels[key])
	}

	summary := fmt.Sprintf("Namespace %s created", name)
	if req.DryRun {
		summary = fmt.Sprintf("Namespace %s would be created", name)
	}
	writeJSON(w, http.StatusOK, changeResponse{
		Context: contextName, Kind: "Namespace", Name: name, Action: "created", DryRun: req.DryRun,
		Summary: summary, Changes: changes, Equivalent: equivalent,
	})
}

// validateNamespaceLabels accepts a few well-formed labels. The values are put in
// the kubectl command shown to the user, so they must be plain label syntax.
func validateNamespaceLabels(labels map[string]string) error {
	if len(labels) > nsMaxLabels {
		return badRequest("at most %d labels are allowed", nsMaxLabels)
	}
	for key, value := range labels {
		if problems := validation.IsQualifiedName(key); len(problems) > 0 {
			return badRequest("invalid label key %q", key)
		}
		if problems := validation.IsValidLabelValue(value); len(problems) > 0 {
			return badRequest("invalid value for label %q", key)
		}
		if key == managedByLabel {
			return badRequest("the label %q is set by the console", managedByLabel)
		}
		// Labels under these domains drive admission, Pod Security and network
		// policy; creating a namespace is not the place to set them.
		if reservedLabelKey(key) {
			return badRequest("label keys under kubernetes.io and k8s.io are reserved: %q", key)
		}
	}
	return nil
}

// reservedLabelKey reports keys whose prefix is kubernetes.io, k8s.io or one of their subdomains.
func reservedLabelKey(key string) bool {
	prefix, _, found := strings.Cut(key, "/")
	if !found {
		return false
	}
	prefix = strings.ToLower(prefix)
	for _, domain := range []string{"kubernetes.io", "k8s.io"} {
		if prefix == domain || strings.HasSuffix(prefix, "."+domain) {
			return true
		}
	}
	return false
}
