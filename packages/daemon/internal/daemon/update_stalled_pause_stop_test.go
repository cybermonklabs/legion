package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// The same through the sweep: a podless round whose issue pod the runtime cannot say of (its
// IssuePod read answers nothing, so podlessRound reaches tellPodlessRound, as on a runtime that
// replaces no pod by issue), whose implementer a project's pause stopped (suspended released, the
// pause still owing it a launch) and whose tester an idle release suspended. An hour past the
// bound the round is told (holds: no-pod), because the pause's claim has not answered
// (unansweredClaims' podHolds.owes); the tester alone would tell nothing. The notice names both
// claims `suspended_by: released`, which the controller's prompt reads as "the daemon's idle
// release of the claim's pod".
func TestAPodlessUpdateStalledNoticeTellsAPausesStopFromAnIdleRelease(t *testing.T) {
	rig := newPodUpdateRig(t, releasedUnasked(pauseStoppedRole), releasedUnasked(claim.RoleTester))
	rig.recordPause(store.PausePodHoldResumed, pauseStoppedRole)
	rig.clockAt(podUpdateBound.Add(time.Hour))
	for range 3 {
		rig.tick()
	}
	rig.settleAll()

	notices := rig.notices()
	if len(notices) != 1 || notices[0].Holds != texts.HoldsNoPod || notices[0].Update == nil {
		t.Fatalf("the controller was told %+v, want one no-pod notice of the round the pause's claim holds", notices)
	}
	rows := map[claim.Role]texts.UpdateWaiting{}
	for _, w := range notices[0].Update.Waiting {
		role := w.Role
		w.Claim, w.Role = "", ""
		rows[role] = w
	}
	if len(rows) != 2 {
		t.Fatalf("the notice lists %+v, want the implementer and the tester", notices[0].Update.Waiting)
	}
	if rows[pauseStoppedRole] == rows[claim.RoleTester] {
		t.Errorf("the update-stalled notice (holds: %s) lists the %s a project's pause stopped, which the round waits for, and the tester an idle release suspended alike: %+v; suspended_by: %s is \"the daemon's idle release of the claim's pod\" in the controller's prompt",
			notices[0].Holds, pauseStoppedRole, rows[claim.RoleTester], rows[claim.RoleTester].SuspendedBy)
	}
}
