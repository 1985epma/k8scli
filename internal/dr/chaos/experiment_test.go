package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/measure"
)

// scriptedProbe is healthy or not as a function of time.
type scriptedProbe struct {
	name    string
	te      *testEnv
	healthy func(since time.Duration) bool
	// byCall, when set, decides by how many times the probe was checked instead of by time.
	byCall func(n int) bool
	// wall, when set, decides by REAL elapsed time since the probe was made (the fake clock does not move
	// while an injection really blocks).
	wall    func(elapsed time.Duration) bool
	created time.Time
	t0      time.Time
	calls   int
	onCall  func(n int)
}

func (p *scriptedProbe) Name() string { return p.name }
func (p *scriptedProbe) Check(context.Context) (bool, string, error) {
	p.calls++
	if p.onCall != nil {
		p.onCall(p.calls)
	}
	since := p.te.clock.Now().Sub(p.t0)
	if p.wall != nil {
		if p.wall(time.Since(p.created)) {
			return true, "", nil
		}
		return false, "1/3 ready", nil
	}
	if p.byCall != nil {
		if p.byCall(p.calls) {
			return true, "", nil
		}
		return false, "1/3 ready", nil
	}
	if p.healthy(since) {
		return true, "", nil
	}
	return false, "1/3 ready", nil
}

// fakeInjection records whether it ran and can take time or fail.
type fakeInjection struct {
	te    *testEnv
	takes time.Duration
	// block really waits, so a sampler running beside the injection has time to look.
	block    time.Duration
	err      error
	applied  int
	recovery *fakeRecovery
}

func (f *fakeInjection) ID() string         { return "chaos/fake:inject" }
func (f *fakeInjection) Title() string      { return "Inject the fake failure" }
func (f *fakeInjection) Describe() string   { return "Breaks things for the test." }
func (f *fakeInjection) Risk() dr.Risk      { return dr.RiskHigh }
func (f *fakeInjection) Equivalent() string { return "kubectl break things" }
func (f *fakeInjection) Done(context.Context) (bool, error) {
	return f.applied > 0, nil
}
func (f *fakeInjection) Apply(ctx context.Context) error {
	f.applied++
	if f.block > 0 {
		time.Sleep(f.block)
	}
	if f.takes > 0 {
		_ = f.te.clock.Sleep(ctx, f.takes)
	}
	return f.err
}
func (f *fakeInjection) Recovery() dr.Action {
	if f.recovery == nil {
		return nil
	}
	return f.recovery
}

type fakeRecovery struct {
	applied     int
	err         error
	ctxWasLive  bool
	alreadyDone bool
}

func (r *fakeRecovery) ID() string         { return "chaos/fake:recover" }
func (r *fakeRecovery) Title() string      { return "Undo the fake failure" }
func (r *fakeRecovery) Describe() string   { return "Undoes it." }
func (r *fakeRecovery) Risk() dr.Risk      { return dr.RiskLow }
func (r *fakeRecovery) Equivalent() string { return "kubectl undo" }
func (r *fakeRecovery) Done(context.Context) (bool, error) {
	return r.alreadyDone, nil
}
func (r *fakeRecovery) Apply(ctx context.Context) error {
	r.applied++
	r.ctxWasLive = ctx.Err() == nil
	return r.err
}

func experiment(te *testEnv, inj dr.Action, probes ...Probe) *Experiment {
	return &Experiment{
		Name: "test", Guard: te.guard(), Injection: inj, Probes: probes,
		Window: 10 * time.Second, Poll: 2 * time.Second, MaxDuration: 40 * time.Second,
		// The timeline tests below count polls exactly; the burst has its own tests.
		BurstWindow: -1,
	}
}

func newProbe(te *testEnv, healthy func(time.Duration) bool) *scriptedProbe {
	return &scriptedProbe{name: "app shop", te: te, healthy: healthy, t0: te.clock.Now()}
}

