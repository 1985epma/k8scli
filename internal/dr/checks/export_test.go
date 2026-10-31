package checks

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/1985epma/k8scli/internal/dr"
)

const secretValue = "TOPSECRET-VALUE-12345"

// stamped adds the server-side fields an export must remove.
func stamped(meta metav1.ObjectMeta) metav1.ObjectMeta {
	meta.UID = types.UID("0f2c-uid")
	meta.ResourceVersion = "99817"
	meta.Generation = 7
	meta.CreationTimestamp = metav1.NewTime(fixedNow.Add(-48 * time.Hour))
	meta.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}}
	meta.Finalizers = []string{"kubernetes.io/pvc-protection"}
	return meta
}

func exportFixture() []runtime.Object {
	controller := true
	api := deployment("shop", "api", 2)
	api.ObjectMeta = stamped(metav1.ObjectMeta{
		Name: "api", Namespace: "shop",
		Annotations: map[string]string{
			"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"x":"` + secretValue + `"}}`,
			"deployment.kubernetes.io/revision":                "3",
			"meta.helm.sh/release-name":                        "shop",
			"team":                                             "payments",
		},
		Labels: map[string]string{"app": "api"},
	})
	api.Status = appsv1.DeploymentStatus{ReadyReplicas: 2, Replicas: 2}

	return []runtime.Object{
		namespace("shop"),
		api,
		&appsv1.StatefulSet{ObjectMeta: stamped(metav1.ObjectMeta{Name: "db", Namespace: "shop"}), Spec: statefulSet("shop", "db", 1).Spec},
		&batchv1.CronJob{ObjectMeta: stamped(metav1.ObjectMeta{Name: "nightly", Namespace: "shop"}), Spec: batchv1.CronJobSpec{Schedule: "0 2 * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: podTemplate("nightly")}}}},
		&corev1.Service{
			ObjectMeta: stamped(metav1.ObjectMeta{Name: "api", Namespace: "shop"}),
			Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.7", ClusterIPs: []string{"10.0.0.7"}, Selector: map[string]string{"app": "api"},
				Ports: []corev1.ServicePort{{Port: 80}}},
			Status: corev1.ServiceStatus{},
		},
		&corev1.Service{ // owned by an operator: it will recreate it
			ObjectMeta: metav1.ObjectMeta{Name: "db-rw", Namespace: "shop", OwnerReferences: []metav1.OwnerReference{{Kind: "Cluster", Name: "db", Controller: &controller}}},
		},
		&networkingv1.Ingress{ObjectMeta: stamped(metav1.ObjectMeta{Name: "public", Namespace: "shop"})},
		&networkingv1.NetworkPolicy{ObjectMeta: stamped(metav1.ObjectMeta{Name: "deny", Namespace: "shop"})},
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: stamped(metav1.ObjectMeta{Name: "api", Namespace: "shop"}),
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 5}},
		&policyv1.PodDisruptionBudget{ObjectMeta: stamped(metav1.ObjectMeta{Name: "api", Namespace: "shop"}),
			Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: ptr(intstr.FromInt32(1))}},
		&corev1.ConfigMap{ObjectMeta: stamped(metav1.ObjectMeta{Name: "app-config", Namespace: "shop"}), Data: map[string]string{"LOG": "info"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "shop"}, Data: map[string]string{"ca.crt": "x"}},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: stamped(metav1.ObjectMeta{Name: "data", Namespace: "shop", Annotations: map[string]string{
				"pv.kubernetes.io/bind-completed": "yes", "volume.kubernetes.io/selected-node": "n1", "owner": "storage-team"}}),
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-123", Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}},
		},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "shop"}, Secrets: []corev1.ObjectReference{{Name: "default-token-x"}}},
		&corev1.ServiceAccount{ObjectMeta: stamped(metav1.ObjectMeta{Name: "runner", Namespace: "shop"}), Secrets: []corev1.ObjectReference{{Name: "runner-token-x"}}},
		&rbacv1.Role{ObjectMeta: stamped(metav1.ObjectMeta{Name: "team:admin", Namespace: "shop"})},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "system:controller:x", Namespace: "shop"}},
		&rbacv1.RoleBinding{ObjectMeta: stamped(metav1.ObjectMeta{Name: "team-admin", Namespace: "shop"})},
		&corev1.Secret{
			ObjectMeta: stamped(metav1.ObjectMeta{Name: "db-credentials", Namespace: "shop"}), Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"password": []byte(secretValue), "username": []byte("ADMIN-USER-9876")},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tls-cert", Namespace: "shop"}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": []byte("CERTDATA"), "tls.key": []byte("KEYDATA")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runner-token-x", Namespace: "shop"}, Type: corev1.SecretTypeServiceAccountToken, Data: map[string][]byte{"token": []byte("jwt")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.shop.v1", Namespace: "shop"}, Type: "helm.sh/release.v1", Data: map[string][]byte{"release": []byte("blob")}},
		// other namespace: must not appear
		deployment("billing", "invoices", 1),
	}
}

