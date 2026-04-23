package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var (
	kubeconfig string
	namespace  string
	clientset  *kubernetes.Clientset
)

const requiresClusterAnnotation = "requires-cluster"

func main() {
	rootCmd := &cobra.Command{
		Use:   "k8scli",
		Short: "Kubernetes CLI helper",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Annotations[requiresClusterAnnotation] != "true" {
				return nil
			}

			if kubeconfig == "" {
				kubeconfig = kubeconfigPath()
			}
			config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
			if err != nil {
				return fmt.Errorf("failed to build config: %w", err)
			}
			clientset, err = kubernetes.NewForConfig(config)
			if err != nil {
				return fmt.Errorf("failed to create clientset: %w", err)
			}
			return nil
		},
	}

	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", kubeconfigPath(), "path to kubeconfig file")
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "namespace")

	rootCmd.AddCommand(completionCmd(rootCmd))
	rootCmd.AddCommand(listPodsCmd())
	rootCmd.AddCommand(listAllPodsCmd())
	rootCmd.AddCommand(listClusterCmd())
	rootCmd.AddCommand(scaleCmd())
	rootCmd.AddCommand(scaleQuickCmd())
	rootCmd.AddCommand(logsCmd())
	rootCmd.AddCommand(useContextCmd())
	rootCmd.AddCommand(listContextsCmd())
	rootCmd.AddCommand(helpCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func completionCmd(rootCmd *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:       "completion [bash|zsh|fish|powershell]",
		Short:     "Generate shell completion scripts",
		Long:      "Generate shell completion scripts for bash, zsh, fish, or powershell.",
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

	return cmd
}

func listPodsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pods",
		Short: "List pods in namespace",
		Annotations: map[string]string{
			requiresClusterAnnotation: "true",
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			pods, err := clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("failed to list pods: %w", err)
			}

			fmt.Printf("Pods in namespace %s:\n\n", namespace)
			fmt.Printf("%-40s %-20s %-15s\n", "NAME", "READY", "STATUS")
			fmt.Printf("%-40s %-20s %-15s\n", strings.Repeat("-", 40), strings.Repeat("-", 20), strings.Repeat("-", 15))

			for _, p := range pods.Items {
				ready := fmt.Sprintf("%d/%d", readyContainers(p), len(p.Spec.Containers))
				fmt.Printf("%-40s %-20s %-15s\n", p.Name, ready, string(p.Status.Phase))
			}
			return nil
		},
	}
}

func listAllPodsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pods-all",
		Short: "List pods across all namespaces",
		Annotations: map[string]string{
			requiresClusterAnnotation: "true",
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			pods, err := clientset.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("failed to list pods: %w", err)
			}

			fmt.Printf("All pods in cluster:\n\n")
			fmt.Printf("%-40s %-20s %-15s %-20s\n", "NAME", "NAMESPACE", "READY", "STATUS")
			fmt.Printf("%-40s %-20s %-15s %-20s\n", strings.Repeat("-", 40), strings.Repeat("-", 20), strings.Repeat("-", 15), strings.Repeat("-", 20))

			for _, p := range pods.Items {
				ready := fmt.Sprintf("%d/%d", readyContainers(p), len(p.Spec.Containers))
				fmt.Printf("%-40s %-20s %-15s %-20s\n", p.Name, p.Namespace, ready, string(p.Status.Phase))
			}
			return nil
		},
	}
}

func listClusterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cluster",
		Short: "Show cluster info",
		Annotations: map[string]string{
			requiresClusterAnnotation: "true",
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			version, err := clientset.ServerVersion()
			if err != nil {
				return fmt.Errorf("failed to get cluster info: %w", err)
			}

			nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("failed to list nodes: %w", err)
			}

			namespaces, err := clientset.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("failed to list namespaces: %w", err)
			}

			fmt.Printf("Cluster Info:\n")
			fmt.Printf("  Version:    %s\n", version.GitVersion)
			fmt.Printf("  Platform:  %s\n", version.Platform)
			fmt.Printf("  Nodes:      %d\n", len(nodes.Items))
			fmt.Printf("  Namespaces: %d\n", len(namespaces.Items))

			fmt.Printf("\nNodes:\n")
			for _, n := range nodes.Items {
				for _, c := range n.Status.Conditions {
					if c.Type == "Ready" {
						fmt.Printf("  - %s: %s\n", n.Name, c.Status)
					}
				}
			}

			fmt.Printf("\nNamespaces:\n")
			for _, ns := range namespaces.Items {
				fmt.Printf("  - %s\n", ns.Name)
			}

			return nil
		},
	}
}

func scaleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scale <deployment> <replicas>",
		Short: "Scale a deployment to custom replicas",
		Args:  cobra.ExactArgs(2),
		Annotations: map[string]string{
			requiresClusterAnnotation: "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			deploymentName := args[0]
			var replicas int32
			_, err := fmt.Sscanf(args[1], "%d", &replicas)
			if err != nil {
				return fmt.Errorf("invalid replicas value: %w", err)
			}

			deploymentsClient := clientset.AppsV1().Deployments(namespace)
			deployment, err := deploymentsClient.Get(context.Background(), deploymentName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("failed to get deployment %s: %w", deploymentName, err)
			}

			deployment.Spec.Replicas = &replicas
			_, err = deploymentsClient.Update(context.Background(), deployment, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to scale deployment: %w", err)
			}

			fmt.Printf("Deployment %s scaled to %d replicas\n", deploymentName, replicas)
			return nil
		},
	}
}

func scaleQuickCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <deployment> <replicas>",
		Short: "Quick scale: 2, 4, 6, or 8 pods",
		Args:  cobra.ExactArgs(2),
		Annotations: map[string]string{
			requiresClusterAnnotation: "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			deploymentName := args[0]
			replicaStr := args[1]

			validReplicas := map[string]int32{
				"2": 2, "4": 4, "6": 6, "8": 8,
			}

			replicas, ok := validReplicas[replicaStr]
			if !ok {
				return fmt.Errorf("invalid replicas: %s (use: 2, 4, 6, or 8)", replicaStr)
			}

			deploymentsClient := clientset.AppsV1().Deployments(namespace)
			deployment, err := deploymentsClient.Get(context.Background(), deploymentName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("failed to get deployment %s: %w", deploymentName, err)
			}

			deployment.Spec.Replicas = &replicas
			_, err = deploymentsClient.Update(context.Background(), deployment, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to scale deployment: %w", err)
			}

			fmt.Printf("Deployment %s scaled to %d pods\n", deploymentName, replicas)
			return nil
		},
	}
}

func logsCmd() *cobra.Command {
	var follow bool
	var previous bool
	var tailLines int64 = 100

	cmd := &cobra.Command{
		Use:   "logs <pod> [container]",
		Short: "Get logs from a pod",
		Args:  cobra.MinimumNArgs(1),
		Annotations: map[string]string{
			requiresClusterAnnotation: "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			podName := args[0]
			var containerName string
			if len(args) > 1 {
				containerName = args[1]
			}

			pod, err := clientset.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("failed to get pod: %w", err)
			}

			if containerName == "" {
				if len(pod.Spec.Containers) > 0 {
					containerName = pod.Spec.Containers[0].Name
				} else {
					return fmt.Errorf("no containers found in pod")
				}
			}

			req := clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
				Follow:    follow,
				Previous: previous,
				TailLines: &tailLines,
			})

			stream, err := req.Stream(context.Background())
			if err != nil {
				return fmt.Errorf("failed to get logs: %w", err)
			}
			defer stream.Close()

			reader := bufio.NewReader(stream)
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					if err == io.EOF {
						break
					}
					return fmt.Errorf("error reading logs: %w", err)
				}
				fmt.Print(line)
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow logs")
	cmd.Flags().BoolVarP(&previous, "previous", "p", false, "Get previous container logs")
	cmd.Flags().Int64VarP(&tailLines, "lines", "l", 100, "Number of lines to show")

	return cmd
}

