package daemon

// The Legion reviewer's Medium 2 on #5285 at 4b123279: a launch's finish that takes a round's
// record ends that round. The round's replacement by issue records its act on the round's pod and
// waits for the pod's launch turn, which a launch holds for its whole try, so the call fails as a
// call that ran out of time does, and its record stands. The launch then replaces the pod with an
// act of its own on it, and its finish (tellLaunchTook) takes the round's record with its own, by
// the target both name. The round ends there, as the act that took its pod, with no notice of its
// own: the launch's notice is the one word of the act, and no later sweep writes on the issue that
// the pod went before the round was over.

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func TestALaunchsFinishThatTakesARoundsRecordEndsThatRound(t *testing.T) {
	t.Parallel()
	rig := podlessBreakingRound(t)
	rig.rt.FailReplaceIssuePod(context.DeadlineExceeded)
	rig.tick()
	if asked := rig.byIssueAsks(); len(asked) != 1 {
		t.Fatalf("the replacement by issue was asked %+v, want once", asked)
	}
	if records := rig.replacementRecords(); records != 1 {
		t.Fatalf("the store holds %d records once the round's call failed, want the round's", records)
	}
	rig.rt.FailReplaceIssuePod(nil)

	// The launch's read through a launcher of the pod finds a commit; the launch records its own
	// replacement of the round's pod, replaces it, and hands the daemon what it took.
	taken := movedFacetTaken(rig, "launch-pod-1", runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{launchReadCommit}, Count: 1}, nil)
	if err := rig.s.recordLaunchTakes(rig.ctx, taken); err != nil {
		t.Fatalf("record the launch's act: %v", err)
	}
	rig.rt.ScriptIssuePod(podUpdateIssue, "")
	rig.s.tellLaunchTook(rig.ctx, taken)
	rig.awaitSwept(claim.RoleImplementer, "its round to end as the launch's act", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	for range roundSweeps {
		rig.tick()
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q, want nothing: the launch's act took the round's pod, and its notice is the one word of the act", messages)
	}
	if told := rig.architectNotices(record.WorkspaceTakenNotice); len(told) != 1 {
		t.Fatalf("the architect was told %+v, want one workspace-taken notice, the launch's", told)
	}
	if records := rig.replacementRecords(); records != 0 {
		t.Fatalf("the store holds %d records once the notice is queued, want none", records)
	}
}
