package measure

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/1985epma/k8scli/internal/dr"
)

// Errors a drill returns next to its report.
var (
	// ErrDrillRefused: the plan is not marked labOnly and production was not explicitly allowed.
	ErrDrillRefused = errors.New("drill refused")
	// ErrDownFailed: the "down" run did not finish; recovery was attempted anyway.
	ErrDownFailed = errors.New("the down run failed")
	// ErrRecoveryFailed: the system could not be brought back. This is the serious one.
	ErrRecoveryFailed = errors.New("recovery failed: the system may still be down")
)

const (
	defaultHealthPoll    = 2 * time.Second
	defaultHealthTimeout = 30 * time.Minute
	defaultFailedUpGrace = 30 * time.Second
)

// BackupFinder reports when the newest successful backup covering a namespace finished.
type BackupFinder func(ctx context.Context, env *dr.Env, namespace string) (completed time.Time, found bool, err error)

// DBEstimate is the data-loss window of one database as the database adapter sees it.
type DBEstimate struct {
	Name   string
	Age    time.Duration
	Source string
	Note   string
}

// DBEstimator returns RPO estimates for the databases of a plan (dbops.EstimateRPO fits behind it).
type DBEstimator func(ctx context.Context, env *dr.Env, plan *dr.Plan) ([]DBEstimate, error)

// DrillOptions configure a drill.
type DrillOptions struct {
	// AllowProduction lets a plan that is not marked safety.labOnly be drilled. Confirm must then be set.
	AllowProduction bool
	// Confirm is handed to the runner untouched; the engine decides what phrase it needs.
	Confirm string
	// Full includes every phase the plan has (nodes, provider power). The default is apps and databases only.
	Full bool
	// Scope narrows the drill to some apps or databases; its Phases override the default phases.
	Scope dr.Scope
	Actor string
	// Dir is where reports are stored; empty means ~/.k8scli/dr.
	Dir string
	// T0 declares the disaster at a given moment; zero means "when the down run starts".
	T0 time.Time

	Backups     BackupFinder
	DBEstimates DBEstimator
	// Heartbeat enables the measured RPO of one volume. It must be configured before the drill: see Heartbeat.
	Heartbeat *HeartbeatOptions

	// HealthPoll is how often application health is checked (default 2s); HealthTimeout is how long an
	// application may take to become healthy (default 30m, or its own health.timeout); FailedUpGrace is how
	// long watchers keep going after the up run failed (default 30s).
	HealthPoll    time.Duration
	HealthTimeout time.Duration
	FailedUpGrace time.Duration

	OnEvent func(dr.Event)
}

