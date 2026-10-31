package cmd

import (
	"github.com/spf13/cobra"

	"github.com/1985epma/k8scli/internal/web"
)

func newServeCmd(app *App) *cobra.Command {
	options := web.Options{Addr: web.DefaultAddr}

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the local web console",
		Long: "Start a web console on your machine. It reads the same kubeconfig as the CLI and offers a dashboard, pods, logs, " +
			"guided changes (deploy, scale, autoscale, configuration), contexts and setup wizards in the browser.\n\n" +
			"The console listens on loopback only and needs the session token in the printed link. " +
			"Use --read-only to disable everything that changes the cluster.",
		Example: "  k8scli serve\n  k8scli serve --read-only\n  k8scli serve --addr 127.0.0.1:9000",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.Kubeconfig = app.Kubeconfig
			options.RequestTimeout = app.RequestTimeout
			options.Version = shortVersion()

			server, err := web.New(options)
			if err != nil {
				return err
			}
			return server.Run(cmd.Context(), app.Out)
		},
	}

	cmd.Flags().StringVar(&options.Addr, "addr", options.Addr, "address to listen on (host:port)")
	cmd.Flags().BoolVar(&options.ReadOnly, "read-only", false, "disable scaling and every other change")
	cmd.Flags().BoolVar(&options.CatalogOnly, "catalog-only", false, "install Helm apps only from the reviewed catalog: no Artifact Hub search")
	cmd.Flags().BoolVar(&options.AllowRemote, "allow-remote", false, "allow listening beyond loopback (anyone with the link can use your credentials)")
	addDRWebFlags(cmd, &options)
	return cmd
}
