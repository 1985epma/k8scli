package measure

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/1985epma/k8scli/internal/dr"
)

var epoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// simClock is a controllable clock. Sleep never advances it (so a test decides exactly when time passes),
// unless autoAdvance is on, which tests use to let timeouts expire.
type simClock struct {
	mu          sync.Mutex
	now         time.Time
	sleeps      atomic.Int64
	autoAdvance atomic.Bool
}

func newClock() *simClock { return &simClock{now: epoch} }

func (c *simClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *simClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *simClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// runawayPolls is a safety net: a test that waits for something that never happens would otherwise spin
// forever on a clock that does not move. After this many polls time starts to pass, so it fails instead.
const runawayPolls = 5000

func (c *simClock) Sleep(ctx context.Context, d time.Duration) error {
	if c.sleeps.Add(1) > runawayPolls || c.autoAdvance.Load() {
		c.Advance(d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Millisecond):
		return nil
	}
}

// settle gives the watchers time to observe the latest state before the test moves the clock again.
func (c *simClock) settle() {
	start := c.sleeps.Load()
	deadline := time.Now().Add(150 * time.Millisecond)
	for c.sleeps.Load() < start+30 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

func deploymentOf(namespace, name string, replicas, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

// shopPlan has two applications and a database, with targets.
func shopPlan() *dr.Plan {
	return &dr.Plan{
		Version: dr.PlanVersion, Name: "shop", Context: "kind-k8scli-lab",
		Targets: dr.Targets{RTO: "10m", RPO: "4h"},
		Apps: []dr.App{
			{Name: "api", Namespace: "shop", Workloads: []string{"api"}, Tier: 1, RTO: "5m"},
			{Name: "web", Namespace: "shop", Workloads: []string{"web"}, Tier: 2},
		},
		Databases: []dr.Database{{Name: "db", Namespace: "shop", Kind: "cnpg", Ref: "pg", RTO: "4m"}},
		Safety:    dr.Safety{LabOnly: true},
	}
}

func shopClient() *fake.Clientset {
	return fake.NewSimpleClientset(
		deploymentOf("shop", "api", 2, 2),
		deploymentOf("shop", "web", 3, 3),
	)
}

// scriptStep is one moment of the fake "up" run: at an offset from its start, something changes.
type scriptStep struct {
	At    time.Duration
	Phase dr.Phase
	Do    func()
}

// fakeRunner is a dr.Runner scripted by the test.
type fakeRunner struct {
	clock  *simClock
	client *fake.Clientset

	downStatus dr.RunStatus
	downErr    error
	downTook   time.Duration
	upStatus   dr.RunStatus
	upErr      error
	upScript   []scriptStep
	// afterUp runs when the up run ends (for example to let timeouts expire).
	afterUp func()
	// onDown runs at the start of the down run (for example to cancel the context).
	onDown func()
	// returnNilOnDown makes Run return no result, like a runner that refused before starting.
	returnNilOnDown bool

	mu       sync.Mutex
	dirs     []dr.Direction
	scopes   []dr.Scope
	confirms []string
	upCtxErr error
}

func (f *fakeRunner) Run(ctx context.Context, _ *dr.Env, _ *dr.Plan, dir dr.Direction, scope dr.Scope, _ []dr.Planner, opts dr.RunOptions) (*dr.RunResult, error) {
	f.mu.Lock()
	f.dirs = append(f.dirs, dir)
	f.scopes = append(f.scopes, scope)
	f.confirms = append(f.confirms, opts.Confirm)
	f.mu.Unlock()

	if dir == dr.DirDown {
		return f.down()
	}
	f.mu.Lock()
	f.upCtxErr = ctx.Err()
	f.mu.Unlock()
	return f.up()
}

func (f *fakeRunner) down() (*dr.RunResult, error) {
	if f.onDown != nil {
		f.onDown()
	}
	if f.returnNilOnDown {
		return nil, f.downErr
	}
	started := f.clock.Now()
	for _, name := range []string{"api", "web"} {
		setDeploymentIfPresent(f.client, "shop", name, 0, 0)
	}
	took := f.downTook
	if took == 0 {
		took = 30 * time.Second
	}
	f.clock.Advance(took)
	status := f.downStatus
	if status == "" {
		status = dr.StatusSucceeded
	}
	return &dr.RunResult{
		ID: "run-down", Direction: dr.DirDown, Status: status, Started: started, Finished: f.clock.Now(),
		Steps: []dr.StepResult{{ID: "apps/shop:down", Phase: dr.PhaseApps, Status: dr.StepDone, Started: started, Finished: f.clock.Now()}},
	}, f.downErr
}

func (f *fakeRunner) up() (*dr.RunResult, error) {
	start := f.clock.Now()
	result := &dr.RunResult{ID: "run-up", Direction: dr.DirUp, Started: start}
	script := append([]scriptStep(nil), f.upScript...)
	sort.SliceStable(script, func(i, j int) bool { return script[i].At < script[j].At })
	for _, step := range script {
		f.clock.Set(start.Add(step.At))
		if step.Do != nil {
			step.Do()
		}
		f.clock.settle()
		result.Steps = append(result.Steps, dr.StepResult{
			ID: "step", Phase: step.Phase, Status: dr.StepDone, Started: start, Finished: f.clock.Now(),
		})
	}
	result.Finished = f.clock.Now()
	result.Status = f.upStatus
	if result.Status == "" {
		result.Status = dr.StatusSucceeded
	}
	if f.afterUp != nil {
		f.afterUp()
	}
	return result, f.upErr
}

func setDeploymentIfPresent(client *fake.Clientset, namespace, name string, replicas, ready int32) {
	deployments := client.AppsV1().Deployments(namespace)
	current, err := deployments.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return
	}
	current.Spec.Replicas = &replicas
	current.Status.ReadyReplicas = ready
	_, _ = deployments.Update(context.Background(), current, metav1.UpdateOptions{})
}

func (f *fakeRunner) Status(context.Context, string) (*dr.RunResult, bool, error) {
	return nil, false, nil
}

func (f *fakeRunner) List(context.Context, int) ([]dr.RunResult, error) { return nil, nil }

// restoreStep returns a script step that brings a deployment back.
func (f *fakeRunner) restoreStep(at time.Duration, phase dr.Phase, name string, replicas int32) scriptStep {
	return scriptStep{At: at, Phase: phase, Do: func() { setDeploymentIfPresent(f.client, "shop", name, replicas, replicas) }}
}

func (f *fakeRunner) directions() []dr.Direction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dr.Direction(nil), f.dirs...)
}

func testEnv(clock *simClock, client *fake.Clientset) *dr.Env {
	return &dr.Env{Context: "kind-k8scli-lab", Client: client, Now: clock.Now, Sleep: clock.Sleep}
}
