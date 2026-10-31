package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/wire"
)

// defaultPlanFile is the plan a command reads when --plan is not given.
const defaultPlanFile = "k8scli-dr.yaml"

// extraDRCommands lets other files add subcommands to `k8scli dr` without editing this one. A file adds
// its command from an init function:
//
//	func init() { extraDRCommands = append(extraDRCommands, newXCmd) }
//
// newDRCmd appends every constructor in it as a subcommand of `dr`.
var extraDRCommands []func(app *App) *cobra.Command

// drStack builds the wired disaster-recovery stack for the command. Tests replace it.
var drStack = func(app *App) (*wire.Stack, error) {
	return wire.New(wire.Options{Kubeconfig: app.Kubeconfig})
}

func newDRCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dr",
		Short: "Disaster recovery: check, stop, start and measure",
		Long: "Plan and rehearse disaster recovery. A plan file (k8scli-dr.yaml) says which applications and databases " +
			"stop and start in which order; `dr down` and `dr up` run it, `dr drill` measures how long recovery takes (RTO) " +
			"and how much data would be lost (RPO).\n\n" +
			"Everything that changes something needs --execute; without it a command only shows what it would do. " +
			"Production contexts and anything that powers machines off ask for a typed confirmation phrase.",
		Example: "  k8scli dr plan validate\n" +
			"  k8scli dr check -n shop\n" +
			"  k8scli dr down --plan k8scli-dr.yaml              # dry run: shows the steps\n" +
			"  k8scli dr down --plan k8scli-dr.yaml --execute    # runs them\n" +
			"  k8scli dr drill --plan k8scli-dr.yaml --execute   # stop, start and measure RTO/RPO",
		Args: cobra.NoArgs,
	}

	cmd.AddCommand(
		newDRPlanCmd(app),
		newDRCheckCmd(app),
		newDRExportCmd(app),
		newDRDiffCmd(app),
		newDRBackupsCmd(app),
		newDRRunCmd(app, dr.DirDown),
		newDRRunCmd(app, dr.DirUp),
		newDRResumeCmd(app),
		newDRStatusCmd(app),
		newDRRunsCmd(app),
		newDRDrillCmd(app),
		newDRReportCmd(app),
		newDRNodesCmd(app),
		newDRClusterCmd(app),
	)
	for _, extra := range extraDRCommands {
		cmd.AddCommand(extra(app))
	}
	return cmd
}

// ---- shared helpers ---------------------------------------------------------------------------------------

// loadDRPlan reads and validates a plan file; every problem is listed in the error.
func loadDRPlan(file string) (*dr.Plan, error) {
	if file == "" {
		file = defaultPlanFile
	}
	plan, err := dr.LoadPlan(file)
	if err != nil {
		return nil, fmt.Errorf("plan %s: %w", file, err)
	}
	return plan, nil
}

// splitList splits a comma-separated flag value, dropping blanks.
func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// allPhases are the names accepted by --scope.
var allPhases = []dr.Phase{dr.PhasePrepare, dr.PhaseApps, dr.PhaseDatabases, dr.PhaseNodes, dr.PhasePower}

// buildScope turns the --scope, --app and --database flags into a dr.Scope.
func buildScope(phases, apps, databases []string) (dr.Scope, error) {
	scope := dr.Scope{Apps: apps, Databases: databases}
	for _, name := range phases {
		var found bool
		for _, phase := range allPhases {
			if string(phase) == strings.ToLower(name) {
				scope.Phases = append(scope.Phases, phase)
				found = true
			}
		}
		if !found {
			names := make([]string, 0, len(allPhases))
			for _, phase := range allPhases {
				names = append(names, string(phase))
			}
			return dr.Scope{}, fmt.Errorf("unknown scope %q (use: %s)", name, strings.Join(names, ", "))
		}
	}
	return scope, nil
}

// scopeFlags renders a scope back into the flags that produce it, for "resume with" hints.
func scopeFlags(scope dr.Scope) string {
	var parts []string
	if len(scope.Phases) > 0 {
		names := make([]string, 0, len(scope.Phases))
		for _, phase := range scope.Phases {
			names = append(names, string(phase))
		}
		parts = append(parts, "--scope "+strings.Join(names, ","))
	}
	for _, name := range scope.Apps {
		parts = append(parts, "--app "+shellWord(name))
	}
	for _, name := range scope.Databases {
		parts = append(parts, "--database "+shellWord(name))
	}
	return strings.Join(parts, " ")
}

// shellWord quotes a value for display only when it needs it.
func shellWord(value string) string {
	plain := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/:=@%+,", r)
	}
	if value != "" && strings.IndexFunc(value, func(r rune) bool { return !plain(r) }) < 0 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// resolveContext picks the context a command acts on: the --context flag, then the plan's, then the
// kubeconfig's current one.
func resolveContext(stack *wire.Stack, flag string, plan *dr.Plan) (string, error) {
	switch {
	case flag != "":
		return flag, nil
	case plan != nil && plan.Context != "":
		return plan.Context, nil
	}
	if current := stack.CurrentContext(); current != "" {
		return current, nil
	}
	return "", errors.New("no context: pass --context, use --plan, or set a current context in the kubeconfig")
}

// confirmPhrase returns the phrase the person typed: --confirm when given, otherwise a line read from the
// input. Without input (a script) it fails and says what to pass.
func (a *App) confirmPhrase(required, given, what string) (string, error) {
	if required == "" {
		return given, nil
	}
	if given != "" {
		return given, nil
	}
	fmt.Fprintf(a.ErrOut, "%s\nType %q to continue: ", what, required)
	line, err := bufio.NewReader(a.In).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(a.ErrOut)
		return "", fmt.Errorf("confirmation required: pass --confirm %s", shellWord(required))
	}
	return strings.TrimSpace(line), nil
}

// humanDuration prints a duration rounded to the second ("3m12s"), or "0s".
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// secondsDuration converts the seconds stored in reports back to a duration.
func secondsDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

// humanTime prints a time compactly, or "-" when it is zero.
func humanTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// eventPrinter streams the progress of a real run to w.
func eventPrinter(w io.Writer) func(dr.Event) {
	return func(e dr.Event) {
		at := e.Time.Local().Format("15:04:05")
		switch e.Kind {
		case "run-start":
			fmt.Fprintf(w, "%s run %s %s\n", at, e.RunID, e.Msg)
		case "step-start":
			fmt.Fprintf(w, "%s -> %s\n", at, e.Msg)
		case "gate-wait":
			fmt.Fprintf(w, "%s    %s\n", at, e.Msg)
		case "gate-ok":
			fmt.Fprintf(w, "%s    ready: %s\n", at, e.Msg)
		case "gate-fail":
			fmt.Fprintf(w, "%s    gate failed: %s: %s\n", at, e.Msg, e.Err)
		case "step-skip":
			fmt.Fprintf(w, "%s    skipped: %s%s\n", at, e.Msg, suffix(e.Err))
		case "step-done":
			fmt.Fprintf(w, "%s    done\n", at)
		case "step-fail":
			fmt.Fprintf(w, "%s    FAILED: %s\n", at, e.Err)
		case "step-manual":
			fmt.Fprintf(w, "%s    WAITING FOR YOU: %s\n", at, e.Msg)
			for _, line := range strings.Split(strings.TrimSpace(e.Err), "\n") {
				fmt.Fprintf(w, "%s      %s\n", at, line)
			}
		case "phase-wait":
			fmt.Fprintf(w, "%s    %s\n", at, e.Msg)
		}
	}
}

func suffix(err string) string {
	if err == "" {
		return ""
	}
	return " (" + err + ")"
}
