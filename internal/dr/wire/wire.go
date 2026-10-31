// Package wire builds everything a disaster-recovery operation needs from one kubeconfig: real
// clients per context, the external-command runner, the HTTP checker, the state store, the planners
// of every phase and the engine. The CLI and the web console both go through a Stack, so they behave
// the same and neither has to know how the packages under internal/dr fit together.
//
// A Stack is cheap to create and safe to use from several goroutines. Nothing is called on a cluster
// when an Env is created, so Env also works for a cluster that is powered off (the "up" direction
// needs that).
package wire

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/backups"
	"github.com/1985epma/k8scli/internal/dr/dbops"
	"github.com/1985epma/k8scli/internal/dr/engine"
	"github.com/1985epma/k8scli/internal/dr/k8sops"
	"github.com/1985epma/k8scli/internal/dr/measure"
	"github.com/1985epma/k8scli/internal/dr/provider"
	"github.com/1985epma/k8scli/internal/kubeconfig"
)

// requestTimeout bounds every single call to a cluster. DR polls clusters that may be half started, so
// a call must never hang.
const requestTimeout = 30 * time.Second

// Options configure a Stack. Only the first four fields are normally set; the rest let tests and
// embedders replace a part (all are optional).
type Options struct {
	// Kubeconfig is an explicit kubeconfig path; empty means the kubectl rules ($KUBECONFIG, ~/.kube/config).
	Kubeconfig string
	// Dir is the DR data directory (ledgers, locks, state, audit log, reports). Default ~/.k8scli/dr.
	Dir string
	// ReadOnly makes every run that is not a dry run fail with dr.ErrReadOnly.
	ReadOnly bool
	// Actor is recorded in the audit log. Default: the current user's name, or "cli".
	Actor string

	// K8sOps, DBOps and Provider tune the planners. DBOps.Backups defaults to backups.Newest.
	K8sOps   k8sops.Options
	DBOps    dbops.Options
	Provider provider.Options

	// Clients builds the clients of a context. Default: from the kubeconfig.
	Clients func(contextName string) (kubernetes.Interface, dynamic.Interface, error)
	// Exec runs provider CLIs. Default: provider.NewOSExec().
	Exec dr.Exec
	// HTTP checks health URLs. Default: measure.NewHTTPChecker().
	HTTP dr.HTTPChecker
	// State keeps what "down" learned for "up". Default: a file store under Dir/state.
	State dr.StateStore
	// Now and Sleep replace the clock; Logf receives adapter log lines.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Logf  func(format string, args ...any)
	// Engine replaces the engine (tests).
	Engine dr.Runner
}

// Stack is the wired module for one kubeconfig.
type Stack struct {
	opts   Options
	loader kubeconfig.Loader
	engine *engine.Engine
	runner dr.Runner
	state  dr.StateStore

	mu    sync.Mutex
	hooks []func(plan *dr.Plan, env *dr.Env) []dr.Planner
}

// New builds a Stack. It reads no cluster and writes no file.
func New(opts Options) (*Stack, error) {
	eng, err := engine.New(engine.Options{Dir: opts.Dir, ReadOnly: opts.ReadOnly, Now: opts.Now, Sleep: opts.Sleep})
	if err != nil {
		return nil, err
	}
	if opts.Dir == "" {
		opts.Dir = eng.Dir()
	}
	if opts.Actor == "" {
		opts.Actor = defaultActor()
	}
	if opts.DBOps.Backups == nil {
		opts.DBOps.Backups = backups.Newest
	}
	s := &Stack{opts: opts, loader: kubeconfig.New(opts.Kubeconfig), engine: eng, runner: eng, state: opts.State}
	if opts.Engine != nil {
		s.runner = opts.Engine
	}
	if s.state == nil {
		s.state = engine.NewFileState(eng.StateDir())
	}
	return s, nil
}

func defaultActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "cli"
}

// Dir is the DR data directory.
func (s *Stack) Dir() string { return s.opts.Dir }

// Actor is the name recorded in the audit log.
func (s *Stack) Actor() string { return s.opts.Actor }

// ReadOnly reports whether real runs are refused.
func (s *Stack) ReadOnly() bool { return s.opts.ReadOnly }

// CurrentContext is the kubeconfig's current context, or "" when there is none.
func (s *Stack) CurrentContext() string { return s.loader.CurrentContext() }

