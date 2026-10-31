// Package chaos injects small, bounded failures so a team can watch a system absorb them: kill some pods
// of a workload, cordon a node, drain a node. It is not a chaos platform. There are no network faults,
// no CPU or memory stress and no machine power loss (a power drill belongs to the provider adapters).
//
// The blast radius is limited in code, not only in the CLI:
//
//   - the plan must say safety.labOnly: true;
//   - pods are killed only in namespaces labelled k8scli.io/chaos-allowed=true (never a system namespace);
//   - a node is drained only if it is a worker, another worker stays schedulable, and no pod of a
//     namespace that is not chaos-allowed would be evicted (unless the caller says so explicitly);
//   - a workload is never left without a ready replica, and a PodDisruptionBudget is always honored;
//   - a context that looks like production also needs the typed phrase "chaos <context>";
//   - every real injection has an automatic recovery step and a maximum duration.
//
// Every injection is a dr.Action (idempotent, describable for a dry run, risk-classified), so it can run
// alone or inside a dr runbook.
package chaos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/1985epma/k8scli/internal/dr"
)

// LabelAllowed marks a namespace where chaos may run. The value must be exactly "true".
const LabelAllowed = "k8scli.io/chaos-allowed"

// MaxPause and MaxExperiment bound how long anything may wait or observe.
const (
	MaxPause      = 30 * time.Minute
	MaxExperiment = 30 * time.Minute
)

// ConfirmPhrase is what a person types to run chaos in a context that looks like production.
func ConfirmPhrase(contextName string) string { return "chaos " + contextName }

// Errors. All of them wrap a dr sentinel so callers can map them with errors.Is.
var (
	// ErrNotLab: the plan does not declare itself a lab.
	ErrNotLab = fmt.Errorf("%w: chaos only runs on plans with safety.labOnly: true", dr.ErrPreflight)
	// ErrNamespaceNotAllowed: the namespace lacks the chaos-allowed label or is a system namespace.
	ErrNamespaceNotAllowed = fmt.Errorf("%w: namespace is not marked for chaos", dr.ErrPreflight)
	// ErrBlastRadius: the request would hurt more than the limits allow.
	ErrBlastRadius = fmt.Errorf("%w: beyond the allowed blast radius", dr.ErrPreflight)
	// ErrDisruptionBudget: a PodDisruptionBudget does not allow the disruption.
	ErrDisruptionBudget = fmt.Errorf("%w: a PodDisruptionBudget forbids it", dr.ErrPreflight)
	// ErrControlPlane: control-plane nodes are never touched.
	ErrControlPlane = fmt.Errorf("%w: control-plane nodes are never touched", dr.ErrPreflight)
	// ErrUnhealthyStart: the system was not healthy before the injection, so nothing was injected.
	ErrUnhealthyStart = fmt.Errorf("%w: the system is not healthy before the injection", dr.ErrPreflight)
	// ErrNothingToObserve: an experiment without a probe proves nothing.
	ErrNothingToObserve = fmt.Errorf("%w: nothing to observe (the plan has no app with a health check in scope)", dr.ErrPreflight)
)

// systemNamespaces are never targets, even when somebody labels them.
var systemNamespaces = map[string]bool{"kube-system": true, "kube-public": true, "kube-node-lease": true}

// Guard holds what every injection re-validates before it acts: the plan, the environment and the
// confirmation. Construct it once per run; its checks read the cluster each time they are called.
type Guard struct {
	Plan *dr.Plan
	Env  *dr.Env
	// Confirm is the phrase typed by the person (see ConfirmPhrase).
	Confirm string
	// ReadOnly refuses real injections (the console's read-only mode).
	ReadOnly bool
}

