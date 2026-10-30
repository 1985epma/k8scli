package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/1985epma/k8scli/internal/output"
	"github.com/1985epma/k8scli/internal/podstatus"
)

// podPageSize keeps list requests small on large clusters.
const podPageSize = 500

func newPodsCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "pods",
		Short:   "List pods in namespace",
		Example: "  k8scli pods\n  k8scli pods -n my-namespace -o wide",
		Args:    cobra.NoArgs,
		Annotations: map[string]string{
			requiresClusterAnnotation: "true",
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.runPods(cmd, app.Namespace, false)
		},
	}

	app.addOutputFlag(cmd)
	return cmd
}

func newPodsAllCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "pods-all",
		Short:   "List pods across all namespaces",
		Example: "  k8scli pods-all\n  k8scli pods-all -o json",
		Args:    cobra.NoArgs,
		Annotations: map[string]string{
			requiresClusterAnnotation: "true",
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.runPods(cmd, metav1.NamespaceAll, true)
		},
	}

	app.addOutputFlag(cmd)
	return cmd
}

func (a *App) runPods(cmd *cobra.Command, namespace string, showNamespace bool) error {
	format, err := a.outputFormat()
	if err != nil {
		return err
	}

	ctx, cancel := a.requestContext(cmd)
	defer cancel()

	pods, err := a.listPods(ctx, namespace)
	if err != nil {
		return err
	}

	if format.Structured() {
		return output.WriteStructured(a.Out, format, &corev1.PodList{
			TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"},
			Items:    pods,
		})
	}

	if len(pods) == 0 {
		if showNamespace {
			fmt.Fprintln(a.ErrOut, "No pods found.")
		} else {
			fmt.Fprintf(a.ErrOut, "No pods found in namespace %q.\n", namespace)
		}
		return nil
	}

	wide := format == output.Wide
	table := output.NewTable(a.Out)

	header := "NAME\tREADY\tSTATUS\tRESTARTS\tAGE"
	if showNamespace {
		header = "NAMESPACE\t" + header
	}
	if wide {
		header += "\tIP\tNODE"
	}
	fmt.Fprintln(table, header)

	for i := range pods {
		pod := &pods[i]
		row := fmt.Sprintf("%s\t%s\t%s\t%d\t%s",
			pod.Name,
			podstatus.Ready(pod),
			podstatus.Status(pod),
			podstatus.Restarts(pod),
			duration.HumanDuration(timeSince(pod.CreationTimestamp)),
		)
		if showNamespace {
			row = pod.Namespace + "\t" + row
		}
		if wide {
			row += fmt.Sprintf("\t%s\t%s", valueOrDash(pod.Status.PodIP), valueOrDash(pod.Spec.NodeName))
		}
		fmt.Fprintln(table, row)
	}
	return table.Flush()
}

// listPods pages through the API so very large namespaces do not time out.
func (a *App) listPods(ctx context.Context, namespace string) ([]corev1.Pod, error) {
	var pods []corev1.Pod
	options := metav1.ListOptions{Limit: podPageSize}

	for {
		list, err := a.Client.CoreV1().Pods(namespace).List(ctx, options)
		if err != nil {
			return nil, fmt.Errorf("failed to list pods: %w", err)
		}
		pods = append(pods, list.Items...)
		if list.Continue == "" {
			return pods, nil
		}
		options.Continue = list.Continue
	}
}

func valueOrDash(value string) string {
	if value == "" {
		return "<none>"
	}
	return value
}
