package dbops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

var epoch = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// testEnv is a dr.Env wired to fakes, with a clock that only moves when something sleeps.
type testEnv struct {
	t      *testing.T
	env    *dr.Env
	dyn    *dynamicfake.FakeDynamicClient
	client *fake.Clientset
	state  *memState

	mu     sync.Mutex
	now    time.Time
	sleeps int
	// afterSleep runs after every poll, with the number of polls so far: it plays the operator or the
	// StatefulSet controller catching up with what the planner asked for.
	afterSleep func(n int)
}

func newTestEnv(t *testing.T, dynObjs []runtime.Object, k8sObjs ...runtime.Object) *testEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		cnpgGVR:       "ClusterList",
		cnpgBackupGVR: "BackupList",
	}, dynObjs...)
	client := fake.NewSimpleClientset(k8sObjs...)

	te := &testEnv{t: t, dyn: dyn, client: client, state: newMemState(), now: epoch}
	te.env = &dr.Env{
		Context: "lab",
		Client:  client,
		Dynamic: dyn,
		State:   te.state,
		Now:     te.clock,
		Logf:    t.Logf,
		Sleep: func(ctx context.Context, d time.Duration) error {
			te.mu.Lock()
			te.now = te.now.Add(d)
			te.sleeps++
			n := te.sleeps
			hook := te.afterSleep
			te.mu.Unlock()
			if hook != nil {
				hook(n)
			}
			return ctx.Err()
		},
	}
	return te
}

func (te *testEnv) clock() time.Time {
	te.mu.Lock()
	defer te.mu.Unlock()
	return te.now
}

func (te *testEnv) pollCount() int {
	te.mu.Lock()
	defer te.mu.Unlock()
	return te.sleeps
}

// memState is an in-memory dr.StateStore that round-trips through JSON like the real one.
type memState struct {
	mu       sync.Mutex
	docs     map[string][]byte
	failSave error
}

func newMemState() *memState { return &memState{docs: map[string][]byte{}} }

func (m *memState) Load(_ context.Context, key string, into any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.docs[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, into)
}

func (m *memState) Save(_ context.Context, key string, value any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSave != nil {
		return m.failSave
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m.docs[key] = raw
	return nil
}

func (m *memState) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.docs, key)
	return nil
}

func (m *memState) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.docs[key]
	return ok
}

// ---- CloudNativePG objects ----------------------------------------------------------------------------------

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func cond(conditionType, status, reason string, transition time.Time) map[string]any {
	return map[string]any{"type": conditionType, "status": status, "reason": reason, "lastTransitionTime": stamp(transition)}
}

// cnpgCluster is a healthy three-instance cluster; options turn it into other states.
func cnpgCluster(namespace, name string, options ...func(*unstructured.Unstructured)) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"spec":       map[string]any{"instances": int64(3)},
		"status": map[string]any{
			"instances":      int64(3),
			"readyInstances": int64(3),
			"phase":          "Cluster in healthy state",
			"conditions":     []any{cond("Ready", "True", "ClusterIsReady", epoch.Add(-48*time.Hour))},
		},
	}}
	for _, option := range options {
		option(obj)
	}
	return obj
}

func withAnnotation(key, value string) func(*unstructured.Unstructured) {
	return func(o *unstructured.Unstructured) {
		annotations := o.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[key] = value
		o.SetAnnotations(annotations)
	}
}

func withStatus(field string, value any) func(*unstructured.Unstructured) {
	return func(o *unstructured.Unstructured) { _ = unstructured.SetNestedField(o.Object, value, "status", field) }
}

func withConditions(list ...map[string]any) func(*unstructured.Unstructured) {
	return func(o *unstructured.Unstructured) {
		items := make([]any, 0, len(list))
		for _, c := range list {
			items = append(items, c)
		}
		_ = unstructured.SetNestedSlice(o.Object, items, "status", "conditions")
	}
}

func withLastBackup(when time.Time) func(*unstructured.Unstructured) {
	return withStatus("lastSuccessfulBackup", stamp(when))
}

func withBackupByMethod(method string, when time.Time) func(*unstructured.Unstructured) {
	return func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(o.Object, stamp(when), "status", "lastSuccessfulBackupByMethod", method)
	}
}

func withArchiveTimeout(value string) func(*unstructured.Unstructured) {
	return func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(o.Object, value, "spec", "postgresql", "parameters", "archive_timeout")
	}
}

// hibernated turns the cluster into what the operator reports after a hibernation.
func hibernated(o *unstructured.Unstructured) {
	withAnnotation(hibernationAnnotation, "on")(o)
	withStatus("instances", int64(0))(o)
	withStatus("readyInstances", int64(0))(o)
	withStatus("phase", "Cluster hibernated")(o)
	withConditions(
		cond("Ready", "False", "ClusterIsNotReady", epoch),
		cond(hibernationCondition, "True", "Hibernated", epoch),
	)(o)
}

// backupObject is a CloudNativePG Backup of a cluster.
func backupObject(namespace, name, cluster, phase string, stopped time.Time) *unstructured.Unstructured {
	status := map[string]any{"phase": phase}
	if !stopped.IsZero() {
		status["stoppedAt"] = stamp(stopped)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Backup",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"spec":       map[string]any{"cluster": map[string]any{"name": cluster}},
		"status":     status,
	}}
}

