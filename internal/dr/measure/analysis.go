package measure

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

// Kind says how a figure was obtained.
type Kind string

// Kinds of figures.
const (
	// KindMeasured figures were observed during the drill.
	KindMeasured Kind = "measured"
	// KindEstimated figures come from metadata (a backup finished at some time); nothing was restored to check.
	KindEstimated Kind = "estimated"
)

// Verdict is the comparison of a figure with its target.
type Verdict string

// Verdicts.
const (
	VerdictPass    Verdict = "pass"
	VerdictFail    Verdict = "fail"
	VerdictUnknown Verdict = "unknown" // no target, or no figure to compare
)

// Stage keys, in the order they happen.
const (
	StageDeclared        = "declared"
	StageRecoveryStarted = "recovery-started"
	StageDataRestored    = "data-restored"
	StageDatabasesUp     = "databases-up"
	StageAppsUp          = "apps-up"
	StageHealthGreen     = "health-green"
)

// Stage is one moment of the recovery, relative to T0.
type Stage struct {
	Key     string    `json:"key"`
	Title   string    `json:"title"`
	At      time.Time `json:"at"`
	Seconds float64   `json:"seconds"`
	Human   string    `json:"human"`
}

// Objective is a figure with its target.
type Objective struct {
	Seconds       float64 `json:"seconds"`
	Human         string  `json:"human"`
	Kind          Kind    `json:"kind"`
	Source        string  `json:"source,omitempty"`
	Target        string  `json:"target,omitempty"`
	TargetSeconds float64 `json:"targetSeconds,omitempty"`
	Verdict       Verdict `json:"verdict"`
}

// RPOFigure is one data-loss window: how old the newest recoverable point was at T0.
type RPOFigure struct {
	// Subject is the application or database the figure belongs to; empty applies to its namespace.
	Subject   string  `json:"subject"`
	Namespace string  `json:"namespace,omitempty"`
	Seconds   float64 `json:"seconds"`
	Human     string  `json:"human"`
	Kind      Kind    `json:"kind"`
	Source    string  `json:"source"`
	Note      string  `json:"note,omitempty"`
}

// AppResult is the outcome for one application.
type AppResult struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Tier       int    `json:"tier"`
	HealthKind string `json:"healthKind"`
	// Recovered is false when the application did not become healthy before the drill stopped waiting.
	Recovered bool       `json:"recovered"`
	GreenAt   time.Time  `json:"greenAt,omitzero"`
	RTO       Objective  `json:"rto"`
	RPO       *Objective `json:"rpo,omitempty"`
	Verdict   Verdict    `json:"verdict"`
	Notes     []string   `json:"notes,omitempty"`
}

// DatabaseResult is the outcome for one database.
type DatabaseResult struct {
	Name      string     `json:"name"`
	Namespace string     `json:"namespace"`
	Kind      string     `json:"kind"`
	RTO       *Objective `json:"rto,omitempty"`
	RPO       *Objective `json:"rpo,omitempty"`
	Verdict   Verdict    `json:"verdict"`
	Notes     []string   `json:"notes,omitempty"`
}

// AnalysisInput is everything the arithmetic needs; it contains no clock and no cluster.
type AnalysisInput struct {
	Plan *dr.Plan
	// T0 is the moment the disaster is declared.
	T0 time.Time
	// Down and Up are the two runs. Either can be nil (a drill that never got to run "up").
	Down, Up *dr.RunResult
	// DataRestoredAt overrides the data-restored stage (for example when a Velero restore finished).
	DataRestoredAt time.Time
	// Greens is when each application (by name) became healthy; an application missing here did not recover.
	Greens map[string]time.Time
	// End is when the drill stopped waiting; unrecovered applications are measured up to this moment.
	End time.Time
	// Figures are the RPO figures.
	Figures []RPOFigure
}

// Analysis is the computed result.
type Analysis struct {
	Stages    []Stage
	Apps      []AppResult
	Databases []DatabaseResult
	// RTO is the time from T0 until the last application is healthy (or the up run ends when the plan has no apps).
	RTO     Objective
	RPO     *Objective
	Verdict Verdict
}

