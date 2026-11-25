package daemon

// The Legion reviewer's item 2 on #5285 at 69e00593: the records of one act each hold the read of
// the workspace made before their own act, and the newest of them is not always the last read
// before the take. A replacement by issue reads outside the pod's launch turn and records before it
// waits for that turn, so its record, over a read that failed, can be newer than a launch's whose
// read through a launcher of the pod named commits. The one notice of the act tells the read that
// says most of what it took: a read that found work over one that failed, a read that failed over
// one that found nothing, and of reads that weigh the same the newest (tellingRead).

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func TestTheNoticeOfOneActTellsAnOlderReadThatFoundWorkOverANewerOneThatFailed(t *testing.T) {
	t.Parallel()
	rig := podlessBreakingRound(t)
	// A launch's own replacement of the round's pod records its act over a read that found
	// launchReadCommit, and the round's replacement by issue then records its own act on the same
	// pod over a read that failed, before its call waits for the launch turn.
	taken := movedFacetTaken(rig, "launch-pod-1", runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{launchReadCommit}, Count: 1}, nil)
	if err := rig.s.recordLaunchTakes(rig.ctx, taken); err != nil {
		t.Fatalf("record the launch's act: %v", err)
	}
	rig.recordRoundOverAFailedRead()

	// The launch takes the pod, and the daemon stops before either call says so.
	next := rig.restart()
	next.rt.ScriptSuspendedOver(podUpdatePod)
	next.rt.ScriptIssuePod(podUpdateIssue, "")
	next.awaitSwept(claim.RoleImplementer, "its round to end as the replacement its records name", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	next.wantToldOnceNaming("after the restart, from the older read, which found the commit", launchReadCommit)
	if records := next.replacementRecords(); records != 0 {
		t.Fatalf("the store holds %d records once the notice is queued, want none", records)
	}
}
