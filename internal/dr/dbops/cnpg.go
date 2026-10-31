package dbops

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/1985epma/k8scli/internal/dr"
)

// CloudNativePG, documented at https://cloudnative-pg.io/documentation/current/declarative_hibernation/:
// setting the annotation cnpg.io/hibernation=on on the Cluster deletes the instance pods (primary
// first, then the replicas, without a switchover) and keeps the PVCs; "off" (or removing the
// annotation) brings them back. Progress is the status condition cnpg.io/hibernation.
const (
	// KindCNPG is the Database.Kind of a CloudNativePG cluster.
	KindCNPG = "cnpg"
	// KindStatefulSet is the Database.Kind of a plain StatefulSet.
	KindStatefulSet = "statefulset"

	hibernationAnnotation = "cnpg.io/hibernation"
	hibernationCondition  = "cnpg.io/hibernation"
	cnpgClusterLabel      = "cnpg.io/cluster"
)

var (
	cnpgGVR       = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
	cnpgBackupGVR = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups"}
)

// cnpgTarget is one CloudNativePG cluster of the plan.
type cnpgTarget struct {
	p    *Planner
	env  *dr.Env
	plan *dr.Plan
	db   dr.Database
}

func (c *cnpgTarget) name() string { return c.db.Namespace + "/" + c.db.Ref }

// get reads the Cluster, translating the two kinds of "not found" into useful messages.
func (c *cnpgTarget) get(ctx context.Context) (*unstructured.Unstructured, error) {
	if c.env.Dynamic == nil {
		return nil, fmt.Errorf("%w: no dynamic client to read CloudNativePG clusters", dr.ErrUnreachable)
	}
	obj, err := c.env.Dynamic.Resource(cnpgGVR).Namespace(c.db.Namespace).Get(ctx, c.db.Ref, metav1.GetOptions{})
	switch {
	case err == nil:
		return obj, nil
	case isMissingResource(err):
		return nil, ErrOperatorMissing
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("CloudNativePG cluster %s was not found: check database %q in the plan", c.name(), c.db.Name)
	default:
		return nil, err
	}
}

func (c *cnpgTarget) annotate(ctx context.Context, value string) error {
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, hibernationAnnotation, value)
	_, err := c.env.Dynamic.Resource(cnpgGVR).Namespace(c.db.Namespace).
		Patch(ctx, c.db.Ref, types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: fieldManager})
	if isMissingResource(err) {
		return ErrOperatorMissing
	}
	return err
}

// fieldManager identifies the writes of this module in managed fields.
const fieldManager = "k8scli-dr"

func hibernationRequested(obj *unstructured.Unstructured) bool {
	return obj.GetAnnotations()[hibernationAnnotation] == "on"
}

func isHibernated(obj *unstructured.Unstructured) bool {
	c, ok := conditionOf(obj, hibernationCondition)
	return ok && c.Status == "True"
}

// clusterHealth judges the Cluster object alone and says what is missing.
//
// The Ready condition is NOT enough. Observed on CloudNativePG v1.30.1: right after "off" the operator
// still shows the Ready value from before the hibernation, so a check on it alone passes about a
// second after the annotation changed, while no instance exists yet. The counters decide instead
// (the API omits them when they are 0, so a missing counter means 0), and a cluster that the
// operator still reports as hibernated is never healthy.
func clusterHealth(obj *unstructured.Unstructured) (bool, string) {
	if hibernationRequested(obj) {
		return false, "hibernation is still requested (annotation " + hibernationAnnotation + "=on)"
	}
	if isHibernated(obj) {
		return false, "the operator still reports the cluster as hibernated"
	}

	want := desiredInstances(obj)
	ready, _ := intField(obj, "status", "readyInstances")
	if want <= 0 || ready < want {
		return false, fmt.Sprintf("%d of %d instances ready (phase: %s)", ready, want, orNone(statusString(obj, "status", "phase")))
	}

	if c, ok := conditionOf(obj, "Ready"); ok {
		if c.Status != "True" {
			return false, "the Ready condition is " + c.Status + " (reason: " + orNone(c.Reason) + ")"
		}
	} else if statusString(obj, "status", "phase") != "Cluster in healthy state" {
		// Older operator versions have no Ready condition; the phase text says the same thing.
		return false, "the cluster phase is " + orNone(statusString(obj, "status", "phase"))
	}
	return true, ""
}

