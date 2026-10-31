package provider

import (
	"context"
	"strings"
	"time"

	"github.com/1985epma/k8scli/internal/dr"
)

// command is one program call with separate arguments.
type command struct {
	program string
	args    []string
}

func (c command) line() string { return commandLine(c.program, c.args) }

// execAction is a dr.Action made of one or more commands, with an optional idempotency check and
// hooks around them.
type execAction struct {
	exec     dr.Exec
	id       string
	title    string
	describe string
	risk     dr.Risk
	commands []command
	// done reports whether the desired state already holds; nil means "never done".
	done func(ctx context.Context) (bool, error)
	// before runs ahead of the commands (for example, saving state); after runs once they succeeded.
	before func(ctx context.Context) error
	after  func(ctx context.Context) error
}

func (a *execAction) ID() string       { return a.id }
func (a *execAction) Title() string    { return a.title }
func (a *execAction) Describe() string { return a.describe }
func (a *execAction) Risk() dr.Risk    { return a.risk }

func (a *execAction) Done(ctx context.Context) (bool, error) {
	if a.done == nil {
		return false, nil
	}
	return a.done(ctx)
}

func (a *execAction) Apply(ctx context.Context) error {
	if a.before != nil {
		if err := a.before(ctx); err != nil {
			return err
		}
	}
	for _, c := range a.commands {
		if _, err := a.exec.Run(ctx, c.program, c.args...); err != nil {
			return err
		}
	}
	if a.after != nil {
		return a.after(ctx)
	}
	return nil
}

// Equivalent joins the commands the way a person would type them.
func (a *execAction) Equivalent() string {
	lines := make([]string, len(a.commands))
	for i, c := range a.commands {
		lines[i] = c.line()
	}
	return strings.Join(lines, " && ")
}

// ---- helpers that tolerate a partly filled dr.Env ---------------------------------------------------------

func nowFn(env *dr.Env) func() time.Time {
	if env != nil && env.Now != nil {
		return env.Now
	}
	return time.Now
}

func sleepFn(env *dr.Env) func(ctx context.Context, d time.Duration) error {
	if env != nil && env.Sleep != nil {
		return env.Sleep
	}
	return func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
}
