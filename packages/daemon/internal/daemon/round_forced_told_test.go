package daemon

// R7 of #5285 (spec v20, verification v15's c-forced-replacement-told), the round's
// case: a round over a drift that breaks the pod replaces it as before, whatever the read of its
// workspace finds, and the issue's architect is told once what the replacement took. A round is one
// over a drift that breaks the pod where the reasons it recorded open on the mark
// runtime.BreakingDrift ("[breaks the pod] "), which the runtime puts at the start of the reason of
// a drift that breaks the pod (runtime.Breaks): today a drift of the pod's service account (spec
// v23), one of its exchange identity where a launch into the pod is refused until its round
// replaces it (spec v26), and the kinds a later change declares. This test scripts such a reason
// of the image.

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// roundBreakingDrift is a drift of a kind a later change declares breaks the pod: its reason
// starts with the mark such a reason carries (runtime.BreakingDrift). It is written out here, as
// the reason a runtime would give, rather than built from the constant.
const roundBreakingDrift = "[breaks the pod] image legion-worker:v22.16.2, now legion-worker:v22.17.0"

// A round over a drift that breaks the pod replaces the pod as before, over a workspace that holds
// one commit origin does not hold, and the issue's architect is told once what the replacement
// took, naming the commit. It keeps no pod, so it records no decline. The control is the same round
// over a clean workspace, which tells nobody.
func TestARoundOverADriftThatBreaksThePodSaysWhatItTook(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		work  bool
		notes int
	}{
		{name: "over_a_workspace_that_holds_an_unpushed_commit", work: true, notes: 1},
		{name: "control:_over_a_clean_workspace", notes: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundClean)
			if tc.work {
				rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
			}
			rig.answerRound(roundBreakingDrift, claim.RoleImplementer, claim.RoleTester)

			rig.awaitReplacement(1, "of a round over a drift that breaks the pod")
			for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
				rig.awaitClaim(role, "its round to end with the replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
			}
			if reads := rig.rt.CallsOf("WorkspaceWork"); len(reads) != 1 {
				t.Fatalf("the round read the workspace %d times, want once before the replacement", len(reads))
			}
			told := rig.architectNotices(record.WorkspaceTakenNotice)
			if len(told) != tc.notes || (tc.notes == 1 && !strings.Contains(told[0].Reason, passedOverCommit)) {
				t.Fatalf("the architect was told %+v, want %d workspace-taken notice(s), naming the commit %s", told, tc.notes, passedOverCommit)
			}
			if reason, _ := rig.keptReason(); reason != "" {
				t.Fatalf("the breaking round recorded a decline %q, want none: it kept no pod", reason)
			}
		})
	}
}
