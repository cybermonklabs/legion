package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A control for a merged-tree vet, never merged: a caller of transition's old signature, which
// took the move's reason as a string. The vet of this tree must fail on it.
func TestPlantedOldSignatureCaller(t *testing.T) {
	var e *Engine
	_ = func() error {
		return e.transition(nil, nil, record.Issue{}, TriggerBackward, "", record.PhaseRow{}, nil, "a reason as a string")
	}
}
