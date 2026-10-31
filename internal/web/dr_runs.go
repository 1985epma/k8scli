package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/engine"
	"github.com/1985epma/k8scli/internal/dr/measure"
)

// webActor is what the audit log records for runs started here.
const webActor = "web"

// runIDShape is the shape of an engine run id (the engine validates it again); drillHandle is the
// shape of the handle this server gives a drill while it runs.
var (
	runIDShape  = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)
	drillHandle = regexp.MustCompile(`^drill-[0-9a-f]{8}$`)
)

// ---- requests ----------------------------------------------------------------------------------------

type drScope struct {
	Apps      []string `json:"apps"`
	Databases []string `json:"databases"`
	Phases    []string `json:"phases"`
}

type drRunRequest struct {
	Plan      string  `json:"plan"`
	Direction string  `json:"direction"`
	Scope     drScope `json:"scope"`
	DryRun    bool    `json:"dryRun"`
	Confirm   string  `json:"confirm"`
}

var knownPhases = map[string]dr.Phase{
	string(dr.PhasePrepare):   dr.PhasePrepare,
	string(dr.PhaseApps):      dr.PhaseApps,
	string(dr.PhaseDatabases): dr.PhaseDatabases,
	string(dr.PhaseNodes):     dr.PhaseNodes,
	string(dr.PhasePower):     dr.PhasePower,
}

const maxScopeEntries = 100

// toScope converts and validates the scope of a request against the plan.
func (s drScope) toScope(plan *dr.Plan) (dr.Scope, error) {
	if len(s.Apps) > maxScopeEntries || len(s.Databases) > maxScopeEntries || len(s.Phases) > len(knownPhases) {
		return dr.Scope{}, drBadRequest("invalid_scope", "the scope lists too many entries")
	}
	scope := dr.Scope{Apps: append([]string{}, s.Apps...), Databases: append([]string{}, s.Databases...)}
	for _, phase := range s.Phases {
		known, ok := knownPhases[phase]
		if !ok {
			return dr.Scope{}, drBadRequest("invalid_scope", fmt.Sprintf("unknown phase %q (use prepare, apps, databases, nodes or power)", phase))
		}
		scope.Phases = append(scope.Phases, known)
	}
	if err := plan.ValidateScope(scope); err != nil {
		return dr.Scope{}, drBadRequest("invalid_scope", err.Error())
	}
	return scope, nil
}

func parseDirection(value string) (dr.Direction, error) {
	switch dr.Direction(value) {
	case dr.DirDown, dr.DirUp:
		return dr.Direction(value), nil
	}
	return "", drBadRequest("invalid_direction", `direction must be "down" or "up"`)
}

// reachesPower reports whether a run touches the power phase: it lists it, or lists nothing and the plan
// has a provider.
func reachesPower(plan *dr.Plan, scope dr.Scope) bool {
	if len(scope.Phases) == 0 {
		return plan.Provider != nil
	}
	return scope.HasPhase(dr.PhasePower)
}

// authorizeRun applies, in order, the gates of a real run: the feature flag, read-only mode, the power
// rule and the typed phrase. It returns the phrase that was required ("" when none).
func (s *Server) authorizeRun(plan *dr.Plan, dir dr.Direction, scope dr.Scope, confirm string) (string, error) {
	if !s.opts.AllowDRRuns {
		return "", &drError{status: http.StatusForbidden, code: "dr_runs_disabled",
			msg: "real DR runs are off: start the console with --allow-dr-runs to enable them", hint: "Dry runs, checks, exports and reports stay available."}
	}
	if s.opts.ReadOnly {
		return "", &drError{status: http.StatusForbidden, code: "read_only", msg: "the console is running in read-only mode"}
	}
	if reachesPower(plan, scope) && !plan.Safety.LabOnly && !s.opts.AllowDRPower {
		return "", &drError{status: http.StatusForbidden, code: "dr_power_disabled",
			msg: "this run powers machines or the cluster on or off: it needs a plan marked safety.labOnly, or --allow-dr-power"}
	}
	phrase := engine.ConfirmPhrase(plan, dir, scope)
	if phrase != "" && confirm != phrase {
		return phrase, &drError{status: http.StatusConflict, code: "confirmation_required",
			msg: fmt.Sprintf("type %q to confirm", phrase), phrase: phrase}
	}
	return phrase, nil
}

