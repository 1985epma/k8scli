package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/1985epma/k8scli/internal/dr"
)

// ---- EKS -----------------------------------------------------------------------------------------------

// fakeEKS behaves like one node group: describe reads the numbers, update-nodegroup-config changes them.
type fakeEKS struct {
	mu                  sync.Mutex
	status              string
	minSize, maxSize, d int
	describeErr         error
}

func (f *fakeEKS) handle(name string, args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != "aws" || len(args) < 2 || args[0] != "eks" {
		return "", fmt.Errorf("unexpected command %s %v", name, args)
	}
	switch args[1] {
	case "describe-nodegroup":
		if f.describeErr != nil {
			return "", f.describeErr
		}
		return fmt.Sprintf(`{"status":%q,"scalingConfig":{"minSize":%d,"maxSize":%d,"desiredSize":%d}}`, f.status, f.minSize, f.maxSize, f.d), nil
	case "update-nodegroup-config":
		cfg := args[indexOf(args, "--scaling-config")+1]
		for _, kv := range strings.Split(cfg, ",") {
			key, value, _ := strings.Cut(kv, "=")
			n, err := strconv.Atoi(value)
			if err != nil {
				return "", fmt.Errorf("bad scaling config %q", cfg)
			}
			switch key {
			case "minSize":
				f.minSize = n
			case "maxSize":
				f.maxSize = n
			case "desiredSize":
				f.d = n
			}
		}
		f.status = "ACTIVE"
		return "", nil
	case "wait":
		return "", nil
	}
	return "", fmt.Errorf("unexpected aws eks %s", args[1])
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

var eksParams = map[string]string{"cluster": "prod", "nodegroup": "workers", "region": "us-east-1"}

func eksSetup(f *fakeEKS) *testEnv {
	e := newTestEnv()
	e.exec.handler = f.handle
	return e
}

func TestEKSScaleDownSavesTheSizeThenScalesToZeroAndWaits(t *testing.T) {
	ctx := context.Background()
	f := &fakeEKS{status: "ACTIVE", minSize: 2, maxSize: 6, d: 3}
	e := eksSetup(f)
	step := e.steps(t, plan(KindEKSNodegroup, eksParams), dr.DirDown)[0]

	if done, err := step.Action.Done(ctx); err != nil || done {
		t.Fatalf("Done = %v, %v; a running node group is not done", done, err)
	}
	if err := step.Action.Apply(ctx); err != nil {
		t.Fatal(err)
	}

	var saved savedState
	if found, _ := e.state.Load(ctx, StateKey("ctx"), &saved); !found || saved.Min != 2 || saved.Max != 6 || saved.Desired != 3 || saved.Provider != KindEKSNodegroup {
		t.Errorf("saved = %+v (found %v)", saved, found)
	}
	wantTail := []string{
		"aws eks update-nodegroup-config --cluster-name prod --nodegroup-name workers --scaling-config minSize=0,maxSize=6,desiredSize=0 --region us-east-1",
		"aws eks wait nodegroup-active --cluster-name prod --nodegroup-name workers --region us-east-1",
	}
	lines := e.exec.lines()
	if got := lines[len(lines)-2:]; got[0] != wantTail[0] || got[1] != wantTail[1] {
		t.Errorf("last commands = %v\nwant %v", got, wantTail)
	}
	if f.d != 0 || f.minSize != 0 || f.maxSize != 6 {
		t.Errorf("node group = min %d max %d desired %d; want 0/6/0 (the maximum is kept)", f.minSize, f.maxSize, f.d)
	}
	if done, _ := step.Action.Done(ctx); !done {
		t.Error("a node group scaled to zero and ACTIVE is done")
	}
}

func TestEKSScaleUpRestoresTheSavedSizeAndForgetsIt(t *testing.T) {
	ctx := context.Background()
	f := &fakeEKS{status: "ACTIVE", minSize: 2, maxSize: 6, d: 3}
	e := eksSetup(f)
	down := e.steps(t, plan(KindEKSNodegroup, eksParams), dr.DirDown)[0]
	up := e.steps(t, plan(KindEKSNodegroup, eksParams), dr.DirUp)[0]

	if err := down.Action.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	// Dry-run text: the up command already shows the sizes saved by the power-off.
	if got := up.Action.Equivalent(); !strings.Contains(got, "minSize=2,maxSize=6,desiredSize=3") {
		t.Errorf("up equivalent = %s", got)
	}
	if done, _ := up.Action.Done(ctx); done {
		t.Fatal("up is not done while the group has no nodes")
	}
	if err := up.Action.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if f.minSize != 2 || f.maxSize != 6 || f.d != 3 {
		t.Errorf("restored to min %d max %d desired %d", f.minSize, f.maxSize, f.d)
	}
	if e.state.has(StateKey("ctx")) {
		t.Error("the saved state must be removed once restored")
	}
	if done, _ := up.Action.Done(ctx); !done {
		t.Error("up should be done now")
	}
}

func TestEKSScaleUpRefusesWithoutSavedState(t *testing.T) {
	f := &fakeEKS{status: "ACTIVE", d: 0, maxSize: 4}
	e := eksSetup(f)
	up := e.steps(t, plan(KindEKSNodegroup, eksParams), dr.DirUp)[0]

	err := up.Action.Apply(context.Background())
	if !errors.Is(err, dr.ErrPreflight) || !errors.Is(err, errNoState) {
		t.Fatalf("err = %v; want a pre-flight refusal that explains nothing was saved", err)
	}
	for _, line := range e.exec.lines() {
		if strings.Contains(line, "update-nodegroup-config") {
			t.Errorf("scaled up without knowing the size: %s", line)
		}
	}
	if f.d != 0 {
		t.Error("the node group was changed")
	}
}

func TestEKSScaleUpWhenNodesAreAlreadyThere(t *testing.T) {
	f := &fakeEKS{status: "ACTIVE", minSize: 1, maxSize: 3, d: 2}
	e := eksSetup(f)
	up := e.steps(t, plan(KindEKSNodegroup, eksParams), dr.DirUp)[0]
	if done, err := up.Action.Done(context.Background()); err != nil || !done {
		t.Errorf("Done = %v, %v", done, err)
	}
	// A resumed run may still reach Apply: it must only wait, and tidy up stale state.
	_ = e.state.Save(context.Background(), StateKey("ctx"), savedState{Provider: KindEKSNodegroup, Target: "prod/workers/us-east-1/", Min: 9, Max: 9, Desired: 9})
	if err := up.Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.d != 2 || f.maxSize != 3 {
		t.Errorf("a running group was resized to %d/%d", f.d, f.maxSize)
	}
	if e.state.has(StateKey("ctx")) {
		t.Error("stale state should be removed")
	}
}

func TestEKSResumeAfterAnInterruptedScaleDownKeepsTheOriginalSize(t *testing.T) {
	ctx := context.Background()
	f := &fakeEKS{status: "ACTIVE", minSize: 2, maxSize: 6, d: 3}
	e := eksSetup(f)
	step := e.steps(t, plan(KindEKSNodegroup, eksParams), dr.DirDown)[0]
	if err := step.Action.Apply(ctx); err != nil {
		t.Fatal(err)
	}

	// The run died while AWS was still applying the update. On resume the group reads 0/6/0 UPDATING.
	f.status = "UPDATING"
	if done, _ := step.Action.Done(ctx); done {
		t.Error("an update still in flight is not done")
	}
	before := e.exec.count()
	if err := step.Action.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	for _, line := range e.exec.lines()[before:] {
		if strings.Contains(line, "update-nodegroup-config") {
			t.Errorf("the update was sent twice (AWS answers ResourceInUse): %s", line)
		}
	}
	var saved savedState
	_, _ = e.state.Load(ctx, StateKey("ctx"), &saved)
	if saved.Desired != 3 || saved.Min != 2 {
		t.Errorf("the original size was overwritten with zeros: %+v", saved)
	}
}

func TestEKSFreshReadingWinsOverStaleSavedState(t *testing.T) {
	ctx := context.Background()
	f := &fakeEKS{status: "ACTIVE", minSize: 1, maxSize: 5, d: 4}
	e := eksSetup(f)
	_ = e.state.Save(ctx, StateKey("ctx"), savedState{Provider: KindEKSNodegroup, Target: "prod/workers/us-east-1/", Min: 7, Max: 7, Desired: 7})
	step := e.steps(t, plan(KindEKSNodegroup, eksParams), dr.DirDown)[0]
	if err := step.Action.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	var saved savedState
	_, _ = e.state.Load(ctx, StateKey("ctx"), &saved)
	if saved.Desired != 4 || saved.Max != 5 {
		t.Errorf("saved = %+v; the running size should replace the stale one", saved)
	}
}

func TestEKSMaximumNeverDropsBelowOne(t *testing.T) {
	f := &fakeEKS{status: "ACTIVE", minSize: 0, maxSize: 0, d: 1}
	e := eksSetup(f)
	if err := e.steps(t, plan(KindEKSNodegroup, eksParams), dr.DirDown)[0].Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.maxSize != 1 {
		t.Errorf("maxSize = %d; EKS rejects a maximum below 1", f.maxSize)
	}
}

func TestEKSRefusesCorruptOrForeignState(t *testing.T) {
	ctx := context.Background()
	f := &fakeEKS{status: "ACTIVE", d: 0, maxSize: 1}
	e := eksSetup(f)
	up := e.steps(t, plan(KindEKSNodegroup, eksParams), dr.DirUp)[0]

	for name, saved := range map[string]savedState{
		"desired above max": {Provider: KindEKSNodegroup, Target: "prod/workers/us-east-1/", Min: 1, Max: 2, Desired: 5},
		"zero max":          {Provider: KindEKSNodegroup, Target: "prod/workers/us-east-1/", Max: 0, Desired: 0},
		"negative min":      {Provider: KindEKSNodegroup, Target: "prod/workers/us-east-1/", Min: -1, Max: 2, Desired: 1},
	} {
		_ = e.state.Save(ctx, StateKey("ctx"), saved)
		if err := up.Action.Apply(ctx); !errors.Is(err, dr.ErrPreflight) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	// State saved for another node group or another provider must never be applied here.
	for name, saved := range map[string]savedState{
		"other nodegroup": {Provider: KindEKSNodegroup, Target: "prod/other/us-east-1/", Min: 1, Max: 2, Desired: 2},
		"other provider":  {Provider: KindGKENodepool, Target: "prod/workers/us-east-1/", Nodes: 2},
	} {
		_ = e.state.Save(ctx, StateKey("ctx"), saved)
		if err := up.Action.Apply(ctx); err == nil || !strings.Contains(err.Error(), "belongs to") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for _, line := range e.exec.lines() {
		if strings.Contains(line, "update-nodegroup-config") {
			t.Errorf("an update ran with bad state: %s", line)
		}
	}
}

func TestEKSProfileAndRegionAreAlwaysPassed(t *testing.T) {
	f := &fakeEKS{status: "ACTIVE", minSize: 1, maxSize: 2, d: 1}
	e := eksSetup(f)
	params := map[string]string{"cluster": "prod", "nodegroup": "workers", "region": "eu-west-1", "profile": "dr-account"}
	step := e.steps(t, plan(KindEKSNodegroup, params), dr.DirDown)[0]
	if got := step.Action.Equivalent(); !strings.HasPrefix(got, "aws eks update-nodegroup-config --cluster-name prod --nodegroup-name workers --scaling-config 'minSize=0,maxSize=<current max>,desiredSize=0' --region eu-west-1 --profile dr-account && aws eks wait nodegroup-active") {
		t.Errorf("equivalent = %s", got)
	}
	if err := step.Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, line := range e.exec.lines() {
		if !strings.Contains(line, "--region eu-west-1") || !strings.Contains(line, "--profile dr-account") {
			t.Errorf("command without region or profile: %s", line)
		}
	}
}

func TestEKSDoneAndApplyErrors(t *testing.T) {
	ctx := context.Background()
	f := &fakeEKS{status: "ACTIVE", maxSize: 2, d: 1, minSize: 1}
	e := eksSetup(f)
	step := e.steps(t, plan(KindEKSNodegroup, eksParams), dr.DirDown)[0]

	f.describeErr = errors.New("ExpiredToken: the security token has expired")
	if _, err := step.Action.Done(ctx); err == nil || !strings.Contains(err.Error(), "ExpiredToken") {
		t.Errorf("Done error = %v", err)
	}
	if err := step.Action.Apply(ctx); err == nil {
		t.Error("Apply must fail when the node group cannot be read")
	}
	if e.state.has(StateKey("ctx")) {
		t.Error("nothing should be saved when nothing could be read")
	}

	f.describeErr = nil
	e.exec.handler = func(name string, args []string) (string, error) {
		if len(args) > 1 && args[1] == "describe-nodegroup" {
			return "not json", nil
		}
		return "", nil
	}
	if _, err := step.Action.Done(ctx); err == nil || !strings.Contains(err.Error(), "unexpected answer") {
		t.Errorf("garbage answer: %v", err)
	}
}

func TestEKSNeedsAStateStore(t *testing.T) {
	e := newTestEnv()
	e.State = nil
	if _, err := NewPlanner(Options{}).Steps(context.Background(), e.Env, plan(KindEKSNodegroup, eksParams), dr.DirDown, dr.Scope{}); err == nil || !strings.Contains(err.Error(), "state store") {
		t.Errorf("err = %v", err)
	}
}

func TestStateIsKeyedByContext(t *testing.T) {
	if StateKey("prod-eu") != "prod-eu/provider" {
		t.Errorf("StateKey = %s", StateKey("prod-eu"))
	}
	// Without an Env.Context the plan's context is used.
	e := newTestEnv()
	e.Context = ""
	f := &fakeEKS{status: "ACTIVE", minSize: 1, maxSize: 2, d: 1}
	e.exec.handler = f.handle
	p := plan(KindEKSNodegroup, eksParams)
	p.Context = "from-plan"
	if err := e.steps(t, p, dr.DirDown)[0].Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.state.has("from-plan/provider") {
		t.Error("state was not keyed by the plan's context")
	}
}

// ---- GKE -----------------------------------------------------------------------------------------------

var gkeParams = map[string]string{"cluster": "main", "nodepool": "pool-1", "location": "us-central1", "project": "my-proj"}

type gkeDescribe struct {
	initial     int
	locations   []string
	autoscaling bool
	err         error
}

func (g *gkeDescribe) handle(name string, args []string) (string, error) {
	if g.err != nil {
		return "", g.err
	}
	if name == "gcloud" && len(args) > 2 && args[2] == "describe" {
		doc := map[string]any{"initialNodeCount": g.initial, "locations": g.locations, "autoscaling": map[string]any{"enabled": g.autoscaling}}
		raw, _ := json.Marshal(doc)
		return string(raw), nil
	}
	return "", nil
}

func gkeNodes(pool string, n int) []runtime.Object {
	var out []runtime.Object
	for i := 0; i < n; i++ {
		out = append(out, node(fmt.Sprintf("%s-node-%d", pool, i), pool))
	}
	return out
}

func resizeCommands(e *testEnv) []string {
	var out []string
	for _, line := range e.exec.lines() {
		if strings.Contains(line, "clusters resize") {
			out = append(out, line)
		}
	}
	return out
}

func TestGKEScaleDownSavesNodesPerZoneThenResizesToZero(t *testing.T) {
	ctx := context.Background()
	// Six nodes spread over three zones: two per zone.
	e := newTestEnv(append(gkeNodes("pool-1", 6), node("other-pool-node", "other"))...)
	e.exec.handler = (&gkeDescribe{initial: 1, locations: []string{"a", "b", "c"}}).handle
	step := e.steps(t, plan(KindGKENodepool, gkeParams), dr.DirDown)[0]

	if done, _ := step.Action.Done(ctx); done {
		t.Fatal("a pool with nodes is not done")
	}
	if err := step.Action.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	var saved savedState
	if found, _ := e.state.Load(ctx, StateKey("ctx"), &saved); !found || saved.Nodes != 2 || saved.Provider != KindGKENodepool {
		t.Errorf("saved = %+v (found %v); want 2 nodes per zone", saved, found)
	}
	want := "gcloud container clusters resize main --node-pool pool-1 --num-nodes 0 --location us-central1 --project my-proj --quiet"
	if got := resizeCommands(e); len(got) != 1 || got[0] != want {
		t.Errorf("resize commands = %v\nwant %s", got, want)
	}
	if step.Action.Equivalent() != want {
		t.Errorf("equivalent = %s", step.Action.Equivalent())
	}
}

func TestGKEScaleDownRefusesWhileTheAutoscalerIsOn(t *testing.T) {
	e := newTestEnv(gkeNodes("pool-1", 2)...)
	e.exec.handler = (&gkeDescribe{initial: 2, autoscaling: true}).handle
	err := e.steps(t, plan(KindGKENodepool, gkeParams), dr.DirDown)[0].Action.Apply(context.Background())
	if !errors.Is(err, dr.ErrPreflight) || !strings.Contains(err.Error(), "--no-enable-autoscaling") {
		t.Fatalf("err = %v; want a refusal that says how to turn it off", err)
	}
	if len(resizeCommands(e)) != 0 || e.state.has(StateKey("ctx")) {
		t.Error("nothing may change when the autoscaler is on")
	}
}

func TestGKEWithoutAPIAccessFallsBackToTheReportedSizeButNeverOverwritesSavedState(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv()
	e.setReachable(false)
	e.exec.handler = (&gkeDescribe{initial: 3}).handle
	step := e.steps(t, plan(KindGKENodepool, gkeParams), dr.DirDown)[0]

	if done, err := step.Action.Done(ctx); done || err != nil {
		t.Errorf("Done = %v, %v; without the API nothing can be proved", done, err)
	}
	if err := step.Action.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	var saved savedState
	_, _ = e.state.Load(ctx, StateKey("ctx"), &saved)
	if saved.Nodes != 3 {
		t.Errorf("saved = %+v; want the size GKE reports", saved)
	}

	// A resumed run must keep the size saved the first time.
	_ = e.state.Save(ctx, StateKey("ctx"), savedState{Provider: KindGKENodepool, Target: "main/pool-1/us-central1/my-proj", Nodes: 5})
	e.exec.handler = (&gkeDescribe{initial: 1}).handle
	if err := step.Action.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = e.state.Load(ctx, StateKey("ctx"), &saved)
	if saved.Nodes != 5 {
		t.Errorf("saved = %+v; the earlier size must be kept", saved)
	}
}

func TestGKERefusesToResizeWhenItCannotRestoreLater(t *testing.T) {
	e := newTestEnv()
	e.setReachable(false)
	e.exec.handler = (&gkeDescribe{initial: 0}).handle
	err := e.steps(t, plan(KindGKENodepool, gkeParams), dr.DirDown)[0].Action.Apply(context.Background())
	if !errors.Is(err, dr.ErrPreflight) || len(resizeCommands(e)) != 0 {
		t.Errorf("err = %v, resizes = %v", err, resizeCommands(e))
	}
}

func TestGKEAlreadyEmptyPoolKeepsEarlierState(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv() // reachable, no nodes
	e.exec.handler = (&gkeDescribe{initial: 2}).handle
	_ = e.state.Save(ctx, StateKey("ctx"), savedState{Provider: KindGKENodepool, Target: "main/pool-1/us-central1/my-proj", Nodes: 4})
	step := e.steps(t, plan(KindGKENodepool, gkeParams), dr.DirDown)[0]
	if done, _ := step.Action.Done(ctx); !done {
		t.Error("an empty pool is done")
	}
	if err := step.Action.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	var saved savedState
	_, _ = e.state.Load(ctx, StateKey("ctx"), &saved)
	if saved.Nodes != 4 {
		t.Errorf("saved = %+v; an empty pool must not overwrite the real size", saved)
	}
}

func TestGKEScaleUpRestoresAndForgets(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv()
	e.exec.handler = (&gkeDescribe{initial: 1}).handle
	_ = e.state.Save(ctx, StateKey("ctx"), savedState{Provider: KindGKENodepool, Target: "main/pool-1/us-central1/my-proj", Nodes: 3})
	up := e.steps(t, plan(KindGKENodepool, gkeParams), dr.DirUp)[0]

	if got := up.Action.Equivalent(); !strings.Contains(got, "--num-nodes 3") {
		t.Errorf("equivalent = %s", got)
	}
	if done, _ := up.Action.Done(ctx); done {
		t.Error("up is not done while the pool is empty")
	}
	if err := up.Action.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if got := resizeCommands(e); len(got) != 1 || !strings.Contains(got[0], "--num-nodes 3 ") {
		t.Errorf("resize = %v", got)
	}
	if e.state.has(StateKey("ctx")) {
		t.Error("state must be removed after restoring")
	}

	// Nodes are back: done.
	e2 := newTestEnv(gkeNodes("pool-1", 3)...)
	e2.exec.handler = (&gkeDescribe{}).handle
	if done, _ := e2.steps(t, plan(KindGKENodepool, gkeParams), dr.DirUp)[0].Action.Done(ctx); !done {
		t.Error("a pool with nodes is up")
	}
}

func TestGKEScaleUpRefusesWithoutUsableState(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv()
	e.exec.handler = (&gkeDescribe{}).handle
	up := e.steps(t, plan(KindGKENodepool, gkeParams), dr.DirUp)[0]

	if err := up.Action.Apply(ctx); !errors.Is(err, dr.ErrPreflight) || !errors.Is(err, errNoState) {
		t.Errorf("no state: %v", err)
	}
	_ = e.state.Save(ctx, StateKey("ctx"), savedState{Provider: KindGKENodepool, Target: "main/pool-1/us-central1/my-proj", Nodes: 0})
	if err := up.Action.Apply(ctx); !errors.Is(err, dr.ErrPreflight) {
		t.Errorf("zero nodes saved: %v", err)
	}
	_ = e.state.Save(ctx, StateKey("ctx"), savedState{Provider: KindGKENodepool, Target: "main/other/us-central1/my-proj", Nodes: 2})
	if err := up.Action.Apply(ctx); err == nil || !strings.Contains(err.Error(), "belongs to") {
		t.Errorf("foreign state: %v", err)
	}
	if len(resizeCommands(e)) != 0 {
		t.Errorf("resized with bad state: %v", resizeCommands(e))
	}
}

func TestGKEDescribeFailures(t *testing.T) {
	e := newTestEnv(gkeNodes("pool-1", 1)...)
	g := &gkeDescribe{err: errors.New("PERMISSION_DENIED: container.nodePools.get")}
	e.exec.handler = g.handle
	if err := e.steps(t, plan(KindGKENodepool, gkeParams), dr.DirDown)[0].Action.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "PERMISSION_DENIED") {
		t.Errorf("err = %v", err)
	}
	e.exec.handler = func(string, []string) (string, error) { return "<html>", nil }
	if err := e.steps(t, plan(KindGKENodepool, gkeParams), dr.DirDown)[0].Action.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected answer") {
		t.Errorf("garbage = %v", err)
	}
}
