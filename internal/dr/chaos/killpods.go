package chaos

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/1985epma/k8scli/internal/dr"
)

// Limits of kill-pods.
const (
	// DefaultMaxPercent is the share of a workload's replicas that one injection may take down.
	DefaultMaxPercent = 50
	// MaxKill bounds the number of pods of one injection whatever the percentage says.
	MaxKill = 20
)

// KillPodsOptions describe one kill-pods injection.
type KillPodsOptions struct {
	Namespace string
	// Kind is "Deployment" (default) or "StatefulSet".
	Kind     string
	Workload string
	// Count is how many ready pods to evict (default 1).
	Count int
	// MaxPercent caps Count as a share of the workload's replicas, rounded down (default 50, at most 100).
	MaxPercent int
	// Pick chooses an index in [0, n); the default is random. Tests replace it.
	Pick func(n int) int
}

type podTarget struct {
	Name string
	UID  types.UID
}

// KillPods evicts a few ready pods of one workload and lets its controller recreate them. It uses the
// Eviction API, so a PodDisruptionBudget is honored by the cluster as well as pre-checked here.
type KillPods struct {
	guard    Guard
	opts     KillPodsOptions
	replicas int32
	ready    int
	targets  []podTarget
	budgets  []string
}

// NewKillPods validates the request against the blast-radius rules and picks the pods. It reads the
// cluster and changes nothing, so it is also what a dry run shows.
func NewKillPods(ctx context.Context, guard Guard, opts KillPodsOptions) (*KillPods, error) {
	if err := guard.Namespace(ctx, opts.Namespace); err != nil {
		return nil, err
	}
	if opts.Kind == "" {
		opts.Kind = "Deployment"
	}
	if opts.Kind != "Deployment" && opts.Kind != "StatefulSet" {
		return nil, fmt.Errorf("%w: kind must be Deployment or StatefulSet, not %q", dr.ErrPreflight, opts.Kind)
	}
	if opts.Workload == "" {
		return nil, fmt.Errorf("%w: a workload name is required", dr.ErrPreflight)
	}
	if opts.Count == 0 {
		opts.Count = 1
	}
	if opts.Count < 1 || opts.Count > MaxKill {
		return nil, fmt.Errorf("%w: count must be between 1 and %d", ErrBlastRadius, MaxKill)
	}
	if opts.MaxPercent == 0 {
		opts.MaxPercent = DefaultMaxPercent
	}
	if opts.MaxPercent < 1 || opts.MaxPercent > 100 {
		return nil, fmt.Errorf("%w: max percent must be between 1 and 100", ErrBlastRadius)
	}
	if opts.Pick == nil {
		opts.Pick = func(n int) int { return rand.IntN(n) } // #nosec G404 -- picking a chaos victim is not security sensitive
	}

	a := &KillPods{guard: guard, opts: opts}
	selector, replicas, err := a.workload(ctx)
	if err != nil {
		return nil, err
	}
	a.replicas = replicas

	pods, err := a.readyPods(ctx, selector)
	if err != nil {
		return nil, err
	}
	a.ready = len(pods)

	// Blast radius: a share of the replicas, and never the last ready one.
	allowed := int(int64(replicas) * int64(opts.MaxPercent) / 100)
	if allowed > MaxKill {
		allowed = MaxKill
	}
	if allowed < 1 {
		return nil, fmt.Errorf("%w: %s %s/%s has %d replica(s); at %d%% no pod may be killed",
			ErrBlastRadius, opts.Kind, opts.Namespace, opts.Workload, replicas, opts.MaxPercent)
	}
	if opts.Count > allowed {
		return nil, fmt.Errorf("%w: killing %d of %d replicas is above %d%% (at most %d)",
			ErrBlastRadius, opts.Count, replicas, opts.MaxPercent, allowed)
	}
	if len(pods)-opts.Count < 1 {
		return nil, fmt.Errorf("%w: %d ready pod(s); killing %d would leave no ready replica",
			ErrBlastRadius, len(pods), opts.Count)
	}

	if err := a.checkBudgets(ctx, pods); err != nil {
		return nil, err
	}

	// Choose among the ready pods, in a stable order so Pick is the only source of randomness.
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	pool := append([]corev1.Pod(nil), pods...)
	for len(a.targets) < opts.Count {
		i := opts.Pick(len(pool))
		if i < 0 || i >= len(pool) {
			i = 0
		}
		a.targets = append(a.targets, podTarget{Name: pool[i].Name, UID: pool[i].UID})
		pool = append(pool[:i], pool[i+1:]...)
	}
	sort.Slice(a.targets, func(i, j int) bool { return a.targets[i].Name < a.targets[j].Name })
	return a, nil
}

