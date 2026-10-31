package k8sops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/1985epma/k8scli/internal/dr"
)

// poll calls check until it succeeds, the timeout passes (measured with the injectable clock) or ctx ends.
// Errors from check are treated as "not yet" and reported when the time runs out, because a cluster that is
// still starting answers with errors for a while.
func poll(ctx context.Context, c clock, timeout, interval time.Duration, what string, check func(context.Context) (bool, string, error)) error {
	if interval <= 0 {
		interval = defaultPollInterval
	}
	deadline := c.now().Add(timeout)
	last := ""
	for {
		ok, detail, err := check(ctx)
		switch {
		case err == nil && ok:
			return nil
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			last = err.Error()
		default:
			last = detail
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !c.now().Before(deadline) {
			if last == "" {
				return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
			}
			return fmt.Errorf("timed out after %s waiting for %s: %s", timeout, what, last)
		}
		if err := c.sleep(ctx, interval); err != nil {
			return err
		}
	}
}

func describeRefs(refs []WorkloadRef) string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.String())
	}
	if len(names) > 3 {
		return fmt.Sprintf("%s and %d more", strings.Join(names[:3], ", "), len(names)-3)
	}
	return strings.Join(names, ", ")
}

// ---- pods ready ------------------------------------------------------------------------------------------

// PodsReadyGate waits until every workload has at least as many ready replicas as it wants.
type PodsReadyGate struct {
	Workloads []WorkloadRef
	Timeout   time.Duration
	Interval  time.Duration

	client kubernetes.Interface
	c      clock
}

// NewPodsReadyGate builds the gate; a zero timeout means 10 minutes.
func NewPodsReadyGate(env *dr.Env, workloads []WorkloadRef, timeout time.Duration) *PodsReadyGate {
	if timeout <= 0 {
		timeout = defaultGateTimeout
	}
	g := &PodsReadyGate{Workloads: workloads, Timeout: timeout, Interval: defaultPollInterval, c: clockOf(env)}
	if env != nil {
		g.client = env.Client
	}
	return g
}

// Name implements dr.Gate.
func (g *PodsReadyGate) Name() string { return "pods ready: " + describeRefs(g.Workloads) }

// Wait implements dr.Gate.
func (g *PodsReadyGate) Wait(ctx context.Context) error {
	if g.client == nil {
		return fmt.Errorf("%w: no Kubernetes client is configured", dr.ErrUnreachable)
	}
	o := ops{client: g.client}
	return poll(ctx, g.c, g.Timeout, g.Interval, g.Name(), func(ctx context.Context) (bool, string, error) {
		var waiting []string
		for _, ref := range g.Workloads {
			st, err := o.status(ctx, ref)
			if err != nil {
				return false, "", fmt.Errorf("%s: %w", ref, err)
			}
			if !st.observed || st.ready < st.desired {
				waiting = append(waiting, fmt.Sprintf("%s %d/%d ready", ref, st.ready, st.desired))
			}
		}
		return len(waiting) == 0, strings.Join(waiting, ", "), nil
	})
}

// ---- pods gone -------------------------------------------------------------------------------------------

// PodsGoneGate waits until the pods of the workloads have terminated. Stopping a database while the pods
// that use it are still shutting down is the mistake this gate prevents.
type PodsGoneGate struct {
	Workloads []WorkloadRef
	Timeout   time.Duration
	Interval  time.Duration

	client kubernetes.Interface
	c      clock
}

// NewPodsGoneGate builds the gate; a zero timeout means 5 minutes.
func NewPodsGoneGate(env *dr.Env, workloads []WorkloadRef, timeout time.Duration) *PodsGoneGate {
	if timeout <= 0 {
		timeout = defaultTerminationTimeout
	}
	g := &PodsGoneGate{Workloads: workloads, Timeout: timeout, Interval: defaultPollInterval, c: clockOf(env)}
	if env != nil {
		g.client = env.Client
	}
	return g
}

// Name implements dr.Gate.
func (g *PodsGoneGate) Name() string { return "pods stopped: " + describeRefs(g.Workloads) }

// Wait implements dr.Gate.
func (g *PodsGoneGate) Wait(ctx context.Context) error {
	if g.client == nil {
		return fmt.Errorf("%w: no Kubernetes client is configured", dr.ErrUnreachable)
	}
	o := ops{client: g.client}
	return poll(ctx, g.c, g.Timeout, g.Interval, g.Name(), func(ctx context.Context) (bool, string, error) {
		var running []string
		for _, ref := range g.Workloads {
			st, err := o.status(ctx, ref)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return false, "", fmt.Errorf("%s: %w", ref, err)
			}
			count := int(st.current)
			if st.selector != nil {
				pods, err := g.client.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{LabelSelector: st.selector.String()})
				if err != nil {
					return false, "", fmt.Errorf("%s: %w", ref, err)
				}
				count = 0
				for i := range pods.Items {
					if pods.Items[i].Status.Phase != corev1.PodSucceeded && pods.Items[i].Status.Phase != corev1.PodFailed {
						count++
					}
				}
			}
			if count > 0 {
				running = append(running, fmt.Sprintf("%s still has %d pod(s)", ref, count))
			}
		}
		return len(running) == 0, strings.Join(running, ", "), nil
	})
}

// ---- nodes ready -----------------------------------------------------------------------------------------

// NodesReadyGate waits until the named nodes are Ready. A node that no longer exists (an autoscaler removed
// it) is not waited for: it will never come back.
type NodesReadyGate struct {
	Nodes    []string
	Timeout  time.Duration
	Interval time.Duration

	client kubernetes.Interface
	c      clock
}