// Drill runs a recovery rehearsal and measures it. Sequence: baseline readiness, RPO estimates at T0,
// stop the heartbeat writer (if any), declare T0, run "down", run "up" while every application's health is
// watched, compare with the targets, verify the system is back to the baseline, save the report.
//
// If "down" ran at all, "up" is always attempted, even when "down" failed or the context was cancelled, so a
// drill never leaves the system stopped on purpose. A report is returned together with ErrDownFailed or
// ErrRecoveryFailed when those happen. Errors before anything ran (refusals, confirmation, read-only, a run
// already in progress) return no report.
func Drill(ctx context.Context, env *dr.Env, plan *dr.Plan, planners []dr.Planner, runner dr.Runner, opts DrillOptions) (*Report, error) {
	if env == nil || plan == nil || runner == nil {
		return nil, errors.New("a drill needs an environment, a plan and a runner")
	}
	if err := checkDrillAllowed(plan, opts); err != nil {
		return nil, err
	}
	e := withDefaults(env)

	scope := opts.Scope
	mode := "full"
	if !opts.Full && len(scope.Phases) == 0 {
		scope.Phases = []dr.Phase{dr.PhaseApps, dr.PhaseDatabases}
	}
	if len(scope.Phases) > 0 {
		names := make([]string, 0, len(scope.Phases))
		for _, phase := range scope.Phases {
			names = append(names, string(phase))
		}
		mode = strings.Join(names, ", ")
	}

	d := &drill{env: e, plan: plan, opts: opts, runner: runner, started: e.Now()}
	d.baseline, d.baselineErr = snapshot(ctx, e, plan)

	cleanupHeartbeat := d.startHeartbeat(ctx)
	defer cleanupHeartbeat()
	d.stopHeartbeat(ctx)

	// The disaster is declared after the heartbeat writer stopped, so its last timestamp marks the data at T0.
	t0 := opts.T0
	if t0.IsZero() {
		t0 = e.Now()
	}
	d.t0 = t0
	d.collectEstimates(ctx)

	runOpts := dr.RunOptions{Confirm: opts.Confirm, Actor: opts.Actor, OnEvent: opts.OnEvent}
	down, downErr := runner.Run(ctx, e, plan, dr.DirDown, scope, planners, runOpts)
	if down == nil {
		// Nothing ran (the runner refused before starting), so there is nothing to recover.
		if downErr == nil {
			downErr = errors.New("the runner returned no result")
		}
		return nil, downErr
	}
	downFailed := downErr != nil || down.Status != dr.StatusSucceeded
	if downFailed {
		d.failures = append(d.failures, "the down run did not finish ("+runDescription(down, downErr)+"); recovery was attempted anyway")
	}

	// Recovery must not be cancelled half way: the point of a drill is that it ends with the system up.
	upCtx := ctx
	if ctx.Err() != nil {
		upCtx = context.WithoutCancel(ctx)
	}
	up, upErr := d.bringUp(upCtx, scope, planners, runOpts)
	upFailed := upErr != nil || up == nil || up.Status != dr.StatusSucceeded
	if upFailed {
		d.failures = append(d.failures, "the up run did not finish ("+runDescription(up, upErr)+"): the system may still be down")
	}

	d.readHeartbeat(upCtx)
	post, postErr := snapshot(upCtx, e, plan)

	report := d.buildReport(mode, down, up, post, postErr, upFailed)
	saveErr := error(nil)
	if _, _, err := Save(opts.Dir, report); err != nil {
		saveErr = err
		report.Notices = append(report.Notices, "the report could not be saved: "+err.Error())
	}

	switch {
	case upFailed:
		return report, fmt.Errorf("%w: %s", ErrRecoveryFailed, joinFailures(d.failures))
	case downFailed:
		return report, fmt.Errorf("%w: %s", ErrDownFailed, joinFailures(d.failures))
	case saveErr != nil:
		return report, fmt.Errorf("the drill finished but its report could not be saved: %w", saveErr)
	}
	return report, nil
}

func checkDrillAllowed(plan *dr.Plan, opts DrillOptions) error {
	if plan.Safety.LabOnly {
		return nil
	}
	if !opts.AllowProduction {
		return fmt.Errorf("%w: plan %q is not marked safety.labOnly; mark the plan as a lab or explicitly allow a drill on it", ErrDrillRefused, plan.Name)
	}
	if strings.TrimSpace(opts.Confirm) == "" {
		return fmt.Errorf("%w: a drill on a plan that is not a lab needs the confirmation phrase", ErrDrillRefused)
	}
	return nil
}

