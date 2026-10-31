package chaos

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/1985epma/k8scli/internal/dr"
)

func plan(t *testing.T, te *testEnv, opts KillPodsOptions) (*KillPods, error) {
	t.Helper()
	if opts.Pick == nil {
		opts.Pick = first
	}
	return NewKillPods(context.Background(), te.guard(), opts)
}

func TestKillPodsPicksReadyPodsWithinTheBlastRadius(t *testing.T) {
	te := newTestEnv(t, threeReplicas("chaos-test")...)
	a, err := plan(t, te, KillPodsOptions{Namespace: "chaos-test", Workload: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Targets(); len(got) != 1 || got[0] != "api-1" {
		t.Errorf("targets = %v; want [api-1] (first pick over the sorted pool)", got)
	}
	if a.Risk() != dr.RiskHigh || a.Recovery() != nil {
		t.Errorf("risk %v, recovery %v", a.Risk(), a.Recovery())
	}
	for _, want := range []string{"api-1", "Eviction API", "3 replicas, 3 ready", "at least one stays ready"} {
		if !strings.Contains(a.Describe(), want) {
			t.Errorf("Describe() missing %q: %s", want, a.Describe())
		}
	}
	if got := a.Equivalent(); got != "kubectl delete pod api-1 -n chaos-test" {
		t.Errorf("Equivalent() = %q", got)
	}
	if a.ID() != "chaos/chaos-test/api:kill-pods" || !strings.Contains(a.Title(), "1 pod") {
		t.Errorf("ID %q title %q", a.ID(), a.Title())
	}
	// Planning reads only.
	if len(te.evicted) != 0 {
		t.Errorf("planning evicted %v", te.evicted)
	}
}

func TestKillPodsHonorsTheMaxPercent(t *testing.T) {
	te := newTestEnv(t, threeReplicas("chaos-test")...)
	// 3 replicas at 50% => 1 may go.
	_, err := plan(t, te, KillPodsOptions{Namespace: "chaos-test", Workload: "api", Count: 2})
	mustErrorIs(t, err, ErrBlastRadius)
	if !strings.Contains(err.Error(), "at most 1") {
		t.Errorf("the refusal should state the limit: %v", err)
	}
	// 67% of 3 = 2.
	a, err := plan(t, te, KillPodsOptions{Namespace: "chaos-test", Workload: "api", Count: 2, MaxPercent: 67})
	if err != nil {
		t.Fatalf("two of three at 67%% must be allowed: %v", err)
	}
	if len(a.Targets()) != 2 || a.Targets()[0] == a.Targets()[1] {
		t.Errorf("two distinct pods expected, got %v", a.Targets())
	}
}

func TestKillPodsNeverLeavesNoReadyReplica(t *testing.T) {
	// Even at 100%, one ready pod must stay.
	te := newTestEnv(t, threeReplicas("chaos-test")...)
	_, err := plan(t, te, KillPodsOptions{Namespace: "chaos-test", Workload: "api", Count: 3, MaxPercent: 100})
	mustErrorIs(t, err, ErrBlastRadius)
	if !strings.Contains(err.Error(), "no ready replica") {
		t.Errorf("unexpected message: %v", err)
	}

	// A single replica can never be killed at the default percentage.
	te = newTestEnv(t, allowedNS("one"), deployment("one", "solo", 1), pod("one", "solo-1", "solo", "w1", true))
	_, err = plan(t, te, KillPodsOptions{Namespace: "one", Workload: "solo"})
	mustErrorIs(t, err, ErrBlastRadius)

	// Three replicas, but only one of them ready: killing it would leave none.
	te = newTestEnv(t, allowedNS("ns"), deployment("ns", "api", 3),
		pod("ns", "api-1", "api", "w1", true), pod("ns", "api-2", "api", "w1", false), pod("ns", "api-3", "api", "w1", false))
	_, err = plan(t, te, KillPodsOptions{Namespace: "ns", Workload: "api"})
	mustErrorIs(t, err, ErrBlastRadius)
}

func TestKillPodsIgnoresPodsThatAreNotReadyOrAreTerminating(t *testing.T) {
	terminating := pod("ns", "api-4", "api", "w1", true)
	deleted := metav1.NewTime(epoch)
	terminating.DeletionTimestamp = &deleted
	te := newTestEnv(t, allowedNS("ns"), deployment("ns", "api", 5),
		pod("ns", "api-1", "api", "w1", false), pod("ns", "api-2", "api", "w1", true),
		pod("ns", "api-3", "api", "w1", true), terminating, pod("ns", "api-5", "api", "w1", true),
		pod("ns", "other-1", "other", "w1", true))
	a, err := plan(t, te, KillPodsOptions{Namespace: "ns", Workload: "api", Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range a.Targets() {
		if name == "api-1" || name == "api-4" || name == "other-1" {
			t.Errorf("%s is not a ready pod of the workload and must never be a target (targets %v)", name, a.Targets())
		}
	}
	if len(a.Targets()) != 2 {
		t.Errorf("targets = %v", a.Targets())
	}
}

func TestKillPodsPodDisruptionBudgets(t *testing.T) {
	objects := append(threeReplicas("chaos-test"), pdb("chaos-test", "api-pdb", "api", 0))
	te := newTestEnv(t, objects...)
	_, err := plan(t, te, KillPodsOptions{Namespace: "chaos-test", Workload: "api"})
	mustErrorIs(t, err, ErrDisruptionBudget)
	if !strings.Contains(err.Error(), "api-pdb") {
		t.Errorf("the budget should be named: %v", err)
	}

	objects = append(threeReplicas("chaos-test"), pdb("chaos-test", "api-pdb", "api", 1), pdb("chaos-test", "unrelated", "web", 0))
	te = newTestEnv(t, objects...)
	a, err := plan(t, te, KillPodsOptions{Namespace: "chaos-test", Workload: "api"})
	if err != nil {
		t.Fatalf("a budget with room must allow it: %v", err)
	}
	if !strings.Contains(a.Describe(), "PodDisruptionBudget api-pdb is honored") || strings.Contains(a.Describe(), "unrelated") {
		t.Errorf("only the covering budget should be mentioned: %s", a.Describe())
	}
}

func TestKillPodsRefusesUnlabelledAndSystemNamespaces(t *testing.T) {
	te := newTestEnv(t, ns("shop", nil), deployment("shop", "api", 3))
	_, err := plan(t, te, KillPodsOptions{Namespace: "shop", Workload: "api"})
	mustErrorIs(t, err, ErrNamespaceNotAllowed)
	_, err = plan(t, te, KillPodsOptions{Namespace: "kube-system", Workload: "coredns"})
	mustErrorIs(t, err, ErrNamespaceNotAllowed)

	te.env.Client = nil
	_, err = NewKillPods(context.Background(), Guard{Plan: labPlan(), Env: te.env}, KillPodsOptions{Namespace: "shop", Workload: "api"})
	mustErrorIs(t, err, dr.ErrUnreachable)

	notLab := newTestEnv(t, threeReplicas("chaos-test")...)
	p := labPlan()
	p.Safety.LabOnly = false
	_, err = NewKillPods(context.Background(), Guard{Plan: p, Env: notLab.env}, KillPodsOptions{Namespace: "chaos-test", Workload: "api"})
	mustErrorIs(t, err, ErrNotLab)
}

func TestKillPodsValidatesTheRequest(t *testing.T) {
	te := newTestEnv(t, threeReplicas("chaos-test")...)
	for name, opts := range map[string]KillPodsOptions{
		"bad kind":         {Namespace: "chaos-test", Workload: "api", Kind: "DaemonSet"},
		"no workload":      {Namespace: "chaos-test"},
		"negative count":   {Namespace: "chaos-test", Workload: "api", Count: -1},
		"huge count":       {Namespace: "chaos-test", Workload: "api", Count: MaxKill + 1},
		"percent over":     {Namespace: "chaos-test", Workload: "api", MaxPercent: 101},
		"percent below":    {Namespace: "chaos-test", Workload: "api", MaxPercent: -5},
		"unknown workload": {Namespace: "chaos-test", Workload: "ghost"},
	} {
		if _, err := plan(t, te, opts); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

func TestKillPodsStatefulSet(t *testing.T) {
	te := newTestEnv(t, allowedNS("db"), statefulSet("db", "redis", 3),
		pod("db", "redis-0", "redis", "w1", true), pod("db", "redis-1", "redis", "w2", true), pod("db", "redis-2", "redis", "w1", true))
	a, err := plan(t, te, KillPodsOptions{Namespace: "db", Kind: "StatefulSet", Workload: "redis"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(te.evicted) != 1 || te.evicted[0] != "db/redis-0" {
		t.Errorf("evicted = %v", te.evicted)
	}
}

func TestKillPodsApplyEvictsAndIsDone(t *testing.T) {
	te := newTestEnv(t, threeReplicas("chaos-test")...)
	a, err := plan(t, te, KillPodsOptions{Namespace: "chaos-test", Workload: "api"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	done, err := a.Done(ctx)
	if err != nil || done {
		t.Fatalf("Done before apply = %v, %v", done, err)
	}
	if err := a.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if len(te.evicted) != 1 || te.evicted[0] != "chaos-test/api-1" {
		t.Errorf("evicted = %v", te.evicted)
	}
	if done, err := a.Done(ctx); err != nil || !done {
		t.Errorf("Done after apply = %v, %v", done, err)
	}
	// Idempotent: a second apply finds the pod gone and succeeds without evicting anything else.
	if err := a.Apply(ctx); err != nil {
		t.Errorf("a repeated apply must not fail: %v", err)
	}
	if len(te.evicted) != 1 {
		t.Errorf("a repeated apply evicted more: %v", te.evicted)
	}
}

func TestKillPodsDoneNoticesAReplacementWithTheSameName(t *testing.T) {
	te := newTestEnv(t, allowedNS("db"), statefulSet("db", "redis", 3),
		pod("db", "redis-0", "redis", "w1", true), pod("db", "redis-1", "redis", "w2", true), pod("db", "redis-2", "redis", "w1", true))
	a, err := plan(t, te, KillPodsOptions{Namespace: "db", Kind: "StatefulSet", Workload: "redis"})
	if err != nil {
		t.Fatal(err)
	}
	// The controller already recreated redis-0 with a new UID.
	replacement := pod("db", "redis-0", "redis", "w1", true)
	replacement.UID = types.UID("a-new-one")
	if err := te.client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), replacement, "db"); err != nil {
		t.Fatal(err)
	}
	if done, err := a.Done(context.Background()); err != nil || !done {
		t.Errorf("a replaced pod means the injection already happened: %v, %v", done, err)
	}
	// Applying now must not kill the fresh pod: the UID precondition protects it.
	if err := a.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(te.evicted) != 0 {
		t.Errorf("the replacement was evicted: %v", te.evicted)
	}
}

func TestKillPodsApplyChecksTheGuardAgain(t *testing.T) {
	te := newTestEnv(t, threeReplicas("chaos-test")...)
	a, err := plan(t, te, KillPodsOptions{Namespace: "chaos-test", Workload: "api"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 1. read-only
	a.guard.ReadOnly = true
	mustErrorIs(t, a.Apply(ctx), dr.ErrReadOnly)
	a.guard.ReadOnly = false

	// 2. the label is removed after planning
	if err := te.client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("namespaces"), ns("chaos-test", nil), ""); err != nil {
		t.Fatal(err)
	}
	mustErrorIs(t, a.Apply(ctx), ErrNamespaceNotAllowed)
	if err := te.client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("namespaces"), allowedNS("chaos-test"), ""); err != nil {
		t.Fatal(err)
	}

	// 3. a replica was lost since planning: only api-1 and api-2... leave one ready outside the target.
	if err := te.client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "chaos-test", "api-2"); err != nil {
		t.Fatal(err)
	}
	if err := te.client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "chaos-test", "api-3"); err != nil {
		t.Fatal(err)
	}
	err = a.Apply(ctx)
	mustErrorIs(t, err, ErrBlastRadius)
	if len(te.evicted) != 0 {
		t.Errorf("nothing may be evicted when the guard fails: %v", te.evicted)
	}
}

func TestKillPodsProductionNeedsThePhrase(t *testing.T) {
	te := newTestEnv(t, threeReplicas("chaos-test")...)
	te.env.Context = "prod-lab"
	p := labPlan()
	p.Context = "prod-lab"
	g := Guard{Plan: p, Env: te.env}
	a, err := NewKillPods(context.Background(), g, KillPodsOptions{Namespace: "chaos-test", Workload: "api", Pick: first})
	if err != nil {
		t.Fatalf("planning (a dry run) needs no phrase: %v", err)
	}
	mustErrorIs(t, a.Apply(context.Background()), dr.ErrConfirmationRequired)
	a.guard.Confirm = "chaos prod-lab"
	if err := a.Apply(context.Background()); err != nil {
		t.Errorf("with the phrase it must run: %v", err)
	}
}

func TestKillPodsMapsABudgetRefusalFromTheCluster(t *testing.T) {
	te := newTestEnv(t, threeReplicas("chaos-test")...)
	a, err := plan(t, te, KillPodsOptions{Namespace: "chaos-test", Workload: "api"})
	if err != nil {
		t.Fatal(err)
	}
	te.refuseEviction = func(string, string) bool { return true }
	err = a.Apply(context.Background())
	mustErrorIs(t, err, ErrDisruptionBudget)
}

func TestKillPodsRandomPickStaysInRange(t *testing.T) {
	te := newTestEnv(t, threeReplicas("chaos-test")...)
	for range 20 {
		a, err := NewKillPods(context.Background(), te.guard(), KillPodsOptions{Namespace: "chaos-test", Workload: "api", Count: 2, MaxPercent: 67})
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Targets()) != 2 {
			t.Fatalf("targets = %v", a.Targets())
		}
	}
	// A broken Pick function must not panic.
	a, err := NewKillPods(context.Background(), te.guard(), KillPodsOptions{Namespace: "chaos-test", Workload: "api", Pick: func(int) int { return 99 }})
	if err != nil || len(a.Targets()) != 1 {
		t.Errorf("out-of-range pick: %v %v", a, err)
	}
}
