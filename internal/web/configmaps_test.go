package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func configMap(namespace, name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: metav1.NewTime(fixedNow.Add(-2 * 24 * 60 * 60 * 1e9))},
		Data:       data,
	}
}

func storedData(t *testing.T, e *env, namespace, name string) map[string]string {
	t.Helper()
	cm, err := e.client.CoreV1().ConfigMaps(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return cm.Data
}

// dataBody builds a /api/configmaps/data request; value is passed as raw JSON
// so tests can send null, a number or nothing at all.
func dataBody(extra map[string]any, rawValue string) map[string]any {
	body := map[string]any{"namespace": "shop", "name": "app", "key": "LOG_LEVEL"}
	for k, v := range extra {
		body[k] = v
	}
	if rawValue != "" {
		body["value"] = json.RawMessage(rawValue)
	}
	return body
}

// ---- read ------------------------------------------------------------------

func TestConfigMapsList(t *testing.T) {
	managed := configMap("shop", "zeta", map[string]string{"a": "1"})
	managed.Labels = managedLabels("zeta")
	managed.BinaryData = map[string][]byte{"blob": []byte("12345")}
	e := newEnv(t, Options{}, managed,
		configMap("shop", "app", map[string]string{"LOG_LEVEL": "info", "MODE": "x"}),
		configMap("tools", "ci", nil))

	rows := decode[[]configMapRow](t, e.do(http.MethodGet, "/api/configmaps?namespace=shop", nil))
	if len(rows) != 2 || rows[0].Name != "app" || rows[1].Name != "zeta" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Keys != 2 || rows[0].Size != len("LOG_LEVEL")+len("info")+len("MODE")+len("x") || rows[0].Managed || rows[0].Age != "2d" {
		t.Errorf("app = %+v", rows[0])
	}
	if rows[1].Keys != 2 || !rows[1].Managed || rows[1].Size != 1+1+4+5 {
		t.Errorf("zeta = %+v (keys count binary data too)", rows[1])
	}

	all := decode[[]configMapRow](t, e.do(http.MethodGet, "/api/configmaps", nil))
	if len(all) != 3 || all[0].Namespace != "shop" || all[2].Namespace != "tools" {
		t.Errorf("all namespaces = %+v", all)
	}
	if body := e.do(http.MethodGet, "/api/configmaps?namespace=none", nil).Body.String(); strings.TrimSpace(body) != "[]" {
		t.Errorf("empty must be [], got %q", body)
	}
	if rec := e.do(http.MethodGet, "/api/configmaps?namespace=Bad_NS", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad namespace = %d", rec.Code)
	}
}

func TestConfigMapDetail(t *testing.T) {
	cm := configMap("shop", "app", map[string]string{"LOG_LEVEL": "info"})
	cm.BinaryData = map[string][]byte{"b2": {1}, "b1": {2}}
	cm.Labels = map[string]string{"team": "payments"}
	e := newEnv(t, Options{}, cm)

	d := decode[configMapDetail](t, e.do(http.MethodGet, "/api/configmap?namespace=shop&name=app", nil))
	if d.Name != "app" || d.Data["LOG_LEVEL"] != "info" || d.Labels["team"] != "payments" || d.Age != "2d" {
		t.Errorf("detail = %+v", d)
	}
	if strings.Join(d.BinaryKeys, ",") != "b1,b2" {
		t.Errorf("binary keys (names only, never the bytes) = %v", d.BinaryKeys)
	}

	empty := newEnv(t, Options{}, configMap("shop", "bare", nil)).do(http.MethodGet, "/api/configmap?namespace=shop&name=bare", nil).Body.String()
	for _, field := range []string{`"data":{}`, `"labels":{}`, `"binaryKeys":[]`} {
		if !strings.Contains(empty, field) {
			t.Errorf("%s missing in %s", field, empty)
		}
	}

	for target, want := range map[string]int{
		"/api/configmap?namespace=shop&name=missing":          http.StatusNotFound,
		"/api/configmap?namespace=shop":                       http.StatusBadRequest,
		"/api/configmap?name=app":                             http.StatusBadRequest,
		"/api/configmap?namespace=shop&name=..%2Fx":           http.StatusBadRequest,
		"/api/configmap?namespace=shop&name=app&context=nope": http.StatusNotFound,
	} {
		if rec := e.do(http.MethodGet, target, nil); rec.Code != want {
			t.Errorf("%s = %d; want %d", target, rec.Code, want)
		}
	}
}

// ---- create ----------------------------------------------------------------

func TestConfigMapCreate(t *testing.T) {
	e := newEnv(t, Options{})

	rec := e.do(http.MethodPost, "/api/configmaps", map[string]any{
		"namespace": "shop", "name": "app", "data": map[string]string{"B": "it's", "A": "1"},
	})
	got := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || got.Kind != "ConfigMap" || got.Action != "created" || got.Summary != "ConfigMap app created with 2 keys" {
		t.Fatalf("status %d: %+v", rec.Code, got)
	}
	if got.Changes[0] != `A: "1"` || got.Changes[1] != `B: "it's"` {
		t.Errorf("changes (sorted) = %v", got.Changes)
	}
	if got.Equivalent != `kubectl create configmap app -n shop --from-literal='A=1' --from-literal='B=it'\''s'` {
		t.Errorf("equivalent (shell quoted) = %q", got.Equivalent)
	}

	cm, err := e.client.CoreV1().ConfigMaps("shop").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil || cm.Data["B"] != "it's" || cm.Labels[managedByLabel] != managedByValue || cm.Labels["app"] != "app" {
		t.Errorf("stored = %+v, %v", cm, err)
	}
}

func TestConfigMapCreateDryRunReadOnlyAndProduction(t *testing.T) {
	body := map[string]any{"namespace": "shop", "name": "app", "data": map[string]string{"A": "1"}}
	exists := func(e *env) bool {
		_, err := e.client.CoreV1().ConfigMaps("shop").Get(context.Background(), "app", metav1.GetOptions{})
		return err == nil
	}

	e := newEnv(t, Options{})
	spy := spyOnDryRuns(e)
	body["dryRun"] = true
	rec := e.do(http.MethodPost, "/api/configmaps", body)
	got := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || !got.DryRun || got.Summary != "ConfigMap app would be created with 1 keys" || spy.creates != 1 || exists(e) {
		t.Errorf("dry run: status %d, %+v, creates=%d", rec.Code, got, spy.creates)
	}

	ro := newEnv(t, Options{ReadOnly: true})
	body["dryRun"] = false
	if rec := ro.do(http.MethodPost, "/api/configmaps", body); rec.Code != http.StatusForbidden || exists(ro) {
		t.Errorf("read-only create = %d", rec.Code)
	}
	body["dryRun"] = true
	roSpy := spyOnDryRuns(ro)
	if rec := ro.do(http.MethodPost, "/api/configmaps", body); rec.Code != http.StatusOK || roSpy.creates != 1 {
		t.Errorf("read-only dry run = %d", rec.Code)
	}

	prod := newEnv(t, Options{})
	body["dryRun"] = false
	body["context"] = "prod-eu"
	if rec := prod.do(http.MethodPost, "/api/configmaps", body); rec.Code != http.StatusConflict || exists(prod) {
		t.Errorf("production without confirm = %d", rec.Code)
	}
	body["confirm"] = "prod-eu"
	if rec := prod.do(http.MethodPost, "/api/configmaps", body); rec.Code != http.StatusOK || !exists(prod) {
		t.Errorf("production with confirm = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestConfigMapCreateAlreadyExists(t *testing.T) {
	e := newEnv(t, Options{}, configMap("shop", "app", nil))
	rec := e.do(http.MethodPost, "/api/configmaps", map[string]any{"namespace": "shop", "name": "app", "data": map[string]string{}})
	if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "already_exists" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestConfigMapCreateValidation(t *testing.T) {
	e := newEnv(t, Options{})
	tooMany := map[string]string{}
	for i := 0; i <= cmMaxKeys; i++ {
		tooMany[strings.Repeat("k", i+1)] = "v"
	}
	chunk := strings.Repeat("x", cmMaxValueBytes)

	for name, body := range map[string]any{
		"bad namespace": map[string]any{"namespace": "Bad", "name": "a", "data": map[string]string{}},
		"bad name":      map[string]any{"namespace": "shop", "name": "A_B", "data": map[string]string{}},
		"bad key":       map[string]any{"namespace": "shop", "name": "a", "data": map[string]string{"bad key": "v"}},
		"slash key":     map[string]any{"namespace": "shop", "name": "a", "data": map[string]string{"a/b": "v"}},
		"dotdot key":    map[string]any{"namespace": "shop", "name": "a", "data": map[string]string{"..": "v"}},
		"empty key":     map[string]any{"namespace": "shop", "name": "a", "data": map[string]string{"": "v"}},
		"too many keys": map[string]any{"namespace": "shop", "name": "a", "data": tooMany},
		"value too big": map[string]any{"namespace": "shop", "name": "a", "data": map[string]string{"a": chunk + "x"}},
		"total too big": map[string]any{"namespace": "shop", "name": "a", "data": map[string]string{"a": chunk, "b": chunk, "c": chunk, "d": chunk, "e": chunk[:100]}},
		"unknown field": map[string]any{"namespace": "shop", "name": "a", "data": map[string]string{}, "binaryData": 1},
	} {
		if rec := e.do(http.MethodPost, "/api/configmaps", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d; want 400", name, rec.Code)
		}
	}
}

// ---- change one key --------------------------------------------------------

func TestConfigMapDataChangesOneKeyAndKeepsTheRest(t *testing.T) {
	e := newEnv(t, Options{}, configMap("shop", "app", map[string]string{"LOG_LEVEL": "info", "MODE": "fast"}))

	rec := e.do(http.MethodPost, "/api/configmaps/data", dataBody(nil, `"debug"`))
	got := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || got.Action != "updated" || got.Summary != `Key "LOG_LEVEL" of ConfigMap app changed` {
		t.Fatalf("status %d: %+v", rec.Code, got)
	}
	if len(got.Changes) != 1 || got.Changes[0] != `LOG_LEVEL: "info" -> "debug"` {
		t.Errorf("changes = %v", got.Changes)
	}
	if got.Equivalent != `kubectl patch configmap app -n shop --type merge -p '{"data":{"LOG_LEVEL":"debug"}}'` {
		t.Errorf("equivalent = %q", got.Equivalent)
	}
	if data := storedData(t, e, "shop", "app"); data["LOG_LEVEL"] != "debug" || data["MODE"] != "fast" {
		t.Errorf("stored = %v (the other key must survive)", data)
	}
}

func TestConfigMapDataAddsAndRemovesKeys(t *testing.T) {
	e := newEnv(t, Options{}, configMap("shop", "app", map[string]string{"MODE": "fast"}))

	added := decode[changeResponse](t, e.do(http.MethodPost, "/api/configmaps/data", dataBody(nil, `"info"`)))
	if added.Action != "updated" || added.Summary != `Key "LOG_LEVEL" added to ConfigMap app` || added.Changes[0] != `LOG_LEVEL: (new) -> "info"` {
		t.Errorf("added = %+v", added)
	}
	if storedData(t, e, "shop", "app")["LOG_LEVEL"] != "info" {
		t.Error("key not added")
	}

	removed := decode[changeResponse](t, e.do(http.MethodPost, "/api/configmaps/data", dataBody(nil, `null`)))
	if removed.Action != "updated" || removed.Summary != `Key "LOG_LEVEL" removed from ConfigMap app` || removed.Changes[0] != `LOG_LEVEL: "info" -> (removed)` {
		t.Errorf("removed = %+v", removed)
	}
	if removed.Equivalent != `kubectl patch configmap app -n shop --type merge -p '{"data":{"LOG_LEVEL":null}}'` {
		t.Errorf("equivalent = %q", removed.Equivalent)
	}
	if data := storedData(t, e, "shop", "app"); len(data) != 1 || data["MODE"] != "fast" {
		t.Errorf("stored = %v", data)
	}
}

func TestConfigMapDataUnchanged(t *testing.T) {
	e := newEnv(t, Options{}, configMap("shop", "app", map[string]string{"LOG_LEVEL": "info"}))
	spy := spyOnDryRuns(e)

	same := decode[changeResponse](t, e.do(http.MethodPost, "/api/configmaps/data", dataBody(nil, `"info"`)))
	missing := decode[changeResponse](t, e.do(http.MethodPost, "/api/configmaps/data", dataBody(map[string]any{"key": "NOPE"}, `null`)))
	if same.Action != "unchanged" || missing.Action != "unchanged" || len(same.Changes) != 0 || same.Equivalent == "" {
		t.Errorf("same = %+v, missing = %+v", same, missing)
	}
	e.client.ClearActions()
	_ = e.do(http.MethodPost, "/api/configmaps/data", dataBody(nil, `"info"`))
	for _, action := range e.client.Actions() {
		if action.GetVerb() == "patch" {
			t.Error("an unchanged request must not patch anything")
		}
	}
	if spy.patches != 0 {
		t.Errorf("patches = %d", spy.patches)
	}
}

func TestConfigMapDataDryRunChangesNothing(t *testing.T) {
	e := newEnv(t, Options{}, configMap("shop", "app", map[string]string{"LOG_LEVEL": "info"}))
	spy := spyOnDryRuns(e)

	rec := e.do(http.MethodPost, "/api/configmaps/data", dataBody(map[string]any{"dryRun": true}, `"debug"`))
	got := decode[changeResponse](t, rec)
	if rec.Code != http.StatusOK || !got.DryRun || got.Action != "updated" || got.Summary != `Key "LOG_LEVEL" of ConfigMap app would change` {
		t.Fatalf("status %d: %+v", rec.Code, got)
	}
	if spy.patches != 1 || storedData(t, e, "shop", "app")["LOG_LEVEL"] != "info" {
		t.Errorf("patches=%d stored=%v", spy.patches, storedData(t, e, "shop", "app"))
	}

	// The dry run still validates.
	if rec := e.do(http.MethodPost, "/api/configmaps/data", dataBody(map[string]any{"dryRun": true, "key": "bad key"}, `"x"`)); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid key in a dry run = %d", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/api/configmaps/data", dataBody(map[string]any{"dryRun": true, "name": "missing"}, `"x"`)); rec.Code != http.StatusNotFound {
		t.Errorf("unknown ConfigMap in a dry run = %d", rec.Code)
	}
}

func TestConfigMapDataReadOnly(t *testing.T) {
	e := newEnv(t, Options{ReadOnly: true}, configMap("shop", "app", map[string]string{"LOG_LEVEL": "info"}))
	spy := spyOnDryRuns(e)

	rec := e.do(http.MethodPost, "/api/configmaps/data", dataBody(nil, `"debug"`))
	if rec.Code != http.StatusForbidden || decode[errorResponse](t, rec).Code != "read_only" || storedData(t, e, "shop", "app")["LOG_LEVEL"] != "info" {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodPost, "/api/configmaps/data", dataBody(map[string]any{"dryRun": true}, `"debug"`)); rec.Code != http.StatusOK || spy.patches != 1 {
		t.Errorf("a dry run must still work: %d", rec.Code)
	}
}

func TestConfigMapDataInProductionNeedsTheContextName(t *testing.T) {
	e := newEnv(t, Options{}, configMap("shop", "app", map[string]string{"LOG_LEVEL": "info"}))
	extra := map[string]any{"context": "prod-eu"}

	rec := e.do(http.MethodPost, "/api/configmaps/data", dataBody(extra, `"debug"`))
	if rec.Code != http.StatusConflict || decode[errorResponse](t, rec).Code != "confirmation_required" || storedData(t, e, "shop", "app")["LOG_LEVEL"] != "info" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	extra["confirm"] = "wrong"
	if rec := e.do(http.MethodPost, "/api/configmaps/data", dataBody(extra, `"debug"`)); rec.Code != http.StatusConflict {
		t.Errorf("wrong confirmation = %d", rec.Code)
	}
	extra["confirm"] = "prod-eu"
	if rec := e.do(http.MethodPost, "/api/configmaps/data", dataBody(extra, `"debug"`)); rec.Code != http.StatusOK || storedData(t, e, "shop", "app")["LOG_LEVEL"] != "debug" {
		t.Errorf("confirmed change failed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestConfigMapDataValidation(t *testing.T) {
	big := configMap("shop", "big", map[string]string{"a": strings.Repeat("x", cmMaxValueBytes), "b": strings.Repeat("x", cmMaxValueBytes), "c": strings.Repeat("x", cmMaxValueBytes), "d": strings.Repeat("x", cmMaxValueBytes-1000)})
	binary := configMap("shop", "bin", map[string]string{"t": "v"})
	binary.BinaryData = map[string][]byte{"blob": {1}}
	e := newEnv(t, Options{}, configMap("shop", "app", map[string]string{"LOG_LEVEL": "info"}), big, binary)

	for name, body := range map[string]map[string]any{
		"missing value is not a delete": dataBody(nil, ""),
		"number value":                  dataBody(nil, `5`),
		"object value":                  dataBody(nil, `{"a":1}`),
		"bad key":                       dataBody(map[string]any{"key": "bad key"}, `"x"`),
		"bad key on remove":             dataBody(map[string]any{"key": "a/b"}, `null`),
		"empty key":                     dataBody(map[string]any{"key": ""}, `"x"`),
		"bad namespace":                 dataBody(map[string]any{"namespace": "Bad"}, `"x"`),
		"bad name":                      dataBody(map[string]any{"name": "A_B"}, `"x"`),
		"value too big":                 dataBody(nil, `"`+strings.Repeat("x", cmMaxValueBytes+1)+`"`),
		"object would pass 1 MiB":       dataBody(map[string]any{"name": "big", "key": "e"}, `"`+strings.Repeat("x", 100000)+`"`),
		"binary key":                    dataBody(map[string]any{"name": "bin", "key": "blob"}, `"x"`),
		"unknown field":                 dataBody(map[string]any{"patch": "x"}, `"x"`),
	} {
		if rec := e.do(http.MethodPost, "/api/configmaps/data", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d; want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
	if storedData(t, e, "shop", "app")["LOG_LEVEL"] != "info" {
		t.Error("an invalid request changed something")
	}
	if rec := e.do(http.MethodPost, "/api/configmaps/data", dataBody(map[string]any{"name": "missing"}, `"x"`)); rec.Code != http.StatusNotFound {
		t.Errorf("unknown ConfigMap = %d; want 404", rec.Code)
	}
}

func TestConfigMapDataKeyLimit(t *testing.T) {
	data := map[string]string{}
	for i := 0; i < cmMaxKeys; i++ {
		data["k"+strings.Repeat("a", i)] = "v"
	}
	e := newEnv(t, Options{}, configMap("shop", "app", data))
	if rec := e.do(http.MethodPost, "/api/configmaps/data", dataBody(nil, `"x"`)); rec.Code != http.StatusBadRequest {
		t.Errorf("adding key %d = %d; want 400", cmMaxKeys+1, rec.Code)
	}
	// Changing an existing key at the limit is fine.
	if rec := e.do(http.MethodPost, "/api/configmaps/data", dataBody(map[string]any{"key": "k"}, `"changed"`)); rec.Code != http.StatusOK {
		t.Errorf("changing an existing key = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestConfigMapDataLongValuesAreSummarised(t *testing.T) {
	e := newEnv(t, Options{}, configMap("shop", "app", map[string]string{"LOG_LEVEL": "a\nb"}))
	long := strings.Repeat("é", 500)

	got := decode[changeResponse](t, e.do(http.MethodPost, "/api/configmaps/data", dataBody(nil, `"`+long+`"`)))
	if strings.Contains(got.Changes[0], "\n") || len([]rune(got.Changes[0])) > len("LOG_LEVEL: ")+cmPreviewRunes*2+20 || !strings.Contains(got.Changes[0], `"a\nb"`) {
		t.Errorf("change line = %q", got.Changes[0])
	}
	if got.Equivalent != "kubectl edit configmap app -n shop" {
		t.Errorf("a huge patch is replaced by kubectl edit, got %q", got.Equivalent)
	}
}

func TestConfigMapEndpointsNeverTouchSecrets(t *testing.T) {
	e := newEnv(t, Options{}, configMap("shop", "app", map[string]string{"LOG_LEVEL": "info"}),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "shop"}, Data: map[string][]byte{"token": []byte("hunter2")}})

	_ = e.do(http.MethodGet, "/api/configmaps", nil)
	_ = e.do(http.MethodGet, "/api/configmap?namespace=shop&name=app", nil)
	_ = e.do(http.MethodPost, "/api/configmaps", map[string]any{"namespace": "shop", "name": "new", "data": map[string]string{"a": "1"}})
	_ = e.do(http.MethodPost, "/api/configmaps/data", dataBody(nil, `"debug"`))
	_ = e.do(http.MethodGet, "/api/dashboard", nil)
	_ = e.do(http.MethodGet, "/api/namespaces", nil)

	for _, action := range e.client.Actions() {
		if action.GetResource().Resource == "secrets" {
			t.Errorf("touched a Secret: %s", action.GetVerb())
		}
	}
}

// ---- helpers ---------------------------------------------------------------

func TestParseConfigMapValue(t *testing.T) {
	value, remove, err := parseConfigMapValue(json.RawMessage(`"a\"b"`))
	if err != nil || remove || value != `a"b` {
		t.Errorf("string: %q %v %v", value, remove, err)
	}
	if _, remove, err := parseConfigMapValue(json.RawMessage(`null`)); err != nil || !remove {
		t.Errorf("null: %v %v", remove, err)
	}
	if _, _, err := parseConfigMapValue(nil); err == nil {
		t.Error("absent value must be refused")
	}
	if _, _, err := parseConfigMapValue(json.RawMessage(`true`)); err == nil {
		t.Error("a boolean must be refused")
	}
}

func TestCmShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":     `'plain'`,
		"it's":      `'it'\''s'`,
		"$(rm -rf)": `'$(rm -rf)'`,
		"a b;c":     `'a b;c'`,
		"":          `''`,
	} {
		if got := cmShellQuote(in); got != want {
			t.Errorf("cmShellQuote(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestCmPreview(t *testing.T) {
	if got := cmPreview("a\nb"); got != `"a\nb"` {
		t.Errorf("newlines: %q", got)
	}
	if got := cmPreview(strings.Repeat("x", 100)); got != `"`+strings.Repeat("x", cmPreviewRunes)+`…"` {
		t.Errorf("long: %q", got)
	}
}
