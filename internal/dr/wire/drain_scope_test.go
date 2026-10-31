package wire_test

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/wire"
	"github.com/1985epma/k8scli/internal/dr/wire/wiretest"
)

func drainDescription(t *testing.T, scope dr.Scope) string {
	t.Helper()
	worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}}
	c := wiretest.NewCluster(wiretest.Deployment("shop", "api", 2), worker)
	stack := wiretest.NewStack(t, wire.Options{}, map[string]*wiretest.Cluster{"dev": c}, nil)

	p := plan()
	p.Provider = &dr.Provider{Kind: "docker-desktop"} // a manual power step: no external command needed
	res, err := stack.Run(context.Background(), p, dr.DirDown, scope, dr.RunOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range res.Steps {
		if strings.HasSuffix(step.ID, ":drain") {
			return step.Describe
		}
	}
	t.Fatalf("no drain step in %+v", res.Steps)
	return ""
}

// The stack, not the caller, tells the nodes planner that this shutdown goes on to power the machines off.
func TestAShutdownThatReachesPowerDrainsOnlyThePlansNamespaces(t *testing.T) {
	got := drainDescription(t, dr.Scope{})
	if !strings.Contains(got, "namespaces the plan manages (shop)") || !strings.Contains(got, "stop with the node") {
		t.Errorf("Describe() = %q", got)
	}
}

func TestARunWithoutThePowerPhaseKeepsTheFullDrain(t *testing.T) {
	got := drainDescription(t, dr.Scope{Phases: []dr.Phase{dr.PhaseApps, dr.PhaseDatabases, dr.PhaseNodes}})
	if strings.Contains(got, "namespaces the plan manages") {
		t.Errorf("Describe() = %q; without power every namespace is drained as before", got)
	}
}
