package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

// savedState is what a power-off remembers so the matching power-on can restore the sizes. Node
// groups scaled to zero lose their previous size, so this document is the only copy.
type savedState struct {
	// Provider and Target say what the numbers belong to; a mismatch makes "up" refuse.
	Provider string `json:"provider"`
	Target   string `json:"target"`
	// EKS node group scaling.
	Min     int `json:"min,omitempty"`
	Max     int `json:"max,omitempty"`
	Desired int `json:"desired,omitempty"`
	// Nodes is the GKE node count per zone.
	Nodes int       `json:"nodes,omitempty"`
	Saved time.Time `json:"saved"`
}

// StateKey is where the provider keeps its saved sizes for a kubeconfig context.
func StateKey(contextName string) string { return contextName + "/provider" }

// errNoState explains a refused power-on.
var errNoState = errors.New("nothing was saved by a power-off for this target")

type stateRef struct {
	env    *dr.Env
	key    string
	kind   string
	target string
}

func newStateRef(env *dr.Env, plan *dr.Plan, kind, target string) (*stateRef, error) {
	if env.State == nil {
		return nil, fmt.Errorf("provider %s: a state store (env.State) is required to remember the size before powering off", kind)
	}
	name := env.Context
	if name == "" {
		name = plan.Context
	}
	return &stateRef{env: env, key: StateKey(name), kind: kind, target: target}, nil
}

// load returns the saved sizes when they exist for this very target.
func (s *stateRef) load(ctx context.Context) (savedState, bool, error) {
	var saved savedState
	found, err := s.env.State.Load(ctx, s.key, &saved)
	if err != nil {
		return savedState{}, false, fmt.Errorf("reading the saved state: %w", err)
	}
	if !found {
		return savedState{}, false, nil
	}
	if saved.Provider != s.kind || saved.Target != s.target {
		return savedState{}, false, fmt.Errorf("the saved state belongs to %s %q, not to %s %q; remove it before continuing", saved.Provider, saved.Target, s.kind, s.target)
	}
	return saved, true, nil
}

func (s *stateRef) save(ctx context.Context, saved savedState) error {
	saved.Provider, saved.Target, saved.Saved = s.kind, s.target, nowFn(s.env)()
	if err := s.env.State.Save(ctx, s.key, saved); err != nil {
		return fmt.Errorf("saving the size before powering off: %w", err)
	}
	return nil
}

func (s *stateRef) delete(ctx context.Context) error {
	if err := s.env.State.Delete(ctx, s.key); err != nil {
		return fmt.Errorf("removing the saved state: %w", err)
	}
	return nil
}
