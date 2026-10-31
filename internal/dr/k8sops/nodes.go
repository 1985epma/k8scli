package k8sops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/1985epma/k8scli/internal/dr"
)

// NodesPlanner cordons and drains worker nodes before they are powered off, and uncordons them afterwards.
//
// It applies when the plan has a Provider (someone will power the nodes off) or when the run's scope asks
// for the nodes phase explicitly; a plan without a provider never drains nodes by surprise.
//
// Down: every worker is cordoned first, then drained one at a time, so evicted pods are not bounced from
// one node that is about to be drained to another. Control-plane nodes are never touched unless
// Options.IncludeControlPlane is set. Only the nodes this run cordoned are written to the DR state; a node
// that was already cordoned is drained but stays cordoned on the way up. Up: exactly the recorded nodes are
// uncordoned, then a gate waits for them to be Ready; a node that no longer exists is skipped.
type NodesPlanner struct {
	opts Options
	mem  *memStore
}

// NewNodesPlanner returns the nodes planner.
func NewNodesPlanner(opts Options) *NodesPlanner {
	return &NodesPlanner{opts: opts.withDefaults(), mem: newMemStore()}
}

// Phase implements dr.Planner.
func (p *NodesPlanner) Phase() dr.Phase { return dr.PhaseNodes }

func appliesToNodes(plan *dr.Plan, scope dr.Scope) bool {
	if plan.Provider != nil {
		return true
	}
	for _, phase := range scope.Phases {
		if phase == dr.PhaseNodes {
			return true
		}
	}
	return false
}

// Steps implements dr.Planner.
func (p *NodesPlanner) Steps(ctx context.Context, env *dr.Env, plan *dr.Plan, dir dr.Direction, scope dr.Scope) ([]dr.Step, error) {
	if env == nil {
		return nil, fmt.Errorf("the nodes planner needs an Env")
	}
	if !appliesToNodes(plan, scope) {
		return nil, nil
	}
	state := newLedger(env, p.mem, "nodes")
	c := clockOf(env)
	if dir == dr.DirUp {
		return p.up(ctx, env, state, c)
	}
	return p.down(ctx, env, plan, scope, state, c)
}

// reachesPower reports whether a down run goes on to power the machines off: the plan names a provider and
// the scope either lists the power phase or leaves the phases open.
func reachesPower(plan *dr.Plan, scope dr.Scope) bool {
	if plan.Provider == nil {
		return false
	}
	if len(scope.Phases) == 0 {
		return true
	}
	for _, phase := range scope.Phases {
		if phase == dr.PhasePower {
			return true
		}
	}
	return false
}

// drainScope returns the namespaces a drain is limited to, or nil for every namespace.
func (p *NodesPlanner) drainScope(plan *dr.Plan, scope dr.Scope) map[string]bool {
	var names []string
	switch {
	case p.opts.DrainNamespaces != nil:
		names = p.opts.DrainNamespaces
	case p.opts.DrainPlanNamespacesBeforePower && reachesPower(plan, scope):
		for _, app := range plan.Apps {
			names = append(names, app.Namespace)
		}
		for _, db := range plan.Databases {
			names = append(names, db.Namespace)
		}
	default:
		return nil
	}
	limit := make(map[string]bool, len(names)) // non-nil even when empty: "no namespace", not "all"
	for _, name := range names {
		limit[name] = true
	}
	return limit
}

func (p *NodesPlanner) down(ctx context.Context, env *dr.Env, plan *dr.Plan, scope dr.Scope, state ledger, c clock) ([]dr.Step, error) {
	if env.Client == nil {
		return nil, fmt.Errorf("%w: no Kubernetes client is configured", dr.ErrUnreachable)
	}
	list, err := env.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		if isUnreachable(err) {
			return nil, fmt.Errorf("%w: list nodes: %w", dr.ErrUnreachable, err)
		}
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	byName := map[string]*corev1.Node{}
	for i := range list.Items {
		byName[list.Items[i].Name] = &list.Items[i]
	}
	var names []string
	if len(p.opts.Nodes) > 0 {
		for _, name := range p.opts.Nodes {
			node, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("node %q was not found", name)
			}
			if isControlPlane(node) && !p.opts.IncludeControlPlane {
				return nil, fmt.Errorf("node %q is a control-plane node; set IncludeControlPlane to drain it", name)
			}
			names = append(names, name)
		}
	} else {
		for name, node := range byName {
			if isControlPlane(node) && !p.opts.IncludeControlPlane {
				continue
			}
			names = append(names, name)
		}
	}
	sort.Strings(names)

	limit := p.drainScope(plan, scope)
	var steps []dr.Step
	for _, name := range names {
		steps = append(steps, dr.Step{Phase: dr.PhaseNodes, Action: &cordonNode{
			nodeAction: nodeAction{client: env.Client, name: name, state: state, c: c},
			already:    byName[name].Spec.Unschedulable,
		}})
	}
	for _, name := range names {
		steps = append(steps, dr.Step{Phase: dr.PhaseNodes, Action: &drainNode{
			nodeAction: nodeAction{client: env.Client, name: name, state: state, c: c},
			opts:       p.opts,
			namespaces: limit,
		}})
	}
	return steps, nil
}