func readyContainers(pod corev1.Pod) int {
	ready := 0
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
	}
	return ready
}

func listContextsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "contexts",
		Short: "List available contexts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := loadKubeconfig()
			if err != nil {
				return err
			}

			current := config.CurrentContext
			fmt.Printf("Available contexts:\n\n")
			fmt.Printf("%-40s %-15s\n", "NAME", "CURRENT")
			fmt.Printf("%-40s %-15s\n", strings.Repeat("-", 40), strings.Repeat("-", 15))

			for name := range config.Contexts {
				marker := ""
				if name == current {
					marker = "*"
				}
				fmt.Printf("%-40s %-15s\n", name, marker)
			}
			return nil
		},
	}
}

func useContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <context>",
		Short: "Switch to context (docker-desktop, eks, etc)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctxName := args[0]

			config, err := loadKubeconfig()
			if err != nil {
				return err
			}

			if _, ok := config.Contexts[ctxName]; !ok {
				return fmt.Errorf("context %q not found", ctxName)
			}

			config.CurrentContext = ctxName
			err = clientcmd.WriteToFile(*config, resolvedKubeconfigPath())
			if err != nil {
				return fmt.Errorf("failed to save kubeconfig: %w", err)
			}

			fmt.Printf("Switched to context: %s\n", ctxName)
			return nil
		},
	}
}

func loadKubeconfig() (*clientcmdapi.Config, error) {
	config, err := clientcmd.LoadFromFile(resolvedKubeconfigPath())
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	return config, nil
}

func kubeconfigPath() string {
	kubeconfigEnv := os.Getenv("KUBECONFIG")
	if kubeconfigEnv == "" {
		homeDir, _ := os.UserHomeDir()
		kubeconfigEnv = filepath.Join(homeDir, ".kube", "config")
	}
	return kubeconfigEnv
}

func resolvedKubeconfigPath() string {
	if kubeconfig != "" {
		return kubeconfig
	}
	return kubeconfigPath()
}

func helpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "help",
		Short: "Show this help message",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Print(`

  ___ ___ ___ ___ ___ ___ ___ ___ ___ ___ ___ ___ ___ 
 | __|  _|  _|  _|  _|  _|  _|  _|  _|  _|  _|  _| 
 | __| |_| |_| |_| |_| |_| |_| |_| |_| |_| 
 |___|___|___|___|___|___|___|___|___|___| 

  K8sCLI - Kubernetes CLI Helper

USAGE:
  k8scli <command> [flags]

COMMANDS:

  Context Management
    contexts         List all available contexts
    use <name>      Switch to a context (docker-desktop, eks, minikube)

  Pod Operations
    pods             List pods in namespace
    pods-all         List pods across all namespaces
    logs <pod>      Get logs from a pod
    set <app> <n>   Quick scale: 2, 4, 6, or 8 pods
    scale <app> <n> Scale to custom number

  Cluster Info
    cluster          Show cluster info (nodes, namespaces, version)

GLOBAL FLAGS:
  -n, --namespace   Namespace (default: default)
  --kubeconfig     Path to kubeconfig file

EXAMPLES:

  # List available contexts
  k8scli contexts

  # Switch to docker-desktop
  k8scli use docker-desktop

  # Switch to EKS cluster
  k8scli use arn:aws:eks:us-east-1:123456789:cluster/my-cluster

  # List pods in namespace
  k8scli pods -n my-namespace

  # List pods in all namespaces
  k8scli pods-all

  # Get logs (follow mode)
  k8scli logs my-pod -f

  # Get last 50 lines
  k8scli logs my-pod -l 50

  # Quick scale to 4 pods
  k8scli set myapp 4

  # Scale to 10 pods
  k8scli scale myapp 10

  # Show cluster info
  k8scli cluster

`)
			return nil
		},
	}
}