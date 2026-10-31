package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/engine"
)

// Plans are YAML files in <drDir>/plans. The browser only ever sees names; the files are read and
// written here, strictly parsed, with the same validation as the CLI.

// planFile is one plan as found on disk.
type planFile struct {
	Name     string
	Raw      []byte
	Plan     *dr.Plan // nil when the file does not parse
	Problems []string
}

func (p *planFile) Valid() bool { return p.Plan != nil && len(p.Problems) == 0 }

// kubeconfigContexts returns the context names of the kubeconfig, or none when it cannot be read.
func (s *Server) kubeconfigContexts() map[string]bool {
	names := map[string]bool{}
	contexts, err := s.loader.Contexts()
	if err != nil {
		return names
	}
	for _, c := range contexts {
		names[c.Name] = true
	}
	return names
}

// inspectPlan parses and validates a plan and checks its contexts against the kubeconfig. A plan whose
// context is missing is reported as invalid, not as an error: the person may be about to add it.
func (s *Server) inspectPlan(name string, raw []byte) *planFile {
	file := &planFile{Name: name, Raw: raw, Problems: []string{}}

	plan, err := dr.DecodePlan(raw)
	if err != nil {
		file.Problems = append(file.Problems, err.Error())
		return file
	}
	file.Plan = plan

	if err := plan.Validate(); err != nil {
		var planErr *dr.PlanError
		if errors.As(err, &planErr) {
			file.Problems = append(file.Problems, planErr.Problems...)
		} else {
			file.Problems = append(file.Problems, err.Error())
		}
	}

	contexts := s.kubeconfigContexts()
	if plan.Context != "" && !contexts[plan.Context] {
		file.Problems = append(file.Problems, fmt.Sprintf("context %q is not in the kubeconfig", plan.Context))
	}
	if plan.DRContext != "" && !contexts[plan.DRContext] {
		file.Problems = append(file.Problems, fmt.Sprintf("drContext %q is not in the kubeconfig", plan.DRContext))
	}
	return file
}

// readPlanFile reads <plansDir>/<name>.yaml. The name is validated first, symbolic links are refused
// (a planted link must not make the console read another file) and the size is capped.
func (d *drService) readPlanFile(name string) ([]byte, error) {
	if !planName.MatchString(name) {
		return nil, drBadRequest("invalid_name", "plan names use lowercase letters, digits and dashes (at most 63 characters)")
	}
	if d.dir == "" {
		return nil, &drError{status: http.StatusServiceUnavailable, code: "dr_unavailable", msg: "no directory for DR data (set --dr-dir)"}
	}
	file := filepath.Join(d.plansDir(), name+".yaml")
	// #nosec G703 -- the name matched planName above, so the path stays inside the plans directory.
	info, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, &drError{status: http.StatusNotFound, code: "plan_not_found", msg: fmt.Sprintf("there is no plan named %q", name)}
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, drBadRequest("invalid_plan_file", fmt.Sprintf("%q is not a regular file", name))
	}
	if info.Size() > maxPlanFile {
		return nil, drBadRequest("plan_too_large", fmt.Sprintf("the plan is larger than %d KiB", maxPlanFile>>10))
	}
	// #nosec G304 G703 -- name matches planName, so the path stays inside the plans directory.
	return os.ReadFile(file)
}

// loadPlan returns a plan that can be run: valid, with its context in the kubeconfig.
func (s *Server) loadPlan(name string) (*dr.Plan, *planFile, error) {
	raw, err := s.dr.readPlanFile(name)
	if err != nil {
		return nil, nil, err
	}
	file := s.inspectPlan(name, raw)
	if !file.Valid() {
		return nil, file, &drError{status: http.StatusBadRequest, code: "invalid_plan", msg: fmt.Sprintf("plan %q is not valid", name), problems: file.Problems}
	}
	return file.Plan, file, nil
}