func (a *KillPods) workload(ctx context.Context) (labels.Selector, int32, error) {
	client := a.guard.Env.Client
	var (
		selector *metav1.LabelSelector
		replicas *int32
	)
	if a.opts.Kind == "StatefulSet" {
		s, err := client.AppsV1().StatefulSets(a.opts.Namespace).Get(ctx, a.opts.Workload, metav1.GetOptions{})
		if err != nil {
			return nil, 0, workloadError(a.opts, err)
		}
		selector, replicas = s.Spec.Selector, s.Spec.Replicas
	} else {
		d, err := client.AppsV1().Deployments(a.opts.Namespace).Get(ctx, a.opts.Workload, metav1.GetOptions{})
		if err != nil {
			return nil, 0, workloadError(a.opts, err)
		}
		selector, replicas = d.Spec.Selector, d.Spec.Replicas
	}
	if selector == nil {
		return nil, 0, fmt.Errorf("%w: %s %s/%s has no selector", dr.ErrPreflight, a.opts.Kind, a.opts.Namespace, a.opts.Workload)
	}
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: invalid selector of %s/%s: %w", dr.ErrPreflight, a.opts.Namespace, a.opts.Workload, err)
	}
	want := int32(1)
	if replicas != nil {
		want = *replicas
	}
	return sel, want, nil
}

func workloadError(opts KillPodsOptions, err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %s %s/%s was not found", dr.ErrPreflight, opts.Kind, opts.Namespace, opts.Workload)
	}
	return fmt.Errorf("read %s %s/%s: %w", opts.Kind, opts.Namespace, opts.Workload, err)
}

// readyPods lists the pods of the selector that are running, ready and not already terminating.
func (a *KillPods) readyPods(ctx context.Context, selector labels.Selector) ([]corev1.Pod, error) {
	list, err := a.guard.Env.Client.CoreV1().Pods(a.opts.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, fmt.Errorf("list the pods of %s/%s: %w", a.opts.Namespace, a.opts.Workload, err)
	}
	var out []corev1.Pod
	for i := range list.Items {
		pod := list.Items[i]
		if selector.Matches(labels.Set(pod.Labels)) && podReady(&pod) {
			out = append(out, pod)
		}
	}
	return out, nil
}

func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// checkBudgets refuses the injection when a PodDisruptionBudget that covers the pods has no disruption left.
func (a *KillPods) checkBudgets(ctx context.Context, pods []corev1.Pod) error {
	list, err := a.guard.Env.Client.PolicyV1().PodDisruptionBudgets(a.opts.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list PodDisruptionBudgets in %s: %w", a.opts.Namespace, err)
	}
	for i := range list.Items {
		pdb := &list.Items[i]
		if pdb.Spec.Selector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			continue
		}
		covered := false
		for j := range pods {
			if sel.Matches(labels.Set(pods[j].Labels)) {
				covered = true
				break
			}
		}
		if !covered {
			continue
		}
		a.budgets = append(a.budgets, pdb.Name)
		if int(pdb.Status.DisruptionsAllowed) < a.opts.Count {
			return fmt.Errorf("%w: PodDisruptionBudget %s allows %d disruption(s) and %d were requested",
				ErrDisruptionBudget, pdb.Name, pdb.Status.DisruptionsAllowed, a.opts.Count)
		}
	}
	return nil
}

// ---- dr.Action --------------------------------------------------------------------------------------------

