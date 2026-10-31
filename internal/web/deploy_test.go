package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// ---- helpers shared with autoscale_test.go -------------------------------------

// writeLog records the writes the fake clientset receives. The fake ignores the
// DryRun option, so dry-run writes are recorded and swallowed here, and only
// real writes reach the object tracker. That is how the tests prove a dry run
// changes nothing.
type writeLog struct {
	mu   sync.Mutex
	dry  []string
	real []string
}

func (l *writeLog) record(dry bool, entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if dry {
		l.dry = append(l.dry, entry)
	} else {
		l.real = append(l.real, entry)
	}
}

func (l *writeLog) reals() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.real...)
}
func (l *writeLog) dries() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.dry...)
}

func trackWrites(e *env) *writeLog {
	log := &writeLog{}
	e.client.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		var dryRun []string
		var object runtime.Object
		switch a := action.(type) {
		case k8stesting.CreateActionImpl:
			dryRun, object = a.GetCreateOptions().DryRun, a.GetObject()
		case k8stesting.UpdateActionImpl:
			dryRun, object = a.GetUpdateOptions().DryRun, a.GetObject()
		case k8stesting.PatchActionImpl:
			dryRun = a.GetPatchOptions().DryRun
		case k8stesting.DeleteActionImpl:
			dryRun = a.GetDeleteOptions().DryRun
		default:
			return false, nil, nil
		}
		entry := action.GetVerb() + " " + action.GetResource().Resource
		if len(dryRun) > 0 {
			log.record(true, entry)
			return true, object, nil
		}
		log.record(false, entry)
		return false, nil, nil
	})
	return log
}

func wlDeployment(namespace, name string, replicas int32, containers ...corev1.Container) *appsv1.Deployment {
	if len(containers) == 0 {
		containers = []corev1.Container{{Name: "app", Image: "nginx:1.25"}}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app": name}, ResourceVersion: "7"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: containers}},
		},
	}
}

func withCPURequest(c corev1.Container, cpu string) corev1.Container {
	c.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}
	return c
}

func storedDeployment(t *testing.T, e *env, namespace, name string) *appsv1.Deployment {
	t.Helper()
	d, err := e.client.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment %s/%s: %v", namespace, name, err)
	}
	return d
}

