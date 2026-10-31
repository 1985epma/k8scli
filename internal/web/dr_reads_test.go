package web

import (
	"archive/zip"
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr/backups"
	"github.com/1985epma/k8scli/internal/dr/measure"
)

func TestDRCheckPassesTheContextAndNamespace(t *testing.T) {
	d := newDREnv(t, Options{})

	if rec := d.do(http.MethodGet, "/api/dr/check?context=dev&namespace=team-a", nil); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	d.do(http.MethodGet, "/api/dr/check?context=prod-eu&namespace=_all", nil)
	d.do(http.MethodGet, "/api/dr/check", nil)

	calls := d.fake.checkCalls
	if len(calls) != 3 || strings.Join(calls[0], ",") != "dev,team-a" || strings.Join(calls[1], ",") != "prod-eu" || strings.Join(calls[2], ",") != "dev" {
		t.Errorf("check calls = %v (all namespaces and the default context mean no namespace filter)", calls)
	}
}

func TestDRReadsValidateInput(t *testing.T) {
	d := newDREnv(t, Options{})
	for target, want := range map[string]int{
		"/api/dr/check?context=nope":                http.StatusNotFound,
		"/api/dr/check?namespace=Bad_NS":            http.StatusBadRequest,
		"/api/dr/backups?context=nope":              http.StatusNotFound,
		"/api/dr/diff?context=dev":                  http.StatusBadRequest,
		"/api/dr/diff?context=dev&to=dev":           http.StatusBadRequest,
		"/api/dr/diff?context=dev&to=nope":          http.StatusNotFound,
		"/api/dr/export?context=dev":                http.StatusBadRequest,
		"/api/dr/export?context=dev&namespace=a_b":  http.StatusBadRequest,
		"/api/dr/export?context=nope&namespace=app": http.StatusNotFound,
	} {
		if rec := d.do(http.MethodGet, target, nil); rec.Code != want {
			t.Errorf("GET %s = %d; want %d (%s)", target, rec.Code, want, rec.Body.String())
		}
	}
}

func TestDRBackupsAndDiff(t *testing.T) {
	d := newDREnv(t, Options{})

	listing := decode[backups.Listing](t, d.do(http.MethodGet, "/api/dr/backups?context=dev&namespace=team-a", nil))
	if !listing.VeleroInstalled {
		t.Errorf("listing = %+v", listing)
	}
	if rec := d.do(http.MethodGet, "/api/dr/diff?context=dev&to=prod-eu&namespace=team-a", nil); rec.Code != http.StatusOK {
		t.Fatalf("diff = %d: %s", rec.Code, rec.Body.String())
	}
	if len(d.fake.diffCalls) != 1 || d.fake.diffCalls[0] != "dev>prod-eu:team-a" {
		t.Errorf("diff calls = %v", d.fake.diffCalls)
	}
}

func TestDRReadsAreAvailableInReadOnlyMode(t *testing.T) {
	d := newDREnv(t, Options{ReadOnly: true})
	for _, target := range []string{
		"/api/dr/check", "/api/dr/backups", "/api/dr/diff?to=prod-eu", "/api/dr/export?namespace=team-a",
		"/api/dr/runs", "/api/dr/reports", "/api/dr/plans", "/api/dr/capabilities",
	} {
		if rec := d.do(http.MethodGet, target, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s in read-only mode = %d: %s", target, rec.Code, rec.Body.String())
		}
	}
}

func TestDRBackendErrorsAreMappedWithTheirHints(t *testing.T) {
	d := newDREnv(t, Options{})
	d.fake.checkErr = errors.New(`dial tcp 10.0.0.1:443: connect: connection refused`)

	rec := d.do(http.MethodGet, "/api/dr/check", nil)
	resp := decode[drErrorResponse](t, rec)
	if rec.Code != http.StatusBadGateway || !strings.Contains(resp.Error, "connection refused") || resp.Hint == "" {
		t.Errorf("status %d: %+v", rec.Code, resp)
	}
}