func TestExperimentAbsorbsAFailureTheSystemDoesNotNotice(t *testing.T) {
	te := newTestEnv(t)
	rec := &fakeRecovery{}
	inj := &fakeInjection{te: te, recovery: rec}
	res, err := experiment(te, inj, newProbe(te, func(time.Duration) bool { return true })).Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SteadyBefore || res.Detected || !res.Absorbed || !res.Recovered {
		t.Errorf("steady %v detected %v absorbed %v recovered %v", res.SteadyBefore, res.Detected, res.Absorbed, res.Recovered)
	}
	if res.TimeToDetect != nil {
		t.Errorf("nothing was detected, got %+v", res.TimeToDetect)
	}
	if res.TimeToRecover == nil || res.TimeToRecover.Seconds != 0 || res.TimeToRecover.Source != "absorbed" || res.TimeToRecover.Kind != measure.KindMeasured {
		t.Errorf("TimeToRecover = %+v", res.TimeToRecover)
	}
	if rec.applied != 1 {
		t.Errorf("the recovery must run once, ran %d times", rec.applied)
	}
	if res.RecoveryRun == nil || !res.RecoveryRun.Applied {
		t.Errorf("RecoveryRun = %+v", res.RecoveryRun)
	}
}

func TestExperimentMeasuresDetectionAndRecovery(t *testing.T) {
	te := newTestEnv(t)
	rec := &fakeRecovery{}
	inj := &fakeInjection{te: te, recovery: rec}
	// Healthy, broken from 3s to 9s after the start, healthy again.
	probe := newProbe(te, func(s time.Duration) bool { return s < 3*time.Second || s >= 9*time.Second })
	res, err := experiment(te, inj, probe).Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Detected || res.Absorbed || !res.Recovered {
		t.Fatalf("detected %v absorbed %v recovered %v", res.Detected, res.Absorbed, res.Recovered)
	}
	// Polls at +0 (healthy), +2 (healthy), +4 (broken => detected at 4s), ..., +10 (first healthy of the streak), +12.
	if res.TimeToDetect.Seconds != 4 || res.TimeToDetect.Human != "4s" {
		t.Errorf("TimeToDetect = %+v", res.TimeToDetect)
	}
	if res.TimeToRecover.Seconds != 10 || res.TimeToRecover.Human != "10s" || res.TimeToRecover.Source != "chaos:chaos/fake:inject" {
		t.Errorf("TimeToRecover = %+v", res.TimeToRecover)
	}
	var keys []string
	for _, s := range res.Stages {
		keys = append(keys, s.Key)
	}
	if got := strings.Join(keys, ","); got != "declared,injected,detected,recovered" {
		t.Errorf("stages = %s", got)
	}
	if len(res.ProbeResults) != 1 || res.ProbeResults[0].FirstUnhealthy.IsZero() || res.ProbeResults[0].Samples < 5 {
		t.Errorf("ProbeResults = %+v", res.ProbeResults)
	}
	if rec.applied != 1 {
		t.Errorf("recovery ran %d times", rec.applied)
	}
}

func TestExperimentComparesRecoveryWithATarget(t *testing.T) {
	for _, tc := range []struct {
		target time.Duration
		want   measure.Verdict
	}{{15 * time.Second, measure.VerdictPass}, {5 * time.Second, measure.VerdictFail}} {
		te := newTestEnv(t)
		probe := newProbe(te, func(s time.Duration) bool { return s < 3*time.Second || s >= 9*time.Second })
		e := experiment(te, &fakeInjection{te: te}, probe)
		e.Target = tc.target
		res, err := e.Run(context.Background(), false)
		if err != nil {
			t.Fatal(err)
		}
		if res.TimeToRecover.Verdict != tc.want || res.TimeToRecover.TargetSeconds != tc.target.Seconds() {
			t.Errorf("target %s: %+v", tc.target, res.TimeToRecover)
		}
	}
}

func TestExperimentThatNeverRecoversStillRunsTheRecovery(t *testing.T) {
	te := newTestEnv(t)
	rec := &fakeRecovery{}
	probe := newProbe(te, func(s time.Duration) bool { return s < 3*time.Second })
	res, err := experiment(te, &fakeInjection{te: te, recovery: rec}, probe).Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Detected || res.Recovered || res.TimeToRecover != nil {
		t.Errorf("detected %v recovered %v ttr %+v", res.Detected, res.Recovered, res.TimeToRecover)
	}
	if rec.applied != 1 {
		t.Errorf("the recovery must run even when the system does not recover, ran %d", rec.applied)
	}
	if res.Finished.Sub(res.Started) < 40*time.Second {
		t.Errorf("it should have waited the full max duration, waited %s", res.Finished.Sub(res.Started))
	}
}

