package k8sops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

const appsStateKey = testContext + "/apps"

func threeTierPlan() *dr.Plan {
	return testPlan(
		dr.App{Name: "data", Namespace: "shop", Tier: 0, Workloads: []string{"cache"}},
		dr.App{Name: "api", Namespace: "shop", Tier: 1, Workloads: []string{"api"}},
		dr.App{Name: "web", Namespace: "shop", Tier: 2, Workloads: []string{"web"}},
	)
}

func threeTierObjects() []runtime.Object {
	return []runtime.Object{deploy("shop", "cache", 1), deploy("shop", "api", 3), deploy("shop", "web", 2)}
}

func stepsFor(t *testing.T, te *testEnv, p dr.Planner, plan *dr.Plan, dir dr.Direction, scope dr.Scope) []dr.Step {
	t.Helper()
	steps, err := p.Steps(context.Background(), te.env, plan, dir, scope)
	if err != nil {
		t.Fatalf("Steps(%s): %v", dir, err)
	}
	return steps
}

func TestAppsDownStopsHighestTierFirstAndRecordsReplicas(t *testing.T) {
	te := newEnv(t, threeTierObjects()...)
	p := NewAppsPlanner(Options{})

	steps := stepsFor(t, te, p, threeTierPlan(), dr.DirDown, dr.Scope{})
	want := []string{"apps/shop/deployment/web:scale-down", "apps/shop/deployment/api:scale-down", "apps/shop/deployment/cache:scale-down"}
	if got := ids(steps); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v; want %v", got, want)
	}
	for _, s := range steps {
		if _, ok := s.Gate.(*PodsGoneGate); !ok {
			t.Errorf("%s: every wave must wait for its pods to terminate, gate = %T", s.Action.ID(), s.Gate)
		}
		if s.Action.Risk() != dr.RiskHigh {
			t.Errorf("%s: scaling down is high risk", s.Action.ID())
		}
	}

	if err := runSteps(context.Background(), onlyActions(steps)); err != nil {
		t.Fatal(err)
	}

	for name, before := range map[string]string{"web": "2", "api": "3", "cache": "1"} {
		if got := te.replicas(KindDeployment, "shop", name); got != 0 {
			t.Errorf("%s replicas = %d; want 0", name, got)
		}
		if got, ok := te.annotation(KindDeployment, "shop", name); !ok || got != before {
			t.Errorf("%s annotation = %q, %v; want %q", name, got, ok, before)
		}
	}
	var recorded map[string]int32
	if found, err := te.state.Load(context.Background(), appsStateKey, &recorded); err != nil || !found {
		t.Fatalf("state not written: %v %v", found, err)
	}
	if recorded["Deployment/shop/api"] != 3 || recorded["Deployment/shop/web"] != 2 || recorded["Deployment/shop/cache"] != 1 {
		t.Errorf("state = %v", recorded)
	}
}

// onlyActions drops the gates, for tests that do not exercise them.
func onlyActions(steps []dr.Step) []dr.Step {
	out := make([]dr.Step, len(steps))
	for i, s := range steps {
		out[i] = dr.Step{Phase: s.Phase, Action: s.Action}
	}
	return out
}

func TestAppsDownIsIdempotentAndNeverOverwritesARecord(t *testing.T) {
	te := newEnv(t, threeTierObjects()...)
	p := NewAppsPlanner(Options{})
	ctx := context.Background()

	steps := onlyActions(stepsFor(t, te, p, threeTierPlan(), dr.DirDown, dr.Scope{}))
	if err := runSteps(ctx, steps); err != nil {
		t.Fatal(err)
	}
	writes := te.state.writes

	// A second run finds everything at 0 with a record: every step is already done.
	again := onlyActions(stepsFor(t, te, p, threeTierPlan(), dr.DirDown, dr.Scope{}))
	for _, s := range again {
		done, err := s.Action.Done(ctx)
		if err != nil || !done {
			t.Errorf("%s should be done on the second run: %v %v", s.Action.ID(), done, err)
		}
	}
	if err := runSteps(ctx, again); err != nil {
		t.Fatal(err)
	}
	if te.state.writes != writes {
		t.Errorf("the second run wrote the state again (%d -> %d writes)", writes, te.state.writes)
	}
	if got, _ := te.annotation(KindDeployment, "shop", "api"); got != "3" {
		t.Errorf("the record of api was overwritten with %q", got)
	}

	// Even forcing Apply on a workload already at 0 keeps the recorded 3.
	if err := again[1].Action.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := te.annotation(KindDeployment, "shop", "api"); got != "3" {
		t.Errorf("Apply at 0 replicas overwrote the record with %q", got)
	}
}

