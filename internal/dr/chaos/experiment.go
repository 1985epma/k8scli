package chaos

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/measure"
)

// Defaults of an experiment.
const (
	DefaultWindow      = 30 * time.Second
	DefaultPoll        = 2 * time.Second
	DefaultMaxDuration = 5 * time.Minute
	// settlePolls is how many healthy polls in a row mean "recovered", and settleTime how long the system
	// must have been healthy: a ready-flapping pod must not count as recovered after a quarter second.
	settlePolls = 2
	settleTime  = time.Second
	// DefaultBurstWindow and DefaultBurstInterval: right after an injection the probes are checked every
	// 250ms, because the interesting failures are over in a second or two.
	DefaultBurstWindow   = 10 * time.Second
	DefaultBurstInterval = 250 * time.Millisecond
	// recoveryTimeout bounds the recovery action that always runs after an injection.
	recoveryTimeout = 5 * time.Minute
	// duringInterval is how often the probes are sampled WHILE the injection runs. A drain takes seconds
	// and its replacement pods can be ready before it returns, so waiting for it to finish would miss
	// the whole degradation.
	duringInterval = 500 * time.Millisecond
)

// Recoverable is an Action that knows how to undo itself (uncordon a node). KillPods returns nil: the
// controller recreates the pods.
type Recoverable interface {
	dr.Action
	Recovery() dr.Action
}

// Event is a line of the experiment's timeline.
type Event struct {
	At  time.Time `json:"at"`
	Msg string    `json:"msg"`
}

// ActionInfo describes an action in a result.
type ActionInfo struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Describe   string `json:"describe"`
	Equivalent string `json:"equivalent,omitempty"`
	Risk       string `json:"risk"`
}

func info(a dr.Action) *ActionInfo {
	if a == nil {
		return nil
	}
	return &ActionInfo{ID: a.ID(), Title: a.Title(), Describe: a.Describe(), Equivalent: a.Equivalent(), Risk: a.Risk().String()}
}

// ProbeResult is what one probe saw.
type ProbeResult struct {
	Name           string    `json:"name"`
	Samples        int       `json:"samples"`
	FirstUnhealthy time.Time `json:"firstUnhealthy,omitzero"`
	LastUnhealthy  time.Time `json:"lastUnhealthy,omitzero"`
	Detail         string    `json:"detail,omitempty"`
}

// RecoveryOutcome is what the automatic recovery action did.
type RecoveryOutcome struct {
	Action  string  `json:"action"`
	Applied bool    `json:"applied"`
	Seconds float64 `json:"seconds"`
	Err     string  `json:"err,omitempty"`
}

