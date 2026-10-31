package chaos

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/k8sops"
)

// DefaultDrainTimeout bounds a chaos drain; a drain that cannot finish in this time is a finding.
const DefaultDrainTimeout = 5 * time.Minute

// NodeMode is what is done to the node.
type NodeMode string

// Modes.
const (
	ModeCordon NodeMode = "cordon"
	ModeDrain  NodeMode = "drain"
)

// NodeOptions describe a node injection.
type NodeOptions struct {
	Node         string
	DrainTimeout time.Duration
	// AllowOtherNamespaces lets a drain evict pods of namespaces that are not marked chaos-allowed. It
	// is off by default: a drain is node-wide, and the label is what limits the blast radius.
	AllowOtherNamespaces bool
}

// NodeAction cordons or drains one worker node. It reuses the k8sops steps (the same code a DR run uses),
// recording what it cordoned under a separate state key so it never confuses a real "down" run.
type NodeAction struct {
	guard   Guard
	opts    NodeOptions
	mode    NodeMode
	steps   []dr.Step
	planner *k8sops.NodesPlanner
	scope   dr.Scope
	// cenv is the environment handed to k8sops: same cluster, its own state namespace.
	cenv *dr.Env

	evicting  map[string]int
	otherNS   []string
	cordoned  bool
	remaining int
}

// NewCordonNode plans cordoning a node: no new pod will be scheduled there, nothing is evicted.
func NewCordonNode(ctx context.Context, guard Guard, opts NodeOptions) (*NodeAction, error) {
	return newNodeAction(ctx, guard, opts, ModeCordon)
}

// NewDrainNode plans cordoning and draining a node through the Eviction API.
func NewDrainNode(ctx context.Context, guard Guard, opts NodeOptions) (*NodeAction, error) {
	return newNodeAction(ctx, guard, opts, ModeDrain)
}

func newNodeAction(ctx context.Context, guard Guard, opts NodeOptions, mode NodeMode) (*NodeAction, error) {
	if err := guard.Lab(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Node) == "" {
		return nil, fmt.Errorf("%w: a node name is required", dr.ErrPreflight)
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = DefaultDrainTimeout
	}
	a := &NodeAction{guard: guard, opts: opts, mode: mode}
	if err := a.validate(ctx); err != nil {
		return nil, err
	}

	copyOfEnv := *guard.Env
	copyOfEnv.Context = guard.Env.Context + ".chaos"
	a.cenv = &copyOfEnv
	a.scope = dr.Scope{Phases: []dr.Phase{dr.PhaseNodes}}
	a.planner = k8sops.NewNodesPlanner(k8sops.Options{Nodes: []string{opts.Node}, DrainTimeout: opts.DrainTimeout})

	steps, err := a.planner.Steps(ctx, a.cenv, guard.Plan, dr.DirDown, a.scope)
	if err != nil {
		return nil, err
	}
	for _, step := range steps {
		if mode == ModeCordon && !strings.HasSuffix(step.Action.ID(), ":cordon") {
			continue
		}
		a.steps = append(a.steps, step)
	}
	if len(a.steps) == 0 {
		return nil, fmt.Errorf("%w: nothing to do for node %s", dr.ErrPreflight, opts.Node)
	}
	return a, nil
}

// validate checks the blast radius against the cluster as it is now. It runs when planning and again
// right before applying.
func (a *NodeAction) validate(ctx context.Context) error {
	client := a.guard.Env.Client
	node, err := client.CoreV1().Nodes().Get(ctx, a.opts.Node, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: node %q was not found", dr.ErrPreflight, a.opts.Node)
	}
	if err != nil {
		return fmt.Errorf("read node %s: %w", a.opts.Node, err)
	}
	if isControlPlane(node) {
		return fmt.Errorf("%w: %s", ErrControlPlane, a.opts.Node)
	}

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	others := 0
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Name != a.opts.Node && !isControlPlane(n) && !n.Spec.Unschedulable && nodeReady(n) {
			others++
		}
	}
	if others < 1 {
		return fmt.Errorf("%w: no other ready, schedulable worker would remain; the pods would have nowhere to go", ErrBlastRadius)
	}
	a.remaining = others
	a.cordoned = node.Spec.Unschedulable

	if a.mode != ModeDrain {
		return nil
	}
	pods, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + a.opts.Node})
	if err != nil {
		return fmt.Errorf("list the pods of node %s: %w", a.opts.Node, err)
	}
	allowed, err := a.guard.allowedNamespaces(ctx)
	if err != nil {
		return err
	}
	a.evicting = map[string]int{}
	foreign := map[string]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != a.opts.Node || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed ||
			ownedByDaemonSet(pod) || isMirrorPod(pod) {
			continue
		}
		a.evicting[pod.Namespace]++
		if !allowed[pod.Namespace] {
			foreign[pod.Namespace] = true
		}
	}
	a.otherNS = a.otherNS[:0]
	for ns := range foreign {
		a.otherNS = append(a.otherNS, ns)
	}
	sort.Strings(a.otherNS)
	if len(a.otherNS) > 0 && !a.opts.AllowOtherNamespaces {
		return fmt.Errorf("%w: draining %s would evict pods of namespaces that are not marked chaos-allowed (%s); label them or pass the explicit option to accept it",
			ErrBlastRadius, a.opts.Node, strings.Join(a.otherNS, ", "))
	}
	return nil
}

