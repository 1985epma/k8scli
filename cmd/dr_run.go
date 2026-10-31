package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/k8sops"
	"github.com/1985epma/k8scli/internal/dr/provider"
	"github.com/1985epma/k8scli/internal/dr/wire"
	"github.com/1985epma/k8scli/internal/output"
)

// runFlags are the flags shared by down, up, resume, nodes and cluster.
type runFlags struct {
	plan      string
	scope     []string
	apps      []string
	databases []string
	execute   bool
	confirm   string
	resume    string
	// skipUnknown (up only) leaves out workloads whose replica count was never recorded.
	skipUnknown bool
}

func (f *runFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.plan, "plan", defaultPlanFile, "plan file")
	cmd.Flags().StringSliceVar(&f.scope, "scope", nil, "only these phases: prepare, apps, databases, nodes, power (comma separated)")
	cmd.Flags().StringSliceVar(&f.apps, "app", nil, "only this app (repeatable)")
	cmd.Flags().StringSliceVar(&f.databases, "database", nil, "only this database (repeatable)")
	cmd.Flags().BoolVar(&f.execute, "execute", false, "run for real; without it the command only shows what it would do")
	cmd.Flags().StringVar(&f.confirm, "confirm", "", "the confirmation phrase, for scripts (a terminal prompts for it)")
}

// addUp adds the flags that only make sense when starting.
func (f *runFlags) addUp(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.skipUnknown, "skip-unknown-replicas", false,
		"leave out workloads that are stopped with no recorded replica count (they stay stopped) instead of failing")
}

// runVariant tunes runDR for the nodes and cluster commands.
type runVariant struct {
	// plan replaces loading --plan (the nodes commands work without a plan file).
	plan *dr.Plan
	// k8s are the node-planner options (nodes to drain, what may be evicted).
	k8s *k8sops.Options
	// banner is printed to stderr before anything runs.
	banner string
}

func newDRRunCmd(app *App, dir dr.Direction) *cobra.Command {
	f := &runFlags{}
	use, short, long := "down", "Stop applications, databases, nodes and machines in order", downLong
	example := "  k8scli dr down --plan k8scli-dr.yaml                          # dry run\n" +
		"  k8scli dr down --plan k8scli-dr.yaml --scope apps --execute   # only the applications\n" +
		"  k8scli dr down --plan k8scli-dr.yaml --execute --confirm 'stop my-context'"
	if dir == dr.DirUp {
		use, short, long = "up", "Start machines, nodes, databases and applications in order", upLong
		example = "  k8scli dr up --plan k8scli-dr.yaml             # dry run\n  k8scli dr up --plan k8scli-dr.yaml --execute"
	}
	cmd := &cobra.Command{
		Use: use, Short: short, Long: long, Example: example, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDR(cmd, app, dir, f, runVariant{})
		},
	}
	f.add(cmd)
	if dir == dr.DirUp {
		f.addUp(cmd)
	}
	cmd.Flags().StringVar(&f.resume, "resume", "", "continue the run with this id from the first step that is not done")
	app.addOutputFlag(cmd)
	return cmd
}

const downLong = "Stops what the plan lists, in this order: CronJobs, applications (highest tier first), databases, nodes " +
	"(cordon and drain) and, when the plan has a provider, the machines.\n\n" +
	"Without --execute it is a dry run: it prints every step with its risk and the equivalent command, and changes nothing. " +
	"With --execute it runs; the replica counts it learns are saved so `dr up` restores exactly them. A run that stops can be " +
	"continued with --resume ID. Ctrl-C aborts cleanly."

const upLong = "Starts what `dr down` stopped, in the reverse order: machines, nodes, databases, then applications (lowest tier first), " +
	"waiting for each tier to be healthy before the next.\n\n" +
	"Without --execute it is a dry run. Powering machines on happens before the Kubernetes API exists, so those steps never " +
	"need it; the later phases are worked out once the cluster answers."

