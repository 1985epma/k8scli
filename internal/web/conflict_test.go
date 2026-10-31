package web

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// failUpdates makes the first n updates of deployments answer with a conflict,
// the way the API server does while a rollout keeps rewriting the status.
func failUpdates(e *env, n int32) *atomic.Int32 {
	var seen atomic.Int32
	e.client.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "" {
			return false, nil, nil
		}
		if seen.Add(1) <= n {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", errors.New("status changed"))
		}
		return false, nil, nil
	})
	return &seen
}

func TestImageUpdateSurvivesConflictsCausedByARollout(t *testing.T) {
	e := newEnv(t, Options{}, imageObjects()...)
	attempts := failUpdates(e, 2)

	rec := e.do(http.MethodPost, "/api/deployments/image", imageBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if attempts.Load() != 3 {
		t.Errorf("update attempts = %d; want 3 (two conflicts, then success)", attempts.Load())
	}
	if got := storedDeployment(t, e, "team-a", "web").Spec.Template.Spec.Containers[0].Image; got != "nginx:1.27" {
		t.Errorf("image = %q after the retry", got)
	}
}

func TestEnvUpdateSurvivesConflictsAndKeepsNewerChanges(t *testing.T) {
	e := newEnv(t, Options{}, imageObjects()...)
	attempts := failUpdates(e, 1)

	body := map[string]any{"namespace": "team-a", "name": "web", "container": "app", "set": map[string]string{"LOG_LEVEL": "debug"}}
	rec := e.do(http.MethodPost, "/api/deployments/env", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if attempts.Load() != 2 {
		t.Errorf("update attempts = %d; want 2", attempts.Load())
	}
	env := storedDeployment(t, e, "team-a", "web").Spec.Template.Spec.Containers[0].Env
	if len(env) != 1 || env[0].Name != "LOG_LEVEL" || env[0].Value != "debug" {
		t.Errorf("env = %+v", env)
	}
}

func TestPersistentConflictStillEndsAs409(t *testing.T) {
	e := newEnv(t, Options{}, imageObjects()...)
	attempts := failUpdates(e, 1000)

	rec := e.do(http.MethodPost, "/api/deployments/image", imageBody())
	if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "conflict" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
	if attempts.Load() != conflictRetries+1 {
		t.Errorf("update attempts = %d; want %d", attempts.Load(), conflictRetries+1)
	}
}
