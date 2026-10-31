package cmd

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestLauncherArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"plain", []string{"k8scli", "pods"}, "k8scli pods"},
		{"launcher", []string{"/usr/local/bin/k8scli-ui"}, "/usr/local/bin/k8scli-ui ui"},
		{"launcher with flags", []string{"k8scli-ui", "--read-only"}, "k8scli-ui ui --read-only"},
		{"windows", []string{`C:\bin\K8SCLI-UI.EXE`, "--no-open"}, `C:\bin\K8SCLI-UI.EXE ui --no-open`},
		{"similar name", []string{"k8scli-ui-old", "pods"}, "k8scli-ui-old pods"},
	} {
		if got := strings.Join(LauncherArgs(tc.argv), " "); got != tc.want {
			t.Errorf("%s: LauncherArgs = %q; want %q", tc.name, got, tc.want)
		}
	}
	if got := LauncherArgs(nil); len(got) != 0 {
		t.Errorf("empty argv = %v", got)
	}
}

func TestResolveListenAddr(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	taken := busy.Addr().String()

	// A busy default falls back to another free port on the same host.
	got, err := resolveListenAddr(taken, false)
	if err != nil || got == taken || !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("busy default: %q, %v", got, err)
	}

	// A port the user typed is never silently replaced.
	if _, err := resolveListenAddr(taken, true); err == nil {
		t.Error("an explicit busy address must fail")
	}

	// Port 0 asks for any free port.
	if got, err := resolveListenAddr("127.0.0.1:0", true); err != nil || strings.HasSuffix(got, ":0") {
		t.Errorf("port 0: %q, %v", got, err)
	}

	if _, err := resolveListenAddr("not-an-address", false); err == nil {
		t.Error("a malformed address must fail")
	}
}

func TestUIOpensTheBrowserWithTheTokenLink(t *testing.T) {
	opened := make(chan string, 1)
	previous := openBrowser
	openBrowser = func(link string) error { opened <- link; return nil }
	defer func() { openBrowser = previous }()

	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		root := NewRootCmd(h.app)
		root.SetArgs([]string{"ui", "--addr", "127.0.0.1:0"})
		done <- root.ExecuteContext(ctx)
	}()

	select {
	case link := <-opened:
		if !strings.HasPrefix(link, "http://127.0.0.1:") || !strings.Contains(link, "token=") {
			t.Errorf("link = %q", link)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the browser was never opened")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ui returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ui did not stop")
	}
}

func TestUINoOpenDoesNotLaunchABrowser(t *testing.T) {
	previous := openBrowser
	openBrowser = func(string) error { t.Error("the browser must not be opened with --no-open"); return nil }
	defer func() { openBrowser = previous }()

	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		root := NewRootCmd(h.app)
		root.SetArgs([]string{"ui", "--no-open", "--addr", "127.0.0.1:0"})
		done <- root.ExecuteContext(ctx)
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("ui returned %v", err)
	}
}
