package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/1985epma/k8scli/internal/dr/chaos"
	"github.com/1985epma/k8scli/internal/dr/checks"
	"github.com/1985epma/k8scli/internal/dr/extchecks"
	"github.com/1985epma/k8scli/internal/dr/wire"
	"github.com/1985epma/k8scli/internal/output"
)

func init() {
	extraDRCommands = append(extraDRCommands, newDRChaosCmd, newDRExtCheckCmd)
}

// ---- dr chaos -----------------------------------------------------------------------------------------------

type chaosFlags struct {
	plan, watch, confirm      string
	window, poll, maxDuration time.Duration
	execute                   bool
}

func (f *chaosFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.plan, "plan", defaultPlanFile, "plan file; it must say safety.labOnly: true")
	cmd.Flags().StringVar(&f.watch, "watch", "", "comma-separated apps of the plan to observe (default: every app in scope)")
	cmd.Flags().DurationVar(&f.window, "window", chaos.DefaultWindow, "how long to look for a degradation before calling the failure absorbed")
	cmd.Flags().DurationVar(&f.poll, "poll", chaos.DefaultPoll, "how often the apps are checked")
	cmd.Flags().DurationVar(&f.maxDuration, "max-duration", chaos.DefaultMaxDuration, "stop observing after this long (at most "+chaos.MaxExperiment.String()+")")
	cmd.Flags().BoolVar(&f.execute, "execute", false, "really inject the failure (without it: a dry run that only shows what would happen)")
	cmd.Flags().StringVar(&f.confirm, "confirm", "", "the typed phrase, needed on production-looking contexts")
}

func newDRChaosCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "chaos",
		Short: "Inject a small, bounded failure and measure how the system reacts",
		Long: "Kills some pods, cordons or drains one worker node, and watches the apps of the plan: did they notice, and how " +
			"long until they were healthy again? The result gives a measured time to detect and time to recover.\n\n" +
			"It is built to be hard to misuse:\n" +
			"  - the plan must say safety.labOnly: true;\n" +
			"  - pods are killed only in namespaces labelled " + chaos.LabelAllowed + "=true;\n" +
			"  - a workload always keeps one ready replica and a PodDisruptionBudget is honored;\n" +
			"  - control-plane nodes are never touched, and a drain refuses to evict pods of namespaces that are not marked;\n" +
			"  - nothing is injected into a system that is already unhealthy;\n" +
			"  - what was cordoned is uncordoned afterwards, even when the run fails or is cancelled.\n\n" +
			"Without --execute every command is a dry run. It is NOT network chaos and it does not power nodes off: that is " +
			"what `k8scli dr drill --full` and the provider adapters are for.",
		Example: "  kubectl label namespace chaos-test " + chaos.LabelAllowed + "=true\n" +
			"  k8scli dr chaos kill-pods -n chaos-test --workload api --plan k8scli-dr.yaml\n" +
			"  k8scli dr chaos kill-pods -n chaos-test --workload api --plan k8scli-dr.yaml --execute\n" +
			"  k8scli dr chaos drain-node --node worker-1 --plan k8scli-dr.yaml --execute",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newChaosKillPodsCmd(app), newChaosNodeCmd(app, wire.ChaosDrainNode), newChaosNodeCmd(app, wire.ChaosCordonNode))
	return cmd
}

func newChaosKillPodsCmd(app *App) *cobra.Command {
	var (
		f                 chaosFlags
		workload, kind    string
		count, maxPercent int
	)
	cmd := &cobra.Command{
		Use:   "kill-pods",
		Short: "Evict a few ready pods of one workload (-n NAMESPACE --workload NAME)",
		Long: "Evicts one or more ready pods of a Deployment or StatefulSet through the Eviction API. The controller " +
			"recreates them. At most --max-percent of the replicas (default 50%) may go, and at least one stays ready.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(app.Namespace) == "" {
				return errors.New("pass the namespace with -n (it must carry the label " + chaos.LabelAllowed + "=true)")
			}
			if workload == "" {
				return errors.New("pass the workload with --workload")
			}
			return runChaos(cmd, app, &f, func() wire.ChaosRequest {
				return wire.ChaosRequest{Kind: wire.ChaosKillPods, Namespace: app.Namespace, WorkloadKind: kind, Workload: workload, Count: count, MaxPercent: maxPercent}
			})
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&workload, "workload", "", "name of the Deployment or StatefulSet")
	cmd.Flags().StringVar(&kind, "kind", "Deployment", "Deployment or StatefulSet")
	cmd.Flags().IntVar(&count, "count", 1, "how many pods to evict")
	cmd.Flags().IntVar(&maxPercent, "max-percent", chaos.DefaultMaxPercent, "most of the replicas that may be evicted at once, in percent")
	app.addOutputFlag(cmd)
	return cmd
}

