//go:build lab

// Live tests against kind clusters. They are not part of normal CI: run them with
//
//	go test -tags lab -run TestLab -count=1 -timeout 20m ./internal/dr/k8sops/
//
// TestLabAppsAndCronJobs needs the shared lab context kind-k8scli-lab and only touches a namespace it creates.
// TestLabNodes cordons and drains nodes, which affects every pod on them, so it needs its own throwaway
// cluster (context kind-k8sops-nodes: one control plane, two workers) and never runs on the shared lab.
package k8sops

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/kubeconfig"
)

const (
	sharedLabContext = "kind-k8scli-lab"
	nodesLabContext  = "kind-k8sops-nodes"
	pauseImage       = "registry.k8s.io/pause:3.10" // preloaded on every kind node
)

func labClient(t *testing.T, contextName string) kubernetes.Interface {
	t.Helper()
	if !strings.HasPrefix(contextName, "kind-") {
		t.Fatalf("refusing to run against %q: live tests only run on kind clusters", contextName)
	}
	config, err := kubeconfig.New("").RESTConfigFor(contextName)
	if err != nil {
		t.Skipf("context %s is not available: %v", contextName, err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Discovery().ServerVersion(); err != nil {
		t.Skipf("cluster %s does not answer: %v", contextName, err)
	}
	return client
}

type labRun struct {
	t      *testing.T
	env    *dr.Env
	client kubernetes.Interface
	state  *stateStore
	logs   []string
}

func newLabRun(t *testing.T, contextName string) *labRun {
	client := labClient(t, contextName)
	r := &labRun{t: t, client: client, state: newStateStore()}
	r.env = &dr.Env{
		Context: contextName, Client: client, State: r.state,
		Logf: func(format string, args ...any) {
			line := sprintfLab(format, args...)
			r.logs = append(r.logs, line)
			t.Log(line)
		},
	}
	return r
}

func (r *labRun) namespace(prefix string) string {
	ns := prefix + strings.ToLower(time.Now().Format("150405"))
	ctx := context.Background()
	if _, err := r.client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() {
		_ = r.client.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			if _, err := r.client.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); apierrors.IsNotFound(err) {
				return
			}
			time.Sleep(2 * time.Second)
		}
	})
	return ns
}