func withFields(body map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range body {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// assertWriteGuards proves the three rules every write action follows (ADR 0003):
// a dry run changes nothing, read-only mode blocks real changes, and a
// production-looking context needs its name typed.
func assertWriteGuards(t *testing.T, path string, objects func() []runtime.Object, body map[string]any) {
	t.Helper()

	t.Run("dry run changes nothing", func(t *testing.T) {
		e := newEnv(t, Options{}, objects()...)
		log := trackWrites(e)
		rec := e.do(http.MethodPost, path, withFields(body, map[string]any{"dryRun": true}))
		resp := decode[changeResponse](t, rec)
		if rec.Code != http.StatusOK || !resp.DryRun || resp.Action == "unchanged" {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if len(log.reals()) != 0 || len(log.dries()) == 0 {
			t.Errorf("real writes %v, dry-run writes %v; want only dry-run writes", log.reals(), log.dries())
		}
		if !strings.Contains(resp.Summary, "would") || resp.Equivalent == "" || len(resp.Changes) == 0 {
			t.Errorf("a preview needs a summary, changes and a kubectl command: %+v", resp)
		}
	})

	t.Run("read-only blocks the real change but not the preview", func(t *testing.T) {
		e := newEnv(t, Options{ReadOnly: true}, objects()...)
		log := trackWrites(e)
		rec := e.do(http.MethodPost, path, body)
		if rec.Code != http.StatusForbidden || decode[errorResponse](t, rec).Code != "read_only" {
			t.Fatalf("real change in read-only mode = %d: %s", rec.Code, rec.Body.String())
		}
		if rec := e.do(http.MethodPost, path, withFields(body, map[string]any{"dryRun": true})); rec.Code != http.StatusOK {
			t.Errorf("preview in read-only mode = %d: %s", rec.Code, rec.Body.String())
		}
		if len(log.reals()) != 0 {
			t.Errorf("read-only mode let through %v", log.reals())
		}
	})

	t.Run("production needs the context name", func(t *testing.T) {
		e := newEnv(t, Options{}, objects()...)
		log := trackWrites(e)
		prod := withFields(body, map[string]any{"context": "prod-eu"})

		rec := e.do(http.MethodPost, path, prod)
		if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "confirmation_required" {
			t.Fatalf("no confirmation = %d: %s", rec.Code, rec.Body.String())
		}
		if rec := e.do(http.MethodPost, path, withFields(prod, map[string]any{"confirm": "dev"})); rec.Code != http.StatusConflict {
			t.Errorf("wrong confirmation = %d; want 409", rec.Code)
		}
		if len(log.reals()) != 0 {
			t.Fatalf("changed production without confirmation: %v", log.reals())
		}
		if rec := e.do(http.MethodPost, path, withFields(prod, map[string]any{"dryRun": true})); rec.Code != http.StatusOK {
			t.Errorf("preview in production = %d; a preview never needs confirmation", rec.Code)
		}
		if rec := e.do(http.MethodPost, path, withFields(prod, map[string]any{"confirm": "prod-eu"})); rec.Code != http.StatusOK {
			t.Errorf("confirmed change = %d: %s", rec.Code, rec.Body.String())
		}
		if len(log.reals()) == 0 {
			t.Error("the confirmed change never reached the cluster")
		}
	})
}

func assertBadRequests(t *testing.T, e *env, path string, cases map[string]map[string]any) {
	t.Helper()
	for name, body := range cases {
		rec := e.do(http.MethodPost, path, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d; want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
}

// ---- create --------------------------------------------------------------------

func createBody() map[string]any {
	return map[string]any{
		"namespace": "team-a", "name": "web", "image": "nginx:1.27", "replicas": 3, "port": 8080,
		"env":        []map[string]string{{"name": "MODE", "value": "fast"}},
		"cpuRequest": "100m", "memoryRequest": "128Mi", "cpuLimit": "500m", "memoryLimit": "256Mi",
		"expose": map[string]any{"enabled": true, "type": "LoadBalancer", "port": 80},
	}
}

func TestDeploymentCreateBuildsDeploymentAndService(t *testing.T) {
	e := newEnv(t, Options{})
	rec := e.do(http.MethodPost, "/api/deployments/create", createBody())
	resp := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || resp.Action != "created" || resp.Kind != "Deployment" || resp.DryRun {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	d := storedDeployment(t, e, "team-a", "web")
	c := d.Spec.Template.Spec.Containers[0]
	if *d.Spec.Replicas != 3 || c.Image != "nginx:1.27" || c.Ports[0].ContainerPort != 8080 || c.Env[0].Name != "MODE" {
		t.Errorf("deployment = %+v", d.Spec)
	}
	if d.Labels[managedByLabel] != managedByValue || d.Spec.Selector.MatchLabels["app"] != "web" || d.Spec.Template.Labels["app"] != "web" {
		t.Errorf("labels = %v, selector = %v", d.Labels, d.Spec.Selector)
	}
	if c.Resources.Requests.Cpu().String() != "100m" || c.Resources.Limits.Memory().String() != "256Mi" {
		t.Errorf("resources = %+v", c.Resources)
	}
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("privilege escalation must be off: %+v", c.SecurityContext)
	}

	svc, err := e.client.CoreV1().Services("team-a").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	port := svc.Spec.Ports[0]
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || port.Port != 80 || port.TargetPort.IntValue() != 8080 || svc.Spec.Selector["app"] != "web" {
		t.Errorf("service = %+v", svc.Spec)
	}

	for _, want := range []string{"kubectl create deployment web --image=nginx:1.27 --replicas=3 --port=8080 -n team-a", "kubectl set env deployment/web MODE=fast", "kubectl expose deployment web --port=80 --target-port=8080 --type=LoadBalancer"} {
		if !strings.Contains(resp.Equivalent, want) {
			t.Errorf("equivalent %q misses %q", resp.Equivalent, want)
		}
	}
}

func TestDeploymentCreateDefaultsAndMinimalForm(t *testing.T) {
	e := newEnv(t, Options{})
	rec := e.do(http.MethodPost, "/api/deployments/create", map[string]any{"namespace": "team-a", "name": "api", "image": "ghcr.io/acme/api:v1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	d := storedDeployment(t, e, "team-a", "api")
	if *d.Spec.Replicas != 1 || len(d.Spec.Template.Spec.Containers[0].Ports) != 0 {
		t.Errorf("defaults: replicas %d, ports %v", *d.Spec.Replicas, d.Spec.Template.Spec.Containers[0].Ports)
	}
	if _, err := e.client.CoreV1().Services("team-a").Get(context.Background(), "api", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("no service was asked for, got err=%v", err)
	}
}

func TestDeploymentCreateGuards(t *testing.T) {
	assertWriteGuards(t, "/api/deployments/create", func() []runtime.Object { return nil }, createBody())
}

func TestDeploymentCreateValidation(t *testing.T) {
	e := newEnv(t, Options{})
	log := trackWrites(e)
	with := func(extra map[string]any) map[string]any { return withFields(createBody(), extra) }

	assertBadRequests(t, e, "/api/deployments/create", map[string]map[string]any{
		"uppercase name":      with(map[string]any{"name": "Web"}),
		"underscore name":     with(map[string]any{"name": "my_app"}),
		"name too long":       with(map[string]any{"name": strings.Repeat("a", 64)}),
		"bad namespace":       with(map[string]any{"namespace": "Team A"}),
		"empty image":         with(map[string]any{"image": ""}),
		"image with a space":  with(map[string]any{"image": "nginx latest"}),
		"image too long":      with(map[string]any{"image": strings.Repeat("a", 256)}),
		"too many replicas":   with(map[string]any{"replicas": 101}),
		"negative replicas":   with(map[string]any{"replicas": -1}),
		"port too high":       with(map[string]any{"port": 70000}),
		"negative port":       with(map[string]any{"port": -1}),
		"bad env name":        with(map[string]any{"env": []map[string]string{{"name": "1BAD", "value": "x"}}}),
		"duplicate env":       with(map[string]any{"env": []map[string]string{{"name": "A", "value": "1"}, {"name": "A", "value": "2"}}}),
		"huge env value":      with(map[string]any{"env": []map[string]string{{"name": "A", "value": strings.Repeat("x", 8193)}}}),
		"bad quantity":        with(map[string]any{"cpuRequest": "lots"}),
		"negative quantity":   with(map[string]any{"memoryRequest": "-1Mi"}),
		"limit below request": with(map[string]any{"cpuRequest": "500m", "cpuLimit": "100m"}),
		"expose without port": with(map[string]any{"port": 0, "expose": map[string]any{"enabled": true}}),
		"expose bad type":     with(map[string]any{"expose": map[string]any{"enabled": true, "type": "NodePort", "port": 80}}),
		"expose port range":   with(map[string]any{"expose": map[string]any{"enabled": true, "port": 70000}}),
		"unknown field":       with(map[string]any{"privileged": true}),
		"missing namespace":   {"name": "web", "image": "nginx"},
	})

	tooMany := make([]map[string]string, 51)
	for i := range tooMany {
		tooMany[i] = map[string]string{"name": "V" + strings.Repeat("A", i), "value": "x"}
	}
	assertBadRequests(t, e, "/api/deployments/create", map[string]map[string]any{"51 env vars": with(map[string]any{"env": tooMany})})

	if len(log.reals()) != 0 || len(log.dries()) != 0 {
		t.Errorf("invalid input reached the cluster: %v %v", log.reals(), log.dries())
	}
}

func TestDeploymentCreateAlreadyExists(t *testing.T) {
	e := newEnv(t, Options{}, wlDeployment("team-a", "web", 1))
	rec := e.do(http.MethodPost, "/api/deployments/create", createBody())
	if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "already_exists" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeploymentCreateRemovesItselfWhenTheServiceFails(t *testing.T) {
	e := newEnv(t, Options{})
	e.client.PrependReactor("create", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "services"}, "web")
	})

	rec := e.do(http.MethodPost, "/api/deployments/create", createBody())
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := e.client.AppsV1().Deployments("team-a").Get(context.Background(), "web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the half-created deployment was left behind (err=%v)", err)
	}
}

func TestClusterRejectionIsReportedAsABadRequest(t *testing.T) {
	e := newEnv(t, Options{})
	e.client.PrependReactor("create", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Deployment"}, "web", nil)
	})
	rec := e.do(http.MethodPost, "/api/deployments/create", createBody())
	if rec.Code != http.StatusBadRequest || decode[errorResponse](t, rec).Code != "rejected_by_cluster" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

// ---- detail --------------------------------------------------------------------

func TestDeploymentDetail(t *testing.T) {
	app := corev1.Container{
		Name: "app", Image: "nginx:1.25", Ports: []corev1.ContainerPort{{ContainerPort: 80}},
		Env: []corev1.EnvVar{
			{Name: "MODE", Value: "fast"},
			{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "password"}}},
			{Name: "LEVEL", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}, Key: "level"}}},
		},
	}
	app = withCPURequest(app, "250m")
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "web-hpa", Namespace: "team-a"},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web"}, MaxReplicas: 5},
	}
	e := newEnv(t, Options{}, wlDeployment("team-a", "web", 2, app), hpa)

	detail := decode[deploymentDetailJSON](t, e.do(http.MethodGet, "/api/deployment?namespace=team-a&name=web", nil))
	if detail.Name != "web" || detail.Replicas != 2 || detail.Autoscaler != "web-hpa" || detail.Strategy != "RollingUpdate" || len(detail.Containers) != 1 {
		t.Fatalf("detail = %+v", detail)
	}
	c := detail.Containers[0]
	if c.Image != "nginx:1.25" || c.Ports[0] != 80 || c.Resources.Requests.CPU != "250m" || c.Resources.Limits.CPU != "" {
		t.Errorf("container = %+v", c)
	}
	if c.Env[0].Value != "fast" || c.Env[0].From != "" || c.Env[1].From != "secret db/password" || c.Env[1].Value != "" || c.Env[2].From != "configmap cfg/level" {
		t.Errorf("env = %+v", c.Env)
	}

	if rec := e.do(http.MethodGet, "/api/deployment?namespace=team-a&name=missing", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown deployment = %d", rec.Code)
	}
	if rec := e.do(http.MethodGet, "/api/deployment?namespace=team-a&name=../x", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name = %d", rec.Code)
	}
	if rec := e.do(http.MethodGet, "/api/deployment?name=web", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("missing namespace = %d", rec.Code)
	}
}