func newChaosNodeCmd(app *App, kind wire.ChaosKind) *cobra.Command {
	var (
		f            chaosFlags
		node         string
		drainTimeout time.Duration
		otherNS      bool
	)
	use, short := "drain-node", "Cordon and drain one worker node (--node NAME), then uncordon it"
	if kind == wire.ChaosCordonNode {
		use, short = "cordon-node", "Cordon one worker node (--node NAME) so nothing new lands there, then uncordon it"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if node == "" {
				return errors.New("pass the node with --node")
			}
			return runChaos(cmd, app, &f, func() wire.ChaosRequest {
				return wire.ChaosRequest{Kind: kind, Node: node, DrainTimeout: drainTimeout, AllowOtherNamespaces: otherNS}
			})
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&node, "node", "", "name of the worker node")
	if kind == wire.ChaosDrainNode {
		cmd.Flags().DurationVar(&drainTimeout, "drain-timeout", chaos.DefaultDrainTimeout, "how long the drain may take")
		cmd.Flags().BoolVar(&otherNS, "allow-other-namespaces", false, "accept evicting pods of namespaces that are not marked "+chaos.LabelAllowed+" (a drain is node-wide)")
	}
	app.addOutputFlag(cmd)
	return cmd
}

func runChaos(cmd *cobra.Command, app *App, f *chaosFlags, build func() wire.ChaosRequest) error {
	format, err := app.outputFormat()
	if err != nil {
		return err
	}
	plan, err := loadDRPlan(f.plan)
	if err != nil {
		return err
	}
	stack, err := drStack(app)
	if err != nil {
		return err
	}

	req := build()
	req.Plan = plan
	req.Watch = splitList(f.watch)
	req.Window, req.Poll, req.MaxDuration = f.window, f.poll, f.maxDuration
	req.DryRun = !f.execute

	if f.execute {
		what := fmt.Sprintf("Chaos on %s (%s). This injects a real failure.", plan.Context, req.Kind)
		if req.Confirm, err = app.confirmPhrase(stack.ChaosPhrase(plan), f.confirm, what); err != nil {
			return err
		}
		if !format.Structured() {
			req.OnEvent = chaosEventPrinter(app.ErrOut)
		}
	}

	result, runErr := stack.Chaos(cmd.Context(), req)
	if result != nil {
		if format.Structured() {
			if err := output.WriteStructured(app.Out, format, result); err != nil {
				return err
			}
		} else {
			writeChaosResult(app.Out, result)
		}
	}
	if runErr != nil {
		return runErr
	}
	if result != nil && result.Mode == "executed" && !result.Recovered {
		return fmt.Errorf("the system did not recover within %s; the recovery step ran (see the notes above)", humanDuration(f.maxDuration))
	}
	if result != nil && result.Mode == "dry-run" && !format.Structured() {
		fmt.Fprintln(app.ErrOut, "Dry run: nothing was injected. Add --execute to run it.")
	}
	return nil
}

func chaosEventPrinter(w io.Writer) func(chaos.Event) {
	return func(e chaos.Event) {
		fmt.Fprintf(w, "%s  %s\n", e.At.Local().Format("15:04:05"), e.Msg)
	}
}

