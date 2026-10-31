package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

func TestAuditLogRecordsWhoDidWhat(t *testing.T) {
	e := newTestEngine(t)
	w := newWorld()
	scope := dr.Scope{Apps: []string{"web", "api"}}
	res, err := e.Run(context.Background(), testEnv("dev"), testPlan("dev"), dr.DirDown, scope,
		[]dr.Planner{w.planner(dr.PhaseApps, "a1", "a2")}, dr.RunOptions{Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	records, err := e.Audit(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, len(records))
	for i, rec := range records {
		kinds[i] = rec.Kind
		if rec.RunID != res.ID || rec.Actor != "alice" || rec.Plan != "demo" || rec.Context != "dev" || rec.Direction != dr.DirDown || rec.Seq != i+1 {
			t.Errorf("record %d = %+v", i, rec)
		}
		if rec.PlanHash == "" || rec.ScopeHash == "" || rec.Time.IsZero() {
			t.Errorf("record %d lacks hashes or time: %+v", i, rec)
		}
	}
	want := []string{"run-start", "step-start", "step-done", "step-start", "step-done", "run-done"}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("kinds = %v; want %v", kinds, want)
	}
	if records[1].Step != "a1" || records[len(records)-1].Msg != "succeeded" {
		t.Errorf("records = %+v", records)
	}

	// The scope hash does not depend on the order the lists were written in.
	if scopeHash(dr.Scope{Apps: []string{"b", "a"}}) != scopeHash(dr.Scope{Apps: []string{"a", "b"}}) {
		t.Error("scope hash depends on list order")
	}
	if scopeHash(dr.Scope{Apps: []string{"a"}}) == scopeHash(dr.Scope{Databases: []string{"a"}}) {
		t.Error("an app and a database named alike must hash differently")
	}

	if last, _ := e.Audit(context.Background(), 2); len(last) != 2 || last[1].Kind != "run-done" {
		t.Errorf("the last two records = %+v", last)
	}
}

func TestAuditAndLedgerFilesArePrivate(t *testing.T) {
	// A directory the engine creates itself; one handed in by the caller keeps the caller's mode.
	e := newTestEngine(t, func(o *Options) { o.Dir = filepath.Join(o.Dir, "dr") })
	w := newWorld()
	res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{w.planner(dr.PhaseApps, "a1")}, dr.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{
		{e.dir, 0o700},
		{filepath.Join(e.dir, "runs"), 0o700},
		{filepath.Join(e.dir, "locks"), 0o700},
		{e.auditPath(), 0o600},
		{e.ledgerPath(res.ID), 0o600},
	} {
		st, err := os.Stat(tc.path)
		if err != nil {
			t.Errorf("%s: %v", tc.path, err)
			continue
		}
		if permissionBitsMatter() && st.Mode().Perm() != tc.mode {
			t.Errorf("%s mode = %v; want %v", tc.path, st.Mode().Perm(), tc.mode)
		}
	}
	// The ledger of a succeeded run is valid JSON and no temp files are left behind.
	raw, _ := os.ReadFile(e.ledgerPath(res.ID))
	var back dr.RunResult
	if err := json.Unmarshal(raw, &back); err != nil || back.ID != res.ID || back.Status != dr.StatusSucceeded {
		t.Errorf("ledger = %s (%v)", raw, err)
	}
	entries, _ := os.ReadDir(filepath.Join(e.dir, "runs"))
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".tmp-") {
			t.Errorf("leftover temp file %s", en.Name())
		}
	}
}

func TestARunThatCannotBeAuditedDoesNotStart(t *testing.T) {
	e := newTestEngine(t)
	// A directory where the log should be makes every append fail.
	if err := os.MkdirAll(e.auditPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	w := newWorld()
	res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{w.planner(dr.PhaseApps, "a1")}, dr.RunOptions{})
	if err == nil || res != nil || !strings.Contains(err.Error(), "audit log") {
		t.Errorf("res %v err %v", res, err)
	}
	if w.log.count("apply:") != 0 || w.log.count("done:") != 0 {
		t.Error("the cluster was touched although the run could not be audited")
	}
	if _, err := os.Stat(e.lockPath("dev")); !os.IsNotExist(err) {
		t.Error("the lock leaked")
	}
}

