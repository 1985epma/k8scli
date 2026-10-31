package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

var deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

// ---- fixed shape: what the cluster is asked to create -------------------------------

func TestImagesOutsideTheReferenceCharsetAreRefused(t *testing.T) {
	bad := []string{
		"ngi\u202enx",      // right-to-left override: the review screen would show another name
		"nginx\u200b",      // zero width space
		"ng\u0131nx",       // look-alike letter
		"-privileged",      // reads as a flag
		"has space",        //
		"nginx;rm -rf /",   //
		"nginx\nimage: x",  // newline
		"ghcr.io/o/a:1.0 ", // trailing space
		"/absolute/path",   // must start with a letter or digit
		strings.Repeat("a", maxImageLength+1),
	}
	for _, image := range bad {
		e := newEnv(t, Options{}, imageObjects()...)
		for path, body := range map[string]map[string]any{
			"/api/deployments/image":  withFields(imageBody(), map[string]any{"image": image, "dryRun": true}),
			"/api/deployments/create": withFields(createBody(), map[string]any{"image": image, "dryRun": true}),
		} {
			if rec := e.do(http.MethodPost, path, body); rec.Code != http.StatusBadRequest {
				t.Errorf("%s accepted image %q (status %d)", path, image, rec.Code)
			}
		}
	}

	e := newEnv(t, Options{}, imageObjects()...)
	for _, image := range []string{"nginx", "nginx:1.27-alpine", "ghcr.io/org/app:1.2.3", "registry.local:5000/team/app@sha256:0123abcd"} {
		body := withFields(imageBody(), map[string]any{"image": image, "dryRun": true})
		if rec := e.do(http.MethodPost, "/api/deployments/image", body); rec.Code != http.StatusOK {
			t.Errorf("image %q refused: %d %s", image, rec.Code, rec.Body.String())
		}
	}
}

func TestExtremeQuantitiesAreRefused(t *testing.T) {
	for _, tc := range []struct {
		field, value string
		ok           bool
	}{
		{"cpuRequest", "1e9999", false},
		{"cpuLimit", "2000", false},
		{"memoryLimit", "9999999999999999999Ei", false},
		{"memoryRequest", "17Ti", false},
		{"cpuRequest", "-1", false},
		{"cpuLimit", "4", true},
		{"memoryLimit", "16Gi", true},
		{"cpuLimit", "250m", true},
	} {
		e := newEnv(t, Options{}) // the fake stores dry-run creates, so one cluster per case
		rec := e.do(http.MethodPost, "/api/deployments/create", withFields(createBody(), map[string]any{tc.field: tc.value, "dryRun": true}))
		if ok := rec.Code == http.StatusOK; ok != tc.ok {
			t.Errorf("%s=%q: status %d (want ok=%v): %s", tc.field, tc.value, rec.Code, tc.ok, rec.Body.String())
		}
	}
}

func TestServiceAndDeploymentOnlySelectConsolePods(t *testing.T) {
	e := newEnv(t, Options{})
	body := withFields(createBody(), map[string]any{"expose": map[string]any{"enabled": true, "type": "LoadBalancer", "port": 80}})
	if rec := e.do(http.MethodPost, "/api/deployments/create", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	d := storedDeployment(t, e, "team-a", "web")
	svc, err := e.client.CoreV1().Services("team-a").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, selector := range map[string]map[string]string{"deployment": d.Spec.Selector.MatchLabels, "service": svc.Spec.Selector} {
		if selector["app"] != "web" || selector[managedByLabel] != managedByValue {
			t.Errorf("%s selector = %v; it must also require %s=%s, or it would match another team's pods labelled app=web",
				name, selector, managedByLabel, managedByValue)
		}
	}
	for key, want := range d.Spec.Selector.MatchLabels {
		if d.Spec.Template.Labels[key] != want {
			t.Errorf("pod template does not carry selector label %s=%s", key, want)
		}
	}
}

func TestRollbackDeletesOnlyTheDeploymentItCreated(t *testing.T) {
	e := newEnv(t, Options{})
	e.client.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		d := action.(k8stesting.CreateActionImpl).GetObject().(*appsv1.Deployment).DeepCopy()
		d.UID = "uid-created-here"
		if err := e.client.Tracker().Create(deploymentsGVR, d, d.Namespace); err != nil {
			return true, nil, err
		}
		return true, d, nil
	})
	e.client.PrependReactor("create", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "services"}, "web")
	})
	var preconditions *metav1.Preconditions
	e.client.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		preconditions = action.(k8stesting.DeleteActionImpl).GetDeleteOptions().Preconditions
		return false, nil, nil
	})

	body := withFields(createBody(), map[string]any{"expose": map[string]any{"enabled": true, "port": 80}})
	if rec := e.do(http.MethodPost, "/api/deployments/create", body); rec.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if preconditions == nil || preconditions.UID == nil || *preconditions.UID != "uid-created-here" {
		t.Errorf("rollback delete preconditions = %+v; it must be pinned to the UID it created", preconditions)
	}
}

