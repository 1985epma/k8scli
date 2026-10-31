package cmd

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr/wire"
	"github.com/1985epma/k8scli/internal/dr/wire/wiretest"
)

const devPlan = `version: 1
name: shop
context: dev
targets:
  rto: 30m
  rpo: 24h
apps:
  - name: shop
    namespace: shop
    workloads: [api]
    tier: 1
`

// drHarness runs the real command tree against fake clusters.
type drHarness struct {
	t      *testing.T
	app    *App
	out    *bytes.Buffer
	errOut *bytes.Buffer
	stack  *wire.Stack
	exec   *wiretest.Exec
}

func newDRHarness(t *testing.T, clusters map[string]*wiretest.Cluster) *drHarness {
	t.Helper()
	h := &drHarness{t: t, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, exec: &wiretest.Exec{}}
	h.stack = wiretest.NewStack(t, wire.Options{}, clusters, h.exec)
	h.app = &App{In: strings.NewReader(""), Out: h.out, ErrOut: h.errOut}

	previous := drStack
	drStack = func(*App) (*wire.Stack, error) { return h.stack, nil }
	t.Cleanup(func() { drStack = previous })
	return h
}

func (h *drHarness) run(args ...string) error {
	h.t.Helper()
	h.out.Reset()
	h.errOut.Reset()
	root := NewRootCmd(h.app)
	root.SetArgs(args)
	return root.ExecuteContext(context.Background())
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func shopCluster() *wiretest.Cluster {
	return wiretest.NewCluster(wiretest.Deployment("shop", "api", 2))
}

var runID = regexp.MustCompile(`Run (\S+):`)

func idOf(t *testing.T, text string) string {
	t.Helper()
	m := runID.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no run id in:\n%s", text)
	}
	return m[1]
}

// ---- plan -------------------------------------------------------------------------------------------------

func TestDRPlanValidate(t *testing.T) {
	h := newDRHarness(t, nil)
	file := writeFile(t, "plan.yaml", devPlan)

	if err := h.run("dr", "plan", "validate", file); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), `Plan "shop" is valid`) {
		t.Errorf("output = %q", h.out.String())
	}

	bad := writeFile(t, "bad.yaml", "version: 1\nname: \"\"\ncontext: dev\napps:\n  - name: a\n    namespace: x\n    tier: -3\n")
	err := h.run("dr", "plan", "validate", bad)
	if err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("err = %v; want the problems of bad.yaml", err)
	}
	if err := h.run("dr", "plan", "validate", filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("a missing file must be an error")
	}
}

func TestDRPlanShowMatchesTheGoldenFile(t *testing.T) {
	h := newDRHarness(t, nil)
	if err := h.run("dr", "plan", "show", "testdata/dr_plan_show.yaml"); err != nil {
		t.Fatal(err)
	}
	golden := "testdata/dr_plan_show.golden"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, h.out.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with UPDATE_GOLDEN=1 to create it)", err)
	}
	if h.out.String() != string(want) {
		t.Errorf("plan show changed:\n--- got ---\n%s\n--- want ---\n%s", h.out.String(), want)
	}

	if err := h.run("dr", "plan", "show", "testdata/dr_plan_show.yaml", "-o", "json"); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(h.out.Bytes()) {
		t.Errorf("not JSON: %s", h.out.String())
	}
}

// ---- down and up ------------------------------------------------------------------------------------------

func TestDRDownIsADryRunUnlessExecuteIsGiven(t *testing.T) {
	c := shopCluster()
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})
	file := writeFile(t, "plan.yaml", devPlan)

	if err := h.run("dr", "down", "--plan", file); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"Dry run", "Scale shop/api", "kubectl scale", "nothing changed", "--execute"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output missing %q:\n%s", want, out)
		}
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 2 {
		t.Errorf("a dry run changed the replicas to %d", got)
	}
	if runs, _ := h.stack.List(context.Background(), 10); len(runs) != 0 {
		t.Errorf("a dry run stored a run: %v", runs)
	}

	if err := h.run("dr", "down", "--plan", file, "-o", "json"); err != nil || !json.Valid(h.out.Bytes()) {
		t.Errorf("json dry run: %v %s", err, h.out.String())
	}
}

