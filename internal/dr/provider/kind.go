package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/1985epma/k8scli/internal/dr"
)

const kindClusterLabel = "io.x-k8s.kind.cluster"

// kind node roles in the order they must STOP; they start in the reverse order.
const (
	roleWorker = iota
	roleControlPlane
	roleLoadBalancer
)

// kindSteps powers the containers of one kind cluster. Down stops workers, then control planes, then
// the external load balancer; up starts them in the opposite order so the API is there before the
// workers try to join.
func kindSteps(ctx context.Context, env *dr.Env, plan *dr.Plan, dir dr.Direction) ([]dr.Step, error) {
	p := newParams(KindKind, plan.Provider.Params)
	p.rejectUnknown("name")
	name := p.optional("name")
	if p.err != nil {
		return nil, p.err
	}
	if name == "" {
		name = strings.TrimPrefix(plan.Context, "kind-")
		if name == "" || name == plan.Context {
			return nil, fmt.Errorf("provider kind: set the parameter %q (the context %q does not start with \"kind-\")", "name", plan.Context)
		}
	}
	if _, err := validIdent("name", name); err != nil {
		return nil, fmt.Errorf("provider kind: %w", err)
	}

	// `docker ps -a` works while the cluster is off, which is exactly when "up" needs it.
	out, err := env.Exec.Run(ctx, "docker", "ps", "-a", "--filter", "label="+kindClusterLabel+"="+name, "--format", "{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("provider kind: listing the nodes of %q: %w", name, err)
	}
	containers, err := parseKindNodes(name, out)
	if err != nil {
		return nil, err
	}

	// Sort into stop order: workers, control planes, load balancer; names alphabetical inside a role.
	sort.SliceStable(containers, func(i, j int) bool {
		if containers[i].role != containers[j].role {
			return containers[i].role < containers[j].role
		}
		return containers[i].name < containers[j].name
	})
	if dir == dr.DirUp {
		for i, j := 0, len(containers)-1; i < j; i, j = i+1, j-1 {
			containers[i], containers[j] = containers[j], containers[i]
		}
	}

	steps := make([]dr.Step, 0, len(containers))
	for _, node := range containers {
		steps = append(steps, powerStep(kindNodeAction(env, name, node, dir)))
	}
	return steps, nil
}

type kindNode struct {
	name string
	role int
}

func parseKindNodes(cluster, output string) ([]kindNode, error) {
	var nodes []kindNode
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, err := validIdent("container", line); err != nil {
			return nil, fmt.Errorf("provider kind: unexpected container name from docker: %w", err)
		}
		nodes = append(nodes, kindNode{name: line, role: kindRole(cluster, line)})
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("provider kind: no containers found for the kind cluster %q (is the name right? `kind get clusters` lists them)", cluster)
	}
	return nodes, nil
}

// kindRole reads the role from the container name (<cluster>-control-plane, <cluster>-worker2,
// <cluster>-external-load-balancer). Anything unrecognised is treated like a worker.
func kindRole(cluster, container string) int {
	suffix := strings.TrimPrefix(container, cluster+"-")
	switch {
	case strings.HasPrefix(suffix, "external-load-balancer"):
		return roleLoadBalancer
	case strings.HasPrefix(suffix, "control-plane"):
		return roleControlPlane
	default:
		return roleWorker
	}
}

var kindRoleName = map[int]string{roleWorker: "worker", roleControlPlane: "control-plane", roleLoadBalancer: "load balancer"}

func kindNodeAction(env *dr.Env, cluster string, node kindNode, dir dr.Direction) dr.Action {
	verb, risk, wantRunning := "start", dr.RiskLow, true
	if dir == dr.DirDown {
		verb, risk, wantRunning = "stop", dr.RiskDestructive, false
	}
	role := kindRoleName[node.role]
	exec := env.Exec

	return &execAction{
		exec:     exec,
		id:       fmt.Sprintf("power/kind/%s/%s:%s", cluster, node.name, verb),
		title:    fmt.Sprintf("%s kind %s node %s", strings.ToUpper(verb[:1])+verb[1:], role, node.name),
		describe: fmt.Sprintf("Runs `docker %s %s`, the %s container of the kind cluster %s.", verb, node.name, role, cluster),
		risk:     risk,
		commands: []command{{"docker", []string{verb, node.name}}},
		done: func(ctx context.Context) (bool, error) {
			out, err := exec.Run(ctx, "docker", "inspect", "-f", "{{.State.Running}}", node.name)
			if err != nil {
				return false, fmt.Errorf("inspecting %s: %w", node.name, err)
			}
			switch strings.TrimSpace(out) {
			case "true":
				return wantRunning, nil
			case "false":
				return !wantRunning, nil
			default:
				return false, fmt.Errorf("inspecting %s: unexpected answer %q", node.name, strings.TrimSpace(out))
			}
		},
	}
}
