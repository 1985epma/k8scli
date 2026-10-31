package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

func TestValidIdent(t *testing.T) {
	good := []string{"a", "prod-eu", "my.cluster_1", "us-east-1", "0abc", "k8scli-lab-control-plane", strings.Repeat("a", 128)}
	for _, v := range good {
		if _, err := validIdent("p", v); err != nil {
			t.Errorf("%q should be valid: %v", v, err)
		}
	}
	bad := []string{
		"", "-x", "--profile=evil", "-", "x; rm -rf /", "a b", " a", "a ", "a\nb", "a\tb", "a\r", "a\x00", "é", "café",
		"аpp",  // Cyrillic a
		"ｋind", // full-width k
		"a/b", `a\b`, "a=b", "$(id)", "`id`", "a|b", "a&b", "a>b", "a'b", `a"b`, "a*b", "a?b", "a:b", "a,b", "a@b",
		".hidden", "_x", strings.Repeat("a", 129),
	}
	for _, v := range bad {
		if _, err := validIdent("p", v); err == nil {
			t.Errorf("%q must be rejected", v)
		}
	}
}

func TestPlannerHasNothingToDoWithoutAProviderOrOutsideItsPhase(t *testing.T) {
	e := newTestEnv()
	pl := NewPlanner(Options{})
	ctx := context.Background()

	if pl.Phase() != dr.PhasePower {
		t.Errorf("Phase = %s", pl.Phase())
	}
	if steps, err := pl.Steps(ctx, e.Env, &dr.Plan{}, dr.DirDown, dr.Scope{}); steps != nil || err != nil {
		t.Errorf("no provider: %v, %v", steps, err)
	}
	if steps, err := pl.Steps(ctx, e.Env, nil, dr.DirDown, dr.Scope{}); steps != nil || err != nil {
		t.Errorf("nil plan: %v, %v", steps, err)
	}
	scoped := dr.Scope{Phases: []dr.Phase{dr.PhaseApps}}
	if steps, err := pl.Steps(ctx, e.Env, plan(KindMinikube, nil), dr.DirDown, scoped); steps != nil || err != nil {
		t.Errorf("scoped away: %v, %v", steps, err)
	}
	withPower := dr.Scope{Phases: []dr.Phase{dr.PhaseApps, dr.PhasePower}}
	if steps, err := pl.Steps(ctx, e.Env, plan(KindMinikube, nil), dr.DirDown, withPower); err != nil || len(steps) != 1 {
		t.Errorf("scoped in: %v, %v", steps, err)
	}
	if e.exec.count() != 0 {
		t.Errorf("planning ran commands: %v", e.exec.lines())
	}
}

func TestPlannerRefusesBadInput(t *testing.T) {
	e := newTestEnv()
	pl := NewPlanner(Options{})
	ctx := context.Background()

	if _, err := pl.Steps(ctx, e.Env, plan("vmware", nil), dr.DirDown, dr.Scope{}); err == nil || !strings.Contains(err.Error(), "unknown provider kind") {
		t.Errorf("unknown kind: %v", err)
	}
	if _, err := pl.Steps(ctx, e.Env, plan(KindMinikube, nil), "sideways", dr.Scope{}); err == nil {
		t.Error("an unknown direction must be refused")
	}
	noExec := &dr.Env{}
	if _, err := pl.Steps(ctx, noExec, plan(KindMinikube, nil), dr.DirDown, dr.Scope{}); err == nil || !strings.Contains(err.Error(), "executor") {
		t.Errorf("missing executor: %v", err)
	}
}

func TestKindsListsEveryProvider(t *testing.T) {
	got := strings.Join(Kinds(), ",")
	if got != "aks,docker-desktop,eks-nodegroup,gke-nodepool,kind,manual,minikube" {
		t.Errorf("Kinds = %s", got)
	}
}

// A realistic parameter set for every kind, with the answers the fake CLIs need.
func providerCases() map[string]struct {
	params  map[string]string
	handler func(name string, args []string) (string, error)
} {
	return map[string]struct {
		params  map[string]string
		handler func(name string, args []string) (string, error)
	}{
		KindKind: {map[string]string{"name": "lab"}, func(string, []string) (string, error) {
			return "lab-control-plane\nlab-worker\n", nil
		}},
		KindMinikube:      {map[string]string{"profile": "dev"}, nil},
		KindDockerDesktop: {nil, nil},
		KindManual:        {nil, nil},
		KindAKS:           {map[string]string{"resourceGroup": "rg", "name": "aks1"}, nil},
		KindEKSNodegroup:  {map[string]string{"cluster": "c", "nodegroup": "ng", "region": "us-east-1"}, nil},
		KindGKENodepool:   {map[string]string{"cluster": "c", "nodepool": "np", "location": "us-central1", "project": "proj"}, nil},
	}
}

