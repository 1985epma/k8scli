package measure

import (
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

func at(offset time.Duration) time.Time { return epoch.Add(offset) }

// upRun is a recovery that powers on, then starts the database, then the apps.
func upRun() *dr.RunResult {
	return &dr.RunResult{
		ID: "up-1", Direction: dr.DirUp, Status: dr.StatusSucceeded,
		Started: at(60 * time.Second), Finished: at(400 * time.Second),
		Steps: []dr.StepResult{
			{Phase: dr.PhasePower, Status: dr.StepDone, Started: at(60 * time.Second), Finished: at(90 * time.Second)},
			{Phase: dr.PhaseNodes, Status: dr.StepDone, Started: at(90 * time.Second), Finished: at(120 * time.Second)},
			{Phase: dr.PhaseDatabases, Status: dr.StepDone, Started: at(120 * time.Second), Finished: at(180 * time.Second)},
			{Phase: dr.PhaseApps, Status: dr.StepDone, Started: at(180 * time.Second), Finished: at(300 * time.Second)},
			{Phase: dr.PhaseApps, Status: dr.StepDone, Started: at(300 * time.Second), Finished: at(350 * time.Second)},
		},
	}
}

func stageSeconds(t *testing.T, stages []Stage, key string) float64 {
	t.Helper()
	for _, stage := range stages {
		if stage.Key == key {
			return stage.Seconds
		}
	}
	t.Fatalf("stage %q missing from %+v", key, stages)
	return 0
}

func TestAnalyzeComputesStagesAndRTOFromT0(t *testing.T) {
	plan := shopPlan()
	plan.Databases[0].RTO = "5m" // the database is up at 180s: inside its target
	analysis := Analyze(AnalysisInput{
		Plan: plan, T0: at(0), Up: upRun(),
		Greens: map[string]time.Time{"api": at(240 * time.Second), "web": at(400 * time.Second)},
		End:    at(400 * time.Second),
	})

	want := map[string]float64{
		StageDeclared: 0, StageRecoveryStarted: 60, StageDataRestored: 120,
		StageDatabasesUp: 180, StageAppsUp: 350, StageHealthGreen: 400,
	}
	for key, seconds := range want {
		if got := stageSeconds(t, analysis.Stages, key); got != seconds {
			t.Errorf("stage %s = %vs; want %vs", key, got, seconds)
		}
	}

	if analysis.RTO.Seconds != 400 || analysis.RTO.Target != "10m00s" {
		t.Errorf("overall RTO = %+v", analysis.RTO)
	}
	if analysis.RTO.Kind != KindMeasured || analysis.RTO.Verdict != VerdictPass {
		t.Errorf("overall RTO should be a measured pass: %+v", analysis.RTO)
	}

	api, web := analysis.Apps[0], analysis.Apps[1]
	if api.RTO.Seconds != 240 || api.RTO.Verdict != VerdictPass || !api.Recovered {
		t.Errorf("api: %+v", api)
	}
	// web has no target of its own, so it uses the plan default (10m): 400s passes.
	if web.RTO.Seconds != 400 || web.RTO.Verdict != VerdictPass {
		t.Errorf("web: %+v", web)
	}
	if db := analysis.Databases[0]; db.RTO == nil || db.RTO.Seconds != 180 || db.RTO.Verdict != VerdictPass {
		t.Errorf("db: %+v", analysis.Databases[0])
	}
	// RPO targets exist but there are no figures: the verdict cannot be a pass.
	if analysis.Verdict != VerdictUnknown {
		t.Errorf("verdict = %s; want unknown because the RPO targets could not be checked", analysis.Verdict)
	}
}

func TestAnalyzeFailsWhenATargetIsMissed(t *testing.T) {
	plan := shopPlan() // db target is 4m: 180s passes; make the api slow instead
	analysis := Analyze(AnalysisInput{
		Plan: plan, T0: at(0), Up: upRun(),
		Greens: map[string]time.Time{"api": at(330 * time.Second), "web": at(400 * time.Second)}, // api target is 5m = 300s
		End:    at(400 * time.Second),
		Figures: []RPOFigure{
			figure("api", "shop", time.Hour, KindEstimated, "backup:shop", ""),
			figure("web", "shop", time.Hour, KindEstimated, "backup:shop", ""),
			figure("db", "shop", time.Hour, KindEstimated, "backup:shop", ""),
		},
	})
	if analysis.Apps[0].RTO.Verdict != VerdictFail || analysis.Apps[0].Verdict != VerdictFail {
		t.Errorf("api RTO 330s must fail a 5m target: %+v", analysis.Apps[0])
	}
	if analysis.Apps[1].Verdict != VerdictPass {
		t.Errorf("web should still pass: %+v", analysis.Apps[1])
	}
	if analysis.Verdict != VerdictFail {
		t.Errorf("overall verdict = %s; want fail", analysis.Verdict)
	}
}

func TestAnalyzePassesWhenEverythingIsWithinTargets(t *testing.T) {
	plan := shopPlan()
	analysis := Analyze(AnalysisInput{
		Plan: plan, T0: at(0), Up: upRun(),
		Greens: map[string]time.Time{"api": at(120 * time.Second), "web": at(250 * time.Second)},
		End:    at(250 * time.Second),
		Figures: []RPOFigure{
			figure("api", "shop", 30*time.Minute, KindMeasured, "heartbeat:data", ""),
			figure("web", "shop", 30*time.Minute, KindEstimated, "backup:shop", ""),
			figure("db", "shop", 2*time.Hour, KindEstimated, "cnpg-backup", ""),
		},
	})
	if analysis.Verdict != VerdictPass {
		t.Fatalf("verdict = %s; want pass\n%+v", analysis.Verdict, analysis)
	}
	if analysis.RPO == nil || analysis.RPO.Seconds != 7200 || analysis.RPO.Kind != KindEstimated || analysis.RPO.Source != "cnpg-backup" {
		t.Errorf("overall RPO should be the worst figure with its source and kind: %+v", analysis.RPO)
	}
	if api := analysis.Apps[0]; api.RPO == nil || api.RPO.Kind != KindMeasured {
		t.Errorf("api RPO should come from the measured heartbeat: %+v", api.RPO)
	}
}

func TestAnalyzeReportsAnApplicationThatNeverRecovered(t *testing.T) {
	plan := shopPlan()
	analysis := Analyze(AnalysisInput{
		Plan: plan, T0: at(0), Up: upRun(),
		Greens: map[string]time.Time{"api": at(200 * time.Second)}, // web never turned healthy
		End:    at(1800 * time.Second),
	})
	web := analysis.Apps[1]
	if web.Recovered || web.Verdict != VerdictFail || web.RTO.Seconds != 1800 || len(web.Notes) == 0 {
		t.Errorf("web should be a failure that waited 1800s: %+v", web)
	}
	if analysis.RTO.Seconds != 1800 || analysis.RTO.Verdict != VerdictFail || analysis.Verdict != VerdictFail {
		t.Errorf("overall must fail: %+v / %s", analysis.RTO, analysis.Verdict)
	}
	for _, stage := range analysis.Stages {
		if stage.Key == StageHealthGreen {
			t.Error("health-green must not exist when an application never became healthy")
		}
	}
}

func TestAnalyzeWithoutAnyTargetIsUnknownNotPass(t *testing.T) {
	plan := shopPlan()
	plan.Targets = dr.Targets{}
	plan.Apps[0].RTO = ""
	plan.Databases[0].RTO = ""
	analysis := Analyze(AnalysisInput{
		Plan: plan, T0: at(0), Up: upRun(),
		Greens: map[string]time.Time{"api": at(10 * time.Second), "web": at(20 * time.Second)}, End: at(20 * time.Second),
	})
	if analysis.Verdict != VerdictUnknown || analysis.Apps[0].RTO.Verdict != VerdictUnknown {
		t.Errorf("no targets means unknown: %s / %+v", analysis.Verdict, analysis.Apps[0].RTO)
	}
	if analysis.Apps[0].RTO.Target != "" {
		t.Errorf("no target should be shown: %+v", analysis.Apps[0].RTO)
	}
}

func TestAnalyzeAppTargetBeatsPlanDefaultAndBadTargetsAreIgnored(t *testing.T) {
	plan := shopPlan()
	plan.Targets.RTO = "1h"
	plan.Apps[0].RTO = "not-a-duration" // ignored: the plan default applies
	plan.Apps[1].RTO = "100s"
	analysis := Analyze(AnalysisInput{
		Plan: plan, T0: at(0), Up: upRun(),
		Greens: map[string]time.Time{"api": at(500 * time.Second), "web": at(150 * time.Second)}, End: at(500 * time.Second),
	})
	if analysis.Apps[0].RTO.Verdict != VerdictPass || analysis.Apps[0].RTO.TargetSeconds != 3600 {
		t.Errorf("api should use the plan default: %+v", analysis.Apps[0].RTO)
	}
	if analysis.Apps[1].RTO.Verdict != VerdictFail || analysis.Apps[1].RTO.TargetSeconds != 100 {
		t.Errorf("web should use its own target: %+v", analysis.Apps[1].RTO)
	}
}

func TestAnalyzeExplicitDataRestoredAndMissingPhases(t *testing.T) {
	plan := &dr.Plan{Name: "tiny", Context: "dev", Apps: []dr.App{{Name: "a", Namespace: "n"}}}
	up := &dr.RunResult{Started: at(10 * time.Second), Finished: at(50 * time.Second), Steps: []dr.StepResult{
		{Phase: dr.PhaseApps, Status: dr.StepDone, Finished: at(50 * time.Second)},
	}}
	analysis := Analyze(AnalysisInput{
		Plan: plan, T0: at(0), Up: up, DataRestoredAt: at(30 * time.Second),
		Greens: map[string]time.Time{"a": at(55 * time.Second)}, End: at(55 * time.Second),
	})
	if got := stageSeconds(t, analysis.Stages, StageDataRestored); got != 30 {
		t.Errorf("explicit data-restored = %v", got)
	}
	for _, stage := range analysis.Stages {
		if stage.Key == StageDatabasesUp {
			t.Error("there is no database phase, so there must be no databases-up stage")
		}
	}

	// Without an explicit time and without infrastructure phases there is no data-restored stage.
	again := Analyze(AnalysisInput{Plan: plan, T0: at(0), Up: up, Greens: map[string]time.Time{"a": at(55 * time.Second)}, End: at(55 * time.Second)})
	for _, stage := range again.Stages {
		if stage.Key == StageDataRestored {
			t.Error("data-restored should not be invented")
		}
	}
}

func TestAnalyzeNoAppsUsesTheEndOfTheUpRunAndNilPlanIsSafe(t *testing.T) {
	plan := &dr.Plan{Name: "dbonly", Context: "dev", Targets: dr.Targets{RTO: "10m"}}
	analysis := Analyze(AnalysisInput{Plan: plan, T0: at(0), Up: &dr.RunResult{Started: at(5 * time.Second), Finished: at(200 * time.Second)}, End: at(200 * time.Second)})
	if analysis.RTO.Seconds != 200 || analysis.RTO.Verdict != VerdictPass {
		t.Errorf("RTO = %+v", analysis.RTO)
	}
	_ = Analyze(AnalysisInput{}) // must not panic
}

func TestAnalyzeClampsNegativeDurations(t *testing.T) {
	plan := &dr.Plan{Name: "x", Context: "dev", Apps: []dr.App{{Name: "a", Namespace: "n"}}}
	analysis := Analyze(AnalysisInput{Plan: plan, T0: at(100 * time.Second), Greens: map[string]time.Time{"a": at(50 * time.Second)}, End: at(100 * time.Second)})
	if analysis.Apps[0].RTO.Seconds != 0 {
		t.Errorf("an application that was never down has an RTO of 0, got %v", analysis.Apps[0].RTO.Seconds)
	}
}

func TestPhaseEndFallsBackToGateWaited(t *testing.T) {
	run := &dr.RunResult{Steps: []dr.StepResult{{Phase: dr.PhaseApps, Started: at(10 * time.Second), GateWaited: "20s"}}}
	end, ok := phaseEnd(run, dr.PhaseApps)
	if !ok || !end.Equal(at(30*time.Second)) {
		t.Errorf("phaseEnd = %v, %v", end, ok)
	}
	if _, ok := phaseEnd(run, dr.PhaseDatabases); ok {
		t.Error("a phase without steps has no end")
	}
}

func TestCombineAndHelpers(t *testing.T) {
	pass, fail, unknown := verdictPart{VerdictPass, true}, verdictPart{VerdictFail, true}, verdictPart{VerdictUnknown, true}
	for name, tc := range map[string]struct {
		parts []verdictPart
		want  Verdict
	}{
		"all pass":          {[]verdictPart{pass, pass}, VerdictPass},
		"fail wins":         {[]verdictPart{pass, unknown, fail}, VerdictFail},
		"unknown over pass": {[]verdictPart{pass, unknown}, VerdictUnknown},
		"nothing judged":    {nil, VerdictUnknown},
		"not applicable":    {[]verdictPart{{VerdictFail, false}}, VerdictUnknown},
	} {
		if got := combine(tc.parts); got != tc.want {
			t.Errorf("%s: %s; want %s", name, got, tc.want)
		}
	}

	for d, want := range map[time.Duration]string{
		0: "0s", 850 * time.Millisecond: "850ms", 45 * time.Second: "45s", 125 * time.Second: "2m05s",
		3723 * time.Second: "1h02m03s", -time.Second: "0s",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q; want %q", d, got, want)
		}
	}
	if got := seconds(1500 * time.Millisecond); got != 1.5 {
		t.Errorf("seconds = %v", got)
	}
	for in, ok := range map[string]bool{"30m": true, "": false, " 5s ": true, "-1m": false, "0s": false, "soon": false} {
		if _, got := parseDuration(in); got != ok {
			t.Errorf("parseDuration(%q) ok = %v", in, got)
		}
	}
}
