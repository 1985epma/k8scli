package web

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Read-only DR views: readiness check, backups, drift between two clusters and the sanitized export.
// They never change anything, so they are available in read-only mode and without --allow-dr-runs.

func (s *Server) drRead(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), drReadTimeout)
}

func (s *Server) handleDRCheck(w http.ResponseWriter, r *http.Request) {
	namespace, err := namespaceParam(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	contextName, err := s.resolveContext(r.URL.Query().Get("context"))
	if err != nil {
		writeErr(w, err)
		return
	}
	backend, err := s.dr.backend()
	if err != nil {
		writeDRErr(w, err)
		return
	}
	ctx, cancel := s.drRead(r)
	defer cancel()

	var namespaces []string
	if namespace != "" {
		namespaces = []string{namespace}
	}
	report, err := backend.Check(ctx, contextName, namespaces)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleDRBackups(w http.ResponseWriter, r *http.Request) {
	namespace, err := namespaceParam(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	contextName, err := s.resolveContext(r.URL.Query().Get("context"))
	if err != nil {
		writeErr(w, err)
		return
	}
	backend, err := s.dr.backend()
	if err != nil {
		writeDRErr(w, err)
		return
	}
	ctx, cancel := s.drRead(r)
	defer cancel()

	listing, err := backend.Backups(ctx, contextName, namespace)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

func (s *Server) handleDRDiff(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	namespace, err := namespaceParam(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	primary, err := s.resolveContext(query.Get("context"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if query.Get("to") == "" {
		writeDRErr(w, drBadRequest("missing_to", "the standby context is required (to=<context>)"))
		return
	}
	standby, err := s.resolveContext(query.Get("to"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if primary == standby {
		writeDRErr(w, drBadRequest("same_context", "choose a standby context that is different from the primary"))
		return
	}
	backend, err := s.dr.backend()
	if err != nil {
		writeDRErr(w, err)
		return
	}
	ctx, cancel := s.drRead(r)
	defer cancel()

	var namespaces []string
	if namespace != "" {
		namespaces = []string{namespace}
	}
	report, err := backend.Diff(ctx, primary, standby, namespaces)
	if err != nil {
		writeDRErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// exportWriter delays the response headers until the first byte is written, so a failure before the
// zip starts can still be reported as JSON instead of a broken download.
type exportWriter struct {
	w       http.ResponseWriter
	file    string
	started bool
}

func (e *exportWriter) Write(p []byte) (int, error) {
	if !e.started {
		e.started = true
		header := e.w.Header()
		header.Set("Content-Type", "application/zip")
		header.Set("Content-Disposition", `attachment; filename="`+e.file+`"`)
		e.w.WriteHeader(http.StatusOK)
	}
	return e.w.Write(p)
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeFileName(parts ...string) string {
	name := strings.Trim(unsafeFileChars.ReplaceAllString(strings.Join(parts, "-"), "-"), "-.")
	if name == "" {
		name = "export"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

// handleDRExport streams a sanitized YAML export of one namespace as a zip. Secrets are never
// included, only listed by name in INVENTORY.md; the backend has no option to include them.
func (s *Server) handleDRExport(w http.ResponseWriter, r *http.Request) {
	namespace, err := validNamespace(r.URL.Query().Get("namespace"))
	if err != nil {
		writeErr(w, err)
		return
	}
	contextName, err := s.resolveContext(r.URL.Query().Get("context"))
	if err != nil {
		writeErr(w, err)
		return
	}
	backend, err := s.dr.backend()
	if err != nil {
		writeDRErr(w, err)
		return
	}
	ctx, cancel := s.drRead(r)
	defer cancel()

	out := &exportWriter{w: w, file: safeFileName(contextName, namespace, s.now().UTC().Format("20060102")) + ".zip"}
	if _, err := backend.Export(ctx, contextName, namespace, out); err != nil {
		if out.started {
			// The download already began; there is no way to report the failure, so the zip is left
			// incomplete and the connection ends.
			return
		}
		writeDRErr(w, err)
		return
	}
	if !out.started {
		// An empty export still has to be a valid answer.
		writeDRErr(w, fmt.Errorf("the export produced no data"))
	}
}
