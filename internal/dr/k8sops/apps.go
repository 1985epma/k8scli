package k8sops

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/1985epma/k8scli/internal/dr"
)

// AppsPlanner stops and starts applications.
//
// Down: apps stop in the reverse of their start order (highest tier first). For each workload the current
// replica count is written to the annotation AnnotationReplicasBefore and to the DR state, and only then the
// workload is scaled to 0 through the scale subresource. After each wave of apps a gate waits for their pods
// to terminate, so a database is never stopped under live clients.
//
// Up: apps start in their start order (lowest tier first; an app after everything it dependsOn). Replicas
// come from the annotation, then from the DR state; when neither knows, the planner refuses and names the
// workload, because it never guesses a count. After each wave a gate waits for the pods to be ready (or for
// the health URL to answer).
//
// HPAs: a HorizontalPodAutoscaler stops acting on a target scaled to 0 and resumes when replicas come back.
// This planner never edits an HPA, so its minReplicas and maxReplicas are exactly what they were.
type AppsPlanner struct {
	opts Options
	mem  *memStore
}

// NewAppsPlanner returns the apps planner.
func NewAppsPlanner(opts Options) *AppsPlanner {
	return &AppsPlanner{opts: opts.withDefaults(), mem: newMemStore()}
}

// Phase implements dr.Planner.
func (p *AppsPlanner) Phase() dr.Phase { return dr.PhaseApps }

// Steps implements dr.Planner.
func (p *AppsPlanner) Steps(ctx context.Context, env *dr.Env, plan *dr.Plan, dir dr.Direction, scope dr.Scope) ([]dr.Step, error) {
	if len(plan.Apps) == 0 {
		return nil, nil
	}
	if env == nil {
		return nil, fmt.Errorf("the apps planner needs an Env")
	}
	ordered, err := startOrder(plan.Apps)
	if err != nil {
		return nil, err
	}
	selected, err := selectApps(ordered, scope.Apps)
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, nil
	}

	c := clockOf(env)
	state := newLedger(env, p.mem, "apps")
	assigned, err := p.assign(ctx, env, plan, state, dir)
	if err != nil {
		return nil, err
	}
	var recorded map[string]int32
	if dir == dr.DirUp {
		recorded, _, err = readState[map[string]int32](ctx, state)
		if err != nil {
			return nil, fmt.Errorf("read the DR state: %w", err)
		}
	}

	groups := waves(selected)
	if dir == dr.DirDown {
		groups = reverseWaves(groups)
	}

	var steps []dr.Step
	for _, wave := range groups {
		var waveSteps []dr.Step
		var waveRefs []WorkloadRef
		var gates []dr.Gate
		for _, app := range wave {
			cands := assigned[app.Name]
			sortByKind(cands, dir)

			var appRefs []WorkloadRef
			for _, cand := range cands {
				base := workloadAction{o: ops{client: env.Client, sleep: c.sleep}, ref: cand.ref, state: state, c: c, seen: -1}
				if cand.live {
					base.seen = cand.replicas
				}
				var action dr.Action
				if dir == dr.DirDown {
					action = &scaleDown{workloadAction: base}
				} else {
					action, err = p.upAction(base, cand, recorded)
					if errors.Is(err, errSkipWorkload) {
						continue
					}
					if err != nil {
						return nil, err
					}
				}
				waveSteps = append(waveSteps, dr.Step{Phase: dr.PhaseApps, Action: action})
				appRefs = append(appRefs, cand.ref)
			}
			waveRefs = append(waveRefs, appRefs...)

			if dir == dr.DirUp {
				gate, err := p.appGate(env, app, appRefs)
				if err != nil {
					return nil, err
				}
				if gate != nil {
					gates = append(gates, gate)
				}
			}
		}
		if len(waveSteps) == 0 {
			continue
		}
		if dir == dr.DirDown {
			waveSteps[len(waveSteps)-1].Gate = NewPodsGoneGate(env, waveRefs, p.opts.TerminationTimeout)
		} else if gate := combineGates(gates); gate != nil {
			waveSteps[len(waveSteps)-1].Gate = gate
		}
		steps = append(steps, waveSteps...)
	}
	return steps, nil
}

