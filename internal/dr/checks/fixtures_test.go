package checks

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/1985epma/k8scli/internal/dr"
)

var fixedNow = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// podTemplate is a pod that passes every rule: probes, requests, spread, a pinned image.
func podTemplate(app string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app}},
		Spec: corev1.PodSpec{
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: corev1.ScheduleAnyway}},
			Containers: []corev1.Container{{
				Name:           "app",
				Image:          "registry.example.com/" + app + ":1.4.2",
				ReadinessProbe: &corev1.Probe{},
				LivenessProbe:  &corev1.Probe{},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
				}},
			}},
		},
	}
}

func deployment(ns, name string, replicas int32, mutate ...func(*appsv1.Deployment)) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: podTemplate(name),
		},
	}
	for _, m := range mutate {
		m(d)
	}
	return d
}

func statefulSet(ns, name string, replicas int32, mutate ...func(*appsv1.StatefulSet)) *appsv1.StatefulSet {
	s := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: podTemplate(name),
		},
	}
	for _, m := range mutate {
		m(s)
	}
	return s
}

func pdbFor(ns, name, app string) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr(intstr.FromInt32(1)),
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}},
		},
	}
}

func pod(ns, name, app, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": app}},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func node(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func pvc(ns, name string, class *string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: class},
	}
}

func namespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func storageClass(name, provisioner string, isDefault bool) *storagev1.StorageClass {
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: provisioner}
	if isDefault {
		sc.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
	}
	return sc
}

func snapshotClass(name, driver string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotClass",
		"metadata": map[string]any{"name": name}, "driver": driver,
	}}
}

var snapshotKinds = map[schema.GroupVersionResource]string{
	snapshotClasses: "VolumeSnapshotClassList",
}

// newEnv builds an Env with a fake clientset (two nodes unless the objects bring their own) and a fake
// dynamic client with VolumeSnapshotClass support.
func newEnv(objects ...runtime.Object) *dr.Env {
	return &dr.Env{
		Context: "lab",
		Client:  fake.NewSimpleClientset(objects...),
		Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), snapshotKinds),
		Now:     func() time.Time { return fixedNow },
	}
}

// twoNodes are the default healthy topology.
func twoNodes() []runtime.Object { return []runtime.Object{node("n1"), node("n2")} }

func with(base []runtime.Object, more ...runtime.Object) []runtime.Object {
	return append(append([]runtime.Object{}, base...), more...)
}

// noBackups and backupAt are BackupFinders for tests.
func noBackups(context.Context, *dr.Env, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func backupAt(t time.Time) BackupFinder {
	return func(context.Context, *dr.Env, string) (time.Time, bool, error) { return t, true, nil }
}
