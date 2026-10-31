package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/measure"
)

// Plans used below (contexts dev and prod-eu exist in the test kubeconfig):
//
//	plain    context dev, no provider, not lab-only
//	lab      context dev, provider kind, lab-only
//	nolab    context dev, provider kind, NOT lab-only (power needs --allow-dr-power)
//	prod     context prod-eu, no provider
func writeRunPlans(t *testing.T, d *drEnv) {
	t.Helper()
	d.writePlan(t, "plain", planYAML("plain", "dev", false, false))
	d.writePlan(t, "lab", planYAML("lab", "dev", true, true))
	d.writePlan(t, "nolab", planYAML("nolab", "dev", false, true))
	d.writePlan(t, "prod", planYAML("prod", "prod-eu", false, false))
}

var appsOnly = drScope{Phases: []string{"apps", "databases"}}

func (d *drEnv) realRun(plan, direction string, scope drScope, confirm string) *httpRec {
	rec := d.do(http.MethodPost, "/api/dr/runs", drRunRequest{Plan: plan, Direction: direction, Scope: scope, Confirm: confirm})
	return &httpRec{code: rec.Code, body: rec.Body.String(), err: decodeErr(rec.Body.Bytes())}
}

type httpRec struct {
	code int
	body string
	err  drErrorResponse
}

func decodeErr(raw []byte) drErrorResponse {
	var out drErrorResponse
	_ = jsonUnmarshal(raw, &out)
	return out
}

// ---- dry runs ----------------------------------------------------------------------------------------

func TestDRDryRunReturnsTheStepsAndNeverStartsAnything(t *testing.T) {
	d := newDREnv(t, Options{})
	writeRunPlans(t, d)

	result := d.dryRun(t, "plain", "down", drScope{})
	if !result.DryRun || result.Status != dr.StatusDryRun || len(result.Steps) != 1 || result.Steps[0].Title != "Scale shop/api to 0" {
		t.Errorf("dry run = %+v", result)
	}
	if result.Kind != "run" || result.Live || result.ConfirmPhrase != "" || result.ReachesPower {
		t.Errorf("dry run flags = %+v", result)
	}
	calls := d.fake.runCalls()
	if len(calls) != 1 || !calls[0].Dry || calls[0].Opts.Actor != "web" || calls[0].Dir != dr.DirDown {
		t.Errorf("calls = %+v", calls)
	}
	if len(d.server.dr.runs) != 0 {
		t.Error("a dry run must not register a live run")
	}
	if len(d.fake.realCalls()) != 0 {
		t.Error("a dry run must not start a real run")
	}
}

func TestDRDryRunExplainsWhatWouldBlockARealRun(t *testing.T) {
	for name, tc := range map[string]struct {
		opts    Options
		plan    string
		scope   drScope
		blocked string
		canRun  bool
		phrase  string
		power   bool
	}{
		"flag off":               {Options{}, "plain", drScope{}, "dr_runs_disabled", false, "", false},
		"read-only":              {Options{AllowDRRuns: true, ReadOnly: true}, "plain", drScope{}, "read_only", false, "", false},
		"power without a lab":    {Options{AllowDRRuns: true}, "nolab", drScope{}, "dr_power_disabled", false, "stop dev", true},
		"power with the flag":    {Options{AllowDRRuns: true, AllowDRPower: true}, "nolab", drScope{}, "", true, "stop dev", true},
		"lab plan reaches power": {Options{AllowDRRuns: true}, "lab", drScope{}, "", true, "stop dev", true},
		"apps only skips power":  {Options{AllowDRRuns: true}, "nolab", appsOnly, "", true, "", false},
		"production":             {Options{AllowDRRuns: true}, "prod", drScope{}, "", true, "prod-eu", false},
	} {
		d := newDREnv(t, tc.opts)
		writeRunPlans(t, d)
		got := d.dryRun(t, tc.plan, "down", tc.scope)
		if got.Blocked != tc.blocked || got.CanRun != tc.canRun || got.ConfirmPhrase != tc.phrase || got.ReachesPower != tc.power {
			t.Errorf("%s: blocked=%q canRun=%v phrase=%q power=%v", name, got.Blocked, got.CanRun, got.ConfirmPhrase, got.ReachesPower)
		}
	}
}