func TestAppsDownRecordsZeroForWorkloadsAlreadyStopped(t *testing.T) {
	te := newEnv(t, deploy("shop", "batch", 0))
	plan := testPlan(dr.App{Name: "batch", Namespace: "shop"})
	steps := stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{})
	ctx := context.Background()

	if done, _ := steps[0].Action.Done(ctx); done {
		t.Fatal("a workload at 0 with no record is not done: the record is still to be written")
	}
	if err := runSteps(ctx, onlyActions(steps)); err != nil {
		t.Fatal(err)
	}
	if got, ok := te.annotation(KindDeployment, "shop", "batch"); !ok || got != "0" {
		t.Errorf("annotation = %q, %v; want 0", got, ok)
	}

	// "up" then knows it was stopped before the DR and leaves it at 0, without refusing.
	up := stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirUp, dr.Scope{})
	if err := runSteps(ctx, onlyActions(up)); err != nil {
		t.Fatal(err)
	}
	if got := te.replicas(KindDeployment, "shop", "batch"); got != 0 {
		t.Errorf("replicas = %d; a workload that was at 0 must stay at 0", got)
	}
}

func TestAppsUpRestoresInTierOrderAndCleansTheRecords(t *testing.T) {
	te := newEnv(t, threeTierObjects()...)
	ctx := context.Background()
	if err := runSteps(ctx, onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), threeTierPlan(), dr.DirDown, dr.Scope{}))); err != nil {
		t.Fatal(err)
	}

	steps := stepsFor(t, te, NewAppsPlanner(Options{}), threeTierPlan(), dr.DirUp, dr.Scope{})
	want := "apps/shop/deployment/cache:scale-up,apps/shop/deployment/api:scale-up,apps/shop/deployment/web:scale-up"
	if got := strings.Join(ids(steps), ","); got != want {
		t.Fatalf("order = %s; want %s", got, want)
	}
	for _, s := range steps {
		if _, ok := s.Gate.(*PodsReadyGate); !ok {
			t.Errorf("%s: the default gate is pods ready, got %T", s.Action.ID(), s.Gate)
		}
	}

	// Gates are exercised in their own tests; here the actions are applied one after another.
	run := func() error {
		for _, s := range steps {
			if err := s.Action.Apply(ctx); err != nil {
				return err
			}
		}
		return nil
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int32{"cache": 1, "api": 3, "web": 2} {
		if got := te.replicas(KindDeployment, "shop", name); got != want {
			t.Errorf("%s replicas = %d; want %d", name, got, want)
		}
		if _, ok := te.annotation(KindDeployment, "shop", name); ok {
			t.Errorf("%s still carries the annotation after up", name)
		}
	}
	if te.state.has(appsStateKey) {
		t.Error("the DR state must be empty once everything is restored")
	}

	// Running up again changes nothing: all steps are done.
	for _, s := range stepsFor(t, te, NewAppsPlanner(Options{}), threeTierPlan(), dr.DirUp, dr.Scope{}) {
		if done, err := s.Action.Done(ctx); err != nil || !done {
			t.Errorf("%s should be done: %v %v", s.Action.ID(), done, err)
		}
	}
}

func TestAppsUpFallsBackToTheDRStateWhenTheAnnotationIsGone(t *testing.T) {
	te := newEnv(t, deploy("shop", "api", 4))
	plan := testPlan(dr.App{Name: "api", Namespace: "shop"})
	ctx := context.Background()
	if err := runSteps(ctx, onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{}))); err != nil {
		t.Fatal(err)
	}
	if err := (ops{client: te.client}).setAnnotation(ctx, WorkloadRef{KindDeployment, "shop", "api"}, nil); err != nil {
		t.Fatal(err)
	}

	steps := stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirUp, dr.Scope{})
	if !strings.Contains(steps[0].Action.Describe(), "DR state") {
		t.Errorf("describe = %q; it should say where the count comes from", steps[0].Action.Describe())
	}
	if err := runSteps(ctx, onlyActions(steps)); err != nil {
		t.Fatal(err)
	}
	if got := te.replicas(KindDeployment, "shop", "api"); got != 4 {
		t.Errorf("replicas = %d; want 4 from the DR state", got)
	}
}

