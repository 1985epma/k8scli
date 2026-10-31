//go:build lab

package provider

// Live test: powers a real kind cluster off and on. It is excluded from normal builds.
//
//	K8SCLI_LAB_CLUSTER=dr-prov-test K8SCLI_LAB_KUBECONFIG=/path/to/kubeconfig \
//	  go test -tags lab -run TestLabKindPowerCycle -v -timeout 25m ./internal/dr/provider/
//
// It refuses to touch any cluster other than the two lab clusters.

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/1985epma/k8scli/internal/dr"
)

// recorder wraps an Exec and remembers the mutating docker commands with their time.
type recorder struct {
	inner dr.Exec
	mu    sync.Mutex
	log   []string
}

func (r *recorder) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "docker" && len(args) > 0 && (args[0] == "stop" || args[0] == "start") {
		r.mu.Lock()
		r.log = append(r.log, args[0]+" "+args[len(args)-1])
		r.mu.Unlock()
	}
	return r.inner.Run(ctx, name, args...)
}

func labClient(t *testing.T, kubeconfig, contextName string) kubernetes.Interface {
	t.Helper()
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}
	if kubeconfig == "" {
		rules = clientcmd.NewDefaultClientConfigLoadingRules()
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	config.Timeout = 5 * time.Second // probes must fail fast while the cluster is off
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func readyNodes(ctx context.Context, client kubernetes.Interface) (ready, total int) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0
	}
	for _, n := range nodes.Items {
		total++
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready++
			}
		}
	}
	return ready, total
}

func TestLabKindPowerCycle(t *testing.T) {
	cluster := os.Getenv("K8SCLI_LAB_CLUSTER")
	if cluster != "k8scli-lab" && cluster != "dr-prov-test" {
		t.Skip("set K8SCLI_LAB_CLUSTER to k8scli-lab or dr-prov-test; no other cluster is ever touched")
	}
	ctx := context.Background()
	contextName := "kind-" + cluster
	client := labClient(t, os.Getenv("K8SCLI_LAB_KUBECONFIG"), contextName)

	rec := &recorder{inner: NewOSExec()}
	env := &dr.Env{Context: contextName, Client: client, Exec: rec, State: newMemState(), Logf: t.Logf}
	p := &dr.Plan{Version: dr.PlanVersion, Name: "lab", Context: contextName, Provider: &dr.Provider{Kind: KindKind}}
	planner := NewPlanner(Options{APITimeout: 5 * time.Minute})

	// Starting point: API up and every node Ready.
	if !APIReachable(ctx, client) {
		t.Fatal("the lab cluster is not running at the start; start it first")
	}
	ready, total := readyNodes(ctx, client)
	if total < 2 || ready != total {
		t.Fatalf("nodes ready %d/%d; the test needs a multi-node cluster that is healthy", ready, total)
	}
	t.Logf("start: %d/%d nodes ready", ready, total)

	// ---- power off -------------------------------------------------------------------------------------
	down, err := planner.Steps(ctx, env, p, dr.DirDown, dr.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(down) != total {
		t.Fatalf("%d power-off steps for %d nodes", len(down), total)
	}
	offStart := time.Now()
	for _, s := range down {
		if done, err := s.Action.Done(ctx); err != nil || done {
			t.Fatalf("%s: Done = %v, %v before running", s.Action.ID(), done, err)
		}
		stepStart := time.Now()
		if err := s.Action.Apply(ctx); err != nil {
			t.Fatalf("%s: %v", s.Action.ID(), err)
		}
		if done, err := s.Action.Done(ctx); err != nil || !done {
			t.Fatalf("%s: not done after Apply (%v, %v)", s.Action.ID(), done, err)
		}
		t.Logf("%-60s %s   (%s)", s.Action.ID(), time.Since(stepStart).Round(100*time.Millisecond), s.Action.Equivalent())
	}
	offTotal := time.Since(offStart)

	if APIReachable(ctx, client) {
		t.Error("the API still answers after every node was stopped")
	}
	if got := strings.Join(rec.log, " | "); !orderedStops(rec.log, cluster) {
		t.Errorf("stop order wrong: %s", got)
	}
	t.Logf("power off: %s, API unreachable", offTotal.Round(100*time.Millisecond))

	// ---- idempotency: running the power-off again does nothing ----------------------------------------------
	for _, s := range down {
		if done, _ := s.Action.Done(ctx); !done {
			t.Errorf("%s should report done on a second look", s.Action.ID())
		}
	}

	// ---- power on --------------------------------------------------------------------------------------
	rec.log = nil
	up, err := planner.Steps(ctx, env, p, dr.DirUp, dr.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	onStart := time.Now()
	for i, s := range up {
		if done, err := s.Action.Done(ctx); err != nil || done {
			t.Fatalf("%s: Done = %v, %v before running", s.Action.ID(), done, err)
		}
		stepStart := time.Now()
		if err := s.Action.Apply(ctx); err != nil {
			t.Fatalf("%s: %v", s.Action.ID(), err)
		}
		t.Logf("%-60s %s", s.Action.ID(), time.Since(stepStart).Round(100*time.Millisecond))
		if s.Gate != nil {
			if i != len(up)-1 {
				t.Errorf("gate on step %d of %d; only the last should have one", i+1, len(up))
			}
			gateStart := time.Now()
			if err := s.Gate.Wait(ctx); err != nil {
				t.Fatalf("gate %q: %v", s.Gate.Name(), err)
			}
			t.Logf("gate %q passed after %s", s.Gate.Name(), time.Since(gateStart).Round(100*time.Millisecond))
		}
	}
	apiBack := time.Since(onStart)

	if !orderedStarts(rec.log, cluster) {
		t.Errorf("start order wrong: %s", strings.Join(rec.log, " | "))
	}

	// Nodes rejoin and turn Ready a little after the API is back.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if r, tot := readyNodes(ctx, client); tot == total && r == total {
			break
		}
		if time.Now().After(deadline) {
			r, tot := readyNodes(ctx, client)
			t.Fatalf("nodes did not become Ready again: %d/%d", r, tot)
		}
		time.Sleep(2 * time.Second)
	}
	allReady := time.Since(onStart)

	for _, s := range up {
		if done, err := s.Action.Done(ctx); err != nil || !done {
			t.Errorf("%s: Done = %v, %v after power on", s.Action.ID(), done, err)
		}
	}
	t.Logf("RESULT cluster=%s nodes=%d power-off=%s api-back=%s all-nodes-ready=%s",
		cluster, total, offTotal.Round(100*time.Millisecond), apiBack.Round(100*time.Millisecond), allReady.Round(100*time.Millisecond))
}

// orderedStops: every worker is stopped before the control plane, and the control plane before the balancer.
func orderedStops(log []string, cluster string) bool {
	last := -1
	for _, entry := range log {
		role := kindRole(cluster, strings.TrimPrefix(entry, "stop "))
		if role < last {
			return false
		}
		last = role
	}
	return len(log) > 0
}

// orderedStarts: balancer, then control plane, then workers.
func orderedStarts(log []string, cluster string) bool {
	last := roleLoadBalancer
	for _, entry := range log {
		role := kindRole(cluster, strings.TrimPrefix(entry, "start "))
		if role > last {
			return false
		}
		last = role
	}
	return len(log) > 0
}
