package extchecks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/checks"
)

var fixedNow = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

var dynamicKinds = map[schema.GroupVersionResource]string{
	{Group: "velero.io", Version: "v1", Resource: "backups"}:                       "BackupList",
	{Group: "velero.io", Version: "v1", Resource: "schedules"}:                     "ScheduleList",
	{Group: "velero.io", Version: "v1", Resource: "backupstoragelocations"}:        "BackupStorageLocationList",
	{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}: "VolumeSnapshotList",
	{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}:            "CertificateList",
}

func newEnv(name string, objects []runtime.Object, dynamicObjects ...runtime.Object) *dr.Env {
	return &dr.Env{
		Context: name,
		Client:  fake.NewSimpleClientset(objects...),
		Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), dynamicKinds, dynamicObjects...),
		Now:     func() time.Time { return fixedNow },
	}
}

func ns(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// deploy builds a Deployment whose pod spec is shaped by mutate.
func deploy(namespace, name string, mutate func(*corev1.PodSpec)) *appsv1.Deployment {
	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/app:1"}}}
	if mutate != nil {
		mutate(&spec)
	}
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &one, Template: corev1.PodTemplateSpec{Spec: spec}},
	}
}

func image(img string) func(*corev1.PodSpec) {
	return func(s *corev1.PodSpec) { s.Containers[0].Image = img }
}

func secret(namespace, name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func configMap(namespace, name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Data: data}
}

func storageClass(name string, isDefault bool) *storagev1.StorageClass {
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if isDefault {
		sc.Annotations = map[string]string{defaultStorageClassAnnotation: "true"}
	}
	return sc
}

func ingressClass(name string) *networkingv1.IngressClass {
	return &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func pvc(namespace, name string, class *string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: class},
	}
}

func strp(s string) *string { return &s }

func ingress(namespace, name string, class *string, tlsSecrets ...string) *networkingv1.Ingress {
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: networkingv1.IngressSpec{IngressClassName: class}}
	for _, s := range tlsSecrets {
		ing.Spec.TLS = append(ing.Spec.TLS, networkingv1.IngressTLS{SecretName: s})
	}
	return ing
}

func certificate(namespace, name string, ready bool, notAfter time.Time) *unstructured.Unstructured {
	status := "False"
	if ready {
		status = "True"
	}
	obj := map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": status, "message": "issuer not ready"}},
		},
	}
	if !notAfter.IsZero() {
		obj["status"].(map[string]any)["notAfter"] = notAfter.Format(time.RFC3339)
	}
	return &unstructured.Unstructured{Object: obj}
}

func location(name, provider, bucket, region, s3Url, phase string) *unstructured.Unstructured {
	config := map[string]any{}
	if region != "" {
		config["region"] = region
	}
	if s3Url != "" {
		config["s3Url"] = s3Url
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation",
		"metadata": map[string]any{"name": name, "namespace": "velero"},
		"spec":     map[string]any{"provider": provider, "objectStorage": map[string]any{"bucket": bucket}, "config": config},
		"status":   map[string]any{"phase": phase},
	}}
}

func veleroBackup(name string, completed time.Time) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": name, "namespace": "velero"},
		"spec":     map[string]any{},
		"status":   map[string]any{"phase": "Completed", "completionTimestamp": completed.Format(time.RFC3339)},
	}}
}

func veleroSchedule(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Schedule",
		"metadata": map[string]any{"name": name, "namespace": "velero"},
		"spec":     map[string]any{"schedule": "0 2 * * *"},
		"status":   map[string]any{"phase": "Enabled", "lastBackup": fixedNow.Add(-time.Hour).Format(time.RFC3339)},
	}}
}

func volumeSnapshot(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":     map[string]any{"source": map[string]any{"persistentVolumeClaimName": "data"}},
		"status":   map[string]any{"readyToUse": true, "creationTime": fixedNow.Add(-time.Hour).Format(time.RFC3339)},
	}}
}

func nodeInRegion(region string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"topology.kubernetes.io/region": region}}}
}

// ---- assertions ---------------------------------------------------------------------------------------------

func run(t *testing.T, opts Options) *Report {
	t.Helper()
	report, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report
}

func findingsOf(report *Report, rule string) []checks.Finding {
	var out []checks.Finding
	for _, f := range report.Findings {
		if f.Rule == rule {
			out = append(out, f)
		}
	}
	return out
}

func wantRule(t *testing.T, report *Report, rule, name string) checks.Finding {
	t.Helper()
	for _, f := range report.Findings {
		if f.Rule == rule && (name == "" || f.Name == name) {
			return f
		}
	}
	t.Fatalf("expected %s for %q, got %s", rule, name, dump(report))
	return checks.Finding{}
}

func noRule(t *testing.T, report *Report, rule string) {
	t.Helper()
	if got := findingsOf(report, rule); len(got) > 0 {
		t.Fatalf("did not expect %s, got %+v", rule, got)
	}
}

func dump(report *Report) string {
	var parts []string
	for _, f := range report.Findings {
		parts = append(parts, f.Rule+":"+f.Name)
	}
	return strings.Join(parts, " ")
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
