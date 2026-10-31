// Package extchecks looks at what a restore depends on that lives outside the workloads themselves:
// the container registries the images come from, certificates, the Secrets and ConfigMaps the workloads
// reference, the StorageClasses, IngressClasses and PriorityClasses they need, and whether the backups
// follow the 3-2-1 rule.
//
// Everything here is read-only. Secrets and ConfigMaps are only ever checked BY NAME: the values are
// never read into a result (the names are listed with a metadata-only request when the cluster supports
// it, and any object the client still returns has its data dropped on the spot).
//
// Findings use the same shape as internal/dr/checks (checks.Finding, checks.Severity) with the rule ids
// DR1xx, so a report can show them next to the readiness findings.
package extchecks

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/checks"
)

// DefaultExpiryDays is how far ahead an expiring certificate is reported.
const DefaultExpiryDays = 30

const (
	maxFindings   = 5000
	maxWorkloads  = 5000
	maxNames      = 50000
	pageSize      = 500
	disclaimerTxt = "These checks compare names and metadata. They show what a restore is likely to miss, not that it will work: only a drill proves that."
)

// Rule describes one rule.
type Rule struct {
	ID       string          `json:"id"`
	Severity checks.Severity `json:"severity"`
	Title    string          `json:"title"`
	Why      string          `json:"why"`
}

// Rules lists every rule with its default severity (a few findings are raised or lowered by context).
func Rules() []Rule {
	high, medium, low := checks.SeverityHigh, checks.SeverityMedium, checks.SeverityLow
	return []Rule{
		{"DR101", medium, "Image registry bound to one region", "Images that live in a regional registry may not be pullable if that region is the one that failed."},
		{"DR102", high, "imagePullSecret missing in the target cluster", "The pods cannot pull their images there."},
		{"DR103", low, "Anonymous Docker Hub pulls", "A restore pulls many images at once and anonymous pulls are rate limited."},
		{"DR104", low, "Private-looking image with no pull credentials", "Unless the nodes authenticate on their own (cloud IAM), the pull will be refused."},
		{"DR105", low, "Every image comes from one registry", "A registry outage then blocks every restore."},
		{"DR110", high, "Certificate is not ready", "A certificate that is not issued now will not be there after a restore."},
		{"DR111", medium, "Certificate expires soon", "An expired certificate takes the service down, restored or not."},
		{"DR112", high, "Ingress TLS secret missing in the target cluster", "The ingress cannot terminate TLS without it."},
		{"DR113", medium, "cert-manager is not installed in the DR cluster", "Certificates cannot be reissued there."},
		{"DR120", high, "Secret referenced by a workload is missing in the target cluster", "The pods will not start."},
		{"DR121", high, "ConfigMap referenced by a workload is missing in the target cluster", "The pods will not start."},
		{"DR125", high, "Namespace is missing in the DR cluster", "Nothing of this namespace can be restored there until it exists."},
		{"DR130", high, "StorageClass is missing in the target cluster", "Volumes cannot be provisioned or restored."},
		{"DR131", medium, "IngressClass is missing in the target cluster", "The ingress is not served."},
		{"DR132", medium, "PriorityClass is missing in the target cluster", "The pods are rejected."},
		{"DR140", high, "No backup location was found", "There is no copy of the data outside the live volumes."},
		{"DR141", medium, "Fewer than three copies of the data", "The 3-2-1 rule asks for three copies."},
		{"DR142", medium, "Only one kind of storage", "The 3-2-1 rule asks for two different media."},
		{"DR143", medium, "No copy is known to be off-site", "The 3-2-1 rule asks for one copy in another place."},
		{"DR144", low, "Immutability of the backups cannot be verified", "Object lock and versioning are bucket settings the Kubernetes API does not show."},
		{"DR145", medium, "Backup storage location is unavailable", "Backups to it are failing."},
		{"DR146", medium, "No backup schedule covers the namespaces", "Backups only happen when somebody remembers."},
		{"DR147", high, "A backup location is inside the cluster it protects", "Losing the cluster loses the backups with it."},
	}
}

// Options select what to check.
type Options struct {
	// Plan supplies the namespaces (apps and databases) when Namespaces is empty.
	Plan *dr.Plan
	// Namespaces overrides the plan.
	Namespaces []string
	// Primary is the cluster that runs the workloads.
	Primary *dr.Env
	// Standby is the DR cluster; when nil the references are checked against the primary itself.
	Standby *dr.Env
	// ExpiryDays is how far ahead certificates are reported as expiring (default 30).
	ExpiryDays int
	// MinSeverity hides findings below it (the summary still counts all of them).
	MinSeverity checks.Severity
}

// Summary counts the findings over all severities.
type Summary struct {
	High   int `json:"high"`
	Medium int `json:"medium"`
	Low    int `json:"low"`
	Total  int `json:"total"`
}

// RegistryUse is how many images come from one registry.
type RegistryUse struct {
	Host    string `json:"host"`
	Images  int    `json:"images"`
	Region  string `json:"region,omitempty"`
	Private bool   `json:"private"`
}

// Report is the result of Run.
type Report struct {
	Context    string           `json:"context"`
	Standby    string           `json:"standby,omitempty"`
	Generated  time.Time        `json:"generated"`
	Disclaimer string           `json:"disclaimer"`
	Namespaces []string         `json:"namespaces"`
	Summary    Summary          `json:"summary"`
	Findings   []checks.Finding `json:"findings"`
	Registries []RegistryUse    `json:"registries,omitempty"`
	Copies     *CopiesReport    `json:"copies,omitempty"`
	Notices    []string         `json:"notices,omitempty"`
	Truncated  bool             `json:"truncated,omitempty"`
	Checked    map[string]int   `json:"checked,omitempty"`
}

