//go:build lab

// Live test against the kind lab created by scripts/dr-lab.sh up. It is excluded from every normal
// build and from CI; run it by hand:
//
//	go test -tags lab -count=1 -run TestLab -v -timeout 30m ./internal/dr/dbops/
//
// It stops and starts the databases of the lab, so it takes the lab lock first (see scripts/dr-lab.sh)
// and it only ever talks to the context kind-k8scli-lab.
package dbops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/kubeconfig"
)

// labContext is the only context this file may act on.
const labContext = "kind-k8scli-lab"

func labLockDir() string {
	if dir := os.Getenv("K8SCLI_LAB_LOCK_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "k8scli-dr-lab.lock")
}

// takeLabLock waits for the exclusive lab lock (an atomic mkdir) and releases it when the test ends.
func takeLabLock(t *testing.T) {
	t.Helper()
	dir := labLockDir()
	deadline := time.Now().Add(20 * time.Minute)
	for {
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			t.Cleanup(func() { _ = os.Remove(dir) })
			return
		}
		if !os.IsExist(err) || time.Now().After(deadline) {
			t.Fatalf("cannot take the lab lock %s: %v", dir, err)
		}
		t.Logf("the lab is in use (%s exists); waiting", dir)
		time.Sleep(15 * time.Second)
	}
}

func labEnv(t *testing.T) *dr.Env {
	t.Helper()
	loader := kubeconfig.New("")
	contexts, err := loader.Contexts()
	if err != nil {
		t.Skipf("no kubeconfig: %v", err)
	}
	found := false
	for _, c := range contexts {
		found = found || c.Name == labContext
	}
	if !found {
		t.Skipf("context %s does not exist: run scripts/dr-lab.sh up", labContext)
	}

	config, err := loader.RESTConfigFor(labContext)
	if err != nil {
		t.Fatal(err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return &dr.Env{Context: labContext, Client: client, Dynamic: dyn, State: newMemState(), Logf: t.Logf}
}

func labKubectl(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--context", labContext}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...) // #nosec G204 -- fixed program, test-only arguments
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("kubectl %v: %w: %s", full, err, stderr.String())
	}
	return out.String(), nil
}

func labScript(ctx context.Context, args ...string) (string, error) {
	script, err := filepath.Abs("../../../scripts/dr-lab.sh")
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, script, args...) // #nosec G204 -- the repository's own lab script
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("dr-lab.sh %v: %w: %s", args, err, stderr.String())
	}
	return out.String(), nil
}

var (
	heartbeatID   = regexp.MustCompile(`newest heartbeat id=(\d+)`)
	heartbeatRows = regexp.MustCompile(`rows=(\d+)`)
)

func heartbeat(t *testing.T, ctx context.Context, mode string) (id, rows int) {
	t.Helper()
	out, err := labScript(ctx, "check-data", mode)
	if err != nil {
		t.Fatalf("check-data %s: %v\n%s", mode, err, out)
	}
	t.Logf("check-data %s: %s", mode, out)
	idMatch, rowMatch := heartbeatID.FindStringSubmatch(out), heartbeatRows.FindStringSubmatch(out)
	if idMatch == nil || rowMatch == nil {
		t.Fatalf("cannot parse the check-data output: %q", out)
	}
	id, _ = strconv.Atoi(idMatch[1])
	rows, _ = strconv.Atoi(rowMatch[1])
	return id, rows
}

// restoreLab puts the databases back the way the lab script built them, whatever happened in the test.
func restoreLab(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if out, err := labKubectl(ctx, "-n", "shop", "annotate", "cluster.postgresql.cnpg.io", "shop-db", "--overwrite", "cnpg.io/hibernation=off"); err != nil {
		t.Logf("restore (cnpg): %v %s", err, out)
	}
	if out, err := labKubectl(ctx, "-n", "shop", "scale", "statefulset", "legacy-db", "--replicas=2"); err != nil {
		t.Logf("restore (statefulset): %v %s", err, out)
	}
	if out, err := labKubectl(ctx, "-n", "shop", "annotate", "statefulset", "legacy-db", replicasAnnotation+"-"); err != nil {
		t.Logf("restore (annotation): %v %s", err, out)
	}
}

func runAll(t *testing.T, ctx context.Context, steps []dr.Step) {
	t.Helper()
	for _, step := range steps {
		start := time.Now()
		skipped, err := runStep(ctx, step)
		if err != nil {
			t.Fatalf("%s: %v", step.Action.ID(), err)
		}
		t.Logf("%-40s skipped=%-5v took %s", step.Action.ID(), skipped, time.Since(start).Round(time.Second))
	}
}

