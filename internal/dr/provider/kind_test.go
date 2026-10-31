package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/1985epma/k8scli/internal/dr"
)

const labNodes = "k8scli-lab-worker2\nk8scli-lab-control-plane\nk8scli-lab-worker\nk8scli-lab-external-load-balancer\n"

func kindEnv(listing string) *testEnv {
	e := newTestEnv()
	e.exec.handler = func(name string, args []string) (string, error) {
		if name == "docker" && args[0] == "ps" {
			return listing, nil
		}
		return "", nil
	}
	return e
}

func TestKindStopsWorkersThenControlPlaneThenBalancer(t *testing.T) {
	e := kindEnv(labNodes)
	steps := e.steps(t, plan(KindKind, map[string]string{"name": "k8scli-lab"}), dr.DirDown)

	want := []string{
		"power/kind/k8scli-lab/k8scli-lab-worker:stop",
		"power/kind/k8scli-lab/k8scli-lab-worker2:stop",
		"power/kind/k8scli-lab/k8scli-lab-control-plane:stop",
		"power/kind/k8scli-lab/k8scli-lab-external-load-balancer:stop",
	}
	if got := ids(steps); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("down order = %v\nwant %v", got, want)
	}
	if eq := equivalents(steps); eq[0] != "docker stop k8scli-lab-worker" || eq[2] != "docker stop k8scli-lab-control-plane" {
		t.Errorf("equivalents = %v", eq)
	}
	if got := e.exec.lines(); len(got) != 1 || got[0] != "docker ps -a --filter label=io.x-k8s.kind.cluster=k8scli-lab --format '{{.Names}}'" {
		t.Errorf("discovery command = %v", got)
	}
}

func TestKindStartsBalancerAndControlPlaneBeforeWorkers(t *testing.T) {
	e := kindEnv(labNodes)
	steps := e.steps(t, plan(KindKind, map[string]string{"name": "k8scli-lab"}), dr.DirUp)

	want := []string{
		"power/kind/k8scli-lab/k8scli-lab-external-load-balancer:start",
		"power/kind/k8scli-lab/k8scli-lab-control-plane:start",
		"power/kind/k8scli-lab/k8scli-lab-worker2:start",
		"power/kind/k8scli-lab/k8scli-lab-worker:start",
	}
	if got := ids(steps); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("up order = %v\nwant %v", got, want)
	}
	if steps[0].Gate != nil || steps[3].Gate == nil {
		t.Error("only the last started node should wait for the API")
	}
	if eq := equivalents(steps); eq[1] != "docker start k8scli-lab-control-plane" {
		t.Errorf("equivalents = %v", eq)
	}
}

func TestKindHighlyAvailableControlPlanes(t *testing.T) {
	e := kindEnv("c-control-plane\nc-control-plane2\nc-control-plane3\nc-worker\n")
	down := ids(e.steps(t, plan(KindKind, map[string]string{"name": "c"}), dr.DirDown))
	if !strings.HasSuffix(down[0], "c-worker:stop") || !strings.HasSuffix(down[3], "c-control-plane3:stop") {
		t.Errorf("down = %v", down)
	}
}

func TestKindNameComesFromTheContextWhenNotGiven(t *testing.T) {
	e := kindEnv("dev-control-plane\n")
	p := plan(KindKind, nil)
	p.Context = "kind-dev"
	e.steps(t, p, dr.DirDown)
	if got := e.exec.lines()[0]; !strings.Contains(got, "label=io.x-k8s.kind.cluster=dev ") {
		t.Errorf("command = %s", got)
	}

	for _, ctxName := range []string{"docker-desktop", "kind-", ""} {
		p := plan(KindKind, nil)
		p.Context = ctxName
		if _, err := NewPlanner(Options{}).Steps(context.Background(), e.Env, p, dr.DirDown, dr.Scope{}); err == nil || !strings.Contains(err.Error(), `"name"`) {
			t.Errorf("context %q: err = %v; want a request for the name parameter", ctxName, err)
		}
	}

	// An explicit name wins over the context.
	e2 := kindEnv("other-control-plane\n")
	p = plan(KindKind, map[string]string{"name": "other"})
	p.Context = "kind-dev"
	e2.steps(t, p, dr.DirDown)
	if got := e2.exec.lines()[0]; !strings.Contains(got, "=other ") {
		t.Errorf("command = %s", got)
	}
}

