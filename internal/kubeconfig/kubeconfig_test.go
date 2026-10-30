package kubeconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `apiVersion: v1
kind: Config
current-context: zeta
clusters:
- name: c1
  cluster: {server: "https://c1.example.com"}
contexts:
- name: zeta
  context: {cluster: c1, user: u1, namespace: team-z}
- name: alpha
  context: {cluster: c1, user: u1}
users:
- name: u1
  user: {token: abc}
`

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestContextsSorted(t *testing.T) {
	contexts, err := New(write(t, sample)).Contexts()
	if err != nil {
		t.Fatal(err)
	}
	if len(contexts) != 2 || contexts[0].Name != "alpha" || contexts[1].Name != "zeta" {
		t.Fatalf("contexts = %+v", contexts)
	}
	if contexts[0].Current || !contexts[1].Current {
		t.Errorf("current flag wrong: %+v", contexts)
	}
	if contexts[1].Namespace != "team-z" || contexts[1].Cluster != "c1" || contexts[1].User != "u1" {
		t.Errorf("context details wrong: %+v", contexts[1])
	}
}

func TestNamespaceFromContext(t *testing.T) {
	path := write(t, sample)

	if got := New(path).Namespace(); got != "team-z" {
		t.Errorf("Namespace() = %q; want team-z", got)
	}

	if err := New(path).UseContext("alpha"); err != nil {
		t.Fatal(err)
	}
	if got := New(path).Namespace(); got != DefaultNamespace {
		t.Errorf("Namespace() = %q; want %q for a context without namespace", got, DefaultNamespace)
	}
}

func TestUseContextPersists(t *testing.T) {
	path := write(t, sample)
	loader := New(path)

	if err := loader.UseContext("alpha"); err != nil {
		t.Fatal(err)
	}
	if got := loader.CurrentContext(); got != "alpha" {
		t.Errorf("CurrentContext() = %q; want alpha", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "current-context: alpha") {
		t.Errorf("file not rewritten:\n%s", data)
	}
	if !strings.Contains(string(data), "team-z") {
		t.Errorf("other contexts were lost:\n%s", data)
	}
}

func TestUseContextUnknown(t *testing.T) {
	if err := New(write(t, sample)).UseContext("nope"); err == nil {
		t.Error("expected an error for an unknown context")
	}
}

func TestMissingExplicitFile(t *testing.T) {
	loader := New(filepath.Join(t.TempDir(), "missing"))
	if _, err := loader.Contexts(); err == nil {
		t.Error("expected an error for a missing explicit kubeconfig")
	}
	if _, err := loader.RESTConfig(); err == nil {
		t.Error("expected RESTConfig to fail for a missing kubeconfig")
	}
}

func TestRESTConfig(t *testing.T) {
	config, err := New(write(t, sample)).RESTConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != "https://c1.example.com" {
		t.Errorf("Host = %q", config.Host)
	}
}