func TestAuditToleratesDamagedLinesAndMissingFile(t *testing.T) {
	e := newTestEngine(t)
	if recs, err := e.Audit(context.Background(), 10); err != nil || len(recs) != 0 {
		t.Errorf("no log yet: %v %v", recs, err)
	}
	good, _ := json.Marshal(AuditRecord{Event: dr.Event{Kind: "run-start", RunID: "r"}})
	content := "garbage\n" + string(good) + "\n{\"kind\":\n\n" + string(good) + "\n"
	if err := os.WriteFile(e.auditPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := e.Audit(context.Background(), 10)
	if err != nil || len(recs) != 2 {
		t.Errorf("records %v err %v", recs, err)
	}
}

func TestAuditReadsOnlyTheNewestPartOfAHugeLog(t *testing.T) {
	e := newTestEngine(t)
	line, _ := json.Marshal(AuditRecord{Event: dr.Event{Kind: "step-done", RunID: "r", Msg: strings.Repeat("x", 500)}})
	var b strings.Builder
	for b.Len() < maxAuditRead+(1<<20) {
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(e.auditPath(), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := e.Audit(context.Background(), 5)
	if err != nil || len(recs) != 5 {
		t.Fatalf("records %d err %v", len(recs), err)
	}
}

// ---- ledger -----------------------------------------------------------------------------------------------------

func TestStatusAndListHandleDamagedAndHostileInput(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if err := os.MkdirAll(e.runsDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	w := newWorld()
	res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{w.planner(dr.PhaseApps, "a1")}, dr.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}

	corruptID := "20200101T000000Z-aaaaaaaa"
	mismatchID := "20200102T000000Z-bbbbbbbb"
	mustWrite := func(name, content string) {
		if err := os.WriteFile(filepath.Join(e.runsDir(), name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(corruptID+".json", "{not json")
	mustWrite(mismatchID+".json", `{"id":"20200101T000000Z-cccccccc","steps":[]}`)
	mustWrite("notes.txt", "not a ledger")
	mustWrite("20200103T000000Z-dddddddd.json.bak", "{}")
	if err := os.Mkdir(filepath.Join(e.runsDir(), "20200104T000000Z-eeeeeeee.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	list, err := e.List(ctx, 0)
	if err != nil || len(list) != 1 || list[0].ID != res.ID {
		t.Fatalf("list = %v, %v; the damaged files must be skipped", list, err)
	}

	if _, found, err := e.Status(ctx, corruptID); err == nil || found {
		t.Errorf("a corrupt ledger must be an error, got found=%v err=%v", found, err)
	}
	if _, found, err := e.Status(ctx, mismatchID); err == nil || found {
		t.Errorf("a ledger naming another run must be an error, got found=%v err=%v", found, err)
	}
	for _, id := range []string{"", "../../etc/passwd", "..", res.ID + "/../x", "20260101T000000Z-DEADBEEF", "not-an-id"} {
		if got, found, err := e.Status(ctx, id); got != nil || found || err != nil {
			t.Errorf("Status(%q) = %v %v %v; want not found", id, got, found, err)
		}
	}
}

func TestListIsNewestFirstAndLimited(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if empty, err := e.List(ctx, 10); err != nil || len(empty) != 0 || empty == nil {
		t.Errorf("empty list = %v %v", empty, err)
	}

	var ids []string
	for i := 0; i < 4; i++ {
		w := newWorld()
		res, err := e.run(t, testPlan("dev"), dr.DirDown, []dr.Planner{w.planner(dr.PhaseApps, "a1")}, dr.RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.ID)
		e.clock.advance(2 * time.Second)
	}
	list, err := e.List(ctx, 3)
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %d, %v", len(list), err)
	}
	for i, want := range []string{ids[3], ids[2], ids[1]} {
		if list[i].ID != want {
			t.Errorf("list[%d] = %s; want %s", i, list[i].ID, want)
		}
	}
	if all, _ := e.List(ctx, -1); len(all) != 4 {
		t.Errorf("a non-positive limit means the default, got %d", len(all))
	}
}

func TestRunIDsAreSortableAndUnique(t *testing.T) {
	c := newClock()
	seen := map[string]bool{}
	var prev string
	for i := 0; i < 50; i++ {
		id, err := newRunID(c.Now())
		if err != nil || !runIDPattern.MatchString(id) {
			t.Fatalf("id %q err %v", id, err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
	first, _ := newRunID(c.Now())
	c.advance(time.Second)
	prev = first
	next, _ := newRunID(c.Now())
	if next <= prev {
		t.Errorf("ids do not sort by time: %s then %s", prev, next)
	}
}
