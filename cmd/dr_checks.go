package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/backups"
	"github.com/1985epma/k8scli/internal/dr/checks"
	"github.com/1985epma/k8scli/internal/output"
)

func sortStrings(values []string) { sort.Strings(values) }

// namespacesFor decides which namespaces a read-only command looks at: -n (comma separated), else the
// namespaces of the plan, else none (the check then covers every non-system namespace).
func namespacesFor(app *App, plan *dr.Plan) []string {
	if list := splitList(app.Namespace); len(list) > 0 {
		return list
	}
	if plan != nil {
		return planNamespaces(plan)
	}
	return nil
}

// optionalPlan loads the plan only when --plan was given explicitly.
func optionalPlan(cmd *cobra.Command, file string) (*dr.Plan, error) {
	if !cmd.Flags().Changed("plan") {
		return nil, nil
	}
	return loadDRPlan(file)
}

// ---- dr check -----------------------------------------------------------------------------------------------

type drCheckResult struct {
	Readiness    *checks.Report         `json:"readiness"`
	Capacity     *checks.CapacityReport `json:"capacity,omitempty"`
	Dependencies *checks.Graph          `json:"dependencies,omitempty"`
}

func newDRCheckCmd(app *App) *cobra.Command {
	var (
		planFile, contextName, minSeverity, capacityTo, failOn string
		suggestTiers, includeSystem                            bool
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Score how ready workloads are to survive failures (read-only)",
		Long: "Reads the cluster and scores each namespace from 0 to 100 using risk signals: single replicas, missing " +
			"PodDisruptionBudgets, no spread across nodes, missing probes, stale backups and more.\n\n" +
			"The score counts SIGNALS. It does not prove that anything can be recovered: only a drill does.",
		Example: "  k8scli dr check -n shop\n" +
			"  k8scli dr check --plan k8scli-dr.yaml --capacity-to dr-cluster\n" +
			"  k8scli dr check -n shop,payments --min-severity high --fail-on high   # for CI",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := app.outputFormat()
			if err != nil {
				return err
			}
			severity, err := checks.ParseSeverity(minSeverity)
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
			namespaces := namespacesFor(app, plan)

			ctx := cmd.Context()
			result := drCheckResult{}
			result.Readiness, err = stack.Readiness(ctx, contextID, checks.ReadinessOptions{
				Namespaces: namespaces, IncludeSystem: includeSystem, MinSeverity: severity,
			})
			if err != nil {
				return err
			}

			if capacityTo != "" {
				options := checks.CapacityOptions{Namespaces: namespaces}
				if plan != nil && app.Namespace == "" {
					options = checks.CapacityOptionsFromPlan(plan)
				}
				result.Capacity, err = stack.Capacity(ctx, contextID, capacityTo, options)
				if err != nil {
					return err
				}
			}
			if suggestTiers {
				result.Dependencies, err = stack.Dependencies(ctx, contextID, checks.DiscoverOptions{Namespaces: namespaces, IncludeSystem: includeSystem})
				if err != nil {
					return err
				}
			}

			if format.Structured() {
				if err := output.WriteStructured(app.Out, format, result); err != nil {
					return err
				}
			} else if err := writeCheck(app.Out, result); err != nil {
				return err
			}
			return failOnFindings(cmd, result.Readiness, failOn, failLevel)
		},
	}

	cmd.Flags().StringVar(&planFile, "plan", defaultPlanFile, "plan file: its namespaces are checked and its context is used")
	cmd.Flags().StringVar(&contextName, "context", "", "kubeconfig context to check (default: the plan's, then the current one)")
	cmd.Flags().StringVar(&minSeverity, "min-severity", "", "hide findings below this severity: high, medium or low")
	cmd.Flags().StringVar(&capacityTo, "capacity-to", "", "also check that the apps would fit on this standby context")
	cmd.Flags().BoolVar(&suggestTiers, "suggest-tiers", false, "also infer dependencies between workloads and suggest start tiers")
	cmd.Flags().BoolVar(&includeSystem, "include-system", false, "include system namespaces (kube-system and similar)")
	cmd.Flags().StringVar(&failOn, "fail-on", "", "exit with an error when a finding at or above this severity exists (for CI)")
	app.addOutputFlag(cmd)
	return cmd
}

