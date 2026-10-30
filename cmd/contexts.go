package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/1985epma/k8scli/internal/kubeconfig"
	"github.com/1985epma/k8scli/internal/output"
)

func newContextsCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "contexts",
		Short:   "List available contexts",
		Example: "  k8scli contexts\n  k8scli contexts -o json",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			format, err := app.outputFormat()
			if err != nil {
				return err
			}

			contexts, err := kubeconfig.New(app.Kubeconfig).Contexts()
			if err != nil {
				return err
			}

			if format.Structured() {
				return output.WriteStructured(app.Out, format, contexts)
			}

			table := output.NewTable(app.Out)
			fmt.Fprintln(table, "CURRENT\tNAME\tCLUSTER\tAUTHINFO\tNAMESPACE")
			for _, ctx := range contexts {
				marker := ""
				if ctx.Current {
					marker = "*"
				}
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", marker, ctx.Name, ctx.Cluster, ctx.User, ctx.Namespace)
			}
			return table.Flush()
		},
	}

	app.addOutputFlag(cmd)
	return cmd
}

func newUseCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "use <context>",
		Short: "Switch to a context (docker-desktop, eks, minikube, ...)",
		Example: "  k8scli use docker-desktop\n" +
			"  k8scli use arn:aws:eks:us-east-1:123456789:cluster/my-cluster",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := kubeconfig.New(app.Kubeconfig).UseContext(args[0]); err != nil {
				return err
			}
			_, err := fmt.Fprintf(app.Out, "Switched to context: %s\n", args[0])
			return err
		},
	}
}
