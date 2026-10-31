package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

// Provider kinds understood by the planner.
const (
	KindKind          = "kind"
	KindMinikube      = "minikube"
	KindDockerDesktop = "docker-desktop"
	KindAKS           = "aks"
	KindEKSNodegroup  = "eks-nodegroup"
	KindGKENodepool   = "gke-nodepool"
	KindManual        = "manual"
)

// Kinds lists the supported provider kinds, sorted.
func Kinds() []string {
	kinds := []string{KindKind, KindMinikube, KindDockerDesktop, KindAKS, KindEKSNodegroup, KindGKENodepool, KindManual}
	sort.Strings(kinds)
	return kinds
}

// ErrManualStep marks a step that only a person can do. The engine can show it as "waiting for you"
// and the run continues with `k8scli dr resume <id>` once the person has acted.
var ErrManualStep = errors.New("manual step")

// Options tune the planner.
type Options struct {
	// APITimeout is how long to wait for the Kubernetes API after powering on; zero means 10 minutes.
	APITimeout time.Duration
}

// Planner is the dr.Planner of the power phase.
type Planner struct{ opts Options }

// NewPlanner returns the planner for dr.PhasePower.
func NewPlanner(opts Options) *Planner { return &Planner{opts: opts} }

// Phase implements dr.Planner.
func (p *Planner) Phase() dr.Phase { return dr.PhasePower }

// Steps implements dr.Planner. Without a provider in the plan, or when the run is scoped away from
// the power phase, there is nothing to do. Reading the cluster is never needed here: powering on
// happens before the Kubernetes API exists.
func (p *Planner) Steps(ctx context.Context, env *dr.Env, plan *dr.Plan, dir dr.Direction, scope dr.Scope) ([]dr.Step, error) {
	if plan == nil || plan.Provider == nil {
		return nil, nil
	}
	if len(scope.Phases) > 0 && !slices.Contains(scope.Phases, dr.PhasePower) {
		return nil, nil
	}
	if dir != dr.DirDown && dir != dr.DirUp {
		return nil, fmt.Errorf("unknown direction %q", dir)
	}
	if env == nil || env.Exec == nil {
		return nil, errors.New("provider: an executor (env.Exec) is required")
	}

	var steps []dr.Step
	var err error
	switch plan.Provider.Kind {
	case KindKind:
		steps, err = kindSteps(ctx, env, plan, dir)
	case KindMinikube:
		steps, err = minikubeSteps(env, plan, dir)
	case KindDockerDesktop:
		steps, err = dockerDesktopSteps(env, plan, dir)
	case KindManual:
		steps, err = manualSteps(env, plan, dir)
	case KindAKS:
		steps, err = aksSteps(env, plan, dir)
	case KindEKSNodegroup:
		steps, err = eksSteps(env, plan, dir)
	case KindGKENodepool:
		steps, err = gkeSteps(env, plan, dir)
	default:
		return nil, fmt.Errorf("unknown provider kind %q (supported: %v)", plan.Provider.Kind, Kinds())
	}
	if err != nil {
		return nil, err
	}

	// After powering on, nothing else may start until the API answers.
	if dir == dr.DirUp && len(steps) > 0 {
		steps[len(steps)-1].Gate = NewAPIGate(env, p.opts.APITimeout)
	}
	return steps, nil
}

func powerStep(a dr.Action) dr.Step { return dr.Step{Phase: dr.PhasePower, Action: a} }