func TestDRDryRunThatCannotPlanShowsTheFailingStepAndGivesNoTicket(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	d.fake.dryFails = errors.New(`app "web": workload "web" was not found in namespace team-a`)

	rec := d.do(http.MethodPost, "/api/dr/runs", drRunRequest{Plan: "plain", Direction: "down", Scope: appsOnly, DryRun: true})
	got := decode[drDryRunResponse](t, rec)
	if rec.Code != http.StatusOK || got.Status != dr.StatusFailed || got.CanRun || got.Blocked != "dry_run_failed" ||
		len(got.Steps) != 1 || !strings.Contains(got.Steps[0].Err, "was not found") {
		t.Fatalf("status %d: %+v", rec.Code, got)
	}

	// Nothing reviewed, nothing to run.
	if r := d.realRun("plain", "down", appsOnly, ""); r.err.Code != "dry_run_required" {
		t.Errorf("a failed dry run must not unlock a real run: %+v", r.err)
	}
}

func TestDRDryRunWorksInReadOnlyModeAndPassesReadOnlyToTheEngine(t *testing.T) {
	d := newDREnv(t, Options{ReadOnly: true})
	writeRunPlans(t, d)
	d.dryRun(t, "plain", "up", drScope{})
	if calls := d.fake.runCalls(); len(calls) != 1 || !calls[0].Opts.ReadOnly {
		t.Errorf("calls = %+v", calls)
	}
}

func TestDRRunRequestValidation(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	d.writePlan(t, "ghost", planYAML("ghost", "nowhere", false, false))

	for name, tc := range map[string]struct {
		req  drRunRequest
		want int
		code string
	}{
		"bad direction":  {drRunRequest{Plan: "plain", Direction: "sideways", DryRun: true}, 400, "invalid_direction"},
		"unknown plan":   {drRunRequest{Plan: "nope", Direction: "down", DryRun: true}, 404, "plan_not_found"},
		"bad plan name":  {drRunRequest{Plan: "../x", Direction: "down", DryRun: true}, 400, "invalid_name"},
		"invalid plan":   {drRunRequest{Plan: "ghost", Direction: "down", DryRun: true}, 400, "invalid_plan"},
		"unknown phase":  {drRunRequest{Plan: "plain", Direction: "down", DryRun: true, Scope: drScope{Phases: []string{"explode"}}}, 400, "invalid_scope"},
		"unknown app":    {drRunRequest{Plan: "plain", Direction: "down", DryRun: true, Scope: drScope{Apps: []string{"nope"}}}, 400, "invalid_scope"},
		"unknown db":     {drRunRequest{Plan: "plain", Direction: "down", DryRun: true, Scope: drScope{Databases: []string{"nope"}}}, 400, "invalid_scope"},
		"too many apps":  {drRunRequest{Plan: "plain", Direction: "down", DryRun: true, Scope: drScope{Apps: make([]string, 101)}}, 400, "invalid_scope"},
		"too many phase": {drRunRequest{Plan: "plain", Direction: "down", DryRun: true, Scope: drScope{Phases: []string{"apps", "apps", "apps", "apps", "apps", "apps"}}}, 400, "invalid_scope"},
	} {
		rec := d.do(http.MethodPost, "/api/dr/runs", tc.req)
		resp := decodeErr(rec.Body.Bytes())
		if rec.Code != tc.want || resp.Code != tc.code {
			t.Errorf("%s: %d %+v", name, rec.Code, resp)
		}
	}
	if rec := d.do(http.MethodPost, "/api/dr/runs", map[string]any{"plan": "plain", "direction": "down", "path": "/tmp/x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown request field = %d", rec.Code)
	}
	if len(d.fake.runCalls()) != 0 {
		t.Error("invalid requests must never reach the engine")
	}

	// An invalid plan reports every problem.
	rec := d.do(http.MethodPost, "/api/dr/runs", drRunRequest{Plan: "ghost", Direction: "down", DryRun: true})
	if resp := decodeErr(rec.Body.Bytes()); len(resp.Problems) == 0 || !strings.Contains(resp.Problems[0], "kubeconfig") {
		t.Errorf("problems = %v", resp.Problems)
	}
}

// ---- real runs: the gates ----------------------------------------------------------------------------

