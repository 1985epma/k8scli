package checks

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/1985epma/k8scli/internal/dr"
)

// Export limits.
const (
	defaultExportObjects  = 5000
	defaultExportBytes    = 64 << 20
	defaultExportFileSize = 1 << 20
)

// SecretsIncludedWarning is the warning ExportResult carries when IncludeSecrets was used. The CLI and
// the web console must show it prominently.
const SecretsIncludedWarning = "SECRETS INCLUDED: this export contains secret values (base64 is not encryption). Store it like a credential and delete it when you are done."

// ExportOptions tune an export.
type ExportOptions struct {
	// IncludeSecrets writes Secrets into the export. It is off by default; only a caller that has asked
	// the person explicitly should set it. Without it, Secrets are only counted and listed by name.
	IncludeSecrets bool
	// Limits (defaults: 5000 objects, 64 MiB in total, 1 MiB per file).
	MaxObjects   int
	MaxBytes     int64
	MaxFileBytes int
}

// SecretInfo is one line of the secrets inventory: never a value, never even a key name.
type SecretInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Keys int    `json:"keys"`
	// OwnedBy is "Kind/name" of the controller that owns the Secret (a CloudNativePG Cluster, say). Such a
	// Secret is listed so the inventory is honest about what exists, but it is never exported: its owner
	// recreates it.
	OwnedBy string `json:"ownedBy,omitempty"`
}

// ExportResult describes what an export wrote.
type ExportResult struct {
	Namespace string         `json:"namespace"`
	Context   string         `json:"context"`
	Files     []string       `json:"files"`
	Objects   int            `json:"objects"`
	Bytes     int64          `json:"bytes"`
	Counts    map[string]int `json:"counts"`
	// Skipped counts objects left out because a controller owns them (they are recreated by it).
	Skipped         int          `json:"skipped"`
	Secrets         []SecretInfo `json:"secrets"`
	IncludedSecrets bool         `json:"includedSecrets"`
	Warnings        []string     `json:"warnings,omitempty"`
	Truncated       bool         `json:"truncated,omitempty"`
}

type exportFile struct {
	path string
	data []byte
}

type exportBundle struct {
	files  []exportFile
	result ExportResult
}

// resourceKind says how to list and name one kind.
type resourceKind struct {
	kind       string
	apiVersion string
	dir        string
	list       func(ctx context.Context, client kubernetes.Interface, namespace string) ([]runtime.Object, error)
}

// collectObjects lists one kind through pages and returns pointers to the items.
func collectObjects[T any, PT interface {
	*T
	runtime.Object
}](fetch func(options metav1.ListOptions) ([]T, string, error)) ([]runtime.Object, error) {
	items, _, err := pageAll(maxObjects, fetch)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.Object, 0, len(items))
	for i := range items {
		out = append(out, PT(&items[i]))
	}
	return out, nil
}