// blockedCode says which gate would stop a real run right now, or "" when none would. It does not
// look at the typed phrase, which the dialog asks for separately.
func (s *Server) blockedCode(plan *dr.Plan, scope dr.Scope) string {
	switch {
	case !s.opts.AllowDRRuns:
		return "dr_runs_disabled"
	case s.opts.ReadOnly:
		return "read_only"
	case reachesPower(plan, scope) && !plan.Safety.LabOnly && !s.opts.AllowDRPower:
		return "dr_power_disabled"
	}
	return ""
}

// ---- dry-run tickets ---------------------------------------------------------------------------------

// A real run needs a recent dry run of exactly the same plan file, direction and scope (ADR 0005, the
// console section): the person has seen the steps before they start.

func ticketKey(name string, raw []byte, dir dr.Direction, scope dr.Scope) string {
	sorted := func(values []string) string {
		copied := append([]string{}, values...)
		sort.Strings(copied)
		return strings.Join(copied, ",")
	}
	phases := make([]string, 0, len(scope.Phases))
	for _, phase := range scope.Phases {
		phases = append(phases, string(phase))
	}
	sum := sha256.Sum256(raw)
	return strings.Join([]string{name, hex.EncodeToString(sum[:]), string(dir), sorted(scope.Apps), sorted(scope.Databases), sorted(phases)}, "|")
}

func (d *drService) recordTicket(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.s.now()
	for k, at := range d.tickets {
		if now.Sub(at) > drTicketTTL {
			delete(d.tickets, k)
		}
	}
	if len(d.tickets) >= drMaxTickets {
		var oldest string
		var oldestAt time.Time
		for k, at := range d.tickets {
			if oldest == "" || at.Before(oldestAt) {
				oldest, oldestAt = k, at
			}
		}
		delete(d.tickets, oldest)
	}
	d.tickets[key] = now
}

func (d *drService) hasTicket(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	at, ok := d.tickets[key]
	return ok && d.s.now().Sub(at) <= drTicketTTL
}

func (d *drService) dropTicket(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.tickets, key)
}

// ---- live runs ---------------------------------------------------------------------------------------

// liveRun is a run or a drill started by this process. The ledger on disk is the record; this adds
// the goroutine, the live events and the way to abort.
type liveRun struct {
	kind      string // "run" or "drill"
	plan      string
	context   string
	direction dr.Direction
	started   time.Time
	cancel    context.CancelFunc
	done      chan struct{}
	ready     chan struct{}

	mu       sync.Mutex
	id       string
	readyOne sync.Once
	events   []dr.Event
	result   *dr.RunResult
	report   *measure.Report
	err      string
	typedErr error
	seenRuns []string
}

func (l *liveRun) onEvent(ev dr.Event) {
	l.mu.Lock()
	if len(l.events) >= drMaxEvents {
		copy(l.events, l.events[1:])
		l.events = l.events[:len(l.events)-1]
	}
	l.events = append(l.events, ev)
	if ev.RunID != "" {
		known := false
		for _, id := range l.seenRuns {
			known = known || id == ev.RunID
		}
		if !known && len(l.seenRuns) < 4 {
			l.seenRuns = append(l.seenRuns, ev.RunID)
		}
		if l.kind == "run" && l.id == "" {
			l.id = ev.RunID
		}
	}
	l.mu.Unlock()
	if ev.RunID != "" {
		l.readyOne.Do(func() { close(l.ready) })
	}
}

