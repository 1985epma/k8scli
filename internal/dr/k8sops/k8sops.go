// Package k8sops stops and starts what runs inside a cluster through the Kubernetes API: CronJobs,
// applications (Deployments and StatefulSets) and nodes (cordon, drain, uncordon). It also provides the
// gates that wait for pods, nodes, the API server or a health URL.
//
// Everything here follows the rules of the dr package: actions are idempotent, describable for dry runs,
// and never guess. What "down" learns (replica counts, the CronJobs and nodes it touched) is written both to
// the cluster (an annotation on the workload) and to dr.Env.State, so the matching "up" restores exactly
// that and nothing else.
package k8sops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/1985epma/k8scli/internal/dr"
)

// Names written into the cluster.
const (
	// AnnotationReplicasBefore holds the replica count a workload had before "down" scaled it to 0.
	AnnotationReplicasBefore = "k8scli.io/dr-replicas-before"
	// AnnotationKeepRunning on a CronJob keeps "down" from suspending it (a backup job, for example).
	AnnotationKeepRunning = "k8scli.io/dr-keep-running"
)

// Defaults.
const (
	defaultGateTimeout        = 10 * time.Minute
	defaultTerminationTimeout = 5 * time.Minute
	defaultDrainTimeout       = 10 * time.Minute
	defaultNodeReadyTimeout   = 10 * time.Minute
	defaultAPIReadyTimeout    = 10 * time.Minute
	defaultPollInterval       = 2 * time.Second
	maxBackoff                = 30 * time.Second
)

// Options tune the planners. The zero value is safe: control-plane nodes are never touched, pods with
// emptyDir data or without a controller are never evicted.
type Options struct {
	// GateTimeout bounds "pods ready" gates when an app does not set Health.Timeout (default 10m).
	GateTimeout time.Duration
	// TerminationTimeout bounds the wait for pods to disappear after a scale down (default 5m).
	TerminationTimeout time.Duration
	// SkipUnknownReplicas turns "up" on a workload at 0 replicas with no record from an error into a
	// warning and a skip. Off by default: the planner never guesses a replica count.
	SkipUnknownReplicas bool
	// ExcludeCronJobs lists "namespace/name" CronJobs that "down" must leave running.
	ExcludeCronJobs []string

	// IncludeControlPlane lets the nodes planner cordon and drain control-plane nodes.
	IncludeControlPlane bool
	// Nodes restricts the nodes planner to these node names; empty means every eligible worker.
	Nodes []string
	// DrainTimeout bounds the drain of one node (default 10m).
	DrainTimeout time.Duration
	// AllowEmptyDir evicts pods that use emptyDir volumes, losing that data (like kubectl --delete-emptydir-data).
	AllowEmptyDir bool
	// AllowUnmanagedPods evicts pods without a controller; nothing will recreate them (like kubectl --force).
	AllowUnmanagedPods bool
	// NodeReadyTimeout bounds the wait for nodes to become Ready on "up" (default 10m).
	NodeReadyTimeout time.Duration
	// DrainNamespaces limits a drain to the pods of these namespaces; the pods of every other namespace are
	// left where they are. Nil means every namespace, like kubectl drain.
	DrainNamespaces []string
	// DrainPlanNamespacesBeforePower, for a "down" that goes on to power the machines off (the plan has a
	// provider and the scope reaches the power phase), limits the drain to the namespaces the plan manages
	// (its apps and databases). Pods elsewhere (kube-system, operator namespaces) stop when the machine
	// does, and many of them use emptyDir or have no controller, which a full drain would refuse for good.
	// A run that does not reach the power phase, and an explicit DrainNamespaces, are not affected.
	DrainPlanNamespacesBeforePower bool
}

func (o Options) withDefaults() Options {
	if o.GateTimeout <= 0 {
		o.GateTimeout = defaultGateTimeout
	}
	if o.TerminationTimeout <= 0 {
		o.TerminationTimeout = defaultTerminationTimeout
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = defaultDrainTimeout
	}
	if o.NodeReadyTimeout <= 0 {
		o.NodeReadyTimeout = defaultNodeReadyTimeout
	}
	return o
}

