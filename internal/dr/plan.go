package dr

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	// maxPlanBytes bounds what LoadPlan reads: a plan is a small hand-written file.
	maxPlanBytes = 1 << 20
	// maxDuration keeps durations away from overflow and from typos such as 3600000h.
	maxDuration = 10 * 365 * 24 * time.Hour
)

// ProviderKinds are the provider adapters a plan may name.
var ProviderKinds = []string{"aks", "docker-desktop", "eks-nodegroup", "gke-nodepool", "kind", "manual", "minikube"}

// providerParams lists, per provider kind, the only parameters that are accepted. Values end up
// as arguments of external programs (az, aws, gcloud, docker, minikube), so everything outside
// this table is refused.
var providerParams = map[string]struct{ required, optional []string }{
	"kind":           {nil, []string{"name"}},
	"minikube":       {nil, []string{"profile"}},
	"docker-desktop": {nil, nil},
	"manual":         {nil, nil},
	"aks":            {[]string{"resourceGroup", "name"}, []string{"subscription"}},
	"eks-nodegroup":  {[]string{"cluster", "nodegroup", "region"}, []string{"profile"}},
	"gke-nodepool":   {[]string{"cluster", "nodepool", "location", "project"}, nil},
}

// paramValue is deliberately strict: it starts with a letter or digit (so it can never look like
// a flag), and has no spaces, quotes, "=" or control characters.
var paramValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// PlanError lists every problem found in a plan, so people fix them in one pass.
type PlanError struct {
	Problems []string
}

func (e *PlanError) Error() string {
	return fmt.Sprintf("invalid plan: %d problem(s)\n  - %s", len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

// DecodePlan reads a plan strictly: unknown fields, duplicate keys and wrong types are errors. It
// does not validate the values; use ParsePlan for that.
func DecodePlan(data []byte) (*Plan, error) {
	if len(data) > maxPlanBytes {
		return nil, fmt.Errorf("the plan is larger than %d bytes", maxPlanBytes)
	}
	var plan Plan
	if err := yaml.UnmarshalStrict(data, &plan, yaml.DisallowUnknownFields); err != nil {
		return nil, fmt.Errorf("cannot read the plan: %w", err)
	}
	return &plan, nil
}

// ParsePlan decodes a plan strictly and validates it.
func ParsePlan(data []byte) (*Plan, error) {
	plan, err := DecodePlan(data)
	if err != nil {
		return nil, err
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	return plan, nil
}

// LoadPlan reads and validates a plan file.
func LoadPlan(file string) (*Plan, error) {
	// #nosec G304 -- the plan path is chosen by the person running the command.
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("cannot open the plan: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot read the plan: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("the plan %q is not a regular file", file)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPlanBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read the plan: %w", err)
	}
	return ParsePlan(data)
}

// ParseDuration parses a Go duration ("30m", "4h") and requires it to be positive and sane.
func ParseDuration(value string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use values such as 30m or 4h)", value)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be greater than zero", value)
	}
	if d > maxDuration {
		return 0, fmt.Errorf("duration %q is longer than 10 years", value)
	}
	return d, nil
}

// LooksLikeProduction is the heuristic the CLI and the web console use: the context name contains "prod".
func LooksLikeProduction(contextName string) bool {
	return strings.Contains(strings.ToLower(contextName), "prod")
}

// ProductionLike reports whether the plan acts on a production-looking context.
func (p *Plan) ProductionLike() bool { return LooksLikeProduction(p.Context) }

type problems []string

func (v *problems) add(format string, args ...any) { *v = append(*v, fmt.Sprintf(format, args...)) }

// Validate checks everything that can be checked without a cluster and returns a *PlanError
// listing all problems, or nil.
func (p *Plan) Validate() error {
	var v problems

	if p.Version != PlanVersion {
		v.add("version must be %d (got %d)", PlanVersion, p.Version)
	}
	if problem := validName(p.Name); problem != "" {
		v.add("name %s", problem)
	}
	if problem := validContext(p.Context); problem != "" {
		v.add("context %s", problem)
	}
	if p.DRContext != "" {
		if problem := validContext(p.DRContext); problem != "" {
			v.add("drContext %s", problem)
		}
		if p.DRContext == p.Context {
			v.add("drContext must differ from context")
		}
	}

	validateOptionalDuration(&v, "targets.rto", p.Targets.RTO)
	validateOptionalDuration(&v, "targets.rpo", p.Targets.RPO)
	validateOptionalDuration(&v, "safety.requireBackupMaxAge", p.Safety.RequireBackupMaxAge)

	p.validateApps(&v)
	p.validateDatabases(&v)
	validateProvider(&v, p.Provider)

	if len(v) == 0 {
		return nil
	}
	return &PlanError{Problems: v}
}

