package measure

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// sampleReport is a complete, deterministic report: apps, a database, figures of both kinds, notices.
func sampleReport() *Report {
	plan := shopPlan()
	plan.Databases[0].RTO = "5m"
	figures := []RPOFigure{
		figure("api", "shop", 20*time.Minute, KindMeasured, "heartbeat:data", "includes clock skew"),
		figure("api", "shop", 3*time.Hour, KindEstimated, "backup:shop", "newest completed backup finished at 2026-01-01T09:00:00Z; no restore was tested"),
		figure("web", "shop", 3*time.Hour, KindEstimated, "backup:shop", ""),
		figure("db", "shop", 90*time.Minute, KindEstimated, "cnpg-backup", ""),
	}
	sortFigures(figures)
	analysis := Analyze(AnalysisInput{
		Plan: plan, T0: at(0), Up: upRun(),
		Greens:  map[string]time.Time{"api": at(240 * time.Second), "web": at(400 * time.Second)},
		End:     at(400 * time.Second),
		Figures: figures,
	})
	return &Report{
		Version: ReportVersion, ID: "drill-20260101T120000Z-0a1b2c", Plan: plan.Name, Context: plan.Context, Actor: "everton",
		Mode: "apps, databases", Outcome: OutcomeRecovered, Verdict: analysis.Verdict,
		Started: at(-5 * time.Second), Finished: at(410 * time.Second), T0: at(0),
		DownRun: "run-down", UpRun: "run-up",
		Stages: analysis.Stages, RTO: analysis.RTO, RPO: analysis.RPO, Apps: analysis.Apps, Databases: analysis.Databases, Figures: figures,
		Baseline: []AppState{
			{App: "api", Namespace: "shop", Healthy: true, Workloads: []WorkloadState{{Kind: "Deployment", Name: "api", Desired: 2, Ready: 2}}},
			{App: "web", Namespace: "shop", Healthy: true, Workloads: []WorkloadState{{Kind: "Deployment", Name: "web", Desired: 3, Ready: 3}}},
		},
		PostState: []AppState{
			{App: "api", Namespace: "shop", Healthy: true, Workloads: []WorkloadState{{Kind: "Deployment", Name: "api", Desired: 2, Ready: 2}}},
			{App: "web", Namespace: "shop", Healthy: true, Workloads: []WorkloadState{{Kind: "Deployment", Name: "web", Desired: 3, Ready: 3}}},
		},
		StateRestored: true,
		Notices:       []string{"no backup was found for namespace billing"},
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	report := sampleReport()
	first, err := report.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, first)
	}
	second, err := back.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("the report changed through a round trip:\n%s\n---\n%s", first, second)
	}
	if back.RTO.Seconds != 400 || len(back.Apps) != 2 || back.Apps[0].RTO.Verdict != VerdictPass || back.Version != ReportVersion {
		t.Errorf("unexpected content after the round trip: %+v", back.RTO)
	}
	for _, key := range []string{`"rto"`, `"rpoFigures"`, `"kind": "measured"`, `"kind": "estimated"`, `"stateRestored": true`} {
		if !strings.Contains(string(first), key) {
			t.Errorf("JSON is missing %s", key)
		}
	}
}

func TestReportMarkdownGolden(t *testing.T) {
	got := sampleReport().Markdown()
	golden := filepath.Join("testdata", "report.golden.md")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run `go test -run Golden -update` to create it)", err)
	}
	// A Windows checkout with autocrlf turns the golden file into CRLF; compare line endings as LF.
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("the Markdown report changed; if intended, run `go test -run Golden -update`.\n--- got ---\n%s", got)
	}
}

func TestMarkdownLabelsFiguresAndEscapesCells(t *testing.T) {
	report := sampleReport()
	report.Apps[0].Name = "a|b\nc"
	report.Failures = []string{"the down run did not finish"}
	report.StateRestored = false
	markdown := report.Markdown()

	for _, want := range []string{"measured", "estimated", "## Failures", "differs", `a\|b c`, "RPO figures"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("Markdown is missing %q", want)
		}
	}
	rows := 0
	for _, line := range strings.Split(markdown, "\n") {
		if strings.HasPrefix(line, `| shop/a\|b c |`) {
			rows++
			if strings.Count(strings.ReplaceAll(line, `\|`, ""), "|") != 9 {
				t.Errorf("an unescaped pipe broke the table row: %q", line)
			}
		}
	}
	if rows != 1 {
		t.Errorf("expected exactly one escaped application row, found %d", rows)
	}
}

func TestSaveWritesPrivateFilesAndLoadReadsThemBack(t *testing.T) {
	dir := t.TempDir()
	report := sampleReport()
	jsonPath, markdownPath, err := Save(dir, report)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(jsonPath) != filepath.Join(dir, "reports") || !strings.HasSuffix(markdownPath, ".md") {
		t.Errorf("unexpected paths: %s %s", jsonPath, markdownPath)
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{jsonPath: 0o600, markdownPath: 0o600, filepath.Dir(jsonPath): 0o700} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != want {
				t.Errorf("%s: mode %v, err %v; want %v", path, info.Mode().Perm(), err, want)
			}
		}
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(jsonPath), ".tmp-*")); len(leftovers) != 0 {
		t.Errorf("temporary files were left behind: %v", leftovers)
	}

	loaded, err := Load(dir, report.ID)
	if err != nil || loaded.ID != report.ID || loaded.RTO.Seconds != report.RTO.Seconds {
		t.Errorf("Load: %+v, %v", loaded, err)
	}
	if _, err := Load(dir, "drill-does-not-exist"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing report should be os.ErrNotExist, got %v", err)
	}
}

func TestReportIDsCannotEscapeTheDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", "../escape", "a/b", `a\b`, ".hidden", "-flag-like-but-ok-pattern-starts-with-dash", "x y", strings.Repeat("a", 200)} {
		report := sampleReport()
		report.ID = id
		if _, _, err := Save(dir, report); !errors.Is(err, ErrInvalidReportID) {
			t.Errorf("Save(%q) = %v; want ErrInvalidReportID", id, err)
		}
		if _, err := Load(dir, id); !errors.Is(err, ErrInvalidReportID) {
			t.Errorf("Load(%q) = %v; want ErrInvalidReportID", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.json")); err == nil {
		t.Error("a report was written outside the directory")
	}
}

func TestHistoryIsNewestFirstLimitedAndIgnoresJunk(t *testing.T) {
	dir := t.TempDir()
	if history, err := History(dir, 5); err != nil || history != nil {
		t.Errorf("no reports yet: %v, %v", history, err)
	}

	for i, name := range []string{"oldest", "middle", "newest"} {
		report := sampleReport()
		report.ID = "drill-" + name
		report.Started = at(time.Duration(i) * time.Hour)
		if _, _, err := Save(dir, report); err != nil {
			t.Fatal(err)
		}
	}
	reports := filepath.Join(dir, "reports")
	for name, content := range map[string]string{"corrupt.json": "{not json", "empty.json": "{}", ".tmp-123.json": "{}", "notes.txt": "hello"} {
		if err := os.WriteFile(filepath.Join(reports, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(reports, "sub.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	history, err := History(dir, 0)
	if err != nil || len(history) != 3 {
		t.Fatalf("History = %d reports, %v", len(history), err)
	}
	if history[0].ID != "drill-newest" || history[2].ID != "drill-oldest" {
		t.Errorf("order: %s, %s, %s", history[0].ID, history[1].ID, history[2].ID)
	}
	if limited, _ := History(dir, 2); len(limited) != 2 || limited[0].ID != "drill-newest" {
		t.Errorf("limit: %+v", limited)
	}
}

func TestRTOTrend(t *testing.T) {
	var reports []*Report
	for i, seconds := range []float64{100, 200, 300, 400} {
		report := sampleReport()
		report.ID = "drill-" + string(rune('a'+i))
		report.Started = at(time.Duration(i) * time.Hour)
		report.RTO.Seconds = seconds
		report.Apps[0].RTO.Seconds = seconds / 2
		reports = append(reports, report)
	}
	// Give it newest-first, the order History returns.
	for i, j := 0, len(reports)-1; i < j; i, j = i+1, j-1 {
		reports[i], reports[j] = reports[j], reports[i]
	}

	overall := RTOTrend(reports, OverallApp, 3)
	if len(overall) != 3 || overall[0].Seconds != 200 || overall[2].Seconds != 400 {
		t.Errorf("overall trend (oldest first, last 3): %+v", overall)
	}
	api := RTOTrend(reports, "api", 0)
	if len(api) != 4 || api[0].Seconds != 50 || api[3].Seconds != 200 {
		t.Errorf("api trend: %+v", api)
	}
	if got := RTOTrend(reports, "unknown-app", 5); len(got) != 0 {
		t.Errorf("an application that was never drilled has no trend: %+v", got)
	}
}

func TestPrometheusText(t *testing.T) {
	report := sampleReport()
	report.Plan = `sh"op\x`
	report.Apps[1].RPO = nil // an objective that was not judged
	text := PrometheusText(report)

	for _, want := range []string{
		"# TYPE k8scli_dr_rto_seconds gauge",
		`k8scli_dr_rto_seconds{plan="sh\"op\\x",app="_overall"} 400`,
		`k8scli_dr_rto_seconds{plan="sh\"op\\x",app="api"} 240`,
		`k8scli_dr_rpo_seconds{plan="sh\"op\\x",subject="api",kind="measured"} 1200`,
		`k8scli_dr_rpo_seconds{plan="sh\"op\\x",subject="db",kind="estimated"} 5400`,
		`k8scli_dr_target_met{plan="sh\"op\\x",app="api",objective="rto"} 1`,
		"k8scli_dr_drill_timestamp_seconds",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}

	failing := sampleReport()
	failing.Apps[0].RTO.Verdict = VerdictFail
	failing.Apps[1].RTO.Verdict = VerdictUnknown
	text = PrometheusText(failing)
	if !strings.Contains(text, `app="api",objective="rto"} 0`) {
		t.Errorf("a missed target must be 0:\n%s", text)
	}
	if strings.Contains(text, `app="web",objective="rto"`) {
		t.Errorf("a target that could not be judged must be omitted, not reported as 0:\n%s", text)
	}
}
