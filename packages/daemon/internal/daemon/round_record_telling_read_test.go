package daemon

// The Legion reviewer's item 2 on #5285 at 69e00593: the records of one act each hold the read of
// the workspace made before their own act, and the newest of them is not always the last read
// before the take. A replacement by issue reads outside the pod's launch turn and records before it
// waits for that turn, so its record, over a read that failed, can be newer than a launch's whose
// read through a launcher of the pod named commits. The one notice of the act tells the read that
// says most of what it took: a read that found work over one that failed, a read that failed over
// one that found nothing, and of reads that weigh the same the newest (tellingRecord).
//
// The Legion reviewer's thread on podkeep.go:381 at 9d5f02d6: a round over a drift the roles can
// keep working under replaces its pod only over its own clean read (roundKeeps), so where that
// round's finish tells of work, the read it tells of is another act's on the same pod, which the
// finish took with its own. Its notice says so, and does not give a drift that breaks the pod as
// the reason (workspaceTakenMessage).

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
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

// keepingDriftTold is the sentence of the workspace-taken notice of a round over a drift the roles
// can keep working under (workspaceTakenMessage): its own read was clean, and the read the notice
// tells of is another act's on the same pod.
const keepingDriftTold = "The round was over a drift that the roles can keep working under. The read that the round made found nothing that is not on a branch at origin, so the round replaced the pod. The read that this notice tells of was made for another act on the same pod."

func TestARoundOverADriftTheRolesKeepWorkingUnderTellsAnotherActsReadThatFoundWork(t *testing.T) {
	t.Parallel()
	// The round is over a drift the roles can keep working under, unasked, with no process in the
	// pod, and its read finds nothing origin lacks, so it replaces the pod by issue.
	rig := newPodUpdateRig(t, podUpdateRole{role: claim.RoleImplementer, state: supervise.StateSuspended, session: true,
		contract: api.DaemonAPIVersion(), suspendedBy: supervise.SuspendedByDaemon, round: podUpdateRound, reasons: podUpdateDrift, unasked: true})
	rig.drifts(podUpdateDrift)
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundClean)
	// A launch's own replacement of the round's pod records its act over a read that found
	// launchReadCommit, and the round then records its replacement by issue of the same pod over its
	// own clean read, so the round's record is the newer.
	taken := movedFacetTaken(rig, "launch-pod-1", runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{launchReadCommit}, Count: 1}, nil)
	if err := rig.s.recordLaunchTakes(rig.ctx, taken); err != nil {
		t.Fatalf("record the launch's act: %v", err)
	}
	read := workspaceRead{round: podUpdateRound, by: viaIssue, at: rig.clock.Now(), work: roundClean}
	if _, ok := rig.s.recordRoundReplacement(rig.ctx, podUpdateIssue, podUpdateRound, podUpdatePod, podUpdateDrift, read); !ok {
		t.Fatal("record the round's replacement by issue")
	}

	// The pod is taken, and the daemon stops before either call says so. The sweep after the
	// restart finishes the newer record first, the round's, which takes the launch's with it.
	next := rig.restart()
	next.rt.ScriptSuspendedOver(podUpdatePod)
	next.rt.ScriptIssuePod(podUpdateIssue, "")
	next.awaitSwept(claim.RoleImplementer, "its round to end as the replacement its records name", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	next.wantToldOnceNaming("after the restart, from the launch's read, which found the commit", launchReadCommit)
	reason := next.architectNotices(record.WorkspaceTakenNotice)[0].Reason
	if !strings.Contains(reason, keepingDriftTold) || strings.Contains(reason, "breaks the pod") {
		t.Fatalf("the architect was told %q, want the round's notice of a drift the roles can keep working under, which says that the read it tells of is another act's on the same pod and names no drift that breaks the pod", reason)
	}
	if records := next.replacementRecords(); records != 0 {
		t.Fatalf("the store holds %d records once the notice is queued, want none", records)
	}
}
