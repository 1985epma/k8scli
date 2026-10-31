package web

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/wire"
)

// These tests run the REAL wire stack (the one the CLI uses) behind the console's adapter, with fake
// Kubernetes clients, to prove the wiring: the right options reach the packages and Secrets never
// leave in an export.

func int32Ptr(v int32) *int32 { return &v }

func newWireBackend(t *testing.T, dir, kubeconfigPath string) *wireBackend {
	t.Helper()
	cluster := fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "team-a", Labels: map[string]string{"app": "api"}},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Ptr(2),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: "nginx:1.27"}}},
				},
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a", Labels: map[string]string{"app": "web"}},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Ptr(2),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx:1.27"}}},
				},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "db-password", Namespace: "team-a"},
			Data:       map[string][]byte{"password": []byte("TOP-SECRET-VALUE")},
		},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	)
	registerScaleReactors(cluster) // the fake clientset has no scale subresource of its own
	stack, err := wire.New(wire.Options{
		Kubeconfig: kubeconfigPath, Dir: dir, Actor: webActor,
		Clients: func(string) (kubernetes.Interface, dynamic.Interface, error) { return cluster, nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &wireBackend{stack: stack}
}

func TestWireBackendExportNeverCarriesSecretValues(t *testing.T) {
	e := newEnv(t, Options{})
	backend := newWireBackend(t, t.TempDir(), e.kubeconfig)

	var out bytes.Buffer
	result, err := backend.Export(context.Background(), "dev", "team-a", &out)
	if err != nil {
		t.Fatal(err)
	}
	if result.IncludedSecrets || result.Namespace != "team-a" {
		t.Errorf("result = %+v", result)
	}

	archive, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range archive.File {
		names = append(names, f.Name)
		rc, _ := f.Open()
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		if strings.Contains(string(body), "TOP-SECRET-VALUE") || strings.Contains(string(body), "VE9QLVNFQ1JFVC1WQUxVRQ") {
			t.Errorf("%s contains the secret value", f.Name)
		}
		if strings.HasPrefix(f.Name, "secret/") {
			t.Errorf("a Secret manifest was exported: %s", f.Name)
		}
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "deployment/api.yaml") || !strings.Contains(joined, "INVENTORY.md") {
		t.Errorf("files = %v", names)
	}
}

func TestWireBackendCheckBackupsDiffAndDryRun(t *testing.T) {
	e := newEnv(t, Options{})
	backend := newWireBackend(t, t.TempDir(), e.kubeconfig)
	ctx := context.Background()

	report, err := backend.Check(ctx, "dev", []string{"team-a"})
	if err != nil || len(report.Namespaces) != 1 || report.Namespaces[0].Name != "team-a" {
		t.Fatalf("check: %+v %v", report, err)
	}
	// The single-replica fixture must trip DR001 on the 2-replica api? No: it has 2 replicas. It lacks
	// probes and requests, so the readiness check finds something to say.
	if len(report.Namespaces[0].Workloads) != 2 || len(report.Namespaces[0].Workloads[0].Findings) == 0 {
		t.Errorf("workloads = %+v", report.Namespaces[0].Workloads)
	}

	if _, err := backend.Backups(ctx, "dev", "team-a"); err != nil {
		t.Errorf("backups without Velero must not fail: %v", err)
	}
	if _, err := backend.Diff(ctx, "dev", "prod-eu", []string{"team-a"}); err != nil {
		t.Errorf("diff: %v", err)
	}

	plan, err := dr.ParsePlan([]byte(planYAML("wired", "dev", true, false)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := backend.Run(ctx, plan, dr.DirDown, dr.Scope{Phases: []dr.Phase{dr.PhaseApps}}, dr.RunOptions{DryRun: true, Actor: webActor})
	if err != nil || result.Status != dr.StatusDryRun || len(result.Steps) == 0 {
		t.Fatalf("dry run through the real stack: %+v %v", result, err)
	}
	for _, step := range result.Steps {
		if step.Status == dr.StepDone {
			t.Errorf("a dry run must not apply anything: %+v", step)
		}
	}
}

func TestDefaultDRBackendIsTheWireStack(t *testing.T) {
	d := newDREnv(t, Options{})
	built, err := buildDRBackend(d.server)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := built.(*wireBackend); !ok {
		t.Errorf("default backend = %T", built)
	}

	// Through the HTTP layer the default stack answers like any other backend (the cluster in the test
	// kubeconfig is not reachable, so a read fails cleanly with a cluster error, never a panic).
	d.server.dr.newBackend = func() (drBackend, error) { return buildDRBackend(d.server) }
	rec := d.do(http.MethodGet, "/api/dr/capabilities", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("capabilities = %d", rec.Code)
	}
}
