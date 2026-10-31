package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer lets the test read the banner while serve is still writing it.
type safeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestServeRefusesNonLoopbackAddresses(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8484", ":8484", "192.168.1.10:8484"} {
		h := newHarness(t)
		err := h.run("serve", "--addr", addr)
		if err == nil || !strings.Contains(err.Error(), "--allow-remote") {
			t.Errorf("serve --addr %s: err = %v; want a refusal that mentions --allow-remote", addr, err)
		}
	}
}

func TestServeRejectsMalformedFlagsAndArguments(t *testing.T) {
	h := newHarness(t)
	if err := h.run("serve", "--addr", "not-an-address"); err == nil {
		t.Error("a malformed --addr must fail")
	}
	if err := h.run("serve", "extra"); err == nil {
		t.Error("serve takes no arguments")
	}
}

func TestServeDoesNotNeedAClusterAndHonoursReadOnly(t *testing.T) {
	out := &safeBuffer{}
	h := newHarness(t)
	h.app.Out = out
	// The first run on a new machine has no kubeconfig at all.
	h.app.Kubeconfig = filepath.Join(t.TempDir(), "missing")

	root := NewRootCmd(h.app)
	root.SetArgs([]string{"serve", "--addr", "127.0.0.1:0", "--read-only"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), "token=") {
		time.Sleep(10 * time.Millisecond)
	}
	banner := out.String()
	if !strings.Contains(banner, "read-only") || !strings.Contains(banner, "http://127.0.0.1:") {
		t.Errorf("banner = %q; want the read-only mode and a loopback link", banner)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v after the context was cancelled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop when the context was cancelled")
	}
}

func TestServeIsListedInHelp(t *testing.T) {
	h := newHarness(t)
	if err := h.run("help"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "serve") {
		t.Errorf("help does not list serve:\n%s", h.out.String())
	}
}