// ---- update image ----------------------------------------------------------------

func imageObjects() []runtime.Object {
	return []runtime.Object{wlDeployment("team-a", "web", 2)}
}

func imageBody() map[string]any {
	return map[string]any{"namespace": "team-a", "name": "web", "image": "nginx:1.27"}
}

func TestDeploymentImageUpdates(t *testing.T) {
	e := newEnv(t, Options{}, imageObjects()...)
	rec := e.do(http.MethodPost, "/api/deployments/image", imageBody())
	resp := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || resp.Action != "updated" || len(resp.Changes) != 1 || resp.Changes[0] != "image: nginx:1.25 -> nginx:1.27" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := storedDeployment(t, e, "team-a", "web").Spec.Template.Spec.Containers[0].Image; got != "nginx:1.27" {
		t.Errorf("stored image = %q", got)
	}
	if resp.Equivalent != "kubectl set image deployment/web app=nginx:1.27 -n team-a" {
		t.Errorf("equivalent = %q", resp.Equivalent)
	}
}

func TestDeploymentImageUnchanged(t *testing.T) {
	e := newEnv(t, Options{}, imageObjects()...)
	log := trackWrites(e)
	resp := decode[changeResponse](t, e.do(http.MethodPost, "/api/deployments/image", withFields(imageBody(), map[string]any{"image": "nginx:1.25"})))
	if resp.Action != "unchanged" || len(log.reals())+len(log.dries()) != 0 {
		t.Errorf("same image must be a no-op: %+v %v", resp, log.reals())
	}
}