// runDR is the one code path behind down, up, resume, nodes and cluster.
func runDR(cmd *cobra.Command, app *App, dir dr.Direction, f *runFlags, v runVariant) error {
	plan := v.plan
	if plan == nil {
		loaded, err := loadDRPlan(f.plan)
		if err != nil {
			return err
		}
		plan = loaded
	}
	scope, err := buildScope(f.scope, f.apps, f.databases)
	if err != nil {
		return err
	}
	if err := plan.ValidateScope(scope); err != nil {
		return err
	}
	stack, err := drStack(app)
	if err != nil {
		return err
	}
	k8s := stackK8sOptions(v)
	k8s.SkipUnknownReplicas = k8s.SkipUnknownReplicas || f.skipUnknown

	if v.banner != "" {
		fmt.Fprintln(app.ErrOut, v.banner)
	}
	if !f.execute {
		return dryRunDR(cmd, app, stack, plan, dir, scope, k8s, f)
	}
	return executeDR(cmd, app, stack, plan, dir, scope, k8s, f)
}

func stackK8sOptions(v runVariant) k8sops.Options {
	if v.k8s != nil {
		return *v.k8s
	}
	return k8sops.Options{}
}

func dryRunDR(cmd *cobra.Command, app *App, stack *wire.Stack, plan *dr.Plan, dir dr.Direction, scope dr.Scope, k8s k8sops.Options, f *runFlags) error {
	format, err := app.outputFormat()
	if err != nil {
		return err
	}
	res, err := stack.RunWithOptions(cmd.Context(), plan, dir, scope, k8s, dr.RunOptions{DryRun: true})
	if err != nil {
		return previewError(err, dir)
	}
	if format.Structured() {
		return output.WriteStructured(app.Out, format, res)
	}
	writeSteps(app.Out, fmt.Sprintf("Dry run of %q for plan %s (context %s)", dir, plan.Name, plan.Context), res)
	fmt.Fprintf(app.Out, "\nThis was a dry run: nothing changed. To run it for real:\n  %s\n", executeCommand(stack, plan, dir, scope, res, f, ""))
	return nil
}

func executeDR(cmd *cobra.Command, app *App, stack *wire.Stack, plan *dr.Plan, dir dr.Direction, scope dr.Scope, k8s k8sops.Options, f *runFlags) error {
	ctx := cmd.Context()

	// Show what is about to happen, then ask. The dry run also catches a bad plan before anything runs.
	preview, err := stack.RunWithOptions(ctx, plan, dir, scope, k8s, dr.RunOptions{DryRun: true})
	if err != nil {
		return previewError(err, dir)
	}
	writeSteps(app.ErrOut, fmt.Sprintf("About to run %q for plan %s (context %s)", dir, plan.Name, plan.Context), preview)

	phrase := requiredPhrase(stack, plan, dir, scope, preview)
	confirm, err := app.confirmPhrase(phrase, f.confirm, "This changes the cluster.")
	if err != nil {
		return err
	}

	res, runErr := stack.RunWithOptions(ctx, plan, dir, scope, k8s, dr.RunOptions{
		Confirm: confirm, Resume: f.resume, OnEvent: eventPrinter(app.ErrOut),
	})
	if res != nil {
		fmt.Fprintln(app.Out)
		writeRunDetail(app.Out, res)
	}
	if runErr == nil {
		return nil
	}

	resume := ""
	if res != nil {
		resume = executeCommand(stack, plan, dir, scope, nil, f, res.ID)
	}
	switch {
	case errors.Is(runErr, provider.ErrManualStep):
		fmt.Fprintf(app.ErrOut, "\nThe run is waiting for you. Do the step above, then continue with:\n  %s\n", resume)
	case res != nil && res.Status == dr.StatusAborted:
		fmt.Fprintf(app.ErrOut, "\nThe run was aborted; nothing is left half done. Continue with:\n  %s\n", resume)
	case res != nil:
		fmt.Fprintf(app.ErrOut, "\nThe run stopped. Fix the problem, then continue with:\n  %s\n", resume)
	}
	cmd.SilenceUsage = true
	return runErr
}

// previewError explains a plan that cannot be worked out before anything ran: nothing changed yet, and
// how to narrow the run.
func previewError(err error, dir dr.Direction) error {
	hint := "Nothing was changed. You can narrow the run with --scope, --app or --database."
	if dir == dr.DirUp {
		hint += " To leave workloads with no recorded replica count out of the run, pass --skip-unknown-replicas."
	}
	return fmt.Errorf("%w\n%s", err, hint)
}

