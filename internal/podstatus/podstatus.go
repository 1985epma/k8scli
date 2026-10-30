// Package podstatus derives the human readable pod columns (STATUS, READY,
// RESTARTS) the same way kubectl does, so CrashLoopBackOff and friends are
// visible instead of a bare "Running".
package podstatus

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Status returns the display status of a pod.
func Status(pod *corev1.Pod) string {
	reason := string(pod.Status.Phase)
	if pod.Status.Reason != "" {
		reason = pod.Status.Reason
	}

	initializing := false
	for i := range pod.Status.InitContainerStatuses {
		container := pod.Status.InitContainerStatuses[i]
		terminated := container.State.Terminated
		waiting := container.State.Waiting

		switch {
		case terminated != nil && terminated.ExitCode == 0:
			continue
		case terminated != nil:
			initializing = true
			switch {
			case terminated.Reason != "":
				reason = "Init:" + terminated.Reason
			case terminated.Signal != 0:
				reason = fmt.Sprintf("Init:Signal:%d", terminated.Signal)
			default:
				reason = fmt.Sprintf("Init:ExitCode:%d", terminated.ExitCode)
			}
		case waiting != nil && waiting.Reason != "" && waiting.Reason != "PodInitializing":
			initializing = true
			reason = "Init:" + waiting.Reason
		default:
			initializing = true
			reason = fmt.Sprintf("Init:%d/%d", i, len(pod.Spec.InitContainers))
		}
		break
	}

	if !initializing {
		hasRunning := false
		for i := len(pod.Status.ContainerStatuses) - 1; i >= 0; i-- {
			container := pod.Status.ContainerStatuses[i]
			terminated := container.State.Terminated
			waiting := container.State.Waiting

			switch {
			case waiting != nil && waiting.Reason != "":
				reason = waiting.Reason
			case terminated != nil && terminated.Reason != "":
				reason = terminated.Reason
			case terminated != nil && terminated.Signal != 0:
				reason = fmt.Sprintf("Signal:%d", terminated.Signal)
			case terminated != nil:
				reason = fmt.Sprintf("ExitCode:%d", terminated.ExitCode)
			case container.Ready && container.State.Running != nil:
				hasRunning = true
			}
		}

		if reason == "Completed" && hasRunning {
			if hasCondition(pod, corev1.PodReady, corev1.ConditionTrue) {
				reason = string(corev1.PodRunning)
			} else {
				reason = "NotReady"
			}
		}
	}

	if pod.DeletionTimestamp != nil {
		if pod.Status.Reason == "NodeLost" {
			return "Unknown"
		}
		return "Terminating"
	}

	return reason
}

// Ready returns "ready/total" for the app containers.
func Ready(pod *corev1.Pod) string {
	ready := 0
	for _, container := range pod.Status.ContainerStatuses {
		if container.Ready {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, len(pod.Spec.Containers))
}

// Restarts sums the restart count of every container, init containers included.
func Restarts(pod *corev1.Pod) int32 {
	var total int32
	for _, container := range pod.Status.InitContainerStatuses {
		total += container.RestartCount
	}
	for _, container := range pod.Status.ContainerStatuses {
		total += container.RestartCount
	}
	return total
}

func hasCondition(pod *corev1.Pod, conditionType corev1.PodConditionType, status corev1.ConditionStatus) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Status == status
		}
	}
	return false
}
