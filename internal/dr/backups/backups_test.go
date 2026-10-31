package backups

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
)

var fixedNow = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func backup(name string, phase string, completed time.Time, included ...string) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": name, "namespace": "velero"},
		"spec":     map[string]any{},
		"status":   map[string]any{"phase": phase},
	}
	if !completed.IsZero() {
		obj["status"].(map[string]any)["completionTimestamp"] = completed.Format(time.RFC3339)
	}
	if len(included) > 0 {
		list := make([]any, len(included))
		for i, name := range included {
			list[i] = name
		}
		obj["spec"].(map[string]any)["includedNamespaces"] = list
	}
	return &unstructured.Unstructured{Object: obj}
}

func snapshot(name, namespace string, ready bool, created time.Time) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":     map[string]any{"source": map[string]any{"persistentVolumeClaimName": "data"}},
		"status":   map[string]any{"readyToUse": ready, "creationTime": created.Format(time.RFC3339)},
	}}
}

func schedule(name string, paused bool, last time.Time, included ...string) *unstructured.Unstructured {
	template := map[string]any{}
	if len(included) > 0 {
		list := make([]any, len(included))
		for i, n := range included {
			list[i] = n
		}
		template["includedNamespaces"] = list
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Schedule",
		"metadata": map[string]any{"name": name, "namespace": "velero"},
		"spec":     map[string]any{"schedule": "0 2 * * *", "paused": paused, "template": template},
		"status":   map[string]any{"phase": "Enabled", "lastBackup": last.Format(time.RFC3339)},
	}}
}

func location(name, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation",
		"metadata": map[string]any{"name": name, "namespace": "velero"},
		"spec":     map[string]any{"provider": "aws", "objectStorage": map[string]any{"bucket": "dr-bucket"}},
		"status":   map[string]any{"phase": phase, "lastValidationTime": fixedNow.Add(-time.Minute).Format(time.RFC3339)},
	}}
}

var listKinds = map[schema.GroupVersionResource]string{
	veleroBackups:   "BackupList",
	veleroSchedules: "ScheduleList",
	veleroLocations: "BackupStorageLocationList",
	volumeSnapshots: "VolumeSnapshotList",
}

func newEnv(objects ...runtime.Object) *dr.Env {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
	return &dr.Env{Dynamic: client, Now: func() time.Time { return fixedNow }}
}

// envWithoutCRDs answers every list the way an API server without the CRDs does.
func envWithoutCRDs() *dr.Env {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	client.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "velero.io", Resource: "backups"}, "")
	})
	return &dr.Env{Dynamic: client, Now: func() time.Time { return fixedNow }}
}

func hoursAgo(h int) time.Time { return fixedNow.Add(-time.Duration(h) * time.Hour) }

func TestNewestPicksTheNewestSuccessfulBackupThatCoversTheNamespace(t *testing.T) {
	env := newEnv(
		backup("old", "Completed", hoursAgo(30), "shop"),
		backup("all-namespaces", "Completed", hoursAgo(10)),            // empty selection covers everything
		backup("star", "Completed", hoursAgo(8), "*"),                  // explicit star
		backup("other-ns", "Completed", hoursAgo(1), "billing"),        // does not cover shop
		backup("failed", "Failed", hoursAgo(2), "shop"),                // does not count
		backup("partial", "PartiallyFailed", hoursAgo(3), "shop"),      // does not count
		backup("running", "InProgress", time.Time{}, "shop"),           // not finished
		backup("shop-new", "Completed", hoursAgo(5), "shop", "orders"), // newest that counts
	)

	got, found, err := Newest(context.Background(), env, "shop")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if !got.Equal(hoursAgo(5)) {
		t.Errorf("newest = %v; want %v", got, hoursAgo(5))
	}
}

func TestNewestHonoursExcludedNamespacesAndDeletion(t *testing.T) {
	excluded := backup("excluded", "Completed", hoursAgo(1))
	_ = unstructured.SetNestedStringSlice(excluded.Object, []string{"shop"}, "spec", "excludedNamespaces")
	deleting := backup("deleting", "Completed", hoursAgo(2), "shop")
	now := metav1.NewTime(fixedNow)
	deleting.SetDeletionTimestamp(&now)

	got, found, err := Newest(context.Background(), newEnv(excluded, deleting, backup("ok", "Completed", hoursAgo(9), "shop")), "shop")
	if err != nil || !found || !got.Equal(hoursAgo(9)) {
		t.Errorf("got %v found=%v err=%v; the excluded and the deleting backups must be ignored", got, found, err)
	}
}

func TestNewestUsesReadyVolumeSnapshotsOfTheNamespaceOnly(t *testing.T) {
	env := newEnv(
		snapshot("ready", "shop", true, hoursAgo(4)),
		snapshot("not-ready", "shop", false, hoursAgo(1)),
		snapshot("other-ns", "billing", true, hoursAgo(0)),
	)
	got, found, err := Newest(context.Background(), env, "shop")
	if err != nil || !found || !got.Equal(hoursAgo(4)) {
		t.Errorf("got %v found=%v err=%v", got, found, err)
	}
}

func TestNewestCombinesVeleroAndSnapshots(t *testing.T) {
	env := newEnv(backup("b", "Completed", hoursAgo(20), "shop"), snapshot("s", "shop", true, hoursAgo(2)))
	got, found, _ := Newest(context.Background(), env, "shop")
	if !found || !got.Equal(hoursAgo(2)) {
		t.Errorf("newest = %v; the snapshot is newer than the backup", got)
	}
}

