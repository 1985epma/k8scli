package cmd

import (
	"strings"
	"testing"
)

func TestParseReplicas(t *testing.T) {
	valid := map[string]int32{"0": 0, "3": 3, "10": 10}
	for input, want := range valid {
		got, err := parseReplicas(input)
		if err != nil || got != want {
			t.Errorf("parseReplicas(%q) = %d, %v; want %d", input, got, err, want)
		}
	}

	for _, input := range []string{"", "abc", "4abc", "-1", "1.5", "99999999999", " 3"} {
		if _, err := parseReplicas(input); err == nil {
			t.Errorf("parseReplicas(%q) succeeded; want error", input)
		}
	}
}

func TestScaleUpdatesReplicas(t *testing.T) {
	h := newHarness(t, deployment("default", "web", 1))

	if err := h.run("scale", "web", "5"); err != nil {
		t.Fatalf("scale: %v", err)
	}

	if got := replicasOf(t, h.client, "default", "web"); got != 5 {
		t.Errorf("replicas = %d; want 5", got)
	}
	if !strings.Contains(h.out.String(), "Deployment web scaled to 5 replicas") {
		t.Errorf("unexpected output: %q", h.out.String())
	}
	if !strings.Contains(h.errOut.String(), "Context: dev | Namespace: default") {
		t.Errorf("context banner missing: %q", h.errOut.String())
	}
}

func TestScaleUsesNamespaceFlag(t *testing.T) {
	h := newHarness(t, deployment("team-a", "web", 1))

	if err := h.run("scale", "web", "3", "-n", "team-a"); err != nil {
		t.Fatalf("scale: %v", err)
	}
	if got := replicasOf(t, h.client, "team-a", "web"); got != 3 {
		t.Errorf("replicas = %d; want 3", got)
	}
}

func TestScaleRejectsInvalidReplicas(t *testing.T) {
	for _, value := range []string{"4abc", "-2", "many"} {
		h := newHarness(t, deployment("default", "web", 1))

		if err := h.run("scale", "web", value); err == nil {
			t.Errorf("scale web %q succeeded; want error", value)
		}
		if got := replicasOf(t, h.client, "default", "web"); got != 1 {
			t.Errorf("replicas changed to %d after invalid input %q", got, value)
		}
	}
}

func TestScaleMissingDeployment(t *testing.T) {
	h := newHarness(t)

	err := h.run("scale", "ghost", "2")
	if err == nil || !strings.Contains(err.Error(), "failed to get deployment ghost") {
		t.Fatalf("err = %v; want a deployment lookup error", err)
	}
}

func TestScaleDryRunDoesNotChange(t *testing.T) {
	h := newHarness(t, deployment("default", "web", 1))

	if err := h.run("scale", "web", "5", "--dry-run"); err != nil {
		t.Fatalf("scale: %v", err)
	}
	if got := replicasOf(t, h.client, "default", "web"); got != 1 {
		t.Errorf("dry-run changed replicas to %d", got)
	}
	if !strings.Contains(h.out.String(), "would be scaled from 1 to 5") {
		t.Errorf("unexpected output: %q", h.out.String())
	}
}

func TestScaleNoopWhenAlreadyAtTarget(t *testing.T) {
	h := newHarness(t, deployment("default", "web", 3))

	if err := h.run("scale", "web", "3"); err != nil {
		t.Fatalf("scale: %v", err)
	}
	if !strings.Contains(h.out.String(), "already has 3 replicas") {
		t.Errorf("unexpected output: %q", h.out.String())
	}
}

func TestScaleProductionContextRequiresConfirmation(t *testing.T) {
	t.Run("declined", func(t *testing.T) {
		h := newHarness(t, deployment("default", "web", 1))
		h.app.ContextName = "arn:aws:eks:us-east-1:1:cluster/prod-main"
		h.app.In = strings.NewReader("n\n")

		if err := h.run("scale", "web", "4"); err == nil {
			t.Fatal("scale succeeded without confirmation")
		}
		if got := replicasOf(t, h.client, "default", "web"); got != 1 {
			t.Errorf("replicas changed to %d despite refusal", got)
		}
	})

	t.Run("no input", func(t *testing.T) {
		h := newHarness(t, deployment("default", "web", 1))
		h.app.ContextName = "production"

		if err := h.run("scale", "web", "4"); err == nil {
			t.Fatal("scale succeeded with empty stdin")
		}
	})

	t.Run("accepted", func(t *testing.T) {
		h := newHarness(t, deployment("default", "web", 1))
		h.app.ContextName = "production"
		h.app.In = strings.NewReader("yes\n")

		if err := h.run("scale", "web", "4"); err != nil {
			t.Fatalf("scale: %v", err)
		}
		if got := replicasOf(t, h.client, "default", "web"); got != 4 {
			t.Errorf("replicas = %d; want 4", got)
		}
	})

	t.Run("yes flag", func(t *testing.T) {
		h := newHarness(t, deployment("default", "web", 1))
		h.app.ContextName = "production"

		if err := h.run("scale", "web", "4", "--yes"); err != nil {
			t.Fatalf("scale: %v", err)
		}
		if got := replicasOf(t, h.client, "default", "web"); got != 4 {
			t.Errorf("replicas = %d; want 4", got)
		}
	})
}

func TestSetQuickScale(t *testing.T) {
	h := newHarness(t, deployment("default", "web", 1))

	if err := h.run("set", "web", "6"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := replicasOf(t, h.client, "default", "web"); got != 6 {
		t.Errorf("replicas = %d; want 6", got)
	}

	err := h.run("set", "web", "5")
	if err == nil || !strings.Contains(err.Error(), "use: 2, 4, 6, or 8") {
		t.Errorf("err = %v; want quick-scale validation error", err)
	}
}
