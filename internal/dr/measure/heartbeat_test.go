package measure

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func pvc(namespace, name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

func newHeartbeatFor(client *fake.Clientset, clock *simClock) *Heartbeat {
	h := &Heartbeat{Client: client, Namespace: "shop", PVC: "data", Now: clock.Now, Sleep: clock.Sleep}
	h.defaults()
	return h
}

// finishReaderJobs makes every reader Job succeed (or fail) and gives it a pod.
func finishReaderJobs(client *fake.Clientset, succeed bool) {
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		if succeed {
			job.Status.Succeeded = 1
		} else {
			job.Status.Failed = 1
		}
		_ = client.Tracker().Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: job.Name + "-abcde", Namespace: job.Namespace, Labels: map[string]string{"job-name": job.Name},
		}})
		return false, nil, nil
	})
}

func TestWriterDeploymentIsSmallHardenedAndPinned(t *testing.T) {
	h := newHeartbeatFor(fake.NewSimpleClientset(), newClock())
	deployment := h.writerDeployment()

	if deployment.Name != HeartbeatName || *deployment.Spec.Replicas != 1 || deployment.Spec.Strategy.Type != "Recreate" {
		t.Errorf("deployment: %+v", deployment.Spec)
	}
	if deployment.Labels["app.kubernetes.io/managed-by"] != "k8scli" || deployment.Labels[heartbeatLabel] != "true" {
		t.Errorf("labels: %v", deployment.Labels)
	}

	pod := deployment.Spec.Template.Spec
	container := pod.Containers[0]
	if !strings.Contains(container.Image, "@sha256:") || !strings.HasPrefix(container.Image, "busybox:") {
		t.Errorf("the image must be pinned by digest: %s", container.Image)
	}
	script := strings.Join(container.Command, " ")
	for _, want := range []string{"date +%s", "/data/.heartbeat.tmp", "mv ", "/data/heartbeat", "sleep 5"} {
		if !strings.Contains(script, want) {
			t.Errorf("the writer script %q is missing %q", script, want)
		}
	}

	sc := container.SecurityContext
	if sc == nil || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || !*sc.RunAsNonRoot ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container security context: %+v", sc)
	}
	if psc := pod.SecurityContext; psc == nil || !*psc.RunAsNonRoot || psc.FSGroup == nil || psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context: %+v", psc)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the writer must not get a service account token")
	}
	limits := container.Resources.Limits
	if limits.Cpu().MilliValue() != 50 || limits.Memory().Value() != 16<<20 || container.Resources.Requests.Cpu().MilliValue() != 5 {
		t.Errorf("resources: %+v", container.Resources)
	}
	if pod.Volumes[0].PersistentVolumeClaim.ClaimName != "data" || pod.Volumes[0].PersistentVolumeClaim.ReadOnly || container.VolumeMounts[0].MountPath != "/data" {
		t.Errorf("volume: %+v", pod.Volumes)
	}
}

func TestReaderJobMountsTheVolumeReadOnly(t *testing.T) {
	h := newHeartbeatFor(fake.NewSimpleClientset(), newClock())
	job := h.readerJob("k8scli-dr-hb-read-1")
	pod := job.Spec.Template.Spec

	if *job.Spec.BackoffLimit != 0 || pod.RestartPolicy != corev1.RestartPolicyNever || job.Spec.TTLSecondsAfterFinished == nil {
		t.Errorf("job spec: %+v", job.Spec)
	}
	if !pod.Volumes[0].PersistentVolumeClaim.ReadOnly || !pod.Containers[0].VolumeMounts[0].ReadOnly {
		t.Error("the reader must mount the volume read-only")
	}
	if strings.Join(pod.Containers[0].Command, " ") != "cat /data/heartbeat" {
		t.Errorf("command: %v", pod.Containers[0].Command)
	}
	if *job.Spec.ActiveDeadlineSeconds != int64(defaultReadTimeout.Seconds()) {
		t.Errorf("deadline: %d", *job.Spec.ActiveDeadlineSeconds)
	}
}

func TestInstallNeedsTheVolumeAndIsIdempotent(t *testing.T) {
	client := fake.NewSimpleClientset()
	h := newHeartbeatFor(client, newClock())
	if err := h.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot use PVC shop/data") {
		t.Errorf("a missing PVC must be an error, got %v", err)
	}

	client = fake.NewSimpleClientset(pvc("shop", "data"))
	h = newHeartbeatFor(client, newClock())
	for i := 0; i < 2; i++ {
		if err := h.Install(context.Background()); err != nil {
			t.Fatalf("install #%d: %v", i+1, err)
		}
	}
	if _, err := client.AppsV1().Deployments("shop").Get(context.Background(), HeartbeatName, metav1.GetOptions{}); err != nil {
		t.Error(err)
	}
}