func TestDRRealRunGateMatrix(t *testing.T) {
	type row struct {
		name    string
		opts    Options
		plan    string
		scope   drScope
		confirm string
		dry     bool // dry-run first, so only the gates are tested
		status  int
		code    string
	}
	for _, tc := range []row{
		{"flag off", Options{}, "plain", appsOnly, "", true, 403, "dr_runs_disabled"},
		{"flag off, read-only", Options{ReadOnly: true}, "plain", appsOnly, "", true, 403, "dr_runs_disabled"},
		{"read-only with the flag", Options{AllowDRRuns: true, ReadOnly: true}, "plain", appsOnly, "", true, 403, "read_only"},
		{"power on a non-lab plan", Options{AllowDRRuns: true}, "nolab", drScope{}, "stop dev", true, 403, "dr_power_disabled"},
		{"explicit power phase on a non-lab plan", Options{AllowDRRuns: true}, "plain", drScope{Phases: []string{"power"}}, "", true, 403, "dr_power_disabled"},
		{"explicit power phase with the flag needs the stop phrase", Options{AllowDRRuns: true, AllowDRPower: true}, "plain", drScope{Phases: []string{"power"}}, "", true, 409, "confirmation_required"},
		{"explicit power phase with the flag and the phrase", Options{AllowDRRuns: true, AllowDRPower: true}, "plain", drScope{Phases: []string{"power"}}, "stop dev", true, 202, ""},
		{"power flag, no phrase", Options{AllowDRRuns: true, AllowDRPower: true}, "nolab", drScope{}, "", true, 409, "confirmation_required"},
		{"power flag, wrong phrase", Options{AllowDRRuns: true, AllowDRPower: true}, "nolab", drScope{}, "stop prod-eu", true, 409, "confirmation_required"},
		{"power flag, right phrase", Options{AllowDRRuns: true, AllowDRPower: true}, "nolab", drScope{}, "stop dev", true, 202, ""},
		{"lab plan needs the stop phrase", Options{AllowDRRuns: true}, "lab", drScope{}, "", true, 409, "confirmation_required"},
		{"lab plan with the stop phrase", Options{AllowDRRuns: true}, "lab", drScope{}, "stop dev", true, 202, ""},
		{"production, no phrase", Options{AllowDRRuns: true}, "prod", appsOnly, "", true, 409, "confirmation_required"},
		{"production, case variant", Options{AllowDRRuns: true}, "prod", appsOnly, "PROD-EU", true, 409, "confirmation_required"},
		{"production, padded", Options{AllowDRRuns: true}, "prod", appsOnly, " prod-eu ", true, 409, "confirmation_required"},
		{"production, right phrase", Options{AllowDRRuns: true}, "prod", appsOnly, "prod-eu", true, 202, ""},
		{"apps only needs nothing", Options{AllowDRRuns: true}, "plain", appsOnly, "", true, 202, ""},
		{"no dry run first", Options{AllowDRRuns: true}, "plain", appsOnly, "", false, 409, "dry_run_required"},
	} {
		d := newDREnv(t, tc.opts)
		writeRunPlans(t, d)
		if tc.dry {
			d.dryRun(t, tc.plan, "down", tc.scope)
		}
		before := len(d.fake.realCalls())

		got := d.realRun(tc.plan, "down", tc.scope, tc.confirm)
		if got.code != tc.status || got.err.Code != tc.code {
			t.Errorf("%s: status %d code %q (%s)", tc.name, got.code, got.err.Code, got.body)
		}
		started := len(d.fake.realCalls()) - before
		if (tc.status == 202) != (started == 1) {
			t.Errorf("%s: the engine was asked %d times", tc.name, started)
		}
		if tc.code == "confirmation_required" && got.err.Phrase == "" {
			t.Errorf("%s: the error must carry the phrase to type", tc.name)
		}
		d.server.dr.shutdown(2 * time.Second)
	}
}

func TestDRConfirmationPhraseIsTheEngineOneForBothDirections(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	d.dryRun(t, "prod", "up", drScope{})

	got := d.realRun("prod", "up", drScope{}, "")
	if got.code != 409 || got.err.Phrase != "prod-eu" {
		t.Errorf("up on production: %d %+v", got.code, got.err)
	}
}