func (l *liveRun) snapshot() (events []dr.Event, result *dr.RunResult, report *measure.Report, errText string, seen []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]dr.Event{}, l.events...), l.result, l.report, l.err, append([]string{}, l.seenRuns...)
}

func (l *liveRun) isLive() bool {
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

func (l *liveRun) key() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.id
}

func (d *drService) register(l *liveRun) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs[l.key()] = l
	d.order = append(d.order, l.key())
	// Keep memory bounded: forget the oldest finished runs. The ledger on disk keeps them.
	for len(d.order) > drMaxRetained {
		dropped := false
		for i, id := range d.order {
			if run := d.runs[id]; run == nil || !run.isLive() {
				delete(d.runs, id)
				d.order = append(d.order[:i], d.order[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			return
		}
	}
}

func (d *drService) liveRun(id string) *liveRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runs[id]
}

func (d *drService) base() context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.baseCtx
}

type runSpec struct {
	plan      *dr.Plan
	planFile  string
	direction dr.Direction
	scope     dr.Scope
	confirm   string
	resume    string
}

// startRun launches a run in the background and returns once the engine has an id for it (or has
// refused it). The run belongs to the server, not to the request that started it.
func (d *drService) startRun(backend drBackend, spec runSpec) (*liveRun, error) {
	ctx, cancel := context.WithCancel(d.base())
	run := &liveRun{
		kind: "run", plan: spec.plan.Name, context: spec.plan.Context, direction: spec.direction,
		started: d.s.now(), cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), id: spec.resume,
	}
	if spec.resume != "" {
		if existing := d.liveRun(spec.resume); existing != nil && existing.isLive() {
			cancel()
			return nil, &drError{status: http.StatusConflict, code: "run_in_progress", msg: "that run is still going"}
		}
	}

	go func() {
		defer close(run.done)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				run.mu.Lock()
				run.err = fmt.Sprintf("the run stopped unexpectedly: %v", r)
				run.mu.Unlock()
			}
		}()
		result, err := backend.Run(ctx, spec.plan, spec.direction, spec.scope, dr.RunOptions{
			Confirm: spec.confirm, Resume: spec.resume, ReadOnly: d.s.opts.ReadOnly, Actor: webActor, OnEvent: run.onEvent,
		})
		run.mu.Lock()
		run.result = result
		if err != nil {
			run.err = err.Error()
		}
		if run.id == "" && result != nil {
			run.id = result.ID
		}
		run.mu.Unlock()
		if err != nil {
			run.finishWithError(err)
		}
	}()

	select {
	case <-run.ready:
	case <-run.done:
		// Finished before it announced itself: a refusal (confirmation, lock, pre-flight...).
		select {
		case <-run.ready:
		default:
			if err := run.finalError(); err != nil {
				return nil, err
			}
		}
	case <-time.After(drStartTimeout):
		cancel()
		<-run.done
		return nil, &drError{status: http.StatusGatewayTimeout, code: "start_timeout", msg: "the run did not start in time"}
	}
	if run.key() == "" {
		return nil, &drError{status: http.StatusInternalServerError, code: "no_run_id", msg: "the run finished without an id"}
	}
	d.register(run)
	return run, nil
}

// finalErr keeps the typed error of a finished run so a refusal can be mapped to its HTTP code.
func (l *liveRun) finishWithError(err error) {
	l.mu.Lock()
	l.typedErr = err
	l.mu.Unlock()
}

func (l *liveRun) finalError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.typedErr
}

// ---- side files: what the ledger does not keep ------------------------------------------------------

// webRunMeta remembers which plan file and scope a run started here used, so a failed run can be
// resumed with exactly the same scope (the engine's ledger does not store it).
type webRunMeta struct {
	Plan      string    `json:"plan"`
	Direction string    `json:"direction"`
	Scope     drScope   `json:"scope"`
	Started   time.Time `json:"started"`
}

