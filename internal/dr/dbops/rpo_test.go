package dbops

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

func archiving(status, reason string, since time.Time) func(*unstructured.Unstructured) {
	return func(o *unstructured.Unstructured) {
		list, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
		list = append(list, cond("ContinuousArchiving", status, reason, since))
		_ = unstructured.SetNestedSlice(o.Object, list, "status", "conditions")
	}
}

func estimateOne(t *testing.T, te *testEnv, p *Planner, db dr.Database) RPOEstimate {
	t.Helper()
	got, err := p.EstimateRPO(context.Background(), te.env, planWith(db))
	if err != nil || len(got) != 1 {
		t.Fatalf("EstimateRPO = %+v, %v", got, err)
	}
	return got[0]
}

func TestRPOWithHealthyArchivingIsBoundedByTheArchiveTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout func(*unstructured.Unstructured)
		want    time.Duration
	}{
		{"default", func(*unstructured.Unstructured) {}, 5 * time.Minute},
		{"seconds", withArchiveTimeout("30s"), 30 * time.Second},
		{"minutes", withArchiveTimeout("2min"), 2 * time.Minute},
		{"bare number is seconds", withArchiveTimeout("90"), 90 * time.Second},
		{"hours", withArchiveTimeout("1h"), time.Hour},
		{"garbage falls back", withArchiveTimeout("often"), 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", archiving("True", "ContinuousArchivingSuccess", epoch.Add(-72*time.Hour)),
				withLastBackup(epoch.Add(-3*time.Hour)), tc.timeout)})
			est := estimateOne(t, te, NewPlanner(Options{}), cnpgDB("pg", 0, ""))
			if est.Source != SourceCNPGWAL || !est.Known || est.Age != tc.want {
				t.Errorf("estimate = %+v; want source %s, known, age %s", est, SourceCNPGWAL, tc.want)
			}
			if !strings.Contains(est.Note, "archive_timeout") || !strings.Contains(est.Note, "not exposed") {
				t.Errorf("the note must say this is a bound, not a measured lag: %q", est.Note)
			}
		})
	}
}

func TestRPOWithHealthyArchivingButNoBaseBackupIsNotKnown(t *testing.T) {
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", archiving("True", "ContinuousArchivingSuccess", epoch))})
	est := estimateOne(t, te, NewPlanner(Options{}), cnpgDB("pg", 0, ""))
	if est.Known || est.Source != SourceCNPGWAL || !strings.Contains(est.Note, "needs a base backup") {
		t.Errorf("estimate = %+v", est)
	}

	// A plugin-made backup that only the finder can see makes the bound meaningful again.
	viaFinder := estimateOne(t, te, NewPlanner(Options{Backups: finderAt(epoch.Add(-time.Hour))}), cnpgDB("pg", 0, ""))
	if !viaFinder.Known || !strings.Contains(viaFinder.Note, "backup-finder") {
		t.Errorf("with a finder: %+v", viaFinder)
	}
}

func TestRPOWithBrokenArchivingGrowsFromTheMomentItBroke(t *testing.T) {
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", archiving("False", "ContinuousArchivingFailing", epoch.Add(-2*time.Hour)),
		withLastBackup(epoch.Add(-10*time.Hour)))})
	est := estimateOne(t, te, NewPlanner(Options{}), cnpgDB("pg", 0, ""))
	if est.Source != SourceCNPGWAL || !est.Known || est.Age != 2*time.Hour {
		t.Errorf("estimate = %+v", est)
	}
	if !strings.Contains(est.Note, "failing") || !strings.Contains(est.Note, "ContinuousArchivingFailing") {
		t.Errorf("note = %q", est.Note)
	}
}

func TestRPOWithoutArchivingFallsBackToTheNewestBackup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		objects    []runtime.Object
		finder     BackupFinder
		wantSource string
		wantAge    time.Duration
	}{
		{"status", []runtime.Object{cnpgCluster("shop", "pg", withLastBackup(epoch.Add(-6*time.Hour)))}, nil, SourceCNPGBackup, 6 * time.Hour},
		{"backup objects", []runtime.Object{cnpgCluster("shop", "pg"), backupObject("shop", "b1", "pg", "completed", epoch.Add(-4*time.Hour))}, nil, SourceCNPGBackup, 4 * time.Hour},
		{"finder", []runtime.Object{cnpgCluster("shop", "pg")}, finderAt(epoch.Add(-90 * time.Minute)), SourceFinder, 90 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t, tc.objects)
			est := estimateOne(t, te, NewPlanner(Options{Backups: tc.finder}), cnpgDB("pg", 0, ""))
			if est.Source != tc.wantSource || !est.Known || est.Age != tc.wantAge {
				t.Errorf("estimate = %+v; want %s / %s", est, tc.wantSource, tc.wantAge)
			}
			if !strings.Contains(est.Note, "no continuous WAL archiving") {
				t.Errorf("note = %q", est.Note)
			}
		})
	}
}