// errSkipWorkload tells Steps to leave a workload out of an "up" run.
var errSkipWorkload = errors.New("skip workload")

// sortByKind puts Deployments before StatefulSets on the way down and the reverse on the way up: stateful
// pieces are the first to be ready and the last to go.
func sortByKind(cands []candidate, dir dr.Direction) {
	rank := func(kind string) int {
		if (kind == KindStatefulSet) == (dir == dr.DirUp) {
			return 0
		}
		return 1
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if ri, rj := rank(cands[i].ref.Kind), rank(cands[j].ref.Kind); ri != rj {
			return ri < rj
		}
		return cands[i].ref.Name < cands[j].ref.Name
	})
}

func (p *AppsPlanner) upAction(base workloadAction, cand candidate, state map[string]int32) (dr.Action, error) {
	before, known, source, err := plannedRecord(cand, state)
	if err != nil {
		return nil, err
	}
	if !known && cand.live && cand.replicas == 0 {
		if p.opts.SkipUnknownReplicas {
			base.c.logf("skipping %s: it is at 0 replicas and no replica count was recorded", cand.ref.label())
			return nil, errSkipWorkload
		}
		return nil, fmt.Errorf("%s is at 0 replicas and has no recorded replica count (annotation %s and the DR state are both empty); "+
			"scale it by hand, or leave it out of the run with the SkipUnknownReplicas option", cand.ref.label(), AnnotationReplicasBefore)
	}
	base.before, base.beforeKnown, base.source = before, known, source
	return &scaleUp{workloadAction: base}, nil
}

// plannedRecord returns the recorded replica count of a candidate at planning time.
func plannedRecord(cand candidate, state map[string]int32) (int32, bool, string, error) {
	if cand.hasAnnotation {
		n, err := parseReplicas(cand.annotation)
		if err != nil {
			return 0, false, "", fmt.Errorf("%s: %w", cand.ref.label(), err)
		}
		return n, true, "annotation", nil
	}
	if n, ok := state[cand.ref.key()]; ok {
		return n, true, "DR state", nil
	}
	return 0, false, "", nil
}

func parseReplicas(value string) (int32, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("annotation %s has the invalid value %q", AnnotationReplicasBefore, value)
	}
	return int32(n), nil
}

