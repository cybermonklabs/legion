package daemon

// The Legion reviewer's item 1 on #5285 at 69e00593: the bound's own line, `supervise pod update
// replaced at the bound`, whose words a production check reads, comes before the finish of the
// replacement's record, since the replacement is made by then (roundReplaced). The walk ends a
// round whose pod a launch replaced as the round's own replacement at each sweep until the store
// finishes that record (endRoundALaunchReplaced), with the launch's reading of the round's end each
// time, so a store that refuses the finish must not have the line written again at each try: one
// replacement, one line. The round's own line, which says the round is over, comes once, after the
// finish commits, and says the bound let the replacement through.

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func TestALaunchsReplacementAtTheBoundWritesTheBoundsLineOnceThoughItsFinishIsRefused(t *testing.T) {
	t.Parallel()
	// The tester, which the daemon released before the round's ask reached it, is launched past the
	// round's bound into the round's pod, which no role process runs in. The implementer holds the
	// round's ask and has not answered it, so the bound is what lets the replacement through. The
	// round is over a drift that breaks the pod, so the launcher's read, which finds a commit that
	// origin does not hold, keeps nothing.
	launching := releasedUnasked(claim.RoleTester)
	heldAsk := podlessMember(claim.RoleImplementer, supervise.StateLaunchUncertain, "", false)
	launching.reasons, heldAsk.reasons = roundBreakingDrift, roundBreakingDrift
	rig := newPodUpdateRig(t, launching, heldAsk)
	rig.drifts(roundBreakingDrift)
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
	rig.clockAt(podUpdateBound.Add(time.Minute))
	lift := rig.refuseWorkspaceTaken()
	if ends, err := rig.s.roundPodEndsAtLaunch(rig.ctx, rig.tokens[claim.RoleTester], podUpdatePod); err != nil || !ends {
		t.Fatalf("the launch's reading of the round = %t, %v; want the pod replaced", ends, err)
	}
	// The launch suspends the Sandbox over the round's pod, and the Sandbox holds no pod until the
	// launch's role comes up.
	rig.rt.ScriptIssuePod(podUpdateIssue, "")

	rig.wantRoundOpenWhileRefused()
	lift()
	rig.wantToldOnce("once the store takes the row")
	rig.wantRoundEnded()
	if bound := loggedLines(t, rig.logs.String(), podUpdateBoundLine); len(bound) != 1 {
		t.Fatalf("the bound's line was written %d times over %d refused finishes and the one the store took, want once: %+v", len(bound), roundSweeps, bound)
	}
	if over := loggedLines(t, rig.logs.String(), podUpdateReplacedLine); len(over) != 1 || over[0]["atTheBound"] != true || over[0]["atLaunch"] != true {
		t.Fatalf("the round's line was written %+v, want once, for a replacement at a launch that the bound let through", over)
	}
}
