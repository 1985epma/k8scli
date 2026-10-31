package k8sops

import (
	"strings"
	"testing"

	"github.com/1985epma/k8scli/internal/dr"
)

func names(apps []dr.App) string {
	out := make([]string, 0, len(apps))
	for _, a := range apps {
		out = append(out, a.Name)
	}
	return strings.Join(out, ",")
}

func TestStartOrderUsesTierThenName(t *testing.T) {
	got, err := startOrder([]dr.App{{Name: "web", Tier: 2}, {Name: "b-api", Tier: 1}, {Name: "a-api", Tier: 1}, {Name: "db", Tier: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if names(got) != "db,a-api,b-api,web" {
		t.Errorf("order = %s", names(got))
	}
}

func TestDependsOnWinsOverTier(t *testing.T) {
	// "report" is tier 0 but needs "cache" (tier 2): the dependency has to start first.
	got, err := startOrder([]dr.App{{Name: "report", Tier: 0, DependsOn: []string{"cache"}}, {Name: "cache", Tier: 2}, {Name: "api", Tier: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if names(got) != "api,cache,report" {
		t.Errorf("order = %s", names(got))
	}
}

func TestStartOrderRejectsBadGraphs(t *testing.T) {
	for name, apps := range map[string][]dr.App{
		"cycle":      {{Name: "a", DependsOn: []string{"b"}}, {Name: "b", DependsOn: []string{"a"}}},
		"unknown":    {{Name: "a", DependsOn: []string{"ghost"}}},
		"self":       {{Name: "a", DependsOn: []string{"a"}}},
		"duplicated": {{Name: "a"}, {Name: "a"}},
	} {
		if _, err := startOrder(apps); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := startOrder([]dr.App{{Name: "a", DependsOn: []string{"b"}}, {Name: "b", DependsOn: []string{"a"}}}); err == nil || !strings.Contains(err.Error(), "a, b") {
		t.Errorf("the cycle error should name the apps: %v", err)
	}
}

func TestWavesGroupSameTierWithoutDependencies(t *testing.T) {
	ordered, _ := startOrder([]dr.App{{Name: "db", Tier: 0}, {Name: "a", Tier: 1}, {Name: "b", Tier: 1}, {Name: "c", Tier: 1, DependsOn: []string{"a"}}, {Name: "web", Tier: 2}})
	var groups []string
	for _, w := range waves(ordered) {
		groups = append(groups, names(w))
	}
	if strings.Join(groups, " | ") != "db | a,b | c | web" {
		t.Errorf("waves = %s", strings.Join(groups, " | "))
	}

	var down []string
	for _, w := range reverseWaves(waves(ordered)) {
		down = append(down, names(w))
	}
	if strings.Join(down, " | ") != "web | c | b,a | db" {
		t.Errorf("down waves = %s", strings.Join(down, " | "))
	}
}

func TestSelectApps(t *testing.T) {
	ordered, _ := startOrder([]dr.App{{Name: "db"}, {Name: "api", Tier: 1}, {Name: "web", Tier: 2}})
	got, err := selectApps(ordered, []string{"web", "db"})
	if err != nil || names(got) != "db,web" {
		t.Errorf("selectApps = %s, %v", names(got), err)
	}
	if _, err := selectApps(ordered, []string{"ghost"}); err == nil {
		t.Error("an unknown app in the scope must fail")
	}
}
