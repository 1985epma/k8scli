package measure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/1985epma/k8scli/internal/dr"
)

// The PVC heartbeat is the one way to MEASURE data loss without database credentials. A tiny pod writes
// the current Unix time to /data/heartbeat on the volume every few seconds. The drill deletes the writer at
// T0 (the disaster), runs down and up, then a read-only Job prints the file: T0 minus that timestamp is the
// data the volume lost. If the volume is simply scaled down and up the loss is the write interval; if it is
// restored from a backup, the loss is how old that backup was.
const (
	// HeartbeatName names the writer Deployment.
	HeartbeatName = "k8scli-dr-heartbeat"

	// heartbeatImage is busybox 1.37.0, pinned by the digest of its multi-architecture index.
	heartbeatImage = "busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

	heartbeatLabel       = "k8scli.io/dr-heartbeat"
	heartbeatFile        = "/data/heartbeat"
	readerJobPrefix      = "k8scli-dr-hb-read-"
	defaultBeatInterval  = 5 * time.Second
	defaultReadTimeout   = 3 * time.Minute
	defaultStartTimeout  = 2 * time.Minute
	heartbeatPollEvery   = time.Second
	nobody               = int64(65534)
	maxHeartbeatInterval = time.Minute
)

// HeartbeatOptions configure the heartbeat of one volume.
type HeartbeatOptions struct {
	// Namespace and PVC identify the volume to watch; the PVC must already exist.
	Namespace string
	PVC       string
	// Image overrides the pinned busybox image (for air-gapped registries).
	Image string
	// Interval is how often the writer writes; default 5s, between 1s and 1m.
	Interval time.Duration
	// ReadTimeout bounds the reader Job; default 3m.
	ReadTimeout time.Duration
	// LogReader returns the log of a pod; nil reads it through the API. Tests replace it.
	LogReader func(ctx context.Context, namespace, pod string) (string, error)
}

// Heartbeat drives the writer and the reader for one volume.
type Heartbeat struct {
	Client    kubernetes.Interface
	Namespace string
	PVC       string
	Image     string
	Interval  time.Duration
	// ReadTimeout bounds the reader Job.
	ReadTimeout time.Duration
	LogReader   func(ctx context.Context, namespace, pod string) (string, error)
	Now         func() time.Time
	Sleep       func(ctx context.Context, d time.Duration) error
}

func newHeartbeat(env *dr.Env, options HeartbeatOptions) *Heartbeat {
	h := &Heartbeat{
		Client: env.Client, Namespace: options.Namespace, PVC: options.PVC, Image: options.Image,
		Interval: options.Interval, ReadTimeout: options.ReadTimeout, LogReader: options.LogReader,
		Now: env.Now, Sleep: env.Sleep,
	}
	h.defaults()
	return h
}

func (h *Heartbeat) defaults() {
	if h.Image == "" {
		h.Image = heartbeatImage
	}
	if h.Interval <= 0 {
		h.Interval = defaultBeatInterval
	}
	if h.ReadTimeout <= 0 {
		h.ReadTimeout = defaultReadTimeout
	}
	if h.Now == nil {
		h.Now = time.Now
	}
	if h.Sleep == nil {
		h.Sleep = sleepContext
	}
	if h.LogReader == nil {
		h.LogReader = h.readPodLog
	}
}

// InstallHeartbeat creates the writer Deployment (it does not wait for it).
func InstallHeartbeat(ctx context.Context, client kubernetes.Interface, options HeartbeatOptions) error {
	h := &Heartbeat{Client: client, Namespace: options.Namespace, PVC: options.PVC, Image: options.Image, Interval: options.Interval}
	h.defaults()
	return h.Install(ctx)
}

// ReadLastHeartbeat reads the newest timestamp on the volume with a short read-only Job.
func ReadLastHeartbeat(ctx context.Context, client kubernetes.Interface, options HeartbeatOptions) (time.Time, error) {
	h := &Heartbeat{Client: client, Namespace: options.Namespace, PVC: options.PVC, Image: options.Image,
		ReadTimeout: options.ReadTimeout, LogReader: options.LogReader}
	h.defaults()
	return h.ReadLast(ctx)
}

// RemoveHeartbeat deletes the writer and any reader Jobs of the namespace.
func RemoveHeartbeat(ctx context.Context, client kubernetes.Interface, namespace string) error {
	h := &Heartbeat{Client: client, Namespace: namespace}
	h.defaults()
	return h.Remove(ctx)
}

