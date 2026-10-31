package chaos

import (
	"context"
	"strings"
	"testing"

	"github.com/1985epma/k8scli/internal/dr"
)

func TestGuardRefusesAPlanThatIsNotALab(t *testing.T) {
	te := newTestEnv(t, allowedNS("shop"))
	plan := labPlan()
	plan.Safety.LabOnly = false
	err := Guard{Plan: plan, Env: te.env}.Lab()
	mustErrorIs(t, err, ErrNotLab)
	mustErrorIs(t, err, dr.ErrPreflight)
}

func TestGuardNeedsMatchingContextAndClient(t *testing.T) {
	te := newTestEnv(t)
	plan := labPlan()
	plan.Context = "another"
	if err := (Guard{Plan: plan, Env: te.env}).Lab(); err == nil || !strings.Contains(err.Error(), "another") {
		t.Errorf("a plan for another context must be refused, got %v", err)
	}
	te.env.Client = nil
	mustErrorIs(t, Guard{Plan: labPlan(), Env: te.env}.Lab(), dr.ErrUnreachable)
	if err := (Guard{}).Lab(); err == nil {
		t.Error("an empty guard must refuse")
	}
}

func TestGuardRealNeedsThePhraseInAProductionLookingContext(t *testing.T) {
	te := newTestEnv(t)
	te.env.Context = "eks-prod-eu"
	plan := labPlan()
	plan.Context = "eks-prod-eu"

	err := Guard{Plan: plan, Env: te.env}.Real()
	mustErrorIs(t, err, dr.ErrConfirmationRequired)
	if !strings.Contains(err.Error(), "chaos eks-prod-eu") {
		t.Errorf("the message should name the phrase: %v", err)
	}
	if err := (Guard{Plan: plan, Env: te.env, Confirm: "eks-prod-eu"}).Real(); err == nil {
		t.Error("the context name alone is not the chaos phrase")
	}
	if err := (Guard{Plan: plan, Env: te.env, Confirm: " chaos eks-prod-eu "}).Real(); err != nil {
		t.Errorf("the right phrase must pass: %v", err)
	}
	// A dry run (Lab only) never needs it.
	if err := (Guard{Plan: plan, Env: te.env}).Lab(); err != nil {
		t.Errorf("Lab must not need the phrase: %v", err)
	}
	// And a non-production context needs none.
	te.env.Context = testContext
	if err := te.guard().Real(); err != nil {
		t.Errorf("a lab context needs no phrase: %v", err)
	}
}

func TestGuardRealRefusesInReadOnlyMode(t *testing.T) {
	te := newTestEnv(t)
	g := te.guard()
	g.ReadOnly = true
	mustErrorIs(t, g.Real(), dr.ErrReadOnly)
}

func TestNamespaceMustCarryTheLabel(t *testing.T) {
	te := newTestEnv(t,
		allowedNS("ok"),
		ns("plain", nil),
		ns("off", map[string]string{LabelAllowed: "false"}),
		ns("kube-system", map[string]string{LabelAllowed: "true"}),
	)
	ctx := context.Background()
	g := te.guard()

	if err := g.Namespace(ctx, "ok"); err != nil {
		t.Errorf("a labelled namespace must pass: %v", err)
	}
	for _, name := range []string{"plain", "off", "kube-system"} {
		err := g.Namespace(ctx, name)
		mustErrorIs(t, err, ErrNamespaceNotAllowed)
	}
	if err := g.Namespace(ctx, "plain"); !strings.Contains(err.Error(), "kubectl label namespace plain "+LabelAllowed+"=true") {
		t.Errorf("the refusal should say how to opt in: %v", err)
	}
	mustErrorIs(t, g.Namespace(ctx, "missing"), dr.ErrPreflight)
}

func TestAnAllowedNamespaceCannotBeASystemOne(t *testing.T) {
	te := newTestEnv(t, ns("kube-public", map[string]string{LabelAllowed: "true"}), allowedNS("shop"))
	allowed, err := te.guard().allowedNamespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if allowed["kube-public"] || !allowed["shop"] {
		t.Errorf("allowed = %v", allowed)
	}
}

func TestConfirmPhrase(t *testing.T) {
	if got := ConfirmPhrase("kind-lab"); got != "chaos kind-lab" {
		t.Errorf("ConfirmPhrase = %q", got)
	}
}
