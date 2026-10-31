package measure

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

// ReportVersion is the schema version of a stored report.
const ReportVersion = 1

// Outcome says whether the system came back.
type Outcome string

// Outcomes.
const (
	// OutcomeRecovered: every application is healthy again and the replica counts match the baseline.
	OutcomeRecovered Outcome = "recovered"
	// OutcomeDifferences: healthy again, but the state after the drill differs from the baseline.
	OutcomeDifferences Outcome = "recovered-with-differences"
	// OutcomeNotRecovered: the up run failed or an application never became healthy.
	OutcomeNotRecovered Outcome = "not-recovered"
)

// WorkloadState is the replica situation of one workload.
type WorkloadState struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Desired int32  `json:"desired"`
	Ready   int32  `json:"ready"`
}

// AppState is the readiness of one application at a point in time.
type AppState struct {
	App       string          `json:"app"`
	Namespace string          `json:"namespace"`
	Healthy   bool            `json:"healthy"`
	Workloads []WorkloadState `json:"workloads,omitempty"`
}

// Report is the record of one drill. It is stored as JSON and rendered as Markdown.
type Report struct {
	Version  int       `json:"version"`
	ID       string    `json:"id"`
	Plan     string    `json:"plan"`
	Context  string    `json:"context"`
	Actor    string    `json:"actor,omitempty"`
	Mode     string    `json:"mode"`
	Outcome  Outcome   `json:"outcome"`
	Verdict  Verdict   `json:"verdict"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	// T0 is when the disaster was declared; every duration in the report is measured from it.
	T0      time.Time `json:"t0"`
	DownRun string    `json:"downRun,omitempty"`
	UpRun   string    `json:"upRun,omitempty"`

	Stages    []Stage          `json:"stages"`
	RTO       Objective        `json:"rto"`
	RPO       *Objective       `json:"rpo,omitempty"`
	Apps      []AppResult      `json:"apps"`
	Databases []DatabaseResult `json:"databases,omitempty"`
	Figures   []RPOFigure      `json:"rpoFigures,omitempty"`

	Baseline      []AppState `json:"baseline,omitempty"`
	PostState     []AppState `json:"postState,omitempty"`
	StateRestored bool       `json:"stateRestored"`

	// Failures are things that went wrong (a run failed); Notices are things worth knowing.
	Failures []string `json:"failures,omitempty"`
	Notices  []string `json:"notices,omitempty"`

	DownSteps []dr.StepResult `json:"downSteps,omitempty"`
	UpSteps   []dr.StepResult `json:"upSteps,omitempty"`
}

// JSON renders the report as indented JSON.
func (r *Report) JSON() ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r); err != nil {
		return nil, fmt.Errorf("failed to encode the report: %w", err)
	}
	return buffer.Bytes(), nil
}

// Markdown renders a report meant to be read by people and attached to an audit.
func (r *Report) Markdown() string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	line("# DR drill report: %s", r.Plan)
	line("")
	line("- Report: `%s`", r.ID)
	line("- Context: `%s`", r.Context)
	if r.Actor != "" {
		line("- Actor: %s", r.Actor)
	}
	line("- Mode: %s", r.Mode)
	line("- Started: %s", r.Started.UTC().Format(time.RFC3339))
	line("- Finished: %s", r.Finished.UTC().Format(time.RFC3339))
	line("- Disaster declared (T0): %s", r.T0.UTC().Format(time.RFC3339))
	line("- Outcome: **%s**", r.Outcome)
	line("- Verdict against the targets: **%s**", strings.ToUpper(string(r.Verdict)))
	if r.DownRun != "" || r.UpRun != "" {
		line("- Runs: down `%s`, up `%s`", orDash(r.DownRun), orDash(r.UpRun))
	}
	line("")

	if len(r.Failures) > 0 {
		line("## Failures")
		line("")
		for _, failure := range r.Failures {
			line("- %s", failure)
		}
		line("")
	}

	line("## Result at a glance")
	line("")
	line("| Objective | Measured | Target | Verdict | Kind |")
	line("|-----------|----------|--------|---------|------|")
	line("| RTO (all applications) | %s | %s | %s | %s |", r.RTO.Human, orDash(r.RTO.Target), r.RTO.Verdict, r.RTO.Kind)
	if r.RPO != nil {
		line("| RPO (worst figure) | %s | %s | %s | %s |", r.RPO.Human, orDash(r.RPO.Target), r.RPO.Verdict, r.RPO.Kind)
	} else {
		line("| RPO (worst figure) | no figure | - | unknown | - |")
	}
	line("")
	line("RTO is measured from T0 until every application passes its health check. An *estimated* RPO is the age")
	line("of the newest backup at T0: nothing was restored to prove it. A *measured* RPO comes from the heartbeat.")
	line("")

	line("## Timeline")
	line("")
	line("| Stage | At (UTC) | Since T0 |")
	line("|-------|----------|----------|")
	for _, stage := range r.Stages {
		line("| %s | %s | %s |", cell(stage.Title), stage.At.UTC().Format("15:04:05"), stage.Human)
	}
	line("")

	if len(r.Apps) > 0 {
		line("## Applications")
		line("")
		line("| Application | Tier | Health | RTO | RTO target | RPO | RPO target | Verdict |")
		line("|-------------|------|--------|-----|------------|-----|------------|---------|")
		for _, app := range r.Apps {
			rto := app.RTO.Human
			if !app.Recovered {
				rto = "not recovered (waited " + app.RTO.Human + ")"
			}
			rpo, rpoTarget := "-", "-"
			if app.RPO != nil {
				rpo, rpoTarget = fmt.Sprintf("%s (%s)", app.RPO.Human, app.RPO.Kind), orDash(app.RPO.Target)
			}
			line("| %s/%s | %d | %s | %s | %s | %s | %s | %s |", cell(app.Namespace), cell(app.Name), app.Tier, app.HealthKind, cell(rto), orDash(app.RTO.Target), cell(rpo), rpoTarget, app.Verdict)
		}
		line("")
		notes := false
		for _, app := range r.Apps {
			for _, note := range app.Notes {
				line("- %s/%s: %s", app.Namespace, app.Name, note)
				notes = true
			}
		}
		if notes {
			line("")
		}
	}

	if len(r.Databases) > 0 {
		line("## Databases")
		line("")
		line("| Database | Kind | RTO | RTO target | RPO | RPO target | Verdict |")
		line("|----------|------|-----|------------|-----|------------|---------|")
		for _, database := range r.Databases {
			rto, rtoTarget, rpo, rpoTarget := "-", "-", "-", "-"
			if database.RTO != nil {
				rto, rtoTarget = database.RTO.Human, orDash(database.RTO.Target)
			}
			if database.RPO != nil {
				rpo, rpoTarget = fmt.Sprintf("%s (%s)", database.RPO.Human, database.RPO.Kind), orDash(database.RPO.Target)
			}
			line("| %s/%s | %s | %s | %s | %s | %s | %s |", cell(database.Namespace), cell(database.Name), database.Kind, rto, rtoTarget, cell(rpo), rpoTarget, database.Verdict)
		}
		line("")
	}

	if len(r.Figures) > 0 {
		line("## RPO figures")
		line("")
		line("| Subject | Namespace | Data-loss window | Kind | Source | Note |")
		line("|---------|-----------|------------------|------|--------|------|")
		for _, f := range r.Figures {
			line("| %s | %s | %s | %s | %s | %s |", orDash(cell(f.Subject)), orDash(cell(f.Namespace)), f.Human, f.Kind, cell(f.Source), orDash(cell(f.Note)))
		}
		line("")
	}

	line("## State after the drill")
	line("")
	if r.StateRestored {
		line("The replica counts and readiness after the drill match the baseline taken before it.")
	} else {
		line("The state after the drill **differs** from the baseline, or it could not be compared.")
	}
	line("")
	if len(r.Baseline) > 0 {
		line("| Application | Baseline | After |")
		line("|-------------|----------|-------|")
		after := map[string]AppState{}
		for _, state := range r.PostState {
			after[state.Namespace+"/"+state.App] = state
		}
		for _, state := range r.Baseline {
			line("| %s/%s | %s | %s |", cell(state.Namespace), cell(state.App), summarizeState(state), summarizeState(after[state.Namespace+"/"+state.App]))
		}
		line("")
	}

	if len(r.Notices) > 0 {
		line("## Notices")
		line("")
		for _, notice := range r.Notices {
			line("- %s", notice)
		}
		line("")
	}
	return b.String()
}

func summarizeState(state AppState) string {
	if len(state.Workloads) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(state.Workloads))
	for _, workload := range state.Workloads {
		parts = append(parts, fmt.Sprintf("%s %d/%d", workload.Name, workload.Ready, workload.Desired))
	}
	return cell(strings.Join(parts, ", "))
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// cell makes text safe inside a Markdown table cell.
func cell(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.ReplaceAll(value, "|", `\|`)
}

// ---- storage ---------------------------------------------------------------------------------------------

var reportIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ErrInvalidReportID is returned for ids that could be a path.
var ErrInvalidReportID = errors.New("invalid report id")

// DefaultDir is where drills keep their data unless told otherwise.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find the home directory: %w", err)
	}
	return filepath.Join(home, ".k8scli", "dr"), nil
}

func reportsDir(dir string) (string, error) {
	if dir == "" {
		base, err := DefaultDir()
		if err != nil {
			return "", err
		}
		dir = base
	}
	return filepath.Join(dir, "reports"), nil
}

// Save writes <dir>/reports/<id>.json and .md (files 0600, directory 0700) and returns both paths.
func Save(dir string, report *Report) (jsonPath, markdownPath string, err error) {
	if !reportIDPattern.MatchString(report.ID) {
		return "", "", fmt.Errorf("%w: %q", ErrInvalidReportID, report.ID)
	}
	reports, err := reportsDir(dir)
	if err != nil {
		return "", "", err
	}
	// 0700 is best effort on Windows (no Unix permission bits; see internal/dr/engine/files.go).
	if err := os.MkdirAll(reports, 0o700); err != nil {
		return "", "", fmt.Errorf("failed to create %s: %w", reports, err)
	}

	data, err := report.JSON()
	if err != nil {
		return "", "", err
	}
	jsonPath = filepath.Join(reports, report.ID+".json")
	markdownPath = filepath.Join(reports, report.ID+".md")
	if err := writeAtomic(jsonPath, data); err != nil {
		return "", "", err
	}
	if err := writeAtomic(markdownPath, []byte(report.Markdown())); err != nil {
		return "", "", err
	}
	return jsonPath, markdownPath, nil
}

// writeAtomic writes through a temporary file in the same directory so a reader never sees half a report.
func writeAtomic(target string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(target), ".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to write %s: %w", target, err)
	}
	name := temp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		cleanup()
		return fmt.Errorf("failed to write %s: %w", target, err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		cleanup()
		return fmt.Errorf("failed to write %s: %w", target, err)
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("failed to write %s: %w", target, err)
	}
	if err := os.Rename(name, target); err != nil {
		cleanup()
		return fmt.Errorf("failed to write %s: %w", target, err)
	}
	return nil
}

// Load reads a stored report by id.
func Load(dir, id string) (*Report, error) {
	if !reportIDPattern.MatchString(id) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidReportID, id)
	}
	reports, err := reportsDir(dir)
	if err != nil {
		return nil, err
	}
	return readReport(filepath.Join(reports, id+".json"))
}

func readReport(file string) (*Report, error) {
	data, err := os.ReadFile(file) // #nosec G304 -- the name is <dir>/reports/<validated id>.json or comes from a directory listing
	if err != nil {
		return nil, err
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("report %s is corrupt: %w", filepath.Base(file), err)
	}
	return &report, nil
}

// History returns stored reports, newest first. Unreadable or corrupt files are skipped.
func History(dir string, limit int) ([]*Report, error) {
	reports, err := reportsDir(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(reports)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read %s: %w", reports, err)
	}

	var found []*Report
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		report, err := readReport(filepath.Join(reports, entry.Name()))
		if err != nil || report.ID == "" {
			continue
		}
		found = append(found, report)
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Started.After(found[j].Started) })
	if limit > 0 && len(found) > limit {
		found = found[:limit]
	}
	return found, nil
}

// TrendPoint is one drill in the RTO trend of an application.
type TrendPoint struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Seconds   float64   `json:"seconds"`
	Recovered bool      `json:"recovered"`
	Verdict   Verdict   `json:"verdict"`
}

// OverallApp is the app name that selects the overall RTO in RTOTrend.
const OverallApp = "_overall"

// RTOTrend returns the RTO of the last n drills (oldest first) for one application, or for the whole
// drill with OverallApp. The input may be in any order; drills without that application are skipped.
func RTOTrend(reports []*Report, app string, n int) []TrendPoint {
	ordered := append([]*Report(nil), reports...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Started.Before(ordered[j].Started) })

	var points []TrendPoint
	for _, report := range ordered {
		if app == "" || app == OverallApp {
			points = append(points, TrendPoint{ID: report.ID, At: report.Started, Seconds: report.RTO.Seconds, Recovered: report.Outcome != OutcomeNotRecovered, Verdict: report.RTO.Verdict})
			continue
		}
		for _, result := range report.Apps {
			if result.Name == app {
				points = append(points, TrendPoint{ID: report.ID, At: report.Started, Seconds: result.RTO.Seconds, Recovered: result.Recovered, Verdict: result.Verdict})
			}
		}
	}
	if n > 0 && len(points) > n {
		points = points[len(points)-n:]
	}
	return points
}

// ---- Prometheus text ---------------------------------------------------------------------------------------

// PrometheusText renders the report in the Prometheus text exposition format, for a textfile collector
// or a pushgateway. Targets that could not be judged are left out instead of being reported as 0.
func PrometheusText(report *Report) string {
	var b strings.Builder
	plan := escapeLabel(report.Plan)
	header := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}

	header("k8scli_dr_rto_seconds", "Measured time from the disaster being declared until the application is healthy again.")
	fmt.Fprintf(&b, "k8scli_dr_rto_seconds{plan=\"%s\",app=\"%s\"} %s\n", plan, OverallApp, formatFloat(report.RTO.Seconds))
	for _, app := range report.Apps {
		fmt.Fprintf(&b, "k8scli_dr_rto_seconds{plan=\"%s\",app=\"%s\"} %s\n", plan, escapeLabel(app.Name), formatFloat(app.RTO.Seconds))
	}

	header("k8scli_dr_rpo_seconds", "Age of the newest recoverable point at the moment the disaster was declared.")
	for _, f := range report.Figures {
		fmt.Fprintf(&b, "k8scli_dr_rpo_seconds{plan=\"%s\",subject=\"%s\",kind=\"%s\"} %s\n", plan, escapeLabel(f.Subject), f.Kind, formatFloat(f.Seconds))
	}

	header("k8scli_dr_target_met", "1 when the objective met its target, 0 when it did not.")
	emit := func(app, objectiveName string, obj *Objective) {
		if obj == nil || obj.Verdict == VerdictUnknown {
			return
		}
		value := "0"
		if obj.Verdict == VerdictPass {
			value = "1"
		}
		fmt.Fprintf(&b, "k8scli_dr_target_met{plan=\"%s\",app=\"%s\",objective=\"%s\"} %s\n", plan, escapeLabel(app), objectiveName, value)
	}
	emit(OverallApp, "rto", &report.RTO)
	emit(OverallApp, "rpo", report.RPO)
	for i := range report.Apps {
		emit(report.Apps[i].Name, "rto", &report.Apps[i].RTO)
		emit(report.Apps[i].Name, "rpo", report.Apps[i].RPO)
	}

	header("k8scli_dr_drill_timestamp_seconds", "Unix time the drill started.")
	fmt.Fprintf(&b, "k8scli_dr_drill_timestamp_seconds{plan=\"%s\"} %d\n", plan, report.Started.Unix())
	return b.String()
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return strings.ReplaceAll(value, "\n", `\n`)
}

func formatFloat(value float64) string {
	return fmt.Sprintf("%g", value)
}