// exported collects the files of an export as path -> content.
func exported(t *testing.T, objects []runtime.Object, options ExportOptions) (map[string]string, *ExportResult) {
	t.Helper()
	var buffer bytes.Buffer
	result, err := ExportZip(context.Background(), newEnv(objects...), "shop", &buffer, options)
	if err != nil {
		t.Fatal(err)
	}
	return unzip(t, buffer.Bytes()), result
}

func unzip(t *testing.T, data []byte) map[string]string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range reader.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, _ := io.ReadAll(rc)
		_ = rc.Close()
		files[f.Name] = string(content)
	}
	return files
}

func parse(t *testing.T, content string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal([]byte(content), &out); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, content)
	}
	return out
}

func TestExportLayoutAndCounts(t *testing.T) {
	files, result := exported(t, exportFixture(), ExportOptions{})

	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{
		"INVENTORY.md",
		"configmap/app-config.yaml",
		"cronjob/nightly.yaml",
		"deployment/api.yaml",
		"horizontalpodautoscaler/api.yaml",
		"ingress/public.yaml",
		"networkpolicy/deny.yaml",
		"persistentvolumeclaim/data.yaml",
		"poddisruptionbudget/api.yaml",
		"role/team_admin.yaml",
		"rolebinding/team-admin.yaml",
		"service/api.yaml",
		"serviceaccount/runner.yaml",
		"statefulset/db.yaml",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Errorf("files:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
	if result.Objects != 13 || result.Counts["Deployment"] != 1 || result.Counts["Role"] != 1 {
		t.Errorf("result = %+v", result)
	}
	// db-rw (controller-owned), kube-root-ca.crt, the default ServiceAccount, the system: Role, the token
	// Secret and the Helm release Secret.
	if result.Skipped != 6 {
		t.Errorf("skipped = %d; want 6", result.Skipped)
	}
	if strings.Contains(strings.Join(names, " "), "invoices") {
		t.Error("another namespace leaked into the export")
	}
}

func TestExportSanitisesManifests(t *testing.T) {
	files, _ := exported(t, exportFixture(), ExportOptions{})

	deploy := parse(t, files["deployment/api.yaml"])
	if deploy["apiVersion"] != "apps/v1" || deploy["kind"] != "Deployment" {
		t.Errorf("type meta = %v %v", deploy["apiVersion"], deploy["kind"])
	}
	if _, ok := deploy["status"]; ok {
		t.Error("status must be removed")
	}
	metadata := deploy["metadata"].(map[string]any)
	for _, field := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "selfLink", "finalizers"} {
		if _, ok := metadata[field]; ok {
			t.Errorf("metadata.%s must be removed", field)
		}
	}
	annotations := metadata["annotations"].(map[string]any)
	if len(annotations) != 1 || annotations["team"] != "payments" {
		t.Errorf("annotations = %v; only the custom one stays (last-applied, revision and Helm ones go)", annotations)
	}
	if strings.Contains(files["deployment/api.yaml"], "creationTimestamp") {
		t.Errorf("a null creationTimestamp is left in the template:\n%s", files["deployment/api.yaml"])
	}
	if strings.Contains(files["deployment/api.yaml"], secretValue) {
		t.Error("the last-applied annotation leaked into the export")
	}

	service := parse(t, files["service/api.yaml"])["spec"].(map[string]any)
	if _, ok := service["clusterIP"]; ok {
		t.Error("clusterIP must be removed")
	}
	if _, ok := service["clusterIPs"]; ok {
		t.Error("clusterIPs must be removed")
	}
	if service["selector"] == nil {
		t.Error("the selector must stay")
	}

	pvc := parse(t, files["persistentvolumeclaim/data.yaml"])
	if _, ok := pvc["spec"].(map[string]any)["volumeName"]; ok {
		t.Error("volumeName must be removed: the PV does not exist elsewhere")
	}
	pvcAnnotations, _ := pvc["metadata"].(map[string]any)["annotations"].(map[string]any)
	if len(pvcAnnotations) != 1 || pvcAnnotations["owner"] != "storage-team" {
		t.Errorf("pvc annotations = %v", pvcAnnotations)
	}

	account := parse(t, files["serviceaccount/runner.yaml"])
	if _, ok := account["secrets"]; ok {
		t.Error("legacy token references must be removed")
	}
	if strings.Contains(files["cronjob/nightly.yaml"], "creationTimestamp") {
		t.Error("the job template carries a null creationTimestamp")
	}
}

func TestExportNeverIncludesSecretValuesByDefault(t *testing.T) {
	files, result := exported(t, exportFixture(), ExportOptions{})

	var everything strings.Builder
	for name, content := range files {
		if strings.HasPrefix(name, "secret/") {
			t.Errorf("unexpected secret file %s", name)
		}
		everything.WriteString(content)
	}
	text := everything.String()
	for _, forbidden := range []string{
		secretValue, base64.StdEncoding.EncodeToString([]byte(secretValue)),
		"KEYDATA", "CERTDATA", base64.StdEncoding.EncodeToString([]byte("KEYDATA")),
		"ADMIN-USER-9876", "password", "username", "tls.key", "tls.crt", // not even key names
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the export contains %q", forbidden)
		}
	}

	// The inventory lists the Secrets by name, type and the NUMBER of keys.
	inventory := files["INVENTORY.md"]
	for _, want := range []string{"| db-credentials | Opaque | 2 |", "| tls-cert | kubernetes.io/tls | 2 |", "NOT exported"} {
		if !strings.Contains(inventory, want) {
			t.Errorf("inventory is missing %q:\n%s", want, inventory)
		}
	}
	if strings.Contains(inventory, "runner-token-x") || strings.Contains(inventory, "sh.helm.release") {
		t.Error("generated token and Helm release Secrets must not be listed")
	}
	if len(result.Secrets) != 2 || result.IncludedSecrets {
		t.Errorf("secrets = %+v", result.Secrets)
	}
	foundWarning := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "2 Secret(s) were NOT exported") {
			foundWarning = true
		}
		if w == SecretsIncludedWarning {
			t.Error("the includes-secrets warning must not appear by default")
		}
	}
	if !foundWarning {
		t.Errorf("warnings = %v", result.Warnings)
	}
}

