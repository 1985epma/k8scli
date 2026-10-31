// Package engine runs disaster-recovery runbooks: it asks planners for the steps of each phase,
// applies them in order, waits for their gates, and keeps a ledger (to resume) and an audit log
// (to answer who did what) on disk. It never touches a cluster itself: everything goes through the
// interfaces of package dr.
//
// Guarantees:
//   - A real run needs the confirmation phrase of ConfirmPhrase, and a destructive step needs
//     "stop <context>" whatever the plan says.
//   - A read-only engine only does dry runs.
//   - One real run per context at a time (a lock file with the owner's pid).
//   - The ledger is rewritten atomically after every step, so a crash or a cancel leaves a run that
//     Resume can continue; steps whose Done reports true are not applied again.
//   - Every event of a real run is appended to audit.jsonl and flushed. If the audit log cannot be
//     written when the run starts, the run does not start.
//   - A dry run writes nothing to disk and changes nothing.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

const (
	defaultUnreachableTimeout = 10 * time.Minute
	defaultLockMaxAge         = 12 * time.Hour
	maxUnreachableBackoff     = 15 * time.Second
)

// Options configure an Engine. Only Dir normally needs a value; the rest exist for tests.
type Options struct {
	// Dir holds ledgers, locks, state and the audit log. Default: ~/.k8scli/dr.
	Dir string
	// ReadOnly makes every run that is not a dry run fail with dr.ErrReadOnly.
	ReadOnly bool
	// UnreachableTimeout is how long a real run waits for a planner that reports dr.ErrUnreachable
	// (for example the cluster is still starting). Default 10 minutes.
	UnreachableTimeout time.Duration
	// LockMaxAge is the age after which a run lock is considered abandoned. Default 12 hours.
	LockMaxAge time.Duration

	Now          func() time.Time
	Sleep        func(ctx context.Context, d time.Duration) error
	Pid          func() int
	ProcessAlive func(pid int) bool
}

// Engine implements dr.Runner.
type Engine struct {
	opts Options
	dir  string
	host string

	auditMu sync.Mutex
}

var _ dr.Runner = (*Engine)(nil)

// New returns an Engine. It does not touch the disk until a run needs to.
func New(opts Options) (*Engine, error) {
	if opts.Dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot find the home directory for the DR state: %w", err)
		}
		opts.Dir = filepath.Join(home, ".k8scli", "dr")
	}
	if opts.UnreachableTimeout <= 0 {
		opts.UnreachableTimeout = defaultUnreachableTimeout
	}
	if opts.LockMaxAge <= 0 {
		opts.LockMaxAge = defaultLockMaxAge
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleep
	}
	if opts.Pid == nil {
		opts.Pid = os.Getpid
	}
	if opts.ProcessAlive == nil {
		opts.ProcessAlive = processAlive
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return &Engine{opts: opts, dir: opts.Dir, host: host}, nil
}

// Dir is where the engine keeps its files.
func (e *Engine) Dir() string { return e.dir }