// ---- list --------------------------------------------------------------------------------------------

type drPlanApp struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Tier      int    `json:"tier"`
}

type drPlanDatabase struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Tier      int    `json:"tier"`
}

type drPhrases struct {
	Down string `json:"down"`
	Up   string `json:"up"`
}

type drPlanSummary struct {
	Name          string           `json:"name"`
	Valid         bool             `json:"valid"`
	Problems      []string         `json:"problems"`
	Context       string           `json:"context"`
	DRContext     string           `json:"drContext,omitempty"`
	Apps          []drPlanApp      `json:"apps"`
	Databases     []drPlanDatabase `json:"databases"`
	Provider      string           `json:"provider"`
	LabOnly       bool             `json:"labOnly"`
	ConfirmPhrase drPhrases        `json:"confirmPhrase"`
}

func summarizePlan(file *planFile) drPlanSummary {
	summary := drPlanSummary{
		Name: file.Name, Valid: file.Valid(), Problems: file.Problems,
		Apps: []drPlanApp{}, Databases: []drPlanDatabase{},
	}
	plan := file.Plan
	if plan == nil {
		return summary
	}
	summary.Context, summary.DRContext, summary.LabOnly = plan.Context, plan.DRContext, plan.Safety.LabOnly
	if plan.Provider != nil {
		summary.Provider = plan.Provider.Kind
	}
	for _, app := range plan.Apps {
		summary.Apps = append(summary.Apps, drPlanApp{Name: app.Name, Namespace: app.Namespace, Tier: app.Tier})
	}
	for _, db := range plan.Databases {
		summary.Databases = append(summary.Databases, drPlanDatabase{Name: db.Name, Namespace: db.Namespace, Kind: db.Kind, Tier: db.Tier})
	}
	summary.ConfirmPhrase = drPhrases{
		Down: engine.ConfirmPhrase(plan, dr.DirDown, dr.Scope{}),
		Up:   engine.ConfirmPhrase(plan, dr.DirUp, dr.Scope{}),
	}
	return summary
}

func (s *Server) handleDRPlans(w http.ResponseWriter, _ *http.Request) {
	plans := []drPlanSummary{}
	if s.dr.dir == "" {
		writeJSON(w, http.StatusOK, plans)
		return
	}

	entries, err := os.ReadDir(s.dr.plansDir())
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusOK, plans)
		return
	}
	if err != nil {
		writeDRErr(w, err)
		return
	}

	names := []string{}
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".yaml")
		if ok && planName.MatchString(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) > maxPlanFiles {
		names = names[:maxPlanFiles]
	}

	for _, name := range names {
		raw, err := s.dr.readPlanFile(name)
		if err != nil {
			// A file that cannot be read is listed as invalid instead of hiding it.
			plans = append(plans, drPlanSummary{Name: name, Problems: []string{err.Error()}, Apps: []drPlanApp{}, Databases: []drPlanDatabase{}})
			continue
		}
		plans = append(plans, summarizePlan(s.inspectPlan(name, raw)))
	}
	writeJSON(w, http.StatusOK, plans)
}

// ---- one plan ----------------------------------------------------------------------------------------

type drPlanDetail struct {
	Name          string    `json:"name"`
	Valid         bool      `json:"valid"`
	YAML          string    `json:"yaml"`
	Plan          *dr.Plan  `json:"plan"`
	Problems      []string  `json:"problems"`
	ConfirmPhrase drPhrases `json:"confirmPhrase"`
}

func (s *Server) handleDRPlan(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	raw, err := s.dr.readPlanFile(name)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	file := s.inspectPlan(name, raw)
	summary := summarizePlan(file)
	writeJSON(w, http.StatusOK, drPlanDetail{
		Name: name, Valid: file.Valid(), YAML: string(raw), Plan: file.Plan,
		Problems: file.Problems, ConfirmPhrase: summary.ConfirmPhrase,
	})
}

