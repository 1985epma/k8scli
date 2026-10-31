package measure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

type fixture struct {
	clock  *simClock
	client *fake.Clientset
	runner *fakeRunner
	env    *dr.Env
	plan   *dr.Plan
	dir    string
}

// newFixture is the shop: db (cnpg), api (2 replicas), web (3 replicas). The scripted recovery brings the
// database up 60s after the up run starts, the api 120s and the web 300s. The down run takes 30s, so
// the RTOs are 90s (db), 150s (api) and 330s (web), counted from T0.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	clock := newClock()
	client := shopClient()
	runner := &fakeRunner{clock: clock, client: client}
	runner.upScript = []scriptStep{
		{At: 60 * time.Second, Phase: dr.PhaseDatabases},
		runner.restoreStep(120*time.Second, dr.PhaseApps, "api", 2),
		runner.restoreStep(300*time.Second, dr.PhaseApps, "web", 3),
	}
	return &fixture{clock: clock, client: client, runner: runner, env: testEnv(clock, client), plan: shopPlan(), dir: t.TempDir()}
}

func (f *fixture) opts() DrillOptions {
	return DrillOptions{
		Dir: f.dir, Actor: "tester",
		Backups: func(context.Context, *dr.Env, string) (time.Time, bool, error) {
			return epoch.Add(-time.Hour), true, nil
		},
	}
}

func (f *fixture) drill(opts DrillOptions) (*Report, error) {
	return Drill(context.Background(), f.env, f.plan, nil, f.runner, opts)
}

func appResult(t *testing.T, report *Report, name string) AppResult {
	t.Helper()
	for _, app := range report.Apps {
		if app.Name == name {
			return app
		}
	}
	t.Fatalf("no result for %s", name)
	return AppResult{}
}