func TestInstallRefusesToTakeOverADeploymentThatIsNotOurs(t *testing.T) {
	foreign := newHeartbeatFor(fake.NewSimpleClientset(), newClock()).writerDeployment()
	foreign.Labels = map[string]string{"owner": "someone-else"}
	client := fake.NewSimpleClientset(pvc("shop", "data"), foreign)

	err := newHeartbeatFor(client, newClock()).Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "was not created by k8scli") {
		t.Errorf("err = %v", err)
	}
}

func TestHeartbeatValidatesWhatItIsGiven(t *testing.T) {
	clock := newClock()
	for name, mutate := range map[string]func(*Heartbeat){
		"bad namespace":   func(h *Heartbeat) { h.Namespace = "Bad NS" },
		"bad pvc":         func(h *Heartbeat) { h.PVC = "../x" },
		"image has space": func(h *Heartbeat) { h.Image = "busybox --privileged" },
		"interval low":    func(h *Heartbeat) { h.Interval = time.Millisecond },
		"interval high":   func(h *Heartbeat) { h.Interval = time.Hour },
		"no client":       func(h *Heartbeat) { h.Client = nil },
	} {
		h := newHeartbeatFor(fake.NewSimpleClientset(pvc("shop", "data")), clock)
		mutate(h)
		if err := h.Install(context.Background()); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestStartWaitsForTheWriterAndOneInterval(t *testing.T) {
	client := fake.NewSimpleClientset(pvc("shop", "data"))
	client.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		// The writer comes up as soon as it is created.
		action.(k8stesting.CreateAction).GetObject().(*appsv1.Deployment).Status.ReadyReplicas = 1
		return false, nil, nil
	})
	clock := newClock()
	h := newHeartbeatFor(client, clock)
	before := clock.sleeps.Load()
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if clock.sleeps.Load() == before {
		t.Error("Start must wait one interval so at least one heartbeat is on the volume")
	}
}

func TestStartGivesUpWhenTheWriterNeverBecomesReady(t *testing.T) {
	clock := newClock()
	clock.autoAdvance.Store(true)
	client := fake.NewSimpleClientset(pvc("shop", "data"))
	err := newHeartbeatFor(client, clock).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "was not ready") {
		t.Errorf("err = %v", err)
	}
}

func TestStopDeletesTheWriterAndWaitsForItsPod(t *testing.T) {
	clock := newClock()
	client := fake.NewSimpleClientset(pvc("shop", "data"))
	h := newHeartbeatFor(client, clock)
	if err := h.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hb-1", Namespace: "shop", Labels: map[string]string{"app.kubernetes.io/name": HeartbeatName}}}
	if _, err := client.CoreV1().Pods("shop").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// The pod disappears after a few polls, like a terminating pod.
	polls := 0
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		polls++
		if polls >= 3 {
			return true, &corev1.PodList{}, nil
		}
		return false, nil, nil
	})
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if polls < 3 {
		t.Errorf("Stop returned after %d polls, before the pod was gone", polls)
	}
	if _, err := client.AppsV1().Deployments("shop").Get(context.Background(), HeartbeatName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the writer must be deleted: %v", err)
	}
	// Stopping again is fine.
	if err := h.Stop(context.Background()); err != nil {
		t.Errorf("a second Stop: %v", err)
	}
}

func TestStopFailsWhenThePodNeverStops(t *testing.T) {
	clock := newClock()
	clock.autoAdvance.Store(true)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hb-1", Namespace: "shop", Labels: map[string]string{"app.kubernetes.io/name": HeartbeatName}}}
	client := fake.NewSimpleClientset(pod)
	if err := newHeartbeatFor(client, clock).Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Errorf("err = %v", err)
	}
}

func TestReadLastReturnsTheTimestampAndCleansUp(t *testing.T) {
	clock := newClock()
	client := fake.NewSimpleClientset(pvc("shop", "data"))
	finishReaderJobs(client, true)
	h := newHeartbeatFor(client, clock)

	var readFrom string
	last := epoch.Add(-90 * time.Second)
	h.LogReader = func(_ context.Context, namespace, pod string) (string, error) {
		readFrom = namespace + "/" + pod
		return strconv.FormatInt(last.Unix(), 10) + "\n", nil
	}

	got, err := h.ReadLast(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(last) {
		t.Errorf("last heartbeat = %v; want %v", got, last)
	}
	if want := "shop/" + readerJobName(epoch) + "-abcde"; readFrom != want {
		t.Errorf("the log was read from %q; want %q", readFrom, want)
	}
	jobs, _ := client.BatchV1().Jobs("shop").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Errorf("the reader job was left behind: %d", len(jobs.Items))
	}
}