func TestDRDownThenUpRestoresTheReplicasAndLeavesARecord(t *testing.T) {
	c := shopCluster()
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})
	file := writeFile(t, "plan.yaml", devPlan)

	if err := h.run("dr", "down", "--plan", file, "--execute"); err != nil {
		t.Fatalf("down: %v\n%s", err, h.errOut.String())
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 0 {
		t.Fatalf("replicas after down = %d", got)
	}
	if !strings.Contains(h.errOut.String(), "-> ") || !strings.Contains(h.out.String(), ": succeeded") {
		t.Errorf("progress or summary missing:\nstdout:\n%s\nstderr:\n%s", h.out.String(), h.errOut.String())
	}
	downID := idOf(t, h.out.String())

	if err := h.run("dr", "up", "--plan", file, "--execute"); err != nil {
		t.Fatalf("up: %v\n%s", err, h.errOut.String())
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 2 {
		t.Errorf("replicas after up = %d; want 2", got)
	}

	if err := h.run("dr", "runs"); err != nil {
		t.Fatal(err)
	}
	if strings.Count(h.out.String(), "succeeded") != 2 || !strings.Contains(h.out.String(), downID) {
		t.Errorf("runs:\n%s", h.out.String())
	}
	if err := h.run("dr", "status"); err != nil || !strings.Contains(h.out.String(), "up") {
		t.Errorf("latest status: %v\n%s", err, h.out.String())
	}
	if err := h.run("dr", "status", downID, "-o", "json"); err != nil || !json.Valid(h.out.Bytes()) {
		t.Errorf("status json: %v\n%s", err, h.out.String())
	}
}

func TestDRExecuteInProductionNeedsTheContextNameTyped(t *testing.T) {
	c := shopCluster()
	h := newDRHarness(t, map[string]*wiretest.Cluster{"eks-prod": c})
	file := writeFile(t, "plan.yaml", strings.Replace(devPlan, "context: dev", "context: eks-prod", 1))

	// No --confirm and nothing to read: a script must not be able to skip the phrase.
	err := h.run("dr", "down", "--plan", file, "--execute")
	if err == nil || !strings.Contains(err.Error(), "--confirm eks-prod") {
		t.Fatalf("err = %v; want a message that says what to pass", err)
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 2 {
		t.Fatalf("replicas changed without confirmation: %d", got)
	}

	// A wrong phrase is refused by the engine.
	if err := h.run("dr", "down", "--plan", file, "--execute", "--confirm", "dev"); err == nil {
		t.Fatal("a wrong phrase must be refused")
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 2 {
		t.Fatalf("replicas changed with a wrong phrase: %d", got)
	}

	// Typing it at the prompt works.
	h.app.In = strings.NewReader("eks-prod\n")
	if err := h.run("dr", "down", "--plan", file, "--execute"); err != nil {
		t.Fatalf("down with the phrase typed: %v", err)
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 0 {
		t.Errorf("replicas = %d after a confirmed run", got)
	}
}

func TestDRScopeFlagsAreValidated(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": shopCluster()})
	file := writeFile(t, "plan.yaml", devPlan)

	if err := h.run("dr", "down", "--plan", file, "--scope", "bogus"); err == nil || !strings.Contains(err.Error(), "unknown scope") {
		t.Errorf("err = %v", err)
	}
	if err := h.run("dr", "down", "--plan", file, "--app", "ghost"); err == nil {
		t.Error("an app that is not in the plan must be refused")
	}
	if err := h.run("dr", "down", "--plan", file, "--scope", "apps", "--app", "shop"); err != nil {
		t.Errorf("a valid scope: %v", err)
	}
}

func TestDRAFailedRunCanBeResumedWithoutRepeatingWork(t *testing.T) {
	c := shopCluster()
	failing := true
	c.Client.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if failing && action.GetSubresource() == "scale" {
			return true, nil, errors.New("the API server said no")
		}
		return false, nil, nil
	})
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})
	file := writeFile(t, "plan.yaml", devPlan)

	err := h.run("dr", "down", "--plan", file, "--execute")
	if err == nil {
		t.Fatal("the run should fail")
	}
	if !strings.Contains(h.errOut.String(), "--resume") || !strings.Contains(h.errOut.String(), "continue with") {
		t.Errorf("no resume hint:\n%s", h.errOut.String())
	}
	id := idOf(t, h.out.String())
	if got := c.Replicas("deployment", "shop", "api"); got != 2 {
		t.Fatalf("replicas = %d after the failed run", got)
	}

	// Without --execute, resume only shows where the run stands.
	if err := h.run("dr", "resume", id, "--plan", file); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "failed") || !strings.Contains(h.out.String(), "dr resume "+id) {
		t.Errorf("resume preview:\n%s", h.out.String())
	}

	failing = false
	if err := h.run("dr", "resume", id, "--plan", file, "--execute"); err != nil {
		t.Fatalf("resume: %v\n%s", err, h.errOut.String())
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 0 {
		t.Errorf("replicas = %d after resuming", got)
	}
	if err := h.run("dr", "resume", "no-such-run", "--plan", file); err == nil {
		t.Error("an unknown run id must be an error")
	}
	if err := h.run("dr", "status", "no-such-run"); err == nil {
		t.Error("an unknown run id must be an error")
	}
}

