package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// The words of roles/controller-root.md that say what an update-stalled notice's suspended_by
// and holds mean, which these tests hold to what the daemon did with the same claims.
const (
	markedOperatorHoldsThePod = "with a `pod`, the daemon does not replace the pod until the operator resumes each claim it suspended"
	markedPauseHoldsTheRound  = "`pause` is a stop by a pause of the project that still owes the claim a launch, so the round waits for that claim"
)

// controllerRootWords is roles/controller-root.md as it ships, read from the repository as
// TestEveryStepTakenOutOfATextIsInItsReadersPrompt reads it.
func controllerRootWords(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(goldenRepositoryRoot, filepath.FromSlash(rolePrompts+"controller-root.md")))
	if err != nil {
		t.Fatalf("read the controller's prompt: %v", err)
	}
	return string(body)
}

// stalledRows is each claim an update-stalled notice lists, by role.
func stalledRows(t *testing.T, notice record.ControllerNotice) map[claim.Role]texts.UpdateWaiting {
	t.Helper()
	if notice.Update == nil {
		t.Fatalf("the notice names no round: %+v", notice)
	}
	rows := map[claim.Role]texts.UpdateWaiting{}
	for _, w := range notice.Update.Waiting {
		rows[w.Role] = w
	}
	return rows
}

// owedAPauseLaunch is whether a project's pause still owes role a launch on the issue's pod, as
// the sweep reads the pod's holds (store.PausePodHold.Owed, podHolds.owedLaunch).
func owedAPauseLaunch(r *podUpdateRig, role claim.Role) bool {
	r.t.Helper()
	holds, err := r.st.PausePodHoldsSince(r.ctx, "LEGION", podUpdateStart)
	if err != nil {
		r.t.Fatalf("read the pod's holds: %v", err)
	}
	for _, h := range holds {
		if h.Issue == podUpdateIssue && slices.Contains(h.Owed, role) {
			return true
		}
	}
	return false
}

// Main's notice of an operator's hold named the claims the hold is of, and only those
// (daemon/podupdate.go:operatorHoldReason at d46ec4e0, over heldByOperator: "An operator
// suspended %s. The daemon does not replace the pod until the operator resumes each of them.").
// heldByOperator counts a claim only where the round asks it (roundAsks), and the round asks no
// claim that left the round's pod (supervise's Claim.LeftPodRound). R8 of dispatch://INFRA-3686:
// "a fact or a step that it has the text give stays as a field", and R2: the fields are "the
// state that the reader acts on that its event has".
//
// The change marks every listed claim the operator suspended `suspended_by: operator`
// (waitingOf), and roles/controller-root.md reads that mark as the hold: "with a `pod`, the daemon
// does not replace the pod until the operator resumes each claim it suspended". Here the
// implementer answered and runs in the round's pod, the operator suspended the tester, and the
// operator also suspended the reviewer, whose process had come up in another pod during the
// round. An hour past the bound the notice of the operator's hold marks both the tester and the
// reviewer; the operator resumes the tester alone, and the daemon replaces the pod with the
// reviewer still suspended. So the mark says of the reviewer what main never said and what is
// false: the pod did not wait for its resume.
func TestAnUpdateStalledNoticeReadsAsTheOperatorsHoldOnlyTheClaimsThatHoldTheRound(t *testing.T) {
	leftTheRound := podlessMember(claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByOperator, false)
	leftTheRound.left = podUpdateRound
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), suspendedRole(claim.RoleTester, supervise.SuspendedByOperator), leftTheRound)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	if c := rig.claimOf(claim.RoleReviewer); !c.LeftPodRound(podUpdateRound) || c.SuspendedBy != supervise.SuspendedByOperator {
		t.Fatalf("the reviewer = %s by %q, left %q; want it suspended by the operator, out of the round's pod", c.State, c.SuspendedBy, c.UpdateLeftRound)
	}

	rig.clockAt(podUpdateBound.Add(time.Hour))
	rig.tick()
	rig.tick()
	notices := rig.notices()
	if len(notices) != 1 || notices[0].Holds != texts.HoldsOperator || notices[0].Update == nil || notices[0].Update.Pod == "" {
		t.Fatalf("the controller was told %+v, want one notice of the operator's hold over the round's pod", notices)
	}
	rows := stalledRows(t, notices[0])

	rig.resumeInPod(claim.RoleTester)
	rig.awaitReplacement(1, "once the operator resumed the tester, past the bound")
	if c := rig.claimOf(claim.RoleReviewer); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByOperator {
		t.Fatalf("the reviewer at the replacement = %s by %q, want it still suspended by the operator", c.State, c.SuspendedBy)
	}

	if rows[claim.RoleReviewer].SuspendedBy == string(supervise.SuspendedByOperator) && strings.Contains(controllerRootWords(t), markedOperatorHoldsThePod) {
		t.Errorf("the notice of the operator's hold marks the reviewer suspended_by: operator, and the controller's prompt says %q; "+
			"but the daemon replaced the pod once the operator resumed the tester, with the reviewer still suspended, since the round does not ask a claim that left its pod; "+
			"main's text named the tester alone (\"An operator suspended tester (suspended).\"); the notice lists %+v",
			markedOperatorHoldsThePod, notices[0].Update.Waiting)
	}
}

