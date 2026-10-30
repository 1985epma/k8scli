package cmd

import (
	"bufio"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// quickReplicas are the values accepted by the `set` shortcut.
var quickReplicas = map[string]int32{"2": 2, "4": 4, "6": 6, "8": 8}

type scaleOptions struct {
	dryRun bool
	yes    bool
}

func (o *scaleOptions) addFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "show what would change without scaling")
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "skip the confirmation asked for production-looking contexts")
}

func newScaleCmd(app *App) *cobra.Command {
	var options scaleOptions

	cmd := &cobra.Command{
		Use:     "scale <deployment> <replicas>",
		Short:   "Scale a deployment to custom replicas",
		Example: "  k8scli scale myapp 10\n  k8scli scale myapp 0 --dry-run",
		Args:    cobra.ExactArgs(2),
		Annotations: map[string]string{
			requiresClusterAnnotation: "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			replicas, err := parseReplicas(args[1])
			if err != nil {
				return err
			}
			return app.scaleDeployment(cmd, args[0], replicas, options)
		},
	}

	options.addFlags(cmd)
	return cmd
}

func newSetCmd(app *App) *cobra.Command {
	var options scaleOptions

	cmd := &cobra.Command{
		Use:     "set <deployment> <replicas>",
		Short:   "Quick scale: 2, 4, 6, or 8 pods",
		Example: "  k8scli set myapp 4",
		Args:    cobra.ExactArgs(2),
		Annotations: map[string]string{
			requiresClusterAnnotation: "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			replicas, ok := quickReplicas[args[1]]
			if !ok {
				return fmt.Errorf("invalid replicas: %s (use: 2, 4, 6, or 8)", args[1])
			}
			return app.scaleDeployment(cmd, args[0], replicas, options)
		},
	}

	options.addFlags(cmd)
	return cmd
}

// parseReplicas accepts only plain non-negative integers.
func parseReplicas(value string) (int32, error) {
	replicas, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid replicas value %q: must be a whole number", value)
	}
	if replicas < 0 || replicas > math.MaxInt32 {
		return 0, fmt.Errorf("invalid replicas value %q: must be 0 or greater", value)
	}
	return int32(replicas), nil
}

// scaleDeployment goes through the scale subresource so it does not overwrite
// unrelated changes made to the Deployment in the meantime.
func (a *App) scaleDeployment(cmd *cobra.Command, name string, replicas int32, options scaleOptions) error {
	ctx, cancel := a.requestContext(cmd)
	defer cancel()

	contextName := a.ContextName
	if contextName == "" {
		contextName = "<none>"
	}
	fmt.Fprintf(a.ErrOut, "Context: %s | Namespace: %s\n", contextName, a.Namespace)

	deployments := a.Client.AppsV1().Deployments(a.Namespace)

	scale, err := deployments.GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment %s: %w", name, err)
	}

	current := scale.Spec.Replicas
	if current == replicas {
		fmt.Fprintf(a.Out, "Deployment %s already has %d replicas\n", name, replicas)
		return nil
	}

	if options.dryRun {
		fmt.Fprintf(a.Out, "Deployment %s would be scaled from %d to %d replicas (dry-run)\n", name, current, replicas)
		return nil
	}

	if !options.yes && looksLikeProduction(a.ContextName) {
		prompt := fmt.Sprintf("Context %q looks like production. Scale %s from %d to %d replicas? [y/N]: ", a.ContextName, name, current, replicas)
		if !a.confirm(prompt) {
			return fmt.Errorf("aborted (use --yes to skip this confirmation)")
		}
	}

	updated := scale.DeepCopy()
	updated.Spec.Replicas = replicas
	if _, err := deployments.UpdateScale(ctx, name, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to scale deployment: %w", err)
	}

	fmt.Fprintf(a.Out, "Deployment %s scaled to %d replicas\n", name, replicas)
	return nil
}

func looksLikeProduction(contextName string) bool {
	return strings.Contains(strings.ToLower(contextName), "prod")
}

// confirm asks a yes/no question; anything but "y"/"yes" (including EOF) is a no.
func (a *App) confirm(prompt string) bool {
	fmt.Fprint(a.ErrOut, prompt)

	answer, err := bufio.NewReader(a.In).ReadString('\n')
	if err != nil && answer == "" {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