func withDefaults(env *dr.Env) *dr.Env {
	e := *env
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.Sleep == nil {
		e.Sleep = sleepContext
	}
	if e.Logf == nil {
		e.Logf = func(string, ...any) {}
	}
	return &e
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

func runDescription(run *dr.RunResult, err error) string {
	parts := []string{}
	if run != nil {
		parts = append(parts, "status "+string(run.Status))
		for _, step := range run.Steps {
			if step.Status == dr.StepFailed && step.Err != "" {
				parts = append(parts, fmt.Sprintf("%s: %s", step.Title, step.Err))
				break
			}
		}
	}
	if err != nil {
		parts = append(parts, err.Error())
	}
	if len(parts) == 0 {
		return "no result"
	}
	return strings.Join(parts, "; ")
}

func joinFailures(failures []string) string { return strings.Join(failures, "; ") }

// drill carries the state of one drill.
type drill struct {
	env    *dr.Env
	plan   *dr.Plan
	opts   DrillOptions
	runner dr.Runner

	started time.Time
	t0      time.Time

	baseline    []AppState
	baselineErr error

	figures  []RPOFigure
	failures []string
	notices  []string

	heartbeat *Heartbeat
	greens    map[string]time.Time
	end       time.Time
}

// collectEstimates gathers the RPO figures that exist before anything is stopped.
func (d *drill) collectEstimates(ctx context.Context) {
	covered := map[string]bool{}
	if d.opts.DBEstimates != nil {
		estimates, err := d.opts.DBEstimates(ctx, d.env, d.plan)
		if err != nil {
			d.notices = append(d.notices, "database RPO estimates are not available: "+err.Error())
		}
		for _, estimate := range estimates {
			namespace := ""
			for _, database := range d.plan.Databases {
				if database.Name == estimate.Name {
					namespace = database.Namespace
				}
			}
			d.figures = append(d.figures, figure(estimate.Name, namespace, estimate.Age, KindEstimated, orDefault(estimate.Source, "database"), estimate.Note))
			covered[estimate.Name] = true
		}
	}

	if d.opts.Backups == nil {
		d.notices = append(d.notices, "no backup source was configured, so the RPO is not estimated from backups")
		sortFigures(d.figures)
		return
	}
	t0 := d.t0
	namespaces := map[string]bool{}
	for _, app := range d.plan.Apps {
		namespaces[app.Namespace] = true
	}
	for _, database := range d.plan.Databases {
		namespaces[database.Namespace] = true
	}
	ordered := make([]string, 0, len(namespaces))
	for namespace := range namespaces {
		ordered = append(ordered, namespace)
	}
	sort.Strings(ordered)

	for _, namespace := range ordered {
		completed, found, err := d.opts.Backups(ctx, d.env, namespace)
		switch {
		case err != nil:
			d.notices = append(d.notices, fmt.Sprintf("could not read the backups of namespace %s: %v", namespace, err))
			continue
		case !found:
			d.notices = append(d.notices, fmt.Sprintf("no backup was found for namespace %s", namespace))
			continue
		}
		age := t0.Sub(completed)
		note := "newest completed backup finished at " + completed.UTC().Format(time.RFC3339) + "; no restore was tested"
		source := "backup:" + namespace
		for _, app := range d.plan.Apps {
			if app.Namespace == namespace {
				d.figures = append(d.figures, figure(app.Name, namespace, age, KindEstimated, source, note))
			}
		}
		for _, database := range d.plan.Databases {
			if database.Namespace == namespace && !covered[database.Name] {
				d.figures = append(d.figures, figure(database.Name, namespace, age, KindEstimated, source, note))
			}
		}
	}
	sortFigures(d.figures)
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// startHeartbeat installs the writer before T0 and returns the cleanup that always runs.
func (d *drill) startHeartbeat(ctx context.Context) func() {
	options := d.opts.Heartbeat
	if options == nil {
		return func() {}
	}
	if d.env.Client == nil {
		d.notices = append(d.notices, "the heartbeat was requested but there is no Kubernetes client")
		return func() {}
	}
	heartbeat := newHeartbeat(d.env, *options)
	if err := heartbeat.Start(ctx); err != nil {
		d.notices = append(d.notices, "the heartbeat could not be started, so the RPO is not measured: "+err.Error())
		_ = heartbeat.Remove(context.WithoutCancel(ctx))
		return func() {}
	}
	d.heartbeat = heartbeat
	return func() {
		// Best effort and independent of cancellation: never leave the writer behind.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = heartbeat.Remove(cleanup)
	}
}

// stopHeartbeat stops the writer: from now on the volume is "the data at the time of the disaster".
func (d *drill) stopHeartbeat(ctx context.Context) {
	if d.heartbeat == nil {
		return
	}
	if err := d.heartbeat.Stop(ctx); err != nil {
		d.notices = append(d.notices, "the heartbeat writer could not be stopped, so the RPO is not measured: "+err.Error())
		d.heartbeat = nil
	}
}

// readHeartbeat reads the newest timestamp on the volume after recovery: T0 minus it is the data that was lost.
func (d *drill) readHeartbeat(ctx context.Context) {
	if d.heartbeat == nil {
		return
	}
	last, err := d.heartbeat.ReadLast(ctx)
	if err != nil {
		d.notices = append(d.notices, "the heartbeat could not be read after recovery, so the RPO of the volume is not measured: "+err.Error())
		return
	}
	age := d.t0.Sub(last)
	note := fmt.Sprintf("last heartbeat on volume %s was written at %s; includes any clock difference between the node and this machine", d.heartbeat.PVC, last.UTC().Format(time.RFC3339))
	if age < 0 {
		age = 0
		note += " (it is after T0, so no data was lost)"
	}
	namespace := d.heartbeat.Namespace
	source := "heartbeat:" + d.heartbeat.PVC
	subjects := 0
	for _, app := range d.plan.Apps {
		if app.Namespace == namespace {
			d.figures = append(d.figures, figure(app.Name, namespace, age, KindMeasured, source, note))
			subjects++
		}
	}
	for _, database := range d.plan.Databases {
		if database.Namespace == namespace {
			d.figures = append(d.figures, figure(database.Name, namespace, age, KindMeasured, source, note))
			subjects++
		}
	}
	if subjects == 0 {
		d.figures = append(d.figures, figure("volume:"+d.heartbeat.PVC, namespace, age, KindMeasured, source, note))
	}
	sortFigures(d.figures)
}

// bringUp runs the up direction while a watcher per application records when it turns healthy.
func (d *drill) bringUp(ctx context.Context, scope dr.Scope, planners []dr.Planner, runOpts dr.RunOptions) (*dr.RunResult, error) {
	poll := d.opts.HealthPoll
	if poll <= 0 {
		poll = defaultHealthPoll
	}
	timeout := d.opts.HealthTimeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}

	watch := newWatchers(d.env, d.plan, d.baseline, d.baselineErr, poll, timeout)
	watch.start(ctx)

	up, err := d.runner.Run(ctx, d.env, d.plan, dr.DirUp, scope, planners, runOpts)

	failed := err != nil || up == nil || up.Status != dr.StatusSucceeded
	grace := d.opts.FailedUpGrace
	if grace <= 0 {
		grace = defaultFailedUpGrace
	}
	watch.finishUp(failed, grace)
	d.greens, d.end = watch.wait()
	d.notices = append(d.notices, watch.notices()...)
	return up, err
}

// ---- report assembly -----------------------------------------------------------------------------------------

func (d *drill) buildReport(mode string, down, up *dr.RunResult, post []AppState, postErr error, upFailed bool) *Report {
	figures := append([]RPOFigure(nil), d.figures...)
	sortFigures(figures)

	analysis := Analyze(AnalysisInput{
		Plan: d.plan, T0: d.t0, Down: down, Up: up, Greens: d.greens, End: d.end, Figures: figures,
	})

	report := &Report{
		Version: ReportVersion, ID: newReportID(d.t0), Plan: d.plan.Name, Context: d.plan.Context, Actor: d.opts.Actor,
		Mode: mode, Started: d.started, Finished: d.env.Now(), T0: d.t0,
		Stages: analysis.Stages, RTO: analysis.RTO, RPO: analysis.RPO, Apps: analysis.Apps, Databases: analysis.Databases,
		Figures: figures, Baseline: d.baseline, PostState: post,
		Failures: d.failures, Notices: append([]string(nil), d.notices...),
	}
	if down != nil {
		report.DownRun, report.DownSteps = down.ID, down.Steps
	}
	if up != nil {
		report.UpRun, report.UpSteps = up.ID, up.Steps
	}

	if d.baselineErr != nil {
		report.Notices = append(report.Notices, "the readiness baseline could not be taken: "+d.baselineErr.Error())
	}
	if postErr != nil {
		report.Notices = append(report.Notices, "the state after the drill could not be read: "+postErr.Error())
	}
	report.StateRestored = d.baselineErr == nil && postErr == nil && sameState(d.baseline, post)

	allHealthy := true
	for _, app := range analysis.Apps {
		if !app.Recovered {
			allHealthy = false
		}
	}
	switch {
	case upFailed || !allHealthy:
		report.Outcome = OutcomeNotRecovered
	case !report.StateRestored:
		report.Outcome = OutcomeDifferences
	default:
		report.Outcome = OutcomeRecovered
	}

	report.Verdict = analysis.Verdict
	if report.Outcome == OutcomeNotRecovered || len(d.failures) > 0 {
		report.Verdict = VerdictFail
	}
	return report
}

func newReportID(t0 time.Time) string {
	random := make([]byte, 3)
	if _, err := rand.Read(random); err != nil {
		random = []byte{0, 0, 0}
	}
	return "drill-" + t0.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random)
}

