package checks

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

// RPOStatus compares the age of the newest backup with the target.
type RPOStatus string

// RPO statuses.
const (
	RPOOK       RPOStatus = "ok"        // newest backup is younger than the target
	RPOBreach   RPOStatus = "breach"    // older than the target: a disaster now loses more than allowed
	RPOUnknown  RPOStatus = "unknown"   // no target, or the backups could not be read
	RPONoBackup RPOStatus = "no-backup" // nothing covers the namespace
)

// RPOEstimate is the worst-case data loss implied by the newest backup of an app or database.
type RPOEstimate struct {
	// Kind is "app" or "database".
	Kind       string        `json:"kind"`
	Name       string        `json:"name"`
	Namespace  string        `json:"namespace"`
	Found      bool          `json:"found"`
	LastBackup time.Time     `json:"lastBackup,omitempty"`
	Age        time.Duration `json:"age,omitempty"`
	Target     time.Duration `json:"target,omitempty"`
	Status     RPOStatus     `json:"status"`
	Note       string        `json:"note,omitempty"`
}

// EstimateBackupRPO estimates the RPO of each app and database of a plan from the newest backup that
// covers its namespace (Velero backups and CSI snapshots). The age of that backup is the data you would
// lose if the disaster happened now. It is an estimate from signals, not a measurement: only a drill with a
// heartbeat measures the real RPO.
func EstimateBackupRPO(ctx context.Context, env *dr.Env, plan *dr.Plan) ([]RPOEstimate, error) {
	return EstimateBackupRPOWith(ctx, env, plan, nil)
}

// EstimateBackupRPOWith is EstimateBackupRPO with an injected finder (default backups.Newest).
func EstimateBackupRPOWith(ctx context.Context, env *dr.Env, plan *dr.Plan, finder BackupFinder) ([]RPOEstimate, error) {
	if env == nil {
		return nil, errors.New("the environment is required")
	}
	if plan == nil {
		return nil, errors.New("the plan is required")
	}
	unavailable := finder == nil && env.Dynamic == nil
	find := defaultFinder(finder)
	at := now(env)

	type found struct {
		when time.Time
		ok   bool
		err  error
	}
	cache := map[string]found{}
	lookup := func(namespace string) found {
		if f, ok := cache[namespace]; ok {
			return f
		}
		when, ok, err := find(ctx, env, namespace)
		cache[namespace] = found{when, ok, err}
		return cache[namespace]
	}

	estimate := func(kind, name, namespace, own string) RPOEstimate {
		e := RPOEstimate{Kind: kind, Name: name, Namespace: namespace, Status: RPOUnknown}
		target, note := firstDuration(own, plan.Targets.RPO)
		e.Target, e.Note = target, note
		if unavailable {
			e.Note = joinNotes(e.Note, "The dynamic client is not available, so backups cannot be read.")
			return e
		}
		f := lookup(namespace)
		switch {
		case f.err != nil:
			e.Note = joinNotes(e.Note, "Could not read backups: "+f.err.Error())
		case !f.ok:
			e.Status = RPONoBackup
			e.Note = joinNotes(e.Note, "No Velero backup or ready volume snapshot covers this namespace.")
		default:
			e.Found, e.LastBackup = true, f.when
			if at.After(f.when) {
				e.Age = at.Sub(f.when)
			}
			switch {
			case target <= 0:
				e.Note = joinNotes(e.Note, "No RPO target is set for this entry.")
			case e.Age <= target:
				e.Status = RPOOK
			default:
				e.Status = RPOBreach
				e.Note = joinNotes(e.Note, fmt.Sprintf("The newest backup is %s old; the target is %s.", humanAge(e.Age), humanAge(target)))
			}
		}
		return e
	}

	var out []RPOEstimate
	for _, app := range plan.Apps {
		out = append(out, estimate("app", app.Name, app.Namespace, app.RPO))
	}
	for _, db := range plan.Databases {
		out = append(out, estimate("database", db.Name, db.Namespace, db.RPO))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// firstDuration parses the first non-empty value; a value that does not parse is reported in the note and
// the next one is tried.
func firstDuration(values ...string) (time.Duration, string) {
	note := ""
	for _, v := range values {
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err == nil && d > 0 {
			return d, note
		}
		note = joinNotes(note, fmt.Sprintf("Ignoring the RPO target %q: it is not a positive duration.", v))
	}
	return 0, note
}

func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	return a + " " + b
}