// Analyze computes stages, RTO, RPO and the comparison with the targets of the plan.
func Analyze(in AnalysisInput) Analysis {
	plan := in.Plan
	if plan == nil {
		plan = &dr.Plan{}
	}
	out := Analysis{}
	out.Stages = buildStages(in)

	var verdicts []verdictPart
	var worstRTO time.Duration
	allRecovered := true

	for _, app := range plan.Apps {
		result := AppResult{Name: app.Name, Namespace: app.Namespace, Tier: app.Tier, HealthKind: healthKind(app)}
		green, ok := in.Greens[app.Name]
		since := in.End.Sub(in.T0)
		if ok {
			result.Recovered, result.GreenAt = true, green
			since = green.Sub(in.T0)
		} else {
			allRecovered = false
			result.Notes = append(result.Notes, "the application did not become healthy before the drill stopped waiting")
		}
		since = clampDuration(since)
		if since > worstRTO {
			worstRTO = since
		}

		target, hasTarget := appTarget(plan, app.RTO, plan.Targets.RTO)
		result.RTO = objective(since, KindMeasured, "health gate", target, hasTarget, result.Recovered)
		var rpoPart *verdictPart
		if worst, found := worstFigure(in.Figures, app.Name); found {
			rpoTarget, hasRPOTarget := appTarget(plan, app.RPO, plan.Targets.RPO)
			obj := figureObjective(worst, rpoTarget, hasRPOTarget)
			result.RPO = &obj
			rpoPart = &verdictPart{obj.Verdict, true}
		} else if _, hasRPOTarget := appTarget(plan, app.RPO, plan.Targets.RPO); hasRPOTarget {
			rpoPart = &verdictPart{VerdictUnknown, true}
			result.Notes = append(result.Notes, "an RPO target is set but no backup or heartbeat figure is available")
		}
		parts := []verdictPart{{result.RTO.Verdict, true}}
		if rpoPart != nil {
			parts = append(parts, *rpoPart)
		}
		result.Verdict = combine(parts)
		verdicts = append(verdicts, parts...)
		out.Apps = append(out.Apps, result)
	}

	for _, database := range plan.Databases {
		result := DatabaseResult{Name: database.Name, Namespace: database.Namespace, Kind: database.Kind}
		var parts []verdictPart
		if end, ok := stageTime(out.Stages, StageDatabasesUp); ok {
			target, hasTarget := appTarget(plan, database.RTO, plan.Targets.RTO)
			obj := objective(clampDuration(end.Sub(in.T0)), KindMeasured, "databases phase of the up run", target, hasTarget, true)
			result.RTO = &obj
			parts = append(parts, verdictPart{obj.Verdict, true})
		} else if in.Up != nil {
			result.Notes = append(result.Notes, "the up run has no databases phase, so no database RTO was measured")
		}
		rpoTarget, hasRPOTarget := appTarget(plan, database.RPO, plan.Targets.RPO)
		if worst, found := worstFigure(in.Figures, database.Name); found {
			obj := figureObjective(worst, rpoTarget, hasRPOTarget)
			result.RPO = &obj
			parts = append(parts, verdictPart{obj.Verdict, true})
		} else if hasRPOTarget {
			parts = append(parts, verdictPart{VerdictUnknown, true})
			result.Notes = append(result.Notes, "an RPO target is set but no backup figure is available")
		}
		result.Verdict = combine(parts)
		verdicts = append(verdicts, parts...)
		out.Databases = append(out.Databases, result)
	}

	// Overall RTO.
	overall := worstRTO
	if len(plan.Apps) == 0 && in.Up != nil && !in.Up.Finished.IsZero() {
		overall = clampDuration(in.Up.Finished.Sub(in.T0))
	}
	target, hasTarget := parseDuration(plan.Targets.RTO)
	out.RTO = objective(overall, KindMeasured, "health gates", target, hasTarget, allRecovered)
	verdicts = append(verdicts, verdictPart{out.RTO.Verdict, true})

	// Overall RPO: the worst figure of the drill.
	rpoTarget, hasRPOTarget := parseDuration(plan.Targets.RPO)
	if worst, found := worstFigure(in.Figures, ""); found {
		obj := figureObjective(worst, rpoTarget, hasRPOTarget)
		out.RPO = &obj
		verdicts = append(verdicts, verdictPart{obj.Verdict, true})
	} else if hasRPOTarget {
		verdicts = append(verdicts, verdictPart{VerdictUnknown, true})
	}

	out.Verdict = combine(verdicts)
	return out
}

// verdictPart is one comparison; applicable parts take part in the overall verdict.
type verdictPart struct {
	verdict    Verdict
	applicable bool
}

// combine returns fail if anything failed, unknown if anything could not be judged, pass otherwise.
func combine(parts []verdictPart) Verdict {
	result := VerdictPass
	judged := false
	for _, part := range parts {
		if !part.applicable {
			continue
		}
		judged = true
		switch part.verdict {
		case VerdictFail:
			return VerdictFail
		case VerdictUnknown:
			result = VerdictUnknown
		}
	}
	if !judged {
		return VerdictUnknown
	}
	return result
}

func objective(value time.Duration, kind Kind, source string, target time.Duration, hasTarget, recovered bool) Objective {
	obj := Objective{Seconds: seconds(value), Human: humanDuration(value), Kind: kind, Source: source, Verdict: VerdictUnknown}
	if hasTarget {
		obj.Target, obj.TargetSeconds = humanDuration(target), seconds(target)
	}
	switch {
	case !recovered:
		obj.Verdict = VerdictFail
	case hasTarget && value <= target:
		obj.Verdict = VerdictPass
	case hasTarget:
		obj.Verdict = VerdictFail
	}
	return obj
}

