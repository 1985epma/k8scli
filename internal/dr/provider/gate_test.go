package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestAPIReachableWithTheFakeClient(t *testing.T) {
	ctx := context.Background()
	if APIReachable(ctx, nil) {
		t.Error("a nil client is not reachable")
	}
	e := newTestEnv()
	if !APIReachable(ctx, e.Client) {
		t.Error("the fake client should answer")
	}
	e.setReachable(false)
	if APIReachable(ctx, e.Client) {
		t.Error("a refused connection is not reachable")
	}
}

// The real client talks HTTP, so cover the /readyz branch against a local server.
func TestAPIReachableOverHTTP(t *testing.T) {
	ctx := context.Background()
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Errorf("probed %s; want /readyz", r.URL.Path)
		}
		if status == http.StatusOK {
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","code":%d}`, status)
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		status int
		want   bool
		why    string
	}{
		{http.StatusOK, true, "ready"},
		{http.StatusServiceUnavailable, false, "starting up"},
		{http.StatusInternalServerError, false, "failing"},
		{http.StatusForbidden, true, "up, but not allowed to read readyz"},
		{http.StatusUnauthorized, true, "up, credentials rejected"},
	} {
		status = tc.status
		if got := APIReachable(ctx, client); got != tc.want {
			t.Errorf("HTTP %d (%s): reachable = %v; want %v", tc.status, tc.why, got, tc.want)
		}
	}

	server.Close()
	if APIReachable(ctx, client) {
		t.Error("a closed server is not reachable")
	}
}

func TestGateReturnsAtOnceWhenTheAPIIsUp(t *testing.T) {
	e := newTestEnv()
	if err := NewAPIGate(e.Env, time.Minute).Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.clock.sleeps) != 0 {
		t.Errorf("slept %v although the API was up", e.clock.sleeps)
	}
}

func TestGatePollsWithGrowingDelays(t *testing.T) {
	e := newTestEnv()
	e.setReachable(false)
	e.clock.hook = func(n int) {
		if n == 7 {
			e.setReachable(true)
		}
	}
	if err := NewAPIGate(e.Env, 10*time.Minute).Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{1, 2, 4, 8, 15, 15, 15}
	if len(e.clock.sleeps) != len(want) {
		t.Fatalf("sleeps = %v", e.clock.sleeps)
	}
	for i, d := range want {
		if e.clock.sleeps[i] != d*time.Second {
			t.Errorf("sleep %d = %s; want %s (doubling, capped at 15s)", i, e.clock.sleeps[i], d*time.Second)
		}
	}
}

func TestGateGivesUpAfterItsTimeout(t *testing.T) {
	e := newTestEnv()
	e.setReachable(false)
	err := NewAPIGate(e.Env, 90*time.Second).Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "1m30s") {
		t.Fatalf("err = %v; want a timeout naming the duration", err)
	}
	var total time.Duration
	for _, d := range e.clock.sleeps {
		total += d
	}
	if total != 90*time.Second {
		t.Errorf("waited %s of fake time; want exactly the timeout (the last sleep is trimmed)", total)
	}
}

func TestGateWithoutAClientNeverBecomesReachable(t *testing.T) {
	e := newTestEnv()
	e.Client = nil
	if err := NewAPIGate(e.Env, 30*time.Second).Wait(context.Background()); err == nil {
		t.Error("a nil client can never become reachable")
	}
}

// The wiring may only build the client once the cluster is back; the gate reads it on every poll.
func TestGatePicksUpAClientThatAppearsLater(t *testing.T) {
	e := newTestEnv()
	late := newTestEnv()
	e.Client = nil
	e.clock.hook = func(n int) {
		if n == 2 {
			e.Client = late.Client
		}
	}
	if err := NewAPIGate(e.Env, time.Minute).Wait(context.Background()); err != nil {
		t.Errorf("a client that appears later should be used: %v", err)
	}
	if len(e.clock.sleeps) != 2 {
		t.Errorf("sleeps = %v; want 2 (the client appeared during the second)", e.clock.sleeps)
	}
}

func TestGateStopsWhenTheContextIsCancelled(t *testing.T) {
	e := newTestEnv()
	e.setReachable(false)
	ctx, cancel := context.WithCancel(context.Background())
	e.clock.hook = func(n int) {
		if n == 2 {
			cancel()
		}
	}
	if err := NewAPIGate(e.Env, time.Hour).Wait(ctx); err == nil || ctx.Err() == nil {
		t.Errorf("err = %v; want the cancellation", err)
	}
}

func TestGateDefaultsAndRealClock(t *testing.T) {
	g := NewAPIGate(nil, 0)
	if g.timeout != DefaultAPITimeout {
		t.Errorf("timeout = %s", g.timeout)
	}
	// With no Sleep or Now in the env, the gate falls back to real time (and returns at once here).
	e := newTestEnv()
	e.Now, e.Sleep = nil, nil
	if err := NewAPIGate(e.Env, time.Second).Wait(context.Background()); err != nil {
		t.Error(err)
	}
}