func (h *Heartbeat) validate() error {
	if h.Client == nil {
		return errors.New("no Kubernetes client")
	}
	if problems := validation.IsDNS1123Label(h.Namespace); len(problems) > 0 {
		return fmt.Errorf("invalid namespace %q", h.Namespace)
	}
	if problems := validation.IsDNS1123Subdomain(h.PVC); len(problems) > 0 {
		return fmt.Errorf("invalid PVC name %q", h.PVC)
	}
	if strings.ContainsAny(h.Image, " \t\r\n") || len(h.Image) > 255 || h.Image == "" {
		return errors.New("invalid heartbeat image")
	}
	if h.Interval < time.Second || h.Interval > maxHeartbeatInterval {
		return fmt.Errorf("the heartbeat interval must be between 1s and %s", maxHeartbeatInterval)
	}
	return nil
}

func heartbeatLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       HeartbeatName,
		"app.kubernetes.io/managed-by": "k8scli",
		heartbeatLabel:                 "true",
	}
}

func hardenedContext() *corev1.SecurityContext {
	no, yes := false, true
	user := nobody
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &no,
		ReadOnlyRootFilesystem:   &yes,
		RunAsNonRoot:             &yes,
		RunAsUser:                &user,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func podSecurity() *corev1.PodSecurityContext {
	yes := true
	id := nobody
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   &yes,
		RunAsUser:      &id,
		RunAsGroup:     &id,
		FSGroup:        &id,
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func (h *Heartbeat) podSpec(command []string, readOnlyVolume bool, restart corev1.RestartPolicy) corev1.PodSpec {
	no := false
	grace := int64(1) // the writer is a shell loop; do not wait 30 seconds for it to stop
	return corev1.PodSpec{
		RestartPolicy:                 restart,
		AutomountServiceAccountToken:  &no,
		TerminationGracePeriodSeconds: &grace,
		SecurityContext:               podSecurity(),
		Containers: []corev1.Container{{
			Name:            "heartbeat",
			Image:           h.Image,
			Command:         command,
			SecurityContext: hardenedContext(),
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m"), corev1.ResourceMemory: resource.MustParse("8Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
			},
			VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data", ReadOnly: readOnlyVolume}},
		}},
		Volumes: []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: h.PVC, ReadOnly: readOnlyVolume},
			},
		}},
	}
}

// writerDeployment is the heartbeat writer. It writes through a temporary file and a rename so a reader
// never sees half a number.
func (h *Heartbeat) writerDeployment() *appsv1.Deployment {
	replicas := int32(1)
	labels := heartbeatLabels()
	script := fmt.Sprintf("while true; do date +%%s > /data/.heartbeat.tmp && mv /data/.heartbeat.tmp %s; sleep %d; done", heartbeatFile, int(h.Interval.Seconds()))
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: HeartbeatName, Namespace: h.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": HeartbeatName}},
			// The volume is usually ReadWriteOnce: the old pod must be gone before a new one mounts it.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       h.podSpec([]string{"sh", "-c", script}, false, corev1.RestartPolicyAlways),
			},
		},
	}
}

func readerJobName(now time.Time) string {
	return readerJobPrefix + strconv.FormatInt(now.Unix(), 16)
}

// readerJob prints the heartbeat file from a read-only mount.
func (h *Heartbeat) readerJob(name string) *batchv1.Job {
	backoff := int32(0)
	ttl := int32(300)
	deadline := int64(h.ReadTimeout.Seconds())
	labels := map[string]string{
		"app.kubernetes.io/name":       HeartbeatName + "-reader",
		"app.kubernetes.io/managed-by": "k8scli",
		heartbeatLabel:                 "reader",
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       h.podSpec([]string{"cat", heartbeatFile}, true, corev1.RestartPolicyNever),
			},
		},
	}
}

// Install creates the writer. An existing Deployment of ours is left alone; anything else with that name is an error.
func (h *Heartbeat) Install(ctx context.Context) error {
	if err := h.validate(); err != nil {
		return err
	}
	if _, err := h.Client.CoreV1().PersistentVolumeClaims(h.Namespace).Get(ctx, h.PVC, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("cannot use PVC %s/%s: %w", h.Namespace, h.PVC, err)
	}
	_, err := h.Client.AppsV1().Deployments(h.Namespace).Create(ctx, h.writerDeployment(), metav1.CreateOptions{FieldManager: "k8scli-dr"})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := h.Client.AppsV1().Deployments(h.Namespace).Get(ctx, HeartbeatName, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		if existing.Labels[heartbeatLabel] != "true" {
			return fmt.Errorf("a Deployment named %s already exists in %s and was not created by k8scli", HeartbeatName, h.Namespace)
		}
		return nil
	}
	return err
}