func failOnFindings(cmd *cobra.Command, report *checks.Report, raw string, level checks.Severity) error {
	if raw == "" {
		return nil
	}
	count := report.Summary.High
	if level == checks.SeverityMedium || level == checks.SeverityLow {
		count += report.Summary.Medium
	}
	if level == checks.SeverityLow {
		count += report.Summary.Low
	}
	if count > 0 {
		cmd.SilenceUsage = true
		return fmt.Errorf("%d finding(s) at or above %q severity", count, level)
	}
	return nil
}

func objectRef(f checks.Finding) string {
	parts := []string{}
	if f.Namespace != "" {
		parts = append(parts, f.Namespace)
	}
	if f.Kind != "" {
		parts = append(parts, strings.ToLower(f.Kind))
	}
	if f.Name != "" {
		parts = append(parts, f.Name)
	}
	if len(parts) == 0 {
		return "cluster"
	}
	return strings.Join(parts, "/")
}

func writeCheck(w io.Writer, result drCheckResult) error {
	r := result.Readiness
	fmt.Fprintf(w, "Readiness of context %s: score %d/100\n", r.Context, r.Score)
	fmt.Fprintf(w, "%s\n\n", r.Disclaimer)

	table := output.NewTable(w)
	fmt.Fprintln(table, "NAMESPACE\tSCORE\tWORKLOADS\tFINDINGS SHOWN")
	for _, ns := range r.Namespaces {
		shown := len(ns.Findings)
		for _, wl := range ns.Workloads {
			shown += len(wl.Findings)
		}
		fmt.Fprintf(table, "%s\t%d\t%d\t%d\n", ns.Name, ns.Score, len(ns.Workloads), shown)
	}
	if err := table.Flush(); err != nil {
		return err
	}

	var findings []checks.Finding
	findings = append(findings, r.Cluster...)
	for _, ns := range r.Namespaces {
		findings = append(findings, ns.Findings...)
		for _, wl := range ns.Workloads {
			findings = append(findings, wl.Findings...)
		}
	}
	if len(findings) > 0 {
		fmt.Fprintln(w)
		table = output.NewTable(w)
		fmt.Fprintln(table, "SEVERITY\tRULE\tOBJECT\tMESSAGE")
		for _, f := range findings {
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", f.Severity, f.Rule, objectRef(f), f.Message)
			if f.Fix != "" {
				fmt.Fprintf(table, "\t\t\tfix: %s\n", f.Fix)
			}
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "\nFindings: %d high, %d medium, %d low across %d workloads in %d namespaces.\n",
		r.Summary.High, r.Summary.Medium, r.Summary.Low, r.Summary.Workloads, r.Summary.Namespaces)
	if r.Truncated {
		fmt.Fprintln(w, "The check stopped at its workload limit; narrow it with -n.")
	}
	for _, n := range r.Notices {
		fmt.Fprintf(w, "Note: %s\n", n)
	}

	if c := result.Capacity; c != nil {
		writeCapacity(w, c)
	}
	if g := result.Dependencies; g != nil {
		writeDependencies(w, g)
	}
	return nil
}

func writeCapacity(w io.Writer, c *checks.CapacityReport) {
	verdict := "FITS"
	if !c.Fits {
		verdict = "DOES NOT FIT"
	}
	fmt.Fprintf(w, "\nStandby capacity (%s -> %s): %s\n", c.Primary, c.Standby, verdict)
	fmt.Fprintf(w, "  %d workloads, %d pods, %d usable standby nodes\n", c.Workloads, c.Pods, c.StandbyNodes)
	fmt.Fprintf(w, "  CPU:    needs %s, standby has %s free\n", c.CPU.DemandText, c.CPU.FreeText)
	fmt.Fprintf(w, "  Memory: needs %s, standby has %s free\n", c.Memory.DemandText, c.Memory.FreeText)
	if c.Largest != nil {
		fmt.Fprintf(w, "  Largest pod: %s (fits on a node: %v)\n", c.Largest.Name, c.Largest.Fits)
	}
	for _, m := range c.Missing {
		fmt.Fprintf(w, "  Missing on standby: %s %s%s\n", strings.ToLower(m.Kind), m.Name, suffix(m.UsedBy))
	}
	for _, n := range c.Notices {
		fmt.Fprintf(w, "  Note: %s\n", n)
	}
}

func writeDependencies(w io.Writer, g *checks.Graph) {
	fmt.Fprintf(w, "\nSuggested start order (inferred from Services, env vars and network policies)\n")
	nodes := append([]checks.DepNode(nil), g.Nodes...)
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Tier < nodes[j].Tier })
	table := output.NewTable(w)
	fmt.Fprintln(table, "TIER\tWORKLOAD\tSTATEFUL\tENTRY POINT")
	for _, n := range nodes {
		fmt.Fprintf(table, "%d\t%s/%s/%s\t%v\t%v\n", n.Tier, n.Namespace, strings.ToLower(n.Kind), n.Name, n.Stateful, n.EntryPoint)
	}
	_ = table.Flush()
	for _, e := range g.Edges {
		if !e.Cycle {
			fmt.Fprintf(w, "  %s depends on %s (%s)\n", e.From, e.To, e.Via)
		}
	}
	for _, cycle := range g.Cycles {
		fmt.Fprintf(w, "  Warning: dependency cycle: %s\n", strings.Join(cycle, " -> "))
	}
	if draft, err := g.DraftYAML(); err == nil && len(draft) > 0 {
		fmt.Fprintf(w, "\nDraft of the apps section for your plan:\n%s", draft)
	}
}