func TestExportWithSecretsWarnsProminently(t *testing.T) {
	files, result := exported(t, exportFixture(), ExportOptions{IncludeSecrets: true})

	secret, ok := files["secret/db-credentials.yaml"]
	if !ok {
		t.Fatalf("the secret must be exported when asked for; files: %v", keys(files))
	}
	if !strings.Contains(secret, base64.StdEncoding.EncodeToString([]byte(secretValue))) {
		t.Error("the secret value is missing")
	}
	if strings.Contains(secret, "uid:") || strings.Contains(secret, "resourceVersion") {
		t.Error("secrets are sanitised too")
	}
	if _, ok := files["secret/runner-token-x.yaml"]; ok {
		t.Error("service-account tokens are never exported")
	}
	if !result.IncludedSecrets || len(result.Warnings) == 0 || result.Warnings[0] != SecretsIncludedWarning {
		t.Errorf("warnings = %v; the first one must be the prominent warning", result.Warnings)
	}
	if !strings.Contains(files["INVENTORY.md"], "SECRETS INCLUDED") || !strings.Contains(files["INVENTORY.md"], "Secrets ARE included") {
		t.Errorf("inventory must say so:\n%s", files["INVENTORY.md"])
	}
	if result.Counts["Secret"] != 2 {
		t.Errorf("counts = %v", result.Counts)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestExportZipIsDeterministic(t *testing.T) {
	var first []byte
	for i := 0; i < 3; i++ {
		var buffer bytes.Buffer
		if _, err := ExportZip(context.Background(), newEnv(exportFixture()...), "shop", &buffer, ExportOptions{}); err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = buffer.Bytes()
		} else if !bytes.Equal(first, buffer.Bytes()) {
			t.Fatal("the same cluster state must produce the same bytes")
		}
	}
	reader, _ := zip.NewReader(bytes.NewReader(first), int64(len(first)))
	if reader.File[0].Name != "INVENTORY.md" {
		t.Errorf("the inventory comes first, got %s", reader.File[0].Name)
	}
	for _, f := range reader.File {
		if !f.Modified.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("%s modified %v", f.Name, f.Modified)
		}
	}
}

func TestExportDirWritesPrivateFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out", "shop")
	result, err := ExportDir(context.Background(), newEnv(exportFixture()...), "shop", dir, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 14 {
		t.Errorf("files = %d", len(result.Files))
	}
	content, err := os.ReadFile(filepath.Join(dir, "deployment", "api.yaml"))
	if err != nil || !strings.Contains(string(content), "kind: Deployment") {
		t.Errorf("deployment file: %v\n%s", err, content)
	}
	if goruntime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dir, "deployment", "api.yaml"))
		if info.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v; want 0600", info.Mode().Perm())
		}
		dirInfo, _ := os.Stat(filepath.Join(dir, "deployment"))
		if dirInfo.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v; want 0700", dirInfo.Mode().Perm())
		}
	}
	// The file named after a role with a colon is safe.
	if _, err := os.Stat(filepath.Join(dir, "role", "team_admin.yaml")); err != nil {
		t.Errorf("sanitised name missing: %v", err)
	}
}

