// Package cmd wires the k8scli commands.
package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/1985epma/k8scli/internal/kubeconfig"
	"github.com/1985epma/k8scli/internal/output"
)

const (
	requiresClusterAnnotation = "requires-cluster"
	defaultRequestTimeout     = 30 * time.Second
)

// App holds the state shared by the commands. Commands depend on the
// kubernetes.Interface instead of a concrete clientset so tests can inject
// a fake one.
type App struct {
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer

	// Flags.
	Kubeconfig     string
	Namespace      string
	Output         string
	RequestTimeout time.Duration

	// Client is built lazily from the kubeconfig unless a test provides one.
	Client kubernetes.Interface
	// ContextName is the kubeconfig context the commands talk to.
	ContextName string
}

// NewApp returns an App wired to the process streams.
func NewApp() *App {
	return &App{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
}

// Execute runs the CLI with the given context and returns the exit error.
func Execute(ctx context.Context) error {
	root := NewRootCmd(NewApp())
	root.SetArgs(LauncherArgs(os.Args)[1:])
	return root.ExecuteContext(ctx)
}

// NewRootCmd builds the command tree for app.
func NewRootCmd(app *App) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "k8scli",
		Short:         "Kubernetes CLI helper",
		Long:          "k8scli is a small helper for everyday Kubernetes work: switch contexts, inspect pods and scale deployments.",
		Version:       versionString(),
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			// Arguments are already validated here, so from now on errors are
			// runtime errors and do not deserve a usage dump.
			cmd.SilenceUsage = true

			if cmd.Annotations[requiresClusterAnnotation] != "true" {
				return nil
			}
			return app.prepare()
		},
	}

	rootCmd.SetIn(app.In)
	rootCmd.SetOut(app.Out)
	rootCmd.SetErr(app.ErrOut)
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	rootCmd.CompletionOptions.DisableDefaultCmd = true

	rootCmd.PersistentFlags().StringVar(&app.Kubeconfig, "kubeconfig", "", "path to kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	rootCmd.PersistentFlags().StringVarP(&app.Namespace, "namespace", "n", "", "namespace (default: the current context's namespace, or \"default\")")
	rootCmd.PersistentFlags().DurationVar(&app.RequestTimeout, "request-timeout", defaultRequestTimeout, "timeout for requests to the cluster (0 disables it)")

	rootCmd.AddCommand(
		newCompletionCmd(rootCmd),
		newPodsCmd(app),
		newPodsAllCmd(app),
		newClusterCmd(app),
		newScaleCmd(app),
		newSetCmd(app),
		newLogsCmd(app),
		newUseCmd(app),
		newContextsCmd(app),
		newVersionCmd(app),
		newServeCmd(app),
		newUICmd(app),
		newDRCmd(app),
	)

	return rootCmd
}

// prepare resolves the kubeconfig, builds the client and picks the namespace.
func (a *App) prepare() error {
	loader := kubeconfig.New(a.Kubeconfig)

	if a.Client == nil {
		config, err := loader.RESTConfig()
		if err != nil {
			return err
		}
		client, err := kubernetes.NewForConfig(config)
		if err != nil {
			return fmt.Errorf("failed to create clientset: %w", err)
		}
		a.Client = client

		if a.ContextName == "" {
			a.ContextName = loader.CurrentContext()
		}
		if a.Namespace == "" {
			a.Namespace = loader.Namespace()
		}
	}

	if a.Namespace == "" {
		a.Namespace = kubeconfig.DefaultNamespace
	}
	return nil
}

// requestContext returns a context bounded by --request-timeout.
func (a *App) requestContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if a.RequestTimeout > 0 {
		return context.WithTimeout(ctx, a.RequestTimeout)
	}
	return ctx, func() {}
}

// outputFormat parses the --output flag.
func (a *App) outputFormat() (output.Format, error) {
	return output.ParseFormat(a.Output)
}

// addOutputFlag registers -o/--output on cmd.
func (a *App) addOutputFlag(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&a.Output, "output", "o", string(output.Table), "output format: table, wide, json or yaml")
}
