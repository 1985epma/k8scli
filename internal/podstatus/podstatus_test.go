package podstatus

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStatus(t *testing.T) {
	now := metav1.Now()
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}

	tests := []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{
			name: "plain phase",
			pod:  corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			want: "Pending",
		},
		{
			name: "status reason wins over phase",
			pod:  corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}},
			want: "Evicted",
		},
		{
			name: "crash loop",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{
					{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
				},
			}},
			want: "CrashLoopBackOff",
		},
		{
			name: "terminated without reason uses exit code",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{
					{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}},
				},
			}},
			want: "ExitCode:137",
		},
		{
			name: "terminated by signal",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{
					{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Signal: 9}}},
				},
			}},
			want: "Signal:9",
		},
		{
			name: "init container still running",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: "a"}, {Name: "b"}}},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{
						{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
						{State: running},
					},
				},
			},
			want: "Init:1/2",
		},
		{
			name: "init container failed",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				InitContainerStatuses: []corev1.ContainerStatus{
					{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}},
				},
			}},
			want: "Init:Error",
		},
		{
			name: "init container image pull",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				InitContainerStatuses: []corev1.ContainerStatus{
					{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}},
				},
			}},
			want: "Init:ImagePullBackOff",
		},
		{
			name: "completed but another container runs and pod is ready",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{
					{Ready: true, State: running},
					{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}},
				},
			}},
			want: "Running",
		},
		{
			name: "terminating",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			},
			want: "Terminating",
		},
		{
			name: "terminating on a lost node",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning, Reason: "NodeLost"},
			},
			want: "Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Status(&tt.pod); got != tt.want {
				t.Errorf("Status() = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestReadyAndRestarts(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}, {Name: "b"}, {Name: "c"}}},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{RestartCount: 4}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Ready: true, RestartCount: 1},
				{Ready: true},
				{Ready: false, RestartCount: 2},
			},
		},
	}

	if got := Ready(pod); got != "2/3" {
		t.Errorf("Ready() = %q; want 2/3", got)
	}
	if got := Restarts(pod); got != 7 {
		t.Errorf("Restarts() = %d; want 7", got)
	}
}