func TestAppsUpRefusesToGuessAndNamesTheWorkload(t *testing.T) {
	te := newEnv(t, deploy("shop", "orphan", 0))
	plan := testPlan(dr.App{Name: "shop", Namespace: "shop"})

	_, err := NewAppsPlanner(Options{}).Steps(context.Background(), te.env, plan, dr.DirUp, dr.Scope{})
	if err == nil || !strings.Contains(err.Error(), "shop/orphan") || !strings.Contains(err.Error(), "no recorded replica count") {
		t.Fatalf("err = %v; it must name shop/orphan and explain", err)
	}

	steps, err := NewAppsPlanner(Options{SkipUnknownReplicas: true}).Steps(context.Background(), te.env, plan, dr.DirUp, dr.Scope{})
	if err != nil || len(steps) != 0 {
		t.Errorf("with SkipUnknownReplicas the workload is left out: %d steps, %v", len(steps), err)
	}
	if !containsLog(te, "skipping") {
		t.Errorf("the skip must be logged: %v", te.logs)
	}

	// Apply on a forced scale-up still refuses: it never invents a number.
	action := &scaleUp{workloadAction{o: ops{client: te.client}, ref: WorkloadRef{KindDeployment, "shop", "orphan"}, state: newLedger(te.env, newMemStore(), "apps"), c: clockOf(te.env), seen: 0}}
	if err := action.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "not guessing") {
		t.Errorf("Apply err = %v", err)
	}
}

func containsLog(te *testEnv, fragment string) bool {
	for _, l := range te.logs {
		if strings.Contains(l, fragment) {
			return true
		}
	}
	return false
}

func TestAppsUpRefusesAnInvalidAnnotation(t *testing.T) {
	d := deploy("shop", "api", 0)
	d.Annotations = map[string]string{AnnotationReplicasBefore: "lots"}
	te := newEnv(t, d)
	_, err := NewAppsPlanner(Options{}).Steps(context.Background(), te.env, testPlan(dr.App{Name: "a", Namespace: "shop"}), dr.DirUp, dr.Scope{})
	if err == nil || !strings.Contains(err.Error(), "invalid value") {
		t.Errorf("err = %v", err)
	}
}

func TestAppsLeaveDatabaseStatefulSetsToTheDatabasesPlanner(t *testing.T) {
	te := newEnv(t, deploy("shop", "api", 2), statefulSet("shop", "pg", 1), statefulSet("shop", "queue", 3))
	plan := testPlan(dr.App{Name: "shop", Namespace: "shop"})
	plan.Databases = []dr.Database{{Name: "pg", Namespace: "shop", Kind: "statefulset", Ref: "pg"}}

	steps := stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{})
	got := strings.Join(ids(steps), ",")
	if strings.Contains(got, "/pg:") {
		t.Errorf("the database StatefulSet must not be scaled by the apps planner: %s", got)
	}
	if !strings.Contains(got, "statefulset/queue") || !strings.Contains(got, "deployment/api") {
		t.Errorf("steps = %s", got)
	}
	// Deployments go before StatefulSets on the way down.
	if strings.Index(got, "deployment/api") > strings.Index(got, "statefulset/queue") {
		t.Errorf("Deployments must be scaled down before StatefulSets: %s", got)
	}
}

func TestAppsStatefulSetsStartBeforeDeploymentsOnTheWayUp(t *testing.T) {
	te := newEnv(t, deploy("shop", "api", 2), statefulSet("shop", "queue", 3))
	plan := testPlan(dr.App{Name: "shop", Namespace: "shop"})
	ctx := context.Background()
	if err := runSteps(ctx, onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{}))); err != nil {
		t.Fatal(err)
	}
	if got := te.replicas(KindStatefulSet, "shop", "queue"); got != 0 {
		t.Fatalf("StatefulSet replicas = %d", got)
	}
	up := strings.Join(ids(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirUp, dr.Scope{})), ",")
	if strings.Index(up, "statefulset/queue") > strings.Index(up, "deployment/api") {
		t.Errorf("StatefulSets must start first: %s", up)
	}
}

