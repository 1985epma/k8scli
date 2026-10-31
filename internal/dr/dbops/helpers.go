package dbops

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/1985epma/k8scli/internal/dr"
)

const (
	defaultGateTimeout = 15 * time.Minute
	defaultGatePoll    = 5 * time.Second
)

// ErrOperatorMissing means the CloudNativePG custom resource does not exist in the cluster, so the
// operator is not installed (or not installed in a version this module understands).
var ErrOperatorMissing = errors.New("the CloudNativePG operator is not installed in this cluster " +
	"(the clusters.postgresql.cnpg.io resource does not exist): install it, or use kind \"statefulset\"")

func nowOf(env *dr.Env) time.Time {
	if env != nil && env.Now != nil {
		return env.Now()
	}
	return time.Now()
}

func sleepOf(ctx context.Context, env *dr.Env, d time.Duration) error {
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

// funcAction is a dr.Action built from closures, so every operation stays a few lines long.
type funcAction struct {
	id, title, describe, equivalent string
	risk                            dr.Risk
	done                            func(ctx context.Context) (bool, error)
	apply                           func(ctx context.Context) error
}

func (a *funcAction) ID() string         { return a.id }
func (a *funcAction) Title() string      { return a.title }
func (a *funcAction) Describe() string   { return a.describe }
func (a *funcAction) Risk() dr.Risk      { return a.risk }
func (a *funcAction) Equivalent() string { return a.equivalent }

func (a *funcAction) Done(ctx context.Context) (bool, error) {
	if a.done == nil {
		return false, nil
	}
	return a.done(ctx)
}

func (a *funcAction) Apply(ctx context.Context) error { return a.apply(ctx) }

// pollGate polls a condition with the injected clock until it holds or its timeout passes.
// Transient read errors are tolerated and reported only if the condition never becomes true.
type pollGate struct {
	name    string
	env     *dr.Env
	timeout time.Duration
	poll    time.Duration
	// check returns (satisfied, what we are still waiting for, error).
	check func(ctx context.Context) (bool, string, error)
}

func (g *pollGate) Name() string { return g.name }

func (g *pollGate) Wait(ctx context.Context) error {
	deadline := nowOf(g.env).Add(g.timeout)
	var waiting string
	for {
		ok, status, err := g.check(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			waiting = "last read failed: " + err.Error()
		case ok:
			return nil
		default:
			waiting = status
		}

		if !nowOf(g.env).Before(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s: %s", g.timeout, g.name, waiting)
		}
		if err := sleepOf(ctx, g.env, g.poll); err != nil {
			return err
		}
	}
}

// isMissingResource tells "this kind of object does not exist in the API" (operator or CRD not
// installed) from "this named object does not exist". The server answers both with 404; only the
// second one carries the object name in the details.
func isMissingResource(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	s := status.Status()
	if s.Reason != metav1.StatusReasonNotFound {
		return false
	}
	return s.Details == nil || s.Details.Name == ""
}

// ---- reading unstructured status --------------------------------------------------------------------------

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func intField(obj *unstructured.Unstructured, fields ...string) (int64, bool) {
	value, found, err := unstructured.NestedFieldNoCopy(obj.Object, fields...)
	if err != nil || !found {
		return 0, false
	}
	return asInt64(value)
}

func statusString(obj *unstructured.Unstructured, fields ...string) string {
	value, _, _ := unstructured.NestedString(obj.Object, fields...)
	return value
}

type condition struct {
	Status         string
	Reason         string
	LastTransition time.Time
}

// conditionOf finds a status condition by type.
func conditionOf(obj *unstructured.Unstructured, conditionType string) (condition, bool) {
	list, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return condition{}, false
	}
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok || entry["type"] != conditionType {
			continue
		}
		c := condition{}
		c.Status, _ = entry["status"].(string)
		c.Reason, _ = entry["reason"].(string)
		if stamp, ok := entry["lastTransitionTime"].(string); ok {
			c.LastTransition, _ = time.Parse(time.RFC3339, stamp)
		}
		return c, true
	}
	return condition{}, false
}

func parseTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	default:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	}
}
