package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/repo"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	"sigs.k8s.io/yaml"
)

// liveEnv turns on the tests that use the network. They check the catalog against
// the upstream repositories, and are what scripts/update-helm-catalog.sh runs.
const liveEnv = "K8SCLI_HELM_LIVE"

// cooldown skips chart versions published less than this long ago, so a release
// that is pulled or fixed within days is not pinned.
const cooldown = 7 * 24 * time.Hour

func liveGet(t *testing.T, address string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", address, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", address, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		t.Fatalf("GET %s: %v", address, err)
	}
	return data
}

// TestHelmCatalogUpstream verifies every pinned archive against its digest and
// prints the newest stable version of each chart, for a maintainer to review.
// It never edits the catalog.
func TestHelmCatalogUpstream(t *testing.T) {
	if os.Getenv(liveEnv) != "1" {
		t.Skipf("set %s=1 to check the catalog against the upstream repositories", liveEnv)
	}

	for _, entry := range helmCatalog {
		t.Run(entry.ID, func(t *testing.T) {
			// 1. The pinned archive still has the pinned digest.
			sum := sha256.Sum256(liveGet(t, entry.archiveURL))
			if got := hex.EncodeToString(sum[:]); got != entry.digest {
				t.Errorf("the pinned archive of %s changed upstream: sha256 is %s, the catalog says %s", entry.ID, got, entry.digest)
			}

			// 2. What is the newest version a maintainer could move to?
			base := strings.TrimSuffix(entry.Repo, "/") + "/"
			var index repo.IndexFile
			if err := yaml.Unmarshal(liveGet(t, base+"index.yaml"), &index); err != nil {
				t.Fatalf("index of %s: %v", entry.Repo, err)
			}
			index.SortEntries()

			for _, version := range index.Entries[entry.Chart] {
				if version.Deprecated || strings.Contains(version.Version, "-") {
					continue
				}
				if time.Since(version.Created) < cooldown {
					continue
				}
				archive := ""
				if len(version.URLs) > 0 {
					resolved, err := url.Parse(base)
					if err == nil {
						if ref, err := url.Parse(version.URLs[0]); err == nil {
							archive = resolved.ResolveReference(ref).String()
						}
					}
				}
				status := "NEWER"
				if version.Version == entry.Version {
					status = "current"
				}
				fmt.Printf("CATALOG %-22s pinned=%-10s newest=%-10s (%s)\n", entry.ID, entry.Version, version.Version, status)
				fmt.Printf("CATALOG %-22s   digest=%s\n", entry.ID, version.Digest)
				fmt.Printf("CATALOG %-22s   url=%s\n", entry.ID, archive)
				fmt.Printf("CATALOG %-22s   published=%s\n", entry.ID, version.Created.Format("2006-01-02"))
				return
			}
			t.Errorf("no stable version of %s older than %s in %s", entry.Chart, cooldown, entry.Repo)
		})
	}
}

// renderOffline renders a chart like helm template: no cluster is involved.
func renderOffline(t *testing.T, loaded *chart.Chart, values map[string]any) string {
	t.Helper()
	cfg := &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities.Copy(),
		Log:          func(string, ...any) {},
	}
	install := action.NewInstall(cfg)
	install.ReleaseName, install.Namespace = "check", "demo"
	install.DryRun, install.ClientOnly, install.IncludeCRDs = true, true, true
	install.KubeVersion = &chartutil.KubeVersion{Version: "v1.36.0", Major: "1", Minor: "36"}
	rel, err := install.Run(loaded, deepCopyMap(values))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return rel.Manifest
}

// alternative picks a valid value that differs from the field's default.
func alternative(f *helmField) (string, bool) {
	switch f.Type {
	case fieldBool:
		if f.Default == "true" {
			return "false", true
		}
		return "true", true
	case fieldInt:
		if fmt.Sprint(f.Max) != f.Default {
			return fmt.Sprint(f.Max), true
		}
		return fmt.Sprint(f.Min), true
	case fieldSelect:
		for _, option := range f.Options {
			if option != f.Default {
				return option, true
			}
		}
	case fieldString:
		for _, candidate := range []string{"owner-1", "example.com", "72h", "0 3 * * *", "hello"} {
			if candidate != f.Default && f.validate(candidate) == nil {
				return candidate, true
			}
		}
	}
	return "", false
}

// onlyWhen lists the fields that matter only while another field has a given
// value, so the effect test sets that one first.
var onlyWhen = map[string]map[string]string{
	"alloy.replicas":       {"controller": "deployment"},
	"descheduler.schedule": {"kind": "CronJob"},
}

// TestHelmCatalogFieldsHaveAnEffect proves, for every field of every entry, that
// the chart value it writes exists: the rendered chart changes when the field
// does, and the chart renders with the defaults.
func TestHelmCatalogFieldsHaveAnEffect(t *testing.T) {
	if os.Getenv(liveEnv) != "1" {
		t.Skipf("set %s=1 to render the catalog charts", liveEnv)
	}
	engine := newHelmEngine(&Server{})

	for i := range helmCatalog {
		entry := &helmCatalog[i]
		t.Run(entry.ID, func(t *testing.T) {
			loaded, err := engine.loadChart(t.Context(), entry)
			if err != nil {
				t.Fatal(err)
			}
			defaults := entry.fieldDefaults()
			base := renderOffline(t, loaded, entry.buildValues(nil, defaults, false, false))
			if strings.TrimSpace(base) == "" {
				t.Fatalf("%s renders nothing with its defaults", entry.ID)
			}

			for _, f := range entry.Fields {
				if f.Secret {
					continue
				}
				changed, ok := alternative(&f)
				if !ok {
					t.Errorf("%s.%s: no alternative value to try", entry.ID, f.Key)
					continue
				}
				fields := entry.fieldDefaults()
				reference := base
				if setup := onlyWhen[entry.ID+"."+f.Key]; setup != nil {
					for key, value := range setup {
						fields[key] = value
					}
					reference = renderOffline(t, loaded, entry.buildValues(nil, fields, false, false))
				}
				fields[f.Key] = changed
				other := renderOffline(t, loaded, entry.buildValues(nil, fields, false, false))
				if other == reference {
					t.Errorf("%s.%s: setting it to %q changes nothing in the rendered chart (wrong value path?)", entry.ID, f.Key, changed)
				}
			}
		})
	}
}
