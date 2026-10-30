package cmd

import (
	"strings"
	"testing"
)

func TestLogsStreamsOutput(t *testing.T) {
	h := newHarness(t, runningPod("default", "api-1"))

	if err := h.run("logs", "api-1"); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if h.out.String() != "fake logs" {
		t.Errorf("stdout = %q; want the fake log body", h.out.String())
	}
	if !strings.Contains(h.errOut.String(), `Defaulting to container "app"`) {
		t.Errorf("expected a multi-container warning, got %q", h.errOut.String())
	}
}

func TestLogsExplicitContainer(t *testing.T) {
	h := newHarness(t, runningPod("default", "api-1"))

	if err := h.run("logs", "api-1", "sidecar", "--since", "5m", "--timestamps", "-l", "-1"); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if strings.Contains(h.errOut.String(), "Defaulting") {
		t.Errorf("no warning expected when the container is explicit, got %q", h.errOut.String())
	}

	if err := h.run("logs", "api-1", "-c", "sidecar"); err != nil {
		t.Fatalf("logs -c: %v", err)
	}
}

func TestLogsUnknownContainer(t *testing.T) {
	h := newHarness(t, runningPod("default", "api-1"))

	err := h.run("logs", "api-1", "-c", "nope")
	if err == nil || !strings.Contains(err.Error(), "available: app, sidecar") {
		t.Errorf("err = %v; want the list of containers", err)
	}
}

func TestLogsConflictingContainerArguments(t *testing.T) {
	h := newHarness(t, runningPod("default", "api-1"))

	if err := h.run("logs", "api-1", "app", "-c", "sidecar"); err == nil {
		t.Error("expected an error when the container is given twice")
	}
}

func TestLogsMissingPod(t *testing.T) {
	h := newHarness(t)

	err := h.run("logs", "ghost")
	if err == nil || !strings.Contains(err.Error(), "failed to get pod") {
		t.Errorf("err = %v; want pod lookup error", err)
	}
}
