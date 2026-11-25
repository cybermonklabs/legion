package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A project's pause stopped both roles of the pod while its round was open: the tester, which had
// already answered the round, and the implementer, which had not. Both are suspended released,
// and the pause still owes each of them a launch (store.PausePodHold.Owed names both, since
// neither has run in the pod since). The round waits for the implementer alone. An hour past the
// bound the podless round is told (holds: no-pod). The change names the implementer `suspended_by:
// pause`, and the tester `suspended_by: released`, which its controller prompt reads as "the
// daemon's idle release of the claim's pod" (roles/controller-root.md). No idle release suspended
// the tester: the pause did, and the round's holds say so (podHolds.owedLaunch). pauseStopped
// marks `pause` only where podHolds.owes holds, which adds !answered.
func TestAnUpdateStalledNoticeNamesNoClaimAPauseStoppedAnIdleRelease(t *testing.T) {
	const answeredRole = claim.RoleTester
	rig := newPodUpdateRig(t, releasedUnasked(pauseStoppedRole),
		podlessMember(answeredRole, supervise.StateSuspended, supervise.SuspendedByReleased, true))
	rig.recordPause(store.PausePodHoldResumed, pauseStoppedRole, answeredRole)
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
		rows[w.Role] = w
	}
	t.Logf("the notice lists %+v", notices[0].Update.Waiting)
	if got := rows[pauseStoppedRole].SuspendedBy; got != texts.SuspendedByPause {
		t.Fatalf("the unanswered %s the pause stopped is suspended_by %q, want %q", pauseStoppedRole, got, texts.SuspendedByPause)
	}
	if got := rows[answeredRole]; !got.Answered || got.SuspendedBy == string(supervise.SuspendedByReleased) {
		t.Errorf("the update-stalled notice (holds: %s) names the %s a project's pause stopped, which answered the round and which the pause still owes a launch, %+v: suspended_by: released is \"the daemon's idle release of the claim's pod\" in the controller's prompt, and no idle release suspended it",
			notices[0].Holds, answeredRole, got)
	}
}