// ---- dr export ----------------------------------------------------------------------------------------------

func newDRExportCmd(app *App) *cobra.Command {
	var contextName, out string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export a namespace as clean YAML (Secrets are never written)",
		Long: "Writes the manifests of a namespace without status, uid, resourceVersion or managedFields, so they can be " +
			"kept in Git or applied to another cluster. Secrets are NOT exported: only their names are listed in INVENTORY.md, never a value.",
		Example: "  k8scli dr export -n shop                         # shop-<context>-<date>.zip in this folder\n" +
			"  k8scli dr export -n shop -o backup/shop/         # a folder of YAML files",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			namespaces := splitList(app.Namespace)
			if len(namespaces) != 1 {
				return fmt.Errorf("export one namespace at a time: pass -n NAMESPACE")
			}
			stack, err := drStack(app)
			if err != nil {
				return err
			}
			contextID, err := resolveContext(stack, contextName, nil)
			if err != nil {
				return err
			}
			namespace := namespaces[0]
			if out == "" {
				out = fmt.Sprintf("%s-%s-%s.zip", namespace, contextID, time.Now().Format("20060102-1504"))
			}

			var result *checks.ExportResult
			if strings.HasSuffix(strings.ToLower(out), ".zip") {
				// O_EXCL: never overwrite an earlier export.
				file, openErr := os.OpenFile(filepath.Clean(out), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if openErr != nil {
					return fmt.Errorf("cannot create %s: %w", out, openErr)
				}
				result, err = stack.ExportZip(cmd.Context(), contextID, namespace, file, checks.ExportOptions{})
				if closeErr := file.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					_ = os.Remove(out)
				}
			} else {
				result, err = stack.ExportDir(cmd.Context(), contextID, namespace, out, checks.ExportOptions{})
			}
			if err != nil {
				return err
			}

			fmt.Fprintf(app.Out, "Exported %d objects of %s/%s to %s\n", result.Objects, contextID, namespace, out)
			kinds := make([]string, 0, len(result.Counts))
			for kind := range result.Counts {
				kinds = append(kinds, kind)
			}
			sort.Strings(kinds)
			for _, kind := range kinds {
				fmt.Fprintf(app.Out, "  %-24s %d\n", kind, result.Counts[kind])
			}
			fmt.Fprintf(app.Out, "Skipped %d objects owned by controllers (they are recreated by them).\n", result.Skipped)
			fmt.Fprintf(app.Out, "Secrets were NOT exported: %d listed by name in INVENTORY.md, no values.\n", len(result.Secrets))
			for _, warning := range result.Warnings {
				fmt.Fprintf(app.ErrOut, "Warning: %s\n", warning)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", "", "kubeconfig context to export from (default: the current one)")
	cmd.Flags().StringVarP(&out, "output", "o", "", "folder, or a .zip file, to write to")
	return cmd
}

// ---- dr diff ------------------------------------------------------------------------------------------------

func newDRDiffCmd(app *App) *cobra.Command {
	var planFile, contextName, to string
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Compare two clusters: what a standby is missing or runs differently",
		Long: "Compares workloads (images, replicas), Services, ConfigMap key names, Ingress hosts and CRDs. " +
			"ConfigMap values are never read into the output.",
		Example: "  k8scli dr diff --to dr-cluster -n shop\n  k8scli dr diff --plan k8scli-dr.yaml      # uses the plan's context and drContext",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := app.outputFormat()
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
			primary, err := resolveContext(stack, contextName, plan)
			if err != nil {
				return err
			}
			if to == "" && plan != nil {
				to = plan.DRContext
			}
			if to == "" {
				return fmt.Errorf("pass --to CONTEXT (the standby), or a plan that sets drContext")
			}
			report, err := stack.Diff(cmd.Context(), primary, to, checks.DiffOptions{Namespaces: namespacesFor(app, plan)})
			if err != nil {
				return err
			}
			if format.Structured() {
				return output.WriteStructured(app.Out, format, report)
			}
			return writeDiff(app.Out, report)
		},
	}
	cmd.Flags().StringVar(&planFile, "plan", defaultPlanFile, "plan file: its namespaces, context and drContext are used")
	cmd.Flags().StringVar(&contextName, "context", "", "primary context (default: the plan's, then the current one)")
	cmd.Flags().StringVar(&to, "to", "", "standby context to compare with")
	app.addOutputFlag(cmd)
	return cmd
}