func scopeToWire(scope dr.Scope) drScope {
	out := drScope{Apps: append([]string{}, scope.Apps...), Databases: append([]string{}, scope.Databases...), Phases: []string{}}
	for _, phase := range scope.Phases {
		out.Phases = append(out.Phases, string(phase))
	}
	return out
}

func (d *drService) writeMeta(id string, meta webRunMeta) error {
	if !runIDShape.MatchString(id) {
		return fmt.Errorf("invalid run id")
	}
	dir := d.sidecarDir()
	// 0700/0600 are best effort on Windows (no Unix permission bits).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	temp := filepath.Join(dir, ".tmp-"+hex.EncodeToString(suffix))
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, filepath.Join(dir, id+".json")); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

func (d *drService) readMeta(id string) (*webRunMeta, bool) {
	if !runIDShape.MatchString(id) {
		return nil, false
	}
	// #nosec G304 -- id matches runIDShape, so the path stays inside the side-file directory.
	data, err := os.ReadFile(filepath.Join(d.sidecarDir(), id+".json"))
	if err != nil {
		return nil, false
	}
	var meta webRunMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, false
	}
	return &meta, true
}

// ---- POST /api/dr/runs -------------------------------------------------------------------------------

type drRunView struct {
	dr.RunResult
	Kind   string     `json:"kind"`
	Live   bool       `json:"live"`
	Events []dr.Event `json:"events,omitempty"`
}

type drDryRunResponse struct {
	dr.RunResult
	Kind          string `json:"kind"`
	Live          bool   `json:"live"`
	DryRun        bool   `json:"dryRun"`
	ConfirmPhrase string `json:"confirmPhrase"`
	ReachesPower  bool   `json:"reachesPower"`
	CanRun        bool   `json:"canRun"`
	Blocked       string `json:"blocked,omitempty"`
}

type drStartedResponse struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Plan      string `json:"plan"`
	Context   string `json:"context"`
	Direction string `json:"direction,omitempty"`
}

func (s *Server) handleDRStartRun(w http.ResponseWriter, r *http.Request) {
	var req drRunRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	direction, err := parseDirection(req.Direction)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	plan, file, err := s.loadPlan(req.Plan)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	scope, err := req.Scope.toScope(plan)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	backend, err := s.dr.backend()
	if err != nil {
		writeDRErr(w, err)
		return
	}
	key := ticketKey(req.Plan, file.Raw, direction, scope)

	if req.DryRun {
		ctx, cancel := s.drRead(r)
		defer cancel()
		result, err := backend.Run(ctx, plan, direction, scope, dr.RunOptions{DryRun: true, ReadOnly: s.opts.ReadOnly, Actor: webActor})
		// A dry run that cannot plan (a workload is missing, say) still returns what it found, with the
		// failing step, so the person sees why. No ticket is given for it: it cannot be run.
		planFailed := err != nil && result != nil && result.Status == dr.StatusFailed
		if err != nil && !planFailed {
			writeDRErr(w, err)
			return
		}
		if result == nil {
			result = &dr.RunResult{Plan: plan.Name, Context: plan.Context, Direction: direction, Status: dr.StatusDryRun, Steps: []dr.StepResult{}}
		}
		if result.Steps == nil {
			result.Steps = []dr.StepResult{}
		}
		blocked := s.blockedCode(plan, scope)
		if planFailed {
			blocked = "dry_run_failed"
		} else {
			s.dr.recordTicket(key)
		}
		writeJSON(w, http.StatusOK, drDryRunResponse{
			RunResult: *result, Kind: "run", DryRun: true,
			ConfirmPhrase: engine.ConfirmPhrase(plan, direction, scope),
			ReachesPower:  reachesPower(plan, scope), CanRun: blocked == "", Blocked: blocked,
		})
		return
	}

	if _, err := s.authorizeRun(plan, direction, scope, req.Confirm); err != nil {
		writeDRErr(w, err)
		return
	}
	if !s.dr.hasTicket(key) {
		writeDRErr(w, &drError{status: http.StatusConflict, code: "dry_run_required",
			msg: "review a dry run of this exact plan, direction and scope first (it stays valid for 30 minutes)"})
		return
	}

	run, err := s.dr.startRun(backend, runSpec{plan: plan, planFile: req.Plan, direction: direction, scope: scope, confirm: req.Confirm})
	if err != nil {
		writeDRErr(w, err)
		return
	}
	s.dr.dropTicket(key)
	// Only resuming from the console needs this record, so a failure to write it never fails the run.
	_ = s.dr.writeMeta(run.key(), webRunMeta{Plan: req.Plan, Direction: string(direction), Scope: scopeToWire(scope), Started: run.started})
	writeJSON(w, http.StatusAccepted, drStartedResponse{ID: run.key(), Kind: "run", Plan: plan.Name, Context: plan.Context, Direction: string(direction)})
}