func TestDRAManualStepTellsThePersonWhatToDoAndHowToContinue(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": shopCluster()})
	file := writeFile(t, "plan.yaml", devPlan+"provider:\n  kind: docker-desktop\n")

	// Powering off needs the stop phrase.
	if err := h.run("dr", "down", "--plan", file, "--scope", "power", "--execute"); err == nil || !strings.Contains(err.Error(), "--confirm 'stop dev'") {
		t.Fatalf("err = %v; want the stop phrase to be asked for", err)
	}

	err := h.run("dr", "down", "--plan", file, "--scope", "power", "--execute", "--confirm", "stop dev")
	if err == nil {
		t.Fatal("a manual step ends the run")
	}
	stderr := h.errOut.String()
	for _, want := range []string{"WAITING FOR YOU", "waiting for you", "k8scli dr down --plan", "--resume", "--execute", "--confirm 'stop dev'"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

func TestDRClusterStopShowsTheProviderCommandsAsADryRun(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{"kind-lab": shopCluster()})
	h.exec.Answers = map[string]string{
		"docker ps*":      "lab-control-plane\nlab-worker\n",
		"docker inspect*": "true",
	}
	file := writeFile(t, "plan.yaml", strings.Replace(devPlan, "context: dev", "context: kind-lab", 1)+"provider:\n  kind: kind\n  params:\n    name: lab\n")

	if err := h.run("dr", "cluster", "stop", "--plan", file); err != nil {
		t.Fatalf("%v\n%s", err, h.errOut.String())
	}
	out := h.out.String()
	for _, want := range []string{"docker stop lab-worker", "docker stop lab-control-plane", "destructive", "--confirm 'stop kind-lab'", "nothing changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(strings.Join(h.exec.Calls(), "\n"), "docker stop") {
		t.Error("a dry run must not stop anything")
	}
	if !strings.Contains(h.errOut.String(), "Stopping the whole cluster") {
		t.Errorf("banner missing:\n%s", h.errOut.String())
	}
}

// ---- nodes ------------------------------------------------------------------------------------------------

func TestDRNodesDrainAndUncordonTouchOnlyWorkers(t *testing.T) {
	c := wiretest.NewCluster(wiretest.Node("worker-1"), wiretest.Node("worker-2"), wiretest.ControlPlane("cp"))
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})
	h.app.In = strings.NewReader("")

	// Context from the kubeconfig: the harness stack has none, so it is passed explicitly.
	if err := h.run("dr", "nodes", "drain", "--context", "dev", "--node", "worker-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "worker-1") || strings.Contains(h.out.String(), "worker-2") {
		t.Errorf("dry run should list only worker-1:\n%s", h.out.String())
	}

	if err := h.run("dr", "nodes", "drain", "--context", "dev", "--node", "worker-1", "--execute"); err != nil {
		t.Fatalf("%v\n%s", err, h.errOut.String())
	}
	unschedulable := func(name string) bool {
		n, err := c.Client.CoreV1().Nodes().Get(context.Background(), name, metav1GetOptions())
		if err != nil {
			t.Fatal(err)
		}
		return n.Spec.Unschedulable
	}
	if !unschedulable("worker-1") || unschedulable("worker-2") || unschedulable("cp") {
		t.Errorf("only worker-1 should be cordoned: w1=%v w2=%v cp=%v", unschedulable("worker-1"), unschedulable("worker-2"), unschedulable("cp"))
	}

	if err := h.run("dr", "nodes", "uncordon", "--context", "dev", "--execute"); err != nil {
		t.Fatalf("%v\n%s", err, h.errOut.String())
	}
	if unschedulable("worker-1") {
		t.Error("uncordon should restore worker-1")
	}
}