func TestDeploymentImageGuards(t *testing.T) {
	assertWriteGuards(t, "/api/deployments/image", imageObjects, imageBody())
}

func TestDeploymentImageContainerSelectionAndValidation(t *testing.T) {
	two := wlDeployment("team-a", "multi", 1, corev1.Container{Name: "app", Image: "a:1"}, corev1.Container{Name: "sidecar", Image: "b:1"})
	e := newEnv(t, Options{}, two, wlDeployment("team-a", "web", 1))

	assertBadRequests(t, e, "/api/deployments/image", map[string]map[string]any{
		"container required with two": {"namespace": "team-a", "name": "multi", "image": "a:2"},
		"unknown container":           {"namespace": "team-a", "name": "multi", "container": "nope", "image": "a:2"},
		"empty image":                 {"namespace": "team-a", "name": "web", "image": ""},
		"image with space":            {"namespace": "team-a", "name": "web", "image": "a b"},
		"bad name":                    {"namespace": "team-a", "name": "Web", "image": "a:2"},
		"bad namespace":               {"namespace": "", "name": "web", "image": "a:2"},
		"unknown field":               {"namespace": "team-a", "name": "web", "image": "a:2", "force": true},
	})
	rec := e.do(http.MethodPost, "/api/deployments/image", map[string]any{"namespace": "team-a", "name": "multi", "container": "sidecar", "image": "b:2"})
	if rec.Code != http.StatusOK || storedDeployment(t, e, "team-a", "multi").Spec.Template.Spec.Containers[1].Image != "b:2" {
		t.Errorf("named container: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodPost, "/api/deployments/image", map[string]any{"namespace": "team-a", "name": "gone", "image": "a:2"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown deployment = %d", rec.Code)
	}
}