func TestAppsWorkloadSelectionByNameAndKind(t *testing.T) {
	te := newEnv(t, deploy("shop", "api", 1), statefulSet("shop", "api", 1), deploy("shop", "web", 1))

	_, err := NewAppsPlanner(Options{}).Steps(context.Background(), te.env,
		testPlan(dr.App{Name: "a", Namespace: "shop", Workloads: []string{"api"}}), dr.DirDown, dr.Scope{})
	if err == nil || !strings.Contains(err.Error(), "both a Deployment and a StatefulSet") {
		t.Errorf("ambiguous name err = %v", err)
	}

	steps := stepsFor(t, te, NewAppsPlanner(Options{}),
		testPlan(dr.App{Name: "a", Namespace: "shop", Workloads: []string{"statefulset/api", "deployment/web"}}), dr.DirDown, dr.Scope{})
	if got := strings.Join(ids(steps), ","); !strings.Contains(got, "statefulset/api") || !strings.Contains(got, "deployment/web") || strings.Contains(got, "deployment/api") {
		t.Errorf("steps = %s", got)
	}

	_, err = NewAppsPlanner(Options{}).Steps(context.Background(), te.env,
		testPlan(dr.App{Name: "a", Namespace: "shop", Workloads: []string{"ghost"}}), dr.DirDown, dr.Scope{})
	if err == nil || !strings.Contains(err.Error(), `"ghost" was not found`) {
		t.Errorf("missing workload err = %v", err)
	}

	_, err = NewAppsPlanner(Options{}).Steps(context.Background(), te.env,
		testPlan(dr.App{Name: "a", Namespace: "shop", Workloads: []string{"deployment/web"}}, dr.App{Name: "b", Namespace: "shop", Workloads: []string{"deployment/web"}}), dr.DirDown, dr.Scope{})
	if err == nil || !strings.Contains(err.Error(), "listed by both") {
		t.Errorf("double claim err = %v", err)
	}
}

func TestAppsWildcardTakesWhatTheNamedAppsDoNotAndScopeNeverSteals(t *testing.T) {
	te := newEnv(t, deploy("shop", "api", 1), deploy("shop", "web", 1), deploy("shop", "worker", 1))
	plan := testPlan(
		dr.App{Name: "named", Namespace: "shop", Tier: 1, Workloads: []string{"web"}},
		dr.App{Name: "rest", Namespace: "shop", Tier: 0},
	)

	all := strings.Join(ids(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{})), ",")
	if !strings.Contains(all, "web") || !strings.Contains(all, "api") || !strings.Contains(all, "worker") {
		t.Fatalf("steps = %s", all)
	}

	// Running only "named" must not hand it the workloads of "rest".
	only := strings.Join(ids(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{Apps: []string{"named"}})), ",")
	if only != "apps/shop/deployment/web:scale-down" {
		t.Errorf("scoped steps = %s", only)
	}
	// "rest" alone does not take "web" either.
	rest := strings.Join(ids(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{Apps: []string{"rest"}})), ",")
	if strings.Contains(rest, "web") {
		t.Errorf("rest must not take web: %s", rest)
	}
}

func TestAppsDependsOnShapesTheOrder(t *testing.T) {
	te := newEnv(t, deploy("shop", "reports", 1), deploy("shop", "cache", 1), deploy("shop", "api", 1))
	plan := testPlan(
		dr.App{Name: "reports", Namespace: "shop", Tier: 0, Workloads: []string{"reports"}, DependsOn: []string{"cache"}},
		dr.App{Name: "cache", Namespace: "shop", Tier: 2, Workloads: []string{"cache"}},
		dr.App{Name: "api", Namespace: "shop", Tier: 1, Workloads: []string{"api"}},
	)
	down := strings.Join(ids(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{})), ",")
	want := "apps/shop/deployment/reports:scale-down,apps/shop/deployment/cache:scale-down,apps/shop/deployment/api:scale-down"
	if down != want {
		t.Errorf("down = %s; want %s", down, want)
	}
}

func TestAppsHPABoundsAreNeverTouched(t *testing.T) {
	minReplicas := int32(2)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api", APIVersion: "apps/v1"},
			MinReplicas:    &minReplicas,
			MaxReplicas:    9,
		},
	}
	te := newEnv(t, deploy("shop", "api", 4), hpa)
	plan := testPlan(dr.App{Name: "api", Namespace: "shop"})
	ctx := context.Background()

	if err := runSteps(ctx, onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{}))); err != nil {
		t.Fatal(err)
	}
	if err := runSteps(ctx, onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirUp, dr.Scope{}))); err != nil {
		t.Fatal(err)
	}

	got, err := te.client.AutoscalingV2().HorizontalPodAutoscalers("shop").Get(ctx, "api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *got.Spec.MinReplicas != 2 || got.Spec.MaxReplicas != 9 {
		t.Errorf("HPA bounds changed to %d..%d", *got.Spec.MinReplicas, got.Spec.MaxReplicas)
	}
	for _, a := range te.client.Actions() {
		if a.GetResource().Resource == "horizontalpodautoscalers" && a.GetVerb() != "get" {
			t.Errorf("the planner must not write HPAs: %s", a.GetVerb())
		}
	}
	if got := te.replicas(KindDeployment, "shop", "api"); got != 4 {
		t.Errorf("replicas = %d; want 4", got)
	}
}

