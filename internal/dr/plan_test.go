package dr

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadValid(t *testing.T) *Plan {
	t.Helper()
	plan, err := LoadPlan(filepath.Join("testdata", "valid.yaml"))
	if err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	return plan
}

// minimal returns a valid plan that tests then break one field at a time.
func minimal() *Plan {
	return &Plan{Version: PlanVersion, Name: "demo", Context: "dev"}
}

func problemsOf(t *testing.T, err error) []string {
	t.Helper()
	var planErr *PlanError
	if !errors.As(err, &planErr) {
		t.Fatalf("err = %v; want a *PlanError", err)
	}
	return planErr.Problems
}

func hasProblem(list []string, fragment string) bool {
	for _, p := range list {
		if strings.Contains(p, fragment) {
			return true
		}
	}
	return false
}

func TestValidPlanLoads(t *testing.T) {
	plan := loadValid(t)
	if plan.Name != "shop" || len(plan.Apps) != 2 || len(plan.Databases) != 1 || plan.Provider.Kind != "kind" {
		t.Errorf("plan = %+v", plan)
	}
	if plan.ProductionLike() {
		t.Error("kind-k8scli-lab must not look like production")
	}
}

func TestDecodeIsStrict(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown field":     "version: 1\nname: a\ncontext: c\nsurprise: true\n",
		"nested unknown":    "version: 1\nname: a\ncontext: c\napps:\n  - name: x\n    namespace: y\n    tierr: 1\n",
		"duplicate key":     "version: 1\nname: a\nname: b\ncontext: c\n",
		"wrong type":        "version: \"1\"\nname: a\ncontext: c\n",
		"fractional number": "version: 1\nname: a\ncontext: c\napps:\n  - name: x\n    namespace: y\n    tier: 1.5\n",
		"not yaml":          "version: [1\n",
	} {
		if _, err := ParsePlan([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseRejectsOversizedAndEmptyInput(t *testing.T) {
	if _, err := DecodePlan(make([]byte, maxPlanBytes+1)); err == nil {
		t.Error("an oversized plan must be refused")
	}
	if _, err := ParsePlan(nil); err == nil {
		t.Error("an empty plan must be refused: version, name and context are required")
	}
}

func TestLoadPlanErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadPlan(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a missing file must fail")
	}
	if _, err := LoadPlan(dir); err == nil {
		t.Error("a directory is not a plan")
	}
	big := filepath.Join(dir, "big.yaml")
	if err := os.WriteFile(big, []byte(strings.Repeat("# padding\n", maxPlanBytes/5)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPlan(big); err == nil {
		t.Error("an oversized file must fail")
	}
}

func TestValidateTopLevel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Plan)
		want   string
	}{
		{"version", func(p *Plan) { p.Version = 2 }, "version must be 1"},
		{"missing name", func(p *Plan) { p.Name = "" }, "name is required"},
		{"bad name", func(p *Plan) { p.Name = "My Plan" }, "name"},
		{"missing context", func(p *Plan) { p.Context = "" }, "context is required"},
		{"flag-like context", func(p *Plan) { p.Context = "--kubeconfig=/etc/x" }, "must not start with '-'"},
		{"context with space", func(p *Plan) { p.Context = "my context" }, "spaces or control"},
		{"context with newline", func(p *Plan) { p.Context = "dev\nprod" }, "spaces or control"},
		{"long context", func(p *Plan) { p.Context = strings.Repeat("a", 254) }, "253"},
		{"same dr context", func(p *Plan) { p.DRContext = "dev" }, "must differ"},
		{"bad dr context", func(p *Plan) { p.DRContext = "-x" }, "drContext"},
		{"bad rto", func(p *Plan) { p.Targets.RTO = "soon" }, "targets.rto"},
		{"zero rpo", func(p *Plan) { p.Targets.RPO = "0s" }, "greater than zero"},
		{"negative age", func(p *Plan) { p.Safety.RequireBackupMaxAge = "-1h" }, "requireBackupMaxAge"},
		{"absurd duration", func(p *Plan) { p.Targets.RTO = "999999h" }, "10 years"},
	} {
		plan := minimal()
		tc.mutate(plan)
		list := problemsOf(t, plan.Validate())
		if !hasProblem(list, tc.want) {
			t.Errorf("%s: problems %v; want one with %q", tc.name, list, tc.want)
		}
	}
	if err := minimal().Validate(); err != nil {
		t.Errorf("minimal plan: %v", err)
	}
}

func TestRealWorldContextNamesAreAccepted(t *testing.T) {
	for _, name := range []string{
		"docker-desktop", "kind-k8scli-lab", "arn:aws:eks:us-east-1:123456789012:cluster/prod",
		"gke_my-project_us-central1_main", "user@cluster.example.com",
	} {
		plan := minimal()
		plan.Context = name
		if err := plan.Validate(); err != nil {
			t.Errorf("context %q rejected: %v", name, err)
		}
	}
}

func TestAllProblemsAreReportedTogether(t *testing.T) {
	plan := &Plan{
		Version: 9,
		Apps:    []App{{Name: "Bad_Name", Namespace: "", Tier: -1}},
	}
	list := problemsOf(t, plan.Validate())
	if len(list) < 5 {
		t.Errorf("only %d problems reported: %v", len(list), list)
	}
	if !strings.Contains(plan.Validate().Error(), "problem(s)") {
		t.Error("the message must say how many problems there are")
	}
}

func TestValidateApps(t *testing.T) {
	app := func(mutate func(*App)) *Plan {
		p := minimal()
		a := App{Name: "api", Namespace: "shop"}
		mutate(&a)
		p.Apps = []App{a}
		return p
	}
	for _, tc := range []struct {
		name string
		plan *Plan
		want string
	}{
		{"bad namespace", app(func(a *App) { a.Namespace = "Shop" }), "namespace"},
		{"negative tier", app(func(a *App) { a.Tier = -2 }), "tier must be 0 or greater"},
		{"bad workload", app(func(a *App) { a.Workloads = []string{"ok", "../etc"} }), "workload"},
		{"self dependency", app(func(a *App) { a.DependsOn = []string{"api"} }), "depends on itself"},
		{"missing dependency", app(func(a *App) { a.DependsOn = []string{"ghost"} }), "not an app or a database"},
		{"bad rto", app(func(a *App) { a.RTO = "1 hour" }), "rto"},
		{"bad rpo", app(func(a *App) { a.RPO = "-5m" }), "rpo"},
	} {
		list := problemsOf(t, tc.plan.Validate())
		if !hasProblem(list, tc.want) {
			t.Errorf("%s: problems %v; want one with %q", tc.name, list, tc.want)
		}
	}

	dup := minimal()
	dup.Apps = []App{{Name: "api", Namespace: "a"}, {Name: "api", Namespace: "b"}}
	if list := problemsOf(t, dup.Validate()); !hasProblem(list, "already used") {
		t.Errorf("duplicate app names: %v", list)
	}
}

// Apps that share a namespace must not both mean "every workload in the namespace".
func TestAppsSharingANamespaceMustNotBothBeWildcards(t *testing.T) {
	apps := func(list ...App) *Plan {
		p := minimal()
		p.Apps = list
		return p
	}
	wild := func(name, ns string) App { return App{Name: name, Namespace: ns} }
	named := func(name, ns string, workloads ...string) App {
		return App{Name: name, Namespace: ns, Workloads: workloads}
	}

	for _, tc := range []struct {
		name string
		plan *Plan
		want string // "" means the plan must be valid
	}{
		{"two wildcards in one namespace", apps(wild("api", "shop"), wild("web", "shop")),
			"apps api and web share namespace shop: list `workloads:` for each (an app without workloads means every workload in the namespace)"},
		{"three wildcards are listed in file order", apps(wild("a", "n"), wild("b", "n"), wild("c", "n")),
			"apps a, b and c share namespace n"},
		{"a named app does not rescue two wildcards", apps(wild("a", "n"), named("b", "n", "b"), wild("c", "n")),
			"apps a and c share namespace n"},
		{"every app names its workloads", apps(named("api", "shop", "api"), named("web", "shop", "web")), ""},
		{"one wildcard takes the rest", apps(named("api", "shop", "api"), wild("rest", "shop")), ""},
		{"one wildcard with several named apps", apps(named("a", "n", "a"), named("b", "n", "b"), wild("rest", "n")), ""},
		{"a wildcard alone in its namespace", apps(wild("api", "shop")), ""},
		{"wildcards in different namespaces", apps(wild("api", "one"), wild("web", "two")), ""},
	} {
		list := problemsOf2(tc.plan.Validate())
		if tc.want == "" {
			if hasProblem(list, "share namespace") {
				t.Errorf("%s: unexpected problem %v", tc.name, list)
			}
			continue
		}
		if !hasProblem(list, tc.want) {
			t.Errorf("%s: problems %v; want one with %q", tc.name, list, tc.want)
		}
	}

	// One problem per namespace, however many apps overlap.
	two := apps(wild("a", "x"), wild("b", "x"), wild("c", "y"), wild("d", "y"))
	count := 0
	for _, problem := range problemsOf2(two.Validate()) {
		if strings.Contains(problem, "share namespace") {
			count++
		}
	}
	if count != 2 {
		t.Errorf("want one problem per shared namespace (2), got %d", count)
	}
}

// problemsOf2 is like problemsOf but returns nothing for a nil error, so it can be used on plans
// that are expected to be valid.
func problemsOf2(err error) []string {
	var perr *PlanError
	if errors.As(err, &perr) {
		return perr.Problems
	}
	return nil
}

func TestDependencyCyclesAndTiers(t *testing.T) {
	cyclic := minimal()
	cyclic.Apps = []App{
		{Name: "a", Namespace: "n", DependsOn: []string{"b"}},
		{Name: "b", Namespace: "n", DependsOn: []string{"c"}},
		{Name: "c", Namespace: "n", DependsOn: []string{"a"}},
	}
	list := problemsOf(t, cyclic.Validate())
	if !hasProblem(list, "loop: a -> b -> c -> a") {
		t.Errorf("cycle not described: %v", list)
	}

	wrongTier := minimal()
	wrongTier.Apps = []App{
		{Name: "front", Namespace: "n", Tier: 1, DependsOn: []string{"back"}},
		{Name: "back", Namespace: "n", Tier: 2},
	}
	if list := problemsOf(t, wrongTier.Validate()); !hasProblem(list, "same or a lower tier") {
		t.Errorf("tier inversion not reported: %v", list)
	}

	// A database dependency is always fine, whatever the tiers say.
	ok := minimal()
	ok.Databases = []Database{{Name: "db", Namespace: "n", Kind: "statefulset", Ref: "db", Tier: 5}}
	ok.Apps = []App{{Name: "api", Namespace: "n", Tier: 0, DependsOn: []string{"db"}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("app depending on a database: %v", err)
	}
}

func TestValidateDatabases(t *testing.T) {
	db := func(mutate func(*Database)) *Plan {
		p := minimal()
		d := Database{Name: "pg", Namespace: "data", Kind: "cnpg", Ref: "pg"}
		mutate(&d)
		p.Databases = []Database{d}
		return p
	}
	for _, tc := range []struct {
		name string
		plan *Plan
		want string
	}{
		{"unknown kind", db(func(d *Database) { d.Kind = "mysql" }), "kind must be cnpg or statefulset"},
		{"missing ref", db(func(d *Database) { d.Ref = "" }), "ref is required"},
		{"bad ref", db(func(d *Database) { d.Ref = "a b" }), "ref"},
		{"negative tier", db(func(d *Database) { d.Tier = -1 }), "tier"},
		{"bad max age", db(func(d *Database) { d.MaxBackupAge = "yesterday" }), "maxBackupAge"},
		{"bad namespace", db(func(d *Database) { d.Namespace = "" }), "namespace"},
	} {
		list := problemsOf(t, tc.plan.Validate())
		if !hasProblem(list, tc.want) {
			t.Errorf("%s: problems %v; want one with %q", tc.name, list, tc.want)
		}
	}

	dup := minimal()
	dup.Databases = []Database{
		{Name: "pg", Namespace: "a", Kind: "cnpg", Ref: "x"},
		{Name: "pg", Namespace: "b", Kind: "cnpg", Ref: "y"},
	}
	if list := problemsOf(t, dup.Validate()); !hasProblem(list, "already used") {
		t.Errorf("duplicate databases: %v", list)
	}
}

func TestValidateHealth(t *testing.T) {
	health := func(h Health) *Plan {
		p := minimal()
		p.Apps = []App{{Name: "api", Namespace: "n", Health: &h}}
		return p
	}
	for _, tc := range []struct {
		name string
		h    Health
		want string // "" means valid
	}{
		{"default kind", Health{}, ""},
		{"pods ready with timeout", Health{Kind: "pods-ready", Timeout: "90s"}, ""},
		{"http ok", Health{Kind: "http", URL: "https://example.com/health", Status: 204}, ""},
		{"http default status", Health{Kind: "http", URL: "http://10.0.0.5:8080/"}, ""},
		{"unknown kind", Health{Kind: "tcp"}, "health.kind"},
		{"http without url", Health{Kind: "http"}, "health.url is required"},
		{"file url", Health{Kind: "http", URL: "file:///etc/passwd"}, "http or https URL"},
		{"ftp url", Health{Kind: "http", URL: "ftp://example.com"}, "http or https URL"},
		{"javascript url", Health{Kind: "http", URL: "javascript:alert(1)"}, "http or https URL"},
		{"no host", Health{Kind: "http", URL: "http:///path"}, "http or https URL"},
		{"credentials", Health{Kind: "http", URL: "https://admin:secret@example.com/"}, "credentials"},
		{"space in url", Health{Kind: "http", URL: "https://example.com/a b"}, "spaces"},
		{"newline in url", Health{Kind: "http", URL: "https://example.com/\nHost: evil"}, "control characters"},
		{"status too low", Health{Kind: "http", URL: "https://e.com", Status: 99}, "between 100 and 599"},
		{"status too high", Health{Kind: "http", URL: "https://e.com", Status: 600}, "between 100 and 599"},
		{"url on pods-ready", Health{Kind: "pods-ready", URL: "https://e.com"}, "only for kind http"},
		{"bad timeout", Health{Timeout: "forever"}, "health.timeout"},
	} {
		err := health(tc.h).Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !hasProblem(problemsOf(t, err), tc.want) {
			t.Errorf("%s: err = %v; want a problem with %q", tc.name, err, tc.want)
		}
	}
}

func TestValidateProvider(t *testing.T) {
	provider := func(kind string, params map[string]string) *Plan {
		p := minimal()
		p.Provider = &Provider{Kind: kind, Params: params}
		return p
	}
	valid := []*Plan{
		provider("kind", nil),
		provider("kind", map[string]string{"name": "k8scli-lab"}),
		provider("minikube", map[string]string{"profile": "dev"}),
		provider("docker-desktop", nil),
		provider("manual", nil),
		provider("aks", map[string]string{"resourceGroup": "rg-prod", "name": "aks-main"}),
		provider("aks", map[string]string{"resourceGroup": "rg", "name": "aks", "subscription": "00000000-0000-0000-0000-000000000000"}),
		provider("eks-nodegroup", map[string]string{"cluster": "main", "nodegroup": "workers", "region": "us-east-1", "profile": "ops"}),
		provider("gke-nodepool", map[string]string{"cluster": "main", "nodepool": "default-pool", "location": "us-central1-a", "project": "my-project-123"}),
	}
	for i, p := range valid {
		if err := p.Validate(); err != nil {
			t.Errorf("valid provider %d (%s): %v", i, p.Provider.Kind, err)
		}
	}

	for _, tc := range []struct {
		name string
		plan *Plan
		want string
	}{
		{"unknown kind", provider("vmware", nil), "provider.kind must be one of"},
		{"missing required", provider("aks", map[string]string{"name": "x"}), "resourceGroup is required"},
		{"unknown param", provider("kind", map[string]string{"namee": "x"}), "not accepted"},
		{"param on empty kind", provider("docker-desktop", map[string]string{"name": "x"}), "not accepted"},
		{"param on manual", provider("manual", map[string]string{"cmd": "rm -rf /"}), "not accepted"},
		{"flag injection", provider("kind", map[string]string{"name": "--all"}), "invalid value"},
		{"option injection", provider("minikube", map[string]string{"profile": "-p"}), "invalid value"},
		{"space injection", provider("aks", map[string]string{"resourceGroup": "rg --yes", "name": "x"}), "invalid value"},
		{"shell metachar", provider("kind", map[string]string{"name": "a;rm -rf /"}), "invalid value"},
		{"command substitution", provider("kind", map[string]string{"name": "$(id)"}), "invalid value"},
		{"equals injection", provider("eks-nodegroup", map[string]string{"cluster": "a", "nodegroup": "b", "region": "us-east-1", "profile": "x=y"}), "invalid value"},
		{"newline", provider("kind", map[string]string{"name": "a\nb"}), "invalid value"},
		{"too long", provider("kind", map[string]string{"name": strings.Repeat("a", 200)}), "invalid value"},
		{"empty required value", provider("gke-nodepool", map[string]string{"cluster": "a", "nodepool": "", "location": "b", "project": "c"}), "nodepool is required"},
	} {
		list := problemsOf(t, tc.plan.Validate())
		if !hasProblem(list, tc.want) {
			t.Errorf("%s: problems %v; want one with %q", tc.name, list, tc.want)
		}
	}
}

func TestProviderKindsMatchTheParameterTable(t *testing.T) {
	if len(ProviderKinds) != len(providerParams) {
		t.Fatalf("ProviderKinds has %d entries, the table %d", len(ProviderKinds), len(providerParams))
	}
	for _, kind := range ProviderKinds {
		if _, ok := providerParams[kind]; !ok {
			t.Errorf("kind %q is listed but has no parameter rules", kind)
		}
	}
}

func TestTargetsFallBack(t *testing.T) {
	plan := loadValid(t)

	if d, ok := plan.TargetRTO("web"); !ok || d != 10*time.Minute {
		t.Errorf("web RTO = %v, %v; want its own 10m", d, ok)
	}
	if d, ok := plan.TargetRTO("api"); !ok || d != 30*time.Minute {
		t.Errorf("api RTO = %v, %v; want the 30m default", d, ok)
	}
	if d, ok := plan.TargetRPO("shop-db"); !ok || d != 5*time.Minute {
		t.Errorf("shop-db RPO = %v, %v; want its own 5m", d, ok)
	}
	if d, ok := plan.TargetRPO("api"); !ok || d != 15*time.Minute {
		t.Errorf("api RPO = %v, %v; want the 15m default", d, ok)
	}
	if d, ok := plan.TargetRTO("unknown"); !ok || d != 30*time.Minute {
		t.Errorf("an unknown name falls back to the default, got %v, %v", d, ok)
	}
	if d, ok := plan.MaxBackupAge("shop-db"); !ok || d != 24*time.Hour {
		t.Errorf("shop-db max backup age = %v, %v", d, ok)
	}

	empty := minimal()
	if _, ok := empty.TargetRTO("x"); ok {
		t.Error("no objective set must report false")
	}
	if _, ok := empty.MaxBackupAge("x"); ok {
		t.Error("no backup age requirement must report false")
	}
	empty.Safety.RequireBackupMaxAge = "12h"
	empty.Databases = []Database{{Name: "x", Namespace: "n", Kind: "cnpg", Ref: "x"}}
	if d, ok := empty.MaxBackupAge("x"); !ok || d != 12*time.Hour {
		t.Errorf("safety default not applied: %v, %v", d, ok)
	}
}

func TestProductionHeuristicMatchesTheCLI(t *testing.T) {
	for name, want := range map[string]bool{
		"prod": true, "eks-PROD-1": true, "production": true, "reproduce-dev": true, // same false positive as the CLI
		"dev": false, "kind-lab": false, "": false,
	} {
		if got := LooksLikeProduction(name); got != want {
			t.Errorf("LooksLikeProduction(%q) = %v; want %v", name, got, want)
		}
	}
}

func names(apps []App) string {
	out := make([]string, len(apps))
	for i, a := range apps {
		out[i] = a.Name
	}
	return strings.Join(out, ",")
}

func TestAppsInTierOrder(t *testing.T) {
	plan := minimal()
	plan.Apps = []App{
		{Name: "web", Namespace: "n", Tier: 2},
		{Name: "api", Namespace: "n", Tier: 1},
		{Name: "worker", Namespace: "n", Tier: 1},
		{Name: "cache", Namespace: "n", Tier: 0},
	}
	if got := names(plan.AppsInTierOrder(DirUp)); got != "cache,api,worker,web" {
		t.Errorf("up = %s", got)
	}
	if got := names(plan.AppsInTierOrder(DirDown)); got != "web,worker,api,cache" {
		t.Errorf("down = %s", got)
	}

	// Inside a tier an app follows its dependencies, and the file order breaks ties.
	plan.Apps = []App{
		{Name: "b", Namespace: "n", Tier: 1, DependsOn: []string{"c"}},
		{Name: "a", Namespace: "n", Tier: 1},
		{Name: "c", Namespace: "n", Tier: 1},
	}
	if got := names(plan.AppsInTierOrder(DirUp)); got != "c,b,a" {
		t.Errorf("dependencies in a tier: up = %s", got)
	}
	if got := names(plan.AppsInTierOrder(DirDown)); got != "a,b,c" {
		t.Errorf("dependencies in a tier: down = %s", got)
	}
}

func TestOrderingDoesNotMutateThePlanAndSurvivesLoops(t *testing.T) {
	plan := minimal()
	plan.Apps = []App{
		{Name: "a", Namespace: "n", Tier: 1, DependsOn: []string{"b"}},
		{Name: "b", Namespace: "n", Tier: 0, DependsOn: []string{"a"}}, // invalid, must not hang
	}
	if got := plan.AppsInTierOrder(DirUp); len(got) != 2 {
		t.Errorf("a loop must still return every app once, got %v", names(got))
	}
	if plan.Apps[0].Name != "a" {
		t.Error("the plan was reordered in place")
	}
}

func TestDatabasesInTierOrder(t *testing.T) {
	plan := minimal()
	plan.Databases = []Database{
		{Name: "late", Namespace: "n", Kind: "cnpg", Ref: "l", Tier: 1},
		{Name: "first", Namespace: "n", Kind: "cnpg", Ref: "f", Tier: 0},
	}
	up := plan.DatabasesInTierOrder(DirUp)
	down := plan.DatabasesInTierOrder(DirDown)
	if up[0].Name != "first" || down[0].Name != "late" || plan.Databases[0].Name != "late" {
		t.Errorf("up %v down %v", up, down)
	}
}

func TestScopeHelpersAndValidation(t *testing.T) {
	plan := loadValid(t)

	if !(Scope{}).HasApp("any") || !(Scope{}).HasDatabase("any") || !(Scope{}).HasPhase(PhasePower) {
		t.Error("an empty scope includes everything")
	}
	scope := Scope{Apps: []string{"api"}, Databases: []string{"shop-db"}, Phases: []Phase{PhaseApps}}
	if !scope.HasApp("api") || scope.HasApp("web") || !scope.HasDatabase("shop-db") || scope.HasPhase(PhasePower) || !scope.HasPhase(PhaseApps) {
		t.Errorf("scope helpers wrong: %+v", scope)
	}
	if err := plan.ValidateScope(scope); err != nil {
		t.Errorf("valid scope: %v", err)
	}
	err := plan.ValidateScope(Scope{Apps: []string{"ghost"}, Databases: []string{"nope"}, Phases: []Phase{"teleport"}})
	if err == nil {
		t.Fatal("unknown names must be rejected")
	}
	for _, want := range []string{"unknown app", "unknown database", "unknown phase"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q misses %q", err, want)
		}
	}
}

func TestPhaseOrdersAreMirrorImages(t *testing.T) {
	if len(DownOrder) != len(UpOrder) {
		t.Fatal("orders differ in length")
	}
	for i := range DownOrder {
		if DownOrder[i] != UpOrder[len(UpOrder)-1-i] {
			t.Errorf("phase %d: down %s, up mirror %s", i, DownOrder[i], UpOrder[len(UpOrder)-1-i])
		}
	}
	if DownOrder[len(DownOrder)-1] != PhasePower || UpOrder[0] != PhasePower {
		t.Error("power must be the last thing to stop and the first to start")
	}
	for _, phase := range DownOrder {
		if !KnownPhase(phase) {
			t.Errorf("%s is not a known phase", phase)
		}
	}
	if KnownPhase("recover") {
		t.Error("recover was cut from this version and must not be a known phase")
	}
}

func TestRiskStrings(t *testing.T) {
	if RiskLow.String() != "low" || RiskHigh.String() != "high" || RiskDestructive.String() != "destructive" {
		t.Error("risk names changed")
	}
}

// The provider package uses the same pattern for the values it passes to az, aws, gcloud, docker and
// minikube: ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$. It is strict on purpose, so a plan that would be
// refused later is refused when it is validated.
func TestProviderParamValuesUseTheStrictSharedPattern(t *testing.T) {
	aks := func(group string) *Plan {
		p := minimal()
		p.Provider = &Provider{Kind: "aks", Params: map[string]string{"resourceGroup": group, "name": "main"}}
		return p
	}
	gke := func(project string) *Plan {
		p := minimal()
		p.Provider = &Provider{Kind: "gke-nodepool", Params: map[string]string{"cluster": "c", "nodepool": "p", "location": "us-central1", "project": project}}
		return p
	}

	for name, plan := range map[string]*Plan{
		"parentheses in an Azure resource group": aks("rg(prod)"),
		"colon in a GKE project id":              gke("example.com:my-project"),
		"space":                                  aks("my group"),
		"equals sign":                            aks("a=b"),
		"leading dot":                            aks(".hidden"),
		"leading underscore":                     aks("_x"),
		"non-ASCII letter":                       aks("grupo-é"),
		"slash":                                  gke("a/b"),
		"129 characters":                         aks(strings.Repeat("a", 129)),
	} {
		if err := plan.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, plan := range map[string]*Plan{
		"128 characters":  aks(strings.Repeat("a", 128)),
		"dots and dashes": aks("rg-prod.eu_1"),
		"digits first":    gke("1234-project"),
	} {
		if err := plan.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestProviderParamsOutsideTheAllowlistAreRejectedAsTypos(t *testing.T) {
	for kind, params := range map[string]map[string]string{
		"kind":           {"cluster": "x"},
		"minikube":       {"name": "x"},
		"aks":            {"resourcegroup": "rg", "name": "n"}, // wrong case
		"eks-nodegroup":  {"cluster": "c", "nodegroup": "n", "region": "r", "node-group": "x"},
		"gke-nodepool":   {"cluster": "c", "nodepool": "p", "location": "l", "project": "x", "zone": "z"},
		"docker-desktop": {"anything": "x"},
		"manual":         {"anything": "x"},
	} {
		p := minimal()
		p.Provider = &Provider{Kind: kind, Params: params}
		if list := problemsOf(t, p.Validate()); !hasProblem(list, "not accepted") && !hasProblem(list, "is required") {
			t.Errorf("%s %v: problems %v", kind, params, list)
		}
	}
}