// ---- resume and abort --------------------------------------------------------------------------------

type drResumeRequest struct {
	ID      string `json:"id"`
	Confirm string `json:"confirm"`
}

func (s *Server) handleDRResume(w http.ResponseWriter, r *http.Request) {
	var req drResumeRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if !runIDShape.MatchString(req.ID) {
		writeDRErr(w, drBadRequest("invalid_id", "that is not a run id"))
		return
	}
	ledger, err := s.dr.ledger()
	if err != nil {
		writeDRErr(w, err)
		return
	}
	prior, found, err := ledger.Status(r.Context(), req.ID)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	if !found {
		writeDRErr(w, &drError{status: http.StatusNotFound, code: "run_not_found", msg: "there is no such run"})
		return
	}
	meta, known := s.dr.readMeta(req.ID)
	if !known {
		writeDRErr(w, &drError{status: http.StatusConflict, code: "scope_unknown",
			msg: "this run was not started from the console, so its scope is not known here", hint: "Resume it with k8scli dr resume."})
		return
	}
	plan, _, err := s.loadPlan(meta.Plan)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	if plan.Name != prior.Plan || plan.Context != prior.Context || string(prior.Direction) != meta.Direction {
		writeDRErr(w, &drError{status: http.StatusConflict, code: "plan_changed", msg: "the plan no longer matches the run that is being resumed"})
		return
	}
	direction, err := parseDirection(meta.Direction)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	scope, err := meta.Scope.toScope(plan)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	if _, err := s.authorizeRun(plan, direction, scope, req.Confirm); err != nil {
		writeDRErr(w, err)
		return
	}
	backend, err := s.dr.backend()
	if err != nil {
		writeDRErr(w, err)
		return
	}
	run, err := s.dr.startRun(backend, runSpec{plan: plan, planFile: meta.Plan, direction: direction, scope: scope, confirm: req.Confirm, resume: req.ID})
	if err != nil {
		writeDRErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, drStartedResponse{ID: run.key(), Kind: "run", Plan: plan.Name, Context: plan.Context, Direction: string(direction)})
}

type drAbortRequest struct {
	ID string `json:"id"`
}

func (s *Server) handleDRAbort(w http.ResponseWriter, r *http.Request) {
	var req drAbortRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if !runIDShape.MatchString(req.ID) && !drillHandle.MatchString(req.ID) {
		writeDRErr(w, drBadRequest("invalid_id", "that is not a run id"))
		return
	}
	run := s.dr.liveRun(req.ID)
	if run == nil || !run.isLive() {
		writeDRErr(w, &drError{status: http.StatusNotFound, code: "not_live",
			msg:  "only runs that this console started and that are still going can be aborted here",
			hint: "For a run started elsewhere, stop its process (Ctrl+C); the ledger stays resumable."})
		return
	}
	run.cancel()
	writeJSON(w, http.StatusAccepted, map[string]any{"id": req.ID, "aborting": true})
}

// ---- GET /api/dr/runs and /api/dr/run ---------------------------------------------------------------

