package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"

	"github.com/1985epma/k8scli/internal/dr"
)

const (
	// DefaultAPITimeout is how long the gate waits for the API after powering on.
	DefaultAPITimeout = 10 * time.Minute

	probeTimeout  = 5 * time.Second
	gateFirstWait = time.Second
	gateMaxWait   = 15 * time.Second
)

// APIReachable reports whether the Kubernetes API answers. A nil client, a refused connection or a
// server that is still starting (5xx) all mean "not yet". An authentication or authorization error
// still proves the API is up.
func APIReachable(ctx context.Context, client kubernetes.Interface) bool {
	if client == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	discovery := client.Discovery()
	if discovery == nil {
		return false
	}
	if rest := discovery.RESTClient(); rest != nil {
		err := rest.Get().AbsPath("/readyz").Do(ctx).Error()
		if err == nil {
			return true
		}
		var status apierrors.APIStatus
		if errors.As(err, &status) {
			code := status.Status().Code
			return code == 401 || code == 403
		}
		return false
	}

	// Clients without a REST transport (the fake clientset) can only ask for the version, and that call
	// takes no context, so bound it here.
	answered := make(chan bool, 1)
	go func() {
		_, err := discovery.ServerVersion()
		answered <- err == nil
	}()
	select {
	case ok := <-answered:
		return ok
	case <-ctx.Done():
		return false
	}
}

// APIGate waits until the Kubernetes API answers, polling with a growing delay.
type APIGate struct {
	env     *dr.Env
	timeout time.Duration
}

// NewAPIGate returns a gate that gives up after timeout (default 10 minutes). It reads env.Client on
// every poll, so a client that is only built once the cluster is back is picked up.
func NewAPIGate(env *dr.Env, timeout time.Duration) *APIGate {
	if timeout <= 0 {
		timeout = DefaultAPITimeout
	}
	return &APIGate{env: env, timeout: timeout}
}

// Name implements dr.Gate.
func (g *APIGate) Name() string { return "Kubernetes API reachable" }

// Wait implements dr.Gate.
func (g *APIGate) Wait(ctx context.Context) error {
	now, sleep := nowFn(g.env), sleepFn(g.env)
	deadline := now().Add(g.timeout)
	delay := gateFirstWait

	for {
		if APIReachable(ctx, g.env.Client) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return fmt.Errorf("the Kubernetes API did not become reachable within %s", g.timeout)
		}
		if err := sleep(ctx, min(delay, remaining)); err != nil {
			return err
		}
		delay = min(delay*2, gateMaxWait)
	}
}