func isHealthy(obj *unstructured.Unstructured) bool {
	ok, _ := clusterHealth(obj)
	return ok
}

// desiredInstances is how many instances the cluster should have: the larger of what the spec asks for
// and what the status says exists, so neither a stale status nor a missing field can lower the bar.
func desiredInstances(obj *unstructured.Unstructured) int64 {
	spec, _ := intField(obj, "spec", "instances")
	current, _ := intField(obj, "status", "instances")
	return max(spec, current)
}

// instancePods counts the cluster's pods that are Running and Ready, and those that exist at all
// (including terminating ones). known is false when there is no Kubernetes client to ask.
func (c *cnpgTarget) instancePods(ctx context.Context) (running, total int, known bool, err error) {
	if c.env.Client == nil {
		return 0, 0, false, nil
	}
	pods, err := c.env.Client.CoreV1().Pods(c.db.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: cnpgClusterLabel + "=" + c.db.Ref,
	})
	if err != nil {
		return 0, 0, true, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning && podReady(pod) {
			running++
		}
	}
	return running, len(pods.Items), true, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// stopped is true when the operator reports the hibernation AND no instance pod is left.
func (c *cnpgTarget) stopped(ctx context.Context, obj *unstructured.Unstructured) (bool, string, error) {
	if !isHibernated(obj) {
		cond, _ := conditionOf(obj, hibernationCondition)
		return false, "the operator has not reported the hibernation yet (condition: " + orNone(cond.Reason) + ")", nil
	}
	_, total, known, err := c.instancePods(ctx)
	if err != nil {
		return false, "", err
	}
	if known && total > 0 {
		return false, fmt.Sprintf("%d instance pod(s) are still terminating", total), nil
	}
	return true, "", nil
}

// healthy is true when the object says so and, with a Kubernetes client, the instance pods are really
// Running and Ready: the object can lag behind what exists.
func (c *cnpgTarget) healthy(ctx context.Context, obj *unstructured.Unstructured) (bool, string, error) {
	if ok, why := clusterHealth(obj); !ok {
		return false, why, nil
	}
	running, _, known, err := c.instancePods(ctx)
	if err != nil {
		return false, "", err
	}
	if want := desiredInstances(obj); known && int64(running) < want {
		return false, fmt.Sprintf("%d of %d instance pods are Running and Ready", running, want), nil
	}
	return true, "", nil
}

func (c *cnpgTarget) steps(ctx context.Context, dir dr.Direction) ([]dr.Step, error) {
	if dir == dr.DirDown {
		return c.downSteps(ctx)
	}
	return c.upSteps(), nil
}