// ID implements dr.Action.
func (a *KillPods) ID() string {
	return "chaos/" + a.opts.Namespace + "/" + a.opts.Workload + ":kill-pods"
}

// Title implements dr.Action.
func (a *KillPods) Title() string {
	return fmt.Sprintf("Kill %d pod(s) of %s/%s", len(a.targets), a.opts.Namespace, a.opts.Workload)
}

// Risk implements dr.Action.
func (a *KillPods) Risk() dr.Risk { return dr.RiskHigh }

// Targets are the names of the pods that will be evicted.
func (a *KillPods) Targets() []string {
	names := make([]string, 0, len(a.targets))
	for _, t := range a.targets {
		names = append(names, t.Name)
	}
	return names
}

// Describe implements dr.Action.
func (a *KillPods) Describe() string {
	guards := "no PodDisruptionBudget covers them"
	if len(a.budgets) > 0 {
		guards = "PodDisruptionBudget " + strings.Join(a.budgets, ", ") + " is honored"
	}
	return fmt.Sprintf("Evicts %s through the Eviction API from %s %s/%s (%d replicas, %d ready; at most %d%% may go and at least one stays ready; %s). The controller recreates them; nothing needs to be undone.",
		strings.Join(a.Targets(), ", "), a.opts.Kind, a.opts.Namespace, a.opts.Workload, a.replicas, a.ready, a.opts.MaxPercent, guards)
}

// Equivalent implements dr.Action.
func (a *KillPods) Equivalent() string {
	return fmt.Sprintf("kubectl delete pod %s -n %s", strings.Join(a.Targets(), " "), a.opts.Namespace)
}

// Recovery returns nil: the workload's controller recreates the pods on its own.
func (a *KillPods) Recovery() dr.Action { return nil }

// Done is true when every chosen pod is gone, terminating or replaced by a pod with another UID.
func (a *KillPods) Done(ctx context.Context) (bool, error) {
	for _, t := range a.targets {
		pod, err := a.guard.Env.Client.CoreV1().Pods(a.opts.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if pod.UID == t.UID && pod.DeletionTimestamp == nil {
			return false, nil
		}
	}
	return true, nil
}

// Apply evicts the pods. The guard is checked again right before: the namespace label may have been
// removed, and a replica may have been lost since the plan was made.
func (a *KillPods) Apply(ctx context.Context) error {
	if err := a.guard.Real(); err != nil {
		return err
	}
	if err := a.guard.namespaceAllowed(ctx, a.opts.Namespace); err != nil {
		return err
	}
	selector, _, err := a.workload(ctx)
	if err != nil {
		return err
	}
	ready, err := a.readyPods(ctx, selector)
	if err != nil {
		return err
	}
	stillReady := 0
	chosen := map[string]bool{}
	for _, t := range a.targets {
		chosen[t.Name] = true
	}
	for i := range ready {
		if !chosen[ready[i].Name] {
			stillReady++
		}
	}
	if stillReady < 1 {
		return fmt.Errorf("%w: only %d ready pod(s) remain outside the chosen ones; refusing to leave none", ErrBlastRadius, stillReady)
	}

	for _, t := range a.targets {
		uid := t.UID
		err := a.guard.Env.Client.CoreV1().Pods(a.opts.Namespace).EvictV1(ctx, &policyv1.Eviction{
			ObjectMeta:    metav1.ObjectMeta{Name: t.Name, Namespace: a.opts.Namespace},
			DeleteOptions: &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}},
		})
		switch {
		case err == nil, apierrors.IsNotFound(err), apierrors.IsConflict(err):
			// gone already, or replaced by a pod with another UID: nothing to kill
		case apierrors.IsTooManyRequests(err):
			return fmt.Errorf("%w: the cluster refused to evict %s/%s", ErrDisruptionBudget, a.opts.Namespace, t.Name)
		default:
			return fmt.Errorf("evict %s/%s: %w", a.opts.Namespace, t.Name, err)
		}
		logf(a.guard.Env, "evicted pod %s/%s", a.opts.Namespace, t.Name)
	}
	return nil
}

var _ dr.Action = (*KillPods)(nil)