func TestDRDryRunTicketIsSingleUseScopedAndExpires(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	d.server.now = func() time.Time { return now }

	d.dryRun(t, "plain", "down", appsOnly)

	// A different scope, direction or plan has no ticket.
	if got := d.realRun("plain", "down", drScope{Phases: []string{"apps"}}, ""); got.err.Code != "dry_run_required" {
		t.Errorf("another scope: %+v", got.err)
	}
	if got := d.realRun("plain", "up", appsOnly, ""); got.err.Code != "dry_run_required" {
		t.Errorf("another direction: %+v", got.err)
	}

	// The ticket survives a refusal, and the order of scope entries does not matter.
	if got := d.realRun("plain", "down", drScope{Phases: []string{"databases", "apps"}}, ""); got.code != 202 {
		t.Fatalf("matching ticket: %d %s", got.code, got.body)
	}
	// It is used up once the run has started.
	if got := d.realRun("plain", "down", appsOnly, ""); got.err.Code != "dry_run_required" {
		t.Errorf("a ticket must be single use: %+v", got.err)
	}

	// A plan that changed on disk invalidates its ticket.
	d.dryRun(t, "plain", "up", appsOnly)
	d.writePlan(t, "plain", planYAML("plain", "dev", false, false)+"# edited\n")
	if got := d.realRun("plain", "up", appsOnly, ""); got.err.Code != "dry_run_required" {
		t.Errorf("an edited plan: %+v", got.err)
	}

	// And an old ticket expires.
	d.dryRun(t, "plain", "up", appsOnly)
	now = now.Add(drTicketTTL + time.Minute)
	if got := d.realRun("plain", "up", appsOnly, ""); got.err.Code != "dry_run_required" {
		t.Errorf("an expired ticket: %+v", got.err)
	}
}

func TestDRTicketsAreBounded(t *testing.T) {
	d := newDREnv(t, Options{})
	for i := 0; i < drMaxTickets*3; i++ {
		d.server.dr.recordTicket(fmt.Sprintf("key-%d", i))
	}
	if len(d.server.dr.tickets) > drMaxTickets {
		t.Errorf("tickets = %d", len(d.server.dr.tickets))
	}
}

// ---- real runs: lifecycle ----------------------------------------------------------------------------

func startPlain(t *testing.T, d *drEnv) string {
	t.Helper()
	d.dryRun(t, "plain", "down", appsOnly)
	rec := d.do(http.MethodPost, "/api/dr/runs", drRunRequest{Plan: "plain", Direction: "down", Scope: appsOnly})
	started := decode[drStartedResponse](t, rec)
	if rec.Code != http.StatusAccepted || started.ID != fakeRunID || started.Kind != "run" || started.Plan != "plain" || started.Context != "dev" {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	return started.ID
}

func TestDRRunLifecycleStartPollAndFinish(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	id := startPlain(t, d)

	call := d.fake.realCalls()[0]
	if call.Opts.Actor != "web" || call.Opts.DryRun || call.Plan != "plain" || len(call.Scope.Phases) != 2 {
		t.Errorf("engine call = %+v", call)
	}

	live := decode[drRunView](t, d.do(http.MethodGet, "/api/dr/run?id="+id, nil))
	if !live.Live || live.Status != dr.StatusRunning || live.Kind != "run" || len(live.Events) == 0 || live.Events[0].Kind != "run-start" || len(live.Steps) != 1 {
		t.Errorf("live view = %+v", live)
	}

	// The run belongs to the server: it keeps going without the request that started it.
	list := decode[[]drRunSummary](t, d.do(http.MethodGet, "/api/dr/runs", nil))
	if len(list) != 1 || !list[0].Live || list[0].Resumable {
		t.Errorf("list = %+v", list)
	}

	close(d.fake.release)
	waitFor(t, "the run to finish", func() bool {
		return !decode[drRunView](t, d.do(http.MethodGet, "/api/dr/run?id="+id, nil)).Live
	})
	done := decode[drRunView](t, d.do(http.MethodGet, "/api/dr/run?id="+id, nil))
	if done.Status != dr.StatusSucceeded || done.Live {
		t.Errorf("finished view = %+v", done)
	}
	if list := decode[[]drRunSummary](t, d.do(http.MethodGet, "/api/dr/runs", nil)); list[0].Live || list[0].Resumable || list[0].Status != dr.StatusSucceeded {
		t.Errorf("finished list = %+v", list)
	}
}

func TestDRRunRefusalsFromTheEngineAreMapped(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"lock":          {dr.ErrRunInProgress, 409, "run_in_progress"},
		"pre-flight":    {fmt.Errorf("%w: the backup is 3 days old", dr.ErrPreflight), 409, "preflight_failed"},
		"confirmation":  {dr.ErrConfirmationRequired, 409, "confirmation_required"},
		"read-only":     {dr.ErrReadOnly, 403, "read_only"},
		"something odd": {errors.New("boom"), 502, "cluster_error"},
	} {
		d := newDREnv(t, Options{AllowDRRuns: true})
		writeRunPlans(t, d)
		d.fake.refuse = tc.err
		d.dryRun(t, "plain", "down", appsOnly)

		got := d.realRun("plain", "down", appsOnly, "")
		if got.code != tc.status || got.err.Code != tc.code {
			t.Errorf("%s: %d %s", name, got.code, got.body)
		}
		if len(d.server.dr.runs) != 0 {
			t.Errorf("%s: a refused run must not be registered", name)
		}
		// A refusal does not use up the dry run the person already reviewed.
		if !d.server.dr.hasTicket(ticketKey("plain", []byte(planYAML("plain", "dev", false, false)), dr.DirDown, dr.Scope{Phases: []dr.Phase{dr.PhaseApps, dr.PhaseDatabases}})) {
			t.Errorf("%s: the ticket was consumed by a refusal", name)
		}
	}
}

