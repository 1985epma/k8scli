package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newCompletionCmd(rootCmd *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:       "completion [bash|zsh|fish|powershell]",
		Short:     "Generate shell completion scripts",
		Long:      "Generate shell completion scripts for bash, zsh, fish, or powershell.",
		Example:   "  k8scli completion zsh > ~/.zsh/completion/_k8scli",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			shell := args[0]
			out := cmd.OutOrStdout()

			switch shell {
			case "bash":
				return rootCmd.GenBashCompletion(out)
			case "zsh":
				return rootCmd.GenZshCompletion(out)
			case "fish":
				return rootCmd.GenFishCompletion(out, true)
			case "powershell":
				return rootCmd.GenPowerShellCompletionWithDesc(out)
			default:
				return fmt.Errorf("unsupported shell %q", shell)
			}
		},
	}
}