// sameState reports whether every application has the same workloads and replica counts as before, and is
// healthy again if it was healthy before.
func sameState(before, after []AppState) bool {
	if len(before) != len(after) {
		return false
	}
	index := map[string]AppState{}
	for _, state := range after {
		index[state.Namespace+"/"+state.App] = state
	}
	for _, was := range before {
		now, ok := index[was.Namespace+"/"+was.App]
		if !ok || len(now.Workloads) != len(was.Workloads) || (was.Healthy && !now.Healthy) {
			return false
		}
		current := map[string]int32{}
		for _, workload := range now.Workloads {
			current[workload.Kind+"/"+workload.Name] = workload.Desired
		}
		for _, workload := range was.Workloads {
			if desired, ok := current[workload.Kind+"/"+workload.Name]; !ok || desired != workload.Desired {
				return false
			}
		}
	}
	return true
}

// ---- readiness snapshots -------------------------------------------------------------------------------------

func snapshot(ctx context.Context, env *dr.Env, plan *dr.Plan) ([]AppState, error) {
	if env.Client == nil {
		return nil, errors.New("no Kubernetes client")
	}
	states := make([]AppState, 0, len(plan.Apps))
	for _, app := range plan.Apps {
		workloads, err := listWorkloads(ctx, env.Client, plan, app)
		if err != nil {
			return nil, fmt.Errorf("failed to read the workloads of %s/%s: %w", app.Namespace, app.Name, err)
		}
		states = append(states, AppState{App: app.Name, Namespace: app.Namespace, Healthy: workloadsHealthy(workloads, nil), Workloads: workloads})
	}
	return states, nil
}