func TestDRAbortCancelsALiveRun(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	id := startPlain(t, d)

	rec := d.do(http.MethodPost, "/api/dr/runs/abort", drAbortRequest{ID: id})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("abort = %d: %s", rec.Code, rec.Body.String())
	}
	waitFor(t, "the run to stop", func() bool {
		return !decode[drRunView](t, d.do(http.MethodGet, "/api/dr/run?id="+id, nil)).Live
	})
	view := decode[drRunView](t, d.do(http.MethodGet, "/api/dr/run?id="+id, nil))
	if view.Status != dr.StatusAborted {
		t.Errorf("status = %s", view.Status)
	}
	if list := decode[[]drRunSummary](t, d.do(http.MethodGet, "/api/dr/runs", nil)); !list[0].Resumable {
		t.Errorf("an aborted run must be resumable: %+v", list[0])
	}

	// Only live runs of this process can be aborted.
	if rec := d.do(http.MethodPost, "/api/dr/runs/abort", drAbortRequest{ID: id}); rec.Code != http.StatusNotFound {
		t.Errorf("aborting a finished run = %d", rec.Code)
	}
	if rec := d.do(http.MethodPost, "/api/dr/runs/abort", drAbortRequest{ID: fakeRunIDTwo}); rec.Code != http.StatusNotFound {
		t.Errorf("aborting an unknown run = %d", rec.Code)
	}
	for _, bad := range []string{"../x", "", "20261031", "drill-zz"} {
		if rec := d.do(http.MethodPost, "/api/dr/runs/abort", drAbortRequest{ID: bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("abort %q = %d; want 400", bad, rec.Code)
		}
	}
}

func TestDRShutdownAbortsRunsAndLeavesThemResumable(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	id := startPlain(t, d)

	d.server.dr.shutdown(5 * time.Second)

	stored, found, _ := d.ledger.Status(context.Background(), id)
	if !found || stored.Status != dr.StatusAborted {
		t.Errorf("after shutdown: %+v", stored)
	}
	if lr := d.server.dr.liveRun(id); lr == nil || lr.isLive() {
		t.Error("the run goroutine must have ended")
	}
}

func TestDRRunViewOfAnUnknownOrInvalidID(t *testing.T) {
	d := newDREnv(t, Options{})
	if rec := d.do(http.MethodGet, "/api/dr/run?id="+fakeRunIDTwo, nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown run = %d", rec.Code)
	}
	if rec := d.do(http.MethodGet, "/api/dr/run?id=drill-00000000", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown drill = %d", rec.Code)
	}
	for _, bad := range []string{"", "../etc/passwd", "20261031T015900Z-1A2B3C4D", "latest"} {
		if rec := d.do(http.MethodGet, "/api/dr/run?id="+urlEscape(bad), nil); rec.Code != http.StatusBadRequest {
			t.Errorf("run id %q = %d; want 400", bad, rec.Code)
		}
	}
	if rec := d.do(http.MethodGet, "/api/dr/runs?limit=9999", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("limit 9999 = %d", rec.Code)
	}
}

