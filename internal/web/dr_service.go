package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/backups"
	"github.com/1985epma/k8scli/internal/dr/checks"
	"github.com/1985epma/k8scli/internal/dr/engine"
	"github.com/1985epma/k8scli/internal/dr/measure"
)

// The disaster-recovery endpoints (ADR 0005). Reads are always available. Real runs, drills and resume
// need --allow-dr-runs, and reaching the power phase also needs a lab-only plan or --allow-dr-power.

const (
	drReadTimeout  = 3 * time.Minute
	drStartTimeout = 20 * time.Second
	maxPlanFile    = 256 << 10
	maxPlanFiles   = 200
	drTicketTTL    = 30 * time.Minute
	drMaxTickets   = 128
	drMaxRetained  = 50
	drMaxEvents    = 200
	drListLimit    = 100
)

// planName is the only shape of plan name the console accepts. The browser never sends or receives a
// file path: names map to <drDir>/plans/<name>.yaml.
var planName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// reportID matches the ids the measure package creates, loosely: the package validates again.
var reportID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,80}$`)

// errDRUnavailable is returned when the DR stack cannot be built (for example no home directory).
var errDRUnavailable = errors.New("the disaster-recovery module is not available in this build")

// drBackend is what the endpoints need from the cluster side. *wire.Stack satisfies it through
// newDRBackend; tests use a fake.
type drBackend interface {
	// Run builds the runbook for the plan and executes it (or dry-runs it).
	Run(ctx context.Context, plan *dr.Plan, dir dr.Direction, scope dr.Scope, opts dr.RunOptions) (*dr.RunResult, error)
	// Drill rehearses a recovery and measures it.
	Drill(ctx context.Context, plan *dr.Plan, opts measure.DrillOptions) (*measure.Report, error)
	Check(ctx context.Context, contextName string, namespaces []string) (*checks.Report, error)
	Backups(ctx context.Context, contextName, namespace string) (*backups.Listing, error)
	Diff(ctx context.Context, primary, standby string, namespaces []string) (*checks.DiffReport, error)
	// Export writes the sanitized namespace as a zip. Secrets are never included: there is no option for it.
	Export(ctx context.Context, contextName, namespace string, w ioWriter) (*checks.ExportResult, error)
}

// drLedger reads stored runs. *engine.Engine satisfies it.
type drLedger interface {
	Status(ctx context.Context, id string) (*dr.RunResult, bool, error)
	List(ctx context.Context, limit int) ([]dr.RunResult, error)
}

// ioWriter is io.Writer, named so the interface above reads on its own.
type ioWriter interface {
	Write(p []byte) (int, error)
}

// drService holds the state of the DR endpoints inside a Server.
type drService struct {
	s *Server

	mu      sync.Mutex
	dir     string
	baseCtx context.Context
	cancel  context.CancelFunc

	// newBackend and newLedger are replaced by tests.
	newBackend func() (drBackend, error)
	newLedger  func() (drLedger, error)

	backendOnce sync.Once
	backendVal  drBackend
	backendErr  error

	runs    map[string]*liveRun
	order   []string
	tickets map[string]time.Time
}

// newDRService creates the service for a Server with the options already validated.
func newDRService(s *Server) *drService {
	dir := s.opts.DRDir
	if dir == "" {
		if def, err := measure.DefaultDir(); err == nil {
			dir = def
		}
	}
	base, cancel := context.WithCancel(context.Background())
	d := &drService{
		s:       s,
		dir:     dir,
		baseCtx: base,
		cancel:  cancel,
		runs:    map[string]*liveRun{},
		tickets: map[string]time.Time{},
	}
	d.newBackend = func() (drBackend, error) { return buildDRBackend(s) }
	d.newLedger = func() (drLedger, error) {
		return engine.New(engine.Options{Dir: d.dir, ReadOnly: true})
	}
	return d
}

// setBase ties the runs to the lifetime of the server: when the server stops, runs are aborted and
// their ledgers stay consistent, so they can be resumed.
func (d *drService) setBase(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cancel()
	d.baseCtx, d.cancel = context.WithCancel(ctx)
}

// shutdown aborts every live run and waits for them to write their last ledger.
func (d *drService) shutdown(timeout time.Duration) {
	d.mu.Lock()
	d.cancel()
	var live []*liveRun
	for _, run := range d.runs {
		live = append(live, run)
	}
	d.mu.Unlock()

	deadline := time.After(timeout)
	for _, run := range live {
		select {
		case <-run.done:
		case <-deadline:
			return
		}
	}
}

func (d *drService) backend() (drBackend, error) {
	if d.newBackend == nil {
		return nil, &apiError{status: http.StatusServiceUnavailable, code: "dr_unavailable", msg: errDRUnavailable.Error()}
	}
	d.backendOnce.Do(func() { d.backendVal, d.backendErr = d.newBackend() })
	if d.backendErr != nil {
		return nil, &apiError{status: http.StatusServiceUnavailable, code: "dr_unavailable", msg: d.backendErr.Error()}
	}
	return d.backendVal, nil
}

func (d *drService) ledger() (drLedger, error) {
	if d.dir == "" || d.newLedger == nil {
		return nil, &apiError{status: http.StatusServiceUnavailable, code: "dr_unavailable", msg: "no directory for DR data (set --dr-dir)"}
	}
	ledger, err := d.newLedger()
	if err != nil {
		return nil, &apiError{status: http.StatusServiceUnavailable, code: "dr_unavailable", msg: err.Error()}
	}
	return ledger, nil
}

func (d *drService) plansDir() string   { return filepath.Join(d.dir, "plans") }
func (d *drService) reportsDir() string { return d.dir }
func (d *drService) sidecarDir() string { return filepath.Join(d.dir, "web-runs") }

// displayPlansDir is the plans directory for people: "~" instead of the home directory.
func (d *drService) displayPlansDir() string {
	dir := d.plansDir()
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(dir, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(dir, home)
	}
	return dir
}

// ---- capabilities ------------------------------------------------------------------------------------

type drCapabilities struct {
	ReadOnly   bool   `json:"readOnly"`
	AllowRuns  bool   `json:"allowRuns"`
	AllowPower bool   `json:"allowPower"`
	PlanDir    string `json:"planDir"`
}

func (s *Server) handleDRCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, drCapabilities{
		ReadOnly:   s.opts.ReadOnly,
		AllowRuns:  s.opts.AllowDRRuns,
		AllowPower: s.opts.AllowDRPower,
		PlanDir:    s.dr.displayPlansDir(),
	})
}

// ---- errors ------------------------------------------------------------------------------------------

// drErrorResponse extends errorResponse with what the DR screens need.
type drErrorResponse struct {
	Error    string   `json:"error"`
	Code     string   `json:"code"`
	Hint     string   `json:"hint,omitempty"`
	Phrase   string   `json:"phrase,omitempty"`
	Problems []string `json:"problems,omitempty"`
}

// drError is an error with the extras of drErrorResponse.
type drError struct {
	status   int
	code     string
	msg      string
	hint     string
	phrase   string
	problems []string
}

func (e *drError) Error() string { return e.msg }

// writeDRErr maps DR errors (engine sentinels included) to JSON; anything else goes to writeErr.
func writeDRErr(w http.ResponseWriter, err error) {
	var de *drError
	switch {
	case errors.As(err, &de):
		writeJSON(w, de.status, drErrorResponse{Error: de.msg, Code: de.code, Hint: de.hint, Phrase: de.phrase, Problems: de.problems})
	case errors.Is(err, dr.ErrConfirmationRequired):
		writeJSON(w, http.StatusConflict, drErrorResponse{Error: err.Error(), Code: "confirmation_required"})
	case errors.Is(err, dr.ErrReadOnly):
		writeJSON(w, http.StatusForbidden, drErrorResponse{Error: "the console is running in read-only mode", Code: "read_only"})
	case errors.Is(err, dr.ErrRunInProgress):
		writeJSON(w, http.StatusConflict, drErrorResponse{Error: err.Error(), Code: "run_in_progress"})
	case errors.Is(err, dr.ErrPreflight):
		writeJSON(w, http.StatusConflict, drErrorResponse{Error: err.Error(), Code: "preflight_failed"})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusGatewayTimeout, drErrorResponse{Error: err.Error(), Code: "timeout"})
	default:
		writeErr(w, err)
	}
}

func drBadRequest(code, msg string, problems ...string) *drError {
	return &drError{status: http.StatusBadRequest, code: code, msg: msg, problems: problems}
}