func TestScaleHasAnUpperBound(t *testing.T) {
	e := newEnv(t, Options{}, deployment("team-a", "api", 2, 2))
	for _, replicas := range []int{1001, 100000, 2147483647} {
		rec := e.do(http.MethodPost, "/api/scale", map[string]any{"namespace": "team-a", "name": "api", "replicas": replicas})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%d replicas = %d; want 400", replicas, rec.Code)
		}
	}
	if replicasOf(t, e.client, "team-a", "api") != 2 {
		t.Error("the deployment was scaled by a refused request")
	}
	if rec := e.do(http.MethodPost, "/api/scale", map[string]any{"namespace": "team-a", "name": "api", "replicas": maxScaleReplicas, "dryRun": true}); rec.Code != http.StatusOK {
		t.Errorf("the limit itself = %d; want 200", rec.Code)
	}
}

// ---- concurrent edits: retry status churn, never overwrite somebody's change -------------

// failOnceAfter answers the first deployment update with a conflict, after
// running mutate on the stored object: what happens when something else wrote
// in between.
func failOnceAfter(e *env, mutate func(*appsv1.Deployment)) {
	var fired atomic.Bool
	e.client.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "" || !fired.CompareAndSwap(false, true) {
			return false, nil, nil
		}
		stored, err := e.client.Tracker().Get(deploymentsGVR, "team-a", "web")
		if err != nil {
			return true, nil, err
		}
		changed := stored.(*appsv1.Deployment).DeepCopy()
		mutate(changed)
		if err := e.client.Tracker().Update(deploymentsGVR, changed, "team-a"); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", nil)
	})
}

func TestImageUpdateDoesNotOverwriteSomeoneElsesImageChange(t *testing.T) {
	e := newEnv(t, Options{}, imageObjects()...)
	failOnceAfter(e, func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Image = "theirs:2" })

	rec := e.do(http.MethodPost, "/api/deployments/image", imageBody())
	if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "conflict" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := storedDeployment(t, e, "team-a", "web").Spec.Template.Spec.Containers[0].Image; got != "theirs:2" {
		t.Errorf("image = %q; the other person's change was overwritten", got)
	}
}

func TestImageUpdateKeepsUnrelatedChangesMadeMeanwhile(t *testing.T) {
	e := newEnv(t, Options{}, imageObjects()...)
	failOnceAfter(e, func(d *appsv1.Deployment) { nine := int32(9); d.Spec.Replicas = &nine })

	if rec := e.do(http.MethodPost, "/api/deployments/image", imageBody()); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	d := storedDeployment(t, e, "team-a", "web")
	if d.Spec.Template.Spec.Containers[0].Image != "nginx:1.27" || *d.Spec.Replicas != 9 {
		t.Errorf("image %q replicas %d; want the new image and the replicas set meanwhile", d.Spec.Template.Spec.Containers[0].Image, *d.Spec.Replicas)
	}
}

func reviewEnvObjects() []runtime.Object {
	return []runtime.Object{wlDeployment("team-a", "web", 2, corev1.Container{
		Name: "app", Image: "nginx", Env: []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "info"}},
	})}
}

