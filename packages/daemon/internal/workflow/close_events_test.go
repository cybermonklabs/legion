package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Each input of what a merged issue's close waits for has an event that judges the close again,
// one case for each row of the plan's table of events (D15): the production check's close and a
// defect's withdrawal among them.
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

	// A defect's withdrawal: the merger's completion is owed, so the reported defect starts no
	// change at the read, and the close waits for that defect alone. Its withdrawal closes the issue.
	t.Run("a defect is withdrawn", func(t *testing.T) {
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		owedMerger(t, rig)
		rig.mergeRoot()
		defect := rig.seedDefect(waitRoot, "The page is blank after a save.")
		rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		rig.stands(waitRoot, phase.Verifying, "testing")
		rig.noChange(waitRoot, waitGeneration)
		rig.withdraw(waitRoot, defect, "same as the merged fix")
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

// A read the backoff of a read that did not decide left waiting holds back every event that asks
// for one: the ask queues no second row, and the row it finds can be due an hour out. An event
// asks for a read now (askCloseRead), so the waiting row is made due now, and the issue still
// holds one read at a time. The control is the backoff's own ask, which is for a later read and
// moves no nearer row out.
func TestAnEventMakesAWaitingBackoffReadDueNow(t *testing.T) {
	t.Parallel()
	unplaced := func() record.CheckReadAnswer {
		return answerOf(waitRoot, nil, check("c-after", waitRootMerge, dispatch.PassStandsUnknown, dispatch.PassStandsReasonNotPlaced))
	}

	t.Run("an event pulls the waiting row to now", func(t *testing.T) {
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		rig.mergeRoot()
		rig.ranOut(waitRoot, "close_read")
		rig.read(waitRoot, unplaced())
		backoff := issueOutboxOf(t, rig.pool, waitRoot, "close_read")
		if len(backoff) != 1 || !backoff[0].nextAt.After(rig.now) {
			t.Fatalf("close_read rows after a check Dispatch could not place = %+v, want the one the backoff queued, due after %s", backoff, rig.now)
		}
		out := rig.dueAt(backoff[0].id, rig.now.Add(time.Hour))

		rig.fact(intake.DispatchResultChanged{Key: waitRoot, Seq: 11})

		rows := issueOutboxOf(t, rig.pool, waitRoot, "close_read")
		if len(rows) != 1 || rows[0].id != backoff[0].id {
			t.Fatalf("close_read rows after the result changed = %+v, want the backoff's own row %d alone", rows, backoff[0].id)
		}
		if !rows[0].nextAt.Equal(rig.now) {
			t.Fatalf("the read of %s is due %s, want %s: the event asks for a read now, and the backoff had left the row at %s",
				waitRoot, rows[0].nextAt, rig.now, out)
		}
	})

	// The control: the ask of the backoff is for a later read, over the row the entry queued, which
	// is due sooner. The row stands where it is.
	t.Run("a backoff ask leaves a nearer row where it is", func(t *testing.T) {
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		rig.mergeRoot()
		queued := issueOutboxOf(t, rig.pool, waitRoot, "close_read")
		if len(queued) != 1 {
			t.Fatalf("close_read rows after the entry into verifying = %+v, want the one it queued", queued)
		}

		rig.read(waitRoot, unplaced())

		rows := issueOutboxOf(t, rig.pool, waitRoot, "close_read")
		if len(rows) != 1 || rows[0].id != queued[0].id || !rows[0].nextAt.Equal(queued[0].nextAt) {
			t.Fatalf("close_read rows after the backoff's ask = %+v, want row %d still due at %s", rows, queued[0].id, queued[0].nextAt)
		}
	})
}

// dueAt stands outbox row id at at, as a backoff that reached its cap leaves it, and answers at.
func (r *waitRig) dueAt(id int64, at time.Time) time.Time {
	r.t.Helper()
	tag, err := r.pool.Exec(r.t.Context(), "update outbox set next_at = $2 where id = $1", id, at)
	if err != nil {
		r.t.Fatalf("stand outbox row %d at %s: %v", id, at, err)
	}
	if tag.RowsAffected() != 1 {
		r.t.Fatalf("stood %d outbox rows at %s, want the one row %d", tag.RowsAffected(), at, id)
	}
	return at
}