func TestDRRunRetentionIsBoundedAndKeepsLiveRuns(t *testing.T) {
	d := newDREnv(t, Options{})
	service := d.server.dr

	live := &liveRun{kind: "run", id: "20260101T000000Z-00000000", done: make(chan struct{}), ready: make(chan struct{})}
	service.register(live)
	for i := 0; i < drMaxRetained*2; i++ {
		finished := &liveRun{kind: "run", id: fmt.Sprintf("20260102T%06dZ-%08x", i, i), done: make(chan struct{}), ready: make(chan struct{})}
		close(finished.done)
		service.register(finished)
	}
	if len(service.runs) > drMaxRetained || len(service.order) > drMaxRetained {
		t.Errorf("retained %d runs (%d in order); the limit is %d", len(service.runs), len(service.order), drMaxRetained)
	}
	if service.liveRun(live.id) == nil {
		t.Error("a run that is still going must never be forgotten")
	}
}

func TestDRLiveRunEventsAreBounded(t *testing.T) {
	run := &liveRun{kind: "run", ready: make(chan struct{})}
	for i := 0; i < drMaxEvents*2; i++ {
		run.onEvent(dr.Event{RunID: fakeRunID, Seq: i, Kind: "step-done"})
	}
	events, _, _, _, _ := run.snapshot()
	if len(events) != drMaxEvents || events[len(events)-1].Seq != drMaxEvents*2-1 || events[0].Seq != drMaxEvents {
		t.Errorf("kept %d events, first %d last %d", len(events), events[0].Seq, events[len(events)-1].Seq)
	}
}

// ---- resume ------------------------------------------------------------------------------------------

func TestDRResumeUsesTheScopeOfTheOriginalRun(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	id := startPlain(t, d)
	d.do(http.MethodPost, "/api/dr/runs/abort", drAbortRequest{ID: id})
	waitFor(t, "the abort", func() bool { return !d.server.dr.liveRun(id).isLive() })

	// The side file keeps what the engine's ledger does not: the scope.
	info, err := os.Stat(filepath.Join(d.dir, "web-runs", id+".json"))
	if err != nil || (permissionBitsMatter() && info.Mode().Perm() != 0o600) {
		t.Fatalf("side file: %v %v", info, err)
	}

	d.fake.release = make(chan struct{})
	rec := d.do(http.MethodPost, "/api/dr/runs/resume", drResumeRequest{ID: id})
	started := decode[drStartedResponse](t, rec)
	if rec.Code != http.StatusAccepted || started.ID != id {
		t.Fatalf("resume = %d: %s", rec.Code, rec.Body.String())
	}
	calls := d.fake.realCalls()
	last := calls[len(calls)-1]
	if last.Resume != id || last.Dir != dr.DirDown || len(last.Scope.Phases) != 2 || last.Scope.Phases[0] != dr.PhaseApps {
		t.Errorf("resume call = %+v; the scope must be the original one, never wider", last)
	}

	// While it is going, it cannot be resumed again.
	if rec := d.do(http.MethodPost, "/api/dr/runs/resume", drResumeRequest{ID: id}); rec.Code != http.StatusConflict {
		t.Errorf("a second resume = %d", rec.Code)
	}
	close(d.fake.release)
}

