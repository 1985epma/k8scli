package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPodsTable(t *testing.T) {
	h := newHarness(t, runningPod("default", "api-1"), runningPod("other", "api-2"))

	if err := h.run("pods"); err != nil {
		t.Fatalf("pods: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want header + 1 pod:\n%s", len(lines), h.out.String())
	}

	header := strings.Fields(lines[0])
	if strings.Join(header, " ") != "NAME READY STATUS RESTARTS AGE" {
		t.Errorf("header = %q", lines[0])
	}

	row := strings.Fields(lines[1])
	want := []string{"api-1", "1/2", "CrashLoopBackOff", "3", "120m"}
	if strings.Join(row, " ") != strings.Join(want, " ") {
		t.Errorf("row = %v; want %v", row, want)
	}
}

func TestPodsWide(t *testing.T) {
	h := newHarness(t, runningPod("default", "api-1"))

	if err := h.run("pods", "-o", "wide"); err != nil {
		t.Fatalf("pods: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"IP", "NODE", "10.0.0.7", "node-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("wide output missing %q:\n%s", want, out)
		}
	}
}

func TestPodsAllShowsNamespace(t *testing.T) {
	h := newHarness(t, runningPod("default", "api-1"), runningPod("other", "api-2"))

	if err := h.run("pods-all"); err != nil {
		t.Fatalf("pods-all: %v", err)
	}
	out := h.out.String()
	if !strings.HasPrefix(out, "NAMESPACE") {
		t.Errorf("header should start with NAMESPACE:\n%s", out)
	}
	for _, want := range []string{"api-1", "api-2", "other"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPodsJSON(t *testing.T) {
	h := newHarness(t, runningPod("default", "api-1"))

	if err := h.run("pods", "-o", "json"); err != nil {
		t.Fatalf("pods: %v", err)
	}

	var list corev1.PodList
	if err := json.Unmarshal(h.out.Bytes(), &list); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, h.out.String())
	}
	if list.Kind != "PodList" || len(list.Items) != 1 || list.Items[0].Name != "api-1" {
		t.Errorf("unexpected list: %+v", list)
	}
}

func TestPodsYAML(t *testing.T) {
	h := newHarness(t, runningPod("default", "api-1"))

	if err := h.run("pods", "-o", "yaml"); err != nil {
		t.Fatalf("pods: %v", err)
	}
	if !strings.Contains(h.out.String(), "kind: PodList") {
		t.Errorf("unexpected yaml:\n%s", h.out.String())
	}
}

func TestPodsInvalidOutputFormat(t *testing.T) {
	h := newHarness(t)

	err := h.run("pods", "-o", "xml")
	if err == nil || !strings.Contains(err.Error(), "unsupported output format") {
		t.Errorf("err = %v; want unsupported output format", err)
	}
}

func TestPodsEmptyNamespaceMessage(t *testing.T) {
	h := newHarness(t)

	if err := h.run("pods", "-n", "empty"); err != nil {
		t.Fatalf("pods: %v", err)
	}
	if h.out.Len() != 0 {
		t.Errorf("stdout should stay empty, got %q", h.out.String())
	}
	if !strings.Contains(h.errOut.String(), `No pods found in namespace "empty"`) {
		t.Errorf("stderr = %q", h.errOut.String())
	}
}
