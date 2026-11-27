package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A parent's held start comes back, once no open child is left and its pull request has not
// merged, as the first step of its walk after the implementer's that stands at or before the held
// start (dispatch://INFRA-3320 R12, the correction of dispatch://INFRA-3320/artifact/open-items-md
// item 28): testing on today's line, whatever was held past implementing; the reviewer on a walk
// with no tester step; and the held start itself where the walk holds no step between the
// implementer's and it, holds no implementer's step, or puts the held start before the
// implementer's. A held planning or implementing always comes back as itself.
func TestAHeldStartReturnsAsTheFirstStepAfterTheImplementersAtOrBeforeIt(t *testing.T) {
	today := []phase.Phase{phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Accepting}
	noTester := []phase.Phase{phase.Implementing, phase.Reviewing}
	noTesterAccepted := []phase.Phase{phase.Implementing, phase.Reviewing, phase.Accepting}
	reviewFirst := []phase.Phase{phase.Implementing, phase.Reviewing, phase.Testing, phase.Accepting}
	acceptedOnly := []phase.Phase{phase.Implementing, phase.Accepting}
	noImplementer := []phase.Phase{phase.Reviewing, phase.Accepting}
	beforeImplementer := []phase.Phase{phase.Reviewing, phase.Implementing, phase.Testing}
	for _, tc := range []struct {
		name  string
		steps []phase.Phase
		held  phase.Phase
		want  phase.Phase
	}{
		{"today's line, a held testing", today, phase.Testing, phase.Testing},
		{"today's line, a held reviewing", today, phase.Reviewing, phase.Testing},
		{"today's line, a held accepting", today, phase.Accepting, phase.Testing},
		{"today's line, a held planning", today, phase.Planning, phase.Planning},
		{"today's line, a held implementing", today, phase.Implementing, phase.Implementing},
		{"no tester step, a held reviewing", noTester, phase.Reviewing, phase.Reviewing},
		{"no tester step, a held accepting", noTesterAccepted, phase.Accepting, phase.Reviewing},
		{"the reviewer before the tester, a held testing", reviewFirst, phase.Testing, phase.Reviewing},
		{"no step between the implementer's and a held accepting", acceptedOnly, phase.Accepting, phase.Accepting},
		{"no implementer's step", noImplementer, phase.Accepting, phase.Accepting},
		{"the held start before the implementer's", beforeImplementer, phase.Reviewing, phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReturnStep(tc.steps, tc.held); got != tc.want {
				t.Fatalf("ReturnStep(%v, %s) = %s, want %s", tc.steps, tc.held, got, tc.want)
			}
		})
	}
}
