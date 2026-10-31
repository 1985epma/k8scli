package web

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDRCapabilitiesReportTheFlags(t *testing.T) {
	for name, opts := range map[string]Options{
		"defaults":  {},
		"runs":      {AllowDRRuns: true},
		"power":     {AllowDRRuns: true, AllowDRPower: true},
		"read-only": {ReadOnly: true, AllowDRRuns: true},
	} {
		d := newDREnv(t, opts)
		caps := decode[drCapabilities](t, d.do(http.MethodGet, "/api/dr/capabilities", nil))
		if caps.ReadOnly != opts.ReadOnly || caps.AllowRuns != opts.AllowDRRuns || caps.AllowPower != opts.AllowDRPower {
			t.Errorf("%s: capabilities = %+v", name, caps)
		}
		if !strings.HasSuffix(caps.PlanDir, filepath.Join("dr", "plans")) && !strings.HasSuffix(caps.PlanDir, "plans") {
			t.Errorf("%s: planDir = %q", name, caps.PlanDir)
		}
	}
}

func TestDRPlansListValidityAndSummaries(t *testing.T) {
	d := newDREnv(t, Options{})
	d.writePlan(t, "good", planYAML("good", "dev", true, true))
	d.writePlan(t, "prod", planYAML("prod", "prod-eu", false, false))
	d.writePlan(t, "ghost", planYAML("ghost", "no-such-context", false, false))
	d.writePlan(t, "broken", "version: 1\nname: broken\ncontext: dev\nsurprise: true\n")
	// Files that are not plans by name are ignored.
	d.writePlan(t, "Bad_Name", planYAML("x", "dev", false, false))
	if err := os.WriteFile(filepath.Join(d.dir, "plans", "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	plans := decode[[]drPlanSummary](t, d.do(http.MethodGet, "/api/dr/plans", nil))
	byName := map[string]drPlanSummary{}
	for _, p := range plans {
		byName[p.Name] = p
	}
	if len(plans) != 4 {
		t.Fatalf("got %d plans: %+v", len(plans), plans)
	}

	good := byName["good"]
	if !good.Valid || len(good.Problems) != 0 || good.Context != "dev" || !good.LabOnly || good.Provider != "kind" {
		t.Errorf("good = %+v", good)
	}
	if len(good.Apps) != 2 || good.Apps[0].Name != "api" || len(good.Databases) != 1 || good.Databases[0].Kind != "statefulset" {
		t.Errorf("good apps/databases = %+v / %+v", good.Apps, good.Databases)
	}
	if good.ConfirmPhrase.Down != "stop dev" || good.ConfirmPhrase.Up != "" {
		t.Errorf("good phrases = %+v", good.ConfirmPhrase)
	}

	if p := byName["prod"]; !p.Valid || p.ConfirmPhrase.Down != "prod-eu" || p.ConfirmPhrase.Up != "prod-eu" {
		t.Errorf("a production plan needs the context name: %+v", p)
	}
	if p := byName["ghost"]; p.Valid || len(p.Problems) == 0 || !strings.Contains(p.Problems[0], "not in the kubeconfig") {
		t.Errorf("a plan whose context is missing must be listed as invalid, not hidden: %+v", p)
	}
	if p := byName["broken"]; p.Valid || len(p.Problems) == 0 || p.Apps == nil || p.Databases == nil {
		t.Errorf("a plan that does not parse must be listed with empty lists: %+v", p)
	}
	if plans[0].Name > plans[1].Name {
		t.Errorf("plans are not sorted: %v", plans)
	}
}

func TestDRPlansListWithoutADirectoryIsEmptyNotAnError(t *testing.T) {
	d := newDREnv(t, Options{})
	rec := d.do(http.MethodGet, "/api/dr/plans", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestDRPlanDetail(t *testing.T) {
	d := newDREnv(t, Options{})
	yaml := planYAML("good", "dev", true, false)
	d.writePlan(t, "good", yaml)

	detail := decode[drPlanDetail](t, d.do(http.MethodGet, "/api/dr/plan?name=good", nil))
	if !detail.Valid || detail.YAML != yaml || detail.Plan == nil || detail.Plan.Name != "good" || len(detail.Problems) != 0 {
		t.Errorf("detail = %+v", detail)
	}

	d.writePlan(t, "ghost", planYAML("ghost", "nowhere", false, false))
	ghost := decode[drPlanDetail](t, d.do(http.MethodGet, "/api/dr/plan?name=ghost", nil))
	if ghost.Valid || ghost.Plan == nil || len(ghost.Problems) == 0 || ghost.YAML == "" {
		t.Errorf("an invalid plan must still be readable so it can be fixed: %+v", ghost)
	}

	if rec := d.do(http.MethodGet, "/api/dr/plan?name=missing", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing plan = %d", rec.Code)
	}
}

func TestDRPlanNamesCannotEscapeTheDirectory(t *testing.T) {
	d := newDREnv(t, Options{})
	secret := filepath.Join(d.dir, "secret.yaml")
	if err := os.WriteFile(secret, []byte(planYAML("secret", "dev", false, false)), 0o600); err != nil {
		t.Fatal(err)
	}
	d.writePlan(t, "ok", planYAML("ok", "dev", false, false))

	for _, name := range []string{"../secret", "ok/../../secret", "..\\secret", "/etc/passwd", ".hidden", "UPPER", "a b", "ok.yaml", strings.Repeat("a", 64), ""} {
		rec := d.do(http.MethodGet, "/api/dr/plan?name="+url.QueryEscape(name), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET plan %q = %d; want 400", name, rec.Code)
		}
		save := d.do(http.MethodPost, "/api/dr/plans", drSavePlanRequest{Name: name, YAML: planYAML("x", "dev", false, false)})
		if save.Code != http.StatusBadRequest {
			t.Errorf("POST plan %q = %d; want 400", name, save.Code)
		}
	}
}

func TestDRPlanSymlinkIsRefused(t *testing.T) {
	d := newDREnv(t, Options{})
	target := filepath.Join(d.dir, "elsewhere.yaml")
	if err := os.WriteFile(target, []byte(planYAML("elsewhere", "dev", false, false)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d.dir, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(d.dir, "plans", "link.yaml")); err != nil {
		t.Skip("symlinks are not available here")
	}
	if rec := d.do(http.MethodGet, "/api/dr/plan?name=link", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("a symlinked plan = %d; want 400", rec.Code)
	}
}

func TestDRSavePlan(t *testing.T) {
	d := newDREnv(t, Options{})
	yaml := planYAML("saved", "dev", true, false)

	rec := d.do(http.MethodPost, "/api/dr/plans", drSavePlanRequest{Name: "saved", YAML: yaml})
	saved := decode[drSavePlanResponse](t, rec)
	if rec.Code != http.StatusOK || !saved.Created || !saved.Valid || saved.Name != "saved" {
		t.Fatalf("status %d: %+v", rec.Code, saved)
	}
	file := filepath.Join(d.dir, "plans", "saved.yaml")
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("plan file: %v %v", info, err)
	}
	if dirInfo, _ := os.Stat(filepath.Dir(file)); dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("plans directory permissions = %v", dirInfo.Mode().Perm())
	}
	if data, _ := os.ReadFile(file); string(data) != yaml {
		t.Errorf("stored plan differs:\n%s", data)
	}

	// No overwrite without asking.
	if rec := d.do(http.MethodPost, "/api/dr/plans", drSavePlanRequest{Name: "saved", YAML: yaml}); rec.Code != http.StatusConflict {
		t.Errorf("existing plan = %d; want 409", rec.Code)
	}
	changed := strings.Replace(yaml, "rto: 30m", "rto: 45m", 1)
	rec = d.do(http.MethodPost, "/api/dr/plans", drSavePlanRequest{Name: "saved", YAML: changed, Overwrite: true})
	again := decode[drSavePlanResponse](t, rec)
	if rec.Code != http.StatusOK || again.Created {
		t.Errorf("overwrite: %d %+v", rec.Code, again)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), "45m") {
		t.Error("the plan was not replaced")
	}

	// No temporary files are left behind.
	entries, _ := os.ReadDir(filepath.Join(d.dir, "plans"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestDRSavePlanRefusesInvalidPlansWithTheirProblems(t *testing.T) {
	d := newDREnv(t, Options{})

	for name, yaml := range map[string]string{
		"unknown field": "version: 1\nname: x\ncontext: dev\nsurprise: true\n",
		"not yaml":      "{{{",
		"no name":       "version: 1\ncontext: dev\n",
		"bad provider":  "version: 1\nname: x\ncontext: dev\nprovider:\n  kind: kind\n  params:\n    name: \"x; rm -rf /\"\n",
		"cycle":         "version: 1\nname: x\ncontext: dev\napps:\n  - {name: a, namespace: n, dependsOn: [b]}\n  - {name: b, namespace: n, dependsOn: [a]}\n",
	} {
		rec := d.do(http.MethodPost, "/api/dr/plans", drSavePlanRequest{Name: "bad", YAML: yaml})
		resp := decode[drErrorResponse](t, rec)
		if rec.Code != http.StatusBadRequest || resp.Code != "invalid_plan" || len(resp.Problems) == 0 {
			t.Errorf("%s: status %d: %+v", name, rec.Code, resp)
		}
	}
	if _, err := os.Stat(filepath.Join(d.dir, "plans", "bad.yaml")); err == nil {
		t.Error("an invalid plan was written")
	}
}

func TestDRSavePlanSizeLimits(t *testing.T) {
	d := newDREnv(t, Options{})
	big := planYAML("big", "dev", false, false) + "# " + strings.Repeat("x", maxPlanFile)
	rec := d.do(http.MethodPost, "/api/dr/plans", drSavePlanRequest{Name: "big", YAML: big})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a plan over the limit = %d; want 400", rec.Code)
	}
	huge := drSavePlanRequest{Name: "huge", YAML: strings.Repeat("x", 3*maxPlanFile)}
	if rec := d.do(http.MethodPost, "/api/dr/plans", huge); rec.Code != http.StatusBadRequest {
		t.Errorf("a body over the limit = %d; want 400", rec.Code)
	}
	if rec := d.do(http.MethodPost, "/api/dr/plans", drSavePlanRequest{Name: "empty"}); rec.Code != http.StatusBadRequest {
		t.Errorf("an empty plan = %d; want 400", rec.Code)
	}
}

func TestDRSavePlanIsBlockedInReadOnlyMode(t *testing.T) {
	d := newDREnv(t, Options{ReadOnly: true})
	rec := d.do(http.MethodPost, "/api/dr/plans", drSavePlanRequest{Name: "x", YAML: planYAML("x", "dev", false, false)})
	if rec.Code != http.StatusForbidden || decode[drErrorResponse](t, rec).Code != "read_only" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
	// Reading stays available.
	if rec := d.do(http.MethodGet, "/api/dr/plans", nil); rec.Code != http.StatusOK {
		t.Errorf("list in read-only mode = %d", rec.Code)
	}
}

func TestDRSavePlanRejectsUnknownFieldsInTheRequest(t *testing.T) {
	d := newDREnv(t, Options{})
	rec := d.do(http.MethodPost, "/api/dr/plans", map[string]any{"name": "x", "yaml": planYAML("x", "dev", false, false), "path": "/etc/cron.d/x"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a request with a path field = %d; want 400", rec.Code)
	}
}
