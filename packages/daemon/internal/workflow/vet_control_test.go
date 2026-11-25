package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
)

// A planted caller of givenRows's signature before this round (fact, stored, changed, defaults,
// now): the merged-tree vet must refuse it. It is the control of that vet and is never merged.
func TestPlantedCallerOfTheOldSignature(t *testing.T) {
	givenRows(intake.PlanGiven{}, nil, nil, nil, time.Time{})
}