// Lab checks the part that does not depend on the kind of injection: the plan is a lab plan, it acts on
// the environment's context, and there is a client to act with.
func (g Guard) Lab() error {
	if g.Plan == nil || g.Env == nil {
		return fmt.Errorf("%w: a plan and an environment are required", dr.ErrPreflight)
	}
	if !g.Plan.Safety.LabOnly {
		return ErrNotLab
	}
	if g.Plan.Context != g.Env.Context {
		return fmt.Errorf("%w: the plan is for context %q but the environment is %q", dr.ErrPreflight, g.Plan.Context, g.Env.Context)
	}
	if g.Env.Client == nil {
		return fmt.Errorf("%w: no Kubernetes client is configured", dr.ErrUnreachable)
	}
	return nil
}

// Real adds the checks of an injection that is about to change something: read-only mode and, for a
// production-looking context, the typed phrase.
func (g Guard) Real() error {
	if err := g.Lab(); err != nil {
		return err
	}
	if g.ReadOnly {
		return dr.ErrReadOnly
	}
	if dr.LooksLikeProduction(g.Env.Context) {
		want := ConfirmPhrase(g.Env.Context)
		if strings.TrimSpace(g.Confirm) != want {
			return fmt.Errorf("%w: context %q looks like production; type %q to run chaos there", dr.ErrConfirmationRequired, g.Env.Context, want)
		}
	}
	return nil
}

// Namespace checks that chaos is allowed in a namespace: not a system namespace, and labelled
// k8scli.io/chaos-allowed=true right now (the label can be removed at any moment to stop chaos).
func (g Guard) Namespace(ctx context.Context, namespace string) error {
	if err := g.Lab(); err != nil {
		return err
	}
	return g.namespaceAllowed(ctx, namespace)
}

func (g Guard) namespaceAllowed(ctx context.Context, namespace string) error {
	if systemNamespaces[namespace] {
		return fmt.Errorf("%w: %s is a system namespace", ErrNamespaceNotAllowed, namespace)
	}
	ns, err := g.Env.Client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: namespace %q does not exist", dr.ErrPreflight, namespace)
	}
	if err != nil {
		return fmt.Errorf("read namespace %s: %w", namespace, err)
	}
	if ns.Labels[LabelAllowed] != "true" {
		return fmt.Errorf("%w: label the namespace first with `kubectl label namespace %s %s=true`", ErrNamespaceNotAllowed, namespace, LabelAllowed)
	}
	return nil
}

// allowedNamespaces returns the set of namespaces that carry the chaos label.
func (g Guard) allowedNamespaces(ctx context.Context) (map[string]bool, error) {
	list, err := g.Env.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: LabelAllowed + "=true"})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	out := map[string]bool{}
	for i := range list.Items {
		if !systemNamespaces[list.Items[i].Name] {
			out[list.Items[i].Name] = true
		}
	}
	return out, nil
}

// ---- small shared helpers -------------------------------------------------------------------------------

func now(env *dr.Env) time.Time {
	if env != nil && env.Now != nil {
		return env.Now()
	}
	return time.Now()
}

func sleep(ctx context.Context, env *dr.Env, d time.Duration) error {
	if env != nil && env.Sleep != nil {
		return env.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func logf(env *dr.Env, format string, args ...any) {
	if env != nil && env.Logf != nil {
		env.Logf(format, args...)
	}
}

// human renders 125s as 2m05s, like the measure package does.
func human(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d == 0 {
		return "0s"
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	total := int64(d.Round(time.Second) / time.Second)
	hours, minutes, secs := total/3600, total%3600/60, total%60
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh%02dm%02ds", hours, minutes, secs)
	case minutes > 0:
		return fmt.Sprintf("%dm%02ds", minutes, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

func seconds(d time.Duration) float64 {
	return float64(d.Round(time.Millisecond)) / float64(time.Second)
}

// isInjectionBlocked tells a refusal of the guard from a failure of the cluster.
func isInjectionBlocked(err error) bool {
	return errors.Is(err, dr.ErrPreflight) || errors.Is(err, dr.ErrConfirmationRequired) || errors.Is(err, dr.ErrReadOnly)
}