// ---- read-only commands -----------------------------------------------------------------------------------

func TestDRCheckPrintsFindingsJSONAndCanFailACIBuild(t *testing.T) {
	c := wiretest.NewCluster(wiretest.Deployment("shop", "api", 1))
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})

	if err := h.run("dr", "check", "--context", "dev", "-n", "shop"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Readiness of context dev", "DR001", "shop/deployment/api", "does not prove", "SCORE"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q:\n%s", want, h.out.String())
		}
	}

	if err := h.run("dr", "check", "--context", "dev", "-n", "shop", "-o", "json"); err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Readiness struct {
			Score int `json:"score"`
		} `json:"readiness"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, h.out.String())
	}

	if err := h.run("dr", "check", "--context", "dev", "-n", "shop", "--fail-on", "high"); err == nil || !strings.Contains(err.Error(), "finding(s)") {
		t.Errorf("--fail-on high should fail: %v", err)
	}
	if err := h.run("dr", "check", "--context", "dev", "-n", "empty", "--fail-on", "high"); err != nil {
		t.Errorf("nothing found must pass: %v", err)
	}
	if err := h.run("dr", "check", "--context", "dev", "--min-severity", "bogus"); err == nil {
		t.Error("a bad severity must be refused")
	}
}

func TestDRCheckCanAlsoCheckCapacityAndSuggestTiers(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{
		"dev": wiretest.NewCluster(wiretest.Deployment("shop", "api", 2)),
		"dr":  wiretest.NewCluster(wiretest.Node("n1")),
	})
	if err := h.run("dr", "check", "--context", "dev", "-n", "shop", "--capacity-to", "dr", "--suggest-tiers"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Standby capacity (dev -> dr)", "Suggested start order"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q:\n%s", want, h.out.String())
		}
	}
}

func TestDRExportNeverWritesSecretsAndNeverOverwrites(t *testing.T) {
	c := wiretest.NewCluster(wiretest.Deployment("shop", "api", 2), wiretest.Secret("shop", "db", "password", "TOPSECRETVALUE"))
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})
	out := filepath.Join(t.TempDir(), "shop.zip")

	if err := h.run("dr", "export", "--context", "dev", "-n", "shop", "-o", out); err != nil {
		t.Fatalf("%v\n%s", err, h.errOut.String())
	}
	if !strings.Contains(h.out.String(), "Secrets were NOT exported") {
		t.Errorf("output:\n%s", h.out.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(archive.File) == 0 {
		t.Fatalf("zip: %v", err)
	}
	for _, f := range archive.File {
		rc, _ := f.Open()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(rc)
		_ = rc.Close()
		if strings.Contains(buf.String(), "TOPSECRETVALUE") {
			t.Errorf("%s contains a secret value", f.Name)
		}
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o600 {
		t.Errorf("export file mode = %v", info.Mode().Perm())
	}

	if err := h.run("dr", "export", "--context", "dev", "-n", "shop", "-o", out); err == nil {
		t.Error("an existing export must not be overwritten")
	}
	if err := h.run("dr", "export", "--context", "dev", "-o", filepath.Join(t.TempDir(), "x.zip")); err == nil {
		t.Error("export needs one namespace")
	}
	if err := h.run("dr", "export", "--context", "dev", "-n", "a,b"); err == nil {
		t.Error("export takes one namespace, not a list")
	}

	dir := filepath.Join(t.TempDir(), "folder")
	if err := h.run("dr", "export", "--context", "dev", "-n", "shop", "-o", dir); err != nil {
		t.Fatalf("export to a folder: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) == 0 {
		t.Error("nothing written to the folder")
	}
}

func TestDRDiffComparesTwoClusters(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{
		"dev": wiretest.NewCluster(wiretest.Namespace("shop"), wiretest.Deployment("shop", "api", 2)),
		"dr":  wiretest.NewCluster(wiretest.Namespace("shop"), wiretest.Deployment("shop", "api", 3)),
	})
	if err := h.run("dr", "diff", "--context", "dev", "--to", "dr", "-n", "shop"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "Comparing dev (primary) with dr (standby)") || !strings.Contains(h.out.String(), "deployment/api") {
		t.Errorf("diff output:\n%s", h.out.String())
	}
	if err := h.run("dr", "diff", "--context", "dev", "-n", "shop"); err == nil || !strings.Contains(err.Error(), "--to") {
		t.Errorf("missing --to: %v", err)
	}
	if err := h.run("dr", "diff", "--context", "dev", "--to", "dev"); err == nil {
		t.Error("diffing a context with itself must be refused")
	}
	if err := h.run("dr", "diff", "--context", "dev", "--to", "dr", "-n", "shop", "-o", "json"); err != nil || !json.Valid(h.out.Bytes()) {
		t.Errorf("json: %v", err)
	}
}

func TestDRBackupsReportsWhatIsThere(t *testing.T) {
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": shopCluster()})
	if err := h.run("dr", "backups", "--context", "dev"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "Backups of context dev") {
		t.Errorf("output:\n%s", h.out.String())
	}
	if err := h.run("dr", "backups", "--context", "dev", "-o", "json"); err != nil || !json.Valid(h.out.Bytes()) {
		t.Errorf("json: %v", err)
	}
}

// ---- drill and report -------------------------------------------------------------------------------------

func TestDRDrillIsADryRunByDefaultAndRefusesPlansThatAreNotLabs(t *testing.T) {
	c := shopCluster()
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})
	file := writeFile(t, "plan.yaml", devPlan)

	if err := h.run("dr", "drill", "--plan", file); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Drill, stopping phase", "Drill, starting phase", "NOT marked safety.labOnly", "--allow-production", "nothing changed"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q:\n%s", want, h.out.String())
		}
	}
	if err := h.run("dr", "drill", "--plan", file, "--execute"); err == nil || !strings.Contains(err.Error(), "labOnly") {
		t.Errorf("err = %v; a drill on a non-lab plan must be refused", err)
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 2 {
		t.Errorf("replicas = %d", got)
	}
}

func TestDRDrillOnALabPlanMeasuresAndTheReportCanBeRead(t *testing.T) {
	c := shopCluster()
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})
	file := writeFile(t, "plan.yaml", devPlan+"safety:\n  labOnly: true\n")

	if err := h.run("dr", "drill", "--plan", file, "--execute"); err != nil {
		t.Fatalf("drill: %v\n%s", err, h.errOut.String())
	}
	for _, want := range []string{"recovered", "RTO", "Report saved", "k8scli dr report"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("drill summary missing %q:\n%s", want, h.out.String())
		}
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 2 {
		t.Errorf("a drill must leave the system up: replicas = %d", got)
	}

	if err := h.run("dr", "report"); err != nil || !strings.Contains(h.out.String(), "shop") {
		t.Errorf("markdown report: %v\n%s", err, h.out.String())
	}
	if err := h.run("dr", "report", "--format", "json"); err != nil || !json.Valid(h.out.Bytes()) {
		t.Errorf("json report: %v", err)
	}
	if err := h.run("dr", "report", "--format", "prometheus"); err != nil || !strings.Contains(h.out.String(), "k8scli_dr_rto_seconds") {
		t.Errorf("prometheus report: %v\n%s", err, h.out.String())
	}
	if err := h.run("dr", "report", "--format", "xml"); err == nil {
		t.Error("an unknown format must be refused")
	}
	if err := h.run("dr", "report", "--list"); err != nil || !strings.Contains(h.out.String(), "RTO trend") {
		t.Errorf("history: %v\n%s", err, h.out.String())
	}
	if err := h.run("dr", "report", "no-such-report"); err == nil {
		t.Error("an unknown report id must be an error")
	}
}

func TestDRReportAndStatusSayWhenThereIsNothingYet(t *testing.T) {
	h := newDRHarness(t, nil)
	for _, args := range [][]string{{"dr", "report"}, {"dr", "report", "--list"}, {"dr", "status"}, {"dr", "runs"}} {
		if err := h.run(args...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
		if h.errOut.Len() == 0 {
			t.Errorf("%v should explain that there is nothing yet", args)
		}
	}
}

// ---- extension point --------------------------------------------------------------------------------------

func TestExtraDRCommandsAreRegisteredAsSubcommands(t *testing.T) {
	previous := extraDRCommands
	t.Cleanup(func() { extraDRCommands = previous })
	extraDRCommands = append(extraDRCommands, func(app *App) *cobra.Command {
		return &cobra.Command{Use: "hello", Short: "x", RunE: func(*cobra.Command, []string) error {
			_, err := app.Out.Write([]byte("hello from an extra command\n"))
			return err
		}}
	})

	h := newDRHarness(t, nil)
	if err := h.run("dr", "hello"); err != nil || !strings.Contains(h.out.String(), "hello from an extra command") {
		t.Errorf("extra command not registered: %v\n%s", err, h.out.String())
	}
	if err := h.run("help", "dr"); err != nil || !strings.Contains(h.out.String(), "hello") {
		t.Errorf("extra command missing from help: %v", err)
	}
}

func TestDRHelperFunctions(t *testing.T) {
	if got := splitList(" a, b ,,c "); strings.Join(got, "|") != "a|b|c" {
		t.Errorf("splitList = %v", got)
	}
	for in, want := range map[string]string{"plain-1.x": "plain-1.x", "has space": "'has space'", "it's": `'it'\''s'`, "": "''"} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %q; want %q", in, got, want)
		}
	}
	if got := humanDuration(0); got != "0s" {
		t.Errorf("humanDuration(0) = %q", got)
	}
	if got := humanTime(zeroTime()); got != "-" {
		t.Errorf("humanTime(zero) = %q", got)
	}
}