func TestEveryProviderIsDestructiveGoingDownAndGatedGoingUp(t *testing.T) {
	for kind, tc := range providerCases() {
		t.Run(kind, func(t *testing.T) {
			e := newTestEnv()
			e.exec.handler = tc.handler

			down := e.steps(t, plan(kind, tc.params), dr.DirDown)
			up := e.steps(t, plan(kind, tc.params), dr.DirUp)
			if len(down) == 0 || len(up) == 0 {
				t.Fatalf("no steps: down=%d up=%d", len(down), len(up))
			}
			for _, s := range down {
				if s.Phase != dr.PhasePower {
					t.Errorf("down step %s in phase %s", s.Action.ID(), s.Phase)
				}
				if s.Action.Risk() != dr.RiskDestructive {
					t.Errorf("down step %s has risk %s; powering off is destructive", s.Action.ID(), s.Action.Risk())
				}
				if s.Gate != nil {
					t.Errorf("down step %s has a gate", s.Action.ID())
				}
				if s.Action.Title() == "" || s.Action.Describe() == "" {
					t.Errorf("down step %s has no title or description", s.Action.ID())
				}
			}
			for i, s := range up {
				if s.Action.Risk() != dr.RiskLow {
					t.Errorf("up step %s has risk %s", s.Action.ID(), s.Action.Risk())
				}
				if last := i == len(up)-1; last != (s.Gate != nil) {
					t.Errorf("up step %s: gate present = %v, last = %v; only the last step waits for the API", s.Action.ID(), s.Gate != nil, last)
				}
			}
			// Planning is read-only: the only command allowed is kind's container listing.
			for _, line := range e.exec.lines() {
				if !strings.HasPrefix(line, "docker ps -a") {
					t.Errorf("planning ran %q", line)
				}
			}
		})
	}
}

func TestPlannerAttachesTheAPIGateWithTheConfiguredTimeout(t *testing.T) {
	e := newTestEnv()
	for _, tc := range []struct {
		opt  time.Duration
		want time.Duration
	}{{0, DefaultAPITimeout}, {3 * time.Minute, 3 * time.Minute}} {
		steps, err := NewPlanner(Options{APITimeout: tc.opt}).Steps(context.Background(), e.Env, plan(KindMinikube, nil), dr.DirUp, dr.Scope{})
		if err != nil {
			t.Fatal(err)
		}
		gate, ok := steps[0].Gate.(*APIGate)
		if !ok || gate.timeout != tc.want || gate.Name() == "" {
			t.Errorf("gate = %#v; want an APIGate with timeout %s", steps[0].Gate, tc.want)
		}
	}
}

// Parameter values reach command lines, so every one of them is checked again before use.
func TestInjectionInParametersIsRefusedBeforeAnyCommandRuns(t *testing.T) {
	payloads := []string{
		"x; rm -rf /", "--profile=evil", "-rf", "a b", "a\nb", "$(id)", "`id`", "a|b", "ｘ", "аpp", "../../etc", "a=b", "a'b",
	}
	targets := map[string][]string{
		KindKind:         {"name"},
		KindMinikube:     {"profile"},
		KindAKS:          {"resourceGroup", "name", "subscription"},
		KindEKSNodegroup: {"cluster", "nodegroup", "region", "profile"},
		KindGKENodepool:  {"cluster", "nodepool", "location", "project"},
	}
	for kind, names := range targets {
		for _, param := range names {
			for _, payload := range payloads {
				e := newTestEnv()
				params := map[string]string{}
				for k, v := range providerCases()[kind].params {
					params[k] = v
				}
				params[param] = payload

				for _, dir := range []dr.Direction{dr.DirDown, dr.DirUp} {
					_, err := NewPlanner(Options{}).Steps(context.Background(), e.Env, plan(kind, params), dir, dr.Scope{})
					if err == nil {
						t.Errorf("%s %s=%q (%s) was accepted", kind, param, payload, dir)
					}
				}
				if e.exec.count() != 0 {
					t.Errorf("%s %s=%q: commands ran before validation: %v", kind, param, payload, e.exec.lines())
				}
			}
		}
	}
}

func TestMissingAndUnknownParameters(t *testing.T) {
	e := newTestEnv()
	pl := NewPlanner(Options{})
	ctx := context.Background()
	for kind, params := range map[string]map[string]string{
		KindAKS:          {"name": "x"},
		KindEKSNodegroup: {"cluster": "c", "region": "r"},
		KindGKENodepool:  {"cluster": "c"},
	} {
		if _, err := pl.Steps(ctx, e.Env, plan(kind, params), dr.DirDown, dr.Scope{}); err == nil || !strings.Contains(err.Error(), "required") {
			t.Errorf("%s with missing parameters: %v", kind, err)
		}
	}
	for kind, tc := range providerCases() {
		params := map[string]string{"surprise": "1"}
		for k, v := range tc.params {
			params[k] = v
		}
		if _, err := pl.Steps(ctx, e.Env, plan(kind, params), dr.DirDown, dr.Scope{}); err == nil || !strings.Contains(err.Error(), "unknown parameter") {
			t.Errorf("%s ignored a misspelled parameter: %v", kind, err)
		}
	}
}