func exportKinds() []resourceKind {
	return []resourceKind{
		{"Deployment", "apps/v1", "deployment", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[appsv1.Deployment](func(o metav1.ListOptions) ([]appsv1.Deployment, string, error) {
				p, err := c.AppsV1().Deployments(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"StatefulSet", "apps/v1", "statefulset", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[appsv1.StatefulSet](func(o metav1.ListOptions) ([]appsv1.StatefulSet, string, error) {
				p, err := c.AppsV1().StatefulSets(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"DaemonSet", "apps/v1", "daemonset", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[appsv1.DaemonSet](func(o metav1.ListOptions) ([]appsv1.DaemonSet, string, error) {
				p, err := c.AppsV1().DaemonSets(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"CronJob", "batch/v1", "cronjob", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[batchv1.CronJob](func(o metav1.ListOptions) ([]batchv1.CronJob, string, error) {
				p, err := c.BatchV1().CronJobs(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"Service", "v1", "service", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[corev1.Service](func(o metav1.ListOptions) ([]corev1.Service, string, error) {
				p, err := c.CoreV1().Services(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"Ingress", "networking.k8s.io/v1", "ingress", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[networkingv1.Ingress](func(o metav1.ListOptions) ([]networkingv1.Ingress, string, error) {
				p, err := c.NetworkingV1().Ingresses(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"NetworkPolicy", "networking.k8s.io/v1", "networkpolicy", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[networkingv1.NetworkPolicy](func(o metav1.ListOptions) ([]networkingv1.NetworkPolicy, string, error) {
				p, err := c.NetworkingV1().NetworkPolicies(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"HorizontalPodAutoscaler", "autoscaling/v2", "horizontalpodautoscaler", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[autoscalingv2.HorizontalPodAutoscaler](func(o metav1.ListOptions) ([]autoscalingv2.HorizontalPodAutoscaler, string, error) {
				p, err := c.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"PodDisruptionBudget", "policy/v1", "poddisruptionbudget", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[policyv1.PodDisruptionBudget](func(o metav1.ListOptions) ([]policyv1.PodDisruptionBudget, string, error) {
				p, err := c.PolicyV1().PodDisruptionBudgets(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"ConfigMap", "v1", "configmap", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[corev1.ConfigMap](func(o metav1.ListOptions) ([]corev1.ConfigMap, string, error) {
				p, err := c.CoreV1().ConfigMaps(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"PersistentVolumeClaim", "v1", "persistentvolumeclaim", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[corev1.PersistentVolumeClaim](func(o metav1.ListOptions) ([]corev1.PersistentVolumeClaim, string, error) {
				p, err := c.CoreV1().PersistentVolumeClaims(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"ServiceAccount", "v1", "serviceaccount", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[corev1.ServiceAccount](func(o metav1.ListOptions) ([]corev1.ServiceAccount, string, error) {
				p, err := c.CoreV1().ServiceAccounts(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"Role", "rbac.authorization.k8s.io/v1", "role", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[rbacv1.Role](func(o metav1.ListOptions) ([]rbacv1.Role, string, error) {
				p, err := c.RbacV1().Roles(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
		{"RoleBinding", "rbac.authorization.k8s.io/v1", "rolebinding", func(ctx context.Context, c kubernetes.Interface, ns string) ([]runtime.Object, error) {
			return collectObjects[rbacv1.RoleBinding](func(o metav1.ListOptions) ([]rbacv1.RoleBinding, string, error) {
				p, err := c.RbacV1().RoleBindings(ns).List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return p.Items, p.Continue, nil
			})
		}},
	}
}

// ownedByController is true for objects a controller creates and will recreate.
func ownedByController(obj metav1.Object) bool { return controllerOf(obj) != "" }

// controllerOf names the controller that owns obj as "Kind/name", or "" when there is none.
func controllerOf(obj metav1.Object) string {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind + "/" + ref.Name
		}
	}
	return ""
}

// autoManaged reports objects Kubernetes or a default install creates in every namespace.
func autoManaged(kind string, obj metav1.Object, raw map[string]any) bool {
	switch kind {
	case "ConfigMap":
		return obj.GetName() == "kube-root-ca.crt"
	case "ServiceAccount":
		if obj.GetName() != "default" {
			return false
		}
		_, pull := raw["imagePullSecrets"]
		return !pull && len(obj.GetAnnotations()) == 0
	case "Service":
		return obj.GetName() == "kubernetes" && obj.GetNamespace() == "default"
	case "Role", "RoleBinding":
		return strings.HasPrefix(obj.GetName(), "system:")
	}
	return false
}

// removedAnnotation reports annotations that are bookkeeping of one cluster.
func removedAnnotation(key string) bool {
	switch {
	case key == "kubectl.kubernetes.io/last-applied-configuration", key == "deployment.kubernetes.io/revision":
		return true
	case strings.HasPrefix(key, "meta.helm.sh/"), strings.HasPrefix(key, "pv.kubernetes.io/"):
		return true
	case strings.HasPrefix(key, "volume.kubernetes.io/"), strings.HasPrefix(key, "volume.beta.kubernetes.io/"):
		return true
	}
	return false
}

// sanitize turns an API object into a manifest that can be applied to another cluster.
func sanitize(kind, apiVersion string, raw map[string]any) {
	raw["apiVersion"], raw["kind"] = apiVersion, kind
	delete(raw, "status")

	if metadata, ok := raw["metadata"].(map[string]any); ok {
		for _, field := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "selfLink",
			"deletionTimestamp", "deletionGracePeriodSeconds", "ownerReferences", "finalizers"} {
			delete(metadata, field)
		}
		if annotations, ok := metadata["annotations"].(map[string]any); ok {
			for key := range annotations {
				if removedAnnotation(key) {
					delete(annotations, key)
				}
			}
			if len(annotations) == 0 {
				delete(metadata, "annotations")
			}
		}
	}

	spec, _ := raw["spec"].(map[string]any)
	switch kind {
	case "Service":
		for _, field := range []string{"clusterIP", "clusterIPs", "healthCheckNodePort"} {
			delete(spec, field)
		}
	case "PersistentVolumeClaim":
		delete(spec, "volumeName")
	case "ServiceAccount":
		// Legacy token Secrets are generated per cluster.
		delete(raw, "secrets")
	}
	dropNilTimestamps(raw)
}

// dropNilTimestamps removes the `creationTimestamp: null` that typed objects serialise inside templates.
func dropNilTimestamps(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "creationTimestamp" && child == nil {
				delete(v, key)
				continue
			}
			dropNilTimestamps(child)
		}
	case []any:
		for _, child := range v {
			dropNilTimestamps(child)
		}
	}
}

func toMap(obj runtime.Object) (map[string]any, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// safeFileName keeps file names to a portable alphabet.
func safeFileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || strings.HasPrefix(out, ".") {
		out = "_" + out
	}
	return out
}

// collect reads a namespace and builds the files of the export. It never writes anywhere.
func collect(ctx context.Context, env *dr.Env, namespace string, options ExportOptions) (*exportBundle, error) {
	if err := requireClient(env); err != nil {
		return nil, err
	}
	if problems := validation.IsDNS1123Label(namespace); len(problems) > 0 {
		return nil, fmt.Errorf("invalid namespace %q", namespace)
	}
	if options.MaxObjects <= 0 {
		options.MaxObjects = defaultExportObjects
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = defaultExportBytes
	}
	if options.MaxFileBytes <= 0 {
		options.MaxFileBytes = defaultExportFileSize
	}

	bundle := &exportBundle{result: ExportResult{
		Namespace: namespace, Context: env.Context, Counts: map[string]int{}, Secrets: []SecretInfo{},
		IncludedSecrets: options.IncludeSecrets, Files: []string{},
	}}
	result := &bundle.result
	used := map[string]bool{}

	add := func(dir, name string, data []byte) bool {
		if len(data) > options.MaxFileBytes {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s/%s is larger than %d bytes and was left out.", dir, name, options.MaxFileBytes))
			return true
		}
		if result.Bytes+int64(len(data)) > options.MaxBytes || result.Objects >= options.MaxObjects {
			result.Truncated = true
			return false
		}
		base := safeFileName(name)
		path := dir + "/" + base + ".yaml"
		for n := 2; used[path]; n++ {
			path = fmt.Sprintf("%s/%s-%d.yaml", dir, base, n)
		}
		used[path] = true
		bundle.files = append(bundle.files, exportFile{path: path, data: data})
		result.Bytes += int64(len(data))
		result.Objects++
		return true
	}

	for _, kind := range exportKinds() {
		objects, err := kind.list(ctx, env.Client, namespace)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("Could not list %ss: %v", kind.kind, err))
			continue
		}
		type named struct {
			name string
			data []byte
		}
		var items []named
		for _, obj := range objects {
			accessor, err := meta.Accessor(obj)
			if err != nil {
				continue
			}
			if ownedByController(accessor) {
				result.Skipped++
				continue
			}
			raw, err := toMap(obj)
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("Could not read %s %s: %v", kind.kind, accessor.GetName(), err))
				continue
			}
			if autoManaged(kind.kind, accessor, raw) {
				result.Skipped++
				continue
			}
			sanitize(kind.kind, kind.apiVersion, raw)
			data, err := yaml.Marshal(raw)
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("Could not write %s %s: %v", kind.kind, accessor.GetName(), err))
				continue
			}
			items = append(items, named{accessor.GetName(), data})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
		for _, item := range items {
			if !add(kind.dir, item.name, item.data) {
				break
			}
			result.Counts[kind.kind]++
		}
	}

	if err := collectSecrets(ctx, env, namespace, options, bundle, add); err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("Could not read Secrets: %v", err))
	}
	if result.Truncated {
		result.Warnings = append(result.Warnings, "The export hit its size or object limit and is incomplete.")
	}
	if options.IncludeSecrets {
		result.Warnings = append([]string{SecretsIncludedWarning}, result.Warnings...)
	} else if len(result.Secrets) > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%d Secret(s) were NOT exported: they are only listed by name in INVENTORY.md.", len(result.Secrets)))
	}

	sort.Slice(bundle.files, func(i, j int) bool { return bundle.files[i].path < bundle.files[j].path })
	inventory := renderInventory(env, result)
	bundle.files = append([]exportFile{{path: "INVENTORY.md", data: inventory}}, bundle.files...)
	for _, f := range bundle.files {
		result.Files = append(result.Files, f.path)
	}
	return bundle, nil
}

// collectSecrets lists Secrets. Without IncludeSecrets it keeps only name, type and the NUMBER of keys:
// the data is dropped as soon as the page is read.
func collectSecrets(ctx context.Context, env *dr.Env, namespace string, options ExportOptions, bundle *exportBundle, add func(dir, name string, data []byte) bool) error {
	secrets, err := collectObjects[corev1.Secret](func(o metav1.ListOptions) ([]corev1.Secret, string, error) {
		page, err := env.Client.CoreV1().Secrets(namespace).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		// Reduce the page right away when values are not wanted.
		if !options.IncludeSecrets {
			slim := make([]corev1.Secret, len(page.Items))
			for i := range page.Items {
				s := &page.Items[i]
				slim[i] = corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: s.Name, OwnerReferences: s.OwnerReferences},
					Type:       s.Type,
					Data:       placeholderKeys(len(s.Data) + len(s.StringData)),
				}
			}
			return slim, page.Continue, nil
		}
		return page.Items, page.Continue, nil
	})
	if err != nil {
		return fmt.Errorf("list secrets: %w", err)
	}

	type named struct {
		name string
		data []byte
	}
	var included []named
	for _, obj := range secrets {
		secret, ok := obj.(*corev1.Secret)
		if !ok {
			continue
		}
		if owner := controllerOf(secret); owner != "" {
			// Never exported (the controller recreates it), but the inventory must still say it exists.
			bundle.result.Skipped++
			bundle.result.Secrets = append(bundle.result.Secrets, SecretInfo{
				Name: secret.Name, Type: string(secret.Type), Keys: len(secret.Data), OwnedBy: owner,
			})
			continue
		}
		// Service-account tokens and Helm release records are generated per cluster.
		if secret.Type == corev1.SecretTypeServiceAccountToken || strings.HasPrefix(string(secret.Type), "helm.sh/release") {
			bundle.result.Skipped++
			continue
		}
		bundle.result.Secrets = append(bundle.result.Secrets, SecretInfo{Name: secret.Name, Type: string(secret.Type), Keys: len(secret.Data)})
		if !options.IncludeSecrets {
			continue
		}
		raw, err := toMap(secret)
		if err != nil {
			continue
		}
		sanitize("Secret", "v1", raw)
		data, err := yaml.Marshal(raw)
		if err != nil {
			continue
		}
		included = append(included, named{secret.Name, data})
	}
	sort.Slice(bundle.result.Secrets, func(i, j int) bool { return bundle.result.Secrets[i].Name < bundle.result.Secrets[j].Name })
	sort.Slice(included, func(i, j int) bool { return included[i].name < included[j].name })
	for _, item := range included {
		if !add("secret", item.name, item.data) {
			break
		}
		bundle.result.Counts["Secret"]++
	}
	return nil
}

// placeholderKeys makes a map with n empty entries so the key COUNT survives without any key name or value.
func placeholderKeys(n int) map[string][]byte {
	out := make(map[string][]byte, n)
	for i := 0; i < n; i++ {
		out[fmt.Sprintf("k%d", i)] = nil
	}
	return out
}

// renderInventory writes INVENTORY.md: counts, secrets by name, and how to restore. No values.
func renderInventory(env *dr.Env, r *ExportResult) []byte {
	var b strings.Builder
	at := now(env).UTC().Format(time.RFC3339)
	fmt.Fprintf(&b, "# Export of namespace %s\n\n", r.Namespace)
	fmt.Fprintf(&b, "- Context: %s\n- Generated: %s\n- Objects: %d\n- Left out because a controller owns them: %d\n\n", r.Context, at, r.Objects, r.Skipped)
	if r.IncludedSecrets {
		fmt.Fprintf(&b, "> **%s**\n\n", SecretsIncludedWarning)
	}

	b.WriteString("## Contents\n\n| Kind | Count |\n|------|-------|\n")
	kinds := make([]string, 0, len(r.Counts))
	for kind := range r.Counts {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		fmt.Fprintf(&b, "| %s | %d |\n", kind, r.Counts[kind])
	}

	b.WriteString("\n## Secrets\n\n")
	switch {
	case r.IncludedSecrets:
		b.WriteString("Secrets ARE included in the `secret/` folder, except the ones a controller owns (listed below, never exported).\n\n")
	case len(r.Secrets) == 0:
		b.WriteString("No Secrets in this namespace.\n\n")
	default:
		b.WriteString("Secrets were NOT exported. Recreate them from your secret manager before the workloads start.\n\n")
	}
	if len(r.Secrets) > 0 {
		b.WriteString("| Name | Type | Keys | Owned by |\n|------|------|------|----------|\n")
		owned := 0
		for _, s := range r.Secrets {
			owner := "-"
			if s.OwnedBy != "" {
				owner = s.OwnedBy
				owned++
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %s |\n", s.Name, s.Type, s.Keys, owner)
		}
		b.WriteString("\n")
		if owned > 0 {
			fmt.Fprintf(&b, "%d of these are owned by a controller (an operator, a Helm hook...). They are listed so the "+
				"inventory is complete but are never exported: the controller creates them again.\n\n", owned)
		}
	}

	b.WriteString("## Restoring\n\nApply in this order, waiting for each step to settle:\n\n")
	b.WriteString("1. the namespace itself (`kubectl create namespace " + r.Namespace + "`)\n")
	b.WriteString("2. `persistentvolumeclaim/`, then restore the data from your backups\n")
	b.WriteString("3. `configmap/`, `serviceaccount/`, `role/`, `rolebinding/` (and the Secrets)\n")
	b.WriteString("4. `service/`, `statefulset/`, `deployment/`, `daemonset/`, `cronjob/`\n")
	b.WriteString("5. `horizontalpodautoscaler/`, `poddisruptionbudget/`, `networkpolicy/`, `ingress/`\n\n")
	b.WriteString("Example: `kubectl apply -f deployment/ -n " + r.Namespace + "`\n")
	return []byte(b.String())
}

// ExportZip writes the export as a zip stream. The bytes are deterministic for the same cluster state.
func ExportZip(ctx context.Context, env *dr.Env, namespace string, w io.Writer, options ExportOptions) (*ExportResult, error) {
	bundle, err := collect(ctx, env, namespace, options)
	if err != nil {
		return nil, err
	}
	archive := zip.NewWriter(w)
	fixed := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, f := range bundle.files {
		header := &zip.FileHeader{Name: f.path, Method: zip.Deflate, Modified: fixed}
		header.SetMode(0o600)
		entry, err := archive.CreateHeader(header)
		if err != nil {
			return nil, fmt.Errorf("failed to write the zip: %w", err)
		}
		if _, err := io.Copy(entry, bytes.NewReader(f.data)); err != nil {
			return nil, fmt.Errorf("failed to write the zip: %w", err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("failed to write the zip: %w", err)
	}
	return &bundle.result, nil
}

// ExportDir writes the export into a directory (created if needed, 0700; files 0600).
func ExportDir(ctx context.Context, env *dr.Env, namespace, dir string, options ExportOptions) (*ExportResult, error) {
	bundle, err := collect(ctx, env, namespace, options)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("invalid directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create %s: %w", root, err)
	}
	for _, f := range bundle.files {
		target := filepath.Join(root, filepath.FromSlash(f.path))
		// File names come from safeFileName, so this never leaves root; check anyway.
		if !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return nil, fmt.Errorf("refusing to write outside %s: %s", root, f.path)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, fmt.Errorf("failed to create a folder: %w", err)
		}
		if err := os.WriteFile(target, f.data, 0o600); err != nil {
			return nil, fmt.Errorf("failed to write %s: %w", target, err)
		}
	}
	return &bundle.result, nil
}