// listWorkloads returns the Deployments and StatefulSets an application consists of: the ones it names, or
// every one in the namespace except the plan's StatefulSet databases and the heartbeat writer.
func listWorkloads(ctx context.Context, client kubernetes.Interface, plan *dr.Plan, app dr.App) ([]WorkloadState, error) {
	named := map[string]bool{}
	for _, name := range app.Workloads {
		named[name] = true
	}
	database := map[string]bool{}
	for _, db := range plan.Databases {
		if db.Namespace == app.Namespace {
			database[db.Ref] = true
		}
	}
	wanted := func(kind, name string) bool {
		if name == HeartbeatName {
			return false
		}
		if len(named) > 0 {
			return named[name]
		}
		return kind != "StatefulSet" || !database[name]
	}

	var workloads []WorkloadState
	deployments, err := client.AppsV1().Deployments(app.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range deployments.Items {
		item := &deployments.Items[i]
		if wanted("Deployment", item.Name) {
			workloads = append(workloads, workloadState("Deployment", item.Name, item.Spec.Replicas, item.Status.ReadyReplicas))
		}
	}
	statefulSets, err := client.AppsV1().StatefulSets(app.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range statefulSets.Items {
		item := &statefulSets.Items[i]
		if wanted("StatefulSet", item.Name) {
			workloads = append(workloads, workloadState("StatefulSet", item.Name, item.Spec.Replicas, item.Status.ReadyReplicas))
		}
	}
	sort.Slice(workloads, func(i, j int) bool {
		return workloads[i].Kind+"/"+workloads[i].Name < workloads[j].Kind+"/"+workloads[j].Name
	})
	return workloads, nil
}

func workloadState(kind, name string, replicas *int32, ready int32) WorkloadState {
	desired := int32(1) // the API default when spec.replicas is unset
	if replicas != nil {
		desired = *replicas
	}
	return WorkloadState{Kind: kind, Name: name, Desired: desired, Ready: ready}
}

// workloadsHealthy: every workload that is expected to run has replicas and all of them are ready. expected
// limits the check to some workloads (by Kind/Name); nil means every listed workload.
func workloadsHealthy(workloads []WorkloadState, expected map[string]bool) bool {
	checked := 0
	for _, workload := range workloads {
		if expected != nil && !expected[workload.Kind+"/"+workload.Name] {
			continue
		}
		checked++
		if workload.Desired == 0 || workload.Ready < workload.Desired {
			return false
		}
	}
	return checked > 0 || expected != nil
}

// ---- watchers --------------------------------------------------------------------------------------------------

// watchers poll the health of every application and remember when each first turned healthy.
type watchers struct {
	env      *dr.Env
	plan     *dr.Plan
	baseline map[string]AppState
	baseErr  error
	poll     time.Duration
	timeout  time.Duration

	mu       sync.Mutex
	greens   map[string]time.Time
	lastErr  map[string]string
	giveUpAt atomic.Int64 // unix nanoseconds, 0 = not set

	wg sync.WaitGroup
}

func newWatchers(env *dr.Env, plan *dr.Plan, baseline []AppState, baseErr error, poll, timeout time.Duration) *watchers {
	w := &watchers{env: env, plan: plan, baseErr: baseErr, poll: poll, timeout: timeout,
		baseline: map[string]AppState{}, greens: map[string]time.Time{}, lastErr: map[string]string{}}
	for _, state := range baseline {
		w.baseline[state.App] = state
	}
	return w
}

func (w *watchers) start(ctx context.Context) {
	started := w.env.Now()
	for _, app := range w.plan.Apps {
		w.wg.Add(1)
		go func(app dr.App) {
			defer w.wg.Done()
			deadline := started.Add(appTimeout(app, w.timeout))
			w.watch(ctx, app, deadline)
		}(app)
	}
}

func appTimeout(app dr.App, fallback time.Duration) time.Duration {
	if app.Health != nil {
		if d, ok := parseDuration(app.Health.Timeout); ok {
			return d
		}
	}
	return fallback
}

func (w *watchers) watch(ctx context.Context, app dr.App, deadline time.Time) {
	for {
		healthy, err := w.healthy(ctx, app)
		if healthy {
			w.mu.Lock()
			w.greens[app.Name] = w.env.Now()
			w.mu.Unlock()
			return
		}
		if err != nil {
			w.mu.Lock()
			w.lastErr[app.Name] = err.Error()
			w.mu.Unlock()
		}
		now := w.env.Now()
		if ctx.Err() != nil || now.After(deadline) {
			return
		}
		if giveUp := w.giveUpAt.Load(); giveUp != 0 && now.UnixNano() > giveUp {
			return
		}
		if err := w.env.Sleep(ctx, w.poll); err != nil {
			return
		}
	}
}

func (w *watchers) healthy(ctx context.Context, app dr.App) (bool, error) {
	if app.Health != nil && app.Health.Kind == "http" {
		if w.env.HTTP == nil {
			return false, errors.New("no HTTP checker is configured")
		}
		status, err := w.env.HTTP.Check(ctx, app.Health.URL)
		if err != nil {
			return false, err
		}
		expected := app.Health.Status
		if expected == 0 {
			expected = 200
		}
		return status == expected, nil
	}

	if w.env.Client == nil {
		return false, errors.New("no Kubernetes client")
	}
	workloads, err := listWorkloads(ctx, w.env.Client, w.plan, app)
	if err != nil {
		return false, err
	}
	// Only the workloads that were running before the drill are expected to come back.
	var expected map[string]bool
	if w.baseErr == nil {
		expected = map[string]bool{}
		for _, was := range w.baseline[app.Name].Workloads {
			if was.Desired > 0 {
				expected[was.Kind+"/"+was.Name] = true
			}
		}
	}
	return workloadsHealthy(workloads, expected), nil
}

// finishUp tells the watchers the up run ended; when it failed they stop soon instead of waiting for the full timeout.
func (w *watchers) finishUp(failed bool, grace time.Duration) {
	if failed {
		w.giveUpAt.Store(w.env.Now().Add(grace).UnixNano())
	}
}

// wait blocks until every watcher stopped and returns when each application turned healthy and the moment waiting ended.
func (w *watchers) wait() (map[string]time.Time, time.Time) {
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	greens := make(map[string]time.Time, len(w.greens))
	for name, at := range w.greens {
		greens[name] = at
	}
	return greens, w.env.Now()
}

// notices describes why applications that did not recover could not be verified.
func (w *watchers) notices() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	names := make([]string, 0, len(w.lastErr))
	for name := range w.lastErr {
		if _, ok := w.greens[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, fmt.Sprintf("the last health check of %s failed: %s", name, w.lastErr[name]))
	}
	return out
}
