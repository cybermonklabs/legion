package daemon

// GPT-6.1 Sol's point 3 on #5285 at fa9deb04 (the Legion reviewer's point C), which the spec's
// architect ruled a fix inside R7: each destructive act has a record of its own, keyed by an
// identity of its own and never by the pod's uid, and no record is written over. A round over a
// drift that breaks the pod replaces the pod over a read that found a commit origin does not hold,
// and the store refuses its notice, so the round's record stands. The issue's next launch then
// replaces the issue's Sandbox and the kept volume it owns, for the volume's reset, and records that
// act of its own, naming the same pod as the last to use the volume. The round's record must
// survive it: once the store takes notices again, the architect is told the commit the round's
// read found, once.

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestALaunchsRecordOfTheSamePodLeavesARoundsRecordWhole(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
	lift := rig.refuseWorkspaceTaken()
	rig.answerRound(roundBreakingDrift, claim.RoleImplementer, claim.RoleTester)
	rig.awaitReplacement(1, "of a round over a drift that breaks the pod")
	if records := rig.replacementRecords(); records != 1 {
		t.Fatalf("the store holds %d records once the round's notice was refused, want the round's one", records)
	}

	if err := rig.s.recordLaunchTakes(rig.ctx, volumeResetTaken(rig, "launch-reset-1", podUpdatePod)); err != nil {
		t.Fatalf("record the launch's replacement of the Sandbox and its volume: %v", err)
	}
	if records := rig.replacementRecords(); records != 2 {
		t.Fatalf("the store holds %d records once the launch recorded its own act, want the round's and the launch's", records)
	}

	lift()
	for range roundSweeps {
		rig.tick()
	}
	var round int
	for _, notice := range rig.architectNotices(record.WorkspaceTakenNotice) {
		if strings.Contains(notice.Reason, passedOverCommit) {
			round++
		}
	}
	if round != 1 {
		t.Fatalf("the architect was told %+v, want one workspace-taken notice naming the commit %s the round's read found",
			rig.architectNotices(record.WorkspaceTakenNotice), passedOverCommit)
	}
}
