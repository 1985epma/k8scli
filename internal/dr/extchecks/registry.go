package extchecks

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/1985epma/k8scli/internal/dr/checks"
)

// imageHost returns the registry host of an image reference ("nginx" and "library/nginx" are docker.io).
func imageHost(image string) string {
	i := strings.Index(image, "/")
	if i < 0 {
		return "docker.io"
	}
	first := image[:i]
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return strings.ToLower(first)
	}
	return "docker.io"
}

var (
	ecrHost = regexp.MustCompile(`^\d{12}\.dkr\.ecr(?:-fips)?\.([a-z0-9-]+)\.amazonaws\.com(?:\.cn)?$`)
	arHost  = regexp.MustCompile(`^([a-z0-9-]+)-docker\.pkg\.dev$`)
)

// multiRegion are Artifact Registry locations that span several regions.
var multiRegion = map[string]bool{"us": true, "europe": true, "asia": true}

// registryRegion returns the region a registry host is bound to, when the host name says so.
func registryRegion(host string) (string, bool) {
	if m := ecrHost.FindStringSubmatch(host); m != nil {
		return m[1], true
	}
	if m := arHost.FindStringSubmatch(host); m != nil && !multiRegion[m[1]] {
		return m[1], true
	}
	return "", false
}

var publicHosts = map[string]bool{
	"docker.io": true, "index.docker.io": true, "registry-1.docker.io": true, "ghcr.io": true, "quay.io": true,
	"registry.k8s.io": true, "k8s.gcr.io": true, "public.ecr.aws": true, "mcr.microsoft.com": true,
}

func isDockerHub(host string) bool {
	return host == "docker.io" || host == "index.docker.io" || host == "registry-1.docker.io"
}

// cloudRegistry reports the registries whose nodes normally authenticate through the cloud account, so a
// missing imagePullSecret is not a problem by itself.
func cloudRegistry(host string) bool {
	return ecrHost.MatchString(host) || arHost.MatchString(host) || host == "gcr.io" || strings.HasSuffix(host, ".gcr.io") ||
		strings.HasSuffix(host, ".azurecr.io") || strings.HasSuffix(host, ".azurecr.cn")
}

func privateLooking(host string) bool { return !publicHosts[host] && !cloudRegistry(host) }

func (r *runner) checkRegistries(w *workload, set *nsData, target *nameSets, targetName string) {
	pull := map[string]bool{}
	for _, ref := range w.Template.Spec.ImagePullSecrets {
		if ref.Name != "" {
			pull[ref.Name] = true
		}
	}
	account := w.Template.Spec.ServiceAccountName
	if account == "" {
		account = "default"
	}
	for _, name := range set.pullSecrets[account] {
		pull[name] = true
	}

	if target != nil && target.secrets != nil {
		for _, name := range sortedKeys(pull) {
			if !target.secrets[name] {
				r.add(checks.Finding{Rule: "DR102", Severity: checks.SeverityHigh, Kind: w.Kind, Name: w.Name, Namespace: w.Namespace,
					Message: fmt.Sprintf("imagePullSecret %q is not in %s", name, targetName),
					Fix:     "create the pull secret in " + w.Namespace + " there (the value is never read by k8scli)"})
			}
		}
	}

	seen := map[string]bool{}
	for _, use := range w.imageUses() {
		host := use.Host
		if seen[host] {
			continue
		}
		seen[host] = true
		switch {
		case func() bool { _, regional := registryRegion(host); return regional }():
			region, _ := registryRegion(host)
			r.add(checks.Finding{Rule: "DR101", Severity: checks.SeverityMedium, Kind: w.Kind, Name: w.Name, Namespace: w.Namespace,
				Message: fmt.Sprintf("image %s is in %s, a registry bound to region %s", use.Image, host, region),
				Fix:     "replicate the repository to the DR region and pull from there (for ECR: cross-region replication)"})
		case isDockerHub(host) && len(pull) == 0:
			r.add(checks.Finding{Rule: "DR103", Severity: checks.SeverityLow, Kind: w.Kind, Name: w.Name, Namespace: w.Namespace,
				Message: fmt.Sprintf("image %s is pulled anonymously from Docker Hub", use.Image),
				Fix:     "pull through a mirror or add an imagePullSecret with a Docker Hub account"})
		case privateLooking(host) && len(pull) == 0:
			r.add(checks.Finding{Rule: "DR104", Severity: checks.SeverityLow, Kind: w.Kind, Name: w.Name, Namespace: w.Namespace,
				Message: fmt.Sprintf("image %s comes from %s and neither the pod nor its service account has pull credentials", use.Image, host),
				Fix:     "ignore this if the nodes authenticate on their own, otherwise add an imagePullSecret"})
		}
	}
}

// summarizeRegistries raises DR105 when every image of the checked namespaces comes from one host.
func (r *runner) summarizeRegistries(uses []imageUse) {
	hosts := map[string]int{}
	for _, u := range uses {
		hosts[u.Host]++
	}
	r.count("images", len(uses))
	if len(hosts) == 1 && len(uses) >= 2 {
		for host, n := range hosts {
			r.add(checks.Finding{Rule: "DR105", Severity: checks.SeverityLow, Kind: "Registry", Name: host,
				Message: fmt.Sprintf("all %d images come from %s", n, host),
				Fix:     "mirror the images you cannot rebuild to a second registry"})
		}
	}
}

func registryList(uses []imageUse) []RegistryUse {
	counts := map[string]int{}
	for _, u := range uses {
		counts[u.Host]++
	}
	out := make([]RegistryUse, 0, len(counts))
	for host, n := range counts {
		region, _ := registryRegion(host)
		out = append(out, RegistryUse{Host: host, Images: n, Region: region, Private: privateLooking(host)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Images != out[j].Images {
			return out[i].Images > out[j].Images
		}
		return out[i].Host < out[j].Host
	})
	return out
}
