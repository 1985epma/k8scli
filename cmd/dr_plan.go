package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/engine"
	"github.com/1985epma/k8scli/internal/output"
)

func newDRPlanCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Validate or show a disaster-recovery plan file",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newDRPlanValidateCmd(app), newDRPlanShowCmd(app))
	return cmd
}

func planFileArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return defaultPlanFile
}

func newDRPlanValidateCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "validate [FILE]",
		Short:   "Check a plan file and list every problem in it",
		Example: "  k8scli dr plan validate\n  k8scli dr plan validate prod/k8scli-dr.yaml",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			file := planFileArg(args)
			plan, err := loadDRPlan(file)
			if err != nil {
				return err
			}
			fmt.Fprintf(app.Out, "Plan %q is valid: context %s, %d apps, %d databases, provider %s\n",
				plan.Name, plan.Context, len(plan.Apps), len(plan.Databases), providerName(plan))
			return nil
		},
	}
}

func newDRPlanShowCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "show [FILE]",
		Short:   "Summarise a plan: order of stopping and starting, targets and confirmation phrases",
		Example: "  k8scli dr plan show\n  k8scli dr plan show -o json",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			format, err := app.outputFormat()
			if err != nil {
				return err
			}
			plan, err := loadDRPlan(planFileArg(args))
			if err != nil {
				return err
			}
			if format.Structured() {
				return output.WriteStructured(app.Out, format, plan)
			}
			return writePlanSummary(app.Out, plan)
		},
	}
	app.addOutputFlag(cmd)
	return cmd
}

func providerName(plan *dr.Plan) string {
	if plan.Provider == nil {
		return "none"
	}
	return plan.Provider.Kind
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// writePlanSummary prints what a plan does, in the order it does it.
func writePlanSummary(w io.Writer, plan *dr.Plan) error {
	fmt.Fprintf(w, "Plan:      %s\n", plan.Name)
	fmt.Fprintf(w, "Context:   %s\n", plan.Context)
	fmt.Fprintf(w, "Standby:   %s\n", orDash(plan.DRContext))
	fmt.Fprintf(w, "Targets:   RTO %s, RPO %s\n", orDash(plan.Targets.RTO), orDash(plan.Targets.RPO))
	if plan.Provider != nil {
		fmt.Fprintf(w, "Provider:  %s%s\n", plan.Provider.Kind, paramText(plan.Provider.Params))
	} else {
		fmt.Fprintf(w, "Provider:  none (machines are not powered by k8scli)\n")
	}
	safety := "no (confirmation is required in production-looking contexts)"
	if plan.Safety.LabOnly {
		safety = "yes (drills are allowed)"
	}
	fmt.Fprintf(w, "Lab only:  %s\n", safety)

	for _, dir := range []dr.Direction{dr.DirDown, dr.DirUp} {
		title := "Stopping (dr down)"
		if dir == dr.DirUp {
			title = "Starting (dr up)"
		}
		fmt.Fprintf(w, "\n%s\n", title)
		order := dr.DownOrder
		if dir == dr.DirUp {
			order = dr.UpOrder
		}
		n := 0
		for _, phase := range order {
			detail := phaseDetail(plan, phase, dir)
			if detail == "" {
				continue
			}
			n++
			fmt.Fprintf(w, "  %d. %-10s %s\n", n, phase, detail)
		}
	}

	fmt.Fprintf(w, "\nConfirmation phrases\n")
	rows := []struct {
		label string
		dir   dr.Direction
		scope dr.Scope
	}{
		{"dr down (apps and databases)", dr.DirDown, dr.Scope{Phases: []dr.Phase{dr.PhasePrepare, dr.PhaseApps, dr.PhaseDatabases}}},
		{"dr down (everything)", dr.DirDown, dr.Scope{}},
		{"dr up", dr.DirUp, dr.Scope{}},
	}
	for _, row := range rows {
		phrase := engine.ConfirmPhrase(plan, row.dir, row.scope)
		if phrase == "" {
			phrase = "none (--execute is enough)"
		} else {
			phrase = `"` + phrase + `"`
		}
		fmt.Fprintf(w, "  %-30s %s\n", row.label, phrase)
	}
	return nil
}

func paramText(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// phaseDetail says what a phase does for this plan and direction ("" when it does nothing).
func phaseDetail(plan *dr.Plan, phase dr.Phase, dir dr.Direction) string {
	switch phase {
	case dr.PhasePrepare:
		namespaces := planNamespaces(plan)
		if len(namespaces) == 0 {
			return ""
		}
		if dir == dr.DirDown {
			return "suspend CronJobs in " + strings.Join(namespaces, ", ")
		}
		return "resume the CronJobs that were suspended"
	case dr.PhaseApps:
		apps := plan.AppsInTierOrder(dir)
		if len(apps) == 0 {
			return ""
		}
		items := make([]string, 0, len(apps))
		for _, a := range apps {
			items = append(items, fmt.Sprintf("%s (tier %d, %s)", a.Name, a.Tier, healthText(a.Health)))
		}
		return strings.Join(items, " -> ")
	case dr.PhaseDatabases:
		dbs := plan.DatabasesInTierOrder(dir)
		if len(dbs) == 0 {
			return ""
		}
		items := make([]string, 0, len(dbs))
		for _, d := range dbs {
			items = append(items, fmt.Sprintf("%s (%s, tier %d)", d.Name, d.Kind, d.Tier))
		}
		return strings.Join(items, " -> ")
	case dr.PhaseNodes:
		if plan.Provider == nil {
			return ""
		}
		if dir == dr.DirDown {
			return "cordon and drain the worker nodes"
		}
		return "uncordon the nodes that were cordoned"
	case dr.PhasePower:
		if plan.Provider == nil {
			return ""
		}
		if dir == dr.DirDown {
			return "power off with " + plan.Provider.Kind
		}
		return "power on with " + plan.Provider.Kind + ", then wait for the API"
	}
	return ""
}

func healthText(h *dr.Health) string {
	if h == nil || h.Kind == "" || h.Kind == "pods-ready" {
		return "pods ready"
	}
	return "http " + h.URL
}

// planNamespaces lists the namespaces of the plan's apps and databases, sorted and unique.
func planNamespaces(plan *dr.Plan) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ns string) {
		if ns != "" && !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	for _, a := range plan.Apps {
		add(a.Namespace)
	}
	for _, d := range plan.Databases {
		add(d.Namespace)
	}
	sortStrings(out)
	return out
}