func TestKindWithoutNodesOrWithOddDockerOutput(t *testing.T) {
	pl := NewPlanner(Options{})
	for name, listing := range map[string]string{"empty": "", "blank lines": "\n\n  \n"} {
		e := kindEnv(listing)
		if _, err := pl.Steps(context.Background(), e.Env, plan(KindKind, map[string]string{"name": "lab"}), dr.DirDown, dr.Scope{}); err == nil || !strings.Contains(err.Error(), `"lab"`) {
			t.Errorf("%s: err = %v; want a message naming the cluster", name, err)
		}
	}
	// Whatever docker prints becomes an argument later, so it is validated too.
	for _, bad := range []string{"lab-worker; rm -rf /\n", "--privileged\n", "lab worker\n"} {
		e := kindEnv(bad)
		if _, err := pl.Steps(context.Background(), e.Env, plan(KindKind, map[string]string{"name": "lab"}), dr.DirDown, dr.Scope{}); err == nil {
			t.Errorf("container name %q was accepted", bad)
		}
	}
	e := newTestEnv()
	e.exec.handler = func(string, []string) (string, error) { return "", errors.New("Cannot connect to the Docker daemon") }
	if _, err := pl.Steps(context.Background(), e.Env, plan(KindKind, map[string]string{"name": "lab"}), dr.DirDown, dr.Scope{}); err == nil || !strings.Contains(err.Error(), "Docker daemon") {
		t.Errorf("a docker failure must surface: %v", err)
	}
}

func TestKindDoneReadsTheContainerState(t *testing.T) {
	for _, tc := range []struct {
		dir     dr.Direction
		running string
		done    bool
	}{
		{dr.DirDown, "false\n", true}, {dr.DirDown, "true\n", false},
		{dr.DirUp, "true\n", true}, {dr.DirUp, "false\n", false},
	} {
		e := newTestEnv()
		e.exec.handler = func(name string, args []string) (string, error) {
			if args[0] == "ps" {
				return "lab-control-plane\n", nil
			}
			return tc.running, nil
		}
		step := e.steps(t, plan(KindKind, map[string]string{"name": "lab"}), tc.dir)[0]
		done, err := step.Action.Done(context.Background())
		if err != nil || done != tc.done {
			t.Errorf("%s with running=%q: done=%v err=%v; want %v", tc.dir, strings.TrimSpace(tc.running), done, err, tc.done)
		}
		if last := e.exec.lines()[len(e.exec.lines())-1]; last != "docker inspect -f '{{.State.Running}}' lab-control-plane" {
			t.Errorf("inspect command = %s", last)
		}
	}
}

func TestKindDoneErrors(t *testing.T) {
	e := newTestEnv()
	answer, failure := "maybe", error(nil)
	e.exec.handler = func(name string, args []string) (string, error) {
		if args[0] == "ps" {
			return "lab-control-plane\n", nil
		}
		return answer, failure
	}
	step := e.steps(t, plan(KindKind, map[string]string{"name": "lab"}), dr.DirDown)[0]
	if _, err := step.Action.Done(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected answer") {
		t.Errorf("odd answer: %v", err)
	}
	failure = errors.New("No such object")
	if _, err := step.Action.Done(context.Background()); err == nil || !strings.Contains(err.Error(), "No such object") {
		t.Errorf("inspect failure: %v", err)
	}
}

func TestKindApplyRunsDockerAndReportsFailures(t *testing.T) {
	e := kindEnv("lab-control-plane\nlab-worker\n")
	steps := e.steps(t, plan(KindKind, map[string]string{"name": "lab"}), dr.DirDown)
	before := e.exec.count()
	for _, s := range steps {
		if err := s.Action.Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.exec.lines()[before:]; strings.Join(got, "|") != "docker stop lab-worker|docker stop lab-control-plane" {
		t.Errorf("applied commands = %v", got)
	}

	e.exec.handler = func(string, []string) (string, error) { return "", errors.New("permission denied") }
	if err := steps[0].Action.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("Apply error = %v", err)
	}
}

func TestKindRoleClassification(t *testing.T) {
	for name, want := range map[string]int{
		"c-worker": roleWorker, "c-worker12": roleWorker, "c-control-plane": roleControlPlane, "c-control-plane2": roleControlPlane,
		"c-external-load-balancer": roleLoadBalancer, "weird": roleWorker, "c-": roleWorker,
	} {
		if got := kindRole("c", name); got != want {
			t.Errorf("kindRole(%q) = %d; want %d", name, got, want)
		}
	}
}
