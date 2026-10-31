package chaos

import (
	"context"
	"fmt"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

// Pause waits, so a person or a probe can watch the system for a while. It changes nothing.
type Pause struct {
	env  *dr.Env
	d    time.Duration
	done bool
}

// NewPause returns an action that waits d (1 second to MaxPause).
func NewPause(env *dr.Env, d time.Duration) (*Pause, error) {
	if d < time.Second || d > MaxPause {
		return nil, fmt.Errorf("%w: a pause must be between 1s and %s", dr.ErrPreflight, MaxPause)
	}
	return &Pause{env: env, d: d}, nil
}

// ID implements dr.Action.
func (p *Pause) ID() string { return "chaos/pause:" + p.d.String() }

// Title implements dr.Action.
func (p *Pause) Title() string { return "Wait " + human(p.d) }

// Describe implements dr.Action.
func (p *Pause) Describe() string {
	return "Waits " + human(p.d) + " so the effect of the previous step can be observed."
}

// Risk implements dr.Action.
func (p *Pause) Risk() dr.Risk { return dr.RiskLow }

// Equivalent implements dr.Action.
func (p *Pause) Equivalent() string { return fmt.Sprintf("sleep %d", int(p.d.Seconds())) }

// Done implements dr.Action.
func (p *Pause) Done(context.Context) (bool, error) { return p.done, nil }

// Apply implements dr.Action.
func (p *Pause) Apply(ctx context.Context) error {
	if err := sleep(ctx, p.env, p.d); err != nil {
		return err
	}
	p.done = true
	return nil
}

var _ dr.Action = (*Pause)(nil)
