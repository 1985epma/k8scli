// Package provider powers machines, node groups or whole clusters on and off through the
// command-line tools of their platform (docker, kind, minikube, az, aws, gcloud).
//
// Nothing here talks to a cloud API directly and nothing goes through a shell: every command is
// one allow-listed program with an argument slice built from validated plan parameters.
package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// defaultTimeout bounds one command. Cloud operations (az aks stop, aws ... wait) are slow.
	defaultTimeout = 30 * time.Minute
	// maxOutput caps the standard output kept in memory.
	maxOutput = 1 << 20
	// maxStderr caps the error text taken from standard error.
	maxStderr = 8 << 10
)

// defaultAllowed is the only set of programs OSExec will start.
var defaultAllowed = map[string]bool{
	"docker": true, "kind": true, "minikube": true, "az": true, "aws": true, "gcloud": true, "kubectl": true,
}

// OSExec implements dr.Exec on top of os/exec, with a fixed allow-list of programs.
type OSExec struct {
	// Timeout bounds each command; zero means 30 minutes.
	Timeout time.Duration
	// MaxOutput caps the stdout returned; zero means 1 MiB. Extra output is dropped.
	MaxOutput int

	// allowed is replaced by tests to run harmless programs.
	allowed map[string]bool
	// env is the child environment; nil means the parent's, which is where cloud credentials live.
	// It is never logged and never copied into an error.
	env []string
}

// NewOSExec returns an executor with the default limits and allow-list.
func NewOSExec() *OSExec { return &OSExec{} }

// ErrNotAllowed is returned for programs outside the allow-list.
var ErrNotAllowed = errors.New("program is not allowed")

// Run starts name with args and returns its standard output. Even when the command fails the
// output captured so far is returned next to the error, because some tools (minikube status)
// report a state on stdout and exit non-zero.
func (e *OSExec) Run(ctx context.Context, name string, args ...string) (string, error) {
	if err := e.check(name, args); err != nil {
		return "", err
	}

	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	limit := e.MaxOutput
	if limit <= 0 {
		limit = maxOutput
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// #nosec G204 -- name is on a fixed allow-list, never contains a path separator, and the args are
	// passed as separate argv entries (no shell) after rejecting NUL and line breaks.
	command := exec.CommandContext(ctx, name, args...)
	if e.env != nil {
		command.Env = e.env
	} else {
		command.Env = os.Environ()
	}
	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: maxStderr}
	command.Stdout, command.Stderr = stdout, stderr
	// If the program ignores the kill, do not wait for its pipes forever.
	command.WaitDelay = 5 * time.Second

	err := command.Run()
	out := stdout.String()
	if err == nil {
		return out, nil
	}
	if ctx.Err() != nil {
		return out, fmt.Errorf("%s: %w", commandLine(name, args), ctx.Err())
	}
	return out, fmt.Errorf("%s: %w%s", commandLine(name, args), err, stderrSuffix(stderr.String()))
}

func (e *OSExec) check(name string, args []string) error {
	allowed := e.allowed
	if allowed == nil {
		allowed = defaultAllowed
	}
	if strings.ContainsAny(name, `/\`) || !allowed[name] {
		return fmt.Errorf("%w: %q", ErrNotAllowed, name)
	}
	for i, arg := range args {
		if strings.ContainsAny(arg, "\x00\r\n") {
			return fmt.Errorf("argument %d of %s contains a NUL or a line break", i+1, name)
		}
	}
	return nil
}

func stderrSuffix(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return ": " + text
}

// commandLine renders a command for messages and for dr.Action.Equivalent.
func commandLine(name string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, name)
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

// shellQuote quotes an argument for display so a person can paste the command.
func shellQuote(arg string) string {
	plain := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r)
	}
	if arg != "" && strings.IndexFunc(arg, func(r rune) bool { return !plain(r) }) < 0 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// limitedBuffer keeps the first limit bytes and silently drops the rest, so a chatty program can
// neither exhaust memory nor block on a full pipe.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }
