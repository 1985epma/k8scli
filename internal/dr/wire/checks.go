package wire

import (
	"context"
	"errors"
	"io"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/backups"
	"github.com/1985epma/k8scli/internal/dr/checks"
)

// Read-only analysis. Everything below only reads the clusters.

// Readiness scores how well the workloads of a context are set up to survive failures. The score
// counts risk signals; it does not prove that anything can be recovered.
func (s *Stack) Readiness(ctx context.Context, contextName string, opts checks.ReadinessOptions) (*checks.Report, error) {
	env, err := s.Env(ctx, contextName)
	if err != nil {
		return nil, err
	}
	return checks.Readiness(ctx, env, opts)
}

// Capacity checks whether the apps of the primary context fit on the standby context.
func (s *Stack) Capacity(ctx context.Context, primary, standby string, opts checks.CapacityOptions) (*checks.CapacityReport, error) {
	a, b, err := s.pair(ctx, primary, standby)
	if err != nil {
		return nil, err
	}
	return checks.CapacityFit(ctx, a, b, opts)
}

// Dependencies infers the dependency graph between workloads and suggests start tiers.
func (s *Stack) Dependencies(ctx context.Context, contextName string, opts checks.DiscoverOptions) (*checks.Graph, error) {
	env, err := s.Env(ctx, contextName)
	if err != nil {
		return nil, err
	}
	return checks.DiscoverDependencies(ctx, env, opts)
}

// ExportZip writes a sanitized export of a namespace to w as a zip stream. Secrets are not exported
// unless opts.IncludeSecrets is set (callers should not expose that option).
func (s *Stack) ExportZip(ctx context.Context, contextName, namespace string, w io.Writer, opts checks.ExportOptions) (*checks.ExportResult, error) {
	env, err := s.Env(ctx, contextName)
	if err != nil {
		return nil, err
	}
	return checks.ExportZip(ctx, env, namespace, w, opts)
}

// ExportDir writes the same export as files under dir.
func (s *Stack) ExportDir(ctx context.Context, contextName, namespace, dir string, opts checks.ExportOptions) (*checks.ExportResult, error) {
	env, err := s.Env(ctx, contextName)
	if err != nil {
		return nil, err
	}
	return checks.ExportDir(ctx, env, namespace, dir, opts)
}

// Diff compares two contexts: workloads, Services, ConfigMap key names, Ingress hosts and CRDs.
func (s *Stack) Diff(ctx context.Context, primary, standby string, opts checks.DiffOptions) (*checks.DiffReport, error) {
	a, b, err := s.pair(ctx, primary, standby)
	if err != nil {
		return nil, err
	}
	return checks.Diff(ctx, a, b, opts)
}

// Backups lists Velero backups, schedules, storage locations and CSI snapshots of a context.
func (s *Stack) Backups(ctx context.Context, contextName string, opts backups.ListOptions) (*backups.Listing, error) {
	env, err := s.Env(ctx, contextName)
	if err != nil {
		return nil, err
	}
	return backups.List(ctx, env, opts)
}

func (s *Stack) pair(ctx context.Context, primary, standby string) (*dr.Env, *dr.Env, error) {
	if primary == standby {
		return nil, nil, errors.New("the two contexts are the same")
	}
	a, err := s.Env(ctx, primary)
	if err != nil {
		return nil, nil, err
	}
	b, err := s.Env(ctx, standby)
	if err != nil {
		return nil, nil, err
	}
	return a, b, nil
}