// Result is the record of one experiment. TimeToDetect, TimeToRecover and Stages use the measure
// package's types, so the figures can be embedded in a drill report as they are.
type Result struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Context string `json:"context"`
	Plan    string `json:"plan"`
	// Mode is "dry-run" or "executed".
	Mode      string      `json:"mode"`
	Injection *ActionInfo `json:"injection"`
	Recovery  *ActionInfo `json:"recovery,omitempty"`
	Probes    []string    `json:"probes"`

	Started  time.Time `json:"started"`
	Injected time.Time `json:"injected,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
	// InjectionSeconds is how long the injection call itself took (a drain waits for pods to terminate).
	InjectionSeconds float64 `json:"injectionSeconds,omitempty"`
	// InjectionSamples is how many times the probes were sampled while the injection was still running.
	InjectionSamples int `json:"injectionSamples,omitempty"`

	// SteadyBefore is true when every probe was healthy before anything was injected.
	SteadyBefore bool `json:"steadyBefore"`
	// Detected: some probe became unhealthy after the injection. Absorbed: none did within the window,
	// so the system took the failure without an outage.
	Detected  bool `json:"detected"`
	Absorbed  bool `json:"absorbed"`
	Recovered bool `json:"recovered"`

	// TimeToDetect and TimeToRecover are measured from the start of the injection (T0).
	TimeToDetect  *measure.Objective `json:"timeToDetect,omitempty"`
	TimeToRecover *measure.Objective `json:"timeToRecover,omitempty"`
	Stages        []measure.Stage    `json:"stages,omitempty"`

	ProbeResults []ProbeResult    `json:"probeResults,omitempty"`
	RecoveryRun  *RecoveryOutcome `json:"recoveryRun,omitempty"`
	Notes        []string         `json:"notes,omitempty"`
	Events       []Event          `json:"events,omitempty"`
	Error        string           `json:"error,omitempty"`
}

// Experiment couples one injection with the observation of what the system does about it.
type Experiment struct {
	Name      string
	Guard     Guard
	Injection dr.Action
	// Recovery undoes the injection after observing. When nil and Injection implements Recoverable, its
	// own recovery is used.
	Recovery dr.Action
	Probes   []Probe

	// Window is how long to watch for a degradation before declaring the failure absorbed (default 30s).
	Window time.Duration
	// Poll is the polling interval (default 2s).
	Poll time.Duration
	// MaxDuration bounds the whole observation (default 5m, at most MaxExperiment).
	MaxDuration time.Duration
	// BurstWindow is how long after the injection the probes are checked every BurstInterval instead of
	// every Poll (default 10s; a negative value turns the burst off). BurstInterval defaults to 250ms.
	BurstWindow   time.Duration
	BurstInterval time.Duration
	// Target is the recovery objective to compare with (optional).
	Target time.Duration

	// OnEvent receives timeline lines as they happen; it must not block.
	OnEvent func(Event)
}

func (e *Experiment) defaults() {
	if e.Window <= 0 {
		e.Window = DefaultWindow
	}
	if e.Poll <= 0 {
		e.Poll = DefaultPoll
	}
	if e.MaxDuration <= 0 {
		e.MaxDuration = DefaultMaxDuration
	}
	if e.MaxDuration > MaxExperiment {
		e.MaxDuration = MaxExperiment
	}
	if e.BurstWindow == 0 {
		e.BurstWindow = DefaultBurstWindow
	}
	if e.BurstInterval <= 0 {
		e.BurstInterval = DefaultBurstInterval
	}
	if e.Window > e.MaxDuration {
		e.Window = e.MaxDuration
	}
}

type recorder struct {
	env    *dr.Env
	result *Result
	emit   func(Event)
}

func (r *recorder) log(format string, args ...any) { r.logAt(now(r.env), format, args...) }

// logAt records an event that happened at a known time, such as a detection made by the sampler.
func (r *recorder) logAt(at time.Time, format string, args ...any) {
	ev := Event{At: at, Msg: fmt.Sprintf(format, args...)}
	r.result.Events = append(r.result.Events, ev)
	if r.emit != nil {
		r.emit(ev)
	}
}

// Run executes the experiment. With dryRun it validates every rule, checks the steady state and describes
// what would happen, without injecting anything. A real run:
//
//  1. checks the guard (lab plan, read-only, production phrase);
//  2. requires every probe to be healthy, otherwise nothing is injected;
//  3. injects, then polls the probes until the system recovers or MaxDuration passes;
//  4. always runs the recovery action once something was injected, even after an error or a cancel.
func (e *Experiment) Run(ctx context.Context, dryRun bool) (*Result, error) {
	e.defaults()
	env := e.Guard.Env
	res := &Result{Name: e.Name, Mode: "executed", Started: now(env), Injection: info(e.Injection)}
	if dryRun {
		res.Mode = "dry-run"
	}
	if e.Guard.Plan != nil {
		res.Plan = e.Guard.Plan.Name
	}
	if env != nil {
		res.Context = env.Context
	}
	res.ID = newID(res.Started)
	rec := &recorder{env: env, result: res, emit: e.OnEvent}

	if e.Injection == nil {
		return res, fail(res, fmt.Errorf("%w: no injection was given", dr.ErrPreflight))
	}
	if len(e.Probes) == 0 {
		return res, fail(res, ErrNothingToObserve)
	}
	for _, p := range e.Probes {
		res.Probes = append(res.Probes, p.Name())
	}
	recovery := e.recoveryAction()
	res.Recovery = info(recovery)

	// Rules first: a dry run reports the same refusals a real run would.
	if err := e.Guard.Lab(); err != nil {
		return res, fail(res, err)
	}
	if !dryRun {
		if err := e.Guard.Real(); err != nil {
			return res, fail(res, err)
		}
	}

	rec.log("steady state check on %d probe(s)", len(e.Probes))
	healthy, detail := e.allHealthy(ctx)
	res.SteadyBefore = healthy
	if !healthy {
		res.Notes = append(res.Notes, "not healthy before the injection: "+detail)
		if !dryRun {
			rec.log("not healthy before the injection (%s); nothing was injected", detail)
			return res, fail(res, fmt.Errorf("%w: %s", ErrUnhealthyStart, detail))
		}
	}
	if dryRun {
		rec.log("dry run: would inject %q and observe for up to %s", e.Injection.Title(), human(e.MaxDuration))
		res.Finished = now(env)
		return res, nil
	}

	return res, e.execute(ctx, rec, recovery)
}

func (e *Experiment) recoveryAction() dr.Action {
	if e.Recovery != nil {
		return e.Recovery
	}
	if r, ok := e.Injection.(Recoverable); ok {
		return r.Recovery()
	}
	return nil
}

func fail(res *Result, err error) error {
	res.Error = err.Error()
	return err
}

// allHealthy polls every probe once. A probe that errors counts as unhealthy with the error as detail.
func (e *Experiment) allHealthy(ctx context.Context) (bool, string) {
	ok, detail, _ := e.poll(ctx, nil)
	return ok, detail
}

// poll checks every probe and, when seen is given, updates what each probe has seen.
func (e *Experiment) poll(ctx context.Context, seen []ProbeResult) (bool, string, []ProbeResult) {
	at := now(e.Guard.Env)
	healthy := true
	var detail string
	for i, p := range e.Probes {
		ok, d, err := p.Check(ctx)
		if err != nil {
			ok, d = false, "probe error: "+err.Error()
		}
		if seen != nil {
			seen[i].Samples++
		}
		if !ok {
			healthy = false
			if detail != "" {
				detail += "; "
			}
			detail += p.Name() + ": " + d
			if seen != nil {
				if seen[i].FirstUnhealthy.IsZero() {
					seen[i].FirstUnhealthy = at
				}
				seen[i].LastUnhealthy = at
				seen[i].Detail = d
			}
		}
	}
	return healthy, detail, seen
}

func (e *Experiment) execute(ctx context.Context, rec *recorder, recovery dr.Action) error {
	env, res := e.Guard.Env, rec.result
	seen := make([]ProbeResult, len(e.Probes))
	for i, p := range e.Probes {
		seen[i].Name = p.Name()
	}

	t0 := now(env)
	rec.log("injecting: %s", e.Injection.Title())

	// Sample the probes while the injection runs; see duringInterval.
	stop := make(chan struct{})
	var (
		wg     sync.WaitGroup
		during duringResult
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		during = e.sampleDuring(ctx, stop, seen)
	}()
	injectErr := e.Injection.Apply(ctx)
	close(stop)
	wg.Wait()

	res.Injected = now(env)
	res.InjectionSeconds = seconds(res.Injected.Sub(t0))
	res.InjectionSamples = during.samples
	if !during.detectedAt.IsZero() {
		rec.logAt(during.detectedAt, "degradation detected after %s, while the injection was still running: %s", human(during.detectedAt.Sub(t0)), during.detail)
	}
	injected := injectErr == nil

	if injectErr != nil {
		rec.log("the injection failed: %v", injectErr)
		res.Error = injectErr.Error()
		// A drain can fail half way (cordoned, not drained): recovery still applies when it was a real attempt.
		injected = !isInjectionBlocked(injectErr)
	}

	if injectErr == nil {
		e.observe(ctx, rec, seen, t0, during.detectedAt)
	}

	// Recovery always runs once an injection was attempted for real, on a context that survives a cancel.
	if injected && recovery != nil {
		e.recover(ctx, rec, recovery)
	}
	res.Finished = now(env)
	res.ProbeResults = seen
	e.finish(res, t0)

	if injectErr != nil {
		return injectErr
	}
	return nil
}

// observe polls until the system recovers, absorbs the failure, or time runs out.
func (e *Experiment) observe(ctx context.Context, rec *recorder, seen []ProbeResult, t0, detectedDuring time.Time) {
	env, res := e.Guard.Env, rec.result
	deadline := res.Injected.Add(e.MaxDuration)
	var (
		detectedAt   = detectedDuring
		recoveredAt  time.Time
		firstHealthy time.Time
		streak       int
	)
	for {
		healthy, detail, _ := e.poll(ctx, seen)
		at := now(env)
		if !healthy && detectedAt.IsZero() {
			detectedAt = at
			rec.log("degradation detected after %s: %s", human(at.Sub(t0)), detail)
		}
		if healthy {
			streak++
			if streak == 1 {
				firstHealthy = at
			}
		} else {
			streak = 0
		}
		switch {
		case !detectedAt.IsZero() && streak >= settlePolls && at.Sub(firstHealthy) >= settleTime:
			recoveredAt = firstHealthy
			rec.log("recovered %s after the injection started", human(recoveredAt.Sub(t0)))
			e.record(res, t0, detectedAt, recoveredAt, true, false)
			return
		case detectedAt.IsZero() && !at.Before(res.Injected.Add(e.Window)):
			rec.log("no degradation within %s: the system absorbed the failure", human(e.Window))
			e.record(res, t0, time.Time{}, time.Time{}, false, true)
			return
		case !at.Before(deadline):
			rec.log("the system did not recover within %s", human(e.MaxDuration))
			e.record(res, t0, detectedAt, time.Time{}, false, false)
			return
		}
		interval := e.Poll
		if e.BurstWindow > 0 && at.Sub(res.Injected) < e.BurstWindow && e.BurstInterval < interval {
			interval = e.BurstInterval
		}
		if err := sleep(ctx, env, interval); err != nil {
			rec.log("observation stopped: %v", err)
			e.record(res, t0, detectedAt, time.Time{}, false, false)
			res.Notes = append(res.Notes, "the observation was interrupted: "+err.Error())
			return
		}
	}
}

func (e *Experiment) record(res *Result, t0, detectedAt, recoveredAt time.Time, recovered, absorbed bool) {
	res.Detected = !detectedAt.IsZero()
	res.Absorbed = absorbed
	res.Recovered = recovered || absorbed
	source := "chaos:" + e.Injection.ID()
	objective := func(d time.Duration) *measure.Objective {
		return &measure.Objective{Seconds: seconds(d), Human: human(d), Kind: measure.KindMeasured, Source: source, Verdict: measure.VerdictUnknown}
	}
	if res.Detected {
		res.TimeToDetect = objective(detectedAt.Sub(t0))
	}
	switch {
	case recovered:
		res.TimeToRecover = objective(recoveredAt.Sub(t0))
	case absorbed:
		res.TimeToRecover = objective(0)
		res.TimeToRecover.Source = "absorbed"
	}
	if res.TimeToRecover != nil && e.Target > 0 {
		res.TimeToRecover.Target = human(e.Target)
		res.TimeToRecover.TargetSeconds = seconds(e.Target)
		res.TimeToRecover.Verdict = measure.VerdictFail
		if time.Duration(res.TimeToRecover.Seconds*float64(time.Second)) <= e.Target {
			res.TimeToRecover.Verdict = measure.VerdictPass
		}
	}
	stage := func(key, title string, at time.Time) {
		if at.IsZero() {
			return
		}
		d := at.Sub(t0)
		res.Stages = append(res.Stages, measure.Stage{Key: key, Title: title, At: at, Seconds: seconds(d), Human: human(d)})
	}
	stage(measure.StageDeclared, "Injection started", t0)
	stage("injected", "Injection finished", res.Injected)
	stage("detected", "Degradation detected", detectedAt)
	stage("recovered", "System healthy again", recoveredAt)
}

func (e *Experiment) recover(ctx context.Context, rec *recorder, action dr.Action) {
	env, res := e.Guard.Env, rec.result
	started := now(env)
	rec.log("recovery: %s", action.Title())
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recoveryTimeout)
	defer cancel()

	out := &RecoveryOutcome{Action: action.Title()}
	done, err := action.Done(recCtx)
	if err == nil && !done {
		err = action.Apply(recCtx)
	}
	out.Applied = err == nil
	out.Seconds = seconds(now(env).Sub(started))
	if err != nil {
		out.Err = err.Error()
		rec.log("recovery failed: %v", err)
		res.Notes = append(res.Notes, "RECOVERY FAILED: "+err.Error()+" — undo it by hand: "+action.Equivalent())
	} else {
		rec.log("recovery done in %s", human(now(env).Sub(started)))
	}
	res.RecoveryRun = out
	if errors.Is(err, context.Canceled) {
		res.Notes = append(res.Notes, "the recovery was cut short")
	}
}

// duringResult is what the sampler saw while the injection was running.
type duringResult struct {
	samples    int
	detectedAt time.Time
	detail     string
}

// sampleDuring polls the probes until stop is closed. It runs beside the injection and uses a real timer:
// it is only about not missing a short degradation, not about the experiment's own timeline.
func (e *Experiment) sampleDuring(ctx context.Context, stop <-chan struct{}, seen []ProbeResult) duringResult {
	var out duringResult
	interval := e.Poll
	if interval > duringInterval {
		interval = duringInterval
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return out
		case <-ctx.Done():
			return out
		case <-timer.C:
		}
		healthy, detail, _ := e.poll(ctx, seen)
		out.samples++
		if !healthy && out.detectedAt.IsZero() {
			out.detectedAt, out.detail = now(e.Guard.Env), detail
		}
		timer.Reset(interval)
	}
}

func (e *Experiment) finish(res *Result, t0 time.Time) {
	_ = t0
	if !res.Absorbed {
		return
	}
	during := e.Poll
	if during > duringInterval {
		during = duringInterval
	}
	after := fmt.Sprintf("every %s afterwards", human(e.Poll))
	if e.BurstWindow > 0 && e.BurstInterval < e.Poll {
		after = fmt.Sprintf("every %s for the first %s afterwards, then every %s", human(e.BurstInterval), human(e.BurstWindow), human(e.Poll))
	}
	res.Notes = append(res.Notes, fmt.Sprintf(
		"\"absorbed\" means no unhealthy sample was seen: the apps were checked every %s while the injection ran (%d sample(s)) and %s, so a degradation shorter than that could go unseen",
		human(during), res.InjectionSamples, after))
}

// ---- persistence --------------------------------------------------------------------------------------------

func newID(at time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return "chaos-" + at.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// auditLine is one line of chaos/audit.jsonl.
type auditLine struct {
	Time      time.Time `json:"time"`
	ID        string    `json:"id"`
	Actor     string    `json:"actor,omitempty"`
	Context   string    `json:"context"`
	Plan      string    `json:"plan"`
	Injection string    `json:"injection"`
	Mode      string    `json:"mode"`
	Detected  bool      `json:"detected"`
	Absorbed  bool      `json:"absorbed"`
	Recovered bool      `json:"recovered"`
	Error     string    `json:"error,omitempty"`
}

// Record stores a result under dir/chaos/<id>.json and appends a line to dir/chaos/audit.jsonl. Both are
// private to the user (0600 in a 0700 directory). Dry runs are not recorded.
func Record(dir, actor string, res *Result) (string, error) {
	if res == nil || res.Mode == "dry-run" {
		return "", nil
	}
	folder := filepath.Join(dir, "chaos")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", folder, err)
	}
	// Everything below is written through a root, so no name can escape the chaos folder.
	root, err := os.OpenRoot(folder)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", folder, err)
	}
	defer func() { _ = root.Close() }()

	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", err
	}
	name := res.ID + ".json"
	file := filepath.Join(folder, name)
	out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("write %s: %w", file, err)
	}
	if _, err := out.Write(append(data, '\n')); err != nil {
		_ = out.Close()
		return "", fmt.Errorf("write %s: %w", file, err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", file, err)
	}

	line, err := json.Marshal(auditLine{
		Time: res.Started, ID: res.ID, Actor: actor, Context: res.Context, Plan: res.Plan, Injection: res.Injection.ID,
		Mode: res.Mode, Detected: res.Detected, Absorbed: res.Absorbed, Recovered: res.Recovered, Error: res.Error,
	})
	if err != nil {
		return file, err
	}
	f, err := root.OpenFile("audit.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return file, fmt.Errorf("open the audit log: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return file, fmt.Errorf("write the audit log: %w", err)
	}
	return file, f.Sync()
}
