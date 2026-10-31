// Package dbops stops and starts the databases of a DR plan: CloudNativePG clusters (through their
// declarative hibernation) and plain StatefulSets (scaled to zero and restored). It also estimates
// the RPO of each database from what the cluster exposes, never from database credentials.
//
// Databases stop AFTER the applications and start BEFORE them; the engine orders the phases. Among
// databases, the higher Tier stops first and the lower Tier starts first.
//
// Left out on purpose: switchover and replica promotion. CloudNativePG exposes them through
// kubectl-cnpg commands and by patching the Cluster status (currentPrimary/targetPrimary), and
// patching a status subresource is not a supported way to drive an operator. A promotion done
// wrong risks split brain, so it is not offered until the operator documents a declarative,
// spec-level way to ask for it.
package dbops

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

// Options configure a Planner.
type Options struct {
	// Backups is consulted for the newest backup when the database itself does not say. Optional.
	Backups BackupFinder
	// GateTimeout is how long a gate waits for a database to stop or become healthy. Default 15m.
	GateTimeout time.Duration
	// GatePoll is the interval between gate checks. Default 5s.
	GatePoll time.Duration
}

// Planner builds the PhaseDatabases steps. It implements dr.Planner.
type Planner struct {
	opts Options
}

// NewPlanner returns a Planner with defaults filled in.
func NewPlanner(opts Options) *Planner {
	if opts.GateTimeout <= 0 {
		opts.GateTimeout = defaultGateTimeout
	}
	if opts.GatePoll <= 0 {
		opts.GatePoll = defaultGatePoll
	}
	return &Planner{opts: opts}
}

var _ dr.Planner = (*Planner)(nil)

// Phase is always dr.PhaseDatabases.
func (p *Planner) Phase() dr.Phase { return dr.PhaseDatabases }

// Steps returns the steps for the databases of plan that match scope, in the order they must run.
func (p *Planner) Steps(ctx context.Context, env *dr.Env, plan *dr.Plan, dir dr.Direction, scope dr.Scope) ([]dr.Step, error) {
	if env == nil || plan == nil {
		return nil, fmt.Errorf("dbops: an environment and a plan are required")
	}
	if dir != dr.DirDown && dir != dr.DirUp {
		return nil, fmt.Errorf("dbops: unknown direction %q", dir)
	}
	if !phaseSelected(scope, dr.PhaseDatabases) {
		return nil, nil
	}

	databases, err := selectDatabases(plan, scope)
	if err != nil {
		return nil, err
	}
	orderByTier(databases, dir)

	var steps []dr.Step
	for _, db := range databases {
		next, err := p.stepsFor(ctx, env, plan, db, dir)
		if err != nil {
			return nil, err
		}
		steps = append(steps, next...)
	}
	return steps, nil
}

func (p *Planner) stepsFor(ctx context.Context, env *dr.Env, plan *dr.Plan, db dr.Database, dir dr.Direction) ([]dr.Step, error) {
	switch db.Kind {
	case KindCNPG:
		return (&cnpgTarget{p: p, env: env, plan: plan, db: db}).steps(ctx, dir)
	case KindStatefulSet:
		return (&statefulSetTarget{p: p, env: env, plan: plan, db: db}).steps(ctx, dir)
	default:
		return nil, fmt.Errorf("database %q has an unsupported kind %q (use %q or %q)", db.Name, db.Kind, KindCNPG, KindStatefulSet)
	}
}

func phaseSelected(scope dr.Scope, phase dr.Phase) bool {
	if len(scope.Phases) == 0 {
		return true
	}
	for _, p := range scope.Phases {
		if p == phase {
			return true
		}
	}
	return false
}

// selectDatabases validates every database of the plan and applies the scope filter.
func selectDatabases(plan *dr.Plan, scope dr.Scope) ([]dr.Database, error) {
	byName := map[string]bool{}
	for _, db := range plan.Databases {
		if db.Name == "" || db.Namespace == "" || db.Ref == "" {
			return nil, fmt.Errorf("database %q needs a name, a namespace and a ref", db.Name)
		}
		byName[db.Name] = true
	}
	for _, name := range scope.Databases {
		if !byName[name] {
			return nil, fmt.Errorf("the scope names database %q, which is not in the plan", name)
		}
	}

	wanted := map[string]bool{}
	for _, name := range scope.Databases {
		wanted[name] = true
	}
	var selected []dr.Database
	for _, db := range plan.Databases {
		if len(wanted) == 0 || wanted[db.Name] {
			selected = append(selected, db)
		}
	}
	return selected, nil
}

// orderByTier sorts stably: on "down" the highest tier goes first, on "up" the lowest.
func orderByTier(databases []dr.Database, dir dr.Direction) {
	sort.SliceStable(databases, func(i, j int) bool {
		if dir == dr.DirDown {
			return databases[i].Tier > databases[j].Tier
		}
		return databases[i].Tier < databases[j].Tier
	})
}
