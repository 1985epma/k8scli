package k8sops

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/1985epma/k8scli/internal/dr"
)

// drainNode evicts the pods of a node through the Eviction API, which honors PodDisruptionBudgets: an
// eviction that a budget refuses is retried with backoff until the drain timeout.
type drainNode struct {
	nodeAction
	opts Options
	// namespaces limits the drain to the pods of these namespaces; nil means every namespace. Everything
	// below (emptyDir and unmanaged-pod refusals, PodDisruptionBudgets, DaemonSet and mirror pods) still
	// applies in full to the pods that are inside it.
	namespaces map[string]bool
}

func (a *drainNode) limited() bool { return a.namespaces != nil }

// namespaceList is the sorted list of the limited namespaces, for messages.
func (a *drainNode) namespaceList() string {
	names := make([]string, 0, len(a.namespaces))
	for name := range a.namespaces {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

func (a *drainNode) ID() string    { return "nodes/" + a.name + ":drain" }
func (a *drainNode) Title() string { return "Drain node " + a.name }
func (a *drainNode) Risk() dr.Risk { return dr.RiskHigh }

func (a *drainNode) Equivalent() string {
	cmd := "kubectl drain " + a.name + " --ignore-daemonsets"
	if a.opts.AllowEmptyDir {
		cmd += " --delete-emptydir-data"
	}
	if a.opts.AllowUnmanagedPods {
		cmd += " --force"
	}
	cmd = fmt.Sprintf("%s --timeout=%s", cmd, a.opts.DrainTimeout)
	if a.limited() {
		cmd += fmt.Sprintf("  # k8scli evicts only the pods of: %s (kubectl drain has no namespace filter)", a.namespaceList())
	}
	return cmd
}

func (a *drainNode) Describe() string {
	if a.limited() {
		return fmt.Sprintf("Evicts only the pods of the namespaces the plan manages (%s) from node %s through the Eviction API "+
			"(PodDisruptionBudgets are honored, emptyDir and unmanaged pods are refused, DaemonSet and mirror pods are left), "+
			"then waits up to %s for them to terminate. Pods of other namespaces (kube-system, operators) are left alone: "+
			"they stop with the node when it is powered off.", a.namespaceList(), a.name, a.opts.DrainTimeout)
	}
	return fmt.Sprintf("Evicts the pods of node %s through the Eviction API (PodDisruptionBudgets are honored, DaemonSet and mirror pods are left), then waits up to %s for them to terminate.",
		a.name, a.opts.DrainTimeout)
}

// drainSet is the pods of a node sorted by what must happen to them.
type drainSet struct {
	evict       []corev1.Pod
	terminating []corev1.Pod
	blockers    []string
}

func (a *drainNode) classify(ctx context.Context) (drainSet, error) {
	pods, err := a.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + a.name})
	if err != nil {
		return drainSet{}, fmt.Errorf("list the pods of node %s: %w", a.name, err)
	}
	var set drainSet
	for i := range pods.Items {
		pod := pods.Items[i]
		// The field selector is a hint for the server; filter again so any client behaves the same.
		if pod.Spec.NodeName != a.name || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if a.limited() && !a.namespaces[pod.Namespace] {
			continue // not ours to move: it stops with the machine
		}
		if isMirrorPod(&pod) || ownedByDaemonSet(&pod) {
			continue
		}
		if pod.DeletionTimestamp != nil {
			set.terminating = append(set.terminating, pod)
			continue
		}
		if usesEmptyDir(&pod) && !a.opts.AllowEmptyDir {
			set.blockers = append(set.blockers, fmt.Sprintf("%s/%s uses an emptyDir volume, so its data would be lost", pod.Namespace, pod.Name))
			continue
		}
		if metav1.GetControllerOf(&pod) == nil && !a.opts.AllowUnmanagedPods {
			set.blockers = append(set.blockers, fmt.Sprintf("%s/%s has no controller, so nothing would recreate it", pod.Namespace, pod.Name))
			continue
		}
		set.evict = append(set.evict, pod)
	}
	return set, nil
}

func (a *drainNode) blockedError(set drainSet) error {
	sort.Strings(set.blockers)
	hints := []string{}
	for _, b := range set.blockers {
		if strings.Contains(b, "emptyDir") {
			hints = append(hints, "AllowEmptyDir")
			break
		}
	}
	for _, b := range set.blockers {
		if strings.Contains(b, "no controller") {
			hints = append(hints, "AllowUnmanagedPods")
			break
		}
	}
	return fmt.Errorf("cannot drain node %s, nothing was evicted: %s (set %s to accept that)", a.name, strings.Join(set.blockers, "; "), strings.Join(hints, " and "))
}

// Done is true when the node is gone or has no pod left that a drain would touch.
func (a *drainNode) Done(ctx context.Context) (bool, error) {
	if _, err := a.client.CoreV1().Nodes().Get(ctx, a.name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	set, err := a.classify(ctx)
	if err != nil {
		return false, err
	}
	return len(set.evict) == 0 && len(set.terminating) == 0 && len(set.blockers) == 0, nil
}

func (a *drainNode) Apply(ctx context.Context) error {
	first, err := a.classify(ctx)
	if err != nil {
		return err
	}
	if len(first.blockers) > 0 {
		return a.blockedError(first) // refuse before evicting anything
	}

	deadline := a.c.now().Add(a.opts.DrainTimeout)
	backoff := defaultPollInterval
	for {
		set, err := a.classify(ctx)
		if err != nil {
			return err
		}
		if len(set.evict) == 0 && len(set.terminating) == 0 {
			a.c.logf("node %s is drained", a.name)
			return nil
		}

		var refused []string
		for i := range set.evict {
			pod := &set.evict[i]
			err := a.client.CoreV1().Pods(pod.Namespace).EvictV1(ctx, &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}})
			switch {
			case err == nil, apierrors.IsNotFound(err):
			case apierrors.IsTooManyRequests(err):
				refused = append(refused, pod.Namespace+"/"+pod.Name)
			default:
				return fmt.Errorf("evict %s/%s from node %s: %w", pod.Namespace, pod.Name, a.name, err)
			}
		}

		remaining, err := a.classify(ctx)
		if err != nil {
			return err
		}
		if len(remaining.evict) == 0 && len(remaining.terminating) == 0 {
			a.c.logf("node %s is drained", a.name)
			return nil
		}
		if !a.c.now().Before(deadline) {
			sort.Strings(refused)
			detail := fmt.Sprintf("%d pod(s) are still on it", len(remaining.evict)+len(remaining.terminating))
			if len(refused) > 0 {
				detail += "; a PodDisruptionBudget refused: " + strings.Join(refused, ", ")
			}
			return fmt.Errorf("timed out after %s draining node %s: %s", a.opts.DrainTimeout, a.name, detail)
		}
		if err := a.c.sleep(ctx, backoff); err != nil {
			return err
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func isMirrorPod(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]
	return ok
}

func ownedByDaemonSet(pod *corev1.Pod) bool {
	owner := metav1.GetControllerOf(pod)
	return owner != nil && owner.Kind == "DaemonSet"
}

func usesEmptyDir(pod *corev1.Pod) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.EmptyDir != nil {
			return true
		}
	}
	return false
}