func TestDRResumeRefusals(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)

	// Started elsewhere (the CLI): no side file, so the scope is unknown here and must not be guessed.
	d.ledger.put(&dr.RunResult{ID: fakeRunIDTwo, Plan: "plain", Context: "dev", Direction: dr.DirDown, Status: dr.StatusFailed})
	rec := d.do(http.MethodPost, "/api/dr/runs/resume", drResumeRequest{ID: fakeRunIDTwo})
	if rec.Code != http.StatusConflict || decodeErr(rec.Body.Bytes()).Code != "scope_unknown" {
		t.Errorf("unknown scope: %d %s", rec.Code, rec.Body.String())
	}

	// The plan on disk no longer matches the run.
	if err := d.server.dr.writeMeta(fakeRunIDTwo, webRunMeta{Plan: "plain", Direction: "down", Scope: drScope{Phases: []string{"apps"}}}); err != nil {
		t.Fatal(err)
	}
	d.ledger.put(&dr.RunResult{ID: fakeRunIDTwo, Plan: "another-name", Context: "dev", Direction: dr.DirDown, Status: dr.StatusFailed})
	if rec := d.do(http.MethodPost, "/api/dr/runs/resume", drResumeRequest{ID: fakeRunIDTwo}); rec.Code != http.StatusConflict || decodeErr(rec.Body.Bytes()).Code != "plan_changed" {
		t.Errorf("changed plan: %d %s", rec.Code, rec.Body.String())
	}

	if rec := d.do(http.MethodPost, "/api/dr/runs/resume", drResumeRequest{ID: "20261031T000000Z-deadbeef"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown run = %d", rec.Code)
	}
	if rec := d.do(http.MethodPost, "/api/dr/runs/resume", drResumeRequest{ID: "../x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id = %d", rec.Code)
	}
}

func TestDRResumeObeysTheSameGates(t *testing.T) {
	for name, tc := range map[string]struct {
		opts Options
		code string
	}{
		"flag off":  {Options{}, "dr_runs_disabled"},
		"read-only": {Options{AllowDRRuns: true, ReadOnly: true}, "read_only"},
	} {
		d := newDREnv(t, tc.opts)
		writeRunPlans(t, d)
		d.ledger.put(&dr.RunResult{ID: fakeRunIDTwo, Plan: "plain", Context: "dev", Direction: dr.DirDown, Status: dr.StatusFailed})
		_ = d.server.dr.writeMeta(fakeRunIDTwo, webRunMeta{Plan: "plain", Direction: "down", Scope: drScope{Phases: []string{"apps"}}})
		rec := d.do(http.MethodPost, "/api/dr/runs/resume", drResumeRequest{ID: fakeRunIDTwo})
		if rec.Code != http.StatusForbidden || decodeErr(rec.Body.Bytes()).Code != tc.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if len(d.fake.realCalls()) != 0 {
			t.Errorf("%s: the engine was asked to run", name)
		}
	}

	// Production needs the phrase on resume too.
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	d.ledger.put(&dr.RunResult{ID: fakeRunIDTwo, Plan: "prod", Context: "prod-eu", Direction: dr.DirDown, Status: dr.StatusFailed})
	_ = d.server.dr.writeMeta(fakeRunIDTwo, webRunMeta{Plan: "prod", Direction: "down", Scope: drScope{Phases: []string{"apps"}}})
	rec := d.do(http.MethodPost, "/api/dr/runs/resume", drResumeRequest{ID: fakeRunIDTwo})
	if rec.Code != http.StatusConflict || decodeErr(rec.Body.Bytes()).Phrase != "prod-eu" {
		t.Errorf("production resume: %d %s", rec.Code, rec.Body.String())
	}
}

// ---- drills ------------------------------------------------------------------------------------------

func TestDRDrillGates(t *testing.T) {
	for name, tc := range map[string]struct {
		opts    Options
		plan    string
		full    bool
		confirm string
		status  int
		code    string
	}{
		"flag off":          {Options{}, "lab", false, "", 403, "dr_runs_disabled"},
		"read-only":         {Options{AllowDRRuns: true, ReadOnly: true}, "lab", false, "", 403, "read_only"},
		"not a lab":         {Options{AllowDRRuns: true}, "plain", false, "", 403, "drill_lab_only"},
		"not a lab, power":  {Options{AllowDRRuns: true, AllowDRPower: true}, "nolab", true, "stop dev", 403, "drill_lab_only"},
		"production":        {Options{AllowDRRuns: true, AllowDRPower: true}, "prod", false, "prod-eu", 403, "drill_lab_only"},
		"full needs a stop": {Options{AllowDRRuns: true}, "lab", true, "", 409, "confirmation_required"},
		"apps only":         {Options{AllowDRRuns: true}, "lab", false, "", 202, ""},
		"full with stop":    {Options{AllowDRRuns: true}, "lab", true, "stop dev", 202, ""},
	} {
		d := newDREnv(t, tc.opts)
		writeRunPlans(t, d)
		d.fake.drillHold = make(chan struct{})

		rec := d.do(http.MethodPost, "/api/dr/drill", drDrillRequest{Plan: tc.plan, Full: tc.full, Confirm: tc.confirm})
		if rec.Code != tc.status || decodeErr(rec.Body.Bytes()).Code != tc.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if tc.status != 202 && len(d.fake.drills) != 0 {
			t.Errorf("%s: the drill started", name)
		}
		close(d.fake.drillHold)
		d.server.dr.shutdown(2 * time.Second)
	}
}

