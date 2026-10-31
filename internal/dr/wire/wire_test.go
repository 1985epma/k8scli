package wire_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/backups"
	"github.com/1985epma/k8scli/internal/dr/checks"
	"github.com/1985epma/k8scli/internal/dr/measure"
	"github.com/1985epma/k8scli/internal/dr/wire"
	"github.com/1985epma/k8scli/internal/dr/wire/wiretest"
)

func plan() *dr.Plan {
	return &dr.Plan{
		Version: dr.PlanVersion, Name: "shop", Context: "dev",
		Apps: []dr.App{{Name: "shop", Namespace: "shop", Workloads: []string{"api"}, Tier: 1}},
	}
}

func cluster() *wiretest.Cluster {
	return wiretest.NewCluster(wiretest.Deployment("shop", "api", 2))
}

func TestEnvHasEverythingAndTouchesNoCluster(t *testing.T) {
	c := cluster()
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": c}, nil)

	env, err := stack.Env(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if env.Context != "dev" || env.Client == nil || env.Dynamic == nil || env.Exec == nil || env.HTTP == nil ||
		env.State == nil || env.Now == nil || env.Sleep == nil || env.Logf == nil {
		t.Fatalf("incomplete environment: %+v", env)
	}
	if len(c.Client.Actions()) != 0 {
		t.Errorf("creating an Env must not call the cluster: %v", c.Client.Actions())
	}
}

func TestEnvNeedsAContextAndPassesClientErrorsOn(t *testing.T) {
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{}, nil)
	if _, err := stack.Env(context.Background(), ""); err == nil {
		t.Error("an empty context must be refused")
	}
	if _, err := stack.Env(context.Background(), "ghost"); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("err = %v; want the client error naming the context", err)
	}
}

func TestPlannersCoverEveryPhase(t *testing.T) {
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": cluster()}, nil)
	seen := map[dr.Phase]int{}
	for _, p := range stack.Planners(plan()) {
		seen[p.Phase()]++
	}
	for _, phase := range dr.DownOrder {
		if seen[phase] == 0 {
			t.Errorf("no planner for phase %q", phase)
		}
	}
}