func TestLabDatabasesSurviveADownAndUpCycle(t *testing.T) {
	takeLabLock(t)
	env := labEnv(t)
	t.Cleanup(func() { restoreLab(t) })

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	plan := &dr.Plan{Version: dr.PlanVersion, Name: "shop-lab", Context: labContext, Databases: []dr.Database{
		{Name: "shop-db", Namespace: "shop", Kind: KindCNPG, Ref: "shop-db", Tier: 0},
		{Name: "legacy-db", Namespace: "shop", Kind: KindStatefulSet, Ref: "legacy-db", Tier: 0},
	}}
	planner := NewPlanner(Options{GatePoll: 3 * time.Second, GateTimeout: 10 * time.Minute})

	// The lab must be healthy to begin with, otherwise the pre-flight refuses (which is also a result).
	shop := &cnpgTarget{p: planner, env: env, plan: plan, db: plan.Databases[0]}
	cluster, err := shop.get(ctx)
	if err != nil || !isHealthy(cluster) {
		t.Fatalf("the lab is not healthy (%v): run scripts/dr-lab.sh status", err)
	}

	// 1. Write data in both databases.
	idBefore, rowsBefore := heartbeat(t, ctx, "write")
	marker := fmt.Sprintf("survives-%d", time.Now().Unix())
	if out, err := labKubectl(ctx, "-n", "shop", "exec", "legacy-db-0", "-c", "redis", "--", "redis-cli", "set", "dr-lab-key", marker); err != nil {
		t.Fatalf("writing the redis key: %v %s", err, out)
	}
	time.Sleep(3 * time.Second) // appendfsync everysec

	// 2. The RPO estimate must work against a real operator without credentials.
	estimates, err := planner.EstimateRPO(ctx, env, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range estimates {
		t.Logf("RPO estimate: %+v", e)
	}

	// 3. Down.
	downStart := time.Now()
	down, err := planner.Steps(ctx, env, plan, dr.DirDown, dr.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range down {
		t.Logf("planned: %-40s %s", step.Action.ID(), step.Action.Describe())
	}
	runAll(t, ctx, down)
	t.Logf("databases down in %s", time.Since(downStart).Round(time.Second))

	pods, err := env.Client.CoreV1().Pods("shop").List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=shop-db"})
	if err != nil || len(pods.Items) != 0 {
		t.Errorf("CloudNativePG instance pods after hibernation: %d (%v)", len(pods.Items), err)
	}
	sts, err := env.Client.AppsV1().StatefulSets("shop").Get(ctx, "legacy-db", metav1.GetOptions{})
	if err != nil || *sts.Spec.Replicas != 0 || sts.Annotations[replicasAnnotation] != "2" {
		t.Errorf("legacy-db after stop: replicas=%v annotation=%q (%v)", sts.Spec.Replicas, sts.Annotations[replicasAnnotation], err)
	}
	pvcs, err := env.Client.CoreV1().PersistentVolumeClaims("shop").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PVCs kept while the databases are down: %d", len(pvcs.Items))
	if len(pvcs.Items) < 4 {
		t.Errorf("expected at least 4 PVCs (2 CloudNativePG + 2 Redis) to survive, found %d", len(pvcs.Items))
	}

	// 4. Up.
	upStart := time.Now()
	up, err := planner.Steps(ctx, env, plan, dr.DirUp, dr.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	runAll(t, ctx, up)
	t.Logf("databases up in %s", time.Since(upStart).Round(time.Second))

	// 5. The data must be there.
	idAfter, rowsAfter := heartbeat(t, ctx, "read")
	if idAfter < idBefore || rowsAfter < rowsBefore {
		t.Errorf("PostgreSQL lost data: heartbeat %d/%d rows before, %d/%d after", idBefore, rowsBefore, idAfter, rowsAfter)
	}
	got, err := labKubectl(ctx, "-n", "shop", "exec", "legacy-db-0", "-c", "redis", "--", "redis-cli", "get", "dr-lab-key")
	if err != nil || got != marker+"\n" {
		t.Errorf("Redis lost data: got %q, want %q (%v)", got, marker, err)
	}

	cluster, err = shop.get(ctx)
	if err != nil || !isHealthy(cluster) {
		t.Errorf("shop-db is not healthy after the cycle: %v", err)
	}
	sts, err = env.Client.AppsV1().StatefulSets("shop").Get(ctx, "legacy-db", metav1.GetOptions{})
	if err != nil || *sts.Spec.Replicas != 2 || sts.Status.ReadyReplicas != 2 {
		t.Errorf("legacy-db after start: replicas=%v ready=%d (%v)", sts.Spec.Replicas, sts.Status.ReadyReplicas, err)
	}
	if _, left := sts.Annotations[replicasAnnotation]; left {
		t.Error("the replica annotation was not cleared")
	}
}

func TestLabPreflightBlocksAnOldBackup(t *testing.T) {
	takeLabLock(t)
	env := labEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The lab has no backup storage, so with a limit set the stop must be refused and nothing may change.
	plan := &dr.Plan{Version: dr.PlanVersion, Name: "shop-lab", Context: labContext, Databases: []dr.Database{
		{Name: "shop-db", Namespace: "shop", Kind: KindCNPG, Ref: "shop-db", MaxBackupAge: "24h"},
		{Name: "legacy-db", Namespace: "shop", Kind: KindStatefulSet, Ref: "legacy-db", MaxBackupAge: "24h"},
	}}
	steps, err := NewPlanner(Options{}).Steps(ctx, env, plan, dr.DirDown, dr.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.Action.ID() == "databases/shop/shop-db:preflight" || step.Action.ID() == "databases/shop/legacy-db:preflight" {
			err := step.Action.Apply(ctx)
			t.Logf("%s -> %v", step.Action.ID(), err)
			if err == nil {
				t.Errorf("%s must be blocked: the lab has no backups", step.Action.ID())
			}
		}
	}
	cluster, err := (&cnpgTarget{p: NewPlanner(Options{}), env: env, plan: plan, db: plan.Databases[0]}).get(ctx)
	if err != nil || !isHealthy(cluster) {
		t.Errorf("the blocked pre-flight must leave the cluster alone: %v", err)
	}
}