// StateDir is where a FileState for this engine should live.
func (e *Engine) StateDir() string { return filepath.Join(e.dir, "state") }

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run builds the runbook phase by phase and executes it.
//
// A run that was refused (bad input, no confirmation, read-only, lock held) returns a nil result.
// A run that started returns its result together with an error when it failed or was aborted: the
// error wraps the cause (errors.Is works), and the result says which step it was.
func (e *Engine) Run(ctx context.Context, env *dr.Env, plan *dr.Plan, dir dr.Direction, scope dr.Scope, planners []dr.Planner, opts dr.RunOptions) (*dr.RunResult, error) {
	if plan == nil || env == nil {
		return nil, errors.New("a plan and an environment are required")
	}
	if dir != dr.DirDown && dir != dr.DirUp {
		return nil, fmt.Errorf("unknown direction %q (use %q or %q)", dir, dr.DirDown, dr.DirUp)
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if err := plan.ValidateScope(scope); err != nil {
		return nil, err
	}
	if env.Context != "" && env.Context != plan.Context {
		return nil, fmt.Errorf("the plan acts on context %q but the run was set up for %q", plan.Context, env.Context)
	}
	if opts.DryRun && opts.Resume != "" {
		return nil, errors.New("a dry run cannot resume a run")
	}

	byPhase := map[dr.Phase][]dr.Planner{}
	for _, planner := range planners {
		if planner == nil {
			return nil, errors.New("a nil planner was given")
		}
		phase := planner.Phase()
		if !knownPhase(phase) {
			return nil, fmt.Errorf("a planner reports the unknown phase %q", phase)
		}
		byPhase[phase] = append(byPhase[phase], planner)
	}

	if !opts.DryRun {
		if e.opts.ReadOnly || opts.ReadOnly {
			return nil, dr.ErrReadOnly
		}
		if phrase := ConfirmPhrase(plan, dir, scope); phrase != "" && opts.Confirm != phrase {
			return nil, fmt.Errorf("%w: type %q to continue", dr.ErrConfirmationRequired, phrase)
		}
	}

	r := &run{
		e: e, plan: plan, dir: dir, scope: scope, planners: byPhase, opts: opts, dry: opts.DryRun,
		env: *env, planHash: hashJSON(plan), scopeHash: scopeHash(scope), seen: map[string]bool{},
	}
	r.env.Context = plan.Context
	if r.env.Now == nil {
		r.env.Now = e.opts.Now
	}
	if r.env.Sleep == nil {
		r.env.Sleep = e.opts.Sleep
	}
	if r.env.Logf == nil {
		r.env.Logf = func(string, ...any) {}
	}
	return r.execute(ctx)
}

func knownPhase(phase dr.Phase) bool { return dr.KnownPhase(phase) }

// run is the state of one Run call.
type run struct {
	e        *Engine
	env      dr.Env // a private copy with defaults filled in
	plan     *dr.Plan
	dir      dr.Direction
	scope    dr.Scope
	planners map[dr.Phase][]dr.Planner
	opts     dr.RunOptions
	dry      bool

	planHash  string
	scopeHash string

	res   *dr.RunResult
	prev  map[string]dr.StepResult // steps a resumed run already finished
	queue []dr.Step                // steps of the current phase that have not run yet
	seen  map[string]bool          // step ids, which must be unique in a run
	seq   int
}

func (r *run) now() time.Time { return r.env.Now() }

func (r *run) execute(ctx context.Context) (*dr.RunResult, error) {
	id, err := newRunID(r.now())
	if err != nil {
		return nil, err
	}
	if r.opts.Resume != "" {
		id = r.opts.Resume
	}

	if !r.dry {
		release, err := r.e.acquireLock(r.plan.Context, id)
		if err != nil {
			return nil, err
		}
		defer release()
	}

	r.res = &dr.RunResult{
		ID: id, Plan: r.plan.Name, Context: r.plan.Context, Direction: r.dir,
		Status: dr.StatusRunning, Actor: r.opts.Actor, Started: r.now(), Steps: []dr.StepResult{},
	}
	if r.dry {
		r.res.Status = dr.StatusDryRun
	}

	startMsg := "started"
	switch {
	case r.dry:
		startMsg = "dry run"
	case r.opts.Resume != "":
		if err := r.loadResume(); err != nil {
			return nil, err
		}
		startMsg = "resumed"
	}

	// Fail closed: a real run that cannot be audited does not start.
	if err := r.emit("run-start", "", startMsg, ""); err != nil {
		return nil, fmt.Errorf("cannot write the audit log, so the run was not started: %w", err)
	}
	r.persist()

	step, cause := r.runPhases(ctx)
	return r.finish(ctx, cause, step)
}

// loadResume reads the ledger of the run being resumed and checks that it belongs to this plan.
func (r *run) loadResume() error {
	prior, found, err := r.e.readLedger(r.opts.Resume)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("there is no run %q to resume", r.opts.Resume)
	}
	switch {
	case prior.Plan != r.plan.Name:
		return fmt.Errorf("run %s belongs to plan %q, not %q", prior.ID, prior.Plan, r.plan.Name)
	case prior.Context != r.plan.Context:
		return fmt.Errorf("run %s acted on context %q, not %q", prior.ID, prior.Context, r.plan.Context)
	case prior.Direction != r.dir:
		return fmt.Errorf("run %s went %s, not %s", prior.ID, prior.Direction, r.dir)
	case prior.Status == dr.StatusSucceeded:
		return fmt.Errorf("run %s already succeeded; there is nothing to resume", prior.ID)
	}

	r.res.Started = prior.Started
	if r.res.Actor == "" {
		r.res.Actor = prior.Actor
	}
	r.prev = map[string]dr.StepResult{}
	for _, step := range prior.Steps {
		if (step.Status == dr.StepDone || step.Status == dr.StepSkipped) && step.Err == "" {
			r.prev[step.ID] = step
		}
	}
	return nil
}

