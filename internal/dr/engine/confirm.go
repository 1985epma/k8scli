package engine

import "github.com/1985epma/k8scli/internal/dr"

// stopPhrase is what must be typed to power machines or a cluster off.
func stopPhrase(contextName string) string { return "stop " + contextName }

// ConfirmPhrase is the exact text a person must type before a real run starts, or "" when none is
// needed:
//
//   - a "down" run that reaches the power phase needs "stop <context>". It does when the scope
//     lists the power phase, or when the scope is open (no phases listed) and the plan has a
//     provider. A run limited to other phases (for example only "apps") does not.
//   - any other run on a production-looking context needs the context name.
//   - everything else needs nothing.
//
// Dry runs never need confirmation.
func ConfirmPhrase(plan *dr.Plan, dir dr.Direction, scope dr.Scope) string {
	if plan == nil {
		return ""
	}
	if dir == dr.DirDown && powerInScope(plan, scope) {
		return stopPhrase(plan.Context)
	}
	if plan.ProductionLike() {
		return plan.Context
	}
	return ""
}

func powerInScope(plan *dr.Plan, scope dr.Scope) bool {
	if len(scope.Phases) == 0 {
		return plan.Provider != nil
	}
	return scope.HasPhase(dr.PhasePower)
}