func writeChaosResult(w io.Writer, r *chaos.Result) {
	mode := "dry run"
	if r.Mode == "executed" {
		mode = "executed"
	}
	fmt.Fprintf(w, "Experiment %s on %s (plan %s) [%s]  id %s\n\n", r.Name, r.Context, r.Plan, mode, r.ID)
	if r.Injection != nil {
		fmt.Fprintf(w, "Injection:  %s (risk %s)\n", r.Injection.Title, r.Injection.Risk)
		fmt.Fprintf(w, "            %s\n", r.Injection.Describe)
		if r.Injection.Equivalent != "" {
			fmt.Fprintf(w, "            equivalent: %s\n", r.Injection.Equivalent)
		}
	}
	if r.Recovery != nil {
		fmt.Fprintf(w, "Recovery:   %s (runs automatically afterwards)\n", r.Recovery.Title)
	} else {
		fmt.Fprintln(w, "Recovery:   nothing to undo; the controller recreates what was removed")
	}
	fmt.Fprintf(w, "Watching:   %s\n", strings.Join(r.Probes, ", "))
	steady := "yes"
	if !r.SteadyBefore {
		steady = "NO (an injection would be refused)"
	}
	fmt.Fprintf(w, "Healthy before: %s\n", steady)

	if r.Mode == "executed" {
		fmt.Fprintln(w)
		switch {
		case r.Absorbed:
			fmt.Fprintln(w, "Result:     the system absorbed the failure: no degradation was seen")
		case r.Recovered:
			fmt.Fprintln(w, "Result:     the system degraded and recovered")
		default:
			fmt.Fprintln(w, "Result:     the system did NOT recover in time")
		}
		if r.TimeToDetect != nil {
			fmt.Fprintf(w, "Time to detect:   %s (measured)\n", r.TimeToDetect.Human)
		}
		if r.TimeToRecover != nil {
			line := fmt.Sprintf("Time to recover:  %s (measured)", r.TimeToRecover.Human)
			if r.TimeToRecover.Target != "" {
				line += fmt.Sprintf(", target %s: %s", r.TimeToRecover.Target, r.TimeToRecover.Verdict)
			}
			fmt.Fprintln(w, line)
		}
		if r.RecoveryRun != nil {
			if r.RecoveryRun.Applied {
				fmt.Fprintf(w, "Recovery step:    done in %s\n", humanDuration(secondsDuration(r.RecoveryRun.Seconds)))
			} else {
				fmt.Fprintf(w, "Recovery step:    FAILED: %s\n", r.RecoveryRun.Err)
			}
		}
	}
	for _, note := range r.Notes {
		fmt.Fprintf(w, "Note:       %s\n", note)
	}
	if r.Error != "" {
		fmt.Fprintf(w, "Error:      %s\n", r.Error)
	}
}

// ---- dr extcheck ----------------------------------------------------------------------------------------------

