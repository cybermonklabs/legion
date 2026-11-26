package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Each input of what a merged issue's close waits for has an event that judges the close again,
// one case for each row of the plan's table of events (D15) that this part's merge builds. The
// production check's close and a defect's withdrawal are the production check's and the next
// change's cases.
func TestEachEventJudgesTheCloseAgain(t *testing.T) {
	t.Parallel()
	t.Run("an issue beneath leaves", func(t *testing.T) {
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		rig.seedChild(waitChild, phase.Implementing, "in_progress")
		rig.mergeRoot()
		rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "in_progress"}}, passing("c-after", waitRootMerge)))
		rig.ranOut(waitRoot, "close_read")
		rig.fact(intake.DispatchIssue{Key: waitChild, Seq: 2, Type: "issue.closed", Status: "done", Title: waitChild, Parent: waitRoot, Rank: "V"})
		readsQueued(t, rig, 1)
		rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "done"}}, passing("c-after", waitRootMerge)))
		closedOnChecks(t, rig, waitRoot)
	})

	// The echo of the daemon's own status write of a child that stays in the workflow: no leave, so
	// only the Dispatch event's own read wakes the root above it.
	t.Run("a recorded Dispatch child moves", func(t *testing.T) {
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		rig.seedChild(waitChild, phase.Implementing, "in_progress")
		if _, err := rig.pool.Exec(t.Context(), "update issues set dispatch_status = 'todo' where key = $1", waitChild); err != nil {
			t.Fatalf("record the status Dispatch showed before the daemon's write: %v", err)
		}
		rig.mergeRoot()
		rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "todo"}}, passing("c-after", waitRootMerge)))
		rig.stands(waitRoot, phase.Verifying, "testing")
		rig.ranOut(waitRoot, "close_read")
		rig.fact(intake.DispatchIssue{Key: waitChild, ResolvedRepo: waitRepo, Seq: 2, Type: "issue.updated", Status: "in_progress", Title: waitChild, Parent: waitRoot, Rank: "V"})
		if got := rig.issue(waitChild).Phase; got != phase.Implementing {
			t.Fatalf("the child stands at %s after the echo, want implementing: the fixture's own premise", got)
		}
		readsQueued(t, rig, 1)
		rig.stands(waitRoot, phase.Verifying, "testing")
	})

	t.Run("an unrecorded filed child, at the controller's tick", func(t *testing.T) {
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		rig.mergeRoot()
		rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: "LEGION-300", Status: "backlog"}}, passing("c-after", waitRootMerge)))
		rig.stands(waitRoot, phase.Verifying, "testing")
		rig.ranOut(waitRoot, "close_read")
		rig.fact(intake.ControllerTick{})
		readsQueued(t, rig, 1)
		rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: "LEGION-300", Status: "icebox"}}, passing("c-after", waitRootMerge)))
		closedOnChecks(t, rig, waitRoot)
	})

	// Each result event, a pull request's move and a deploy queue one read. The project runs a
	// production check that no read opens before the production check's own change, so the issue
	// stays in verifying through every read here.
	t.Run("a check's state and the current deploy", func(t *testing.T) {
		rig := newWaitRig(t, claim.RoleTester)
		rig.seedRoot(phase.Merging, "retro")
		rig.mergeRoot()
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
		seq := int64(10)
		for _, event := range []intake.DispatchResultChanged{
			{Key: waitRoot, ResultID: "result-1", Check: "c-after", Verdict: "pass"},
			{Key: waitRoot, ResultID: "result-1", Check: "c-after"},
			{Key: waitRoot},
			{Key: waitRoot},
		} {
			seq++
			event.Seq = seq
			rig.ranOut(waitRoot, "close_read")
			rig.fact(event)
			readsQueued(t, rig, 1)
			rig.stands(waitRoot, phase.Verifying, "testing")
		}
		rig.ranOut(waitRoot, "close_read")
		rig.fact(intake.DispatchResultChanged{Key: waitRoot, Seq: seq + 1})
		rig.fact(intake.DispatchResultChanged{Key: waitRoot, Seq: seq + 2})
		readsQueued(t, rig, 1)
	})

	t.Run("a version and an approval of the Verification document", func(t *testing.T) {
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		rig.mergeRoot()
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
		for _, kind := range []intake.DispatchArtifactKind{intake.DispatchArtifactVersion, intake.DispatchArtifactApproved} {
			rig.ranOut(waitRoot, "close_read")
			rig.fact(intake.DispatchArtifact{Key: waitRoot, ArtifactID: waitDocument, Kind: kind, Version: 2, Mark: record.EventMark{Seq: int64(len(kind))}})
			readsQueued(t, rig, 1)
		}
		rig.ranOut(waitRoot, "close_read")
		rig.fact(intake.DispatchArtifact{Key: waitRoot, ArtifactID: "close-wait-md-208", Kind: intake.DispatchArtifactVersion, Version: 3, Mark: record.EventMark{Seq: 30}})
		readsQueued(t, rig, 0)
	})

	t.Run("a read that was stale, then a fresh one", func(t *testing.T) {
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		rig.mergeRoot()
		rig.ranOut(waitRoot, "close_read")
		rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", "")))
		rig.stands(waitRoot, phase.Verifying, "testing")
		if rows := issueOutboxOf(t, rig.pool, waitRoot, "close_read"); len(rows) != 1 || !rows[0].nextAt.After(rig.now) {
			t.Fatalf("close_read rows after a stale read = %+v, want one due later", rows)
		}
		rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		closedOnChecks(t, rig, waitRoot)
	})

	// A child's record left the workflow while the event that would have queued its root's read was
	// lost: the tick reads the root again and judges it.
	t.Run("any of them missed, at the controller's tick", func(t *testing.T) {
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		rig.seedChild(waitChild, phase.Implementing, "in_progress")
		rig.mergeRoot()
		rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "in_progress"}}, passing("c-after", waitRootMerge)))
		rig.ranOut(waitRoot, "close_read")
		child := rig.issue(waitChild)
		child.Phase, child.Status, child.DispatchStatus = phase.Done, "done", "done"
		seedIssue(t, rig.pool, child)
		rig.fact(intake.ControllerTick{})
		readsQueued(t, rig, 1)
		rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "done"}}, passing("c-after", waitRootMerge)))
		closedOnChecks(t, rig, waitRoot)
	})

	// The production check's close: a pass at its look closes it recorded, and that close queues
	// the read that closes the issue.
	t.Run("the production check closes", func(t *testing.T) {
		rig := newWaitRig(t, claim.RoleTester)
		rig.seedRoot(phase.Merging, "retro")
		rig.mergeRoot()
		rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		rig.stands(waitRoot, phase.Verifying, "testing")
		rig.ranOut(waitRoot, "close_read")
		rig.report(waitRoot, 1, "pass", "the check passed in production", nil)
		readsQueued(t, rig, 1)
		rig.stands(waitRoot, phase.Verifying, "testing")
		rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		closedOnChecks(t, rig, waitRoot)
	})
}

// readsQueued fails the test unless the root holds want close_read rows.
func readsQueued(t *testing.T, rig *waitRig, want int) {
	t.Helper()
	if got := rig.rows(waitRoot, "close_read"); got != want {
		t.Fatalf("the root's close_read rows = %d, want %d", got, want)
	}
}