func (p *Plan) validateApps(v *problems) {
	index := map[string]int{}
	for i := range p.Apps {
		a := &p.Apps[i]
		where := fmt.Sprintf("apps[%d] %q", i, a.Name)

		if problem := validName(a.Name); problem != "" {
			v.add("%s: name %s", where, problem)
		} else if first, dup := index[a.Name]; dup {
			v.add("%s: name already used by apps[%d]", where, first)
		} else {
			index[a.Name] = i
		}
		if problem := validNamespace(a.Namespace); problem != "" {
			v.add("%s: namespace %s", where, problem)
		}
		if a.Tier < 0 {
			v.add("%s: tier must be 0 or greater", where)
		}
		for _, w := range a.Workloads {
			if len(validation.IsDNS1123Subdomain(w)) > 0 {
				v.add("%s: workload %q is not a valid Kubernetes name", where, w)
			}
		}
		validateOptionalDuration(v, where+": rto", a.RTO)
		validateOptionalDuration(v, where+": rpo", a.RPO)
		validateHealth(v, where, a.Health)
	}

	p.validateSharedNamespaces(v)

	dbNames := map[string]bool{}
	for _, d := range p.Databases {
		dbNames[d.Name] = true
	}

	// Dependencies: they must exist, must not loop, and must start no later than their dependents.
	for i := range p.Apps {
		a := &p.Apps[i]
		where := fmt.Sprintf("apps[%d] %q", i, a.Name)
		for _, dep := range a.DependsOn {
			if dep == a.Name {
				v.add("%s: depends on itself", where)
				continue
			}
			j, isApp := index[dep]
			switch {
			case isApp:
				if p.Apps[j].Tier > a.Tier {
					v.add("%s (tier %d): dependsOn %q is in tier %d, which starts later; a dependency must be in the same or a lower tier",
						where, a.Tier, dep, p.Apps[j].Tier)
				}
			case dbNames[dep]:
				// A database always starts before every app, whatever its tier.
			default:
				v.add("%s: dependsOn %q is not an app or a database of this plan", where, dep)
			}
		}
	}
	if cycle := p.appCycle(index); len(cycle) > 0 {
		v.add("apps depend on each other in a loop: %s", strings.Join(cycle, " -> "))
	}
}

// validateSharedNamespaces refuses apps that would handle the same workloads. An app without
// `workloads:` means every workload in its namespace, so two of them in one namespace overlap: each
// one's health gate would wait for the other's workloads, RTO per app would mean nothing, and the
// apps planner would stop and start the same workloads twice. One such app may share a namespace
// with apps that list their workloads: it takes the rest.
func (p *Plan) validateSharedNamespaces(v *problems) {
	wildcards := map[string][]string{}
	var namespaces []string
	for i := range p.Apps {
		a := &p.Apps[i]
		if len(a.Workloads) > 0 || validNamespace(a.Namespace) != "" {
			continue
		}
		if _, seen := wildcards[a.Namespace]; !seen {
			namespaces = append(namespaces, a.Namespace)
		}
		wildcards[a.Namespace] = append(wildcards[a.Namespace], a.Name)
	}
	sort.Strings(namespaces)
	for _, ns := range namespaces {
		if names := wildcards[ns]; len(names) > 1 {
			v.add("apps %s share namespace %s: list `workloads:` for each (an app without workloads means every workload in the namespace)",
				joinNames(names), ns)
		}
	}
}

