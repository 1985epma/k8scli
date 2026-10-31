package web

import (
	"context"
	"io"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/backups"
	"github.com/1985epma/k8scli/internal/dr/checks"
	"github.com/1985epma/k8scli/internal/dr/measure"
	"github.com/1985epma/k8scli/internal/dr/wire"
)

// wireBackend is the real cluster side of the DR endpoints: the same stack the CLI uses, so the console
// and `k8scli dr` behave identically.
type wireBackend struct {
	stack *wire.Stack
}

// buildDRBackend creates the stack for this server's kubeconfig and DR directory. Real runs from the
// console always carry the actor "web" in the audit log (set per run by the handlers).
func buildDRBackend(s *Server) (drBackend, error) {
	stack, err := wire.New(wire.Options{
		Kubeconfig: s.opts.Kubeconfig,
		Dir:        s.dr.dir,
		ReadOnly:   s.opts.ReadOnly,
		Actor:      webActor,
	})
	if err != nil {
		return nil, err
	}
	return &wireBackend{stack: stack}, nil
}

func (b *wireBackend) Run(ctx context.Context, plan *dr.Plan, dir dr.Direction, scope dr.Scope, opts dr.RunOptions) (*dr.RunResult, error) {
	return b.stack.Run(ctx, plan, dir, scope, opts)
}

func (b *wireBackend) Drill(ctx context.Context, plan *dr.Plan, opts measure.DrillOptions) (*measure.Report, error) {
	return b.stack.Drill(ctx, plan, opts)
}

func (b *wireBackend) Check(ctx context.Context, contextName string, namespaces []string) (*checks.Report, error) {
	return b.stack.Readiness(ctx, contextName, checks.ReadinessOptions{Namespaces: namespaces})
}

func (b *wireBackend) Backups(ctx context.Context, contextName, namespace string) (*backups.Listing, error) {
	return b.stack.Backups(ctx, contextName, backups.ListOptions{Namespace: namespace})
}

func (b *wireBackend) Diff(ctx context.Context, primary, standby string, namespaces []string) (*checks.DiffReport, error) {
	return b.stack.Diff(ctx, primary, standby, checks.DiffOptions{Namespaces: namespaces})
}

// Export never includes Secrets: ExportOptions is left at its zero value on purpose, and the console
// has no way to ask for them.
func (b *wireBackend) Export(ctx context.Context, contextName, namespace string, w ioWriter) (*checks.ExportResult, error) {
	return b.stack.ExportZip(ctx, contextName, namespace, writerFunc(w), checks.ExportOptions{})
}

// writerFunc adapts the small ioWriter interface to io.Writer.
func writerFunc(w ioWriter) io.Writer { return w }
