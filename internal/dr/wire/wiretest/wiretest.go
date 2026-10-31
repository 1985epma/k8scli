// Package wiretest provides fakes for tests of anything that sits on top of the wire package (the
// CLI commands, the web API): a fake cluster whose Deployments and StatefulSets really scale, a fake
// command runner and a Stack wired to them. It is only imported by tests.
package wiretest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr/wire"
)

var (
	deploymentsGVR  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	statefulSetsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	podsGVR         = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
)

// Cluster is a fake cluster: a typed clientset plus a dynamic one that knows the CRDs DR reads
// (Velero, CSI snapshots, CloudNativePG) so listing them returns nothing instead of failing.
type Cluster struct {
	Client  *fake.Clientset
	Dynamic *dynamicfake.FakeDynamicClient
}

// NewCluster builds a fake cluster with the given typed objects. Scaling a Deployment or StatefulSet
// through the scale subresource updates the object, and evicting a pod deletes it.
func NewCluster(objects ...runtime.Object) *Cluster {
	client := fake.NewSimpleClientset(objects...)
	registerScale(client, deploymentsGVR)
	registerScale(client, statefulSetsGVR)
	registerEviction(client)

	scheme := runtime.NewScheme()
	kinds := map[schema.GroupVersionResource]string{
		{Group: "velero.io", Version: "v1", Resource: "backups"}:                              "BackupList",
		{Group: "velero.io", Version: "v1", Resource: "schedules"}:                            "ScheduleList",
		{Group: "velero.io", Version: "v1", Resource: "backupstoragelocations"}:               "BackupStorageLocationList",
		{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}:        "VolumeSnapshotList",
		{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
		{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}:                    "ClusterList",
		{Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups"}:                     "BackupList",
		{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotclasses"}:  "VolumeSnapshotClassList",
	}
	return &Cluster{Client: client, Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, kinds)}
}

// Replicas returns the desired replicas of a "deployment" or "statefulset", or -1 when it is missing.
func (c *Cluster) Replicas(kind, namespace, name string) int32 {
	ctx := context.Background()
	switch strings.ToLower(kind) {
	case "deployment":
		d, err := c.Client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil || d.Spec.Replicas == nil {
			return -1
		}
		return *d.Spec.Replicas
	case "statefulset":
		s, err := c.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil || s.Spec.Replicas == nil {
			return -1
		}
		return *s.Spec.Replicas
	}
	return -1
}

func registerScale(client *fake.Clientset, gvr schema.GroupVersionResource) {
	replicasOf := func(obj runtime.Object) (string, int32) {
		switch o := obj.(type) {
		case *appsv1.Deployment:
			if o.Spec.Replicas != nil {
				return o.Name, *o.Spec.Replicas
			}
			return o.Name, 1
		case *appsv1.StatefulSet:
			if o.Spec.Replicas != nil {
				return o.Name, *o.Spec.Replicas
			}
			return o.Name, 1
		}
		return "", 0
	}
	client.PrependReactor("get", gvr.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		obj, err := client.Tracker().Get(gvr, action.GetNamespace(), action.(k8stesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		name, replicas := replicasOf(obj)
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

// Deployment builds a healthy Deployment: its status says every replica is ready.
func Deployment(namespace, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labelsFor(name)}},
		Status:     appsv1.DeploymentStatus{Replicas: replicas, ReadyReplicas: replicas},
	}
}

// StatefulSet builds a healthy StatefulSet.
func StatefulSet(namespace, name string, replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labelsFor(name)}},
		Status:     appsv1.StatefulSetStatus{Replicas: replicas, ReadyReplicas: replicas},
	}
}

