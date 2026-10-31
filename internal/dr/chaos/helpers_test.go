package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

var epoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// fakeClock moves only when something sleeps.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock { return &fakeClock{now: epoch} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

type memState struct {
	mu   sync.Mutex
	docs map[string][]byte
}

func newMemState() *memState { return &memState{docs: map[string][]byte{}} }

func (s *memState) Load(_ context.Context, key string, into any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.docs[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, into)
}

func (s *memState) Save(_ context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.docs[key] = raw
	s.mu.Unlock()
	return nil
}

func (s *memState) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	delete(s.docs, key)
	s.mu.Unlock()
	return nil
}

func (s *memState) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.docs {
		out = append(out, k)
	}
	return out
}

type testEnv struct {
	env    *dr.Env
	client *fake.Clientset
	clock  *fakeClock
	state  *memState
	// refuseEviction, when it returns true for a pod, answers 429 like a PodDisruptionBudget would.
	refuseEviction func(namespace, name string) bool
	evicted        []string
}

const testContext = "kind-lab"

func newTestEnv(t *testing.T, objects ...runtime.Object) *testEnv {
	t.Helper()
	te := &testEnv{client: fake.NewSimpleClientset(objects...), clock: newClock(), state: newMemState()}
	te.client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := action.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		if te.refuseEviction != nil && te.refuseEviction(ev.Namespace, ev.Name) {
			return true, nil, apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 1)
		}
		pod, err := te.client.Tracker().Get(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, ev.Namespace, ev.Name)
		if err != nil {
			return true, nil, err
		}
		if ev.DeleteOptions != nil && ev.DeleteOptions.Preconditions != nil && ev.DeleteOptions.Preconditions.UID != nil &&
			*ev.DeleteOptions.Preconditions.UID != pod.(*corev1.Pod).UID {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, ev.Name, errors.New("uid mismatch"))
		}
		if err := te.client.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, ev.Namespace, ev.Name); err != nil {
			return true, nil, err
		}
		te.evicted = append(te.evicted, ev.Namespace+"/"+ev.Name)
		return true, nil, nil
	})
	te.env = &dr.Env{Context: testContext, Client: te.client, State: te.state, Now: te.clock.Now, Sleep: te.clock.Sleep}
	return te
}

func labPlan() *dr.Plan {
	return &dr.Plan{Version: 1, Name: "lab", Context: testContext, Safety: dr.Safety{LabOnly: true}}
}

func (te *testEnv) guard() Guard { return Guard{Plan: labPlan(), Env: te.env} }

func ns(name string, lbls map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls}}
}

func allowedNS(name string) *corev1.Namespace {
	return ns(name, map[string]string{LabelAllowed: "true"})
}

func int32p(v int32) *int32 { return &v }

func deployment(namespace, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: int32p(replicas), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: replicas},
	}
}

func statefulSet(namespace, name string, replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32p(replicas), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: replicas},
	}
}

func pod(namespace, name, app, nodeName string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	yes := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, UID: types.UID("uid-" + name), Labels: map[string]string{"app": app},
			// A controller owns every pod, like in a real cluster; kubectl drain refuses orphans.
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: app + "-rs", UID: "rs-" + types.UID(app), Controller: &yes}},
		},
		Spec: corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func pdb(namespace, name, app string, allowed int32) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}}},
		Status:     policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: allowed},
	}
}

func worker(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func controlPlane(name string) *corev1.Node {
	n := worker(name)
	n.Labels = map[string]string{"node-role.kubernetes.io/control-plane": ""}
	return n
}

// first returns the first pick: tests choose victims by name order.
func first(int) int { return 0 }

func threeReplicas(namespace string) []runtime.Object {
	return []runtime.Object{
		allowedNS(namespace),
		deployment(namespace, "api", 3),
		pod(namespace, "api-1", "api", "w1", true),
		pod(namespace, "api-2", "api", "w2", true),
		pod(namespace, "api-3", "api", "w1", true),
	}
}

func mustErrorIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v; want it to wrap %v", err, target)
	}
}