func reviewEnvBody() map[string]any {
	return map[string]any{"namespace": "team-a", "name": "web", "container": "app", "set": map[string]string{"LOG_LEVEL": "debug"}}
}

func TestEnvUpdateDoesNotOverwriteSomeoneElsesChangeToTheSameVariable(t *testing.T) {
	e := newEnv(t, Options{}, reviewEnvObjects()...)
	failOnceAfter(e, func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Env[0].Value = "trace" })

	rec := e.do(http.MethodPost, "/api/deployments/env", reviewEnvBody())
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := storedDeployment(t, e, "team-a", "web").Spec.Template.Spec.Containers[0].Env[0].Value; got != "trace" {
		t.Errorf("LOG_LEVEL = %q; the other person's value was overwritten", got)
	}
}

func TestEnvUpdateRefusesWhenTheVariableBecameASecretReferenceMeanwhile(t *testing.T) {
	e := newEnv(t, Options{}, reviewEnvObjects()...)
	failOnceAfter(e, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Env[0] = corev1.EnvVar{Name: "LOG_LEVEL", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "level"},
		}}
	})

	if rec := e.do(http.MethodPost, "/api/deployments/env", reviewEnvBody()); rec.Code == http.StatusOK {
		t.Fatalf("a variable that now comes from a Secret was overwritten: %s", rec.Body.String())
	}
	if env := storedDeployment(t, e, "team-a", "web").Spec.Template.Spec.Containers[0].Env[0]; env.ValueFrom == nil {
		t.Errorf("the Secret reference was replaced by %+v", env)
	}
}

func TestEnvUpdateKeepsOtherVariablesAddedMeanwhile(t *testing.T) {
	e := newEnv(t, Options{}, reviewEnvObjects()...)
	failOnceAfter(e, func(d *appsv1.Deployment) {
		c := &d.Spec.Template.Spec.Containers[0]
		c.Env = append(c.Env, corev1.EnvVar{Name: "THEIRS", Value: "1"})
	})

	if rec := e.do(http.MethodPost, "/api/deployments/env", reviewEnvBody()); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := map[string]string{}
	for _, v := range storedDeployment(t, e, "team-a", "web").Spec.Template.Spec.Containers[0].Env {
		got[v.Name] = v.Value
	}
	if got["LOG_LEVEL"] != "debug" || got["THEIRS"] != "1" {
		t.Errorf("env = %v; want LOG_LEVEL=debug and the variable added meanwhile", got)
	}
}

// ---- what the review screen shows ---------------------------------------------------------

