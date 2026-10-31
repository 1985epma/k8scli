package web

import "net/http"

// registerDRRoutes registers the disaster-recovery endpoints. Reads are always on; the POSTs that start
// real runs check --allow-dr-runs themselves.
func (s *Server) registerDRRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/dr/capabilities", s.handleDRCapabilities)
	mux.HandleFunc("GET /api/dr/plans", s.handleDRPlans)
	mux.HandleFunc("GET /api/dr/plan", s.handleDRPlan)
	mux.HandleFunc("POST /api/dr/plans", s.handleDRSavePlan)
	mux.HandleFunc("GET /api/dr/check", s.handleDRCheck)
	mux.HandleFunc("GET /api/dr/backups", s.handleDRBackups)
	mux.HandleFunc("GET /api/dr/diff", s.handleDRDiff)
	mux.HandleFunc("GET /api/dr/export", s.handleDRExport)
	mux.HandleFunc("GET /api/dr/runs", s.handleDRRuns)
	mux.HandleFunc("POST /api/dr/runs", s.handleDRStartRun)
	mux.HandleFunc("GET /api/dr/run", s.handleDRRun)
	mux.HandleFunc("POST /api/dr/runs/abort", s.handleDRAbort)
	mux.HandleFunc("POST /api/dr/runs/resume", s.handleDRResume)
	mux.HandleFunc("POST /api/dr/drill", s.handleDRDrill)
	mux.HandleFunc("GET /api/dr/reports", s.handleDRReports)
	mux.HandleFunc("GET /api/dr/report", s.handleDRReport)
}
