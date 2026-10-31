package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/measure"
	"github.com/1985epma/k8scli/internal/dr/wire"
	"github.com/1985epma/k8scli/internal/output"
)

func newDRDrillCmd(app *App) *cobra.Command {
	f := &runFlags{}
	var full, allowProduction bool
	cmd := &cobra.Command{
		Use:   "drill",
		Short: "Rehearse recovery: stop, start and measure how long it takes (RTO) and how much data is at risk (RPO)",
		Long: "A drill declares a disaster (T0), runs `dr down`, then `dr up`, and watches every application's health until it " +
			"is back. RTO is the time from T0 to each application being healthy; RPO comes from backups and database " +
			"status. Every figure in the report is labelled measured or estimated.\n\n" +
			"By default it stops and starts applications and databases only. --full includes nodes and machines. " +
			"Without --execute it shows what it would do. A real drill is allowed on plans marked safety.labOnly; " +
			"anything else needs --allow-production and the confirmation phrase.\n\n" +
			"A drill always tries to bring everything back up, even when stopping failed or Ctrl-C was pressed.\n\n" +
			"What it proves: that the plan stops and starts in order, and how long that takes. It does NOT restore data from " +
			"a backup, so it does not prove that a restore works; the RPO it reports comes from backup and database metadata " +
			"(estimated) unless a heartbeat measured it.",
		Example: "  k8scli dr drill --plan lab.yaml                 # dry run\n" +
			"  k8scli dr drill --plan lab.yaml --execute\n" +
			"  k8scli dr drill --plan lab.yaml --full --execute --confirm 'stop my-lab'",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			plan, err := loadDRPlan(f.plan)
			if err != nil {
				return err
			}
			scope, err := buildScope(f.scope, f.apps, f.databases)
			if err != nil {
				return err
			}
			// The same default as measure.Drill, so the dry run shows what the drill will really do.
			shown := scope
			if !full && len(shown.Phases) == 0 {
				shown.Phases = []dr.Phase{dr.PhaseApps, dr.PhaseDatabases}
			}
			if err := plan.ValidateScope(shown); err != nil {
				return err
			}
			stack, err := drStack(app)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			down, err := stack.Run(ctx, plan, dr.DirDown, shown, dr.RunOptions{DryRun: true})
			if err != nil {
				return err
			}
			allowed := plan.Safety.LabOnly || allowProduction

			if !f.execute {
				writeSteps(app.Out, fmt.Sprintf("Drill, stopping phase, for plan %s (context %s)", plan.Name, plan.Context), down)
				if up, upErr := stack.Run(ctx, plan, dr.DirUp, shown, dr.RunOptions{DryRun: true}); upErr == nil {
					fmt.Fprintln(app.Out)
					writeSteps(app.Out, "Drill, starting phase", up)
				}
				fmt.Fprintln(app.Out)
				if plan.Safety.LabOnly {
					fmt.Fprintln(app.Out, "Safety: the plan is marked lab only, so a drill is allowed.")
				} else {
					fmt.Fprintln(app.Out, "Safety: the plan is NOT marked safety.labOnly. A real drill needs --allow-production and the confirmation phrase.")
				}
				fmt.Fprintf(app.Out, "\nThis was a dry run: nothing changed. To run the drill for real:\n  %s\n", drillCommand(stack, plan, shown, down, f, full, allowProduction))
				return nil
			}

			if !allowed {
				return fmt.Errorf("plan %q is not marked safety.labOnly: mark it as a lab, or pass --allow-production (and the confirmation phrase)", plan.Name)
			}
			phrase := requiredPhrase(stack, plan, dr.DirDown, shown, down)
			if phrase == "" && !plan.Safety.LabOnly {
				phrase = plan.Context // measure demands a phrase for plans that are not labs
			}
			writeSteps(app.ErrOut, fmt.Sprintf("About to run a drill for plan %s (context %s)", plan.Name, plan.Context), down)
			confirm, err := app.confirmPhrase(phrase, f.confirm, "A drill stops and starts the system on purpose.")
			if err != nil {
				return err
			}

			report, runErr := stack.Drill(ctx, plan, measure.DrillOptions{
				AllowProduction: allowProduction, Confirm: confirm, Full: full, Scope: scope,
				OnEvent: eventPrinter(app.ErrOut),
			})
			if report != nil {
				fmt.Fprintln(app.Out)
				writeDrillSummary(app.Out, report)
				jsonPath, mdPath := stack.ReportPaths(report.ID)
				fmt.Fprintf(app.Out, "\nReport saved:\n  %s\n  %s\nRead it again with: k8scli dr report %s\n", mdPath, jsonPath, report.ID)
			}
			if runErr != nil {
				cmd.SilenceUsage = true
			}
			return runErr
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&full, "full", false, "also stop and start nodes and machines (everything the plan has)")
	cmd.Flags().BoolVar(&allowProduction, "allow-production", false, "allow a drill on a plan that is not marked safety.labOnly")
	return cmd
}

