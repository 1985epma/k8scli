package cmd

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type logsOptions struct {
	follow     bool
	previous   bool
	timestamps bool
	lines      int64
	container  string
	since      time.Duration
}

func newLogsCmd(app *App) *cobra.Command {
	options := logsOptions{lines: 100}

	cmd := &cobra.Command{
		Use:   "logs <pod> [container]",
		Short: "Get logs from a pod",
		Example: "  k8scli logs my-pod -f\n" +
			"  k8scli logs my-pod -l 50\n" +
			"  k8scli logs my-pod -c sidecar --since 10m --timestamps",
		Args: cobra.RangeArgs(1, 2),
		Annotations: map[string]string{
			requiresClusterAnnotation: "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 {
				if options.container != "" && options.container != args[1] {
					return fmt.Errorf("container given twice: %q (argument) and %q (--container)", args[1], options.container)
				}
				options.container = args[1]
			}
			return app.runLogs(cmd, args[0], options)
		},
	}

	cmd.Flags().BoolVarP(&options.follow, "follow", "f", false, "Follow logs")
	cmd.Flags().BoolVarP(&options.previous, "previous", "p", false, "Get previous container logs")
	cmd.Flags().Int64VarP(&options.lines, "lines", "l", options.lines, "Number of lines to show (negative shows everything)")
	cmd.Flags().StringVarP(&options.container, "container", "c", "", "Container name (default: the first container)")
	cmd.Flags().DurationVar(&options.since, "since", 0, "Only logs newer than a relative duration such as 5s, 2m or 3h")
	cmd.Flags().BoolVar(&options.timestamps, "timestamps", false, "Prefix each line with its timestamp")

	return cmd
}

func (a *App) runLogs(cmd *cobra.Command, podName string, options logsOptions) error {
	// The pod lookup is bounded by --request-timeout; the log stream is not,
	// otherwise `--follow` would be cut off.
	lookupCtx, cancel := a.requestContext(cmd)
	defer cancel()

	pod, err := a.Client.CoreV1().Pods(a.Namespace).Get(lookupCtx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pod: %w", err)
	}

	container, err := a.selectContainer(pod, options.container)
	if err != nil {
		return err
	}

	logOptions := &corev1.PodLogOptions{
		Container:  container,
		Follow:     options.follow,
		Previous:   options.previous,
		Timestamps: options.timestamps,
	}
	if options.lines >= 0 {
		logOptions.TailLines = &options.lines
	}
	if options.since > 0 {
		seconds := int64(options.since.Seconds())
		logOptions.SinceSeconds = &seconds
	}

	streamCtx := cmd.Context()
	stream, err := a.Client.CoreV1().Pods(a.Namespace).GetLogs(podName, logOptions).Stream(streamCtx)
	if err != nil {
		return fmt.Errorf("failed to get logs: %w", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := io.Copy(a.Out, stream); err != nil {
		return fmt.Errorf("error reading logs: %w", err)
	}
	return nil
}

// selectContainer validates an explicit container or picks the first one,
// warning when that choice is ambiguous.
func (a *App) selectContainer(pod *corev1.Pod, requested string) (string, error) {
	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("no containers found in pod %s", pod.Name)
	}

	names := make([]string, 0, len(pod.Spec.Containers))
	for _, container := range pod.Spec.Containers {
		names = append(names, container.Name)
	}

	if requested != "" {
		for _, name := range names {
			if name == requested {
				return requested, nil
			}
		}
		return "", fmt.Errorf("container %q not found in pod %s (available: %s)", requested, pod.Name, strings.Join(names, ", "))
	}

	if len(names) > 1 {
		fmt.Fprintf(a.ErrOut, "Defaulting to container %q (pod has %d containers: %s; use -c to choose)\n", names[0], len(names), strings.Join(names, ", "))
	}
	return names[0], nil
}