// runPhases walks the phases in order. It returns the title of the step that ended the run (empty
// when none did) and the error that ended it (nil on success).
func (r *run) runPhases(ctx context.Context) (string, error) {
	order := dr.DownOrder
	if r.dir == dr.DirUp {
		order = dr.UpOrder
	}

	for _, phase := range order {
		planners := r.planners[phase]
		if len(planners) == 0 || !r.scope.HasPhase(phase) {
			continue
		}

		var steps []dr.Step
		deferred := false
		for _, planner := range planners {
			got, err := r.planPhase(ctx, planner, phase)
			switch {
			case err == nil:
				steps = append(steps, got...)
			case errors.Is(err, dr.ErrUnreachable) && r.dry:
				deferred = true
			default:
				r.res.Steps = append(r.res.Steps, dr.StepResult{
					ID: "phase/" + string(phase), Title: fmt.Sprintf("Plan the %s phase", phase),
					Describe: "Work out the steps of this phase", Phase: phase, Risk: dr.RiskLow.String(),
					Status: dr.StepFailed, Err: err.Error(), Started: r.now(), Finished: r.now(),
				})
				return "", fmt.Errorf("cannot plan the %s phase: %w", phase, err)
			}
		}
		if deferred {
			r.res.Steps = append(r.res.Steps, dr.StepResult{
				ID: "phase/" + string(phase), Title: fmt.Sprintf("Phase %s: computed when the cluster is reachable", phase),
				Describe: "The steps of this phase depend on the cluster, which is not reachable yet; they are worked out during the real run",
				Phase:    phase, Risk: dr.RiskLow.String(), Status: dr.StepDeferred,
			})
			_ = r.emit("phase-deferred", "", fmt.Sprintf("phase %s is computed when the cluster is reachable", phase), "")
		}

		if err := r.checkSteps(phase, steps); err != nil {
			return "", err
		}

		r.queue = steps
		for i, step := range steps {
			r.queue = steps[i+1:]
			if r.dry {
				if err := r.planStep(ctx, step); err != nil {
					return "", err
				}
				continue
			}
			if stop, cause := r.execStep(ctx, step); stop {
				return step.Action.Title(), cause
			}
		}
		r.queue = nil
	}
	return "", nil
}

// checkSteps rejects malformed planner output before anything runs.
func (r *run) checkSteps(phase dr.Phase, steps []dr.Step) error {
	for i := range steps {
		step := &steps[i]
		if step.Action == nil {
			return fmt.Errorf("the %s phase returned a step without an action", phase)
		}
		if step.Phase == "" {
			step.Phase = phase
		}
		if step.Phase != phase {
			return fmt.Errorf("a planner for the %s phase returned the step %q for the %s phase", phase, step.Action.ID(), step.Phase)
		}
		if r.seen[step.Action.ID()] {
			return fmt.Errorf("the step id %q appears twice in this run", step.Action.ID())
		}
		r.seen[step.Action.ID()] = true
	}
	return nil
}