type drRunSummary struct {
	ID        string       `json:"id"`
	Plan      string       `json:"plan"`
	Context   string       `json:"context"`
	Direction dr.Direction `json:"direction"`
	Status    dr.RunStatus `json:"status"`
	Actor     string       `json:"actor,omitempty"`
	Started   time.Time    `json:"started"`
	Finished  *time.Time   `json:"finished,omitempty"`
	Live      bool         `json:"live"`
	Steps     int          `json:"steps"`
	Failed    string       `json:"failed,omitempty"`
	Resumable bool         `json:"resumable"`
}

func (s *Server) handleDRRuns(w http.ResponseWriter, r *http.Request) {
	limit := drListLimit
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed := 0
		if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed < 1 || parsed > 500 {
			writeDRErr(w, drBadRequest("invalid_limit", "limit must be between 1 and 500"))
			return
		}
		limit = parsed
	}
	ledger, err := s.dr.ledger()
	if err != nil {
		writeDRErr(w, err)
		return
	}
	stored, err := ledger.List(r.Context(), limit)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	out := make([]drRunSummary, 0, len(stored))
	for i := range stored {
		run := &stored[i]
		live := false
		if lr := s.dr.liveRun(run.ID); lr != nil {
			live = lr.isLive()
		}
		summary := drRunSummary{
			ID: run.ID, Plan: run.Plan, Context: run.Context, Direction: run.Direction, Status: run.Status,
			Actor: run.Actor, Started: run.Started, Live: live, Steps: len(run.Steps),
		}
		if !run.Finished.IsZero() {
			finished := run.Finished
			summary.Finished = &finished
		}
		for _, step := range run.Steps {
			if step.Status == dr.StepFailed {
				summary.Failed = step.Title
				break
			}
		}
		summary.Resumable = !live && (run.Status == dr.StatusFailed || run.Status == dr.StatusAborted)
		out = append(out, summary)
	}
	writeJSON(w, http.StatusOK, out)
}

type drDrillView struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"`
	Live     bool       `json:"live"`
	Plan     string     `json:"plan"`
	Context  string     `json:"context"`
	Status   string     `json:"status"`
	Started  time.Time  `json:"started"`
	DownRun  string     `json:"downRun,omitempty"`
	UpRun    string     `json:"upRun,omitempty"`
	ReportID string     `json:"reportId,omitempty"`
	Error    string     `json:"error,omitempty"`
	Events   []dr.Event `json:"events"`
}

func (s *Server) handleDRRun(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	switch {
	case drillHandle.MatchString(id):
		s.writeDrillView(w, id)
	case runIDShape.MatchString(id):
		s.writeRunView(w, r, id)
	default:
		writeDRErr(w, drBadRequest("invalid_id", "that is not a run id"))
	}
}