// Start installs the writer, waits until it is ready and until it has had time to write once.
func (h *Heartbeat) Start(ctx context.Context) error {
	if err := h.Install(ctx); err != nil {
		return err
	}
	deadline := h.Now().Add(defaultStartTimeout)
	for {
		deployment, err := h.Client.AppsV1().Deployments(h.Namespace).Get(ctx, HeartbeatName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if deployment.Status.ReadyReplicas >= 1 {
			break
		}
		if h.Now().After(deadline) {
			return fmt.Errorf("the heartbeat writer in %s was not ready after %s", h.Namespace, defaultStartTimeout)
		}
		if err := h.Sleep(ctx, heartbeatPollEvery); err != nil {
			return err
		}
	}
	// One full interval guarantees at least one heartbeat is on the volume.
	return h.Sleep(ctx, h.Interval)
}

// Stop deletes the writer and waits until its pod is gone, so no more heartbeats are written.
func (h *Heartbeat) Stop(ctx context.Context) error {
	if err := h.validate(); err != nil {
		return err
	}
	err := h.Client.AppsV1().Deployments(h.Namespace).Delete(ctx, HeartbeatName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	selector := "app.kubernetes.io/name=" + HeartbeatName
	deadline := h.Now().Add(defaultStartTimeout)
	for {
		pods, err := h.Client.CoreV1().Pods(h.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return err
		}
		if len(pods.Items) == 0 {
			return nil
		}
		if h.Now().After(deadline) {
			return fmt.Errorf("the heartbeat pod in %s did not stop after %s", h.Namespace, defaultStartTimeout)
		}
		if err := h.Sleep(ctx, heartbeatPollEvery); err != nil {
			return err
		}
	}
}

// Remove deletes the writer and every reader Job. It ignores objects that are already gone.
func (h *Heartbeat) Remove(ctx context.Context) error {
	if h.Client == nil || len(validation.IsDNS1123Label(h.Namespace)) > 0 {
		return nil
	}
	var firstErr error
	if err := h.Client.AppsV1().Deployments(h.Namespace).Delete(ctx, HeartbeatName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		firstErr = err
	}
	jobs, err := h.Client.BatchV1().Jobs(h.Namespace).List(ctx, metav1.ListOptions{LabelSelector: heartbeatLabel + "=reader"})
	if err != nil {
		if firstErr == nil {
			firstErr = err
		}
		return firstErr
	}
	background := metav1.DeletePropagationBackground
	for i := range jobs.Items {
		if err := h.Client.BatchV1().Jobs(h.Namespace).Delete(ctx, jobs.Items[i].Name, metav1.DeleteOptions{PropagationPolicy: &background}); err != nil && !apierrors.IsNotFound(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ReadLast runs the reader Job, returns the timestamp it printed and removes the Job.
func (h *Heartbeat) ReadLast(ctx context.Context) (time.Time, error) {
	if err := h.validate(); err != nil {
		return time.Time{}, err
	}
	name := readerJobName(h.Now())
	jobs := h.Client.BatchV1().Jobs(h.Namespace)
	if _, err := jobs.Create(ctx, h.readerJob(name), metav1.CreateOptions{FieldManager: "k8scli-dr"}); err != nil {
		return time.Time{}, fmt.Errorf("failed to create the reader job: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		background := metav1.DeletePropagationBackground
		_ = jobs.Delete(cleanup, name, metav1.DeleteOptions{PropagationPolicy: &background})
	}()

	deadline := h.Now().Add(h.ReadTimeout)
	for {
		job, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return time.Time{}, fmt.Errorf("failed to read the reader job: %w", err)
		}
		if job.Status.Succeeded > 0 {
			break
		}
		if job.Status.Failed > 0 {
			return time.Time{}, errors.New("the reader job failed (is there a heartbeat file on the volume, and can the volume be mounted by a second pod?)")
		}
		if h.Now().After(deadline) {
			return time.Time{}, fmt.Errorf("the reader job did not finish within %s (a ReadWriteOnce volume can only be mounted on the node that already uses it)", h.ReadTimeout)
		}
		if err := h.Sleep(ctx, heartbeatPollEvery); err != nil {
			return time.Time{}, err
		}
	}

	pods, err := h.Client.CoreV1().Pods(h.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
	if err != nil {
		return time.Time{}, err
	}
	if len(pods.Items) == 0 {
		return time.Time{}, errors.New("the reader job finished but its pod was not found")
	}
	output, err := h.LogReader(ctx, h.Namespace, pods.Items[0].Name)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to read the reader pod log: %w", err)
	}
	return parseHeartbeat(output)
}

func parseHeartbeat(output string) (time.Time, error) {
	text := strings.TrimSpace(output)
	seconds, err := strconv.ParseInt(text, 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, fmt.Errorf("the heartbeat file does not contain a Unix time: %q", truncate(text, 40))
	}
	return time.Unix(seconds, 0).UTC(), nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

// readPodLog is the default LogReader.
func (h *Heartbeat) readPodLog(ctx context.Context, namespace, pod string) (string, error) {
	stream, err := h.Client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()
	data, err := io.ReadAll(io.LimitReader(stream, 4096))
	return string(data), err
}