func TestExperimentRefusesToInjectIntoAnUnhealthySystem(t *testing.T) {
	te := newTestEnv(t)
	inj := &fakeInjection{te: te}
	probe := newProbe(te, func(time.Duration) bool { return false })
	res, err := experiment(te, inj, probe).Run(context.Background(), false)
	mustErrorIs(t, err, ErrUnhealthyStart)
	if inj.applied != 0 {
		t.Error("nothing may be injected into a system that is already unhealthy")
	}
	if res.SteadyBefore || res.Error == "" || !strings.Contains(res.Notes[0], "not healthy") {
		t.Errorf("result = %+v", res)
	}
}

func TestExperimentDryRunInjectsNothing(t *testing.T) {
	te := newTestEnv(t)
	inj := &fakeInjection{te: te, recovery: &fakeRecovery{}}
	res, err := experiment(te, inj, newProbe(te, func(time.Duration) bool { return true })).Run(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if inj.applied != 0 || inj.recovery.applied != 0 {
		t.Error("a dry run must not inject or recover")
	}
	if res.Mode != "dry-run" || !res.SteadyBefore || res.Injection == nil || res.Injection.Equivalent != "kubectl break things" || res.Recovery == nil {
		t.Errorf("result = %+v", res)
	}
	if res.RecoveryRun != nil || res.TimeToRecover != nil {
		t.Error("a dry run measures nothing")
	}
	// An unhealthy system is reported, not refused, in a dry run.
	res, err = experiment(te, inj, newProbe(te, func(time.Duration) bool { return false })).Run(context.Background(), true)
	if err != nil || res.SteadyBefore {
		t.Errorf("dry run on an unhealthy system: %v %+v", err, res)
	}
}

func TestExperimentAppliesTheGuardToDryAndRealRuns(t *testing.T) {
	te := newTestEnv(t)
	inj := &fakeInjection{te: te}
	healthy := func() Probe { return newProbe(te, func(time.Duration) bool { return true }) }

	// Not a lab plan: refused in both modes.
	notLab := experiment(te, inj, healthy())
	notLab.Guard.Plan = &dr.Plan{Name: "x", Context: testContext}
	for _, dry := range []bool{true, false} {
		_, err := notLab.Run(context.Background(), dry)
		mustErrorIs(t, err, ErrNotLab)
	}

	// Production-looking context: the dry run passes, the real run needs the phrase.
	te.env.Context = "prod-lab"
	prod := experiment(te, inj, healthy())
	prod.Guard.Plan = &dr.Plan{Name: "x", Context: "prod-lab", Safety: dr.Safety{LabOnly: true}}
	if _, err := prod.Run(context.Background(), true); err != nil {
		t.Errorf("dry run: %v", err)
	}
	_, err := prod.Run(context.Background(), false)
	mustErrorIs(t, err, dr.ErrConfirmationRequired)
	prod.Guard.Confirm = "chaos prod-lab"
	if _, err := prod.Run(context.Background(), false); err != nil {
		t.Errorf("with the phrase: %v", err)
	}

	// Read-only.
	te.env.Context = testContext
	ro := experiment(te, inj, healthy())
	ro.Guard.ReadOnly = true
	_, err = ro.Run(context.Background(), false)
	mustErrorIs(t, err, dr.ErrReadOnly)
	if inj.applied != 1 {
		t.Errorf("only the allowed run may have injected, injected %d", inj.applied)
	}
}

func TestExperimentNeedsAnInjectionAndAProbe(t *testing.T) {
	te := newTestEnv(t)
	_, err := experiment(te, nil, newProbe(te, func(time.Duration) bool { return true })).Run(context.Background(), true)
	mustErrorIs(t, err, dr.ErrPreflight)
	_, err = experiment(te, &fakeInjection{te: te}).Run(context.Background(), true)
	mustErrorIs(t, err, ErrNothingToObserve)
}

func TestExperimentRecoversAfterAFailedInjectionButNotAfterARefusal(t *testing.T) {
	te := newTestEnv(t)
	healthy := func() Probe { return newProbe(te, func(time.Duration) bool { return true }) }

	// The cluster failed half way (a drain that timed out): undo what was done.
	rec := &fakeRecovery{}
	inj := &fakeInjection{te: te, err: errors.New("timed out draining"), recovery: rec}
	res, err := experiment(te, inj, healthy()).Run(context.Background(), false)
	if err == nil || rec.applied != 1 || res.Error == "" {
		t.Errorf("err %v, recovery %d, result error %q", err, rec.applied, res.Error)
	}

	// The injection refused to start (a guard): there is nothing to undo.
	rec = &fakeRecovery{}
	inj = &fakeInjection{te: te, err: ErrBlastRadius, recovery: rec}
	_, err = experiment(te, inj, healthy()).Run(context.Background(), false)
	mustErrorIs(t, err, ErrBlastRadius)
	if rec.applied != 0 {
		t.Error("a refused injection must not run a recovery")
	}
}

func TestExperimentRecoveryOutlivesACancelledContext(t *testing.T) {
	te := newTestEnv(t)
	rec := &fakeRecovery{}
	ctx, cancel := context.WithCancel(context.Background())
	probe := newProbe(te, func(s time.Duration) bool { return s < 3*time.Second })
	probe.onCall = func(n int) {
		if n == 6 {
			cancel()
		}
	}
	res, err := experiment(te, &fakeInjection{te: te, recovery: rec}, probe).Run(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if rec.applied != 1 || !rec.ctxWasLive {
		t.Errorf("the recovery must run on a live context after a cancel (ran %d, live %v)", rec.applied, rec.ctxWasLive)
	}
	if res.Recovered || !strings.Contains(strings.Join(res.Notes, " "), "interrupted") {
		t.Errorf("notes = %v recovered %v", res.Notes, res.Recovered)
	}
}

func TestExperimentReportsAFailedRecoveryLoudly(t *testing.T) {
	te := newTestEnv(t)
	rec := &fakeRecovery{err: errors.New("node is gone")}
	res, err := experiment(te, &fakeInjection{te: te, recovery: rec}, newProbe(te, func(time.Duration) bool { return true })).Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.RecoveryRun == nil || res.RecoveryRun.Applied || res.RecoveryRun.Err == "" {
		t.Errorf("RecoveryRun = %+v", res.RecoveryRun)
	}
	if got := strings.Join(res.Notes, " "); !strings.Contains(got, "RECOVERY FAILED") || !strings.Contains(got, "kubectl undo") {
		t.Errorf("the note should say how to undo it by hand: %q", got)
	}
}

func TestExperimentSkipsRecoveryThatIsAlreadyDone(t *testing.T) {
	te := newTestEnv(t)
	rec := &fakeRecovery{alreadyDone: true}
	_, err := experiment(te, &fakeInjection{te: te, recovery: rec}, newProbe(te, func(time.Duration) bool { return true })).Run(context.Background(), false)
	if err != nil || rec.applied != 0 {
		t.Errorf("err %v, applied %d", err, rec.applied)
	}
}

func TestAbsorbedOutcomeStatesItsResolution(t *testing.T) {
	te := newTestEnv(t)
	inj := &fakeInjection{te: te, takes: 8 * time.Second}
	res, err := experiment(te, inj, newProbe(te, func(time.Duration) bool { return true })).Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(res.Notes, " ")
	if res.InjectionSeconds != 8 || !res.Absorbed ||
		!strings.Contains(notes, `"absorbed" means no unhealthy sample was seen`) || !strings.Contains(notes, "every 500ms while the injection ran") || !strings.Contains(notes, "every 2s afterwards") {
		t.Errorf("injection seconds %v, notes %v", res.InjectionSeconds, res.Notes)
	}
	// A degraded outcome needs no such caveat.
	probe := newProbe(te, func(s time.Duration) bool { return s < 3*time.Second || s >= 9*time.Second })
	res, err = experiment(te, &fakeInjection{te: te}, probe).Run(context.Background(), false)
	if err != nil || len(res.Notes) != 0 {
		t.Errorf("a measured degradation has nothing to caveat: %v %v", err, res.Notes)
	}
}

// The case found live: a drain returned after ~2s, its replacement pods were already ready, and the
// experiment reported "absorbed" although the apps had been degraded in between.
func TestExperimentSeesADegradationThatEndsBeforeTheInjectionReturns(t *testing.T) {
	te := newTestEnv(t)
	// Healthy, broken from 60ms to 200ms of real time, healthy again; the injection returns at 400ms,
	// after the apps recovered. Only a sampler running beside it can see the degradation.
	probe := &scriptedProbe{name: "app shop", te: te, t0: te.clock.Now(), created: time.Now()}
	probe.wall = func(d time.Duration) bool { return d < 60*time.Millisecond || d >= 200*time.Millisecond }
	inj := &fakeInjection{te: te, block: 400 * time.Millisecond}
	e := experiment(te, inj, probe)
	e.Poll = 20 * time.Millisecond

	res, err := e.Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Detected || res.Absorbed || !res.Recovered {
		t.Fatalf("detected %v absorbed %v recovered %v: a degradation during the injection must not be called absorbed", res.Detected, res.Absorbed, res.Recovered)
	}
	if res.InjectionSamples < 5 || res.TimeToDetect == nil {
		t.Errorf("samples %d, time to detect %+v", res.InjectionSamples, res.TimeToDetect)
	}
	if got := strings.Join(res.Notes, " "); strings.Contains(got, "absorbed") {
		t.Errorf("no absorbed caveat for a detected degradation: %q", got)
	}
	var sawEvent bool
	for _, ev := range res.Events {
		if strings.Contains(ev.Msg, "while the injection was still running") {
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Errorf("the timeline should say it was detected during the injection: %+v", res.Events)
	}
}

func TestExperimentBoundsItsOwnDuration(t *testing.T) {
	te := newTestEnv(t)
	e := experiment(te, &fakeInjection{te: te}, newProbe(te, func(time.Duration) bool { return true }))
	e.MaxDuration = 24 * time.Hour
	e.Window = 48 * time.Hour
	e.defaults()
	if e.MaxDuration != MaxExperiment || e.Window != MaxExperiment {
		t.Errorf("max %s window %s", e.MaxDuration, e.Window)
	}
	e = &Experiment{}
	e.defaults()
	if e.Window != DefaultWindow || e.Poll != DefaultPoll || e.MaxDuration != DefaultMaxDuration {
		t.Errorf("defaults: %s %s %s", e.Window, e.Poll, e.MaxDuration)
	}
}

func TestExperimentEmitsEvents(t *testing.T) {
	te := newTestEnv(t)
	var lines []string
	e := experiment(te, &fakeInjection{te: te}, newProbe(te, func(s time.Duration) bool { return s < 3*time.Second || s >= 9*time.Second }))
	e.OnEvent = func(ev Event) { lines = append(lines, ev.Msg) }
	res, err := e.Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "|")
	for _, want := range []string{"steady state", "injecting", "degradation detected", "recovered"} {
		if !strings.Contains(joined, want) {
			t.Errorf("events miss %q: %s", want, joined)
		}
	}
	if len(res.Events) != len(lines) {
		t.Errorf("%d events in the result, %d emitted", len(res.Events), len(lines))
	}
}

func TestRecordWritesPrivateFilesAndAnAuditLine(t *testing.T) {
	te := newTestEnv(t)
	dir := t.TempDir()
	res, err := experiment(te, &fakeInjection{te: te}, newProbe(te, func(time.Duration) bool { return true })).Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	file, err := Record(dir, "ana", res)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil || (permissionBitsMatter() && info.Mode().Perm() != 0o600) {
		t.Fatalf("result file: %v %v", err, info)
	}
	if dirInfo, _ := os.Stat(filepath.Join(dir, "chaos")); permissionBitsMatter() && dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v", dirInfo.Mode().Perm())
	}
	var back Result
	raw, _ := os.ReadFile(file)
	if err := json.Unmarshal(raw, &back); err != nil || back.ID != res.ID || !back.Absorbed {
		t.Errorf("round trip: %v %+v", err, back)
	}
	audit, err := os.ReadFile(filepath.Join(dir, "chaos", "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), `"actor":"ana"`) || !strings.Contains(string(audit), res.ID) {
		t.Errorf("audit = %q (%v)", audit, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "chaos", "audit.jsonl")); info.Mode().Perm() != 0o600 {
		t.Errorf("audit mode %v", info.Mode().Perm())
	}

	// A second record appends a second line; a dry run records nothing.
	if _, err := Record(dir, "ana", res); err != nil {
		t.Fatal(err)
	}
	audit, _ = os.ReadFile(filepath.Join(dir, "chaos", "audit.jsonl"))
	if n := strings.Count(string(audit), "\n"); n != 2 {
		t.Errorf("%d audit lines", n)
	}
	dry := &Result{Mode: "dry-run", ID: "x"}
	if path, err := Record(dir, "ana", dry); path != "" || err != nil {
		t.Errorf("dry run recorded: %q %v", path, err)
	}
	if path, err := Record(dir, "ana", nil); path != "" || err != nil {
		t.Errorf("nil recorded: %q %v", path, err)
	}
}

func TestResultIDsAreSortableAndUnique(t *testing.T) {
	a, b := newID(epoch), newID(epoch)
	if a == b || !strings.HasPrefix(a, "chaos-20260101-120000-") {
		t.Errorf("ids %q %q", a, b)
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0s", 400 * time.Millisecond: "400ms", 4 * time.Second: "4s", 125 * time.Second: "2m05s", 3725 * time.Second: "1h02m05s", -time.Second: "0s",
	} {
		if got := human(d); got != want {
			t.Errorf("human(%s) = %q; want %q", d, got, want)
		}
	}
}

func TestAnExplicitRecoveryReplacesTheInjectionsOwn(t *testing.T) {
	te := newTestEnv(t)
	own, explicit := &fakeRecovery{}, &fakeRecovery{}
	e := experiment(te, &fakeInjection{te: te, recovery: own}, newProbe(te, func(time.Duration) bool { return true }))
	e.Recovery = explicit
	if _, err := e.Run(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if explicit.applied != 1 || own.applied != 0 {
		t.Errorf("explicit ran %d times, the injection's own %d", explicit.applied, own.applied)
	}
}

func TestRealClockFallbacks(t *testing.T) {
	if time.Since(now(nil)) > time.Minute {
		t.Error("now(nil) must be the real clock")
	}
	if err := sleep(context.Background(), nil, time.Millisecond); err != nil {
		t.Errorf("sleep with the real timer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleep(ctx, nil, time.Hour); err == nil {
		t.Error("a cancelled sleep must return")
	}
	logf(nil, "ignored %d", 1) // must not panic
}

func TestTheBurstCatchesADegradationShorterThanThePollingInterval(t *testing.T) {
	// Broken from 300ms to 800ms of fake time, with a 2s poll: only the 250ms burst can see it.
	run := func(burst time.Duration) *Result {
		te := newTestEnv(t)
		probe := newProbe(te, func(s time.Duration) bool { return s < 300*time.Millisecond || s >= 800*time.Millisecond })
		e := experiment(te, &fakeInjection{te: te}, probe)
		e.BurstWindow = burst
		res, err := e.Run(context.Background(), false)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	off := run(-1)
	if !off.Absorbed || off.Detected {
		t.Fatalf("without the burst a half-second blip is invisible to a 2s poll: %+v", off)
	}
	on := run(0) // the default burst
	if !on.Detected || on.Absorbed || !on.Recovered {
		t.Fatalf("with the burst the blip must be measured: detected %v absorbed %v recovered %v", on.Detected, on.Absorbed, on.Recovered)
	}
	// Polls at 250ms (healthy), 500ms (broken => detected), 750ms (broken), 1000ms (first healthy), and the
	// system must stay healthy for a full second: recovered at 1s, confirmed at 2s.
	if on.TimeToDetect.Seconds != 0.5 || on.TimeToRecover.Seconds != 1 {
		t.Errorf("time to detect %+v, time to recover %+v", on.TimeToDetect, on.TimeToRecover)
	}
}

func TestRecoveredNeedsASecondOfHealthNotJustTwoPolls(t *testing.T) {
	// Healthy again for 400ms, broken again, then healthy for good: the flap must not count as recovered.
	te := newTestEnv(t)
	probe := newProbe(te, func(s time.Duration) bool {
		switch {
		case s < 300*time.Millisecond:
			return true
		case s < 1000*time.Millisecond:
			return false
		case s < 1500*time.Millisecond:
			return true
		case s < 1800*time.Millisecond:
			return false
		default:
			return true
		}
	})
	e := experiment(te, &fakeInjection{te: te}, probe)
	e.BurstWindow = 0
	res, err := e.Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Recovered || res.TimeToRecover.Seconds != 2 {
		t.Errorf("recovery must be the first healthy poll of the final stretch (2s, polls every 250ms), got %+v", res.TimeToRecover)
	}
}