func (s *Server) writeDrillView(w http.ResponseWriter, id string) {
	lr := s.dr.liveRun(id)
	if lr == nil {
		writeDRErr(w, &drError{status: http.StatusNotFound, code: "run_not_found", msg: "there is no such drill (drills are only tracked while this console runs)"})
		return
	}
	events, _, report, errText, seen := lr.snapshot()
	live := lr.isLive()
	view := drDrillView{ID: id, Kind: "drill", Live: live, Plan: lr.plan, Context: lr.context, Started: lr.started, Error: errText, Events: events}
	if len(seen) > 0 {
		view.DownRun = seen[0]
	}
	if len(seen) > 1 {
		view.UpRun = seen[1]
	}
	switch {
	case live:
		view.Status = string(dr.StatusRunning)
	case report != nil:
		view.ReportID = report.ID
		view.Status = string(dr.StatusSucceeded)
		if report.Outcome != measure.Outcome("recovered") && errText != "" {
			view.Status = string(dr.StatusFailed)
		}
	case errText != "":
		view.Status = string(dr.StatusFailed)
	default:
		view.Status = string(dr.StatusAborted)
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) writeRunView(w http.ResponseWriter, r *http.Request, id string) {
	ledger, err := s.dr.ledger()
	if err != nil {
		writeDRErr(w, err)
		return
	}
	stored, found, err := ledger.Status(r.Context(), id)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	lr := s.dr.liveRun(id)
	if !found && lr == nil {
		writeDRErr(w, &drError{status: http.StatusNotFound, code: "run_not_found", msg: "there is no such run"})
		return
	}

	view := drRunView{Kind: "run"}
	if found {
		view.RunResult = *stored
	} else {
		view.RunResult = dr.RunResult{ID: id, Plan: lr.plan, Context: lr.context, Direction: lr.direction, Status: dr.StatusRunning, Started: lr.started, Steps: []dr.StepResult{}}
	}
	if view.Steps == nil {
		view.Steps = []dr.StepResult{}
	}
	if lr != nil {
		view.Live = lr.isLive()
		view.Events, _, _, _, _ = lr.snapshot()
	}
	writeJSON(w, http.StatusOK, view)
}

// ---- drills ------------------------------------------------------------------------------------------

type drDrillRequest struct {
	Plan    string `json:"plan"`
	Full    bool   `json:"full"`
	Confirm string `json:"confirm"`
}

func newDrillHandle() (string, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	return "drill-" + hex.EncodeToString(suffix), nil
}

// handleDRDrill starts a rehearsal. The web console only drills plans marked safety.labOnly: a drill
// stops things on purpose, and a production drill is a decision for the terminal.
func (s *Server) handleDRDrill(w http.ResponseWriter, r *http.Request) {
	var req drDrillRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	plan, _, err := s.loadPlan(req.Plan)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	scope := dr.Scope{}
	if !req.Full {
		scope.Phases = []dr.Phase{dr.PhaseApps, dr.PhaseDatabases}
	}
	if _, err := s.authorizeRun(plan, dr.DirDown, scope, req.Confirm); err != nil {
		writeDRErr(w, err)
		return
	}
	if !plan.Safety.LabOnly {
		writeDRErr(w, &drError{status: http.StatusForbidden, code: "drill_lab_only",
			msg: "only plans marked safety.labOnly can be drilled from the console", hint: "Run production drills from the terminal with k8scli dr drill."})
		return
	}
	backend, err := s.dr.backend()
	if err != nil {
		writeDRErr(w, err)
		return
	}
	handle, err := newDrillHandle()
	if err != nil {
		writeDRErr(w, err)
		return
	}

	ctx, cancel := context.WithCancel(s.dr.base())
	run := &liveRun{
		kind: "drill", plan: plan.Name, context: plan.Context, direction: dr.DirDown,
		started: s.now(), cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), id: handle,
	}
	go func() {
		defer close(run.done)
		defer cancel()
		defer func() {
			if rec := recover(); rec != nil {
				run.mu.Lock()
				run.err = fmt.Sprintf("the drill stopped unexpectedly: %v", rec)
				run.mu.Unlock()
			}
		}()
		report, err := backend.Drill(ctx, plan, measure.DrillOptions{
			Confirm: req.Confirm, Full: req.Full, Actor: webActor, Dir: s.dr.reportsDir(), OnEvent: run.onEvent,
		})
		run.mu.Lock()
		run.report = report
		if err != nil {
			run.err = err.Error()
			run.typedErr = err
		}
		run.mu.Unlock()
	}()

	// A refusal (lock, confirmation, pre-flight) comes back quickly and is reported as such; otherwise
	// the drill is accepted and the page follows it by handle.
	select {
	case <-run.ready:
	case <-run.done:
		if err := run.finalError(); err != nil && run.reportOrNil() == nil {
			writeDRErr(w, err)
			return
		}
	case <-time.After(drStartTimeout / 4):
	}
	s.dr.register(run)
	writeJSON(w, http.StatusAccepted, drStartedResponse{ID: handle, Kind: "drill", Plan: plan.Name, Context: plan.Context})
}

func (l *liveRun) reportOrNil() *measure.Report {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.report
}