func figureObjective(figure RPOFigure, target time.Duration, hasTarget bool) Objective {
	value := time.Duration(figure.Seconds * float64(time.Second))
	obj := objective(value, figure.Kind, figure.Source, target, hasTarget, true)
	return obj
}

// worstFigure returns the largest data-loss window for a subject; an empty subject means every figure.
func worstFigure(figures []RPOFigure, subject string) (RPOFigure, bool) {
	var worst RPOFigure
	found := false
	for _, figure := range figures {
		if subject != "" && figure.Subject != subject {
			continue
		}
		if !found || figure.Seconds > worst.Seconds {
			worst, found = figure, true
		}
	}
	return worst, found
}

func buildStages(in AnalysisInput) []Stage {
	var stages []Stage
	add := func(key, title string, at time.Time) {
		if at.IsZero() {
			return
		}
		since := clampDuration(at.Sub(in.T0))
		stages = append(stages, Stage{Key: key, Title: title, At: at, Seconds: seconds(since), Human: humanDuration(since)})
	}

	add(StageDeclared, "Disaster declared (T0)", in.T0)
	if in.Up != nil {
		add(StageRecoveryStarted, "Recovery started", in.Up.Started)

		restored := in.DataRestoredAt
		if restored.IsZero() {
			// Without an explicit restore, the data layer is ready when the machines and nodes are.
			for _, phase := range []dr.Phase{dr.PhasePower, dr.PhaseNodes} {
				if end, ok := phaseEnd(in.Up, phase); ok && end.After(restored) {
					restored = end
				}
			}
		}
		add(StageDataRestored, "Data layer restored (machines, nodes, volumes available)", restored)

		if end, ok := phaseEnd(in.Up, dr.PhaseDatabases); ok {
			add(StageDatabasesUp, "Databases up", end)
		}
		if end, ok := phaseEnd(in.Up, dr.PhaseApps); ok {
			add(StageAppsUp, "Applications started", end)
		}
	}

	plan := in.Plan
	if plan != nil && len(plan.Apps) > 0 && len(in.Greens) == len(plan.Apps) {
		var last time.Time
		for _, green := range in.Greens {
			if green.After(last) {
				last = green
			}
		}
		add(StageHealthGreen, "Every application healthy", last)
	}
	return stages
}

func stageTime(stages []Stage, key string) (time.Time, bool) {
	for _, stage := range stages {
		if stage.Key == key {
			return stage.At, true
		}
	}
	return time.Time{}, false
}

// phaseEnd is when the last step of a phase finished, gate included.
func phaseEnd(run *dr.RunResult, phase dr.Phase) (time.Time, bool) {
	var end time.Time
	found := false
	for _, step := range run.Steps {
		if step.Phase != phase {
			continue
		}
		finished := step.Finished
		if finished.IsZero() && !step.Started.IsZero() {
			if waited, err := time.ParseDuration(step.GateWaited); err == nil {
				finished = step.Started.Add(waited)
			}
		}
		if finished.IsZero() {
			continue
		}
		found = true
		if finished.After(end) {
			end = finished
		}
	}
	return end, found
}

func healthKind(app dr.App) string {
	if app.Health != nil && app.Health.Kind != "" {
		return app.Health.Kind
	}
	return "pods-ready"
}

// appTarget picks the entry's own target, then the plan default.
func appTarget(_ *dr.Plan, own, fallback string) (time.Duration, bool) {
	if d, ok := parseDuration(own); ok {
		return d, true
	}
	return parseDuration(fallback)
}

func parseDuration(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

func clampDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

func seconds(d time.Duration) float64 {
	return math.Round(d.Seconds()*1000) / 1000
}

// humanDuration renders 125s as 2m05s.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d == 0 {
		return "0s"
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	total := int64(d.Round(time.Second) / time.Second)
	hours, minutes, secs := total/3600, total%3600/60, total%60
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh%02dm%02ds", hours, minutes, secs)
	case minutes > 0:
		return fmt.Sprintf("%dm%02ds", minutes, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

// figure builds an RPO figure from the age of the newest recoverable point at T0.
func figure(subject, namespace string, age time.Duration, kind Kind, source, note string) RPOFigure {
	age = clampDuration(age)
	return RPOFigure{Subject: subject, Namespace: namespace, Seconds: seconds(age), Human: humanDuration(age), Kind: kind, Source: source, Note: note}
}

func sortFigures(figures []RPOFigure) {
	sort.SliceStable(figures, func(i, j int) bool {
		if figures[i].Subject != figures[j].Subject {
			return figures[i].Subject < figures[j].Subject
		}
		return figures[i].Seconds > figures[j].Seconds
	})
}