// mutateCluster plays the operator: read, change and write the Cluster back.
func (te *testEnv) mutateCluster(namespace, name string, change func(*unstructured.Unstructured)) {
	te.t.Helper()
	resource := te.dyn.Resource(cnpgGVR).Namespace(namespace)
	obj, err := resource.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		te.t.Fatalf("mutateCluster get: %v", err)
	}
	change(obj)
	if _, err := resource.Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
		te.t.Fatalf("mutateCluster update: %v", err)
	}
}

func (te *testEnv) cluster(namespace, name string) *unstructured.Unstructured {
	te.t.Helper()
	obj, err := te.dyn.Resource(cnpgGVR).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		te.t.Fatalf("get cluster: %v", err)
	}
	return obj
}

// ---- StatefulSets ---------------------------------------------------------------------------------------------

func statefulSet(namespace, name string, replicas, ready int32, annotations map[string]string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Annotations: annotations},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
		Status:     appsv1.StatefulSetStatus{Replicas: replicas, ReadyReplicas: ready},
	}
}

// settle plays the StatefulSet controller: status follows spec.
func (te *testEnv) settle(namespace, name string) {
	te.t.Helper()
	sets := te.client.AppsV1().StatefulSets(namespace)
	sts, err := sets.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		te.t.Fatalf("settle get: %v", err)
	}
	sts.Status.Replicas = *sts.Spec.Replicas
	sts.Status.ReadyReplicas = *sts.Spec.Replicas
	if _, err := sets.Update(context.Background(), sts, metav1.UpdateOptions{}); err != nil {
		te.t.Fatalf("settle update: %v", err)
	}
}

func (te *testEnv) sts(namespace, name string) *appsv1.StatefulSet {
	te.t.Helper()
	sts, err := te.client.AppsV1().StatefulSets(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		te.t.Fatalf("get statefulset: %v", err)
	}
	return sts
}

// runningPod is an instance pod that is Running and Ready.
func runningPod(namespace, name, cluster string) *corev1.Pod {
	pod := instancePod(namespace, name, cluster)
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	return pod
}

// startPods plays the operator creating the instance pods of a cluster.
func (te *testEnv) startPods(namespace, cluster string, n int) {
	te.t.Helper()
	for i := 1; i <= n; i++ {
		pod := runningPod(namespace, fmt.Sprintf("%s-%d", cluster, i), cluster)
		if _, err := te.client.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			te.t.Fatalf("startPods: %v", err)
		}
	}
}

func withoutStatus(fields ...string) func(*unstructured.Unstructured) {
	return func(o *unstructured.Unstructured) {
		for _, f := range fields {
			unstructured.RemoveNestedField(o.Object, "status", f)
		}
	}
}

func instancePod(namespace, name, cluster string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: map[string]string{cnpgClusterLabel: cluster}}}
}

// ---- running steps --------------------------------------------------------------------------------------------

// runStep does what the engine does for one step: skip it when Done, else apply and pass the gate.
func runStep(ctx context.Context, step dr.Step) (skipped bool, err error) {
	done, err := step.Action.Done(ctx)
	if err != nil {
		return false, err
	}
	if done {
		return true, nil
	}
	if err := step.Action.Apply(ctx); err != nil {
		return false, err
	}
	if step.Gate != nil {
		return false, step.Gate.Wait(ctx)
	}
	return false, nil
}

func stepByID(t *testing.T, steps []dr.Step, id string) dr.Step {
	t.Helper()
	for _, step := range steps {
		if step.Action.ID() == id {
			return step
		}
	}
	t.Fatalf("no step %q in %v", id, stepIDs(steps))
	return dr.Step{}
}

func stepIDs(steps []dr.Step) []string {
	ids := make([]string, 0, len(steps))
	for _, step := range steps {
		ids = append(ids, step.Action.ID())
	}
	return ids
}

func planWith(databases ...dr.Database) *dr.Plan {
	return &dr.Plan{Version: dr.PlanVersion, Name: "test", Context: "lab", Databases: databases}
}

func cnpgDB(name string, tier int, maxAge string) dr.Database {
	return dr.Database{Name: name, Namespace: "shop", Kind: KindCNPG, Ref: name, Tier: tier, MaxBackupAge: maxAge}
}

func stsDB(name string, tier int, maxAge string) dr.Database {
	return dr.Database{Name: name, Namespace: "shop", Kind: KindStatefulSet, Ref: name, Tier: tier, MaxBackupAge: maxAge}
}

func finderAt(when time.Time) BackupFinder {
	return func(context.Context, *dr.Env, string) (time.Time, bool, error) { return when, true, nil }
}

func finderNone() BackupFinder {
	return func(context.Context, *dr.Env, string) (time.Time, bool, error) { return time.Time{}, false, nil }
}

func finderError(err error) BackupFinder {
	return func(context.Context, *dr.Env, string) (time.Time, bool, error) { return time.Time{}, false, err }
}

var errBoom = errors.New("boom")

// countVerb counts the recorded API actions of one verb.
func countVerb(actions []k8stesting.Action, verb string) int {
	n := 0
	for _, a := range actions {
		if a.GetVerb() == verb {
			n++
		}
	}
	return n
}