func (p *NodesPlanner) up(ctx context.Context, env *dr.Env, state ledger, c clock) ([]dr.Step, error) {
	names, _, err := readState[[]string](ctx, state)
	if err != nil {
		return nil, fmt.Errorf("read the DR state: %w", err)
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)

	var steps []dr.Step
	for _, name := range sorted {
		steps = append(steps, dr.Step{Phase: dr.PhaseNodes, Action: &uncordonNode{
			nodeAction: nodeAction{client: env.Client, name: name, state: state, c: c},
		}})
	}
	if len(steps) > 0 {
		steps[len(steps)-1].Gate = NewNodesReadyGate(env, sorted, p.opts.NodeReadyTimeout)
	}
	return steps, nil
}

func isControlPlane(node *corev1.Node) bool {
	for _, key := range []string{"node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master"} {
		if _, ok := node.Labels[key]; ok {
			return true
		}
		for _, taint := range node.Spec.Taints {
			if taint.Key == key {
				return true
			}
		}
	}
	return false
}

// nodeAction is what the node actions share.
type nodeAction struct {
	client kubernetes.Interface
	name   string
	state  ledger
	c      clock
}

func (a nodeAction) setUnschedulable(ctx context.Context, unschedulable bool) error {
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"unschedulable": unschedulable}})
	if err != nil {
		return err
	}
	return retryOnConflict(ctx, a.c.sleep, "update node "+a.name, func() error {
		_, err := a.client.CoreV1().Nodes().Patch(ctx, a.name, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	})
}

// ---- cordon ------------------------------------------------------------------------------------------------

// cordonNode marks a node unschedulable and records it, unless it already was.
type cordonNode struct {
	nodeAction
	// already is true when the node was unschedulable while planning.
	already bool
}

func (a *cordonNode) ID() string         { return "nodes/" + a.name + ":cordon" }
func (a *cordonNode) Title() string      { return "Cordon node " + a.name }
func (a *cordonNode) Risk() dr.Risk      { return dr.RiskHigh }
func (a *cordonNode) Equivalent() string { return "kubectl cordon " + a.name }

func (a *cordonNode) Describe() string {
	if a.already {
		return fmt.Sprintf("Node %s is already cordoned. It was not cordoned by this run, so \"up\" leaves it cordoned.", a.name)
	}
	return fmt.Sprintf("Marks node %s unschedulable so nothing new lands on it, and records it so \"up\" uncordons exactly this node.", a.name)
}

func (a *cordonNode) Done(ctx context.Context) (bool, error) {
	node, err := a.client.CoreV1().Nodes().Get(ctx, a.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return node.Spec.Unschedulable, nil
}

func (a *cordonNode) Apply(ctx context.Context) error {
	node, err := a.client.CoreV1().Nodes().Get(ctx, a.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		a.c.logf("node %s no longer exists; nothing to cordon", a.name)
		return nil
	}
	if err != nil {
		return err
	}
	if node.Spec.Unschedulable {
		return nil // cordoned by someone else, or by an earlier attempt that already recorded it
	}

	// Record first: a crash after the patch must still let "up" uncordon it.
	if err := modifyState(ctx, a.state, func(list *[]string) bool {
		if !contains(*list, a.name) {
			*list = append(*list, a.name)
		}
		return true
	}); err != nil {
		return fmt.Errorf("record node %s in the DR state: %w", a.name, err)
	}
	if err := a.setUnschedulable(ctx, true); err != nil {
		return fmt.Errorf("cordon node %s: %w", a.name, err)
	}
	a.c.logf("cordoned node %s", a.name)
	return nil
}

// ---- uncordon ----------------------------------------------------------------------------------------------

// uncordonNode makes a node schedulable again, only because "down" had cordoned it.
type uncordonNode struct{ nodeAction }

func (a *uncordonNode) ID() string         { return "nodes/" + a.name + ":uncordon" }
func (a *uncordonNode) Title() string      { return "Uncordon node " + a.name }
func (a *uncordonNode) Risk() dr.Risk      { return dr.RiskLow }
func (a *uncordonNode) Equivalent() string { return "kubectl uncordon " + a.name }

func (a *uncordonNode) Describe() string {
	return fmt.Sprintf("Makes node %s schedulable again (the matching \"down\" cordoned it) and clears it from the DR state.", a.name)
}

// Done is true once the node is no longer in the DR state.
func (a *uncordonNode) Done(ctx context.Context) (bool, error) {
	names, _, err := readState[[]string](ctx, a.state)
	if err != nil {
		return false, err
	}
	return !contains(names, a.name), nil
}

func (a *uncordonNode) Apply(ctx context.Context) error {
	node, err := a.client.CoreV1().Nodes().Get(ctx, a.name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		a.c.logf("node %s no longer exists (for example an autoscaler removed it); dropping it from the DR state", a.name)
	case err != nil:
		return err
	case node.Spec.Unschedulable:
		if err := a.setUnschedulable(ctx, false); err != nil {
			return fmt.Errorf("uncordon node %s: %w", a.name, err)
		}
		a.c.logf("uncordoned node %s", a.name)
	}
	return modifyState(ctx, a.state, func(list *[]string) bool {
		kept := (*list)[:0]
		for _, name := range *list {
			if name != a.name {
				kept = append(kept, name)
			}
		}
		*list = kept
		return len(kept) > 0
	})
}
