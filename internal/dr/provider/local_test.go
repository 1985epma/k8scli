package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/1985epma/k8scli/internal/dr"
)

// ---- minikube --------------------------------------------------------------------------------------------

func TestMinikubeCommands(t *testing.T) {
	e := newTestEnv()
	down := e.steps(t, plan(KindMinikube, map[string]string{"profile": "dev"}), dr.DirDown)
	up := e.steps(t, plan(KindMinikube, map[string]string{"profile": "dev"}), dr.DirUp)

	if got := equivalents(down)[0]; got != "minikube stop -p dev" {
		t.Errorf("down = %s", got)
	}
	if got := equivalents(up)[0]; got != "minikube start -p dev" {
		t.Errorf("up = %s", got)
	}
	if ids(down)[0] != "power/minikube/dev:stop" || ids(up)[0] != "power/minikube/dev:start" {
		t.Errorf("ids = %v %v", ids(down), ids(up))
	}
	if err := down[0].Action.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.exec.lines(); got[len(got)-1] != "minikube stop -p dev" {
		t.Errorf("Apply ran %v", got)
	}
}

func TestMinikubeProfileDefaults(t *testing.T) {
	e := newTestEnv()
	p := plan(KindMinikube, nil)
	p.Context = "work"
	if got := equivalents(e.steps(t, p, dr.DirDown))[0]; got != "minikube stop -p work" {
		t.Errorf("profile from context: %s", got)
	}
	p.Context = "not valid!"
	if got := equivalents(e.steps(t, p, dr.DirDown))[0]; got != "minikube stop -p minikube" {
		t.Errorf("profile fallback: %s", got)
	}
}

func TestMinikubeDone(t *testing.T) {
	for _, tc := range []struct {
		dir    dr.Direction
		out    string
		runErr error
		done   bool
		fail   string
	}{
		{dr.DirDown, "Stopped\n", errors.New("exit status 7"), true, ""}, // stopped hosts make minikube exit non-zero
		{dr.DirDown, "Running\n", nil, false, ""},
		{dr.DirUp, "Running\n", nil, true, ""},
		{dr.DirUp, "Stopped\n", errors.New("exit status 7"), false, ""},
		{dr.DirDown, "Nonexistent\n", errors.New("exit status 85"), false, "does not exist"},
		{dr.DirDown, "", errors.New("minikube: executable file not found"), false, "not found"},
		{dr.DirDown, "Paused\n", nil, false, "unexpected state"},
	} {
		e := newTestEnv()
		e.exec.handler = func(string, []string) (string, error) { return tc.out, tc.runErr }
		step := e.steps(t, plan(KindMinikube, map[string]string{"profile": "dev"}), tc.dir)[0]
		done, err := step.Action.Done(context.Background())
		if tc.fail != "" {
			if err == nil || !strings.Contains(err.Error(), tc.fail) {
				t.Errorf("%s %q: err = %v; want it to mention %q", tc.dir, tc.out, err, tc.fail)
			}
			continue
		}
		if err != nil || done != tc.done {
			t.Errorf("%s %q: done=%v err=%v; want %v", tc.dir, tc.out, done, err, tc.done)
		}
		if last := e.exec.lines()[e.exec.count()-1]; last != "minikube status -p dev --format '{{.Host}}'" {
			t.Errorf("status command = %s", last)
		}
	}
}

// ---- docker-desktop and manual -------------------------------------------------------------------------------