func TestSafeFileNames(t *testing.T) {
	for in, want := range map[string]string{
		"api": "api", "team:admin": "team_admin", "../../etc/passwd": ".._.._etc_passwd" /* leading dot handled below */, ".hidden": "_.hidden", "": "_", "a b": "a_b",
	} {
		got := safeFileName(in)
		if in == "../../etc/passwd" {
			if strings.Contains(got, "/") || strings.HasPrefix(got, ".") {
				t.Errorf("safeFileName(%q) = %q; it must not contain a separator or start with a dot", in, got)
			}
			continue
		}
		if got != want {
			t.Errorf("safeFileName(%q) = %q; want %q", in, got, want)
		}
	}

	// Two names that map to the same file get distinct paths.
	objects := []runtime.Object{
		namespace("shop"),
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "a:b", Namespace: "shop"}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "a_b", Namespace: "shop"}},
	}
	files, _ := exported(t, objects, ExportOptions{})
	if _, ok := files["role/a_b.yaml"]; !ok {
		t.Errorf("files = %v", keys(files))
	}
	if _, ok := files["role/a_b-2.yaml"]; !ok {
		t.Errorf("a collision must be disambiguated: %v", keys(files))
	}
}

func TestExportLimits(t *testing.T) {
	var objects []runtime.Object
	objects = append(objects, namespace("shop"))
	for i := 0; i < 5; i++ {
		objects = append(objects, deployment("shop", "d"+itoa(i), 1))
	}
	_, result := exported(t, objects, ExportOptions{MaxObjects: 3})
	if !result.Truncated || result.Objects != 3 {
		t.Errorf("truncated=%v objects=%d", result.Truncated, result.Objects)
	}
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "incomplete") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v", result.Warnings)
	}

	files, result := exported(t, objects, ExportOptions{MaxFileBytes: 50})
	if len(files) != 1 || len(result.Warnings) < 5 { // only INVENTORY.md; each oversized file is reported
		t.Errorf("files = %v warnings = %v", keys(files), result.Warnings)
	}

	_, result = exported(t, objects, ExportOptions{MaxBytes: 2000})
	if !result.Truncated {
		t.Errorf("a byte limit must truncate: %+v", result)
	}
}

func TestExportErrors(t *testing.T) {
	env := newEnv()
	for _, bad := range []string{"", "Bad_NS", "../x", "a/b"} {
		if _, err := ExportZip(context.Background(), env, bad, io.Discard, ExportOptions{}); err == nil {
			t.Errorf("namespace %q must be rejected", bad)
		}
	}
	if _, err := ExportZip(context.Background(), &dr.Env{}, "shop", io.Discard, ExportOptions{}); err == nil {
		t.Error("expected an error without a client")
	}
}

func TestExportKeepsGoingWhenAListFails(t *testing.T) {
	client := fake.NewSimpleClientset(exportFixture()...)
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("secrets is forbidden")
	})
	client.PrependReactor("list", "cronjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("cronjobs is forbidden")
	})
	env := &dr.Env{Context: "lab", Client: client, Now: func() time.Time { return fixedNow }}

	var buffer bytes.Buffer
	result, err := ExportZip(context.Background(), env, "shop", &buffer, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	files := unzip(t, buffer.Bytes())
	if _, ok := files["deployment/api.yaml"]; !ok {
		t.Error("the rest of the export must survive a failing list")
	}
	joined := strings.Join(result.Warnings, "\n")
	if !strings.Contains(joined, "CronJobs") || !strings.Contains(joined, "Secrets") {
		t.Errorf("warnings = %v", result.Warnings)
	}
}

func TestExportIsReadOnly(t *testing.T) {
	client := fake.NewSimpleClientset(exportFixture()...)
	env := &dr.Env{Context: "lab", Client: client, Now: func() time.Time { return fixedNow }}
	if _, err := ExportZip(context.Background(), env, "shop", io.Discard, ExportOptions{IncludeSecrets: true}); err != nil {
		t.Fatal(err)
	}
	assertReadOnly(t, client)
}

// assertReadOnly fails when the fake clientset saw anything but get, list or watch.
func assertReadOnly(t *testing.T, client *fake.Clientset) {
	t.Helper()
	for _, action := range client.Actions() {
		switch action.GetVerb() {
		case "get", "list", "watch":
		default:
			t.Errorf("a read-only package issued %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}
