// Package checks holds the read-only analysis of the disaster-recovery module: a readiness score, a
// capacity-fit check against a standby cluster, dependency discovery, a sanitized namespace export,
// a diff between two clusters and RPO estimates from backups.
//
// Nothing in this package creates, updates or deletes anything in a cluster.
package checks

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/backups"
)

const (
	// pageSize is the list page size, like internal/web uses.
	pageSize = 500
	// maxObjects bounds how many objects of one kind are read from one list.
	maxObjects = 20000
)

// Severity ranks a finding.
type Severity string

// Severities.
const (
	SeverityHigh   Severity = "high"
	SeverityMedium Severity = "medium"
	SeverityLow    Severity = "low"
)

func (s Severity) rank() int {
	switch s {
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	default:
		return 1
	}
}

// ParseSeverity accepts "high", "medium" or "low"; the empty string means "low" (show everything).
func ParseSeverity(value string) (Severity, error) {
	switch value {
	case "":
		return SeverityLow, nil
	case string(SeverityHigh), string(SeverityMedium), string(SeverityLow):
		return Severity(value), nil
	default:
		return "", fmt.Errorf("unknown severity %q (use high, medium or low)", value)
	}
}

// BackupFinder is the signature of backups.Newest; it is injected so tests and callers can replace it.
type BackupFinder func(ctx context.Context, env *dr.Env, namespace string) (time.Time, bool, error)

// defaultFinder is backups.Newest.
func defaultFinder(f BackupFinder) BackupFinder {
	if f != nil {
		return f
	}
	return backups.Newest
}

var systemNamespaces = map[string]bool{"kube-system": true, "kube-public": true, "kube-node-lease": true}

// IsSystemNamespace reports the namespaces Kubernetes itself owns.
func IsSystemNamespace(name string) bool { return systemNamespaces[name] }

func now(env *dr.Env) time.Time {
	if env != nil && env.Now != nil {
		return env.Now()
	}
	return time.Now()
}

func requireClient(env *dr.Env) error {
	if env == nil || env.Client == nil {
		return errors.New("the Kubernetes client is not available")
	}
	return nil
}

// pageAll collects items through list pages until there are no more or limit items are read; the bool
// says the result was cut at limit.
func pageAll[T any](limit int, fetch func(options metav1.ListOptions) (items []T, next string, err error)) ([]T, bool, error) {
	var all []T
	options := metav1.ListOptions{Limit: pageSize}
	for {
		items, next, err := fetch(options)
		if err != nil {
			return nil, false, err
		}
		all = append(all, items...)
		if len(all) >= limit {
			return all[:limit], next != "" || len(all) > limit, nil
		}
		if next == "" {
			return all, false, nil
		}
		options.Continue = next
	}
}

// namespaceNames lists namespaces sorted by name, leaving out the system ones unless asked.
func namespaceNames(ctx context.Context, client kubernetes.Interface, includeSystem bool) ([]string, error) {
	list, _, err := pageAll(maxObjects, func(options metav1.ListOptions) ([]string, string, error) {
		page, err := client.CoreV1().Namespaces().List(ctx, options)
		if err != nil {
			return nil, "", err
		}
		names := make([]string, 0, len(page.Items))
		for i := range page.Items {
			names = append(names, page.Items[i].Name)
		}
		return names, page.Continue, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}
	names := list[:0]
	for _, name := range list {
		if includeSystem || !IsSystemNamespace(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func int32Value(p *int32, fallback int32) int32 {
	if p == nil {
		return fallback
	}
	return *p
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < 2*time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
