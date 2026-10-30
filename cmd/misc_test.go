package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testKubeconfig = `apiVersion: v1
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

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func TestContextsAreSortedAndMarked(t *testing.T) {
	h := newHarness(t)
	path := writeKubeconfig(t)

	for i := 0; i < 5; i++ { // map iteration used to make this flaky
		if err := h.run("contexts", "--kubeconfig", path); err != nil {
			t.Fatalf("contexts: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
		if len(lines) != 3 {
			t.Fatalf("got %d lines:\n%s", len(lines), h.out.String())
		}
		if !strings.Contains(lines[1], "alpha") || !strings.Contains(lines[2], "zeta") {
			t.Fatalf("contexts not sorted:\n%s", h.out.String())
		}
		if !strings.HasPrefix(lines[2], "*") {
			t.Errorf("current context should be marked: %q", lines[2])
		}
	}
}

func TestContextsJSON(t *testing.T) {
	h := newHarness(t)

	if err := h.run("contexts", "--kubeconfig", writeKubeconfig(t), "-o", "json"); err != nil {
		t.Fatalf("contexts: %v", err)
	}
	var contexts []struct {
		Name    string `json:"name"`
		Current bool   `json:"current"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &contexts); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(contexts) != 2 || contexts[0].Name != "alpha" || !contexts[1].Current {
		t.Errorf("unexpected contexts: %+v", contexts)
	}
}

func TestUseSwitchesContext(t *testing.T) {
	h := newHarness(t)
	path := writeKubeconfig(t)

	if err := h.run("use", "alpha", "--kubeconfig", path); err != nil {
		t.Fatalf("use: %v", err)
	}
	if !strings.Contains(h.out.String(), "Switched to context: alpha") {
		t.Errorf("unexpected output: %q", h.out.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "current-context: alpha") {
		t.Errorf("kubeconfig not updated:\n%s", data)
	}
}

func TestUseUnknownContext(t *testing.T) {
	h := newHarness(t)

	err := h.run("use", "nope", "--kubeconfig", writeKubeconfig(t))
	if err == nil || !strings.Contains(err.Error(), `context "nope" not found`) {
		t.Errorf("err = %v", err)
	}
}

func TestClusterInfo(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}
	h := newHarness(t, node, namespace)

	if err := h.run("cluster"); err != nil {
		t.Fatalf("cluster: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"Context:    dev", "Nodes:      1", "- node-1: True", "- kube-system"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	if err := h.run("cluster", "-o", "json"); err != nil {
		t.Fatalf("cluster -o json: %v", err)
	}
	var info clusterInfo
	if err := json.Unmarshal(h.out.Bytes(), &info); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(info.Nodes) != 1 || info.Nodes[0].Ready != "True" {
		t.Errorf("unexpected info: %+v", info)
	}
}

func TestVersionCommand(t *testing.T) {
	h := newHarness(t)

	if err := h.run("version"); err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.HasPrefix(h.out.String(), "k8scli version ") {
		t.Errorf("unexpected output: %q", h.out.String())
	}

	if err := h.run("--version"); err != nil {
		t.Fatalf("--version: %v", err)
	}
	if !strings.HasPrefix(h.out.String(), "k8scli version ") {
		t.Errorf("unexpected --version output: %q", h.out.String())
	}
}

func TestCompletionAndHelp(t *testing.T) {
	h := newHarness(t)

	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		if err := h.run("completion", shell); err != nil {
			t.Errorf("completion %s: %v", shell, err)
		}
		if h.out.Len() == 0 {
			t.Errorf("completion %s produced no output", shell)
		}
	}
	if err := h.run("completion", "tcsh"); err == nil {
		t.Error("completion tcsh should fail")
	}

	if err := h.run("help"); err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, want := range []string{"pods", "scale", "logs", "contexts", "version"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("help output missing %q", want)
		}
	}
}