func writeDiff(w io.Writer, r *checks.DiffReport) error {
	fmt.Fprintf(w, "Comparing %s (primary) with %s (standby)\n", r.Primary, r.Standby)
	if r.Summary.Identical {
		fmt.Fprintln(w, "No differences found.")
		return nil
	}
	table := output.NewTable(w)
	fmt.Fprintln(table, "NAMESPACE\tCHANGE\tSEVERITY\tOBJECT\tDETAIL")
	row := func(ns string, item checks.DiffItem) {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s/%s\t%s\n", orDash(ns), item.Change, item.Severity, strings.ToLower(item.Kind), item.Name, item.Detail)
	}
	for _, item := range r.Cluster {
		row("(cluster)", item)
	}
	for _, ns := range r.Namespaces {
		for _, item := range ns.Items {
			row(ns.Name, item)
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "\n%d high, %d medium, %d low\n", r.Summary.High, r.Summary.Medium, r.Summary.Low)
	if r.Truncated {
		fmt.Fprintln(w, "The output was truncated; narrow it with -n.")
	}
	for _, n := range r.Notices {
		fmt.Fprintf(w, "Note: %s\n", n)
	}
	return nil
}

// ---- dr backups ---------------------------------------------------------------------------------------------

func newDRBackupsCmd(app *App) *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   "backups",
		Short: "List Velero backups, schedules and volume snapshots (read-only)",
		Long: "Shows the newest backups and snapshots, their age and status, the Velero schedules and whether their " +
			"storage location is reachable. Missing Velero or snapshot support is reported, not treated as an error.",
		Example: "  k8scli dr backups\n  k8scli dr backups -n shop -o json",
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
			contextID, err := resolveContext(stack, contextName, nil)
			if err != nil {
				return err
			}
			listing, err := stack.Backups(cmd.Context(), contextID, backups.ListOptions{Namespace: app.Namespace})
			if err != nil {
				return err
			}
			if format.Structured() {
				return output.WriteStructured(app.Out, format, listing)
			}
			return writeBackups(app, contextID, listing)
		},
	}
	cmd.Flags().StringVar(&contextName, "context", "", "kubeconfig context (default: the current one)")
	app.addOutputFlag(cmd)
	return cmd
}

func writeBackups(app *App, contextName string, l *backups.Listing) error {
	w := app.Out
	fmt.Fprintf(w, "Backups of context %s (Velero: %s, volume snapshots: %s)\n", contextName, installedText(l.VeleroInstalled), installedText(l.SnapshotsInstalled))
	if len(l.Items) == 0 {
		fmt.Fprintln(w, "Nothing found.")
	} else {
		table := output.NewTable(w)
		fmt.Fprintln(table, "SOURCE\tNAME\tNAMESPACE\tSTATUS\tAGE\tOK\tDETAIL")
		for _, item := range l.Items {
			age := "-"
			if !item.Time.IsZero() {
				age = humanDuration(item.Age)
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%v\t%s\n", item.Source, item.Name, orDash(item.Namespace), item.Status, age, item.OK, item.Detail)
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	for _, n := range l.Notices {
		fmt.Fprintf(app.ErrOut, "Note: %s\n", n)
	}
	return nil
}

func installedText(installed bool) string {
	if installed {
		return "installed"
	}
	return "not installed"
}
