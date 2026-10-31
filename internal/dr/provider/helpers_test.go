package provider

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

// fakeExec records every command and answers from a handler.
type fakeExec struct {
	mu      sync.Mutex
	calls   []command
	handler func(name string, args []string) (string, error)
}

func (f *fakeExec) Run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, command{program: name, args: append([]string(nil), args...)})
	handler := f.handler
	f.mu.Unlock()
	if handler == nil {
		return "", nil
	}
	return handler(name, args)
}

// lines returns the recorded commands as they would be typed.
func (f *fakeExec) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.line()
	}
	return out
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// memState is an in-memory dr.StateStore.
type memState struct {
	mu   sync.Mutex
	docs map[string][]byte
}

func newMemState() *memState { return &memState{docs: map[string][]byte{}} }

func (m *memState) Load(_ context.Context, key string, into any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.docs[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, into)
}

func (m *memState) Save(_ context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs[key] = raw
	return nil
}

func (m *memState) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.docs, key)
	return nil
}

func (m *memState) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.docs[key]
	return ok
}

// clock is a fake time source: Sleep only moves it forward.
type clock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
	hook   func(n int) // called after each sleep with the number of sleeps so far
}

func newClock() *clock { return &clock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.sleeps = append(c.sleeps, d)
	n, hook := len(c.sleeps), c.hook
	c.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return nil
}

// testEnv bundles a dr.Env with the fakes behind it.
type testEnv struct {
	*dr.Env
	exec      *fakeExec
	state     *memState
	clock     *clock
	client    *fake.Clientset
	reachable *atomic.Bool
}

// newTestEnv returns an env whose API server is reachable until setReachable(false).
func newTestEnv(objects ...runtime.Object) *testEnv {
	client := fake.NewSimpleClientset(objects...)
	reachable := &atomic.Bool{}
	reachable.Store(true)
	client.PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !reachable.Load() {
			return true, nil, errConnectionRefused
		}
		return false, nil, nil
	})

	exec, state, clk := &fakeExec{}, newMemState(), newClock()
	return &testEnv{
		Env: &dr.Env{
			Context: "ctx", Client: client, Exec: exec, State: state,
			Now: clk.Now, Sleep: clk.Sleep, Logf: func(string, ...any) {},
		},
		exec: exec, state: state, clock: clk, client: client, reachable: reachable,
	}
}

type simpleError string

func (e simpleError) Error() string { return string(e) }

const errConnectionRefused = simpleError("connection refused")

func (e *testEnv) setReachable(v bool) { e.reachable.Store(v) }

func plan(kind string, params map[string]string) *dr.Plan {
	return &dr.Plan{Version: dr.PlanVersion, Name: "test", Context: "ctx", Provider: &dr.Provider{Kind: kind, Params: params}}
}

// steps builds the power steps for a kind and fails the test on error.
func (e *testEnv) steps(t *testing.T, p *dr.Plan, dir dr.Direction) []dr.Step {
	t.Helper()
	steps, err := NewPlanner(Options{}).Steps(context.Background(), e.Env, p, dir, dr.Scope{})
	if err != nil {
		t.Fatalf("Steps(%s): %v", dir, err)
	}
	return steps
}

func ids(steps []dr.Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Action.ID()
	}
	return out
}

func equivalents(steps []dr.Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Action.Equivalent()
	}
	return out
}

func node(name, pool string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if pool != "" {
		n.Labels[gkeNodeLabel] = pool
	}
	return n
}