func drillCommand(stack *wire.Stack, plan *dr.Plan, scope dr.Scope, down *dr.RunResult, f *runFlags, full, allowProduction bool) string {
	parts := []string{"k8scli", "dr", "drill", "--plan", shellWord(orDefault(f.plan, defaultPlanFile))}
	if full {
		parts = append(parts, "--full")
	}
	if allowProduction {
		parts = append(parts, "--allow-production")
	}
	for _, name := range f.apps {
		parts = append(parts, "--app", shellWord(name))
	}
	for _, name := range f.databases {
		parts = append(parts, "--database", shellWord(name))
	}
	parts = append(parts, "--execute")
	phrase := stack.ConfirmPhrase(plan, dr.DirDown, scope)
	for _, step := range down.Steps {
		if step.Risk == dr.RiskDestructive.String() && step.Status == dr.StepPending {
			phrase = "stop " + plan.Context
		}
	}
	if phrase == "" && !plan.Safety.LabOnly {
		phrase = plan.Context
	}
	if phrase != "" {
		parts = append(parts, "--confirm", shellWord(phrase))
	}
	return strings.Join(parts, " ")
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// writeDrillSummary prints the outcome of a drill compactly; the full report is on disk.
func writeDrillSummary(w io.Writer, r *measure.Report) {
	fmt.Fprintf(w, "Drill %s: %s (plan %s, context %s, %s)\n", r.ID, r.Outcome, r.Plan, r.Context, r.Mode)
	fmt.Fprintf(w, "  RTO %s (%s) target %s: %s\n", r.RTO.Human, r.RTO.Kind, orDash(r.RTO.Target), r.RTO.Verdict)
	if r.RPO != nil {
		fmt.Fprintf(w, "  RPO %s (%s) target %s: %s\n", r.RPO.Human, r.RPO.Kind, orDash(r.RPO.Target), r.RPO.Verdict)
	} else {
		fmt.Fprintln(w, "  RPO: no figure (no backup or database status was found)")
	}
	fmt.Fprintf(w, "  Overall verdict: %s. State after the drill matches the baseline: %v\n", r.Verdict, r.StateRestored)
	if len(r.Apps) > 0 {
		fmt.Fprintln(w)
		table := output.NewTable(w)
		fmt.Fprintln(table, "APP\tNAMESPACE\tTIER\tRECOVERED\tRTO\tTARGET\tVERDICT")
		for _, a := range r.Apps {
			fmt.Fprintf(table, "%s\t%s\t%d\t%v\t%s\t%s\t%s\n", a.Name, a.Namespace, a.Tier, a.Recovered, a.RTO.Human, orDash(a.RTO.Target), a.Verdict)
		}
		_ = table.Flush()
	}
	for _, fig := range r.Figures {
		fmt.Fprintf(w, "  RPO figure: %s %s = %s (%s, %s)\n", fig.Subject, orDash(fig.Namespace), fig.Human, fig.Kind, fig.Source)
	}
	for _, line := range r.Failures {
		fmt.Fprintf(w, "  FAILURE: %s\n", line)
	}
	for _, line := range r.Notices {
		fmt.Fprintf(w, "  Note: %s\n", line)
	}
}

// ---- dr report ----------------------------------------------------------------------------------------------

func newDRReportCmd(app *App) *cobra.Command {
	var (
		list   bool
		format string
		limit  int
		trend  int
	)
	cmd := &cobra.Command{
		Use:   "report [ID]",
		Short: "Show a drill report (the latest by default), or the history of drills",
		Long: "Prints a stored drill report as Markdown (for people and audits), JSON, or Prometheus text. " +
			"With --list it shows the history and the RTO trend of each application over the last drills.",
		Example: "  k8scli dr report                       # the latest, as Markdown\n" +
			"  k8scli dr report --format json\n" +
			"  k8scli dr report --list --trend 5\n" +
			"  k8scli dr report --format prometheus > /var/lib/node_exporter/dr.prom",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			stack, err := drStack(app)
			if err != nil {
				return err
			}
			if list {
				return writeReportList(app, stack.Reports, limit, trend)
			}
			var report *measure.Report
			if len(args) == 1 {
				report, err = stack.Report(args[0])
				if err != nil {
					return fmt.Errorf("cannot read report %q: %w", args[0], err)
				}
			} else {
				reports, err := stack.Reports(1)
				if err != nil {
					return err
				}
				if len(reports) == 0 {
					fmt.Fprintln(app.ErrOut, "No drill reports yet. Run: k8scli dr drill --plan k8scli-dr.yaml --execute")
					return nil
				}
				report = reports[0]
			}
			switch format {
			case "md", "markdown", "":
				_, err = io.WriteString(app.Out, report.Markdown())
			case "json":
				var data []byte
				if data, err = report.JSON(); err == nil {
					_, err = app.Out.Write(data)
				}
			case "prometheus", "prom":
				_, err = io.WriteString(app.Out, measure.PrometheusText(report))
			default:
				err = fmt.Errorf("unknown format %q (use md, json or prometheus)", format)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&list, "list", false, "list the stored reports and the RTO trend")
	cmd.Flags().StringVar(&format, "format", "md", "md, json or prometheus")
	cmd.Flags().IntVar(&limit, "limit", 20, "with --list: how many reports")
	cmd.Flags().IntVar(&trend, "trend", 5, "with --list: how many drills the RTO trend covers")
	return cmd
}

func writeReportList(app *App, reports func(int) ([]*measure.Report, error), limit, trend int) error {
	all, err := reports(limit)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(app.ErrOut, "No drill reports yet.")
		return nil
	}
	table := output.NewTable(app.Out)
	fmt.Fprintln(table, "ID\tSTARTED\tPLAN\tMODE\tOUTCOME\tRTO\tRTO VERDICT\tRPO")
	for _, r := range all {
		rpo := "-"
		if r.RPO != nil {
			rpo = r.RPO.Human
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, humanTime(r.Started), r.Plan, r.Mode, r.Outcome, r.RTO.Human, r.RTO.Verdict, rpo)
	}
	if err := table.Flush(); err != nil {
		return err
	}

	// RTO trend of every application in the newest report, oldest drill first.
	latest := all[0]
	fmt.Fprintf(app.Out, "\nRTO trend, last %d drills (oldest first)\n", trend)
	names := []string{measure.OverallApp}
	for _, a := range latest.Apps {
		names = append(names, a.Name)
	}
	for _, name := range names {
		points := measure.RTOTrend(all, name, trend)
		if len(points) == 0 {
			continue
		}
		values := make([]string, 0, len(points))
		for _, p := range points {
			mark := ""
			if !p.Recovered {
				mark = " (not recovered)"
			}
			values = append(values, humanDuration(secondsDuration(p.Seconds))+mark)
		}
		label := name
		if name == measure.OverallApp {
			label = "overall"
		}
		fmt.Fprintf(app.Out, "  %-20s %s\n", label, strings.Join(values, ", "))
	}
	return nil
}
