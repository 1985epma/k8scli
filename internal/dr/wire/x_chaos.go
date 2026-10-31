package wire

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/chaos"
	"github.com/1985epma/k8scli/internal/dr/checks"
	"github.com/1985epma/k8scli/internal/dr/extchecks"
)

// ChaosKind is the injection a ChaosRequest asks for.
type ChaosKind string

// Injections.
const (
	ChaosKillPods   ChaosKind = "kill-pods"
	ChaosDrainNode  ChaosKind = "drain-node"
	ChaosCordonNode ChaosKind = "cordon-node"
)

// ChaosRequest describes one chaos experiment.
type ChaosRequest struct {
	Plan *dr.Plan
	Kind ChaosKind

	// kill-pods
	Namespace    string
	WorkloadKind string // Deployment (default) or StatefulSet
	Workload     string
	Count        int
	MaxPercent   int

	// drain-node and cordon-node
	Node                 string
	DrainTimeout         time.Duration
	AllowOtherNamespaces bool

	// Watch restricts the observation to these apps of the plan; empty means every app in scope.
	Watch []string
	// Window, Poll and MaxDuration tune the observation (see chaos.Experiment); zero means the default.
	Window, Poll, MaxDuration time.Duration

	// DryRun validates and describes without injecting anything.
	DryRun bool
	// Confirm is the typed phrase (chaos.ConfirmPhrase) for production-looking contexts.
	Confirm string
	// OnEvent receives the timeline as it happens.
	OnEvent func(chaos.Event)
}

// ChaosPhrase is the phrase a real chaos run needs for the plan's context, or "" when none is needed.
func (s *Stack) ChaosPhrase(plan *dr.Plan) string {
	if plan != nil && dr.LooksLikeProduction(plan.Context) {
		return chaos.ConfirmPhrase(plan.Context)
	}
	return ""
}

// Chaos validates and runs one experiment: it injects, observes how the plan's apps react, recovers, and
// stores the result under the DR directory (a dry run stores nothing). The Stack's Actor and ReadOnly apply.
func (s *Stack) Chaos(ctx context.Context, req ChaosRequest) (*chaos.Result, error) {
	if req.Plan == nil {
		return nil, errors.New("a plan is required")
	}
	env, err := s.Env(ctx, req.Plan.Context)
	if err != nil {
		return nil, err
	}
	guard := chaos.Guard{Plan: req.Plan, Env: env, Confirm: req.Confirm, ReadOnly: s.ReadOnly()}

	var (
		injection dr.Action
		observe   string
	)
	switch req.Kind {
	case ChaosKillPods:
		injection, err = chaos.NewKillPods(ctx, guard, chaos.KillPodsOptions{
			Namespace: req.Namespace, Kind: req.WorkloadKind, Workload: req.Workload, Count: req.Count, MaxPercent: req.MaxPercent,
		})
		observe = req.Namespace
	case ChaosDrainNode:
		injection, err = chaos.NewDrainNode(ctx, guard, chaos.NodeOptions{Node: req.Node, DrainTimeout: req.DrainTimeout, AllowOtherNamespaces: req.AllowOtherNamespaces})
	case ChaosCordonNode:
		injection, err = chaos.NewCordonNode(ctx, guard, chaos.NodeOptions{Node: req.Node, DrainTimeout: req.DrainTimeout})
	default:
		return nil, fmt.Errorf("unknown chaos kind %q", req.Kind)
	}
	if err != nil {
		return nil, err
	}

	probes, err := chaos.ProbesFromPlan(ctx, env, req.Plan, observe, req.Watch)
	if err != nil {
		return nil, err
	}
	experiment := &chaos.Experiment{
		Name: string(req.Kind), Guard: guard, Injection: injection, Probes: probes,
		Window: req.Window, Poll: req.Poll, MaxDuration: req.MaxDuration, OnEvent: req.OnEvent,
	}
	if len(req.Watch) == 1 {
		if target, ok := req.Plan.TargetRTO(req.Watch[0]); ok {
			experiment.Target = target
		}
	}

	result, runErr := experiment.Run(ctx, req.DryRun)
	if result != nil && !req.DryRun {
		if _, err := chaos.Record(s.Dir(), s.Actor(), result); err != nil {
			result.Notes = append(result.Notes, "the result could not be saved: "+err.Error())
		}
	}
	return result, runErr
}

// ExtChecksOptions select the external-dependency checks.
type ExtChecksOptions struct {
	// Standby is the DR context; empty means the plan's drContext, and without one the references are
	// checked against the primary itself.
	Standby    string
	Namespaces []string
	ExpiryDays int
	// MinSeverity hides findings below it.
	MinSeverity string
}

// ExtChecks runs the read-only external-dependency checks for a plan.
func (s *Stack) ExtChecks(ctx context.Context, plan *dr.Plan, contextName string, opts ExtChecksOptions) (*extchecks.Report, error) {
	if contextName == "" && plan != nil {
		contextName = plan.Context
	}
	primary, err := s.Env(ctx, contextName)
	if err != nil {
		return nil, err
	}
	options := extchecks.Options{Plan: plan, Namespaces: opts.Namespaces, Primary: primary, ExpiryDays: opts.ExpiryDays}

	standby := opts.Standby
	if standby == "" && plan != nil {
		standby = plan.DRContext
	}
	if standby != "" {
		if options.Standby, err = s.Env(ctx, standby); err != nil {
			return nil, err
		}
	}
	if options.MinSeverity, err = checks.ParseSeverity(opts.MinSeverity); err != nil {
		return nil, err
	}
	return extchecks.Run(ctx, options)
}