// requiredPhrase is what has to be typed: the engine's phrase, or "stop <context>" as soon as a destructive
// step is going to run (the engine demands it for those whatever the plan says).
func requiredPhrase(stack *wire.Stack, plan *dr.Plan, dir dr.Direction, scope dr.Scope, preview *dr.RunResult) string {
	for _, step := range preview.Steps {
		if step.Risk == dr.RiskDestructive.String() && step.Status == dr.StepPending {
			return "stop " + plan.Context
		}
	}
	return stack.ConfirmPhrase(plan, dir, scope)
}

// executeCommand renders the command that runs (or resumes) the same thing for real.
func executeCommand(stack *wire.Stack, plan *dr.Plan, dir dr.Direction, scope dr.Scope, preview *dr.RunResult, f *runFlags, resumeID string) string {
	parts := []string{"k8scli", "dr", string(dir)}
	if f.plan != "" && f.plan != defaultPlanFile {
		parts = append(parts, "--plan", shellWord(f.plan))
	} else {
		parts = append(parts, "--plan", defaultPlanFile)
	}
	if flags := scopeFlags(scope); flags != "" {
		parts = append(parts, flags)
	}
	if resumeID != "" {
		parts = append(parts, "--resume", shellWord(resumeID))
	}
	parts = append(parts, "--execute")
	phrase := stack.ConfirmPhrase(plan, dir, scope)
	if preview != nil {
		phrase = requiredPhrase(stack, plan, dir, scope, preview)
	}
	if phrase != "" {
		parts = append(parts, "--confirm", shellWord(phrase))
	}
	return strings.Join(parts, " ")
}

// ---- output ------------------------------------------------------------------------------------------------

func stepStatusText(s dr.StepStatus) string {
	switch s {
	case dr.StepPending:
		return "will run"
	case dr.StepSkipped:
		return "already done"
	case dr.StepDeferred:
		return "computed when the cluster is reachable"
	default:
		return string(s)
	}
}

// writeSteps prints the steps of a (dry) run with what each one does and the command a person would run.
func writeSteps(w io.Writer, title string, res *dr.RunResult) {
	fmt.Fprintf(w, "%s: %d steps\n", title, len(res.Steps))
	for i, step := range res.Steps {
		fmt.Fprintf(w, "%3d. [%s, %s risk] %s - %s\n", i+1, step.Phase, step.Risk, step.Title, stepStatusText(step.Status))
		if step.Describe != "" {
			fmt.Fprintf(w, "       %s\n", step.Describe)
		}
		if step.Equivalent != "" {
			fmt.Fprintf(w, "       $ %s\n", step.Equivalent)
		}
		if step.Gate != "" {
			fmt.Fprintf(w, "       then wait: %s\n", step.Gate)
		}
	}
}

// writeRunDetail prints one run: header and a table of steps.
func writeRunDetail(w io.Writer, res *dr.RunResult) {
	fmt.Fprintf(w, "Run %s: %s\n", res.ID, res.Status)
	fmt.Fprintf(w, "Plan:       %s (context %s)\n", res.Plan, res.Context)
	fmt.Fprintf(w, "Direction:  %s\n", res.Direction)
	fmt.Fprintf(w, "Actor:      %s\n", orDash(res.Actor))
	fmt.Fprintf(w, "Started:    %s\n", humanTime(res.Started))
	if !res.Finished.IsZero() {
		fmt.Fprintf(w, "Finished:   %s (%s)\n", humanTime(res.Finished), humanDuration(res.Finished.Sub(res.Started)))
	}
	if len(res.Steps) == 0 {
		return
	}
	fmt.Fprintln(w)
	table := output.NewTable(w)
	fmt.Fprintln(table, "PHASE\tSTATUS\tTOOK\tSTEP")
	for _, step := range res.Steps {
		took := "-"
		if !step.Started.IsZero() && !step.Finished.IsZero() {
			took = humanDuration(step.Finished.Sub(step.Started))
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", step.Phase, step.Status, took, step.Title)
	}
	_ = table.Flush()
	for _, step := range res.Steps {
		if step.Err != "" {
			fmt.Fprintf(w, "\n%s:\n", step.Title)
			for _, line := range strings.Split(strings.TrimSpace(step.Err), "\n") {
				fmt.Fprintf(w, "  %s\n", line)
			}
		}
	}
}

// ---- resume, status, runs ----------------------------------------------------------------------------------

func newDRResumeCmd(app *App) *cobra.Command {
	f := &runFlags{}
	cmd := &cobra.Command{
		Use:   "resume ID",
		Short: "Continue a run that stopped, failed or is waiting for you",
		Long: "Shows where a stored run stands and, with --execute, continues it from the first step that is not done. " +
			"Steps that already finished are not repeated. Pass the same --plan and scope the run used.",
		Example: "  k8scli dr resume 20261031T101500-ab12                  # shows what is left\n" +
			"  k8scli dr resume 20261031T101500-ab12 --execute",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stack, err := drStack(app)
			if err != nil {
				return err
			}
			res, found, err := stack.Status(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("no stored run %q (see: k8scli dr runs)", args[0])
			}
			if !f.execute {
				writeRunDetail(app.Out, res)
				fmt.Fprintf(app.Out, "\nTo continue it:\n  k8scli dr resume %s --plan %s --execute\n", res.ID, shellWord(f.plan))
				return nil
			}
			f.resume = res.ID
			return runDR(cmd, app, res.Direction, f, runVariant{})
		},
	}
	f.add(cmd)
	f.addUp(cmd)
	return cmd
}

func newDRStatusCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "status [ID]",
		Short:   "Show a run: its steps, how long they took and what failed (the latest run by default)",
		Example: "  k8scli dr status\n  k8scli dr status 20261031T101500-ab12 -o json",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := app.outputFormat()
			if err != nil {
				return err
			}
			stack, err := drStack(app)
			if err != nil {
				return err
			}
			var res *dr.RunResult
			if len(args) == 1 {
				stored, found, err := stack.Status(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if !found {
					return fmt.Errorf("no stored run %q (see: k8scli dr runs)", args[0])
				}
				res = stored
			} else {
				runs, err := stack.List(cmd.Context(), 1)
				if err != nil {
					return err
				}
				if len(runs) == 0 {
					fmt.Fprintln(app.ErrOut, "No runs yet. Start with: k8scli dr down --plan k8scli-dr.yaml")
					return nil
				}
				res = &runs[0]
			}
			if format.Structured() {
				return output.WriteStructured(app.Out, format, res)
			}
			writeRunDetail(app.Out, res)
			return nil
		},
	}
	app.addOutputFlag(cmd)
	return cmd
}

func newDRRunsCmd(app *App) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:     "runs",
		Short:   "List the stored runs, newest first",
		Example: "  k8scli dr runs\n  k8scli dr runs --limit 5 -o json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := app.outputFormat()
			if err != nil {
				return err
			}
			stack, err := drStack(app)
			if err != nil {
				return err
			}
			runs, err := stack.List(cmd.Context(), limit)
			if err != nil {
				return err
			}
			if format.Structured() {
				return output.WriteStructured(app.Out, format, runs)
			}
			if len(runs) == 0 {
				fmt.Fprintln(app.ErrOut, "No runs yet.")
				return nil
			}
			table := output.NewTable(app.Out)
			fmt.Fprintln(table, "ID\tSTARTED\tDIRECTION\tSTATUS\tPLAN\tCONTEXT\tACTOR")
			for _, run := range runs {
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", run.ID, humanTime(run.Started), run.Direction, run.Status, run.Plan, run.Context, orDash(run.Actor))
			}
			return table.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "how many runs to list")
	app.addOutputFlag(cmd)
	return cmd
}

// ---- nodes and cluster -------------------------------------------------------------------------------------

func newDRNodesCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "Cordon and drain nodes, or uncordon the ones a drain cordoned",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newDRNodesActionCmd(app, "drain", dr.DirDown), newDRNodesActionCmd(app, "uncordon", dr.DirUp))
	return cmd
}