// NewNodesReadyGate builds the gate; a zero timeout means 10 minutes.
func NewNodesReadyGate(env *dr.Env, nodes []string, timeout time.Duration) *NodesReadyGate {
	if timeout <= 0 {
		timeout = defaultNodeReadyTimeout
	}
	g := &NodesReadyGate{Nodes: nodes, Timeout: timeout, Interval: defaultPollInterval, c: clockOf(env)}
	if env != nil {
		g.client = env.Client
	}
	return g
}

// Name implements dr.Gate.
func (g *NodesReadyGate) Name() string { return "nodes ready: " + strings.Join(g.Nodes, ", ") }

// Wait implements dr.Gate.
func (g *NodesReadyGate) Wait(ctx context.Context) error {
	if g.client == nil {
		return fmt.Errorf("%w: no Kubernetes client is configured", dr.ErrUnreachable)
	}
	return poll(ctx, g.c, g.Timeout, g.Interval, g.Name(), func(ctx context.Context) (bool, string, error) {
		var waiting []string
		for _, name := range g.Nodes {
			node, err := g.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return false, "", fmt.Errorf("node %s: %w", name, err)
			}
			if !nodeReady(node) {
				waiting = append(waiting, name+" is not Ready")
			}
		}
		return len(waiting) == 0, strings.Join(waiting, ", "), nil
	})
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ---- API ready -------------------------------------------------------------------------------------------

// APIReadyGate waits until the API server answers. It is meant for after a power-on. When the cluster does
// not answer in time, or there is no client at all, the error wraps dr.ErrUnreachable so the engine can tell
// "the cluster is still down" from a real failure.
type APIReadyGate struct {
	Timeout  time.Duration
	Interval time.Duration

	client kubernetes.Interface
	c      clock
}

// NewAPIReadyGate builds the gate; a zero timeout means 10 minutes.
func NewAPIReadyGate(env *dr.Env, timeout time.Duration) *APIReadyGate {
	if timeout <= 0 {
		timeout = defaultAPIReadyTimeout
	}
	g := &APIReadyGate{Timeout: timeout, Interval: defaultPollInterval, c: clockOf(env)}
	if env != nil {
		g.client = env.Client
	}
	return g
}

// Name implements dr.Gate.
func (g *APIReadyGate) Name() string { return "Kubernetes API answers" }

// Wait implements dr.Gate.
func (g *APIReadyGate) Wait(ctx context.Context) error {
	if g.client == nil {
		return fmt.Errorf("%w: no Kubernetes client is configured", dr.ErrUnreachable)
	}
	err := poll(ctx, g.c, g.Timeout, g.Interval, "the Kubernetes API", func(ctx context.Context) (bool, string, error) {
		// Each attempt is bounded on its own: a half-started API server can accept the connection and stall.
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err := g.client.Discovery().ServerVersionWithContext(attempt); err != nil {
			return false, "", err
		}
		return true, "", nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return fmt.Errorf("%w: %w", dr.ErrUnreachable, err)
	}
	return err
}

// ---- HTTP ------------------------------------------------------------------------------------------------

// HTTPGate polls a URL until it answers with the expected status (any 2xx when Status is 0).
type HTTPGate struct {
	URL      string
	Status   int
	Timeout  time.Duration
	Interval time.Duration

	http dr.HTTPChecker
	c    clock
}

// NewHTTPGate builds the gate; a zero timeout means 10 minutes.
func NewHTTPGate(env *dr.Env, url string, status int, timeout time.Duration) *HTTPGate {
	if timeout <= 0 {
		timeout = defaultGateTimeout
	}
	g := &HTTPGate{URL: url, Status: status, Timeout: timeout, Interval: defaultPollInterval, c: clockOf(env)}
	if env != nil {
		g.http = env.HTTP
	}
	return g
}

// Name implements dr.Gate.
func (g *HTTPGate) Name() string {
	if g.Status > 0 {
		return fmt.Sprintf("GET %s answers %d", g.URL, g.Status)
	}
	return fmt.Sprintf("GET %s answers 2xx", g.URL)
}

// Wait implements dr.Gate.
func (g *HTTPGate) Wait(ctx context.Context) error {
	if g.http == nil {
		return errors.New("no HTTP checker is configured, so the health URL cannot be verified")
	}
	return poll(ctx, g.c, g.Timeout, g.Interval, g.Name(), func(ctx context.Context) (bool, string, error) {
		status, err := g.http.Check(ctx, g.URL)
		if err != nil {
			return false, "", err
		}
		if (g.Status > 0 && status == g.Status) || (g.Status == 0 && status >= 200 && status < 300) {
			return true, "", nil
		}
		return false, fmt.Sprintf("it answered %d", status), nil
	})
}

// ---- several gates ---------------------------------------------------------------------------------------

// allGates waits for each gate in turn.
type allGates struct{ gates []dr.Gate }

func (a allGates) Name() string {
	names := make([]string, 0, len(a.gates))
	for _, g := range a.gates {
		names = append(names, g.Name())
	}
	return strings.Join(names, "; ")
}

func (a allGates) Wait(ctx context.Context) error {
	for _, g := range a.gates {
		if err := g.Wait(ctx); err != nil {
			return err
		}
	}
	return nil
}

func combineGates(gates []dr.Gate) dr.Gate {
	switch len(gates) {
	case 0:
		return nil
	case 1:
		return gates[0]
	}
	return allGates{gates: gates}
}
