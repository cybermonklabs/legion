package daemon

// R7 of #5285, GPT-6.1 Sol's point 1 on #5285 at 0259f8af (and the Legion
// reviewer's point 3), which the spec's architect ruled a fix inside R7: a round's replacement
// whose suspension landed is a replacement made, whatever the call answered. The Sandbox runtime
// writes the suspension that takes the pod before it waits for the pod to go (replaceRoundPod),
// so a call whose wait ran out of its bound, or a daemon that stopped in that wait, has taken the
// pod all the same. The daemon records the replacement, with the read of the workspace before it,
// by issue and pod before the runtime acts, and a later sweep finishes it from that record: it
// ends the round as the replacement it was and tells the issue's architect what the read found.
// It never reads such a pod again and never keeps it: a read of a pod whose processes are going
// can fail, and a failed read over a drift the roles can keep working under would keep a pod that
// is already on its way out. The cases are the pod still there under its suspended Sandbox, the
// pod gone with each role back in the pod that replaced it, and a daemon that restarted after the
// call. The control is a call refused before its suspension: the round stays open, and the next
// sweep reads the workspace again and replaces the pod over that read.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// errWaitedOut is a replacement's wait for its pod to go, run out once its suspension landed.
var errWaitedOut = fmt.Errorf("replace the pod: wait out its pod: %w", context.DeadlineExceeded)

// landedRoles are the two roles of the pod in each case here, both of which answer the round.
var landedRoles = []claim.Role{claim.RoleImplementer, claim.RoleTester}

// stillThere is the pod still there under its suspended Sandbox at the next sweep: its role
// processes are going, so a read of the workspace through them can no longer be made.
func stillThere(r *podUpdateRig) *podUpdateRig {
	r.rt.FailWorkspaceWork(podUpdateIssue, errRoundRead)
	return r
}

// goneWithItsRoles is the pod gone at the next sweep, each role back in the pod that replaced it.
func goneWithItsRoles(r *podUpdateRig) *podUpdateRig {
	for _, role := range landedRoles {
		r.displacedInto(role, podUpdateReplacementPod)
	}
	return r
}

// restartedOverIt is a daemon restarted after the call: it reads the cluster as the first daemon
// left it, the Sandbox suspended over the pod, and a read through the going pod's roles fails.
func restartedOverIt(r *podUpdateRig) *podUpdateRig {
	next := r.restart()
	next.rt.ScriptSuspendedOver(podUpdatePod)
	next.rt.FailWorkspaceWork(podUpdateIssue, errRoundRead)
	return next
}

func TestAReplacementWhoseSuspensionLandedIsFinishedFromItsOwnRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		drift string
		work  runtime.WorkspaceWork
		// after is the pod and the daemon as the next sweep finds them, and the rig that sweeps.
		after func(*podUpdateRig) *podUpdateRig
		notes int
	}{
		{name: "a_breaking_round_whose_pod_is_still_there", drift: roundBreakingDrift, work: roundUnpushed, after: stillThere, notes: 1},
		{name: "a_breaking_round_whose_pod_is_gone", drift: roundBreakingDrift, work: roundUnpushed, after: goneWithItsRoles, notes: 1},
		{name: "a_breaking_round_after_a_restart", drift: roundBreakingDrift, work: roundUnpushed, after: restartedOverIt, notes: 1},
		{name: "a_round_the_roles_keep_working_under_whose_pod_is_still_there", drift: podUpdateDrift, work: roundClean, after: stillThere},
		{name: "a_round_the_roles_keep_working_under_after_a_restart", drift: podUpdateDrift, work: roundClean, after: restartedOverIt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			rig.rt.ScriptWorkspaceWork(podUpdateIssue, tc.work)
			rig.rt.FailReplacePodAfterItsSuspension(errWaitedOut)
			rig.answerRound(tc.drift, landedRoles...)
			rig.awaitReplacement(1, "for the call whose wait ran out once its suspension landed")
			if reads := rig.workspaceReads(); reads != 1 {
				t.Fatalf("the round read the workspace %d times, want once before its replacement", reads)
			}

			next := tc.after(rig)
			for _, role := range landedRoles {
				next.awaitSwept(role, "its round to end as the replacement it was", func(c supervise.Claim) bool { return c.UpdateRound == "" })
			}
			for range roundSweeps {
				next.tick()
			}
			told := next.architectNotices(record.WorkspaceTakenNotice)
			if len(told) != tc.notes || (tc.notes == 1 && !strings.Contains(told[0].Reason, passedOverCommit)) {
				t.Fatalf("the architect was told %+v, want %d workspace-taken notice(s), naming the commit %s the read before the replacement found",
					told, tc.notes, passedOverCommit)
			}
			if reason, pod := next.keptReason(); reason != "" {
				t.Fatalf("the issue's decline row reads %q of pod %q, want none: a pod its own replacement's suspension took is never kept", reason, pod)
			}
			if kept := next.architectNotices(record.WorkspaceKeptNotice); len(kept) != 0 {
				t.Fatalf("the architect was told the pod stays %+v, want nothing of the kind", kept)
			}
			reads := next.workspaceReads()
			if next == rig {
				reads--
			}
			if reads != 0 {
				t.Fatalf("the workspace of the pod the suspension took was read %d times more, want no read after the replacement's own", reads)
			}
			if again := next.replacements(); next == rig && len(again) != 1 {
				t.Fatalf("the pod was replaced through a process %d times, want the one call whose suspension landed", len(again))
			}
		})
	}
}

// The control: a ReplacePod the runtime refused before its suspension took nothing. The round stays
// open, the next sweep reads the workspace again, and the replacement it makes over that read is
// told once.
func TestAReplacementRefusedBeforeItsSuspensionReadsTheWorkspaceAgain(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
	rig.rt.FailReplacePod(errors.New("the api server refused the suspension"))
	rig.answerRound(roundBreakingDrift, landedRoles...)
	rig.awaitReplacement(1, "for the call the runtime refused")
	for _, role := range landedRoles {
		if c := rig.claimOf(role); c.UpdateRound != podUpdateRound {
			t.Fatalf("the %s records round %q after a refused replacement, want the round still open", role, c.UpdateRound)
		}
	}
	rig.rt.FailReplacePod(nil)
	rig.awaitReplacement(2, "once the runtime takes the replacement")
	for _, role := range landedRoles {
		rig.awaitClaim(role, "its round to end with the replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
	if reads := rig.workspaceReads(); reads != 2 {
		t.Fatalf("the workspace was read %d times, want twice: once before each replacement asked for", reads)
	}
	rig.wantToldOnce("once the runtime took the replacement")
}