func TestAppsUpGatesUseHTTPHealthWhenAskedAndMergeSameWave(t *testing.T) {
	te := newEnv(t, deploy("shop", "api", 2), deploy("shop", "web", 2))
	plan := testPlan(
		dr.App{Name: "api", Namespace: "shop", Tier: 1, Workloads: []string{"api"}, Health: &dr.Health{Kind: "http", URL: "https://shop.example/health", Status: 200, Timeout: "3m"}},
		dr.App{Name: "web", Namespace: "shop", Tier: 1, Workloads: []string{"web"}},
	)
	ctx := context.Background()
	if err := runSteps(ctx, onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{}))); err != nil {
		t.Fatal(err)
	}
	steps := stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirUp, dr.Scope{})
	if len(steps) != 2 {
		t.Fatalf("steps = %v", ids(steps))
	}
	if steps[0].Gate != nil {
		t.Errorf("the gate belongs to the last step of the wave, got %T on the first", steps[0].Gate)
	}
	gate := steps[1].Gate
	if gate == nil || !strings.Contains(gate.Name(), "https://shop.example/health") || !strings.Contains(gate.Name(), "pods ready") {
		t.Errorf("the wave gate should combine the HTTP check and pods ready: %v", gate)
	}
}

func TestAppsHealthErrors(t *testing.T) {
	te := newEnv(t, deploy("shop", "api", 1))
	for name, health := range map[string]*dr.Health{
		"bad timeout": {Timeout: "soon"},
		"http no url": {Kind: "http"},
		"bad kind":    {Kind: "tcp"},
	} {
		plan := testPlan(dr.App{Name: "api", Namespace: "shop", Health: health})
		// Make "up" plannable first.
		_ = runSteps(context.Background(), onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), testPlan(dr.App{Name: "api", Namespace: "shop"}), dr.DirDown, dr.Scope{})))
		if _, err := NewAppsPlanner(Options{}).Steps(context.Background(), te.env, plan, dr.DirUp, dr.Scope{}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestAppsUnreachableCluster(t *testing.T) {
	te := newEnv(t, deploy("shop", "api", 3))
	plan := testPlan(dr.App{Name: "api", Namespace: "shop"})
	ctx := context.Background()
	if err := runSteps(ctx, onlyActions(stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{}))); err != nil {
		t.Fatal(err)
	}

	te.client.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp 127.0.0.1:6443: connect: connection refused")
	})

	// Down needs the live cluster.
	if _, err := NewAppsPlanner(Options{}).Steps(ctx, te.env, plan, dr.DirDown, dr.Scope{}); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("down err = %v; want ErrUnreachable", err)
	}
	// Up can still be planned from what down recorded.
	steps, err := NewAppsPlanner(Options{}).Steps(ctx, te.env, plan, dr.DirUp, dr.Scope{})
	if err != nil || len(steps) != 1 {
		t.Fatalf("up from state: %d steps, %v", len(steps), err)
	}
	if !strings.Contains(steps[0].Action.Describe(), "3 replicas") {
		t.Errorf("describe = %q", steps[0].Action.Describe())
	}

	// With nothing recorded, there is nothing to fall back to.
	fresh := newEnv(t)
	fresh.client.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	if _, err := NewAppsPlanner(Options{}).Steps(ctx, fresh.env, plan, dr.DirUp, dr.Scope{}); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("err = %v; want ErrUnreachable", err)
	}

	// No client at all.
	noClient := newEnv(t)
	noClient.env.Client = nil
	if _, err := NewAppsPlanner(Options{}).Steps(ctx, noClient.env, plan, dr.DirDown, dr.Scope{}); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("nil client err = %v; want ErrUnreachable", err)
	}

	// An answer from the API server is not "unreachable".
	forbidden := newEnv(t)
	forbidden.client.PrependReactor("list", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apiForbidden()
	})
	if _, err := NewAppsPlanner(Options{}).Steps(ctx, forbidden.env, plan, dr.DirDown, dr.Scope{}); err == nil || errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("a Forbidden answer must not look unreachable: %v", err)
	}
}