// roles/controller-root.md says of the mark the change added this round (texts.SuspendedByPause):
// "`pause` is a stop by a pause of the project that still owes the claim a launch, so the round
// waits for that claim". An update-stalled notice is told only past the round's bound, and past
// it no answer holds the round (main's daemon/podupdate.go:tellLateRound at d46ec4e0: "past the
// bound no answer holds the round, so the list says what each role said rather than whom the
// round waits for"; roundPodEnds: "Past the bound only the first two hold the pod"). R2 of
// dispatch://INFRA-3686: the fields are "the state that the reader acts on that its event has";
// a field the reader's prompt explains wrongly is the change stating a fact wrongly.
//
// Here the implementer answered and runs in the round's pod, the operator suspended the tester,
// and a project's pause stopped the reviewer and still owes it a launch. An hour past the bound
// the notice of the operator's hold marks the reviewer `pause`. The operator resumes the tester,
// and the daemon replaces the pod with the reviewer still suspended, unanswered, its launch
// still owed: the round did not wait for that claim.
func TestAnUpdateStalledNoticeSaysNoPauseClaimHoldsTheRoundPastItsBound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), suspendedRole(claim.RoleTester, supervise.SuspendedByOperator),
		releasedUnasked(claim.RoleReviewer))
	rig.recordPause(store.PausePodHoldResumed, claim.RoleReviewer)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")

	rig.clockAt(podUpdateBound.Add(time.Hour))
	rig.tick()
	rig.tick()
	notices := rig.notices()
	if len(notices) != 1 || notices[0].Holds != texts.HoldsOperator {
		t.Fatalf("the controller was told %+v, want one notice of the operator's hold", notices)
	}
	rows := stalledRows(t, notices[0])
	if rows[claim.RoleReviewer].SuspendedBy != texts.SuspendedByPause {
		t.Fatalf("the notice lists %+v, want the reviewer the pause stopped marked %q", notices[0].Update.Waiting, texts.SuspendedByPause)
	}

	rig.resumeInPod(claim.RoleTester)
	rig.awaitReplacement(1, "once the operator resumed the tester, past the bound")
	if c := rig.claimOf(claim.RoleReviewer); !c.Released() || c.UpdateSignalledAt != nil || !owedAPauseLaunch(rig, claim.RoleReviewer) {
		t.Fatalf("the reviewer at the replacement = %s by %q, answered %v, owed a launch by the pause %t; want it still suspended by the pause's stop, unanswered and owed",
			c.State, c.SuspendedBy, c.UpdateSignalledAt, owedAPauseLaunch(rig, claim.RoleReviewer))
	}

	if strings.Contains(controllerRootWords(t), markedPauseHoldsTheRound) {
		t.Errorf("the notice of the operator's hold marks the reviewer suspended_by: %s, and the controller's prompt says %q; "+
			"but the daemon replaced the pod past the bound once the operator resumed the tester, with the reviewer unanswered and its launch still owed",
			texts.SuspendedByPause, markedPauseHoldsTheRound)
	}
}