func TestDRUnavailableBackendAnswers503(t *testing.T) {
	d := newDREnv(t, Options{})
	d.server.dr.newBackend = nil
	rec := d.do(http.MethodGet, "/api/dr/check", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDRExportStreamsAZipAndNeverTakesAnOptionForSecrets(t *testing.T) {
	d := newDREnv(t, Options{})
	d.server.now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }

	// A real zip, to prove the bytes are passed through untouched.
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	file, _ := archive.Create("INVENTORY.md")
	_, _ = file.Write([]byte("Secrets (names only): db-password\n"))
	_ = archive.Close()
	d.fake.exportBytes = buffer.String()

	// Asking for secrets changes nothing: the parameter does not exist.
	rec := d.do(http.MethodGet, "/api/dr/export?context=dev&namespace=team-a&includeSecrets=true", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("status %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if disposition := rec.Header().Get("Content-Disposition"); disposition != `attachment; filename="dev-team-a-20261031.zip"` {
		t.Errorf("Content-Disposition = %q", disposition)
	}
	reader, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil || len(reader.File) != 1 || reader.File[0].Name != "INVENTORY.md" {
		t.Errorf("zip: %v %v", reader, err)
	}
	if len(d.fake.exportCalls) != 1 || d.fake.exportCalls[0] != "dev/team-a" {
		t.Errorf("export calls = %v", d.fake.exportCalls)
	}
}

func TestDRExportFailureBeforeTheZipStartsIsJSON(t *testing.T) {
	d := newDREnv(t, Options{})
	d.fake.exportErr = errors.New("namespace not found")

	rec := d.do(http.MethodGet, "/api/dr/export?namespace=team-a", nil)
	if rec.Code == http.StatusOK || strings.Contains(rec.Header().Get("Content-Type"), "zip") || !strings.Contains(rec.Body.String(), "namespace not found") {
		t.Errorf("status %d type %q body %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"dev-team-a-20261031": "dev-team-a-20261031",
		"../../etc/passwd":    "etc-passwd",
		"a b\"c\r\nd":         "a-b-c-d",
		"":                    "export",
		"...":                 "export",
	} {
		if got := safeFileName(in); got != want {
			t.Errorf("safeFileName(%q) = %q; want %q", in, got, want)
		}
	}
	if got := safeFileName(strings.Repeat("a", 500)); len(got) != 120 {
		t.Errorf("length = %d", len(got))
	}
}

// ---- reports -----------------------------------------------------------------------------------------

func saveReport(t *testing.T, dir, id string, hour int) *measure.Report {
	t.Helper()
	report := &measure.Report{
		Version: 1, ID: id, Plan: "shop-lab", Context: "dev", Actor: "web", Mode: "apps-only",
		Outcome: measure.Outcome("recovered"), Verdict: measure.Verdict("pass"),
		Started: time.Date(2026, 10, 31, hour, 0, 0, 0, time.UTC), Finished: time.Date(2026, 10, 31, hour, 5, 0, 0, time.UTC),
		T0:     time.Date(2026, 10, 31, 12, 0, 1, 0, time.UTC),
		RTO:    measure.Objective{Seconds: 120, Human: "2m"},
		Stages: []measure.Stage{}, Apps: []measure.AppResult{}, Failures: []string{"x"},
	}
	if _, _, err := measure.Save(dir, report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestDRReportsListGetAndMarkdown(t *testing.T) {
	d := newDREnv(t, Options{})
	saveReport(t, d.dir, "20261031T120000Z-aaaaaaaa", 12)
	saveReport(t, d.dir, "20261031T130000Z-bbbbbbbb", 13)

	list := decode[[]drReportSummary](t, d.do(http.MethodGet, "/api/dr/reports?limit=1", nil))
	if len(list) != 1 || list[0].ID != "20261031T130000Z-bbbbbbbb" || list[0].Plan != "shop-lab" || list[0].Failures != 1 || list[0].RTO.Seconds != 120 {
		t.Errorf("list = %+v", list)
	}

	rec := d.do(http.MethodGet, "/api/dr/report?id=20261031T120000Z-aaaaaaaa", nil)
	got := decode[measure.Report](t, rec)
	if rec.Code != http.StatusOK || got.ID != "20261031T120000Z-aaaaaaaa" {
		t.Errorf("report: %d %+v", rec.Code, got)
	}

	md := d.do(http.MethodGet, "/api/dr/report?id=20261031T120000Z-aaaaaaaa&format=md", nil)
	if md.Code != http.StatusOK || !strings.HasPrefix(md.Header().Get("Content-Type"), "text/markdown") ||
		!strings.Contains(md.Header().Get("Content-Disposition"), "attachment") || !strings.Contains(md.Body.String(), "shop-lab") {
		t.Errorf("markdown: %d %v\n%s", md.Code, md.Header(), md.Body.String())
	}
	if rec := d.do(http.MethodGet, "/api/dr/report?id=20261031T120000Z-aaaaaaaa&format=pdf", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown format = %d", rec.Code)
	}
}

func TestDRReportIDsCannotEscape(t *testing.T) {
	d := newDREnv(t, Options{})
	if err := os.WriteFile(filepath.Join(d.dir, "secret.json"), []byte(`{"id":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../secret", "..", ".hidden", "a/b", "a%00b", "", "x y"} {
		rec := d.do(http.MethodGet, "/api/dr/report?id="+urlEscape(id), nil)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("report id %q = %d; want 400 or 404", id, rec.Code)
		}
	}
	if rec := d.do(http.MethodGet, "/api/dr/report?id=20261031T999999Z-cccccccc", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing report = %d; want 404", rec.Code)
	}
	if rec := d.do(http.MethodGet, "/api/dr/reports?limit=0", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d", rec.Code)
	}
	empty := decode[[]drReportSummary](t, d.do(http.MethodGet, "/api/dr/reports", nil))
	if empty == nil || len(empty) != 0 {
		t.Errorf("no reports must be [] not null: %v", empty)
	}
}

func urlEscape(s string) string { return url.QueryEscape(s) }