func TestDRDrillRunsInTheBackgroundAndLinksTheReport(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	d.fake.drillHold = make(chan struct{})
	d.fake.drillRep = &measure.Report{ID: "20261031T120000Z-aaaaaaaa", Plan: "lab", Outcome: measure.Outcome("recovered")}

	rec := d.do(http.MethodPost, "/api/dr/drill", drDrillRequest{Plan: "lab", Full: false})
	started := decode[drStartedResponse](t, rec)
	if rec.Code != http.StatusAccepted || started.Kind != "drill" || !drillHandle.MatchString(started.ID) {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	opts := d.fake.drills[0]
	if opts.Actor != "web" || opts.Full || opts.AllowProduction || opts.Dir != d.dir || opts.Confirm != "" {
		t.Errorf("drill options = %+v (the web never allows production)", opts)
	}

	live := decode[drDrillView](t, d.do(http.MethodGet, "/api/dr/run?id="+started.ID, nil))
	if !live.Live || live.Kind != "drill" || live.Status != "running" || live.DownRun != fakeRunID || len(live.Events) == 0 {
		t.Errorf("live drill = %+v", live)
	}

	close(d.fake.drillHold)
	waitFor(t, "the drill to finish", func() bool {
		return !decode[drDrillView](t, d.do(http.MethodGet, "/api/dr/run?id="+started.ID, nil)).Live
	})
	done := decode[drDrillView](t, d.do(http.MethodGet, "/api/dr/run?id="+started.ID, nil))
	if done.Status != "succeeded" || done.ReportID != "20261031T120000Z-aaaaaaaa" || done.Error != "" {
		t.Errorf("finished drill = %+v", done)
	}
}

func TestDRDrillRefusalIsReportedImmediately(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	d.fake.drillErr = dr.ErrRunInProgress

	rec := d.do(http.MethodPost, "/api/dr/drill", drDrillRequest{Plan: "lab"})
	if rec.Code != http.StatusConflict || decodeErr(rec.Body.Bytes()).Code != "run_in_progress" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDRDrillCanBeAbortedAndFailureIsShown(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	d.fake.drillHold = make(chan struct{})

	started := decode[drStartedResponse](t, d.do(http.MethodPost, "/api/dr/drill", drDrillRequest{Plan: "lab"}))
	if rec := d.do(http.MethodPost, "/api/dr/runs/abort", drAbortRequest{ID: started.ID}); rec.Code != http.StatusAccepted {
		t.Fatalf("abort = %d", rec.Code)
	}
	waitFor(t, "the drill to stop", func() bool {
		return !decode[drDrillView](t, d.do(http.MethodGet, "/api/dr/run?id="+started.ID, nil)).Live
	})
	view := decode[drDrillView](t, d.do(http.MethodGet, "/api/dr/run?id="+started.ID, nil))
	if view.Status != "failed" || view.Error == "" {
		t.Errorf("an aborted drill must show why it stopped: %+v", view)
	}
}

// ---- concurrency -------------------------------------------------------------------------------------

func TestDRConcurrentRequestsAreRaceFree(t *testing.T) {
	d := newDREnv(t, Options{AllowDRRuns: true})
	writeRunPlans(t, d)
	id := startPlain(t, d)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				d.do(http.MethodGet, "/api/dr/run?id="+id, nil)
				d.do(http.MethodGet, "/api/dr/runs", nil)
				d.do(http.MethodGet, "/api/dr/plans", nil)
				d.do(http.MethodPost, "/api/dr/runs", drRunRequest{Plan: "plain", Direction: "up", Scope: appsOnly, DryRun: true})
			}
		}()
	}
	wg.Wait()
	close(d.fake.release)
	waitFor(t, "the run to finish", func() bool { return !d.server.dr.liveRun(id).isLive() })
}