func TestEditsAreRefusedWhenTheDeploymentChangedMeanwhile(t *testing.T) {
	e := newEnv(t, Options{}, imageObjects()...)
	e.client.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", errors.New("stale"))
	})
	rec := e.do(http.MethodPost, "/api/deployments/image", imageBody())
	if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "conflict" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdatesCarryTheResourceVersionTheyRead(t *testing.T) {
	e := newEnv(t, Options{}, imageObjects()...)
	var sent string
	e.client.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "" {
			sent = action.(k8stesting.UpdateActionImpl).GetObject().(*appsv1.Deployment).ResourceVersion
		}
		return false, nil, nil
	})
	if rec := e.do(http.MethodPost, "/api/deployments/image", imageBody()); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if sent != "7" {
		t.Errorf("resourceVersion sent = %q; the update must carry the one it read", sent)
	}
}

// ---- restart ---------------------------------------------------------------------

func restartBody() map[string]any { return map[string]any{"namespace": "team-a", "name": "web"} }

func TestDeploymentRestartStampsThePodTemplate(t *testing.T) {
	e := newEnv(t, Options{}, imageObjects()...)
	rec := e.do(http.MethodPost, "/api/deployments/restart", restartBody())
	resp := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || resp.Action != "restarted" || resp.Equivalent != "kubectl rollout restart deployment/web -n team-a" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := storedDeployment(t, e, "team-a", "web").Spec.Template.Annotations[restartedAtKey]
	if got != "2026-01-01T12:00:00Z" {
		t.Errorf("restartedAt = %q", got)
	}
}

func TestDeploymentRestartGuards(t *testing.T) {
	assertWriteGuards(t, "/api/deployments/restart", imageObjects, restartBody())
}

func TestDeploymentRestartWarnsAboutZeroReplicasAndValidates(t *testing.T) {
	e := newEnv(t, Options{}, wlDeployment("team-a", "idle", 0))
	resp := decode[changeResponse](t, e.do(http.MethodPost, "/api/deployments/restart", map[string]any{"namespace": "team-a", "name": "idle"}))
	if len(resp.Changes) < 2 || !strings.HasPrefix(resp.Changes[len(resp.Changes)-1], "warning:") {
		t.Errorf("changes = %v", resp.Changes)
	}
	assertBadRequests(t, e, "/api/deployments/restart", map[string]map[string]any{
		"bad name": {"namespace": "team-a", "name": "A_B"}, "no namespace": {"name": "idle"},
	})
	if rec := e.do(http.MethodPost, "/api/deployments/restart", map[string]any{"namespace": "team-a", "name": "gone"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown deployment = %d", rec.Code)
	}
}

// ---- environment -------------------------------------------------------------------

func envDeployment() []runtime.Object {
	return []runtime.Object{wlDeployment("team-a", "web", 1, corev1.Container{
		Name: "app", Image: "nginx",
		Env: []corev1.EnvVar{
			{Name: "MODE", Value: "slow"},
			{Name: "OLD", Value: "x"},
			{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "password"}}},
		},
	})}
}

func envBody() map[string]any {
	return map[string]any{"namespace": "team-a", "name": "web", "set": map[string]string{"MODE": "fast", "NEW": "1"}, "unset": []string{"OLD"}}
}

func TestDeploymentEnvSetsAndUnsets(t *testing.T) {
	e := newEnv(t, Options{}, envDeployment()...)
	rec := e.do(http.MethodPost, "/api/deployments/env", envBody())
	resp := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || resp.Action != "updated" || !strings.Contains(resp.Summary, "rolls out") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	want := []string{`~ MODE: "slow" -> "fast"`, "+ NEW=1", "- OLD"}
	if strings.Join(resp.Changes, "|") != strings.Join(want, "|") {
		t.Errorf("changes = %v; want %v", resp.Changes, want)
	}

	got := map[string]string{}
	for _, v := range storedDeployment(t, e, "team-a", "web").Spec.Template.Spec.Containers[0].Env {
		got[v.Name] = v.Value
		if v.Name == "DB_PASSWORD" && v.ValueFrom == nil {
			t.Error("the secret reference was lost")
		}
	}
	if got["MODE"] != "fast" || got["NEW"] != "1" || got["OLD"] != "" || len(got) != 3 {
		t.Errorf("env = %v", got)
	}
	if !strings.Contains(resp.Equivalent, "kubectl set env deployment/web -c app MODE=fast NEW=1 OLD- -n team-a") {
		t.Errorf("equivalent = %q", resp.Equivalent)
	}
}

