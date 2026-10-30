package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var fixedNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func init() {
	now = func() time.Time { return fixedNow }
}

// harness runs the root command against a fake clientset and captures output.
type harness struct {
	app    *App
	client *fake.Clientset
	out    *bytes.Buffer
	errOut *bytes.Buffer
}

func newHarness(t *testing.T, objects ...runtime.Object) *harness {
	t.Helper()

	client := fake.NewSimpleClientset(objects...)
	registerScaleReactors(client)

	h := &harness{client: client, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	h.app = &App{
		In:          strings.NewReader(""),
		Out:         h.out,
		ErrOut:      h.errOut,
		Client:      client,
		ContextName: "dev",
	}
	return h
}

func (h *harness) run(args ...string) error {
	h.out.Reset()
	h.errOut.Reset()
	root := NewRootCmd(h.app)
	root.SetArgs(args)
	return root.ExecuteContext(context.Background())
}

// registerScaleReactors makes the fake clientset understand the deployments/scale
// subresource, which the default object tracker does not.
func registerScaleReactors(client *fake.Clientset) {
	client.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		get := action.(k8stesting.GetAction)
		obj, err := client.Tracker().Get(action.GetResource(), action.GetNamespace(), get.GetName())
		if err != nil {
			return true, nil, err
		}
		deployment := obj.(*appsv1.Deployment)
		return true, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{Name: deployment.Name, Namespace: deployment.Namespace},
			Spec:       autoscalingv1.ScaleSpec{Replicas: *deployment.Spec.Replicas},
		}, nil
	})

	client.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		scale := action.(k8stesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
		obj, err := client.Tracker().Get(action.GetResource(), action.GetNamespace(), scale.Name)
		if err != nil {
			return true, nil, err
		}
		deployment := obj.(*appsv1.Deployment).DeepCopy()
		deployment.Spec.Replicas = &scale.Spec.Replicas
		if err := client.Tracker().Update(action.GetResource(), deployment, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, scale, nil
	})
}

func deployment(namespace, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

func runningPod(namespace, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(fixedNow.Add(-2 * time.Hour)),
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.7",
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: true, RestartCount: 1, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "sidecar", Ready: false, RestartCount: 2, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
			},
		},
	}
}

func replicasOf(t *testing.T, client *fake.Clientset, namespace, name string) int32 {
	t.Helper()
	got, err := client.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return *got.Spec.Replicas
}
