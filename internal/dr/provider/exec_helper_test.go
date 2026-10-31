package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// echoArgsEnv switches the test binary into a tiny program that prints its arguments, one %q-quoted
// per line. Tests copy the binary to a temp dir as "echoargs" and put that dir on PATH, so the
// argument handling of OSExec can be checked on every OS without relying on echo, which is a shell
// builtin on Windows and an MSYS program (that expands '*' itself) when Git for Windows is on PATH.
const echoArgsEnv = "K8SCLI_PROVIDER_ECHO_ARGS"

func TestMain(m *testing.M) {
	if os.Getenv(echoArgsEnv) == "1" {
		for _, arg := range os.Args[1:] {
			fmt.Printf("%q\n", arg)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// installEchoArgs makes "echoargs" resolvable through PATH for the current test.
func installEchoArgs(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot find the test binary: %v", err)
	}
	name := "echoargs"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dir := t.TempDir()
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	target := filepath.Join(dir, name)
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(echoArgsEnv, "1")
}

// Arguments must reach the program exactly as given, on every OS: nothing expands $HOME, backticks
// or '*', and spaces, quotes and backslashes survive the Windows command-line quoting.
func TestOSExecPassesArgumentsLiterallyOnEveryOS(t *testing.T) {
	installEchoArgs(t)
	args := []string{"$HOME; id", "`id`", "*", "with space", `quote"inside`, `back\slash`, `trailing\`, "", "--flag=a b", "'single'", "a&b|c>d"}

	out, err := testExec("echoargs").Run(context.Background(), "echoargs", args...)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var want []string
	for _, arg := range args {
		want = append(want, fmt.Sprintf("%q", arg))
	}
	if got := strings.Split(strings.TrimRight(strings.ReplaceAll(out, "\r\n", "\n"), "\n"), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("arguments changed on the way:\n got: %q\nwant: %q", got, want)
	}
}
