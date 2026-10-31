package cmd

import (
	"github.com/spf13/cobra"

	"github.com/1985epma/k8scli/internal/web"
)

// addDRWebFlags registers the disaster-recovery flags shared by `serve` and `ui`. Checks, exports, dry
// runs and reports are always available; real runs are opt-in because they can stop applications,
// databases and machines.
func addDRWebFlags(cmd *cobra.Command, options *web.Options) {
	cmd.Flags().BoolVar(&options.AllowDRRuns, "allow-dr-runs", false, "allow real disaster-recovery runs, drills and resume from the console (dry runs, checks, exports and reports are always on)")
	cmd.Flags().BoolVar(&options.AllowDRPower, "allow-dr-power", false, "allow DR runs to power machines or the cluster on and off for plans that are not marked safety.labOnly")
	cmd.Flags().StringVar(&options.DRDir, "dr-dir", "", "directory for DR plans, run ledgers, audit log and reports (default ~/.k8scli/dr)")
}
