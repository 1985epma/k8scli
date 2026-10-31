package engine

import (
	"errors"

	"github.com/1985epma/k8scli/internal/dr/provider"
)

// isManual reports whether an action failed because only a person can do the step (for example
// stopping Docker Desktop's Kubernetes from its settings). The provider package owns that sentinel;
// this is the only place the engine looks at it.
//
// Such a step is not a malfunction: the run stops there, shows the instructions the action gave
// (kept verbatim in the step's Err) with the event kind "step-manual", and continues with Resume
// once the person has acted. Resume finds the step's Done true and goes on.
func isManual(err error) bool { return errors.Is(err, provider.ErrManualStep) }