// ---- save --------------------------------------------------------------------------------------------

type drSavePlanRequest struct {
	Name      string `json:"name"`
	YAML      string `json:"yaml"`
	Overwrite bool   `json:"overwrite"`
}

type drSavePlanResponse struct {
	Name     string   `json:"name"`
	Valid    bool     `json:"valid"`
	Problems []string `json:"problems"`
	Created  bool     `json:"created"`
}

// handleDRSavePlan stores a plan. It is configuration of the DR module, not a Kubernetes manifest: the
// text is parsed strictly as a plan and refused unless it validates, so nothing else can be written here.
func (s *Server) handleDRSavePlan(w http.ResponseWriter, r *http.Request) {
	var req drSavePlanRequest
	if err := decodeBodyLimit(w, r, &req, 2*maxPlanFile); err != nil {
		writeErr(w, err)
		return
	}
	if s.opts.ReadOnly {
		writeError(w, http.StatusForbidden, "read_only", "the console is running in read-only mode")
		return
	}
	if !planName.MatchString(req.Name) {
		writeDRErr(w, drBadRequest("invalid_name", "plan names use lowercase letters, digits and dashes (at most 63 characters)"))
		return
	}
	if len(req.YAML) == 0 || len(req.YAML) > maxPlanFile {
		writeDRErr(w, drBadRequest("invalid_plan", fmt.Sprintf("the plan must be between 1 byte and %d KiB", maxPlanFile>>10)))
		return
	}
	if s.dr.dir == "" {
		writeDRErr(w, &drError{status: http.StatusServiceUnavailable, code: "dr_unavailable", msg: "no directory for DR data (set --dr-dir)"})
		return
	}

	// Parse and validate the plan itself; the kubeconfig check is left out on purpose, so a plan for a
	// cluster that is about to be added can be saved (the list marks it invalid until the context exists).
	if _, err := dr.ParsePlan([]byte(req.YAML)); err != nil {
		var planErr *dr.PlanError
		problems := []string{err.Error()}
		if errors.As(err, &planErr) {
			problems = planErr.Problems
		}
		writeDRErr(w, drBadRequest("invalid_plan", "the plan is not valid", problems...))
		return
	}

	created, err := s.dr.writePlanFile(req.Name, []byte(req.YAML), req.Overwrite)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	file := s.inspectPlan(req.Name, []byte(req.YAML))
	writeJSON(w, http.StatusOK, drSavePlanResponse{Name: req.Name, Valid: file.Valid(), Problems: file.Problems, Created: created})
}

// writePlanFile writes the plan atomically with private permissions. Without overwrite, an existing
// plan is never replaced, even by a concurrent save (the file is hard-linked into place).
func (d *drService) writePlanFile(name string, data []byte, overwrite bool) (created bool, err error) {
	dir := d.plansDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("cannot create the plans directory: %w", err)
	}
	final := filepath.Join(dir, name+".yaml")
	existing, statErr := os.Lstat(final)
	exists := statErr == nil
	if exists && !existing.Mode().IsRegular() {
		return false, drBadRequest("invalid_plan_file", fmt.Sprintf("%q exists and is not a regular file", name))
	}
	if exists && !overwrite {
		return false, &drError{status: http.StatusConflict, code: "already_exists", msg: fmt.Sprintf("a plan named %q already exists", name)}
	}

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return false, err
	}
	temp := filepath.Join(dir, ".tmp-"+hex.EncodeToString(suffix))
	// #nosec G304 -- temp lives in the plans directory with a random name.
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(temp) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}

	if overwrite {
		if err := os.Rename(temp, final); err != nil {
			return false, err
		}
		return !exists, nil
	}
	if err := os.Link(temp, final); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, &drError{status: http.StatusConflict, code: "already_exists", msg: fmt.Sprintf("a plan named %q already exists", name)}
		}
		return false, err
	}
	return true, nil
}
