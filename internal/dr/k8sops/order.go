package k8sops

import (
	"fmt"
	"sort"
	"strings"

	"github.com/1985epma/k8scli/internal/dr"
)

// startOrder returns the apps in the order they start: an app comes after everything it dependsOn, and
// among the apps that are ready at the same moment the lowest tier goes first, then the name. A dependency
// therefore wins over a tier when the two disagree. The stop order is the reverse.
func startOrder(apps []dr.App) ([]dr.App, error) {
	byName := make(map[string]dr.App, len(apps))
	for _, a := range apps {
		if _, dup := byName[a.Name]; dup {
			return nil, fmt.Errorf("app %q is defined twice in the plan", a.Name)
		}
		byName[a.Name] = a
	}
	waiting := map[string]int{}
	dependents := map[string][]string{}
	for _, a := range apps {
		seen := map[string]bool{}
		for _, dep := range a.DependsOn {
			if _, ok := byName[dep]; !ok {
				return nil, fmt.Errorf("app %q depends on %q, which is not in the plan", a.Name, dep)
			}
			if dep == a.Name {
				return nil, fmt.Errorf("app %q depends on itself", a.Name)
			}
			if seen[dep] {
				continue
			}
			seen[dep] = true
			waiting[a.Name]++
			dependents[dep] = append(dependents[dep], a.Name)
		}
	}

	var ready []dr.App
	for _, a := range apps {
		if waiting[a.Name] == 0 {
			ready = append(ready, a)
		}
	}
	less := func(a, b dr.App) bool {
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		return a.Name < b.Name
	}

	var ordered []dr.App
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
		next := ready[0]
		ready = ready[1:]
		ordered = append(ordered, next)
		for _, name := range dependents[next.Name] {
			waiting[name]--
			if waiting[name] == 0 {
				ready = append(ready, byName[name])
			}
		}
	}
	if len(ordered) != len(apps) {
		var stuck []string
		for _, a := range apps {
			if waiting[a.Name] > 0 {
				stuck = append(stuck, a.Name)
			}
		}
		sort.Strings(stuck)
		return nil, fmt.Errorf("apps depend on each other in a cycle: %s", strings.Join(stuck, ", "))
	}
	return ordered, nil
}

// selectApps keeps the apps named in want (all of them when want is empty), preserving order.
func selectApps(ordered []dr.App, want []string) ([]dr.App, error) {
	if len(want) == 0 {
		return ordered, nil
	}
	known := map[string]bool{}
	for _, a := range ordered {
		known[a.Name] = true
	}
	wanted := map[string]bool{}
	for _, name := range want {
		if !known[name] {
			return nil, fmt.Errorf("scope names the app %q, which is not in the plan", name)
		}
		wanted[name] = true
	}
	var out []dr.App
	for _, a := range ordered {
		if wanted[a.Name] {
			out = append(out, a)
		}
	}
	return out, nil
}

// waves groups consecutive apps of a start order that can start together: the same tier and no app
// depending on another one of the group. A gate runs after each wave.
func waves(ordered []dr.App) [][]dr.App {
	var out [][]dr.App
	var current []dr.App
	for _, a := range ordered {
		if len(current) > 0 && (a.Tier != current[0].Tier || dependsOnAny(a, current)) {
			out = append(out, current)
			current = nil
		}
		current = append(current, a)
	}
	if len(current) > 0 {
		out = append(out, current)
	}
	return out
}

func dependsOnAny(a dr.App, group []dr.App) bool {
	for _, dep := range a.DependsOn {
		for _, member := range group {
			if member.Name == dep {
				return true
			}
		}
	}
	return false
}

func reverseWaves(in [][]dr.App) [][]dr.App {
	out := make([][]dr.App, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		wave := make([]dr.App, 0, len(in[i]))
		for j := len(in[i]) - 1; j >= 0; j-- {
			wave = append(wave, in[i][j])
		}
		out = append(out, wave)
	}
	return out
}
