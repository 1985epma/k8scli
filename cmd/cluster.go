package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/1985epma/k8scli/internal/output"
)

type nodeInfo struct {
	Name  string `json:"name"`
	Ready string `json:"ready"`
}

type clusterInfo struct {
	Context    string     `json:"context,omitempty"`
	Version    string     `json:"version"`
	Platform   string     `json:"platform"`
	Nodes      []nodeInfo `json:"nodes"`
	Namespaces []string   `json:"namespaces"`
}

func newClusterCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cluster",
		Short:   "Show cluster info",
		Example: "  k8scli cluster\n  k8scli cluster -o yaml",
		Args:    cobra.NoArgs,
		Annotations: map[string]string{
			requiresClusterAnnotation: "true",
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := app.outputFormat()
			if err != nil {
				return err
			}

			ctx, cancel := app.requestContext(cmd)
			defer cancel()

			serverVersion, err := app.Client.Discovery().ServerVersion()
			if err != nil {
				return fmt.Errorf("failed to get cluster info: %w", err)
			}

			nodes, err := app.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("failed to list nodes: %w", err)
			}

			namespaces, err := app.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("failed to list namespaces: %w", err)
			}

			info := clusterInfo{
				Context:    app.ContextName,
				Version:    serverVersion.GitVersion,
				Platform:   serverVersion.Platform,
				Nodes:      make([]nodeInfo, 0, len(nodes.Items)),
				Namespaces: make([]string, 0, len(namespaces.Items)),
			}
			for _, node := range nodes.Items {
				info.Nodes = append(info.Nodes, nodeInfo{Name: node.Name, Ready: nodeReady(node)})
			}
			for _, namespace := range namespaces.Items {
				info.Namespaces = append(info.Namespaces, namespace.Name)
			}

			if format.Structured() {
				return output.WriteStructured(app.Out, format, info)
			}

			out := app.Out
			fmt.Fprintf(out, "Cluster Info:\n")
			if info.Context != "" {
				fmt.Fprintf(out, "  Context:    %s\n", info.Context)
			}
			fmt.Fprintf(out, "  Version:    %s\n", info.Version)
			fmt.Fprintf(out, "  Platform:   %s\n", info.Platform)
			fmt.Fprintf(out, "  Nodes:      %d\n", len(info.Nodes))
			fmt.Fprintf(out, "  Namespaces: %d\n", len(info.Namespaces))

			fmt.Fprintf(out, "\nNodes:\n")
			for _, node := range info.Nodes {
				fmt.Fprintf(out, "  - %s: %s\n", node.Name, node.Ready)
			}

			fmt.Fprintf(out, "\nNamespaces:\n")
			for _, namespace := range info.Namespaces {
				fmt.Fprintf(out, "  - %s\n", namespace)
			}
			return nil
		},
	}

	app.addOutputFlag(cmd)
	return cmd
}

func nodeReady(node corev1.Node) string {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return string(condition.Status)
		}
	}
	return string(corev1.ConditionUnknown)
}