// Planners returns the three planners of this package in phase order of a "down" run.
func Planners(opts Options) []dr.Planner {
	return []dr.Planner{NewPreparePlanner(opts), NewAppsPlanner(opts), NewNodesPlanner(opts)}
}

// ---- conflicts -------------------------------------------------------------------------------------------

// conflictRetries is how many times a read-modify-write is repeated after a 409 Conflict.
const conflictRetries = 6

// retryOnConflict runs fn, and runs it again after a Conflict ("the object has been modified"). fn must
// do its own read, so every attempt starts from the newest version of the object and re-applies the same
// edit. Controllers (an HPA, a rollout, an operator) rewrite objects constantly, so a conflict is routine and
// not an error; only a conflict that never clears ends the step, with a message that says so.
func retryOnConflict(ctx context.Context, sleep func(context.Context, time.Duration) error, what string, fn func() error) error {
	if sleep == nil {
		sleep = realSleep
	}
	backoff := 50 * time.Millisecond
	var err error
	for attempt := 0; ; attempt++ {
		if err = fn(); !apierrors.IsConflict(err) {
			return err
		}
		if attempt >= conflictRetries {
			break
		}
		if serr := sleep(ctx, backoff); serr != nil {
			return serr
		}
		if backoff < time.Second {
			backoff *= 2
		}
	}
	return fmt.Errorf("%s: the object kept changing underneath (%d conflicts in a row); try again: %w", what, conflictRetries+1, err)
}

// ---- clock -----------------------------------------------------------------------------------------------

// clock gathers the injectable time functions of an Env with safe defaults.
type clock struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
	logf  func(format string, args ...any)
}

func clockOf(env *dr.Env) clock {
	c := clock{now: time.Now, sleep: realSleep, logf: func(string, ...any) {}}
	if env == nil {
		return c
	}
	if env.Now != nil {
		c.now = env.Now
	}
	if env.Sleep != nil {
		c.sleep = env.Sleep
	}
	if env.Logf != nil {
		c.logf = env.Logf
	}
	return c
}

func realSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ---- state -----------------------------------------------------------------------------------------------

// ledger is one document of dr.Env.State, "<context>/<name>".
type ledger struct {
	store dr.StateStore
	key   string
}

// stateMu serializes read-modify-write cycles: steps run one at a time, but a resumed run or the web
// console may overlap planning with applying.
var stateMu sync.Mutex

func newLedger(env *dr.Env, fallback *memStore, name string) ledger {
	var store dr.StateStore = fallback
	if env != nil && env.State != nil {
		store = env.State
	}
	contextName := ""
	if env != nil {
		contextName = env.Context
	}
	return ledger{store: store, key: contextName + "/" + name}
}

func readState[T any](ctx context.Context, l ledger) (T, bool, error) {
	stateMu.Lock()
	defer stateMu.Unlock()

	var value T
	found, err := l.store.Load(ctx, l.key, &value)
	return value, found, err
}

// modifyState loads the document, lets mutate change it and stores the result; when mutate reports that
// nothing is left to keep, the document is deleted.
func modifyState[T any](ctx context.Context, l ledger, mutate func(value *T) (keep bool)) error {
	stateMu.Lock()
	defer stateMu.Unlock()

	var value T
	if _, err := l.store.Load(ctx, l.key, &value); err != nil {
		return err
	}
	if mutate(&value) {
		return l.store.Save(ctx, l.key, value)
	}
	return l.store.Delete(ctx, l.key)
}

// memStore is the fallback when the Env has no StateStore: progress then lives only in the cluster
// annotations and in this process.
type memStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemStore() *memStore { return &memStore{data: map[string][]byte{}} }

func (m *memStore) Load(_ context.Context, key string, into any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.data[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, into)
}

func (m *memStore) Save(_ context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = raw
	return nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

// ---- errors ----------------------------------------------------------------------------------------------

// isUnreachable reports whether err looks like "the cluster cannot be reached" (connection refused, no
// route, timeout) as opposed to an answer from the API server such as NotFound or Forbidden.
func isUnreachable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, dr.ErrUnreachable) {
		return true
	}
	var status apierrors.APIStatus
	return !errors.As(err, &status)
}