func TestDrillMeasuresRTOAndSavesTheReport(t *testing.T) {
	f := newFixture(t)
	report, err := f.drill(f.opts())
	if err != nil {
		t.Fatal(err)
	}

	if dirs := f.runner.directions(); len(dirs) != 2 || dirs[0] != dr.DirDown || dirs[1] != dr.DirUp {
		t.Fatalf("directions = %v; want down then up", dirs)
	}
	for _, scope := range f.runner.scopes {
		if len(scope.Phases) != 2 || scope.Phases[0] != dr.PhaseApps || scope.Phases[1] != dr.PhaseDatabases {
			t.Errorf("the default scope must be apps and databases only, got %v", scope.Phases)
		}
	}

	if report.Outcome != OutcomeRecovered || !report.StateRestored || report.Verdict != VerdictPass {
		t.Fatalf("outcome %s, restored %v, verdict %s\n%s", report.Outcome, report.StateRestored, report.Verdict, report.Markdown())
	}
	// Times are measured from T0 (the start of the down run): 30s of down + the scripted recovery.
	if got := appResult(t, report, "api").RTO.Seconds; got != 150 {
		t.Errorf("api RTO = %vs; want 150", got)
	}
	if got := appResult(t, report, "web").RTO.Seconds; got != 330 {
		t.Errorf("web RTO = %vs; want 330", got)
	}
	if report.RTO.Seconds != 330 || report.RTO.Kind != KindMeasured {
		t.Errorf("overall RTO = %+v", report.RTO)
	}
	if got := stageSeconds(t, report.Stages, StageDatabasesUp); got != 90 {
		t.Errorf("databases-up = %vs; want 90", got)
	}
	if got := stageSeconds(t, report.Stages, StageHealthGreen); got != 330 {
		t.Errorf("health-green = %vs; want 330", got)
	}
	if report.DownRun != "run-down" || report.UpRun != "run-up" || report.Actor != "tester" || report.Plan != "shop" {
		t.Errorf("identity fields: %+v", report)
	}
	if report.RPO == nil || report.RPO.Kind != KindEstimated || report.RPO.Seconds != 3600 {
		t.Errorf("RPO should be the estimated age of the backup: %+v", report.RPO)
	}

	saved, err := Load(f.dir, report.ID)
	if err != nil || saved.RTO.Seconds != 330 {
		t.Errorf("the report was not saved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "reports", report.ID+".md")); err != nil {
		t.Errorf("the Markdown report was not saved: %v", err)
	}
	if !strings.HasPrefix(report.ID, "drill-20260101T120000Z-") {
		t.Errorf("id = %s", report.ID)
	}
}

func TestDrillRefusesPlansThatAreNotLabs(t *testing.T) {
	f := newFixture(t)
	f.plan.Safety.LabOnly = false

	report, err := f.drill(f.opts())
	if !errors.Is(err, ErrDrillRefused) || report != nil {
		t.Fatalf("err = %v, report %v", err, report)
	}
	if len(f.runner.directions()) != 0 {
		t.Error("the runner must not be called when the drill is refused")
	}

	opts := f.opts()
	opts.AllowProduction = true // but no confirmation phrase
	if _, err := f.drill(opts); !errors.Is(err, ErrDrillRefused) {
		t.Errorf("allowing production without a phrase: %v", err)
	}
	if len(f.runner.directions()) != 0 {
		t.Error("still no run expected")
	}

	opts.Confirm = "kind-k8scli-lab"
	if _, err := f.drill(opts); err != nil {
		t.Fatalf("an explicit, confirmed drill should run: %v", err)
	}
	if got := f.runner.confirms; len(got) != 2 || got[0] != "kind-k8scli-lab" || got[1] != "kind-k8scli-lab" {
		t.Errorf("the confirmation phrase must reach the runner for both directions, got %v", got)
	}
}

func TestDrillScopeOptions(t *testing.T) {
	f := newFixture(t)
	opts := f.opts()
	opts.Full = true
	if _, err := f.drill(opts); err != nil {
		t.Fatal(err)
	}
	if got := f.runner.scopes[0].Phases; len(got) != 0 {
		t.Errorf("a full drill must not restrict the phases, got %v", got)
	}

	f = newFixture(t)
	opts = f.opts()
	opts.Scope = dr.Scope{Apps: []string{"api"}, Phases: []dr.Phase{dr.PhaseApps}}
	if _, err := f.drill(opts); err != nil {
		t.Fatal(err)
	}
	if got := f.runner.scopes[0]; len(got.Apps) != 1 || got.Apps[0] != "api" || len(got.Phases) != 1 {
		t.Errorf("an explicit scope must be honoured, got %+v", got)
	}
}

func TestDrillRPOFromBackupsAndDatabaseEstimates(t *testing.T) {
	f := newFixture(t)
	opts := f.opts()
	opts.Backups = func(context.Context, *dr.Env, string) (time.Time, bool, error) {
		return epoch.Add(-5 * time.Hour), true, nil // older than the 4h target
	}
	opts.DBEstimates = func(context.Context, *dr.Env, *dr.Plan) ([]DBEstimate, error) {
		return []DBEstimate{{Name: "db", Age: 90 * time.Minute, Source: "cnpg-backup", Note: "lastSuccessfulBackup"}}, nil
	}
	report, err := f.drill(opts)
	if err != nil {
		t.Fatal(err)
	}

	byKey := map[string]RPOFigure{}
	for _, figure := range report.Figures {
		if figure.Kind != KindEstimated {
			t.Errorf("figures without a restore must be estimated: %+v", figure)
		}
		byKey[figure.Subject+"|"+figure.Source] = figure
	}
	if got := byKey["api|backup:shop"]; got.Seconds != 5*3600 {
		t.Errorf("api RPO figure: %+v", got)
	}
	if got := byKey["db|cnpg-backup"]; got.Seconds != 5400 || got.Note != "lastSuccessfulBackup" {
		t.Errorf("db figure should come from the database estimator: %+v", got)
	}
	if _, duplicated := byKey["db|backup:shop"]; duplicated {
		t.Error("a database with its own estimate must not also get the namespace backup figure")
	}
	if api := appResult(t, report, "api"); api.RPO == nil || api.RPO.Verdict != VerdictFail {
		t.Errorf("a 5h-old backup misses a 4h RPO: %+v", api.RPO)
	}
	if report.Verdict != VerdictFail {
		t.Errorf("verdict = %s; want fail", report.Verdict)
	}
	if report.RPO == nil || report.RPO.Seconds != 5*3600 {
		t.Errorf("overall RPO is the worst figure: %+v", report.RPO)
	}
}

func TestDrillSaysSoWhenThereIsNoBackupInformation(t *testing.T) {
	f := newFixture(t)
	opts := f.opts()
	opts.Backups = nil
	report, err := f.drill(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Figures) != 0 || !containsNotice(report, "no backup source was configured") {
		t.Errorf("figures %v, notices %v", report.Figures, report.Notices)
	}
	if report.Verdict != VerdictUnknown {
		t.Errorf("an RPO target that could not be checked cannot be a pass, got %s", report.Verdict)
	}

	f = newFixture(t)
	opts = f.opts()
	opts.Backups = func(context.Context, *dr.Env, string) (time.Time, bool, error) { return time.Time{}, false, nil }
	opts.DBEstimates = func(context.Context, *dr.Env, *dr.Plan) ([]DBEstimate, error) {
		return nil, errors.New("operator missing")
	}
	report, err = f.drill(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !containsNotice(report, "no backup was found for namespace shop") || !containsNotice(report, "operator missing") {
		t.Errorf("notices: %v", report.Notices)
	}

	f = newFixture(t)
	opts = f.opts()
	opts.Backups = func(context.Context, *dr.Env, string) (time.Time, bool, error) {
		return time.Time{}, false, errors.New("velero unreachable")
	}
	report, _ = f.drill(opts)
	if !containsNotice(report, "velero unreachable") {
		t.Errorf("notices: %v", report.Notices)
	}
}

func containsNotice(report *Report, fragment string) bool {
	for _, notice := range report.Notices {
		if strings.Contains(notice, fragment) {
			return true
		}
	}
	return false
}

func TestDrillStillRecoversWhenTheDownRunFails(t *testing.T) {
	f := newFixture(t)
	f.runner.downStatus = dr.StatusFailed
	f.runner.downErr = errors.New("drain blocked by a PodDisruptionBudget")

	report, err := f.drill(f.opts())
	if !errors.Is(err, ErrDownFailed) {
		t.Fatalf("err = %v; want ErrDownFailed", err)
	}
	if dirs := f.runner.directions(); len(dirs) != 2 || dirs[1] != dr.DirUp {
		t.Fatalf("the up run must still happen after a failed down: %v", dirs)
	}
	if report == nil || len(report.Failures) == 0 || !strings.Contains(report.Failures[0], "drain blocked") {
		t.Fatalf("the failure must be reported prominently: %+v", report)
	}
	if report.Verdict != VerdictFail {
		t.Errorf("a drill whose down run failed cannot pass: %s", report.Verdict)
	}
	if !strings.Contains(report.Markdown(), "## Failures") {
		t.Error("the Markdown report must start with the failures")
	}
}

func TestDrillReturnsNoReportWhenTheRunnerRefusesToStart(t *testing.T) {
	for _, refusal := range []error{dr.ErrConfirmationRequired, dr.ErrReadOnly, dr.ErrRunInProgress} {
		f := newFixture(t)
		f.runner.returnNilOnDown = true
		f.runner.downErr = refusal
		report, err := f.drill(f.opts())
		if report != nil || !errors.Is(err, refusal) {
			t.Errorf("%v: report %v, err %v", refusal, report, err)
		}
		if dirs := f.runner.directions(); len(dirs) != 1 {
			t.Errorf("nothing ran, so there is nothing to recover: %v", dirs)
		}
		entries, _ := os.ReadDir(filepath.Join(f.dir, "reports"))
		if len(entries) != 0 {
			t.Error("no report should be stored for a drill that never started")
		}
	}
}

func TestDrillReportsAFailedRecoveryLoudly(t *testing.T) {
	f := newFixture(t)
	f.runner.upScript = nil // nothing comes back
	f.runner.upStatus = dr.StatusFailed
	f.runner.upErr = errors.New("database did not hibernate-resume")
	f.runner.afterUp = func() { f.clock.autoAdvance.Store(true) } // let the watchers' grace period expire
	opts := f.opts()
	opts.FailedUpGrace = 10 * time.Second

	report, err := f.drill(opts)
	if !errors.Is(err, ErrRecoveryFailed) {
		t.Fatalf("err = %v; want ErrRecoveryFailed", err)
	}
	if report == nil || report.Outcome != OutcomeNotRecovered || report.Verdict != VerdictFail {
		t.Fatalf("report: %+v", report)
	}
	for _, name := range []string{"api", "web"} {
		if app := appResult(t, report, name); app.Recovered || app.Verdict != VerdictFail {
			t.Errorf("%s should be a failure: %+v", name, app)
		}
	}
	if !strings.Contains(strings.Join(report.Failures, " "), "may still be down") {
		t.Errorf("failures: %v", report.Failures)
	}
	if !strings.Contains(err.Error(), "may still be down") {
		t.Errorf("the error must say the system may still be down: %v", err)
	}
	if _, loadErr := Load(f.dir, report.ID); loadErr != nil {
		t.Errorf("a failed drill must still be saved: %v", loadErr)
	}
}

func TestDrillRecoversEvenWhenTheContextWasCancelledDuringDown(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.runner.onDown = cancel
	f.runner.downStatus = dr.StatusAborted
	f.runner.downErr = context.Canceled

	report, err := Drill(ctx, f.env, f.plan, nil, f.runner, f.opts())
	if !errors.Is(err, ErrDownFailed) {
		t.Fatalf("err = %v", err)
	}
	if dirs := f.runner.directions(); len(dirs) != 2 || dirs[1] != dr.DirUp {
		t.Fatalf("the recovery must run after a cancelled down: %v", dirs)
	}
	if f.runner.upCtxErr != nil {
		t.Errorf("the recovery must not inherit the cancellation: %v", f.runner.upCtxErr)
	}
	if report == nil || report.Outcome != OutcomeRecovered {
		t.Errorf("the system came back, the report should say so: %+v", report)
	}
}

func TestDrillFlagsAStateThatDiffersFromTheBaseline(t *testing.T) {
	f := newFixture(t)
	f.runner.upScript = []scriptStep{
		{At: 60 * time.Second, Phase: dr.PhaseDatabases},
		f.runner.restoreStep(120*time.Second, dr.PhaseApps, "api", 2),
		f.runner.restoreStep(180*time.Second, dr.PhaseApps, "web", 2), // it had 3 replicas before
	}
	report, err := f.drill(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != OutcomeDifferences || report.StateRestored {
		t.Errorf("outcome %s, restored %v", report.Outcome, report.StateRestored)
	}
	if !appResult(t, report, "web").Recovered {
		t.Error("web is healthy, only different")
	}
	if !strings.Contains(report.Markdown(), "differs") {
		t.Error("the Markdown must say the state differs")
	}
}

func TestDrillWithoutAClientCannotVerifyPodReadiness(t *testing.T) {
	f := newFixture(t)
	f.env.Client = nil
	opts := f.opts()
	opts.FailedUpGrace = time.Second
	f.runner.afterUp = func() { f.clock.autoAdvance.Store(true) }
	f.runner.upStatus = dr.StatusFailed
	f.runner.upErr = errors.New("cannot reach the cluster")

	report, err := f.drill(opts)
	if !errors.Is(err, ErrRecoveryFailed) || report == nil {
		t.Fatalf("report %v, err %v", report, err)
	}
	if !containsNotice(report, "no Kubernetes client") {
		t.Errorf("notices: %v", report.Notices)
	}
}

func TestDrillMeasuresHTTPHealthPerApplication(t *testing.T) {
	f := newFixture(t)
	f.plan.Apps[1].Health = &dr.Health{Kind: "http", URL: "http://shop.internal/healthz", Status: 204}
	upStart := epoch.Add(30 * time.Second) // the down run takes 30s, so the up run starts then
	f.runner.upScript = []scriptStep{
		f.runner.restoreStep(100*time.Second, dr.PhaseApps, "api", 2),
		{At: 200 * time.Second, Phase: dr.PhaseApps}, // web's endpoint starts answering here
	}
	f.env.HTTP = httpFunc(func(url string) (int, error) {
		if url != "http://shop.internal/healthz" {
			t.Errorf("unexpected URL %s", url)
		}
		switch {
		case f.clock.Now().Before(upStart.Add(100 * time.Second)):
			return 0, errors.New("connection refused")
		case f.clock.Now().Before(upStart.Add(200 * time.Second)):
			return 503, nil
		}
		return 204, nil // the status the plan expects, not 200
	})

	report, err := f.drill(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	web := appResult(t, report, "web")
	if !web.Recovered || web.HealthKind != "http" || web.RTO.Seconds != 30+200 {
		t.Errorf("web should be healthy 200s after the up run started (T0 + 30s of down): %+v", web)
	}
	if api := appResult(t, report, "api"); api.RTO.Seconds != 30+100 {
		t.Errorf("api: %+v", api.RTO)
	}
}

type httpFunc func(url string) (int, error)

func (f httpFunc) Check(_ context.Context, url string) (int, error) { return f(url) }

func TestDrillReportsAnHTTPApplicationThatNeverAnswers(t *testing.T) {
	f := newFixture(t)
	f.plan.Apps[0].Health = &dr.Health{Kind: "http", URL: "http://api.internal/", Timeout: "30s"}
	f.env.HTTP = httpFunc(func(string) (int, error) { return 0, errors.New("connection refused") })
	f.runner.afterUp = func() { f.clock.autoAdvance.Store(true) }

	report, err := f.drill(f.opts())
	if err != nil {
		t.Fatalf("the runs succeeded, the failure belongs in the report: %v", err)
	}
	api := appResult(t, report, "api")
	if api.Recovered || api.Verdict != VerdictFail || report.Outcome != OutcomeNotRecovered {
		t.Errorf("api: %+v, outcome %s", api, report.Outcome)
	}
	if !containsNotice(report, "the last health check of api failed: connection refused") {
		t.Errorf("notices: %v", report.Notices)
	}
}

func TestDrillMeasuresDataLossWithTheHeartbeat(t *testing.T) {
	f := newFixture(t)
	f.client.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		action.(k8stesting.CreateAction).GetObject().(*appsv1.Deployment).Status.ReadyReplicas = 1
		return false, nil, nil
	})
	finishReaderJobs(f.client, true)
	if _, err := f.client.CoreV1().PersistentVolumeClaims("shop").Create(context.Background(), pvc("shop", "data"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	opts := f.opts()
	opts.Heartbeat = &HeartbeatOptions{
		Namespace: "shop", PVC: "data",
		// The last timestamp on the volume is 90s older than the disaster.
		LogReader: func(context.Context, string, string) (string, error) {
			return strconv.FormatInt(epoch.Add(-90*time.Second).Unix(), 10) + "\n", nil
		},
	}
	report, err := f.drill(opts)
	if err != nil {
		t.Fatal(err)
	}

	var measured []RPOFigure
	for _, figure := range report.Figures {
		if figure.Kind == KindMeasured {
			measured = append(measured, figure)
		}
	}
	if len(measured) != 3 { // api, web and db live in the namespace of the volume
		t.Fatalf("measured figures = %+v", measured)
	}
	for _, figure := range measured {
		if figure.Seconds != 90 || figure.Source != "heartbeat:data" || !strings.Contains(figure.Note, "clock") {
			t.Errorf("figure: %+v", figure)
		}
	}
	if api := appResult(t, report, "api"); api.RPO == nil || api.RPO.Seconds != 3600 {
		t.Errorf("the app RPO is the worst of its figures (the 1h backup estimate), got %+v", api.RPO)
	}

	if _, err := f.client.AppsV1().Deployments("shop").Get(context.Background(), HeartbeatName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the heartbeat writer must not be left behind: %v", err)
	}
	jobs, _ := f.client.BatchV1().Jobs("shop").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Errorf("reader jobs left behind: %d", len(jobs.Items))
	}
	if report.Baseline[0].Workloads[0].Name == HeartbeatName || len(report.PostState[0].Workloads) != 1 {
		t.Errorf("the heartbeat must not count as an application workload: %+v", report.PostState)
	}
}

func TestDrillKeepsGoingWhenTheHeartbeatCannotBeUsed(t *testing.T) {
	f := newFixture(t) // there is no PVC "data", so the heartbeat cannot start
	opts := f.opts()
	opts.Heartbeat = &HeartbeatOptions{Namespace: "shop", PVC: "data"}
	report, err := f.drill(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !containsNotice(report, "the heartbeat could not be started") {
		t.Errorf("notices: %v", report.Notices)
	}
	for _, figure := range report.Figures {
		if figure.Kind == KindMeasured {
			t.Errorf("no measured figure is possible: %+v", figure)
		}
	}
}

func TestDrillReadFailureOfTheHeartbeatIsANoticeNotAnError(t *testing.T) {
	f := newFixture(t)
	f.client.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		action.(k8stesting.CreateAction).GetObject().(*appsv1.Deployment).Status.ReadyReplicas = 1
		return false, nil, nil
	})
	f.client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).Status.Failed = 1
		return false, nil, nil
	})
	if _, err := f.client.CoreV1().PersistentVolumeClaims("shop").Create(context.Background(), pvc("shop", "data"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	opts := f.opts()
	opts.Heartbeat = &HeartbeatOptions{Namespace: "shop", PVC: "data"}
	report, err := f.drill(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !containsNotice(report, "could not be read after recovery") {
		t.Errorf("notices: %v", report.Notices)
	}
}

func TestDrillReportsWhenItCannotSaveTheReport(t *testing.T) {
	f := newFixture(t)
	blocker := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := f.opts()
	opts.Dir = blocker // reports/ cannot be created inside a regular file

	report, err := f.drill(opts)
	if err == nil || errors.Is(err, ErrRecoveryFailed) || errors.Is(err, ErrDownFailed) {
		t.Fatalf("err = %v; want a save error", err)
	}
	if report == nil || !containsNotice(report, "could not be saved") {
		t.Errorf("the report is still returned: %+v", report)
	}
}

func TestDrillRejectsMissingParts(t *testing.T) {
	f := newFixture(t)
	if _, err := Drill(context.Background(), nil, f.plan, nil, f.runner, f.opts()); err == nil {
		t.Error("a nil env must be an error")
	}
	if _, err := Drill(context.Background(), f.env, nil, nil, f.runner, f.opts()); err == nil {
		t.Error("a nil plan must be an error")
	}
	if _, err := Drill(context.Background(), f.env, f.plan, nil, nil, f.opts()); err == nil {
		t.Error("a nil runner must be an error")
	}
}

func TestDrillUsesTheRealClockAndSleepWhenTheEnvHasNone(t *testing.T) {
	f := newFixture(t)
	f.env = &dr.Env{Context: "x", Client: f.client} // no Now, Sleep or Logf
	f.runner.clock = newClock()
	report, err := f.drill(f.opts())
	// The fake runner uses its own clock, so the numbers are meaningless here; this checks nothing panics
	// and the defaults are applied.
	if err != nil && report == nil {
		t.Fatal(err)
	}
}

func TestListWorkloadsSelection(t *testing.T) {
	two, one := int32(2), int32(1)
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}, Spec: appsv1.DeploymentSpec{Replicas: &two}, Status: appsv1.DeploymentStatus{ReadyReplicas: 2}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "unset", Namespace: "shop"}}, // replicas default to 1
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: HeartbeatName, Namespace: "shop"}, Spec: appsv1.DeploymentSpec{Replicas: &one}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "shop"}, Spec: appsv1.StatefulSetSpec{Replicas: &one}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "shop"}, Spec: appsv1.StatefulSetSpec{Replicas: &one}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "other-ns", Namespace: "elsewhere"}},
	)
	plan := &dr.Plan{Databases: []dr.Database{{Name: "db", Namespace: "shop", Kind: "statefulset", Ref: "pg"}}}

	got, err := listWorkloads(context.Background(), client, plan, dr.App{Name: "shop", Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, workload := range got {
		names = append(names, workload.Kind+"/"+workload.Name)
	}
	want := "Deployment/api Deployment/unset StatefulSet/cache"
	if strings.Join(names, " ") != want {
		t.Errorf("workloads = %v; want %s (no database, no heartbeat, no other namespace)", names, want)
	}
	if got[1].Desired != 1 {
		t.Errorf("an unset replica count defaults to 1: %+v", got[1])
	}

	named, _ := listWorkloads(context.Background(), client, plan, dr.App{Name: "shop", Namespace: "shop", Workloads: []string{"unset", HeartbeatName}})
	if len(named) != 1 || named[0].Name != "unset" {
		t.Errorf("named workloads: %+v", named)
	}
}

func TestWorkloadsHealthyAndSameState(t *testing.T) {
	up := WorkloadState{Kind: "Deployment", Name: "a", Desired: 2, Ready: 2}
	partial := WorkloadState{Kind: "Deployment", Name: "b", Desired: 2, Ready: 1}
	off := WorkloadState{Kind: "Deployment", Name: "c", Desired: 0}
	for name, tc := range map[string]struct {
		workloads []WorkloadState
		expected  map[string]bool
		want      bool
	}{
		"all ready":                              {[]WorkloadState{up}, nil, true},
		"one partial":                            {[]WorkloadState{up, partial}, nil, false},
		"scaled to zero is not ready":            {[]WorkloadState{off}, nil, false},
		"nothing at all":                         {nil, nil, false},
		"expected subset only":                   {[]WorkloadState{up, partial}, map[string]bool{"Deployment/a": true}, true},
		"expected is empty: nothing to wait for": {[]WorkloadState{partial}, map[string]bool{}, true},
		"expected workload missing":              {[]WorkloadState{up}, map[string]bool{"Deployment/zzz": true}, true},
	} {
		if got := workloadsHealthy(tc.workloads, tc.expected); got != tc.want {
			t.Errorf("%s: %v; want %v", name, got, tc.want)
		}
	}

	before := []AppState{{App: "a", Namespace: "n", Healthy: true, Workloads: []WorkloadState{up}}}
	same := []AppState{{App: "a", Namespace: "n", Healthy: true, Workloads: []WorkloadState{up}}}
	fewer := []AppState{{App: "a", Namespace: "n", Healthy: true, Workloads: []WorkloadState{{Kind: "Deployment", Name: "a", Desired: 1, Ready: 1}}}}
	sick := []AppState{{App: "a", Namespace: "n", Healthy: false, Workloads: []WorkloadState{up}}}
	if !sameState(before, same) || sameState(before, fewer) || sameState(before, sick) || sameState(before, nil) {
		t.Error("sameState compares replica counts and health")
	}
}

func TestSleepContextStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Errorf("err = %v", err)
	}
}