func ownedByDaemonSet(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

func isMirrorPod(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]
	return ok
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

func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ---- dr.Action --------------------------------------------------------------------------------------------

// ID implements dr.Action.
func (a *NodeAction) ID() string { return "chaos/node/" + a.opts.Node + ":" + string(a.mode) }

// Title implements dr.Action.
func (a *NodeAction) Title() string {
	if a.mode == ModeDrain {
		return "Drain node " + a.opts.Node
	}
	return "Cordon node " + a.opts.Node
}

// Risk implements dr.Action.
func (a *NodeAction) Risk() dr.Risk { return dr.RiskHigh }

// Mode is cordon or drain.
func (a *NodeAction) Mode() NodeMode { return a.mode }

// Describe implements dr.Action.
func (a *NodeAction) Describe() string {
	if a.mode == ModeCordon {
		return fmt.Sprintf("Cordons node %s: no new pod is scheduled there and nothing is evicted. %d other ready worker(s) stay schedulable. The recovery step uncordons it.",
			a.opts.Node, a.remaining)
	}
	total := 0
	var parts []string
	for ns, n := range a.evicting {
		total += n
		parts = append(parts, fmt.Sprintf("%s: %d", ns, n))
	}
	sort.Strings(parts)
	where := "no pods to evict"
	if total > 0 {
		where = fmt.Sprintf("%d pod(s) to evict (%s)", total, strings.Join(parts, ", "))
	}
	note := ""
	if len(a.otherNS) > 0 {
		note = " WARNING: this includes namespaces that are not chaos-allowed: " + strings.Join(a.otherNS, ", ") + "."
	}
	if a.cordoned {
		note += " The node is already cordoned; it stays cordoned on recovery because this run did not cordon it."
	}
	return fmt.Sprintf("Cordons node %s and drains it through the Eviction API (PodDisruptionBudgets are honored, DaemonSet and mirror pods stay): %s, up to %s. %d other ready worker(s) stay schedulable. The recovery step uncordons it.%s",
		a.opts.Node, where, a.opts.DrainTimeout, a.remaining, note)
}

// Equivalent implements dr.Action.
func (a *NodeAction) Equivalent() string {
	if a.mode == ModeCordon {
		return "kubectl cordon " + a.opts.Node
	}
	return fmt.Sprintf("kubectl drain %s --ignore-daemonsets --timeout=%s", a.opts.Node, a.opts.DrainTimeout)
}

// Done is true when every underlying step is satisfied.
func (a *NodeAction) Done(ctx context.Context) (bool, error) {
	for _, step := range a.steps {
		done, err := step.Action.Done(ctx)
		if err != nil {
			return false, err
		}
		if !done {
			return false, nil
		}
	}
	return true, nil
}

// Apply cordons (and drains) the node after checking the guard again.
func (a *NodeAction) Apply(ctx context.Context) error {
	if err := a.guard.Real(); err != nil {
		return err
	}
	if err := a.validate(ctx); err != nil {
		return err
	}
	for _, step := range a.steps {
		done, err := step.Action.Done(ctx)
		if err != nil {
			return err
		}
		if done {
			continue
		}
		if err := step.Action.Apply(ctx); err != nil {
			return fmt.Errorf("%s: %w", step.Action.Title(), err)
		}
	}
	return nil
}

// Recovery returns the action that uncordons what this injection cordoned.
func (a *NodeAction) Recovery() dr.Action {
	return &uncordon{node: a.opts.Node, planner: a.planner, env: a.cenv, plan: a.guard.Plan, scope: a.scope}
}

// uncordon undoes a NodeAction. It asks k8sops for the "up" steps, which uncordon exactly the nodes that
// the injection cordoned (and no node that was already cordoned before).
type uncordon struct {
	node    string
	planner *k8sops.NodesPlanner
	env     *dr.Env
	plan    *dr.Plan
	scope   dr.Scope
}

func (u *uncordon) ID() string         { return "chaos/node/" + u.node + ":uncordon" }
func (u *uncordon) Title() string      { return "Uncordon node " + u.node }
func (u *uncordon) Risk() dr.Risk      { return dr.RiskLow }
func (u *uncordon) Equivalent() string { return "kubectl uncordon " + u.node }
func (u *uncordon) Describe() string {
	return "Makes node " + u.node + " schedulable again, only if the chaos injection is what cordoned it, and waits for it to be Ready."
}

func (u *uncordon) steps(ctx context.Context) ([]dr.Step, error) {
	return u.planner.Steps(ctx, u.env, u.plan, dr.DirUp, u.scope)
}

func (u *uncordon) Done(ctx context.Context) (bool, error) {
	steps, err := u.steps(ctx)
	if err != nil {
		return false, err
	}
	for _, step := range steps {
		done, err := step.Action.Done(ctx)
		if err != nil {
			return false, err
		}
		if !done {
			return false, nil
		}
	}
	return true, nil
}

func (u *uncordon) Apply(ctx context.Context) error {
	steps, err := u.steps(ctx)
	if err != nil {
		return err
	}
	for _, step := range steps {
		done, err := step.Action.Done(ctx)
		if err != nil {
			return err
		}
		if !done {
			if err := step.Action.Apply(ctx); err != nil {
				return fmt.Errorf("%s: %w", step.Action.Title(), err)
			}
		}
		if step.Gate != nil {
			if err := step.Gate.Wait(ctx); err != nil {
				return fmt.Errorf("%s: %w", step.Gate.Name(), err)
			}
		}
	}
	return nil
}

var (
	_ dr.Action = (*NodeAction)(nil)
	_ dr.Action = (*uncordon)(nil)
)
