package cmd

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Injected at build time through -ldflags (see .goreleaser.yaml and the Makefile).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// shortVersion is the bare version, e.g. v0.1.1.
func shortVersion() string {
	v := version
	if v == "dev" {
		// Binaries built with `go install module@version` carry their version here.
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
	}
	return v
}

func versionString() string {
	return fmt.Sprintf("k8scli version %s (commit %s, built %s)", shortVersion(), commit, date)
}

func newVersionCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the k8scli version",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(app.Out, versionString())
			return err
		},
	}
}