func metav1GetOptions() metav1.GetOptions { return metav1.GetOptions{} }

func zeroTime() time.Time { return time.Time{} }

func TestDRResolvesTheContextFromFlagPlanOrKubeconfig(t *testing.T) {
	clusters := map[string]*wiretest.Cluster{"dev": shopCluster(), "cur": shopCluster()}
	h := newDRHarness(t, clusters)

	// Nothing says which context: a clear error, not the developer's real kubeconfig.
	if err := h.run("dr", "backups"); err == nil || !strings.Contains(err.Error(), "no context") {
		t.Errorf("err = %v; want a message about the missing context", err)
	}

	// The kubeconfig's current context is the last resort.
	withCurrent := wiretest.NewStack(t, wire.Options{Kubeconfig: wiretest.Kubeconfig(t, "cur")}, clusters, nil)
	drStack = func(*App) (*wire.Stack, error) { return withCurrent, nil }
	if err := h.run("dr", "backups"); err != nil || !strings.Contains(h.out.String(), "context cur") {
		t.Errorf("current context: %v\n%s", err, h.out.String())
	}

	// The plan wins over the current context, and the flag wins over the plan.
	file := writeFile(t, "plan.yaml", devPlan)
	if err := h.run("dr", "diff", "--plan", file, "--to", "cur", "-n", "shop"); err != nil || !strings.Contains(h.out.String(), "Comparing dev") {
		t.Errorf("plan context: %v\n%s", err, h.out.String())
	}
	if err := h.run("dr", "backups", "--context", "dev"); err != nil || !strings.Contains(h.out.String(), "context dev") {
		t.Errorf("flag context: %v\n%s", err, h.out.String())
	}
}

func TestDRUpWithAnUnknownReplicaCountChangesNothingAndSaysHowToGoOn(t *testing.T) {
	// A workload that is at 0 with no record of what it had: the plan must not guess.
	c := wiretest.NewCluster(wiretest.Deployment("shop", "api", 0))
	h := newDRHarness(t, map[string]*wiretest.Cluster{"dev": c})
	file := writeFile(t, "plan.yaml", devPlan)

	err := h.run("dr", "up", "--plan", file, "--execute")
	if err == nil || !strings.Contains(err.Error(), "no recorded replica count") ||
		!strings.Contains(err.Error(), "--skip-unknown-replicas") || !strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatalf("err = %v; want the cause and the way forward", err)
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 0 {
		t.Errorf("replicas = %d; nothing should have changed", got)
	}

	if err := h.run("dr", "up", "--plan", file, "--execute", "--skip-unknown-replicas"); err != nil {
		t.Fatalf("with the flag the run should leave the workload out: %v\n%s", err, h.errOut.String())
	}
	if got := c.Replicas("deployment", "shop", "api"); got != 0 {
		t.Errorf("a workload with an unknown count must stay stopped, replicas = %d", got)
	}
}
