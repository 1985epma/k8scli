package provider

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testExec(programs ...string) *OSExec {
	allowed := map[string]bool{}
	for _, p := range programs {
		allowed[p] = true
	}
	return &OSExec{allowed: allowed}
}

func TestOSExecOnlyRunsAllowListedPrograms(t *testing.T) {
	e := NewOSExec()
	for _, name := range []string{"rm", "sh", "bash", "curl", "", "/bin/echo", "/usr/bin/docker", "../docker", `..\docker`, "docker ", "Docker", "docker\x00", "ssh"} {
		_, err := e.Run(context.Background(), name, "x")
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Run(%q) = %v; want ErrNotAllowed", name, err)
		}
	}
	for _, name := range []string{"docker", "kind", "minikube", "az", "aws", "gcloud", "kubectl"} {
		if !defaultAllowed[name] {
			t.Errorf("%s should be allowed", name)
		}
	}
	if len(defaultAllowed) != 7 {
		t.Errorf("the allow-list has %d entries; adding one is a security decision", len(defaultAllowed))
	}
}

func TestOSExecRejectsArgumentsThatCouldSmuggleLines(t *testing.T) {
	e := testExec("echo")
	for _, arg := range []string{"a\nb", "a\rb", "a\x00b", "\n"} {
		if _, err := e.Run(context.Background(), "echo", "ok", arg); err == nil || !strings.Contains(err.Error(), "argument 2") {
			t.Errorf("arg %q: err = %v", arg, err)
		}
	}
}

func TestOSExecRunsWithoutAShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		// echo is not a program on Windows, and the one Git for Windows puts on PATH is an MSYS
		// binary that globs '*' itself. TestOSExecPassesArgumentsLiterallyOnEveryOS covers Windows.
		t.Skip("echo is not a plain program on Windows")
	}
	// A shell would expand $HOME and split on ';'. Arguments must arrive literally.
	out, err := testExec("echo").Run(context.Background(), "echo", "$HOME; id", "`id`", "*")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "$HOME; id `id` *" {
		t.Errorf("output = %q; arguments were interpreted", out)
	}
}

func TestOSExecErrorCarriesStderrAndKeepsStdout(t *testing.T) {
	e := testExec("sh")
	out, err := e.Run(context.Background(), "sh", "-c", "echo Stopped; echo 'boom happened' >&2; exit 7")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.TrimSpace(out) != "Stopped" {
		t.Errorf("stdout = %q; callers such as minikube status need it even on failure", out)
	}
	if !strings.Contains(err.Error(), "boom happened") || !strings.Contains(err.Error(), "exit status 7") {
		t.Errorf("err = %v", err)
	}
}

func TestOSExecTimesOut(t *testing.T) {
	e := testExec("sleep")
	e.Timeout = 150 * time.Millisecond
	start := time.Now()
	_, err := e.Run(context.Background(), "sleep", "10")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want a deadline error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s; the command was not killed", elapsed)
	}
}

func TestOSExecStopsWhenTheCallerCancels(t *testing.T) {
	e := testExec("sleep")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if _, err := e.Run(ctx, "sleep", "10"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want context.Canceled", err)
	}
}

func TestOSExecCapsOutput(t *testing.T) {
	e := testExec("sh")
	e.MaxOutput = 100
	out, err := e.Run(context.Background(), "sh", "-c", `i=0; while [ $i -lt 3000 ]; do echo xxxxxxxxxxxxxxxxxxxx; i=$((i+1)); done`)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 100 {
		t.Errorf("kept %d bytes; want exactly the cap of 100", len(out))
	}
}

func TestOSExecPassesTheEnvironmentButDoesNotPutItInErrors(t *testing.T) {
	t.Setenv("K8SCLI_PROVIDER_TEST", "from-parent")
	out, err := testExec("sh").Run(context.Background(), "sh", "-c", "echo $K8SCLI_PROVIDER_TEST")
	if err != nil || strings.TrimSpace(out) != "from-parent" {
		t.Errorf("parent environment not passed: %q, %v", out, err)
	}

	e := testExec("ls")
	e.env = []string{"AWS_SECRET_ACCESS_KEY=hunter2"}
	_, err = e.Run(context.Background(), "ls", "/this/path/does/not/exist")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("an error must never carry the environment: %v", err)
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{limit: 5}
	for _, chunk := range []string{"ab", "cdef", "ghi"} {
		n, err := b.Write([]byte(chunk))
		if n != len(chunk) || err != nil {
			t.Fatalf("Write(%q) = %d, %v; a capped buffer must still accept the bytes", chunk, n, err)
		}
	}
	if b.String() != "abcde" {
		t.Errorf("buffer = %q", b.String())
	}
}

func TestCommandLineQuotesForDisplay(t *testing.T) {
	cases := map[string]string{
		"docker":              "docker",
		"label=a.b/c":         "label=a.b/c",
		"{{.State.Running}}":  "'{{.State.Running}}'",
		"nodegroup.{a:b}":     "'nodegroup.{a:b}'",
		"with space":          "'with space'",
		"it's":                `'it'\''s'`,
		"":                    "''",
		"minSize=0,maxSize=3": "minSize=0,maxSize=3",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s; want %s", in, got, want)
		}
	}
	if got := commandLine("docker", []string{"inspect", "-f", "{{.State.Running}}", "n1"}); got != "docker inspect -f '{{.State.Running}}' n1" {
		t.Errorf("commandLine = %s", got)
	}
}
