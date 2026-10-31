package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/1985epma/k8scli/internal/dr"
)

// aksSteps stops or starts a whole AKS cluster (`az aks stop|start`), which also stops its control
// plane. The API comes back a few minutes after `az aks start` returns; the gate waits for it.
func aksSteps(env *dr.Env, plan *dr.Plan, dir dr.Direction) ([]dr.Step, error) {
	p := newParams(KindAKS, plan.Provider.Params)
	p.rejectUnknown("resourceGroup", "name", "subscription")
	group, name, subscription := p.required("resourceGroup"), p.required("name"), p.optional("subscription")
	if p.err != nil {
		return nil, p.err
	}

	base := []string{"--name", name, "--resource-group", group}
	if subscription != "" {
		base = append(base, "--subscription", subscription)
	}
	verb, risk, want := "start", dr.RiskLow, "Running"
	if dir == dr.DirDown {
		verb, risk, want = "stop", dr.RiskDestructive, "Stopped"
	}
	exec := env.Exec

	action := &execAction{
		exec:     exec,
		id:       fmt.Sprintf("power/aks/%s/%s:%s", group, name, verb),
		title:    fmt.Sprintf("%s AKS cluster %s", strings.ToUpper(verb[:1])+verb[1:], name),
		describe: fmt.Sprintf("Runs `az aks %s` for %s in the resource group %s; this takes several minutes.", verb, name, group),
		risk:     risk,
		commands: []command{{"az", append([]string{"aks", verb}, base...)}},
		done: func(ctx context.Context) (bool, error) {
			args := append([]string{"aks", "show"}, base...)
			args = append(args, "--query", "powerState.code", "-o", "tsv")
			out, err := exec.Run(ctx, "az", args...)
			if err != nil {
				return false, fmt.Errorf("reading the power state of %s: %w", name, err)
			}
			state := strings.TrimSpace(out)
			switch state {
			case "Running", "Stopped":
				return state == want, nil
			default:
				return false, fmt.Errorf("AKS cluster %s reported the power state %q", name, state)
			}
		},
	}
	return []dr.Step{powerStep(action)}, nil
}