func TestInvisibleCharactersAreShownAsEscapes(t *testing.T) {
	if got := short("a\u202eb\u200bc"); got != `a\u202eb\u200bc` {
		t.Errorf("short = %q", got)
	}
	if got := short("line1\nline2"); got != "line1 line2" {
		t.Errorf("short = %q", got)
	}

	e := newEnv(t, Options{}, reviewEnvObjects()...)
	body := map[string]any{"namespace": "team-a", "name": "web", "container": "app", "set": map[string]string{"NEW_VAR": "safe\u202etxt.exe"}, "dryRun": true}
	rec := e.do(http.MethodPost, "/api/deployments/env", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, line := range raw["changes"].([]any) {
		if strings.ContainsRune(line.(string), '\u202e') {
			t.Errorf("a change line carries a raw override character: %q", line)
		}
	}
}

func TestChangesIsAlwaysAnArray(t *testing.T) {
	out, err := json.Marshal(changeResponse{Name: "x"})
	if err != nil || !strings.Contains(string(out), `"changes":[]`) {
		t.Errorf("marshal = %s (%v)", out, err)
	}

	// The same through a real endpoint: an unchanged autoscaler has nothing to list.
	e := newEnv(t, Options{}, wlDeployment("team-a", "web", 3, withCPURequest(corev1.Container{Name: "app", Image: "nginx"}, "100m")))
	body := map[string]any{"namespace": "team-a", "deployment": "web", "min": 2, "max": 5, "cpuPercent": 70}
	e.do(http.MethodPost, "/api/autoscalers", body)
	rec := e.do(http.MethodPost, "/api/autoscalers", body)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"changes":null`) || !strings.Contains(rec.Body.String(), `"unchanged"`) {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

// ---- labels ---------------------------------------------------------------------------------

func TestLongNamesDoNotBreakTheAppLabel(t *testing.T) {
	long := strings.Repeat("a", 70) // a valid object name, too long to be a label value
	e := newEnv(t, Options{}, wlDeployment("team-a", long, 3, withCPURequest(corev1.Container{Name: "app", Image: "nginx"}, "100m")))

	rec := e.do(http.MethodPost, "/api/autoscalers", map[string]any{"namespace": "team-a", "deployment": long, "min": 1, "max": 3, "cpuPercent": 50})
	if rec.Code != http.StatusOK {
		t.Fatalf("autoscaler: %d %s", rec.Code, rec.Body.String())
	}
	hpa, err := e.client.AutoscalingV2().HorizontalPodAutoscalers("team-a").Get(context.Background(), long, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := hpa.Labels["app"]; has || hpa.Labels[managedByLabel] != managedByValue {
		t.Errorf("labels = %v; want only the managed-by label for a name that is not a valid label value", hpa.Labels)
	}

	rec = e.do(http.MethodPost, "/api/configmaps", map[string]any{"namespace": "team-a", "name": long, "data": map[string]string{"a": "b"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("configmap: %d %s", rec.Code, rec.Body.String())
	}
	if labels := managedLabels("web"); labels["app"] != "web" || labels[managedByLabel] != managedByValue {
		t.Errorf("managedLabels(web) = %v", labels)
	}
}

func TestNamespaceLabelsUnderKubernetesDomainsAreReserved(t *testing.T) {
	// The fake clientset stores dry-run creates, so each case gets its own cluster.
	try := func(key string) *httptest.ResponseRecorder {
		e := newEnv(t, Options{})
		return e.do(http.MethodPost, "/api/namespaces", map[string]any{"name": "fresh", "labels": map[string]string{key: "x"}, "dryRun": true})
	}
	for _, key := range []string{
		"kubernetes.io/metadata.name",
		"pod-security.kubernetes.io/enforce",
		"foo.k8s.io/bar",
		"K8S.IO/x",
		"node-restriction.kubernetes.io/y",
	} {
		if rec := try(key); rec.Code != http.StatusBadRequest {
			t.Errorf("label %q = %d; want 400", key, rec.Code)
		}
	}
	for _, key := range []string{"team", "team.example.com/owner", "mykubernetes.io.example.com/x"} {
		if rec := try(key); rec.Code != http.StatusOK {
			t.Errorf("label %q = %d (%s); want 200", key, rec.Code, rec.Body.String())
		}
	}
}

// ---- ConfigMap sizes really reach the handler -------------------------------------------------

func TestConfigMapValuesUpToTheLimitFitInTheRequest(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "team-a"}, Data: map[string]string{"a": "b"}}
	e := newEnv(t, Options{}, cm)

	big := strings.Repeat("x", 200<<10) // far over the generic 64 KiB body limit
	rec := e.do(http.MethodPost, "/api/configmaps/data", map[string]any{"namespace": "team-a", "name": "cfg", "key": "big", "value": big})
	if rec.Code != http.StatusOK {
		t.Fatalf("a 200 KiB value = %d: %.200s", rec.Code, rec.Body.String())
	}
	stored, _ := e.client.CoreV1().ConfigMaps("team-a").Get(context.Background(), "cfg", metav1.GetOptions{})
	if len(stored.Data["big"]) != len(big) {
		t.Errorf("stored %d bytes", len(stored.Data["big"]))
	}

	tooBig := strings.Repeat("x", cmMaxValueBytes+1)
	rec = e.do(http.MethodPost, "/api/configmaps/data", map[string]any{"namespace": "team-a", "name": "cfg", "key": "huge", "value": tooBig})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "KiB") {
		t.Errorf("a value over the limit = %d: %.200s", rec.Code, rec.Body.String())
	}

	rec = e.do(http.MethodPost, "/api/configmaps/data", map[string]any{"namespace": "team-a", "name": "cfg", "key": "k", "value": strings.Repeat("y", cmMaxBodyBytes)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a body over the limit = %d", rec.Code)
	}
}

// ---- every write endpoint is guarded before it touches the cluster -------------------------------

func writeEndpoints() map[string]map[string]any {
	return map[string]map[string]any{
		"/api/namespaces":          {"name": "fresh"},
		"/api/configmaps":          {"namespace": "team-a", "name": "cfg", "data": map[string]string{"a": "b"}},
		"/api/configmaps/data":     {"namespace": "team-a", "name": "cfg", "key": "a", "value": "b"},
		"/api/deployments/create":  createBody(),
		"/api/deployments/image":   imageBody(),
		"/api/deployments/restart": restartBody(),
		"/api/deployments/env":     reviewEnvBody(),
		"/api/autoscalers":         {"namespace": "team-a", "deployment": "web", "min": 1, "max": 2, "cpuPercent": 50},
		"/api/autoscalers/delete":  {"namespace": "team-a", "name": "web"},
		"/api/scale":               {"namespace": "team-a", "name": "web", "replicas": 3},
	}
}

func TestReadOnlyRefusesEveryWriteBeforeAnyClusterCall(t *testing.T) {
	for path, body := range writeEndpoints() {
		e := newEnv(t, Options{ReadOnly: true}, reviewEnvObjects()...)
		rec := e.do(http.MethodPost, path, body)
		if rec.Code != http.StatusForbidden || decode[errorResponse](t, rec).Code != "read_only" {
			t.Errorf("%s: status %d: %s", path, rec.Code, rec.Body.String())
		}
		if actions := e.client.Actions(); len(actions) != 0 {
			t.Errorf("%s: %d cluster calls were made before the refusal: %v", path, len(actions), actions)
		}
	}
}

func TestProductionRefusesEveryWriteWithoutTheContextName(t *testing.T) {
	for path, body := range writeEndpoints() {
		if path == "/api/scale" {
			continue // reads the current scale first, by design; covered in web_test.go
		}
		for _, confirm := range []string{"", "PROD-EU", " prod-eu", "prod-eu ", "prod", "dev"} {
			e := newEnv(t, Options{}, reviewEnvObjects()...)
			rec := e.do(http.MethodPost, path, withFields(body, map[string]any{"context": "prod-eu", "confirm": confirm}))
			if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "confirmation_required" {
				t.Errorf("%s confirm=%q: status %d: %s", path, confirm, rec.Code, rec.Body.String())
			}
			if actions := e.client.Actions(); len(actions) != 0 {
				t.Errorf("%s confirm=%q: %d cluster calls before the refusal", path, confirm, len(actions))
			}
		}
	}
}

func TestContextNameVariantsDoNotSlipPastTheProductionGate(t *testing.T) {
	e := newEnv(t, Options{})
	for _, name := range []string{"PROD-EU", "Prod-eu", " prod-eu", "prod-eu\n", "prod-eu\u200b"} {
		rec := e.do(http.MethodPost, "/api/namespaces", map[string]any{"context": name, "name": "fresh"})
		if rec.Code != http.StatusNotFound || decode[errorResponse](t, rec).Code != "unknown_context" {
			t.Errorf("context %q = %d: %s", name, rec.Code, rec.Body.String())
		}
	}
	if len(e.client.Actions()) != 0 {
		t.Error("an unknown context reached the cluster")
	}
}

func TestAnEmptyContextResolvingToProductionNeedsConfirmation(t *testing.T) {
	e := newEnv(t, Options{})
	moved := strings.Replace(testKubeconfig, "current-context: dev", "current-context: prod-eu", 1)
	if err := os.WriteFile(e.kubeconfig, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := e.do(http.MethodPost, "/api/namespaces", map[string]any{"name": "fresh"}) // no context at all
	if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "confirmation_required" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodPost, "/api/namespaces", map[string]any{"name": "fresh", "confirm": "prod-eu"}); rec.Code != http.StatusOK {
		t.Errorf("the right confirmation = %d: %s", rec.Code, rec.Body.String())
	}
}