func TestAppsStateFallbackWithoutAStateStoreUsesMemory(t *testing.T) {
	te := newEnv(t, deploy("shop", "api", 5))
	te.env.State = nil // no store: the planner keeps what it learned in memory and in the annotation
	plan := testPlan(dr.App{Name: "api", Namespace: "shop"})
	p := NewAppsPlanner(Options{})
	ctx := context.Background()
	if err := runSteps(ctx, onlyActions(stepsFor(t, te, p, plan, dr.DirDown, dr.Scope{}))); err != nil {
		t.Fatal(err)
	}
	if err := runSteps(ctx, onlyActions(stepsFor(t, te, p, plan, dr.DirUp, dr.Scope{}))); err != nil {
		t.Fatal(err)
	}
	if got := te.replicas(KindDeployment, "shop", "api"); got != 5 {
		t.Errorf("replicas = %d; want 5", got)
	}
}

func TestAppsDescriptionsAndEquivalents(t *testing.T) {
	te := newEnv(t, deploy("shop", "api", 3), statefulSet("shop", "queue", 2))
	plan := testPlan(dr.App{Name: "shop", Namespace: "shop"})
	down := stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirDown, dr.Scope{})

	byID := map[string]dr.Action{}
	for _, s := range down {
		byID[s.Action.ID()] = s.Action
	}
	api := byID["apps/shop/deployment/api:scale-down"]
	if api.Title() != "Scale shop/api to 0" || api.Equivalent() != "kubectl scale deployment/api --replicas=0 -n shop" ||
		!strings.Contains(api.Describe(), "(3)") {
		t.Errorf("api: %q | %q | %q", api.Title(), api.Equivalent(), api.Describe())
	}
	queue := byID["apps/shop/statefulset/queue:scale-down"]
	if queue.Title() != "Scale StatefulSet shop/queue to 0" || queue.Equivalent() != "kubectl scale statefulset/queue --replicas=0 -n shop" {
		t.Errorf("queue: %q | %q", queue.Title(), queue.Equivalent())
	}

	if err := runSteps(context.Background(), onlyActions(down)); err != nil {
		t.Fatal(err)
	}
	for _, s := range stepsFor(t, te, NewAppsPlanner(Options{}), plan, dr.DirUp, dr.Scope{}) {
		if s.Action.ID() == "apps/shop/deployment/api:scale-up" {
			if s.Action.Title() != "Scale shop/api to 3" || s.Action.Equivalent() != "kubectl scale deployment/api --replicas=3 -n shop" ||
				!strings.Contains(s.Action.Describe(), "annotation") || s.Action.Risk() != dr.RiskLow {
				t.Errorf("up api: %q | %q | %q", s.Action.Title(), s.Action.Equivalent(), s.Action.Describe())
			}
		}
	}
}

func TestAppsNoAppsMeansNoSteps(t *testing.T) {
	te := newEnv(t)
	steps, err := NewAppsPlanner(Options{}).Steps(context.Background(), te.env, testPlan(), dr.DirDown, dr.Scope{})
	if err != nil || len(steps) != 0 {
		t.Errorf("steps = %d, err = %v", len(steps), err)
	}
	if _, err := NewAppsPlanner(Options{}).Steps(context.Background(), te.env, testPlan(dr.App{Name: "a", Namespace: "x"}), dr.DirDown, dr.Scope{Apps: []string{"ghost"}}); err == nil {
		t.Error("an unknown app in the scope must fail")
	}
}

func TestPodsGoneGateBlocksUntilPodsTerminate(t *testing.T) {
	te := newEnv(t, deploy("shop", "api", 2), nodePod("shop", "api", "n1"))
	gate := NewPodsGoneGate(te.env, []WorkloadRef{{KindDeployment, "shop", "api"}}, time.Minute)
	te.clock.onSleep = func(total time.Duration) {
		if total >= 6*time.Second {
			_ = te.client.Tracker().Delete(podsGVR, "shop", "api")
		}
	}
	if err := gate.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if te.clock.slept < 6*time.Second {
		t.Errorf("the gate returned before the pod was gone (slept %s)", te.clock.slept)
	}
}