func TestDockerDesktopIsAManualStep(t *testing.T) {
	e := newTestEnv()
	down := e.steps(t, plan(KindDockerDesktop, nil), dr.DirDown)
	up := e.steps(t, plan(KindDockerDesktop, nil), dr.DirUp)
	if len(down) != 1 || len(up) != 1 {
		t.Fatalf("steps: %d %d", len(down), len(up))
	}
	if !strings.Contains(down[0].Action.Describe(), "Docker Desktop") || !strings.Contains(down[0].Action.Describe(), "off") {
		t.Errorf("down text = %s", down[0].Action.Describe())
	}
	if !strings.Contains(up[0].Action.Describe(), "turn on") {
		t.Errorf("up text = %s", up[0].Action.Describe())
	}
	if down[0].Action.Equivalent() != "" {
		t.Error("a manual step has no command")
	}
	if ids(down)[0] != "power/docker-desktop:off" || ids(up)[0] != "power/docker-desktop:on" {
		t.Errorf("ids = %v %v", ids(down), ids(up))
	}
	if e.exec.count() != 0 {
		t.Errorf("a manual provider must not run anything: %v", e.exec.lines())
	}

	if _, err := NewPlanner(Options{}).Steps(context.Background(), e.Env, plan(KindDockerDesktop, map[string]string{"x": "1"}), dr.DirDown, dr.Scope{}); err == nil {
		t.Error("docker-desktop takes no parameters")
	}
}

func TestManualApplyExplainsAndFailsWithTheSentinel(t *testing.T) {
	e := newTestEnv()
	for _, dir := range []dr.Direction{dr.DirDown, dr.DirUp} {
		step := e.steps(t, plan(KindManual, map[string]string{"instructions": "Ask the data center to cut the power"}), dir)[0]
		err := step.Action.Apply(context.Background())
		if !errors.Is(err, ErrManualStep) {
			t.Fatalf("%s: err = %v; want ErrManualStep", dir, err)
		}
		for _, want := range []string{"Ask the data center to cut the power", "k8scli dr resume <id>"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: message %q does not mention %q", dir, err, want)
			}
		}
	}
}

func TestManualDefaultTextAndLimits(t *testing.T) {
	e := newTestEnv()
	pl := NewPlanner(Options{})
	if got := e.steps(t, plan(KindManual, nil), dr.DirDown)[0].Action.Describe(); !strings.Contains(got, "workers first") {
		t.Errorf("default down text = %s", got)
	}
	if got := e.steps(t, plan(KindManual, nil), dr.DirUp)[0].Action.Describe(); !strings.Contains(got, "control plane first") {
		t.Errorf("default up text = %s", got)
	}
	for name, text := range map[string]string{"newline": "a\nb", "control": "a\x1b[31mb", "too long": strings.Repeat("x", maxInstructions+1)} {
		if _, err := pl.Steps(context.Background(), e.Env, plan(KindManual, map[string]string{"instructions": text}), dr.DirDown, dr.Scope{}); err == nil {
			t.Errorf("%s instructions were accepted", name)
		}
	}
	// The text is only shown, never run, so ordinary punctuation and non-ASCII letters are fine.
	if _, err := pl.Steps(context.Background(), e.Env, plan(KindManual, map[string]string{"instructions": "Desligue as máquinas; depois confirme (urgente)."}), dr.DirDown, dr.Scope{}); err != nil {
		t.Errorf("plain text refused: %v", err)
	}
	if _, err := pl.Steps(context.Background(), e.Env, plan(KindManual, map[string]string{"cmd": "x"}), dr.DirDown, dr.Scope{}); err == nil {
		t.Error("an unknown parameter must be refused")
	}
}

func TestManualDoneFollowsTheAPI(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv()
	down := e.steps(t, plan(KindManual, nil), dr.DirDown)[0].Action
	up := e.steps(t, plan(KindManual, nil), dr.DirUp)[0].Action

	// Cluster running: powering off is not done yet, powering on is.
	if done, _ := down.Done(ctx); done {
		t.Error("down must not be done while the API answers")
	}
	if done, _ := up.Done(ctx); !done {
		t.Error("up must be done while the API answers")
	}

	// The person powered it off.
	e.setReachable(false)
	if done, _ := down.Done(ctx); !done {
		t.Error("down must be done once the API is gone")
	}
	if done, _ := up.Done(ctx); done {
		t.Error("up must not be done while the API is gone")
	}

	// Without a client nothing can be proved: never "done" for down, so the instructions are shown.
	e.Client = nil
	if done, _ := down.Done(ctx); done {
		t.Error("down must not be considered done without a client")
	}
	if done, _ := up.Done(ctx); done {
		t.Error("up must not be considered done without a client")
	}
}