// Contexts lists the context names of the kubeconfig.
func (s *Stack) Contexts() ([]string, error) {
	infos, err := s.loader.Contexts()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	return names, nil
}

// ---- environment -------------------------------------------------------------------------------------

func (s *Stack) clients(contextName string) (kubernetes.Interface, dynamic.Interface, error) {
	if s.opts.Clients != nil {
		return s.opts.Clients(contextName)
	}
	config, err := s.loader.RESTConfigFor(contextName)
	if err != nil {
		return nil, nil, err
	}
	config.Timeout = requestTimeout
	config.QPS, config.Burst = 20, 40
	config.UserAgent = "k8scli-dr"
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create the Kubernetes client for context %q: %w", contextName, err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create the dynamic client for context %q: %w", contextName, err)
	}
	return client, dynamicClient, nil
}

// Env returns the environment for a kubeconfig context. The clients are built but no request is made,
// so the cluster may be down.
func (s *Stack) Env(_ context.Context, contextName string) (*dr.Env, error) {
	if contextName == "" {
		return nil, errors.New("a context name is required")
	}
	client, dynamicClient, err := s.clients(contextName)
	if err != nil {
		return nil, err
	}
	exec := s.opts.Exec
	if exec == nil {
		exec = provider.NewOSExec()
	}
	httpChecker := s.opts.HTTP
	if httpChecker == nil {
		httpChecker = measure.NewHTTPChecker()
	}
	now := s.opts.Now
	if now == nil {
		now = time.Now
	}
	sleep := s.opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	logf := s.opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &dr.Env{
		Context: contextName, Client: client, Dynamic: dynamicClient,
		Exec: exec, HTTP: httpChecker, State: s.state, Now: now, Sleep: sleep, Logf: logf,
	}, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ---- planners ----------------------------------------------------------------------------------------

// RegisterPlanners adds a function whose planners join the built-in ones in Planners, Run and Drill.
// It is how features that live in other files (chaos, external checks...) take part in runs without this
// package knowing about them. The function receives the plan and the Env of the run; it may return nil.
func (s *Stack) RegisterPlanners(f func(plan *dr.Plan, env *dr.Env) []dr.Planner) {
	if f == nil {
		return
	}
	s.mu.Lock()
	s.hooks = append(s.hooks, f)
	s.mu.Unlock()
}

// Planners returns the planner of every phase for a plan: prepare, apps, databases, nodes and power,
// plus whatever RegisterPlanners added. The engine orders them by phase. Building them reads no cluster.
func (s *Stack) Planners(plan *dr.Plan) []dr.Planner {
	var env *dr.Env
	if plan != nil && plan.Context != "" {
		if e, err := s.Env(context.Background(), plan.Context); err == nil {
			env = e
		}
	}
	return s.PlannersWith(plan, env, s.opts.K8sOps)
}

// PlannersWith is Planners with different k8sops options (for example a list of nodes to drain) and the
// Env the registered functions should see (it may be nil when none could be built).
func (s *Stack) PlannersWith(plan *dr.Plan, env *dr.Env, k8s k8sops.Options) []dr.Planner {
	// A shutdown that powers the machines off drains only the plan's own namespaces: the pods of kube-system
	// and of operators stop with the machine, and a full drain would refuse them (emptyDir, no controller).
	// Runs that do not reach the power phase, such as `dr nodes drain`, keep the full kubectl-like drain.
	k8s.DrainPlanNamespacesBeforePower = true
	planners := k8sops.Planners(k8s)
	planners = append(planners, dbops.NewPlanner(s.opts.DBOps), provider.NewPlanner(s.opts.Provider))

	s.mu.Lock()
	hooks := append([]func(*dr.Plan, *dr.Env) []dr.Planner(nil), s.hooks...)
	s.mu.Unlock()
	for _, hook := range hooks {
		planners = append(planners, hook(plan, env)...)
	}
	return planners
}

// ConfirmPhrase is the phrase a real run needs: "" when none, the context name for production-looking
// contexts, "stop <context>" for runs that power machines off.
func (s *Stack) ConfirmPhrase(plan *dr.Plan, dir dr.Direction, scope dr.Scope) string {
	return engine.ConfirmPhrase(plan, dir, scope)
}

// ---- runs --------------------------------------------------------------------------------------------

// Run builds the environment and planners for the plan's context and runs the plan in a direction. A
// dry run (opts.DryRun) changes nothing. The Stack's Actor and ReadOnly are applied.
func (s *Stack) Run(ctx context.Context, plan *dr.Plan, dir dr.Direction, scope dr.Scope, opts dr.RunOptions) (*dr.RunResult, error) {
	return s.RunWithOptions(ctx, plan, dir, scope, s.opts.K8sOps, opts)
}

// RunWithOptions is Run with different k8sops options (for example the nodes to drain).
func (s *Stack) RunWithOptions(ctx context.Context, plan *dr.Plan, dir dr.Direction, scope dr.Scope, k8s k8sops.Options, opts dr.RunOptions) (*dr.RunResult, error) {
	if plan == nil {
		return nil, errors.New("a plan is required")
	}
	env, err := s.Env(ctx, plan.Context)
	if err != nil {
		return nil, err
	}
	if opts.Actor == "" {
		opts.Actor = s.opts.Actor
	}
	opts.ReadOnly = opts.ReadOnly || s.opts.ReadOnly
	return s.runner.Run(ctx, env, plan, dir, scope, s.PlannersWith(plan, env, k8s), opts)
}

// Status returns a stored run.
func (s *Stack) Status(ctx context.Context, id string) (*dr.RunResult, bool, error) {
	return s.runner.Status(ctx, id)
}

// List returns stored runs, newest first.
func (s *Stack) List(ctx context.Context, limit int) ([]dr.RunResult, error) {
	return s.runner.List(ctx, limit)
}

// Audit returns the newest audit records, oldest first (limit <= 0 means 200).
func (s *Stack) Audit(ctx context.Context, limit int) ([]engine.AuditRecord, error) {
	return s.engine.Audit(ctx, limit)
}

// ---- measuring ---------------------------------------------------------------------------------------

// Drill runs a recovery rehearsal for the plan and measures RTO and RPO. Defaults filled in: Dir (the
// Stack's directory), Actor, the backup finder and the database estimator.
func (s *Stack) Drill(ctx context.Context, plan *dr.Plan, opts measure.DrillOptions) (*measure.Report, error) {
	if plan == nil {
		return nil, errors.New("a plan is required")
	}
	env, err := s.Env(ctx, plan.Context)
	if err != nil {
		return nil, err
	}
	if s.opts.ReadOnly {
		return nil, dr.ErrReadOnly
	}
	if opts.Dir == "" {
		opts.Dir = s.opts.Dir
	}
	if opts.Actor == "" {
		opts.Actor = s.opts.Actor
	}
	if opts.Backups == nil {
		opts.Backups = backups.Newest
	}
	if opts.DBEstimates == nil {
		opts.DBEstimates = estimateDatabases
	}
	return measure.Drill(ctx, env, plan, s.PlannersWith(plan, env, s.opts.K8sOps), s.runner, opts)
}

// estimateDatabases adapts dbops.EstimateRPO to the measure package's estimator type.
func estimateDatabases(ctx context.Context, env *dr.Env, plan *dr.Plan) ([]measure.DBEstimate, error) {
	estimates, err := dbops.EstimateRPO(ctx, env, plan)
	if err != nil {
		return nil, err
	}
	out := make([]measure.DBEstimate, 0, len(estimates))
	for _, e := range estimates {
		if !e.Known {
			continue
		}
		out = append(out, measure.DBEstimate{Name: e.Name, Age: e.Age, Source: e.Source, Note: e.Note})
	}
	return out, nil
}

// EstimateRPO estimates the data-loss window of every database in the plan without database credentials.
func (s *Stack) EstimateRPO(ctx context.Context, plan *dr.Plan) ([]dbops.RPOEstimate, error) {
	env, err := s.Env(ctx, plan.Context)
	if err != nil {
		return nil, err
	}
	return dbops.EstimateRPO(ctx, env, plan)
}

// Reports lists stored drill reports, newest first (limit <= 0 means all).
func (s *Stack) Reports(limit int) ([]*measure.Report, error) {
	return measure.History(s.opts.Dir, limit)
}

// Report loads one stored drill report by id.
func (s *Stack) Report(id string) (*measure.Report, error) {
	return measure.Load(s.opts.Dir, id)
}

// ReportPaths returns where a report's JSON and Markdown files live.
func (s *Stack) ReportPaths(id string) (jsonPath, markdownPath string) {
	base := filepath.Join(s.opts.Dir, "reports", id)
	return base + ".json", base + ".md"
}