func TestRegisteredPlannersJoinPlannersRunAndSeeTheEnv(t *testing.T) {
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": cluster()}, nil)
	var gotPlan *dr.Plan
	var gotEnv *dr.Env
	stack.RegisterPlanners(func(p *dr.Plan, env *dr.Env) []dr.Planner {
		gotPlan, gotEnv = p, env
		return []dr.Planner{extraPlanner{}}
	})
	stack.RegisterPlanners(nil) // ignored

	var found bool
	for _, p := range stack.Planners(plan()) {
		if _, ok := p.(extraPlanner); ok {
			found = true
		}
	}
	if !found || gotPlan == nil || gotPlan.Name != "shop" || gotEnv == nil || gotEnv.Context != "dev" {
		t.Fatalf("hook not applied: found=%v plan=%v env=%v", found, gotPlan, gotEnv)
	}

	res, err := stack.Run(context.Background(), plan(), dr.DirDown, dr.Scope{}, dr.RunOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	var extra bool
	for _, s := range res.Steps {
		if s.ID == "extra/step" {
			extra = true
		}
	}
	if !extra {
		t.Errorf("the registered planner's step is missing from the run: %+v", res.Steps)
	}
}

type extraPlanner struct{}

func (extraPlanner) Phase() dr.Phase { return dr.PhasePrepare }
func (extraPlanner) Steps(context.Context, *dr.Env, *dr.Plan, dr.Direction, dr.Scope) ([]dr.Step, error) {
	return []dr.Step{{Phase: dr.PhasePrepare, Action: extraAction{}}}, nil
}

type extraAction struct{}

func (extraAction) ID() string                         { return "extra/step" }
func (extraAction) Title() string                      { return "Extra" }
func (extraAction) Describe() string                   { return "An extra step" }
func (extraAction) Risk() dr.Risk                      { return dr.RiskLow }
func (extraAction) Done(context.Context) (bool, error) { return true, nil }
func (extraAction) Apply(context.Context) error        { return nil }
func (extraAction) Equivalent() string                 { return "" }

func TestADryRunListsTheStepsAndChangesNothing(t *testing.T) {
	c := cluster()
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": c}, nil)

	res, err := stack.Run(context.Background(), plan(), dr.DirDown, dr.Scope{}, dr.RunOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != dr.StatusDryRun || len(res.Steps) == 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 2 {
		t.Errorf("a dry run changed the replicas to %d", got)
	}
	if entries, _ := os.ReadDir(stack.Dir()); len(entries) != 0 {
		t.Errorf("a dry run wrote files: %v", entries)
	}
}

func TestADownAndUpRoundTripRestoresTheReplicasAndLeavesARecord(t *testing.T) {
	c := cluster()
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": c}, nil)
	ctx := context.Background()

	down, err := stack.Run(ctx, plan(), dr.DirDown, dr.Scope{}, dr.RunOptions{})
	if err != nil || down.Status != dr.StatusSucceeded {
		t.Fatalf("down: %v %+v", err, down)
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 0 {
		t.Fatalf("replicas after down = %d", got)
	}
	if down.Actor != "tester" {
		t.Errorf("actor = %q; want the stack's actor", down.Actor)
	}

	up, err := stack.Run(ctx, plan(), dr.DirUp, dr.Scope{}, dr.RunOptions{})
	if err != nil || up.Status != dr.StatusSucceeded {
		t.Fatalf("up: %v %+v", err, up)
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 2 {
		t.Errorf("replicas after up = %d; want 2", got)
	}

	stored, found, err := stack.Status(ctx, down.ID)
	if err != nil || !found || stored.Direction != dr.DirDown {
		t.Errorf("Status = %+v, %v, %v", stored, found, err)
	}
	runs, err := stack.List(ctx, 10)
	if err != nil || len(runs) != 2 {
		t.Errorf("List = %d runs, %v", len(runs), err)
	}
	audit, err := stack.Audit(ctx, 0)
	if err != nil || len(audit) == 0 || audit[0].Actor != "tester" {
		t.Errorf("Audit = %v, %v", audit, err)
	}
}

func TestAReadOnlyStackRefusesRealRunsButNotDryRuns(t *testing.T) {
	c := cluster()
	stack := wiretest.NewStack(t, wire.Options{ReadOnly: true}, map[string]*wiretest.Cluster{"dev": c}, nil)

	if _, err := stack.Run(context.Background(), plan(), dr.DirDown, dr.Scope{}, dr.RunOptions{}); !errors.Is(err, dr.ErrReadOnly) {
		t.Errorf("err = %v; want ErrReadOnly", err)
	}
	if _, err := stack.Run(context.Background(), plan(), dr.DirDown, dr.Scope{}, dr.RunOptions{DryRun: true}); err != nil {
		t.Errorf("dry run in read-only mode: %v", err)
	}
	if _, err := stack.Drill(context.Background(), plan(), measure.DrillOptions{}); !errors.Is(err, dr.ErrReadOnly) {
		t.Errorf("drill err = %v; want ErrReadOnly", err)
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 2 {
		t.Errorf("replicas = %d", got)
	}
}

func TestAProductionContextNeedsItsNameAndThePhraseIsExposed(t *testing.T) {
	p := plan()
	p.Context = "eks-prod"
	c := cluster()
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"eks-prod": c}, nil)

	if got := stack.ConfirmPhrase(p, dr.DirDown, dr.Scope{}); got != "eks-prod" {
		t.Fatalf("phrase = %q", got)
	}
	if _, err := stack.Run(context.Background(), p, dr.DirDown, dr.Scope{}, dr.RunOptions{}); !errors.Is(err, dr.ErrConfirmationRequired) {
		t.Errorf("err = %v; want ErrConfirmationRequired", err)
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 2 {
		t.Errorf("replicas changed without confirmation: %d", got)
	}
}

func TestADrillIsRefusedOnAPlanThatIsNotALab(t *testing.T) {
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": cluster()}, nil)
	_, err := stack.Drill(context.Background(), plan(), measure.DrillOptions{})
	if !errors.Is(err, measure.ErrDrillRefused) {
		t.Errorf("err = %v; want ErrDrillRefused", err)
	}
}

func TestADrillOnALabPlanSavesAReportThatCanBeReadBack(t *testing.T) {
	c := cluster()
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": c}, nil)
	p := plan()
	p.Safety.LabOnly = true

	report, err := stack.Drill(context.Background(), p, measure.DrillOptions{HealthPoll: time.Millisecond, HealthTimeout: 30 * time.Minute})
	if err != nil {
		t.Fatalf("drill: %v", err)
	}
	if report.Outcome != measure.OutcomeRecovered {
		t.Errorf("outcome = %s; failures: %v", report.Outcome, report.Failures)
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 2 {
		t.Errorf("a drill must leave the system up: replicas = %d", got)
	}

	loaded, err := stack.Report(report.ID)
	if err != nil || loaded.ID != report.ID {
		t.Fatalf("Report = %+v, %v", loaded, err)
	}
	list, err := stack.Reports(0)
	if err != nil || len(list) != 1 {
		t.Errorf("Reports = %d, %v", len(list), err)
	}
	jsonPath, mdPath := stack.ReportPaths(report.ID)
	for _, f := range []string{jsonPath, mdPath} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("report file %s: %v", f, err)
		}
	}
	if !strings.HasPrefix(jsonPath, stack.Dir()) || filepath.Ext(mdPath) != ".md" {
		t.Errorf("paths = %s, %s", jsonPath, mdPath)
	}
}

func TestChecksWrappersReadOnlyAndNeverLeakSecrets(t *testing.T) {
	c := wiretest.NewCluster(
		wiretest.Deployment("shop", "api", 1),
		wiretest.Secret("shop", "db", "password", "TOPSECRETVALUE"),
	)
	other := wiretest.NewCluster(wiretest.Deployment("shop", "api", 3))
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": c, "dr": other}, nil)
	ctx := context.Background()

	report, err := stack.Readiness(ctx, "dev", checks.ReadinessOptions{Namespaces: []string{"shop"}})
	if err != nil {
		t.Fatal(err)
	}
	var single bool
	for _, ns := range report.Namespaces {
		for _, w := range ns.Workloads {
			for _, f := range w.Findings {
				if f.Rule == "DR001" {
					single = true
				}
			}
		}
	}
	if !single {
		t.Error("a single-replica Deployment should raise DR001")
	}

	var zipped bytes.Buffer
	result, err := stack.ExportZip(ctx, "dev", "shop", &zipped, checks.ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.IncludedSecrets {
		t.Error("secrets must not be exported by default")
	}
	archive, err := zip.NewReader(bytes.NewReader(zipped.Bytes()), int64(zipped.Len()))
	if err != nil || len(archive.File) == 0 {
		t.Fatalf("zip: %v (%d files)", err, len(archive.File))
	}
	for _, f := range archive.File {
		rc, _ := f.Open()
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		if bytes.Contains(body, []byte("TOPSECRETVALUE")) {
			t.Errorf("%s contains a secret value", f.Name)
		}
	}

	dir := filepath.Join(t.TempDir(), "out")
	if _, err := stack.ExportDir(ctx, "dev", "shop", dir, checks.ExportOptions{}); err != nil {
		t.Fatal(err)
	}

	diff, err := stack.Diff(ctx, "dev", "dr", checks.DiffOptions{Namespaces: []string{"shop"}})
	if err != nil || diff.Summary.Identical {
		t.Errorf("Diff = %+v, %v; replicas differ", diff, err)
	}
	if _, err := stack.Diff(ctx, "dev", "dev", checks.DiffOptions{}); err == nil {
		t.Error("diffing a context with itself must be refused")
	}
	if _, err := stack.Capacity(ctx, "dev", "dr", checks.CapacityOptions{Namespaces: []string{"shop"}}); err != nil {
		t.Errorf("Capacity: %v", err)
	}
	if _, err := stack.Dependencies(ctx, "dev", checks.DiscoverOptions{Namespaces: []string{"shop"}}); err != nil {
		t.Errorf("Dependencies: %v", err)
	}
	listing, err := stack.Backups(ctx, "dev", backups.ListOptions{})
	if err != nil || listing == nil {
		t.Errorf("Backups = %v, %v", listing, err)
	}
}

func TestTheDefaultStackReadsTheKubeconfigWithoutCallingAnyCluster(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
current-context: beta
clusters:
- name: c
  cluster: {server: "https://127.0.0.1:1"}
contexts:
- name: beta
  context: {cluster: c, user: u}
- name: alpha
  context: {cluster: c, user: u}
users:
- name: u
  user: {token: x}
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	stack, err := wire.New(wire.Options{Kubeconfig: kubeconfig, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := stack.CurrentContext(); got != "beta" {
		t.Errorf("CurrentContext = %q", got)
	}
	names, err := stack.Contexts()
	if err != nil || len(names) != 2 || names[0] != "alpha" {
		t.Errorf("Contexts = %v, %v", names, err)
	}
	env, err := stack.Env(context.Background(), "alpha")
	if err != nil || env.Client == nil || env.Dynamic == nil || env.Exec == nil {
		t.Fatalf("Env = %+v, %v", env, err)
	}
	if _, err := stack.Env(context.Background(), "nope"); err == nil {
		t.Error("an unknown context must be an error")
	}
	if stack.Actor() == "" || stack.ReadOnly() {
		t.Errorf("defaults: actor=%q readOnly=%v", stack.Actor(), stack.ReadOnly())
	}
}
