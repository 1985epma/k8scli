package web

import (
	"encoding/json"
	"fmt"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// managedByLabel marks objects created by the console (ADR 0003).
const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "k8scli"
)

// mutation is the part every write request shares.
type mutation struct {
	Context string `json:"context"`
	// DryRun asks the API server to validate the request without storing it.
	DryRun bool `json:"dryRun"`
	// Confirm must equal the context name when the context looks like production.
	Confirm string `json:"confirm"`
}

// authorize applies the rules of ADR 0003 to a write request and returns the
// resolved context name. Dry runs always pass: they change nothing.
func (s *Server) authorize(m mutation) (string, error) {
	contextName, err := s.resolveContext(m.Context)
	if err != nil {
		return "", err
	}
	if m.DryRun {
		return contextName, nil
	}
	if s.opts.ReadOnly {
		return "", &apiError{status: http.StatusForbidden, code: "read_only", msg: "the console is running in read-only mode"}
	}
	if looksLikeProduction(contextName) && m.Confirm != contextName {
		return "", &apiError{status: http.StatusConflict, code: "confirmation_required",
			msg: fmt.Sprintf("context %q looks like production: type its name to confirm", contextName)}
	}
	return contextName, nil
}

// dryRunOption is what to pass as DryRun in create, update and patch options.
func dryRunOption(dryRun bool) []string {
	if dryRun {
		return []string{metav1.DryRunAll}
	}
	return nil
}

// managedLabels returns the labels the console puts on what it creates. The
// "app" label is left out when the name cannot be a label value (an HPA or a
// ConfigMap may be named with up to 253 characters), which would make the
// API server reject the whole object.
func managedLabels(app string) map[string]string {
	labels := map[string]string{managedByLabel: managedByValue}
	if len(validation.IsValidLabelValue(app)) == 0 {
		labels["app"] = app
	}
	return labels
}

// selectorLabels is what a Deployment and its Service select on. It includes the
// managed-by label so a Service created here can never route to pods that
// merely share the "app" label (for example another team's workload), and the
// Deployment never adopts them.
func selectorLabels(app string) map[string]string {
	return map[string]string{"app": app, managedByLabel: managedByValue}
}

// changeResponse is the answer of every write action, so the UI can show a
// preview (dry run) and a result the same way.
type changeResponse struct {
	Context   string `json:"context"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Action is one of created, updated, deleted, restarted or unchanged.
	Action string `json:"action"`
	DryRun bool   `json:"dryRun"`
	// Summary is one human sentence, e.g. "Deployment api would be created with 3 replicas".
	Summary string `json:"summary"`
	// Changes lists what differs, one line each, e.g. "image: nginx:1.25 -> nginx:1.27".
	Changes []string `json:"changes"`
	// Equivalent is the kubectl command that does the same.
	Equivalent string `json:"equivalent"`
}

// MarshalJSON keeps "changes" an array when there is nothing to list: a nil
// slice would be sent as null, which the UI does not expect.
func (c changeResponse) MarshalJSON() ([]byte, error) {
	type plain changeResponse
	if c.Changes == nil {
		c.Changes = []string{}
	}
	return json.Marshal(plain(c))
}