func TestReadLastFailurePaths(t *testing.T) {
	t.Run("job fails", func(t *testing.T) {
		client := fake.NewSimpleClientset(pvc("shop", "data"))
		finishReaderJobs(client, false)
		_, err := newHeartbeatFor(client, newClock()).ReadLast(context.Background())
		if err == nil || !strings.Contains(err.Error(), "reader job failed") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("job never finishes", func(t *testing.T) {
		clock := newClock()
		clock.autoAdvance.Store(true)
		_, err := newHeartbeatFor(fake.NewSimpleClientset(pvc("shop", "data")), clock).ReadLast(context.Background())
		if err == nil || !strings.Contains(err.Error(), "ReadWriteOnce") {
			t.Errorf("a timeout should explain the RWO limitation, got %v", err)
		}
	})

	t.Run("pod log is not a number", func(t *testing.T) {
		client := fake.NewSimpleClientset(pvc("shop", "data"))
		finishReaderJobs(client, true)
		h := newHeartbeatFor(client, newClock()) // the fake clientset's default log is "fake logs"
		if _, err := h.ReadLast(context.Background()); err == nil || !strings.Contains(err.Error(), "Unix time") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("log reader fails", func(t *testing.T) {
		client := fake.NewSimpleClientset(pvc("shop", "data"))
		finishReaderJobs(client, true)
		h := newHeartbeatFor(client, newClock())
		h.LogReader = func(context.Context, string, string) (string, error) { return "", errors.New("log stream broke") }
		if _, err := h.ReadLast(context.Background()); err == nil || !strings.Contains(err.Error(), "log stream broke") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("pod missing", func(t *testing.T) {
		client := fake.NewSimpleClientset(pvc("shop", "data"))
		client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
			action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).Status.Succeeded = 1
			return false, nil, nil
		})
		if _, err := newHeartbeatFor(client, newClock()).ReadLast(context.Background()); err == nil || !strings.Contains(err.Error(), "pod was not found") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestRemoveDeletesWriterAndReadersAndIsIdempotent(t *testing.T) {
	client := fake.NewSimpleClientset(pvc("shop", "data"))
	h := newHeartbeatFor(client, newClock())
	if err := h.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	reader := h.readerJob("k8scli-dr-hb-read-old")
	reader.Namespace = "shop"
	if _, err := client.BatchV1().Jobs("shop").Create(context.Background(), reader, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	unrelated := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses-job", Namespace: "shop"}}
	if _, err := client.BatchV1().Jobs("shop").Create(context.Background(), unrelated, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := h.Remove(context.Background()); err != nil {
			t.Fatalf("Remove #%d: %v", i+1, err)
		}
	}
	if _, err := client.AppsV1().Deployments("shop").Get(context.Background(), HeartbeatName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("the writer must be gone")
	}
	jobs, _ := client.BatchV1().Jobs("shop").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 || jobs.Items[0].Name != "someone-elses-job" {
		t.Errorf("only our reader jobs may be removed: %+v", jobs.Items)
	}

	// Never fails the caller's cleanup because of bad input.
	if err := (&Heartbeat{Namespace: "Bad NS"}).Remove(context.Background()); err != nil {
		t.Errorf("Remove with a bad namespace: %v", err)
	}
}

func TestPackageLevelHelpers(t *testing.T) {
	client := fake.NewSimpleClientset(pvc("shop", "data"))
	opts := HeartbeatOptions{Namespace: "shop", PVC: "data"}
	if err := InstallHeartbeat(context.Background(), client, opts); err != nil {
		t.Fatal(err)
	}
	if err := RemoveHeartbeat(context.Background(), client, "shop"); err != nil {
		t.Fatal(err)
	}
	finishReaderJobs(client, true)
	opts.LogReader = func(context.Context, string, string) (string, error) { return "1767268800\n", nil }
	got, err := ReadLastHeartbeat(context.Background(), client, opts)
	if err != nil || got.Unix() != 1767268800 {
		t.Errorf("ReadLastHeartbeat = %v, %v", got, err)
	}
}

func TestParseHeartbeat(t *testing.T) {
	for input, ok := range map[string]bool{
		"1767268800": true, "1767268800\n": true, "  42 \n": true,
		"": false, "abc": false, "-5": false, "0": false, "12.5": false, strings.Repeat("9", 40): false,
	} {
		_, err := parseHeartbeat(input)
		if (err == nil) != ok {
			t.Errorf("parseHeartbeat(%q) err = %v; ok should be %v", input, err, ok)
		}
	}
	if _, err := parseHeartbeat(strings.Repeat("x", 100)); err == nil || len(err.Error()) > 200 {
		t.Errorf("a long garbage file should be truncated in the error: %v", err)
	}
}