// appGate is what "this app is back" means: its health URL, or its pods being ready.
func (p *AppsPlanner) appGate(env *dr.Env, app dr.App, refs []WorkloadRef) (dr.Gate, error) {
	timeout := p.opts.GateTimeout
	kind := "pods-ready"
	if h := app.Health; h != nil {
		if h.Timeout != "" {
			d, err := time.ParseDuration(h.Timeout)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("app %q: invalid health timeout %q", app.Name, h.Timeout)
			}
			timeout = d
		}
		if h.Kind != "" {
			kind = h.Kind
		}
		if kind == "http" {
			if h.URL == "" {
				return nil, fmt.Errorf("app %q: health kind http needs a url", app.Name)
			}
			return NewHTTPGate(env, h.URL, h.Status, timeout), nil
		}
		if kind != "pods-ready" {
			return nil, fmt.Errorf("app %q: unknown health kind %q (use pods-ready or http)", app.Name, kind)
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	return NewPodsReadyGate(env, refs, timeout), nil
}

// ---- which workloads belong to which app -------------------------------------------------------------------

// assign decides, for every app of the plan, which workloads it owns. A workload named by an app belongs to
// it; the rest of a namespace goes to the first app of the plan that names no workloads. Database
// StatefulSets of the plan never belong to an app: the databases planner owns them. The assignment is made
// over all apps, so running with a scope never hands an app the workloads of another.
func (p *AppsPlanner) assign(ctx context.Context, env *dr.Env, plan *dr.Plan, state ledger, dir dr.Direction) (map[string][]candidate, error) {
	pools := map[string][]candidate{}
	fromState := map[string]bool{}
	for _, app := range plan.Apps {
		if _, done := pools[app.Namespace]; done {
			continue
		}
		pool, rebuilt, err := p.pool(ctx, env, state, app.Namespace, dir)
		if err != nil {
			return nil, err
		}
		pools[app.Namespace], fromState[app.Namespace] = pool, rebuilt
	}

	databases := map[string]bool{}
	for _, db := range plan.Databases {
		if strings.EqualFold(db.Kind, "statefulset") {
			databases[WorkloadRef{KindStatefulSet, db.Namespace, db.Ref}.key()] = true
		}
	}

	claimed := map[string]string{}
	result := map[string][]candidate{}
	for _, app := range plan.Apps {
		for _, spec := range app.Workloads {
			cand, found, err := match(pools[app.Namespace], app.Namespace, spec)
			if err != nil {
				return nil, fmt.Errorf("app %q: %w", app.Name, err)
			}
			if !found {
				if fromState[app.Namespace] {
					continue // not recorded by "down": there is nothing to restore
				}
				return nil, fmt.Errorf("app %q: workload %q was not found in namespace %s", app.Name, spec, app.Namespace)
			}
			if databases[cand.ref.key()] {
				clockOf(env).logf("app %q lists %s, which is a database of this plan: the databases planner handles it", app.Name, cand.ref.label())
				continue
			}
			if other, taken := claimed[cand.ref.key()]; taken && other != app.Name {
				return nil, fmt.Errorf("workload %s is listed by both app %q and app %q", cand.ref.label(), other, app.Name)
			}
			if _, taken := claimed[cand.ref.key()]; !taken {
				claimed[cand.ref.key()] = app.Name
				result[app.Name] = append(result[app.Name], cand)
			}
		}
	}
	for _, app := range plan.Apps {
		if len(app.Workloads) > 0 {
			continue
		}
		for _, cand := range pools[app.Namespace] {
			if _, taken := claimed[cand.ref.key()]; taken || databases[cand.ref.key()] {
				continue
			}
			claimed[cand.ref.key()] = app.Name
			result[app.Name] = append(result[app.Name], cand)
		}
	}
	return result, nil
}

// pool lists the workloads of a namespace. When the cluster cannot be reached and the run goes up, the
// candidates are rebuilt from the DR state; going down needs the live cluster.
func (p *AppsPlanner) pool(ctx context.Context, env *dr.Env, state ledger, namespace string, dir dr.Direction) ([]candidate, bool, error) {
	var cause error
	if env.Client == nil {
		cause = fmt.Errorf("no Kubernetes client is configured")
	} else {
		list, err := (ops{client: env.Client}).list(ctx, namespace)
		if err == nil {
			return list, false, nil
		}
		if !isUnreachable(err) {
			return nil, false, fmt.Errorf("list workloads in namespace %s: %w", namespace, err)
		}
		cause = err
	}

	if dir == dr.DirUp {
		recorded, _, err := readState[map[string]int32](ctx, state)
		if err != nil {
			return nil, false, fmt.Errorf("read the DR state: %w", err)
		}
		var out []candidate
		for key := range recorded {
			if ref, ok := parseKey(key); ok && ref.Namespace == namespace {
				out = append(out, candidate{ref: ref, live: false})
			}
		}
		if len(out) > 0 {
			sort.Slice(out, func(i, j int) bool { return out[i].ref.key() < out[j].ref.key() })
			return out, true, nil
		}
	}
	return nil, false, fmt.Errorf("%w: cannot list the workloads of namespace %s: %w", dr.ErrUnreachable, namespace, cause)
}

// match resolves a workload name from the plan. "name" finds a Deployment or a StatefulSet (an error when
// both exist); "deployment/name" and "statefulset/name" pick the kind.
func match(pool []candidate, namespace, spec string) (candidate, bool, error) {
	kind, name := "", spec
	if prefix, rest, ok := strings.Cut(spec, "/"); ok {
		switch strings.ToLower(prefix) {
		case "deployment":
			kind, name = KindDeployment, rest
		case "statefulset":
			kind, name = KindStatefulSet, rest
		}
	}
	var found []candidate
	for _, cand := range pool {
		if cand.ref.Name == name && (kind == "" || cand.ref.Kind == kind) {
			found = append(found, cand)
		}
	}
	switch len(found) {
	case 0:
		return candidate{}, false, nil
	case 1:
		return found[0], true, nil
	}
	return candidate{}, false, fmt.Errorf("%q in namespace %s is both a Deployment and a StatefulSet: write deployment/%s or statefulset/%s", spec, namespace, name, name)
}

// ---- actions -----------------------------------------------------------------------------------------------

// workloadAction is what both directions share.
type workloadAction struct {
	o     ops
	ref   WorkloadRef
	state ledger
	c     clock
	// seen is the replica count read while planning, -1 when it could not be read.
	seen int32
	// before is the recorded count known while planning ("up" only).
	before      int32
	beforeKnown bool
	source      string
}

func (a workloadAction) idPrefix() string {
	return fmt.Sprintf("apps/%s/%s/%s", a.ref.Namespace, a.ref.kubectlKind(), a.ref.Name)
}

func (a workloadAction) kubectlScale(replicas string) string {
	return fmt.Sprintf("kubectl scale %s/%s --replicas=%s -n %s", a.ref.kubectlKind(), a.ref.Name, replicas, a.ref.Namespace)
}

// recorded reads the count "down" saved: the annotation first, then the DR state.
func (a workloadAction) recorded(ctx context.Context) (replicas int32, known bool, source string, err error) {
	value, present, err := a.o.annotation(ctx, a.ref)
	if err != nil {
		return 0, false, "", err
	}
	if present {
		n, err := parseReplicas(value)
		if err != nil {
			return 0, false, "", fmt.Errorf("%s: %w", a.ref.label(), err)
		}
		return n, true, "annotation", nil
	}
	m, _, err := readState[map[string]int32](ctx, a.state)
	if err != nil {
		return 0, false, "", err
	}
	if n, ok := m[a.ref.key()]; ok {
		return n, true, "DR state", nil
	}
	return 0, false, "", nil
}

// scaleDown records the replica count and scales a workload to 0.
type scaleDown struct{ workloadAction }

func (a *scaleDown) ID() string         { return a.idPrefix() + ":scale-down" }
func (a *scaleDown) Title() string      { return "Scale " + a.ref.label() + " to 0" }
func (a *scaleDown) Risk() dr.Risk      { return dr.RiskHigh }
func (a *scaleDown) Equivalent() string { return a.kubectlScale("0") }

func (a *scaleDown) Describe() string {
	switch {
	case a.seen > 0:
		return fmt.Sprintf("Records the current replica count (%d) on %s and in the DR state, then scales it to 0.", a.seen, a.ref.label())
	case a.seen == 0:
		return fmt.Sprintf("%s is already at 0 replicas; any count recorded earlier is kept so \"up\" can restore it.", a.ref.label())
	}
	return fmt.Sprintf("Records the current replica count on %s and in the DR state, then scales it to 0.", a.ref.label())
}

// Done is true when the workload is at 0 and its previous count is on record.
func (a *scaleDown) Done(ctx context.Context) (bool, error) {
	current, err := a.o.replicas(ctx, a.ref)
	if err != nil {
		return false, err
	}
	if current > 0 {
		return false, nil
	}
	_, known, _, err := a.recorded(ctx)
	return known, err
}

func (a *scaleDown) Apply(ctx context.Context) error {
	current, err := a.o.replicas(ctx, a.ref)
	if err != nil {
		return err
	}
	before := current
	if current == 0 {
		// Already stopped: keep what an earlier run recorded; with no record, note that it was at 0.
		if recorded, known, _, err := a.recorded(ctx); err != nil {
			return err
		} else if known {
			before = recorded
		}
	}

	// Record first, scale second: a crash in between must never lose the count.
	value := strconv.Itoa(int(before))
	if err := a.o.setAnnotation(ctx, a.ref, &value); err != nil {
		return fmt.Errorf("record the replica count on %s: %w", a.ref.label(), err)
	}
	if err := modifyState(ctx, a.state, func(m *map[string]int32) bool {
		if *m == nil {
			*m = map[string]int32{}
		}
		(*m)[a.ref.key()] = before
		return true
	}); err != nil {
		return fmt.Errorf("record the replica count in the DR state: %w", err)
	}

	if current > 0 {
		if err := a.o.setReplicas(ctx, a.ref, 0); err != nil {
			return fmt.Errorf("scale %s to 0: %w", a.ref.label(), err)
		}
		a.c.logf("scaled %s from %d to 0", a.ref.label(), current)
	}
	return nil
}

// scaleUp restores the recorded replica count.
type scaleUp struct{ workloadAction }

func (a *scaleUp) ID() string    { return a.idPrefix() + ":scale-up" }
func (a *scaleUp) Risk() dr.Risk { return dr.RiskLow }

func (a *scaleUp) Title() string {
	if a.beforeKnown {
		return fmt.Sprintf("Scale %s to %d", a.ref.label(), a.before)
	}
	return "Restore the replicas of " + a.ref.label()
}

func (a *scaleUp) Equivalent() string {
	if a.beforeKnown && a.before > 0 {
		return a.kubectlScale(strconv.Itoa(int(a.before)))
	}
	if a.beforeKnown {
		return ""
	}
	return a.kubectlScale("<recorded replicas>")
}

func (a *scaleUp) Describe() string {
	switch {
	case a.seen > 0 && a.beforeKnown:
		return fmt.Sprintf("%s is already running with %d replicas; only the DR record is cleared.", a.ref.label(), a.seen)
	case a.beforeKnown && a.before == 0:
		return fmt.Sprintf("%s was at 0 replicas before the DR run, so it stays at 0; the DR record is cleared.", a.ref.label())
	case a.beforeKnown:
		return fmt.Sprintf("Restores %d replicas on %s (read from the %s), then clears the DR record.", a.before, a.ref.label(), a.source)
	}
	return fmt.Sprintf("Restores the replica count recorded for %s, then clears the DR record.", a.ref.label())
}

// Done is true only when the workload runs and nothing is recorded for it.
func (a *scaleUp) Done(ctx context.Context) (bool, error) {
	current, err := a.o.replicas(ctx, a.ref)
	if err != nil {
		return false, err
	}
	if current == 0 {
		return false, nil
	}
	_, known, _, err := a.recorded(ctx)
	return !known, err
}

func (a *scaleUp) Apply(ctx context.Context) error {
	current, err := a.o.replicas(ctx, a.ref)
	if err != nil {
		return err
	}
	if current == 0 {
		before, known, source, err := a.recorded(ctx)
		if err != nil {
			return err
		}
		if !known {
			return fmt.Errorf("%s is at 0 replicas and has no recorded replica count (annotation %s and the DR state are both empty); "+
				"not guessing", a.ref.label(), AnnotationReplicasBefore)
		}
		if before > 0 {
			if err := a.o.setReplicas(ctx, a.ref, before); err != nil {
				return fmt.Errorf("scale %s to %d: %w", a.ref.label(), before, err)
			}
			a.c.logf("scaled %s to %d (from the %s)", a.ref.label(), before, source)
		}
	}
	a.clearRecord(ctx)
	return nil
}

// clearRecord removes the annotation and the state entry. Failing here does not fail the step: the workload
// is already back, and a stale record is only noise.
func (a *scaleUp) clearRecord(ctx context.Context) {
	if err := a.o.setAnnotation(ctx, a.ref, nil); err != nil && !apierrors.IsNotFound(err) {
		a.c.logf("could not remove the annotation from %s: %v", a.ref.label(), err)
	}
	err := modifyState(ctx, a.state, func(m *map[string]int32) bool {
		delete(*m, a.ref.key())
		return len(*m) > 0
	})
	if err != nil {
		a.c.logf("could not clear the DR state of %s: %v", a.ref.label(), err)
	}
}