// Node builds a Ready worker node.
func Node(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

// ControlPlane builds a Ready control-plane node.
func ControlPlane(name string) *corev1.Node {
	n := Node(name)
	n.Labels = map[string]string{"node-role.kubernetes.io/control-plane": ""}
	return n
}

// Namespace builds a Namespace object (the diff compares namespaces).
func Namespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// Secret builds a Secret with one value, for tests that prove values never leave the cluster.
func Secret(namespace, name, key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

// ---- fake command runner -----------------------------------------------------------------------------------

// Exec records the commands it is asked to run and answers from a script. A command with no scripted
// answer succeeds with empty output.
type Exec struct {
	mu    sync.Mutex
	calls []string
	// Answers maps a command line ("docker stop x") or a prefix ending in "*" ("docker ps*") to its output.
	Answers map[string]string
	// Failures maps a command line or prefix to an error message.
	Failures map[string]string
}

// Run implements dr.Exec.
func (e *Exec) Run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	e.mu.Lock()
	e.calls = append(e.calls, line)
	e.mu.Unlock()
	if msg, ok := lookup(e.Failures, line); ok {
		return "", fmt.Errorf("%s", msg)
	}
	out, _ := lookup(e.Answers, line)
	return out, nil
}

// Calls returns every command line run so far.
func (e *Exec) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func lookup(table map[string]string, line string) (string, bool) {
	if v, ok := table[line]; ok {
		return v, true
	}
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasSuffix(k, "*") && strings.HasPrefix(line, strings.TrimSuffix(k, "*")) {
			return table[k], true
		}
	}
	return "", false
}

// HTTP is a fake health checker: it answers the same status for every URL.
type HTTP struct{ Status int }

// Check implements dr.HTTPChecker.
func (h HTTP) Check(context.Context, string) (int, error) {
	if h.Status == 0 {
		return 200, nil
	}
	return h.Status, nil
}

// ---- stack -------------------------------------------------------------------------------------------------

// NewStack returns a Stack wired to fake clusters, keyed by context name, with a temporary data
// directory, no waiting between polls and the given fake command runner (nil means a fresh Exec). Options
// not set by the caller get these test defaults; Clients is always replaced.
func NewStack(t testing.TB, opts wire.Options, clusters map[string]*Cluster, exec *Exec) *wire.Stack {
	t.Helper()
	if exec == nil {
		exec = &Exec{}
	}
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.Kubeconfig == "" {
		// Never read the developer's real kubeconfig from a test.
		opts.Kubeconfig = Kubeconfig(t, "")
	}
	if opts.Actor == "" {
		opts.Actor = "tester"
	}
	if opts.Exec == nil {
		opts.Exec = exec
	}
	if opts.HTTP == nil {
		opts.HTTP = HTTP{}
	}
	if opts.Now == nil {
		opts.Now = newStepClock().Now
	}
	if opts.Sleep == nil {
		// No real waiting in tests, but a cancelled context still stops a run.
		opts.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	}
	opts.Clients = func(contextName string) (kubernetes.Interface, dynamic.Interface, error) {
		c, ok := clusters[contextName]
		if !ok {
			return nil, nil, fmt.Errorf("context %q not found", contextName)
		}
		return c.Client, c.Dynamic, nil
	}
	stack, err := wire.New(opts)
	if err != nil {
		t.Fatalf("wiretest: %v", err)
	}
	return stack
}

// stepClock is a clock that moves one second forward every time it is read, so two runs started
// back to back have different times (and sortable ids) and tests do not depend on the machine's speed.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func newStepClock() *stepClock {
	return &stepClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

// Now returns the current fake time and advances it by one second.
func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

// Kubeconfig writes a kubeconfig to a temporary file and returns its path. current is the current
// context ("" for none); contexts are the other context names.
func Kubeconfig(t testing.TB, current string, contexts ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\n")
	if current != "" {
		b.WriteString("current-context: " + current + "\n")
		contexts = append([]string{current}, contexts...)
	}
	b.WriteString("clusters:\n- name: c\n  cluster: {server: \"https://127.0.0.1:1\"}\nusers:\n- name: u\n  user: {token: x}\ncontexts:\n")
	for _, name := range contexts {
		b.WriteString("- name: " + name + "\n  context: {cluster: c, user: u}\n")
	}
	if len(contexts) == 0 {
		b.Reset()
		b.WriteString("apiVersion: v1\nkind: Config\n")
	}
	file := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("wiretest: %v", err)
	}
	return file
}
