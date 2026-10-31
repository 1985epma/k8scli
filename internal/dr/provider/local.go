package provider

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/1985epma/k8scli/internal/dr"
)

// ---- minikube --------------------------------------------------------------------------------------------

func minikubeSteps(env *dr.Env, plan *dr.Plan, dir dr.Direction) ([]dr.Step, error) {
	p := newParams(KindMinikube, plan.Provider.Params)
	p.rejectUnknown("profile")
	profile := p.optional("profile")
	if p.err != nil {
		return nil, p.err
	}
	if profile == "" {
		// minikube names the kubeconfig context after the profile.
		profile = "minikube"
		if ident.MatchString(plan.Context) {
			profile = plan.Context
		}
	}

	verb, risk, want := "start", dr.RiskLow, "Running"
	if dir == dr.DirDown {
		verb, risk, want = "stop", dr.RiskDestructive, "Stopped"
	}
	exec := env.Exec

	action := &execAction{
		exec:     exec,
		id:       fmt.Sprintf("power/minikube/%s:%s", profile, verb),
		title:    fmt.Sprintf("%s minikube profile %s", strings.ToUpper(verb[:1])+verb[1:], profile),
		describe: fmt.Sprintf("Runs `minikube %s -p %s`.", verb, profile),
		risk:     risk,
		commands: []command{{"minikube", []string{verb, "-p", profile}}},
		done: func(ctx context.Context) (bool, error) {
			// minikube status exits non-zero when the host is stopped but still prints the state.
			out, err := exec.Run(ctx, "minikube", "status", "-p", profile, "--format", "{{.Host}}")
			state := strings.TrimSpace(out)
			switch state {
			case "Running", "Stopped":
				return state == want, nil
			case "Nonexistent":
				return false, fmt.Errorf("the minikube profile %q does not exist", profile)
			}
			if err != nil {
				return false, fmt.Errorf("reading the state of minikube profile %s: %w", profile, err)
			}
			return false, fmt.Errorf("minikube profile %s is in an unexpected state %q", profile, state)
		},
	}
	return []dr.Step{powerStep(action)}, nil
}

// ---- docker-desktop and manual -------------------------------------------------------------------------------

const (
	dockerDesktopOff = "Open Docker Desktop, go to Settings > Kubernetes, turn off \"Enable Kubernetes\" and apply (or quit Docker Desktop)"
	dockerDesktopOn  = "Open Docker Desktop, go to Settings > Kubernetes, turn on \"Enable Kubernetes\" and wait until it is running"
)

// maxInstructions bounds the free text a plan may carry for a manual step.
const maxInstructions = 500

// dockerDesktopSteps: Docker Desktop's Kubernetes has no command-line switch, so a person does it.
func dockerDesktopSteps(env *dr.Env, plan *dr.Plan, dir dr.Direction) ([]dr.Step, error) {
	p := newParams(KindDockerDesktop, plan.Provider.Params)
	p.rejectUnknown()
	if p.err != nil {
		return nil, p.err
	}
	text := dockerDesktopOff
	if dir == dr.DirUp {
		text = dockerDesktopOn
	}
	return []dr.Step{powerStep(newManualAction(env, KindDockerDesktop, dir, text))}, nil
}

// manualSteps: the plan says who powers the machines; the step waits for the API to confirm.
func manualSteps(env *dr.Env, plan *dr.Plan, dir dr.Direction) ([]dr.Step, error) {
	known := newParams(KindManual, plan.Provider.Params)
	known.rejectUnknown("instructions")
	if known.err != nil {
		return nil, known.err
	}
	text := plan.Provider.Params["instructions"]
	if text == "" {
		text = "Power the cluster machines off (workers first, control plane last)"
		if dir == dr.DirUp {
			text = "Power the cluster machines on (control plane first, then workers)"
		}
	}
	if err := validInstructions(text); err != nil {
		return nil, fmt.Errorf("provider manual: %w", err)
	}
	return []dr.Step{powerStep(newManualAction(env, KindManual, dir, text))}, nil
}

// validInstructions accepts one line of printable text: it is shown to people, never run.
func validInstructions(text string) error {
	if len([]rune(text)) > maxInstructions {
		return fmt.Errorf("parameter %q is longer than %d characters", "instructions", maxInstructions)
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			return fmt.Errorf("parameter %q must be a single line of printable text", "instructions")
		}
	}
	return nil
}

// manualAction is a step only a person can do. Apply explains what to do and fails with
// ErrManualStep; the person acts, then `k8scli dr resume` finds Done true (the API went away on
// "down", or came back on "up") and continues.
type manualAction struct {
	env          *dr.Env
	kind         string
	dir          dr.Direction
	instructions string
}

func newManualAction(env *dr.Env, kind string, dir dr.Direction, instructions string) *manualAction {
	return &manualAction{env: env, kind: kind, dir: dir, instructions: instructions}
}

func (a *manualAction) ID() string {
	if a.dir == dr.DirDown {
		return "power/" + a.kind + ":off"
	}
	return "power/" + a.kind + ":on"
}

func (a *manualAction) Title() string {
	if a.dir == dr.DirDown {
		return "Power off the cluster (manual)"
	}
	return "Power on the cluster (manual)"
}

func (a *manualAction) Describe() string { return "You do this step yourself: " + a.instructions + "." }

func (a *manualAction) Risk() dr.Risk {
	if a.dir == dr.DirDown {
		return dr.RiskDestructive
	}
	return dr.RiskLow
}

func (a *manualAction) Equivalent() string { return "" }

// Done checks the API. On "down" the cluster must be gone; a missing client proves nothing, so it
// never counts as done and the instructions are shown.
func (a *manualAction) Done(ctx context.Context) (bool, error) {
	if a.dir == dr.DirDown {
		return a.env.Client != nil && !APIReachable(ctx, a.env.Client), nil
	}
	return APIReachable(ctx, a.env.Client), nil
}

func (a *manualAction) Apply(context.Context) error {
	return fmt.Errorf("%w: %s; then run `k8scli dr resume <id>` to continue", ErrManualStep, a.instructions)
}
