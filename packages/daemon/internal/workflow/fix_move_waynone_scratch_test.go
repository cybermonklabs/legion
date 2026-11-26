package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// A scratch measurement, never merged: a stop ends a fix that pushed Y (way 4), the restart's
// implementer start moves the task's copy to Y, and the tester's start after it must move nothing.
// This start still decides by the clock (copyMoveAfterFix), so it is measured on each clock.
func TestScratchTheStartAfterARestartMovesNothingWhateverTheClock(t *testing.T) {
	for _, clock := range engineClocks {
		t.Run(clock.name, func(t *testing.T) {
			r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
			clock.set(r.engine)
			r.pushed(headTwo)
			r.stop()
			r.todo()
			if got := r.moveToOf(claim.RoleImplementer); got != headTwo {
				t.Fatalf("the restarted implementer moves the task's copy to %q, want %s", got, headTwo)
			}
			if got := r.nextStart(TriggerImplementationReady, claim.RoleTester, "testing").MoveTo; got != "" {
				t.Fatalf("the tester's start after the restart moves the task's copy to %q, want no move", got)
			}
		})
	}
}