// joinNames writes "a and b" or "a, b and c".
func joinNames(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// appCycle returns one dependency loop among apps (a -> b -> a), or nil.
func (p *Plan) appCycle(index map[string]int) []string {
	const (
		unseen = iota
		visiting
		finished
	)
	state := make([]int, len(p.Apps))
	var stack []string
	var found []string

	var visit func(i int) bool
	visit = func(i int) bool {
		state[i] = visiting
		stack = append(stack, p.Apps[i].Name)
		for _, dep := range p.Apps[i].DependsOn {
			j, ok := index[dep]
			if !ok || dep == p.Apps[i].Name {
				continue
			}
			switch state[j] {
			case visiting:
				start := 0
				for k, name := range stack {
					if name == dep {
						start = k
					}
				}
				found = append(append([]string{}, stack[start:]...), dep)
				return true
			case unseen:
				if visit(j) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[i] = finished
		return false
	}

	for i := range p.Apps {
		if state[i] == unseen && visit(i) {
			return found
		}
	}
	return nil
}

func (p *Plan) validateDatabases(v *problems) {
	seen := map[string]int{}
	for i := range p.Databases {
		d := &p.Databases[i]
		where := fmt.Sprintf("databases[%d] %q", i, d.Name)

		if problem := validName(d.Name); problem != "" {
			v.add("%s: name %s", where, problem)
		} else if first, dup := seen[d.Name]; dup {
			v.add("%s: name already used by databases[%d]", where, first)
		} else {
			seen[d.Name] = i
		}
		if problem := validNamespace(d.Namespace); problem != "" {
			v.add("%s: namespace %s", where, problem)
		}
		switch d.Kind {
		case "cnpg", "statefulset":
		default:
			v.add("%s: kind must be cnpg or statefulset (got %q)", where, d.Kind)
		}
		if d.Ref == "" {
			v.add("%s: ref is required (the name of the CloudNativePG Cluster or of the StatefulSet)", where)
		} else if len(validation.IsDNS1123Subdomain(d.Ref)) > 0 {
			v.add("%s: ref %q is not a valid Kubernetes name", where, d.Ref)
		}
		if d.Tier < 0 {
			v.add("%s: tier must be 0 or greater", where)
		}
		validateOptionalDuration(v, where+": maxBackupAge", d.MaxBackupAge)
		validateOptionalDuration(v, where+": rto", d.RTO)
		validateOptionalDuration(v, where+": rpo", d.RPO)
	}
}

func validateProvider(v *problems, provider *Provider) {
	if provider == nil {
		return
	}
	spec, known := providerParams[provider.Kind]
	if !known {
		v.add("provider.kind must be one of %s (got %q)", strings.Join(ProviderKinds, ", "), provider.Kind)
		return
	}

	allowed := map[string]bool{}
	for _, k := range spec.required {
		allowed[k] = true
		if provider.Params[k] == "" {
			v.add("provider.params.%s is required for %s", k, provider.Kind)
		}
	}
	for _, k := range spec.optional {
		allowed[k] = true
	}

	keys := make([]string, 0, len(provider.Params))
	for k := range provider.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case !allowed[k]:
			v.add("provider.params.%s is not accepted for %s", k, provider.Kind)
		case provider.Params[k] != "" && !paramValue.MatchString(provider.Params[k]):
			v.add("provider.params.%s has an invalid value: use letters, digits, '.', '_' or '-', starting with a letter or digit", k)
		}
	}
}

func validateHealth(v *problems, where string, h *Health) {
	if h == nil {
		return
	}
	validateOptionalDuration(v, where+": health.timeout", h.Timeout)

	switch h.Kind {
	case "", "pods-ready":
		if h.URL != "" || h.Status != 0 {
			v.add("%s: health.url and health.status are only for kind http", where)
		}
	case "http":
		validateHealthURL(v, where, h)
	default:
		v.add("%s: health.kind must be pods-ready or http (got %q)", where, h.Kind)
	}
}

func validateHealthURL(v *problems, where string, h *Health) {
	if h.URL == "" {
		v.add("%s: health.url is required for kind http", where)
	} else if strings.ContainsFunc(h.URL, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		v.add("%s: health.url must not contain spaces or control characters", where)
	} else if u, err := url.Parse(h.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		v.add("%s: health.url must be a http or https URL with a host", where)
	} else if u.User != nil {
		v.add("%s: health.url must not carry credentials", where)
	}
	if h.Status != 0 && (h.Status < 100 || h.Status > 599) {
		v.add("%s: health.status must be between 100 and 599", where)
	}
}

func validateOptionalDuration(v *problems, where, value string) {
	if value == "" {
		return
	}
	if _, err := ParseDuration(value); err != nil {
		v.add("%s: %s", where, err)
	}
}

func validName(name string) string {
	if name == "" {
		return "is required"
	}
	if problems := validation.IsDNS1123Label(name); len(problems) > 0 {
		return fmt.Sprintf("%q must be a DNS label: lowercase letters, digits and '-', at most 63 characters", name)
	}
	return ""
}

func validNamespace(name string) string { return validName(name) }

// validContext accepts what real kubeconfig context names look like (for example
// arn:aws:eks:us-east-1:123456789012:cluster/prod) and refuses what could be mistaken for a
// flag or break a log line.
func validContext(name string) string {
	switch {
	case name == "":
		return "is required"
	case len(name) > 253:
		return "is longer than 253 characters"
	case strings.HasPrefix(name, "-"):
		return "must not start with '-'"
	case strings.ContainsFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return "must not contain spaces or control characters"
	}
	return ""
}

// ---- Lookups and ordering ----------------------------------------------------------------------------------

// App returns the app with the given name.
func (p *Plan) App(name string) (*App, bool) {
	for i := range p.Apps {
		if p.Apps[i].Name == name {
			return &p.Apps[i], true
		}
	}
	return nil, false
}

// Database returns the database with the given name.
func (p *Plan) Database(name string) (*Database, bool) {
	for i := range p.Databases {
		if p.Databases[i].Name == name {
			return &p.Databases[i], true
		}
	}
	return nil, false
}

// TargetRTO is the recovery time objective of an app or database, falling back to targets.rto.
// The second result is false when none is set (or the value is not a valid duration).
func (p *Plan) TargetRTO(name string) (time.Duration, bool) {
	own := ""
	if a, ok := p.App(name); ok {
		own = a.RTO
	} else if d, ok := p.Database(name); ok {
		own = d.RTO
	}
	return firstDuration(own, p.Targets.RTO)
}

// TargetRPO is the recovery point objective of an app or database, falling back to targets.rpo.
func (p *Plan) TargetRPO(name string) (time.Duration, bool) {
	own := ""
	if a, ok := p.App(name); ok {
		own = a.RPO
	} else if d, ok := p.Database(name); ok {
		own = d.RPO
	}
	return firstDuration(own, p.Targets.RPO)
}

// MaxBackupAge is the backup freshness a database requires before it is stopped, falling back to
// safety.requireBackupMaxAge. The second result is false when the plan does not require a check.
func (p *Plan) MaxBackupAge(database string) (time.Duration, bool) {
	own := ""
	if d, ok := p.Database(database); ok {
		own = d.MaxBackupAge
	}
	return firstDuration(own, p.Safety.RequireBackupMaxAge)
}

func firstDuration(values ...string) (time.Duration, bool) {
	for _, value := range values {
		if value == "" {
			continue
		}
		d, err := ParseDuration(value)
		return d, err == nil
	}
	return 0, false
}

// AppsInTierOrder returns the apps in the order they must be handled. On DirUp that is startup
// order: lower tiers first, and an app after the apps it depends on. On DirDown it is the exact
// reverse. Apps in the same tier keep the order of the file.
func (p *Plan) AppsInTierOrder(dir Direction) []App {
	order := make([]int, len(p.Apps))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return p.Apps[order[a]].Tier < p.Apps[order[b]].Tier })

	index := map[string]int{}
	for i, a := range p.Apps {
		index[a.Name] = i
	}

	placed := make([]bool, len(p.Apps))
	started := make([]bool, len(p.Apps))
	out := make([]App, 0, len(p.Apps))
	var place func(i int)
	place = func(i int) {
		if placed[i] || started[i] { // started and not placed means a loop; Validate rejects those
			return
		}
		started[i] = true
		for _, dep := range p.Apps[i].DependsOn {
			if j, ok := index[dep]; ok {
				place(j)
			}
		}
		placed[i] = true
		out = append(out, p.Apps[i])
	}
	for _, i := range order {
		place(i)
	}

	if dir == DirDown {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// DatabasesInTierOrder is AppsInTierOrder for databases: lower tiers start first and stop last.
func (p *Plan) DatabasesInTierOrder(dir Direction) []Database {
	out := append([]Database(nil), p.Databases...)
	sort.SliceStable(out, func(a, b int) bool { return out[a].Tier < out[b].Tier })
	if dir == DirDown {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// ---- Scope -----------------------------------------------------------------------------------------------

// HasApp reports whether the scope includes the app (an empty list means all of them).
func (s Scope) HasApp(name string) bool { return len(s.Apps) == 0 || contains(s.Apps, name) }

// HasDatabase reports whether the scope includes the database.
func (s Scope) HasDatabase(name string) bool {
	return len(s.Databases) == 0 || contains(s.Databases, name)
}

// HasPhase reports whether the scope includes the phase (an empty list means all of them).
func (s Scope) HasPhase(phase Phase) bool {
	if len(s.Phases) == 0 {
		return true
	}
	for _, candidate := range s.Phases {
		if candidate == phase {
			return true
		}
	}
	return false
}

// ValidateScope checks that a scope only names things that exist in the plan.
func (p *Plan) ValidateScope(s Scope) error {
	var v problems
	for _, name := range s.Apps {
		if _, ok := p.App(name); !ok {
			v.add("scope: unknown app %q", name)
		}
	}
	for _, name := range s.Databases {
		if _, ok := p.Database(name); !ok {
			v.add("scope: unknown database %q", name)
		}
	}
	for _, phase := range s.Phases {
		if !KnownPhase(phase) {
			v.add("scope: unknown phase %q", phase)
		}
	}
	if len(v) == 0 {
		return nil
	}
	return errors.New(strings.Join(v, "; "))
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
