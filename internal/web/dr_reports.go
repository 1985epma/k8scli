package web

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/1985epma/k8scli/internal/dr/measure"
)

// Drill reports are files the measure package writes under the DR directory. The browser only names a
// report by its id, which is validated here and again by the package.

type drReportSummary struct {
	ID       string             `json:"id"`
	Plan     string             `json:"plan"`
	Context  string             `json:"context"`
	Actor    string             `json:"actor,omitempty"`
	Mode     string             `json:"mode"`
	Outcome  measure.Outcome    `json:"outcome"`
	Verdict  measure.Verdict    `json:"verdict"`
	Started  time.Time          `json:"started"`
	Finished time.Time          `json:"finished"`
	RTO      measure.Objective  `json:"rto"`
	RPO      *measure.Objective `json:"rpo,omitempty"`
	Failures int                `json:"failures"`
	Notices  int                `json:"notices"`
}

func (s *Server) handleDRReports(w http.ResponseWriter, r *http.Request) {
	limit := drListLimit
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed := 0
		if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed < 1 || parsed > 500 {
			writeDRErr(w, drBadRequest("invalid_limit", "limit must be between 1 and 500"))
			return
		}
		limit = parsed
	}
	out := []drReportSummary{}
	if s.dr.dir == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	reports, err := measure.History(s.dr.reportsDir(), limit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeDRErr(w, err)
		return
	}
	for _, report := range reports {
		out = append(out, drReportSummary{
			ID: report.ID, Plan: report.Plan, Context: report.Context, Actor: report.Actor, Mode: report.Mode,
			Outcome: report.Outcome, Verdict: report.Verdict, Started: report.Started, Finished: report.Finished,
			RTO: report.RTO, RPO: report.RPO, Failures: len(report.Failures), Notices: len(report.Notices),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDRReport(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !reportID.MatchString(id) {
		writeDRErr(w, drBadRequest("invalid_id", "that is not a report id"))
		return
	}
	if s.dr.dir == "" {
		writeDRErr(w, &drError{status: http.StatusNotFound, code: "report_not_found", msg: "there is no such report"})
		return
	}
	report, err := measure.Load(s.dr.reportsDir(), id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeDRErr(w, &drError{status: http.StatusNotFound, code: "report_not_found", msg: "there is no such report"})
			return
		}
		writeDRErr(w, drBadRequest("invalid_report", err.Error()))
		return
	}

	switch r.URL.Query().Get("format") {
	case "", "json":
		writeJSON(w, http.StatusOK, report)
	case "md", "markdown":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+safeFileName("dr-report", report.ID)+`.md"`)
		w.WriteHeader(http.StatusOK)
		// #nosec G705 -- an attachment served as text/markdown with nosniff (set for every response), never rendered as HTML.
		_, _ = w.Write([]byte(report.Markdown()))
	default:
		writeDRErr(w, drBadRequest("invalid_format", `format must be "json" or "md"`))
	}
}