func newDRExtCheckCmd(app *App) *cobra.Command {
	var (
		planFile, contextName, to, minSeverity, failOn string
		expiryDays                                     int
	)
	cmd := &cobra.Command{
		Use:   "extcheck",
		Short: "Check what a restore depends on outside the workloads (read-only)",
		Long: "Looks at the things a restore needs that are easy to forget: the registries the images come from, certificates, " +
			"the Secrets and ConfigMaps the workloads reference, the StorageClasses, IngressClasses and PriorityClasses they use, and " +
			"whether the backups follow the 3-2-1 rule.\n\n" +
			"With a DR context (--to, or drContext in the plan) the references are checked against that cluster; without one, " +
			"against the cluster itself. Secrets and ConfigMaps are checked BY NAME only: their values are never read.\n\n" +
			"These checks compare names and metadata. They show what a restore is likely to miss; only a drill proves it works.",
		Example: "  k8scli dr extcheck --plan k8scli-dr.yaml\n" +
			"  k8scli dr extcheck -n shop --to dr-cluster --expiry-days 14\n" +
			"  k8scli dr extcheck --plan k8scli-dr.yaml --fail-on high    # for CI",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := app.outputFormat()
			if err != nil {
				return err
			}
			failLevel, err := checks.ParseSeverity(failOn)
			if err != nil {
				return err
			}
			plan, err := optionalPlan(cmd, planFile)
			if err != nil {
				return err
			}
			stack, err := drStack(app)
			if err != nil {
				return err
			}
			contextID, err := resolveContext(stack, contextName, plan)
			if err != nil {
				return err
			}
			report, err := stack.ExtChecks(cmd.Context(), plan, contextID, wire.ExtChecksOptions{
				Standby: to, Namespaces: namespacesFor(app, plan), ExpiryDays: expiryDays, MinSeverity: minSeverity,
			})
			if err != nil {
				return err
			}

			if format.Structured() {
				if err := output.WriteStructured(app.Out, format, report); err != nil {
					return err
				}
			} else {
				writeExtCheck(app.Out, report)
			}
			if failOn != "" && report.Summary.Total > 0 {
				if n := countAtOrAbove(report, failLevel); n > 0 {
					return fmt.Errorf("%d finding(s) at or above %s", n, failLevel)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&planFile, "plan", defaultPlanFile, "plan file: its namespaces are checked, its context and drContext are used")
	cmd.Flags().StringVar(&contextName, "context", "", "kubeconfig context to check (default: the plan's, then the current one)")
	cmd.Flags().StringVar(&to, "to", "", "the DR context the references are checked against (default: the plan's drContext)")
	cmd.Flags().IntVar(&expiryDays, "expiry-days", extchecks.DefaultExpiryDays, "report certificates that expire within this many days")
	cmd.Flags().StringVar(&minSeverity, "min-severity", "", "hide findings below this severity: high, medium or low")
	cmd.Flags().StringVar(&failOn, "fail-on", "", "exit with an error when a finding at or above this severity exists (for CI)")
	app.addOutputFlag(cmd)
	return cmd
}

func countAtOrAbove(report *extchecks.Report, level checks.Severity) int {
	rank := func(s checks.Severity) int {
		switch s {
		case checks.SeverityHigh:
			return 3
		case checks.SeverityMedium:
			return 2
		default:
			return 1
		}
	}
	n := 0
	switch {
	case rank(level) <= 1:
		n = report.Summary.Total
	case rank(level) == 2:
		n = report.Summary.High + report.Summary.Medium
	default:
		n = report.Summary.High
	}
	return n
}

func writeExtCheck(w io.Writer, r *extchecks.Report) {
	where := "against itself"
	if r.Standby != "" {
		where = "against the DR cluster " + r.Standby
	}
	fmt.Fprintf(w, "External dependencies of %s, %s\n", r.Context, where)
	fmt.Fprintf(w, "Namespaces: %s\n", strings.Join(r.Namespaces, ", "))
	fmt.Fprintf(w, "Findings:   %d (%d high, %d medium, %d low)\n\n", r.Summary.Total, r.Summary.High, r.Summary.Medium, r.Summary.Low)

	if len(r.Findings) == 0 {
		fmt.Fprintln(w, "No findings at this severity.")
	}
	for _, f := range r.Findings {
		what := f.Kind
		if f.Name != "" {
			what += " " + f.Name
		}
		if f.Namespace != "" {
			what = f.Kind + " " + f.Namespace + "/" + f.Name
		}
		fmt.Fprintf(w, "%-6s %s  %s\n       %s\n", strings.ToUpper(string(f.Severity)), f.Rule, what, f.Message)
		if f.Fix != "" {
			fmt.Fprintf(w, "       fix: %s\n", f.Fix)
		}
	}

	if len(r.Registries) > 0 {
		fmt.Fprintln(w, "\nImage registries")
		table := output.NewTable(w)
		fmt.Fprintln(table, "HOST\tIMAGES\tREGION\tPRIVATE")
		for _, reg := range r.Registries {
			region := reg.Region
			if region == "" {
				region = "-"
			}
			fmt.Fprintf(table, "%s\t%d\t%s\t%v\n", reg.Host, reg.Images, region, reg.Private)
		}
		_ = table.Flush()
	}

	if c := r.Copies; c != nil {
		fmt.Fprintf(w, "\nBackups and the 3-2-1 rule: %d copy(ies), media %s, off-site %s, rule %s\n",
			c.Total, strings.Join(c.Media, " + "), c.OffSite, map[bool]string{true: "met", false: "NOT met"}[c.Met])
		for _, copy := range c.Copies {
			label := copy.Kind
			if copy.Name != "" {
				label += " " + copy.Name
			}
			detail := copy.Provider
			if copy.Bucket != "" {
				detail += " " + copy.Bucket
			}
			if copy.Region != "" {
				detail += " (" + copy.Region + ")"
			}
			fmt.Fprintf(w, "  - %s %s\n", label, strings.TrimSpace(detail))
		}
		fmt.Fprintf(w, "  Immutability: %s\n", c.Immutability)
	}
	for _, n := range r.Notices {
		fmt.Fprintf(w, "\nNotice: %s", n)
	}
	if len(r.Notices) > 0 {
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "\n%s\n", r.Disclaimer)
}
