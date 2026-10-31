package dbops

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/1985epma/k8scli/internal/dr"
)

// defaultArchiveTimeout is the archive_timeout CloudNativePG configures when the cluster does not
// override it: PostgreSQL closes and archives the current WAL segment at least this often.
const defaultArchiveTimeout = 5 * time.Minute

// RPOEstimate is the estimated data-loss window of one database. It is an estimate read from what the
// cluster exposes, not a measurement: only a restore test (the measure package) proves the real RPO.
type RPOEstimate struct {
	Name      string
	Namespace string
	Kind      string
	// Source is SourceCNPGWAL, SourceCNPGBackup, SourceFinder or SourceUnknown.
	Source string
	// Age is the estimated window of data that would be lost. It is only meaningful when Known is true.
	Age time.Duration
	// Known is false when the cluster does not expose enough to estimate anything.
	Known bool
	// Note explains how the number was derived and what it does not cover.
	Note string
}

// EstimateRPO estimates the RPO of every database in the plan without database credentials, using no
// backup finder. Use Planner.EstimateRPO to include one.
func EstimateRPO(ctx context.Context, env *dr.Env, plan *dr.Plan) ([]RPOEstimate, error) {
	return NewPlanner(Options{}).EstimateRPO(ctx, env, plan)
}

// EstimateRPO estimates the RPO of every database in the plan. A database that cannot be read does not
// fail the call: it gets a SourceUnknown estimate whose Note says why.
func (p *Planner) EstimateRPO(ctx context.Context, env *dr.Env, plan *dr.Plan) ([]RPOEstimate, error) {
	if env == nil || plan == nil {
		return nil, fmt.Errorf("dbops: an environment and a plan are required")
	}

	estimates := make([]RPOEstimate, 0, len(plan.Databases))
	for _, db := range plan.Databases {
		base := RPOEstimate{Name: db.Name, Namespace: db.Namespace, Kind: db.Kind, Source: SourceUnknown}
		switch db.Kind {
		case KindCNPG:
			estimates = append(estimates, p.estimateCNPG(ctx, env, plan, db, base))
		case KindStatefulSet:
			estimates = append(estimates, p.estimateStatefulSet(ctx, env, db, base))
		default:
			base.Note = fmt.Sprintf("unsupported database kind %q", db.Kind)
			estimates = append(estimates, base)
		}
	}
	return estimates, nil
}

func (p *Planner) estimateStatefulSet(ctx context.Context, env *dr.Env, db dr.Database, est RPOEstimate) RPOEstimate {
	ref, found, err := p.newestBackup(ctx, env, db, nil)
	switch {
	case err != nil:
		est.Note = "could not look up backups: " + err.Error()
	case !found:
		est.Note = "no backup information is available for a plain StatefulSet: its backups cannot be inferred from the " +
			"cluster. Use a backup system that the backup finder understands (for example Velero), or a CloudNativePG cluster"
	default:
		est.Source = SourceFinder
		est.Age = nowOf(env).Sub(ref.When)
		est.Known = true
		est.Note = "age of the newest backup; data written after it is lost. Disk snapshots of a running database may be crash-consistent only"
	}
	return est
}

func (p *Planner) estimateCNPG(ctx context.Context, env *dr.Env, plan *dr.Plan, db dr.Database, est RPOEstimate) RPOEstimate {
	target := &cnpgTarget{p: p, env: env, plan: plan, db: db}
	cluster, err := target.get(ctx)
	if err != nil {
		est.Note = err.Error()
		return est
	}

	backup, haveBackup, backupErr := p.newestBackup(ctx, env, db, cluster)
	backupText := "no successful base backup is visible to k8scli (backups made by a plugin or another tool may not be)"
	if backupErr != nil {
		backupText = "base backups could not be looked up: " + backupErr.Error()
	} else if haveBackup {
		backupText = fmt.Sprintf("the newest base backup is %s old (%s)", humanAge(nowOf(env).Sub(backup.When)), backup.Source)
	}

	archiving, haveArchiving := conditionOf(cluster, "ContinuousArchiving")
	switch {
	case haveArchiving && archiving.Status == "True":
		est.Source = SourceCNPGWAL
		est.Age = archiveTimeout(cluster)
		// A bound, not a lag: the status exposes no "last archived WAL" time. Without a base backup
		// the archive cannot be replayed, so there is nothing to restore from.
		est.Known = haveBackup
		if haveBackup {
			est.Note = fmt.Sprintf("WAL archiving is healthy, so the loss is bounded by archive_timeout (%s) plus upload time; %s. "+
				"The exact lag is not exposed in the cluster status", est.Age, backupText)
		} else {
			est.Note = "WAL archiving is healthy, but " + backupText + ": a restore needs a base backup"
		}

	case haveArchiving:
		est.Source = SourceCNPGWAL
		est.Known = !archiving.LastTransition.IsZero()
		if est.Known {
			est.Age = nowOf(env).Sub(archiving.LastTransition)
		}
		est.Note = fmt.Sprintf("WAL archiving is failing (reason: %s): everything written since it broke is not in the archive; %s",
			orNone(archiving.Reason), backupText)

	case haveBackup:
		est.Source = backup.Source
		est.Age = nowOf(env).Sub(backup.When)
		est.Known = true
		est.Note = "recovery from backups only: the cluster reports no continuous WAL archiving, so data written after the last backup is lost"

	default:
		est.Note = "the cluster reports no WAL archiving and " + backupText
	}
	return est
}

// archiveTimeout reads spec.postgresql.parameters.archive_timeout, defaulting to what CloudNativePG sets.
func archiveTimeout(cluster *unstructured.Unstructured) time.Duration {
	text, _, _ := unstructured.NestedString(cluster.Object, "spec", "postgresql", "parameters", "archive_timeout")
	if d, ok := parsePostgresDuration(text); ok && d > 0 {
		return d
	}
	return defaultArchiveTimeout
}

// parsePostgresDuration reads a PostgreSQL time setting: a number with an optional unit
// (us, ms, s, min, h, d). A bare number is seconds, which is the unit of archive_timeout.
func parsePostgresDuration(text string) (time.Duration, bool) {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return 0, false
	}

	split := len(text)
	for i, r := range text {
		if (r < '0' || r > '9') && r != '.' {
			split = i
			break
		}
	}
	number, err := strconv.ParseFloat(text[:split], 64)
	if err != nil {
		return 0, false
	}

	unit := time.Second
	switch strings.TrimSpace(text[split:]) {
	case "", "s":
	case "us":
		unit = time.Microsecond
	case "ms":
		unit = time.Millisecond
	case "min":
		unit = time.Minute
	case "h":
		unit = time.Hour
	case "d":
		unit = 24 * time.Hour
	default:
		return 0, false
	}
	return time.Duration(number * float64(unit)), true
}
