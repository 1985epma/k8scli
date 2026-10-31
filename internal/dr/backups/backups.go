// Package backups finds out what backups exist for a namespace. It is read-only and works through
// the dynamic client, so it does not need Velero or the CSI snapshot CRDs to be installed: a missing
// CRD simply means "nothing found".
//
// Newest has the signature the databases planner (dbops.BackupFinder) and the readiness checks
// receive by injection.
package backups

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/1985epma/k8scli/internal/dr"
)

// Sources of an Item.
const (
	SourceVeleroBackup    = "velero-backup"
	SourceVolumeSnapshot  = "volume-snapshot"
	SourceVeleroSchedule  = "velero-schedule"
	SourceStorageLocation = "velero-storage-location"
)

const (
	pageSize     = 500
	maxPerSource = 5000
)

var (
	veleroBackups   = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backups"}
	veleroSchedules = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "schedules"}
	veleroLocations = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backupstoragelocations"}
	volumeSnapshots = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
)

// Item is one backup, snapshot, schedule or storage location.
type Item struct {
	Source    string `json:"source"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Status is the phase as the tool reports it ("Completed", "Failed", "Available", "ready"...).
	Status string `json:"status"`
	// OK is true for a successful backup or snapshot, an enabled schedule, an available location.
	OK bool `json:"ok"`
	// Time is the completion time of a backup, the creation time of a snapshot, the last backup of a
	// schedule or the last validation of a location. Zero when unknown.
	Time time.Time     `json:"time,omitempty"`
	Age  time.Duration `json:"age,omitempty"`
	// Namespaces are the namespaces a backup or schedule covers; empty or ["*"] means all.
	Namespaces []string `json:"namespaces,omitempty"`
	Detail     string   `json:"detail,omitempty"`
}

// Listing is the answer of List.
type Listing struct {
	Items []Item `json:"items"`
	// Notices are partial failures (for example a permission error on one resource).
	Notices            []string `json:"notices,omitempty"`
	VeleroInstalled    bool     `json:"veleroInstalled"`
	SnapshotsInstalled bool     `json:"snapshotsInstalled"`
}

// ListOptions narrow List.
type ListOptions struct {
	// Namespace keeps only backups, schedules and snapshots that cover this namespace.
	Namespace string
}

func now(env *dr.Env) time.Time {
	if env != nil && env.Now != nil {
		return env.Now()
	}
	return time.Now()
}

// absent reports an error that means "this API does not exist on the cluster".
func absent(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
}

// listAll pages through a resource, stopping at maxPerSource objects.
func listAll(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, namespace string) (items []unstructured.Unstructured, truncated bool, err error) {
	var resource dynamic.ResourceInterface = client.Resource(gvr)
	if namespace != "" {
		resource = client.Resource(gvr).Namespace(namespace)
	}
	options := metav1.ListOptions{Limit: pageSize}
	for {
		list, err := resource.List(ctx, options)
		if err != nil {
			return nil, false, err
		}
		items = append(items, list.Items...)
		if len(items) >= maxPerSource {
			return items[:maxPerSource], list.GetContinue() != "" || len(items) > maxPerSource, nil
		}
		if list.GetContinue() == "" {
			return items, false, nil
		}
		options.Continue = list.GetContinue()
	}
}

func parseTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func stringField(obj map[string]any, fields ...string) string {
	value, _, _ := unstructured.NestedString(obj, fields...)
	return value
}

func stringSlice(obj map[string]any, fields ...string) []string {
	value, _, _ := unstructured.NestedStringSlice(obj, fields...)
	return value
}

// covers reports whether a Velero namespace selection includes the namespace.
func covers(included, excluded []string, namespace string) bool {
	for _, name := range excluded {
		if name == namespace {
			return false
		}
	}
	if len(included) == 0 {
		return true
	}
	for _, name := range included {
		if name == "*" || name == namespace {
			return true
		}
	}
	return false
}

func deleting(obj *unstructured.Unstructured) bool {
	return obj.GetDeletionTimestamp() != nil
}

// backupTime is when a Velero backup finished; it falls back to the start time.
func backupTime(obj *unstructured.Unstructured) time.Time {
	if t := parseTime(stringField(obj.Object, "status", "completionTimestamp")); !t.IsZero() {
		return t
	}
	return parseTime(stringField(obj.Object, "status", "startTimestamp"))
}

func snapshotTime(obj *unstructured.Unstructured) time.Time {
	if t := parseTime(stringField(obj.Object, "status", "creationTime")); !t.IsZero() {
		return t
	}
	return obj.GetCreationTimestamp().Time
}

func readyToUse(obj *unstructured.Unstructured) bool {
	ready, found, _ := unstructured.NestedBool(obj.Object, "status", "readyToUse")
	return found && ready
}

// Newest returns the completion time of the newest SUCCESSFUL backup that covers the namespace: a
// Velero Backup in phase Completed (PartiallyFailed and Failed do not count) or a CSI VolumeSnapshot in
// the namespace that is ready to use. Missing CRDs mean "none found", not an error; any other error
// (a permission problem, for instance) is returned, because "none found" would be misleading.
func Newest(ctx context.Context, env *dr.Env, namespace string) (completed time.Time, found bool, err error) {
	if env == nil || env.Dynamic == nil {
		return time.Time{}, false, nil
	}

	var newest time.Time
	record := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if !found || t.After(newest) {
			newest, found = t, true
		}
	}

	backups, _, err := listAll(ctx, env.Dynamic, veleroBackups, "")
	switch {
	case err == nil:
		for i := range backups {
			b := &backups[i]
			if deleting(b) || stringField(b.Object, "status", "phase") != "Completed" {
				continue
			}
			if covers(stringSlice(b.Object, "spec", "includedNamespaces"), stringSlice(b.Object, "spec", "excludedNamespaces"), namespace) {
				record(backupTime(b))
			}
		}
	case !absent(err):
		return time.Time{}, false, fmt.Errorf("failed to list Velero backups: %w", err)
	}

	snapshots, _, err := listAll(ctx, env.Dynamic, volumeSnapshots, namespace)
	switch {
	case err == nil:
		for i := range snapshots {
			s := &snapshots[i]
			if !deleting(s) && readyToUse(s) {
				record(snapshotTime(s))
			}
		}
	case !absent(err):
		return time.Time{}, false, fmt.Errorf("failed to list volume snapshots: %w", err)
	}

	return newest, found, nil
}

// List returns every backup, snapshot, schedule and storage location, newest first, for the dashboard.
// Errors on one source become notices so the others still show up.
func List(ctx context.Context, env *dr.Env, options ListOptions) (*Listing, error) {
	listing := &Listing{Items: []Item{}}
	if env == nil || env.Dynamic == nil {
		listing.Notices = append(listing.Notices, "The dynamic client is not available, so backups cannot be read.")
		return listing, nil
	}
	at := now(env)
	age := func(t time.Time) time.Duration {
		if t.IsZero() || t.After(at) {
			return 0
		}
		return at.Sub(t)
	}
	notice := func(what string, err error, truncated bool) {
		if err != nil {
			listing.Notices = append(listing.Notices, fmt.Sprintf("Could not read %s: %v", what, err))
		}
		if truncated {
			listing.Notices = append(listing.Notices, fmt.Sprintf("Only the first %d %s are shown.", maxPerSource, what))
		}
	}

	backups, truncated, err := listAll(ctx, env.Dynamic, veleroBackups, "")
	switch {
	case err == nil:
		listing.VeleroInstalled = true
		notice("Velero backups", nil, truncated)
		for i := range backups {
			b := &backups[i]
			included, excluded := stringSlice(b.Object, "spec", "includedNamespaces"), stringSlice(b.Object, "spec", "excludedNamespaces")
			if options.Namespace != "" && !covers(included, excluded, options.Namespace) {
				continue
			}
			phase := stringField(b.Object, "status", "phase")
			if phase == "" {
				phase = "New"
			}
			t := backupTime(b)
			listing.Items = append(listing.Items, Item{
				Source: SourceVeleroBackup, Name: b.GetName(), Namespace: b.GetNamespace(), Status: phase,
				OK: phase == "Completed", Time: t, Age: age(t), Namespaces: included,
				Detail: backupDetail(b),
			})
		}
	case !absent(err):
		notice("Velero backups", err, false)
	}

	schedules, truncated, err := listAll(ctx, env.Dynamic, veleroSchedules, "")
	switch {
	case err == nil:
		listing.VeleroInstalled = true
		notice("Velero schedules", nil, truncated)
		for i := range schedules {
			s := &schedules[i]
			included, excluded := stringSlice(s.Object, "spec", "template", "includedNamespaces"), stringSlice(s.Object, "spec", "template", "excludedNamespaces")
			if options.Namespace != "" && !covers(included, excluded, options.Namespace) {
				continue
			}
			paused, _, _ := unstructured.NestedBool(s.Object, "spec", "paused")
			phase := stringField(s.Object, "status", "phase")
			if phase == "" {
				phase = "New"
			}
			t := parseTime(stringField(s.Object, "status", "lastBackup"))
			detail := "cron " + stringField(s.Object, "spec", "schedule")
			if paused {
				detail += ", paused"
			}
			listing.Items = append(listing.Items, Item{
				Source: SourceVeleroSchedule, Name: s.GetName(), Namespace: s.GetNamespace(), Status: phase,
				OK: phase == "Enabled" && !paused, Time: t, Age: age(t), Namespaces: included, Detail: detail,
			})
		}
	case !absent(err):
		notice("Velero schedules", err, false)
	}

	locations, _, err := listAll(ctx, env.Dynamic, veleroLocations, "")
	switch {
	case err == nil:
		listing.VeleroInstalled = true
		for i := range locations {
			l := &locations[i]
			phase := stringField(l.Object, "status", "phase")
			if phase == "" {
				phase = "Unknown"
			}
			t := parseTime(stringField(l.Object, "status", "lastValidationTime"))
			detail := stringField(l.Object, "spec", "provider")
			if bucket := stringField(l.Object, "spec", "objectStorage", "bucket"); bucket != "" {
				detail += " bucket " + bucket
			}
			listing.Items = append(listing.Items, Item{
				Source: SourceStorageLocation, Name: l.GetName(), Namespace: l.GetNamespace(), Status: phase,
				OK: phase == "Available", Time: t, Age: age(t), Detail: detail,
			})
		}
	case !absent(err):
		notice("Velero storage locations", err, false)
	}

	snapshots, truncated, err := listAll(ctx, env.Dynamic, volumeSnapshots, options.Namespace)
	switch {
	case err == nil:
		listing.SnapshotsInstalled = true
		notice("volume snapshots", nil, truncated)
		for i := range snapshots {
			s := &snapshots[i]
			ready := readyToUse(s)
			status := "not ready"
			if ready {
				status = "ready"
			}
			t := snapshotTime(s)
			listing.Items = append(listing.Items, Item{
				Source: SourceVolumeSnapshot, Name: s.GetName(), Namespace: s.GetNamespace(), Status: status,
				OK: ready, Time: t, Age: age(t), Detail: stringField(s.Object, "spec", "source", "persistentVolumeClaimName"),
			})
		}
	case !absent(err):
		notice("volume snapshots", err, false)
	}

	sortItems(listing.Items)
	return listing, nil
}

func backupDetail(b *unstructured.Unstructured) string {
	detail := ""
	if errors, found, _ := unstructured.NestedInt64(b.Object, "status", "errors"); found && errors > 0 {
		detail = fmt.Sprintf("%d errors", errors)
	}
	if warnings, found, _ := unstructured.NestedInt64(b.Object, "status", "warnings"); found && warnings > 0 {
		if detail != "" {
			detail += ", "
		}
		detail += fmt.Sprintf("%d warnings", warnings)
	}
	if location := stringField(b.Object, "spec", "storageLocation"); location != "" {
		if detail != "" {
			detail += ", "
		}
		detail += "location " + location
	}
	return detail
}

// sortItems orders newest first; items without a time go last; ties are broken by source and name so
// the order is deterministic.
func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch {
		case a.Time.IsZero() != b.Time.IsZero():
			return !a.Time.IsZero()
		case !a.Time.Equal(b.Time):
			return a.Time.After(b.Time)
		case a.Source != b.Source:
			return a.Source < b.Source
		case a.Namespace != b.Namespace:
			return a.Namespace < b.Namespace
		default:
			return a.Name < b.Name
		}
	})
}
