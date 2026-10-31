package web

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
)

// TestProductionClientsReachTheRealInternet proves that the connect-time address guard lets the
// legitimate traffic through: every pinned catalog archive downloads and matches its digest, and
// Artifact Hub answers a search and a package lookup, all through the production clients.
// Run it with K8SCLI_HELM_LIVE=1.
func TestProductionClientsReachTheRealInternet(t *testing.T) {
	if os.Getenv(liveEnv) != "1" {
		t.Skipf("set %s=1 to run this against the real internet", liveEnv)
	}
	engine := newHelmEngine(&Server{})

	for i := range helmCatalog {
		entry := &helmCatalog[i]
		t.Run(entry.ID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if _, err := engine.archive(ctx, entry); err != nil {
				t.Fatalf("%s: %v", entry.ID, err)
			}
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := engine.hub.search(ctx, engine.catalog, "podinfo", 1)
	if err != nil || len(result.Items) == 0 {
		t.Fatalf("Artifact Hub search: %v %+v", err, result)
	}
	item := result.Items[0]
	ref, ok := engine.hub.lookup(item.ID)
	if !ok {
		t.Fatal("the search did not remember the package")
	}
	detail, err := engine.hub.detail(ctx, ref, "")
	if err != nil || detail.ContentURL == "" {
		t.Fatalf("Artifact Hub package lookup: %v", err)
	}
	if _, sum, err := engine.download(ctx, detail.ContentURL, detail.Name); err != nil || sum == "" {
		t.Fatalf("download of a package from search: %v", err)
	}
}

// TestALookupOfASecretIsBlockedOnARealCluster renders a chart whose template looks up a Secret, as a
// server-side dry run against the docker-desktop context (nothing is created, and the Secret it
// asks for does not exist, so no Secret data is read). Without the Secret-blind client the render
// works and the lookup answers "nothing"; with it the render fails, which is the point.
func TestALookupOfASecretIsBlockedOnARealCluster(t *testing.T) {
	if os.Getenv(liveEnv) != "1" {
		t.Skipf("set %s=1 to run this against the docker-desktop cluster", liveEnv)
	}
	const contextName = "docker-desktop"
	server := &Server{}

	render := func(blind bool) error {
		cfg, err := server.newHelmConfig(contextName, "k8scli-helm-review")
		if err != nil {
			t.Skipf("no usable %s context: %v", contextName, err)
		}
		if blind {
			blindToSecrets(cfg)
		}
		c := &chart.Chart{
			Metadata: &chart.Metadata{APIVersion: "v2", Name: "lookupcheck", Version: "0.1.0"},
			Templates: []*chart.File{{Name: "templates/note.yaml", Data: []byte(
				`{{ $s := lookup "v1" "Secret" "kube-system" "k8scli-review-does-not-exist" }}{{ if $s }}# found{{ else }}# absent{{ end }}`)}},
		}
		install := action.NewInstall(cfg)
		install.ReleaseName, install.Namespace = "lookupcheck", "k8scli-helm-review"
		install.DryRun, install.DryRunOption = true, "server"
		_, err = install.Run(c, map[string]any{})
		return err
	}

	if err := render(false); err != nil {
		t.Skipf("the control render failed (%v); the cluster may be unreachable", err)
	}
	err := render(true)
	if err == nil {
		t.Fatal("a Secret lookup rendered with the Secret-blind client: the guard is not in effect")
	}
	if !strings.Contains(err.Error(), "Secrets") {
		t.Errorf("the render failed, but not because of the guard: %v", err)
	}
	t.Logf("blocked as intended: %v", err)
}