// planPhase asks one planner for its steps. While a real run waits for the cluster to come up it
// retries with a growing pause, up to the engine's UnreachableTimeout.
func (r *run) planPhase(ctx context.Context, planner dr.Planner, phase dr.Phase) ([]dr.Step, error) {
	deadline := r.now().Add(r.e.opts.UnreachableTimeout)
	wait := time.Second
	for {
		steps, err := planner.Steps(ctx, &r.env, r.plan, r.dir, r.scope)
		if !errors.Is(err, dr.ErrUnreachable) || r.dry {
			return steps, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !r.now().Before(deadline) {
			return nil, fmt.Errorf("%w (waited %s)", err, r.e.opts.UnreachableTimeout)
		}
		_ = r.emit("phase-wait", "", fmt.Sprintf("waiting for the cluster before planning the %s phase", phase), err.Error())
		if serr := r.env.Sleep(ctx, wait); serr != nil {
			return nil, serr
		}
		wait = min(wait*2, maxUnreachableBackoff)
	}
}

// planStep records what a dry run would do for one step.
func (r *run) planStep(ctx context.Context, step dr.Step) error {
	res := resultFor(step)
	done, err := step.Action.Done(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	msg := step.Action.Describe()
	if err == nil && done {
		res.Status = dr.StepSkipped
		msg += " (already satisfied)"
	}
	r.res.Steps = append(r.res.Steps, res)
	_ = r.emit("step-plan", step.Action.ID(), msg, "")
	return nil
}

// execStep applies one step. It returns true and the error that must end the run, or false to continue.
func (r *run) execStep(ctx context.Context, step dr.Step) (bool, error) {
	action := step.Action
	id := action.ID()
	res := resultFor(step)
	res.Started = r.now()
	prev, resumed := r.prev[id]

	_ = r.emit("step-start", id, action.Title(), "")

	done, err := action.Done(ctx)
	if err != nil {
		return r.stepFailed(ctx, step, res, fmt.Errorf("cannot check the current state: %w", err))
	}
	earlier := false // finished by the run being resumed, not by this one

	switch {
	case !done:
		// A destructive step always needs the stop phrase, even when the plan did not announce one
		// (the steps of a phase are only known while it runs).
		if action.Risk() == dr.RiskDestructive && r.opts.Confirm != stopPhrase(r.plan.Context) {
			return r.stepFailed(ctx, step, res, fmt.Errorf("%w: the step %q is destructive; type %q to allow it",
				dr.ErrConfirmationRequired, action.Title(), stopPhrase(r.plan.Context)))
		}
		if err := action.Apply(ctx); err != nil {
			return r.stepFailed(ctx, step, res, err)
		}
		res.Status = dr.StepDone
	case resumed && prev.Status == dr.StepDone:
		res.Status = dr.StepDone // applied by the run being resumed
		res.Started = prev.Started
		earlier = true
	default:
		res.Status = dr.StepSkipped
	}

	if step.Gate != nil {
		if resumed && prev.GateWaited != "" {
			res.GateWaited = prev.GateWaited // it passed before the interruption
		} else {
			_ = r.emit("gate-wait", id, "waiting for "+step.Gate.Name(), "")
			began := r.now()
			if err := step.Gate.Wait(ctx); err != nil {
				// A cancelled context is the person stopping the run, not the gate failing: stepFailed
				// reports it as an abort, so do not announce a gate failure first.
				if ctx.Err() == nil {
					_ = r.emit("gate-fail", id, step.Gate.Name(), err.Error())
				}
				return r.stepFailed(ctx, step, res, fmt.Errorf("gate %q: %w", step.Gate.Name(), err))
			}
			res.GateWaited = r.now().Sub(began).String()
			_ = r.emit("gate-ok", id, step.Gate.Name(), "")
		}
	}

	res.Finished = r.now()
	r.res.Steps = append(r.res.Steps, res)
	switch {
	case earlier:
		// Not applied again: the ledger keeps it as done (the run that did it), the event says it was earlier.
		_ = r.emit("step-skip", id, "already done in the previous run", "")
	case res.Status == dr.StepSkipped:
		_ = r.emit("step-skip", id, "already satisfied", "")
	default:
		_ = r.emit("step-done", id, action.Title(), "")
	}
	r.persist()
	return false, nil
}

// stepFailed records a failed step. An optional step is skipped with its error; a cancelled context
// is an abort; anything else ends the run.
func (r *run) stepFailed(ctx context.Context, step dr.Step, res dr.StepResult, cause error) (bool, error) {
	id := step.Action.ID()
	res.Finished = r.now()
	res.Err = cause.Error()

	if ctx.Err() != nil {
		res.Status = dr.StepAborted
		res.Err = "aborted: " + ctx.Err().Error() // not the step's own text ("gate ...: context canceled")
		r.res.Steps = append(r.res.Steps, res)
		return true, ctx.Err()
	}
	// A step for a person ends the run even when it is optional: whoever planned it decided that
	// nothing after it makes sense until the person has acted. It is never retried by the engine.
	if isManual(cause) {
		res.Status = dr.StepFailed
		r.res.Steps = append(r.res.Steps, res)
		_ = r.emit("step-manual", id, step.Action.Title(), cause.Error())
		return true, cause
	}
	if step.Optional {
		res.Status = dr.StepSkipped
		r.res.Steps = append(r.res.Steps, res)
		_ = r.emit("step-skip", id, "optional step failed, continuing", cause.Error())
		r.persist()
		return false, nil
	}
	res.Status = dr.StepFailed
	r.res.Steps = append(r.res.Steps, res)
	_ = r.emit("step-fail", id, step.Action.Title(), cause.Error())
	return true, cause
}

// finish closes the run: it lists what never ran as pending, stores the ledger and audits the end.
func (r *run) finish(ctx context.Context, cause error, step string) (*dr.RunResult, error) {
	r.res.Finished = r.now()
	for _, queued := range r.queue {
		r.res.Steps = append(r.res.Steps, resultFor(queued))
	}
	r.queue = nil

	switch {
	case cause == nil && r.dry:
		_ = r.emit("run-done", "", "dry run complete", "")
		return r.res, nil
	case cause == nil:
		r.res.Status = dr.StatusSucceeded
		r.persist()
		_ = r.emit("run-done", "", "succeeded", "")
		return r.res, nil
	case ctx.Err() != nil && errors.Is(cause, ctx.Err()):
		r.res.Status = dr.StatusAborted
		r.persist()
		_ = r.emit("run-abort", "", "aborted", cause.Error())
		return r.res, fmt.Errorf("run %s aborted: %w", r.res.ID, cause)
	case isManual(cause):
		// Failed-like and resumable, but reported as waiting for the person.
		r.res.Status = dr.StatusFailed
		r.persist()
		_ = r.emit("run-fail", "", "waiting for a person", cause.Error())
		return r.res, fmt.Errorf("run %s is waiting for a person at step %q: %w", r.res.ID, step, cause)
	default:
		r.res.Status = dr.StatusFailed
		r.persist()
		_ = r.emit("run-fail", "", "failed", cause.Error())
		if step != "" {
			return r.res, fmt.Errorf("run %s failed at step %q: %w", r.res.ID, step, cause)
		}
		return r.res, fmt.Errorf("run %s failed: %w", r.res.ID, cause)
	}
}

// persist stores the ledger: the steps so far plus the rest of the current phase as pending, so
// anyone watching sees what is coming. A dry run stores nothing.
func (r *run) persist() {
	if r.dry {
		return
	}
	snapshot := *r.res
	snapshot.Steps = append([]dr.StepResult{}, r.res.Steps...)
	for _, queued := range r.queue {
		snapshot.Steps = append(snapshot.Steps, resultFor(queued))
	}
	if err := r.e.writeLedger(&snapshot); err != nil {
		// The run goes on: stopping halfway through a shutdown is worse than losing resumability.
		r.env.Logf("dr: cannot write the ledger of run %s: %v", r.res.ID, err)
	}
}

// emit reports one event to the caller and, for real runs, to the audit log. It returns the audit
// error so the run start can fail closed; later callers ignore it after logging.
func (r *run) emit(kind, step, msg, errText string) error {
	r.seq++
	event := dr.Event{
		Time: r.now(), RunID: r.res.ID, Seq: r.seq, Actor: r.opts.Actor,
		Step: step, Kind: kind, Msg: msg, Err: errText,
	}
	if r.opts.OnEvent != nil {
		r.deliver(event)
	}
	if r.dry {
		return nil
	}
	err := r.e.appendAudit(AuditRecord{
		Event: event, Plan: r.plan.Name, Context: r.plan.Context, Direction: r.dir,
		PlanHash: r.planHash, ScopeHash: r.scopeHash,
	})
	if err != nil && kind != "run-start" {
		r.env.Logf("dr: cannot write the audit log: %v", err)
	}
	return err
}

// deliver calls the caller's callback; a panic in it must not take the run down.
func (r *run) deliver(event dr.Event) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.env.Logf("dr: the event callback panicked: %v", recovered)
		}
	}()
	r.opts.OnEvent(event)
}

// resultFor builds the pending record of a step.
func resultFor(step dr.Step) dr.StepResult {
	res := dr.StepResult{
		ID: step.Action.ID(), Title: step.Action.Title(), Describe: step.Action.Describe(),
		Phase: step.Phase, Risk: step.Action.Risk().String(), Equivalent: step.Action.Equivalent(),
		Status: dr.StepPending,
	}
	if step.Gate != nil {
		res.Gate = step.Gate.Name()
	}
	return res
}
