package dbops

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/1985epma/k8scli/internal/dr"
)

// BackupFinder reports the completion time of the newest successful backup that protects the data
// in a namespace (Velero, a snapshot, a database operator). It is injected: dbops never imports the
// package that implements it, so the wiring decides which backup system is consulted.
type BackupFinder func(ctx context.Context, env *dr.Env, namespace string) (completed time.Time, found bool, err error)

// Sources of a backup time or of an RPO estimate.
const (
	SourceCNPGBackup = "cnpg-backup"
	SourceCNPGWAL    = "cnpg-wal"
	SourceFinder     = "backup-finder"
	SourceUnknown    = "unknown"
)

type backupRef struct {
	When   time.Time
	Source string
}

// backupLimit is the maximum accepted age of the newest backup: the database's own setting, else the
// plan-wide default. set is false when no limit applies.
func backupLimit(plan *dr.Plan, db dr.Database) (limit time.Duration, set bool, err error) {
	text := db.MaxBackupAge
	if text == "" && plan != nil {
		text = plan.Safety.RequireBackupMaxAge
	}
	if text == "" {
		return 0, false, nil
	}
	limit, err = time.ParseDuration(text)
	if err != nil || limit <= 0 {
		return 0, false, fmt.Errorf("database %q has an invalid maxBackupAge %q (use a positive duration such as 24h)", db.Name, text)
	}
	return limit, true, nil
}

// newestBackup looks for the newest successful backup of db. For CloudNativePG it reads the cluster
// status first, then the Backup objects, and finally asks the injected finder; for anything else
// only the finder can answer. cluster may be nil when it was not read.
func (p *Planner) newestBackup(ctx context.Context, env *dr.Env, db dr.Database, cluster *unstructured.Unstructured) (backupRef, bool, error) {
	if db.Kind == KindCNPG {
		if cluster != nil {
			if when, ok := cnpgBackupFromStatus(cluster); ok {
				return backupRef{When: when, Source: SourceCNPGBackup}, true, nil
			}
		}
		// A listing failure is not fatal: backups made by a plugin or another system may still be
		// visible to the finder below.
		if when, ok, err := cnpgBackupObjects(ctx, env, db); err == nil && ok {
			return backupRef{When: when, Source: SourceCNPGBackup}, true, nil
		}
	}

	if p.opts.Backups != nil {
		when, found, err := p.opts.Backups(ctx, env, db.Namespace)
		if err != nil {
			return backupRef{}, false, fmt.Errorf("looking up backups in namespace %s: %w", db.Namespace, err)
		}
		if found {
			return backupRef{When: when, Source: SourceFinder}, true, nil
		}
	}
	return backupRef{}, false, nil
}

// backupAgeCheck is the pre-flight shared by every kind of database. It returns a one-line summary
// when the stop may go ahead, and an error wrapping dr.ErrPreflight when it must not. It fails
// closed: if the backups cannot be inspected, the stop is blocked.
func (p *Planner) backupAgeCheck(ctx context.Context, env *dr.Env, plan *dr.Plan, db dr.Database, cluster *unstructured.Unstructured) (string, error) {
	limit, set, err := backupLimit(plan, db)
	if err != nil {
		return "", fmt.Errorf("%w: %w", dr.ErrPreflight, err)
	}
	if !set {
		return "no backup age limit is configured (maxBackupAge), so backups are not checked", nil
	}

	ref, found, err := p.newestBackup(ctx, env, db, cluster)
	if err != nil {
		return "", fmt.Errorf("%w: cannot verify the backups of %s/%s: %w", dr.ErrPreflight, db.Namespace, db.Ref, err)
	}
	if !found {
		return "", fmt.Errorf("%w: no successful backup was found for %s/%s, and the limit is %s. "+
			"Take a backup first, or raise maxBackupAge", dr.ErrPreflight, db.Namespace, db.Ref, limit)
	}

	age := nowOf(env).Sub(ref.When)
	if age > limit {
		return "", fmt.Errorf("%w: the newest backup of %s/%s is %s old (%s), older than the %s limit. "+
			"Take a fresh backup before stopping it", dr.ErrPreflight, db.Namespace, db.Ref, humanAge(age), ref.Source, limit)
	}
	return fmt.Sprintf("the newest backup is %s old (%s), within the %s limit", humanAge(age), ref.Source, limit), nil
}

// cnpgBackupFromStatus returns the newest backup time the Cluster status exposes. The status fields
// are deprecated and absent for backup plugins, which is why the Backup objects are tried next.
func cnpgBackupFromStatus(cluster *unstructured.Unstructured) (time.Time, bool) {
	var newest time.Time
	found := false
	consider := func(text string) {
		if when, ok := parseTime(text); ok && (!found || when.After(newest)) {
			newest, found = when, true
		}
	}

	consider(statusString(cluster, "status", "lastSuccessfulBackup"))
	if byMethod, ok, _ := unstructured.NestedMap(cluster.Object, "status", "lastSuccessfulBackupByMethod"); ok {
		for _, value := range byMethod {
			if text, isString := value.(string); isString {
				consider(text)
			}
		}
	}
	return newest, found
}

// cnpgBackupObjects scans the Backup objects that belong to the cluster for the newest completed one.
func cnpgBackupObjects(ctx context.Context, env *dr.Env, db dr.Database) (time.Time, bool, error) {
	if env.Dynamic == nil {
		return time.Time{}, false, fmt.Errorf("%w: no dynamic client", dr.ErrUnreachable)
	}
	list, err := env.Dynamic.Resource(cnpgBackupGVR).Namespace(db.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return time.Time{}, false, err
	}

	var newest time.Time
	found := false
	for i := range list.Items {
		item := &list.Items[i]
		if name, _, _ := unstructured.NestedString(item.Object, "spec", "cluster", "name"); name != db.Ref {
			continue
		}
		if statusString(item, "status", "phase") != "completed" {
			continue
		}
		if when, ok := parseTime(statusString(item, "status", "stoppedAt")); ok && (!found || when.After(newest)) {
			newest, found = when, true
		}
	}
	return newest, found, nil
}