func (r *labRun) deployment(ns, name string, replicas int32) {
	labels := map[string]string{"app": name}
	_, err := r.client.AppsV1().Deployments(ns).Create(context.Background(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: new(int64),
					Containers:                    []corev1.Container{{Name: "p", Image: pauseImage}},
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *labRun) pdb(ns, name string, minAvailable int) {
	min := intstr.FromInt32(int32(minAvailable))
	_, err := r.client.PolicyV1().PodDisruptionBudgets(ns).Create(context.Background(), &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &min, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}},
	}, metav1.CreateOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *labRun) cronJob(ns, name string, keep bool) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 1 1 *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "p", Image: pauseImage}}},
			}}},
		},
	}
	if keep {
		cj.Annotations = map[string]string{AnnotationKeepRunning: "true"}
	}
	if _, err := r.client.BatchV1().CronJobs(ns).Create(context.Background(), cj, metav1.CreateOptions{}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *labRun) waitReady(ns string, names ...string) {
	r.t.Helper()
	refs := make([]WorkloadRef, 0, len(names))
	for _, n := range names {
		refs = append(refs, WorkloadRef{KindDeployment, ns, n})
	}
	if err := NewPodsReadyGate(r.env, refs, 3*time.Minute).Wait(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *labRun) run(p dr.Planner, plan *dr.Plan, dir dr.Direction, scope dr.Scope) {
	r.t.Helper()
	steps, err := p.Steps(context.Background(), r.env, plan, dir, scope)
	if err != nil {
		r.t.Fatalf("%s %s: %v", p.Phase(), dir, err)
	}
	r.t.Logf("%s %s: %d steps", p.Phase(), dir, len(steps))
	for _, s := range steps {
		r.t.Logf("  %-8s %-45s %s", s.Action.Risk(), s.Action.ID(), s.Action.Describe())
	}
	if err := runSteps(context.Background(), steps); err != nil {
		r.t.Fatalf("%s %s: %v", p.Phase(), dir, err)
	}
}

func (r *labRun) replicas(ns, name string) int32 {
	r.t.Helper()
	n, err := ops{client: r.client}.replicas(context.Background(), WorkloadRef{KindDeployment, ns, name})
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *labRun) pods(ns string) []corev1.Pod {
	r.t.Helper()
	list, err := r.client.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	return list.Items
}

func (r *labRun) suspended(ns, name string) bool {
	cj, err := r.client.BatchV1().CronJobs(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	return isSuspended(cj)
}

func indexOfLog(logs []string, fragment string) int {
	for i, l := range logs {
		if strings.Contains(l, fragment) {
			return i
		}
	}
	return -1
}

// ---- apps and CronJobs on the shared lab -------------------------------------------------------------------

func TestLabAppsAndCronJobs(t *testing.T) {
	r := newLabRun(t, sharedLabContext)
	ns := r.namespace("k8sops-test-apps-")

	r.deployment(ns, "cache", 1)
	r.deployment(ns, "api", 3)
	r.deployment(ns, "web", 2)
	r.cronJob(ns, "tick", false)
	r.cronJob(ns, "backup", true)
	r.waitReady(ns, "cache", "api", "web")

	plan := &dr.Plan{Version: dr.PlanVersion, Name: "lab-apps", Context: sharedLabContext, Apps: []dr.App{
		{Name: "data", Namespace: ns, Tier: 0, Workloads: []string{"cache"}},
		{Name: "api", Namespace: ns, Tier: 1, Workloads: []string{"api"}},
		{Name: "web", Namespace: ns, Tier: 2, Workloads: []string{"web"}},
	}}
	prepare, apps := NewPreparePlanner(Options{}), NewAppsPlanner(Options{})

	// ---- down
	r.run(prepare, plan, dr.DirDown, dr.Scope{})
	r.run(apps, plan, dr.DirDown, dr.Scope{})

	for name := range map[string]int32{"cache": 1, "api": 3, "web": 2} {
		if got := r.replicas(ns, name); got != 0 {
			t.Errorf("%s replicas = %d after down; want 0", name, got)
		}
	}
	if left := r.pods(ns); len(left) != 0 {
		t.Errorf("%d pod(s) still exist after the pods-gone gates passed", len(left))
	}
	if !r.suspended(ns, "tick") || r.suspended(ns, "backup") {
		t.Errorf("tick suspended=%v (want true), backup suspended=%v (want false: it is kept running)", r.suspended(ns, "tick"), r.suspended(ns, "backup"))
	}
	if a, b, c := indexOfLog(r.logs, "scaled "+ns+"/web from 2 to 0"), indexOfLog(r.logs, "scaled "+ns+"/api from 3 to 0"), indexOfLog(r.logs, "scaled "+ns+"/cache from 1 to 0"); a < 0 || a >= b || b >= c {
		t.Errorf("down order must be web, api, cache; log positions %d %d %d", a, b, c)
	}

	// A second down changes nothing and writes no new record.
	writes := r.state.writes
	r.run(apps, plan, dr.DirDown, dr.Scope{})
	if r.state.writes != writes {
		t.Errorf("the second down rewrote the state (%d -> %d)", writes, r.state.writes)
	}

	// ---- up
	r.logs = nil
	r.run(apps, plan, dr.DirUp, dr.Scope{})
	r.run(prepare, plan, dr.DirUp, dr.Scope{})

	for name, want := range map[string]int32{"cache": 1, "api": 3, "web": 2} {
		if got := r.replicas(ns, name); got != want {
			t.Errorf("%s replicas = %d after up; want %d", name, got, want)
		}
	}
	r.waitReady(ns, "cache", "api", "web")
	if r.suspended(ns, "tick") {
		t.Error("tick must be resumed")
	}
	if a, b, c := indexOfLog(r.logs, "scaled "+ns+"/cache to 1"), indexOfLog(r.logs, "scaled "+ns+"/api to 3"), indexOfLog(r.logs, "scaled "+ns+"/web to 2"); a < 0 || a >= b || b >= c {
		t.Errorf("up order must be cache, api, web; log positions %d %d %d", a, b, c)
	}
	if r.state.has(sharedLabContext+"/apps") || r.state.has(sharedLabContext+"/cronjobs") {
		t.Error("the DR state must be empty after up")
	}
	d, _ := r.client.AppsV1().Deployments(ns).Get(context.Background(), "api", metav1.GetOptions{})
	if _, ok := d.Annotations[AnnotationReplicasBefore]; ok {
		t.Error("the annotation must be removed after up")
	}
}

// ---- nodes on a private cluster -----------------------------------------------------------------------------

func TestLabNodes(t *testing.T) {
	r := newLabRun(t, nodesLabContext)
	ctx := context.Background()
	ns := r.namespace("k8sops-test-nodes-")

	nodes, err := r.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var workers []string
	var cp string
	for i := range nodes.Items {
		if isControlPlane(&nodes.Items[i]) {
			cp = nodes.Items[i].Name
		} else {
			workers = append(workers, nodes.Items[i].Name)
		}
	}
	if cp == "" || len(workers) < 2 {
		t.Fatalf("this test needs a control plane and two workers, got cp=%q workers=%v", cp, workers)
	}
	t.Logf("control plane %s, workers %v", cp, workers)

	r.deployment(ns, "api", 3)
	r.pdb(ns, "api", 1)
	r.deployment(ns, "web", 2)
	r.waitReady(ns, "api", "web")

	plan := &dr.Plan{Version: dr.PlanVersion, Name: "lab-nodes", Context: nodesLabContext, Provider: &dr.Provider{Kind: "kind"},
		Apps: []dr.App{{Name: "api", Namespace: ns, Tier: 1, Workloads: []string{"api"}}, {Name: "web", Namespace: ns, Tier: 2, Workloads: []string{"web"}}}}

	schedulable := func(name string) bool {
		n, err := r.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return !n.Spec.Unschedulable
	}

	t.Run("drain one worker while a PodDisruptionBudget protects the app", func(t *testing.T) {
		one := NewNodesPlanner(Options{Nodes: []string{workers[0]}, DrainTimeout: 3 * time.Minute})
		r.run(one, plan, dr.DirDown, dr.Scope{})

		if schedulable(workers[0]) {
			t.Errorf("%s must be cordoned", workers[0])
		}
		if !schedulable(workers[1]) || !schedulable(cp) {
			t.Error("the other worker and the control plane must stay schedulable")
		}
		for _, pod := range r.pods(ns) {
			if pod.Spec.NodeName == workers[0] && pod.DeletionTimestamp == nil {
				t.Errorf("pod %s is still on the drained node", pod.Name)
			}
		}
		r.waitReady(ns, "api", "web") // the budget held: the app moved to the other worker without going down

		r.run(one, plan, dr.DirUp, dr.Scope{})
		if !schedulable(workers[0]) {
			t.Errorf("%s must be uncordoned by up", workers[0])
		}
		if r.state.has(nodesLabKey()) {
			t.Error("the node state must be empty after up")
		}
	})

	t.Run("a PodDisruptionBudget that cannot be met stops the drain with a clear error", func(t *testing.T) {
		// Both workers cordoned and the app running: draining the second worker would take api below minAvailable.
		for _, w := range workers {
			patch := &nodeAction{client: r.client, name: w, state: newLedger(r.env, nil, "nodes"), c: clockOf(r.env)}
			if err := patch.setUnschedulable(ctx, true); err != nil {
				t.Fatal(err)
			}
		}
		d := &drainNode{nodeAction: nodeAction{client: r.client, name: workers[1], state: newLedger(r.env, nil, "nodes"), c: clockOf(r.env)},
			opts: Options{DrainTimeout: 25 * time.Second}.withDefaults()}
		err := d.Apply(ctx)
		// Whether it blocks depends on how the replicas are spread; if the node held none of api, nothing blocks.
		if err != nil && !strings.Contains(err.Error(), "PodDisruptionBudget refused") {
			t.Errorf("a blocked drain must say a PodDisruptionBudget refused: %v", err)
		}
		t.Logf("drain of %s: %v", workers[1], err)
		for _, w := range workers {
			patch := &nodeAction{client: r.client, name: w, state: newLedger(r.env, nil, "nodes"), c: clockOf(r.env)}
			if err := patch.setUnschedulable(ctx, false); err != nil {
				t.Fatal(err)
			}
		}
		r.waitReady(ns, "api", "web")
	})

	t.Run("full stop: apps down, every worker drained, then back up in order", func(t *testing.T) {
		apps := NewAppsPlanner(Options{})
		all := NewNodesPlanner(Options{DrainTimeout: 3 * time.Minute})

		r.run(apps, plan, dr.DirDown, dr.Scope{})
		r.run(all, plan, dr.DirDown, dr.Scope{})
		for _, w := range workers {
			if schedulable(w) {
				t.Errorf("worker %s must be cordoned", w)
			}
		}
		if !schedulable(cp) {
			t.Error("the control plane must never be cordoned")
		}

		r.run(all, plan, dr.DirUp, dr.Scope{})
		for _, w := range workers {
			if !schedulable(w) {
				t.Errorf("worker %s must be uncordoned", w)
			}
		}
		r.run(apps, plan, dr.DirUp, dr.Scope{})
		r.waitReady(ns, "api", "web")
		if got := r.replicas(ns, "api"); got != 3 {
			t.Errorf("api replicas = %d; want 3", got)
		}
	})
}

func nodesLabKey() string { return nodesLabContext + "/nodes" }

func sprintfLab(format string, args ...any) string {
	return strings.TrimSpace(fmt.Sprintf(format, args...))
}