func TestNewestNothingFound(t *testing.T) {
	if _, found, err := Newest(context.Background(), newEnv(), "shop"); found || err != nil {
		t.Errorf("empty cluster: found=%v err=%v", found, err)
	}
}

func TestMissingCRDsMeanNoneFoundNotAnError(t *testing.T) {
	_, found, err := Newest(context.Background(), envWithoutCRDs(), "shop")
	if found || err != nil {
		t.Errorf("found=%v err=%v; missing CRDs are not an error", found, err)
	}
	listing, err := List(context.Background(), envWithoutCRDs(), ListOptions{})
	if err != nil || listing.VeleroInstalled || listing.SnapshotsInstalled || len(listing.Items) != 0 || len(listing.Notices) != 0 {
		t.Errorf("listing = %+v err=%v", listing, err)
	}
}

func TestNilClientMeansNothingFound(t *testing.T) {
	if _, found, err := Newest(context.Background(), &dr.Env{}, "shop"); found || err != nil {
		t.Errorf("found=%v err=%v", found, err)
	}
	listing, err := List(context.Background(), &dr.Env{}, ListOptions{})
	if err != nil || len(listing.Notices) != 1 {
		t.Errorf("listing = %+v err=%v", listing, err)
	}
}

func TestPermissionErrorsAreReported(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	client.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "velero.io", Resource: "backups"}, "", errors.New("no access"))
	})
	env := &dr.Env{Dynamic: client, Now: func() time.Time { return fixedNow }}

	if _, _, err := Newest(context.Background(), env, "shop"); err == nil {
		t.Error("Newest must return a permission error: \"none found\" would be misleading")
	}
	listing, err := List(context.Background(), env, ListOptions{})
	if err != nil || len(listing.Notices) == 0 {
		t.Errorf("List must turn it into notices: %+v err=%v", listing, err)
	}
}

func TestListShowsEverythingNewestFirst(t *testing.T) {
	env := newEnv(
		backup("nightly-1", "Completed", hoursAgo(26), "shop"),
		backup("nightly-2", "Failed", hoursAgo(2), "shop"),
		backup("billing-only", "Completed", hoursAgo(1), "billing"),
		schedule("nightly", false, hoursAgo(3), "shop"),
		schedule("paused", true, hoursAgo(100), "shop"),
		location("default", "Available"),
		snapshot("data-snap", "shop", true, hoursAgo(5)),
	)

	listing, err := List(context.Background(), env, ListOptions{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if !listing.VeleroInstalled || !listing.SnapshotsInstalled {
		t.Errorf("installed flags: %+v", listing)
	}

	var names []string
	for _, item := range listing.Items {
		names = append(names, item.Source+"/"+item.Name)
	}
	want := []string{
		"velero-storage-location/default", // 1 minute ago
		"velero-backup/nightly-2",         // 2 hours
		"velero-schedule/nightly",         // 3 hours
		"volume-snapshot/data-snap",       // 5 hours
		"velero-backup/nightly-1",         // 26 hours
		"velero-schedule/paused",          // 100 hours
	}
	if len(names) != len(want) {
		t.Fatalf("items = %v; want %v (billing-only must be filtered out)", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("items = %v; want %v", names, want)
		}
	}

	byName := map[string]Item{}
	for _, item := range listing.Items {
		byName[item.Name] = item
	}
	if !byName["nightly-1"].OK || byName["nightly-2"].OK {
		t.Error("only Completed backups are OK")
	}
	if byName["paused"].OK || !byName["nightly"].OK {
		t.Error("a paused schedule is not OK")
	}
	if !byName["default"].OK || byName["default"].Detail != "aws bucket dr-bucket" {
		t.Errorf("location = %+v", byName["default"])
	}
	if byName["data-snap"].Age != 5*time.Hour {
		t.Errorf("age = %v", byName["data-snap"].Age)
	}
}

func TestListWithoutNamespaceKeepsAllBackups(t *testing.T) {
	env := newEnv(backup("a", "Completed", hoursAgo(1), "shop"), backup("b", "Completed", hoursAgo(2), "billing"))
	listing, err := List(context.Background(), env, ListOptions{})
	if err != nil || len(listing.Items) != 2 {
		t.Errorf("items = %d err=%v", len(listing.Items), err)
	}
}

func TestListToleratesOddObjects(t *testing.T) {
	// A Velero version that does not set phase or timestamps yet.
	bare := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": "bare", "namespace": "velero"},
	}}
	listing, err := List(context.Background(), newEnv(bare), ListOptions{})
	if err != nil || len(listing.Items) != 1 || listing.Items[0].Status != "New" || listing.Items[0].OK {
		t.Errorf("listing = %+v err=%v", listing, err)
	}
}

func TestCoversTable(t *testing.T) {
	for _, tc := range []struct {
		included, excluded []string
		namespace          string
		want               bool
	}{
		{nil, nil, "a", true},
		{[]string{"*"}, nil, "a", true},
		{[]string{"a"}, nil, "a", true},
		{[]string{"b"}, nil, "a", false},
		{[]string{"*"}, []string{"a"}, "a", false},
		{nil, []string{"a"}, "b", true},
	} {
		if got := covers(tc.included, tc.excluded, tc.namespace); got != tc.want {
			t.Errorf("covers(%v, %v, %q) = %v; want %v", tc.included, tc.excluded, tc.namespace, got, tc.want)
		}
	}
}