func (c *cnpgTarget) downSteps(ctx context.Context) ([]dr.Step, error) {
	if c.env.Dynamic == nil {
		return nil, fmt.Errorf("%w: no dynamic client to read CloudNativePG clusters", dr.ErrUnreachable)
	}

	// The pre-flight verdict is computed now so a dry run shows it, and again, fresh, when applied.
	describe := "Check that the newest backup of " + c.name() + " is recent enough before it is hibernated."
	if cluster, err := c.get(ctx); err == nil {
		if summary, err := c.p.backupAgeCheck(ctx, c.env, c.plan, c.db, cluster); err != nil {
			describe += " BLOCKED: " + err.Error()
		} else {
			describe += " Currently OK: " + summary + "."
		}
	} else {
		describe += " It could not be evaluated now: " + err.Error()
	}

	preflight := &funcAction{
		id:       c.actionID("preflight"),
		title:    "Check backups before hibernating " + c.name(),
		describe: describe,
		risk:     dr.RiskLow,
		// A cluster that is already stopped has nothing left to protect; this keeps a resumed run from
		// being blocked by an old backup after the hibernation already happened.
		done: func(ctx context.Context) (bool, error) {
			obj, err := c.get(ctx)
			if err != nil {
				return false, err
			}
			return isHibernated(obj), nil
		},
		apply: func(ctx context.Context) error {
			obj, err := c.get(ctx)
			if err != nil {
				return err
			}
			summary, err := c.p.backupAgeCheck(ctx, c.env, c.plan, c.db, obj)
			if err != nil {
				return err
			}
			logf(c.env, "%s: %s", c.name(), summary)
			return nil
		},
	}

	hibernate := &funcAction{
		id:    c.actionID("hibernate"),
		title: "Hibernate CloudNativePG cluster " + c.name(),
		describe: "Set the annotation " + hibernationAnnotation + "=on on cluster " + c.name() + ". The operator deletes the primary " +
			"pod and then the replicas without a switchover and keeps every PVC, so the data stays on disk.",
		risk:       dr.RiskHigh,
		equivalent: fmt.Sprintf("kubectl annotate cluster %s -n %s --overwrite %s=on", c.db.Ref, c.db.Namespace, hibernationAnnotation),
		done: func(ctx context.Context) (bool, error) {
			obj, err := c.get(ctx)
			if err != nil {
				return false, err
			}
			ok, _, err := c.stopped(ctx, obj)
			return ok, err
		},
		apply: func(ctx context.Context) error {
			obj, err := c.get(ctx)
			if err != nil {
				return err
			}
			if hibernationRequested(obj) {
				return nil // already asked; the gate waits for the operator
			}
			return c.annotate(ctx, "on")
		},
	}

	return []dr.Step{
		{Phase: dr.PhaseDatabases, Action: preflight},
		{Phase: dr.PhaseDatabases, Action: hibernate, Gate: c.hibernatedGate()},
	}, nil
}

func (c *cnpgTarget) upSteps() []dr.Step {
	start := &funcAction{
		id:    c.actionID("start"),
		title: "Start CloudNativePG cluster " + c.name(),
		describe: "Set the annotation " + hibernationAnnotation + "=off on cluster " + c.name() + " so the operator recreates the " +
			"instances from the retained PVCs, then wait until the cluster is healthy.",
		risk:       dr.RiskLow,
		equivalent: fmt.Sprintf("kubectl annotate cluster %s -n %s --overwrite %s=off", c.db.Ref, c.db.Namespace, hibernationAnnotation),
		done: func(ctx context.Context) (bool, error) {
			obj, err := c.get(ctx)
			if err != nil {
				return false, err
			}
			ok, _, err := c.healthy(ctx, obj)
			return ok, err
		},
		apply: func(ctx context.Context) error {
			obj, err := c.get(ctx)
			if err != nil {
				return err
			}
			if !hibernationRequested(obj) {
				return nil // nothing to switch off; the gate waits for the instances
			}
			return c.annotate(ctx, "off")
		},
	}
	return []dr.Step{{Phase: dr.PhaseDatabases, Action: start, Gate: c.healthyGate()}}
}

func (c *cnpgTarget) actionID(verb string) string {
	return "databases/" + c.db.Namespace + "/" + c.db.Ref + ":" + verb
}

// hibernatedGate waits for the operator's hibernation condition and for the instance pods to be gone.
func (c *cnpgTarget) hibernatedGate() dr.Gate {
	return &pollGate{
		name:    "cluster " + c.name() + " hibernated",
		env:     c.env,
		timeout: c.p.opts.GateTimeout,
		poll:    c.p.opts.GatePoll,
		check: func(ctx context.Context) (bool, string, error) {
			obj, err := c.get(ctx)
			if err != nil {
				return false, "", err
			}
			return c.stopped(ctx, obj)
		},
	}
}

// healthyGate waits until every instance exists, is Running and Ready, and the object agrees.
func (c *cnpgTarget) healthyGate() dr.Gate {
	return &pollGate{
		name:    "cluster " + c.name() + " healthy",
		env:     c.env,
		timeout: c.p.opts.GateTimeout,
		poll:    c.p.opts.GatePoll,
		check: func(ctx context.Context) (bool, string, error) {
			obj, err := c.get(ctx)
			if err != nil {
				return false, "", err
			}
			return c.healthy(ctx, obj)
		},
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
