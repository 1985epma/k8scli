package k8sops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

var (
	podsGVR         = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	deploymentsGVR  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	statefulSetsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
)

// fakeClock moves only when something sleeps, so no test waits for real.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	onSleep func(total time.Duration) // optional: lets a test change the cluster between polls
	slept   time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.slept += d
	total, hook := f.slept, f.onSleep
	f.mu.Unlock()
	if hook != nil {
		hook(total)
	}
	return nil
}

// stateStore is a StateStore backed by memory that also counts writes.
type stateStore struct {
	mu     sync.Mutex
	docs   map[string][]byte
	writes int
}

func newStateStore() *stateStore { return &stateStore{docs: map[string][]byte{}} }

func (s *stateStore) Load(_ context.Context, key string, into any) (bool, error) {
	s.mu.Lock()
	raw, ok := s.docs[key]
	s.mu.Unlock()
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, into)
}

func (s *stateStore) Save(_ context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[key] = raw
	s.writes++
	return nil
}

func (s *stateStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, key)
	return nil
}

func (s *stateStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.docs[key]
	return ok
}

// testEnv is a dr.Env over the fake clientset.
type testEnv struct {
	t      *testing.T
	env    *dr.Env
	client *fake.Clientset
	clock  *fakeClock
	state  *stateStore
	logs   []string
}

const testContext = "lab"

func newEnv(t *testing.T, objects ...runtime.Object) *testEnv {
	t.Helper()
	client := fake.NewSimpleClientset(objects...)
	registerScale(client, deploymentsGVR)
	registerScale(client, statefulSetsGVR)
	registerEviction(client)

	te := &testEnv{t: t, client: client, clock: newFakeClock(), state: newStateStore()}
	te.env = &dr.Env{
		Context: testContext,
		Client:  client,
		State:   te.state,
		Now:     te.clock.Now,
		Sleep:   te.clock.Sleep,
		Logf:    func(format string, args ...any) { te.logs = append(te.logs, fmt.Sprintf(format, args...)) },
	}
	return te
}

// registerScale teaches the fake clientset the scale subresource, which its tracker does not know.
func registerScale(client *fake.Clientset, gvr schema.GroupVersionResource) {
	replicasOf := func(obj runtime.Object) (name string, replicas int32, ok bool) {
		switch o := obj.(type) {
		case *appsv1.Deployment:
			if o.Spec.Replicas != nil {
				return o.Name, *o.Spec.Replicas, true
			}
			return o.Name, 1, true
		case *appsv1.StatefulSet:
			if o.Spec.Replicas != nil {
				return o.Name, *o.Spec.Replicas, true
			}
			return o.Name, 1, true
		}
		return "", 0, false
	}
	client.PrependReactor("get", gvr.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		obj, err := client.Tracker().Get(gvr, action.GetNamespace(), action.(k8stesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		name, replicas, _ := replicasOf(obj)
		return true, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: action.GetNamespace()},
			Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
		}, nil
	})
	client.PrependReactor("update", gvr.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		scale := action.(k8stesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
		obj, err := client.Tracker().Get(gvr, action.GetNamespace(), scale.Name)
		if err != nil {
			return true, nil, err
		}
		updated := obj.DeepCopyObject()
		switch o := updated.(type) {
		case *appsv1.Deployment:
			o.Spec.Replicas = &scale.Spec.Replicas
		case *appsv1.StatefulSet:
			o.Spec.Replicas = &scale.Spec.Replicas
		}
		if err := client.Tracker().Update(gvr, updated, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, scale, nil
	})
}

// registerEviction makes an eviction delete the pod, like a cluster without PodDisruptionBudgets.
func registerEviction(client *fake.Clientset) {
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		name := action.(k8stesting.CreateAction).GetObject().(interface{ GetName() string }).GetName()
		if err := client.Tracker().Delete(podsGVR, action.GetNamespace(), name); err != nil {
			return true, nil, err
		}
		return true, nil, nil
	})
}

// ---- object builders ---------------------------------------------------------------------------------------

func labelsFor(name string) map[string]string { return map[string]string{"app": name} }

func deploy(namespace, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labelsFor(name)}},
		Status:     appsv1.DeploymentStatus{Replicas: replicas, ReadyReplicas: replicas},
	}
}

func statefulSet(namespace, name string, replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labelsFor(name)}},
		Status:     appsv1.StatefulSetStatus{Replicas: replicas, ReadyReplicas: replicas},
	}
}

func cronJob(namespace, name string, suspended bool) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       batchv1.CronJobSpec{Schedule: "* * * * *", Suspend: &suspended},
	}
}

func node(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func controlPlane(name string) *corev1.Node {
	return node(name, map[string]string{"node-role.kubernetes.io/control-plane": ""})
}

func nodePod(namespace, name, nodeName string) *corev1.Pod {
	controller := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labelsFor(name),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: name + "-rs", Controller: &controller}}},
		Spec:   corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func testPlan(apps ...dr.App) *dr.Plan {
	return &dr.Plan{Version: dr.PlanVersion, Name: "test", Context: testContext, Apps: apps}
}

// ---- running steps like the engine does --------------------------------------------------------------------

func runSteps(ctx context.Context, steps []dr.Step) error {
	for _, step := range steps {
		done, err := step.Action.Done(ctx)
		if err != nil {
			return err
		}
		if !done {
			if err := step.Action.Apply(ctx); err != nil {
				return errors.Join(errors.New(step.Action.ID()), err)
			}
		}
		if step.Gate != nil {
			if err := step.Gate.Wait(ctx); err != nil {
				return errors.Join(errors.New(step.Action.ID()), err)
			}
		}
	}
	return nil
}

func ids(steps []dr.Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Action.ID())
	}
	return out
}

func (te *testEnv) replicas(kind, namespace, name string) int32 {
	te.t.Helper()
	o := ops{client: te.client}
	n, err := o.replicas(context.Background(), WorkloadRef{Kind: kind, Namespace: namespace, Name: name})
	if err != nil {
		te.t.Fatal(err)
	}
	return n
}

func (te *testEnv) annotation(kind, namespace, name string) (string, bool) {
	te.t.Helper()
	v, ok, err := ops{client: te.client}.annotation(context.Background(), WorkloadRef{Kind: kind, Namespace: namespace, Name: name})
	if err != nil {
		te.t.Fatal(err)
	}
	return v, ok
}

func apiForbidden() error {
	return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "", errors.New("not allowed"))
}