func newDRNodesActionCmd(app *App, name string, dir dr.Direction) *cobra.Command {
	f := &runFlags{}
	var (
		contextName string
		nodes       []string
		emptyDir    bool
		force       bool
	)
	short := "Cordon, then drain, worker nodes (control-plane nodes are never touched)"
	long := "Cordons each node and evicts its pods through the Eviction API, honouring PodDisruptionBudgets. DaemonSet and " +
		"mirror pods stay. Pods with emptyDir data or without a controller are refused unless you allow them. The nodes this " +
		"run cordoned are remembered, so `uncordon` restores exactly those."
	if dir == dr.DirUp {
		short = "Uncordon the nodes that a drain cordoned"
		long = "Waits for the nodes to be Ready and uncordons exactly the ones `dr nodes drain` cordoned. Nodes that were " +
			"already cordoned before are left alone."
	}
	cmd := &cobra.Command{
		Use: name, Short: short, Long: long, Args: cobra.NoArgs,
		Example: fmt.Sprintf("  k8scli dr nodes %s --node worker-1                 # dry run\n  k8scli dr nodes %s --node worker-1 --execute", name, name),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var plan *dr.Plan
			if cmd.Flags().Changed("plan") {
				loaded, err := loadDRPlan(f.plan)
				if err != nil {
					return err
				}
				plan = loaded
			} else {
				stack, err := drStack(app)
				if err != nil {
					return err
				}
				contextID, err := resolveContext(stack, contextName, nil)
				if err != nil {
					return err
				}
				plan = &dr.Plan{Version: dr.PlanVersion, Name: "adhoc-nodes", Context: contextID}
			}
			f.scope = []string{string(dr.PhaseNodes)}
			opts := k8sops.Options{Nodes: nodes, AllowEmptyDir: emptyDir, AllowUnmanagedPods: force}
			return runDR(cmd, app, dir, f, runVariant{plan: plan, k8s: &opts})
		},
	}
	f.add(cmd)
	// The scope is fixed to the nodes phase.
	_ = cmd.Flags().MarkHidden("scope")
	cmd.Flags().StringVar(&contextName, "context", "", "kubeconfig context (default: the plan's, then the current one)")
	cmd.Flags().StringSliceVar(&nodes, "node", nil, "node to act on (repeatable); default: every worker node")
	if dir == dr.DirDown {
		cmd.Flags().BoolVar(&emptyDir, "delete-emptydir-data", false, "also evict pods that use emptyDir volumes (their data is lost)")
		cmd.Flags().BoolVar(&force, "force", false, "also evict pods that no controller will recreate")
	}
	app.addOutputFlag(cmd)
	return cmd
}

func newDRClusterCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Stop or start a whole cluster with its plan (applications, databases, nodes and machines)",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newDRClusterActionCmd(app, "stop", dr.DirDown), newDRClusterActionCmd(app, "start", dr.DirUp))
	return cmd
}

func newDRClusterActionCmd(app *App, name string, dir dr.Direction) *cobra.Command {
	f := &runFlags{}
	short := "Stop everything in the plan and power the machines off"
	banner := "Stopping the whole cluster. Machines are powered off when the plan has a provider; starting them again is done from outside the cluster."
	if dir == dr.DirUp {
		short = "Power the machines on and start everything in the plan"
		banner = "Starting the whole cluster: machines first, then nodes, databases and applications."
	}
	cmd := &cobra.Command{
		Use: name, Short: short, Args: cobra.NoArgs,
		Long: short + ". It is `dr " + string(dir) + "` over every phase of the plan. Without --execute it is a dry run. " +
			"Powering machines off needs the phrase \"stop <context>\".",
		Example: fmt.Sprintf("  k8scli dr cluster %s --plan k8scli-dr.yaml\n  k8scli dr cluster %s --plan k8scli-dr.yaml --execute", name, name),
		RunE: func(cmd *cobra.Command, _ []string) error {
			plan, err := loadDRPlan(f.plan)
			if err != nil {
				return err
			}
			if plan.Provider == nil && dir == dr.DirDown {
				fmt.Fprintln(app.ErrOut, "Note: this plan has no provider, so machines are not powered off; only the cluster's workloads and nodes stop.")
			}
			return runDR(cmd, app, dir, f, runVariant{plan: plan, banner: banner})
		},
	}
	f.add(cmd)
	if dir == dr.DirUp {
		f.addUp(cmd)
	}
	_ = cmd.Flags().MarkHidden("scope")
	_ = cmd.Flags().MarkHidden("app")
	_ = cmd.Flags().MarkHidden("database")
	cmd.Flags().StringVar(&f.resume, "resume", "", "continue the run with this id")
	app.addOutputFlag(cmd)
	return cmd
}