func TestRPOIsUnknownWhenTheClusterSaysNothing(t *testing.T) {
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg")})
	est := estimateOne(t, te, NewPlanner(Options{}), cnpgDB("pg", 0, ""))
	if est.Known || est.Source != SourceUnknown || est.Note == "" {
		t.Errorf("estimate = %+v", est)
	}
}

func TestRPODoesNotFailWhenADatabaseCannotBeRead(t *testing.T) {
	te := newTestEnv(t, nil)
	te.dyn.PrependReactor("*", "clusters", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, missingResourceError()
	})
	est := estimateOne(t, te, NewPlanner(Options{}), cnpgDB("pg", 0, ""))
	if est.Source != SourceUnknown || est.Known || !strings.Contains(est.Note, "not installed") {
		t.Errorf("operator missing: %+v", est)
	}

	absent := estimateOne(t, newTestEnv(t, nil), NewPlanner(Options{}), cnpgDB("pg", 0, ""))
	if absent.Source != SourceUnknown || !strings.Contains(absent.Note, "was not found") {
		t.Errorf("cluster missing: %+v", absent)
	}

	odd := estimateOne(t, newTestEnv(t, nil), NewPlanner(Options{}), dr.Database{Name: "x", Namespace: "shop", Kind: "mongo", Ref: "x"})
	if odd.Source != SourceUnknown || !strings.Contains(odd.Note, "unsupported") {
		t.Errorf("unsupported kind: %+v", odd)
	}
}

func TestRPOOfAStatefulSetComesOnlyFromTheFinder(t *testing.T) {
	te := newTestEnv(t, nil)
	db := stsDB("mysql", 0, "")

	known := estimateOne(t, te, NewPlanner(Options{Backups: finderAt(epoch.Add(-4 * time.Hour))}), db)
	if known.Source != SourceFinder || !known.Known || known.Age != 4*time.Hour {
		t.Errorf("with a finder: %+v", known)
	}
	if none := estimateOne(t, te, NewPlanner(Options{}), db); none.Known || !strings.Contains(none.Note, "cannot be inferred") {
		t.Errorf("without a finder: %+v", none)
	}
	if failed := estimateOne(t, te, NewPlanner(Options{Backups: finderError(errBoom)}), db); failed.Known || !strings.Contains(failed.Note, "boom") {
		t.Errorf("finder error: %+v", failed)
	}
}

func TestEstimateRPOKeepsThePlanOrderAndHasAPackageLevelForm(t *testing.T) {
	te := newTestEnv(t, []runtime.Object{cnpgCluster("shop", "pg", withLastBackup(epoch.Add(-time.Hour)))})
	plan := planWith(stsDB("zeta", 0, ""), cnpgDB("pg", 0, ""))

	got, err := EstimateRPO(context.Background(), te.env, plan)
	if err != nil || len(got) != 2 || got[0].Name != "zeta" || got[1].Name != "pg" {
		t.Fatalf("EstimateRPO = %+v, %v", got, err)
	}
	if got[0].Kind != KindStatefulSet || got[0].Namespace != "shop" || got[1].Kind != KindCNPG {
		t.Errorf("identity fields = %+v", got)
	}
	if _, err := EstimateRPO(context.Background(), nil, plan); err == nil {
		t.Error("a nil environment must be an error")
	}
}

func TestParsePostgresDuration(t *testing.T) {
	for text, want := range map[string]time.Duration{
		"5min": 5 * time.Minute, "5 min": 5 * time.Minute, "300": 300 * time.Second, "30s": 30 * time.Second,
		"1h": time.Hour, "2d": 48 * time.Hour, "500ms": 500 * time.Millisecond, "1.5min": 90 * time.Second, " 10S ": 10 * time.Second,
	} {
		if got, ok := parsePostgresDuration(text); !ok || got != want {
			t.Errorf("parsePostgresDuration(%q) = %v, %v; want %v", text, got, ok, want)
		}
	}
	for _, text := range []string{"", "abc", "5 fortnights", "min"} {
		if _, ok := parsePostgresDuration(text); ok {
			t.Errorf("parsePostgresDuration(%q) must not parse", text)
		}
	}
}

func TestHumanAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0s", 30 * time.Second: "30s", 5 * time.Minute: "5m", 3 * time.Hour: "3.0h", 72 * time.Hour: "3.0d",
	} {
		if got := humanAge(d); got != want {
			t.Errorf("humanAge(%v) = %q; want %q", d, got, want)
		}
	}
}