type runner struct {
	opts      Options
	findings  []checks.Finding
	notices   []string
	truncated bool
	checked   map[string]int
	now       time.Time
	// checkedCertManager makes the DR cluster's cert-manager check run once.
	checkedCertManager bool
}

func (r *runner) add(f checks.Finding) {
	if len(r.findings) >= maxFindings {
		r.truncated = true
		return
	}
	r.findings = append(r.findings, f)
}

func (r *runner) notice(format string, args ...any) {
	r.notices = append(r.notices, fmt.Sprintf(format, args...))
}

func (r *runner) count(kind string, n int) { r.checked[kind] += n }

// Run performs every check. Failures to read one thing become notices; the report is still returned
// as long as the workloads could be read.
func Run(ctx context.Context, opts Options) (*Report, error) {
	if opts.Primary == nil || opts.Primary.Client == nil {
		return nil, fmt.Errorf("%w: a client for the primary cluster is required", dr.ErrUnreachable)
	}
	if opts.ExpiryDays <= 0 {
		opts.ExpiryDays = DefaultExpiryDays
	}
	namespaces := namespacesOf(opts)
	if len(namespaces) == 0 {
		return nil, errors.New("no namespace to check: give namespaces, or a plan with apps or databases")
	}

	r := &runner{opts: opts, checked: map[string]int{}, now: now(opts.Primary)}
	target := opts.Primary
	targetName := "this cluster"
	if opts.Standby != nil {
		if opts.Standby.Client == nil {
			return nil, fmt.Errorf("%w: a client for the DR cluster is required", dr.ErrUnreachable)
		}
		target = opts.Standby
		targetName = "the DR cluster"
	}

	// Cluster-scoped things of the target, read once.
	classes := r.readClasses(ctx, target)

	workloadsSeen := map[string]bool{}
	var allImages []imageUse
	for _, namespace := range namespaces {
		set, err := r.readNamespace(ctx, namespace)
		if err != nil {
			return nil, err
		}
		exists := true
		var targetNames *nameSets
		if opts.Standby != nil {
			exists, err = namespaceExists(ctx, target, namespace)
			if err != nil {
				r.notice("could not check whether namespace %s exists in the DR cluster: %v", namespace, err)
				exists = true
			}
			if !exists {
				r.add(checks.Finding{Rule: "DR125", Severity: checks.SeverityHigh, Kind: "Namespace", Name: namespace,
					Message: fmt.Sprintf("namespace %s does not exist in %s", namespace, targetName),
					Fix:     "kubectl create namespace " + namespace + " --context <dr-context>"})
			}
		}
		if exists {
			targetNames = r.readNames(ctx, target, namespace)
		}

		for i := range set.workloads {
			w := &set.workloads[i]
			workloadsSeen[w.key()] = true
			allImages = append(allImages, w.imageUses()...)
			r.checkRegistries(w, set, targetNames, targetName)
			if targetNames != nil {
				r.checkReferences(w, targetNames, targetName)
			}
			r.checkClasses(w, classes, targetName)
		}
		r.checkVolumes(set, classes, targetName)
		r.checkIngresses(set, classes, targetNames, targetName)
		r.checkCertificates(ctx, set, opts, target)
	}
	r.count("workloads", len(workloadsSeen))
	r.summarizeRegistries(allImages)

	copies := r.checkCopies(ctx, namespaces)

	report := &Report{
		Context: opts.Primary.Context, Generated: r.now, Disclaimer: disclaimerTxt, Namespaces: namespaces,
		Copies: copies, Notices: r.notices, Truncated: r.truncated, Checked: r.checked,
	}
	if opts.Standby != nil {
		report.Standby = opts.Standby.Context
	}
	report.Registries = registryList(allImages)
	report.Findings, report.Summary = finalize(r.findings, opts.MinSeverity)
	return report, nil
}

func namespacesOf(opts Options) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ns string) {
		if ns != "" && !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	for _, ns := range opts.Namespaces {
		add(ns)
	}
	if len(out) == 0 && opts.Plan != nil {
		for _, app := range opts.Plan.Apps {
			add(app.Namespace)
		}
		for _, db := range opts.Plan.Databases {
			add(db.Namespace)
		}
	}
	sort.Strings(out)
	return out
}

func now(env *dr.Env) time.Time {
	if env != nil && env.Now != nil {
		return env.Now()
	}
	return time.Now()
}

func rank(s checks.Severity) int {
	switch s {
	case checks.SeverityHigh:
		return 3
	case checks.SeverityMedium:
		return 2
	default:
		return 1
	}
}

// finalize sorts deterministically, counts everything, then drops what is below the filter.
func finalize(all []checks.Finding, min checks.Severity) ([]checks.Finding, Summary) {
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if rank(a.Severity) != rank(b.Severity) {
			return rank(a.Severity) > rank(b.Severity)
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Message < b.Message
	})
	var s Summary
	out := make([]checks.Finding, 0, len(all))
	for _, f := range all {
		switch f.Severity {
		case checks.SeverityHigh:
			s.High++
		case checks.SeverityMedium:
			s.Medium++
		default:
			s.Low++
		}
		s.Total++
		if min == "" || rank(f.Severity) >= rank(min) {
			out = append(out, f)
		}
	}
	return out, s
}