func TestDeploymentEnvGuards(t *testing.T) {
	assertWriteGuards(t, "/api/deployments/env", envDeployment, envBody())
}

func TestDeploymentEnvUnchanged(t *testing.T) {
	e := newEnv(t, Options{}, envDeployment()...)
	log := trackWrites(e)
	body := map[string]any{"namespace": "team-a", "name": "web", "set": map[string]string{"MODE": "slow"}, "unset": []string{"NOT_THERE"}}
	resp := decode[changeResponse](t, e.do(http.MethodPost, "/api/deployments/env", body))
	if resp.Action != "unchanged" || len(log.reals())+len(log.dries()) != 0 {
		t.Errorf("resp = %+v, writes = %v", resp, log.reals())
	}
}

func TestDeploymentEnvNeverTouchesReferences(t *testing.T) {
	e := newEnv(t, Options{}, envDeployment()...)
	log := trackWrites(e)
	assertBadRequests(t, e, "/api/deployments/env", map[string]map[string]any{
		"overwrite a secret reference": {"namespace": "team-a", "name": "web", "set": map[string]string{"DB_PASSWORD": "plain"}},
		"unset a secret reference":     {"namespace": "team-a", "name": "web", "unset": []string{"DB_PASSWORD"}},
	})
	if len(log.reals()) != 0 {
		t.Errorf("a refused change was written: %v", log.reals())
	}
}

func TestDeploymentEnvValidation(t *testing.T) {
	e := newEnv(t, Options{}, envDeployment()...)
	many := map[string]string{}
	for i := 0; i < 51; i++ {
		many["V"+strings.Repeat("A", i)] = "x"
	}
	assertBadRequests(t, e, "/api/deployments/env", map[string]map[string]any{
		"nothing to change": {"namespace": "team-a", "name": "web"},
		"bad variable name": {"namespace": "team-a", "name": "web", "set": map[string]string{"1X": "a"}},
		"bad unset name":    {"namespace": "team-a", "name": "web", "unset": []string{"a b"}},
		"set and unset":     {"namespace": "team-a", "name": "web", "set": map[string]string{"MODE": "a"}, "unset": []string{"MODE"}},
		"huge value":        {"namespace": "team-a", "name": "web", "set": map[string]string{"MODE": strings.Repeat("x", 8193)}},
		"too many at once":  {"namespace": "team-a", "name": "web", "set": many},
		"unknown container": {"namespace": "team-a", "name": "web", "container": "nope", "set": map[string]string{"A": "1"}},
		"bad namespace":     {"namespace": "A", "name": "web", "set": map[string]string{"A": "1"}},
	})
}

// ---- small helpers ---------------------------------------------------------------

func TestKubectlQuote(t *testing.T) {
	for in, want := range map[string]string{
		"nginx:1.27":  "nginx:1.27",
		"MODE=fast":   "MODE=fast",
		"a b":         "'a b'",
		"it's":        `'it'\''s'`,
		"$(rm -rf /)": "'$(rm -rf /)'",
		"":            "''",
	} {
		if got := kubectlQuote(in); got != want {
			t.Errorf("kubectlQuote(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestShortTruncatesAndFlattensLines(t *testing.T) {
	if got := short("a\nb"); got != "a b" {
		t.Errorf("short = %q", got)
	}
	if got := short(strings.Repeat("x", 100)); len([]rune(got)) != 40 || !strings.HasSuffix(got, "…") {
		t.Errorf("short = %q", got)
	}
}

func TestDescribeEnvSource(t *testing.T) {
	for want, source := range map[string]*corev1.EnvVarSource{
		"":                    nil,
		"field metadata.name": {FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		"resource limits.cpu": {ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}},
		"reference":           {},
	} {
		if got := describeEnvSource(source); got != want {
			t.Errorf("describeEnvSource = %q; want %q", got, want)
		}
	}
}

func TestMinReplicasDefaultsToOne(t *testing.T) {
	hpa := hpaFor("web", "web", 1, 3, 50)
	hpa.Spec.MinReplicas = nil
	if got := minReplicas(hpa); got != 1 {
		t.Errorf("minReplicas = %d; want the API default 1", got)
	}
}
