package admit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// srMove is Dispatch's event of key moving to status at sequence seq, a person's.
func srMove(key, status string, seq int64) intake.DispatchIssue {
	return intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated", Status: status,
		Title: key, Rank: key, HandedOver: true}
}

// An issue Dispatch shows out of Legion's work stops where it stands and, moved back into work,
// returns to the phase it stood in at a new phase entry (R20 of dispatch://INFRA-3320, decision 9
// of dispatch://INFRA-4730): the same generation, the same sessions, the phase's worker started at
// the next entry with the phase's resume task, among the work that came back, and every other claim
// that has run started again in its own session. A parent in awaiting_children keeps the start it
// holds through the stop and the return, and starts no worker on its return (R13); nor does the
// return of an issue above a waiting parent start any role of that parent (R11).
func TestAnIssueBackInWorkResumesItsPhaseAtANewEntry(t *testing.T) {
	t.Run("an issue in implementing", func(t *testing.T) {
		const key = "LEGION-500"
		pool := migratedPool(t)
		admission := quiet(t, 2)
		spSeedParent(t, pool, key, phase.Implementing, true, func(issue *record.Issue) { issue.LastDispatchSeq = 1 })
		raised := fixedNow.Add(-time.Hour)
		spSeedPhase(t, pool, record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: "implementer", Entry: 3,
			EntryTask: "Continue " + key + ".", EntryRaisedAt: raised})
		putWorkerClaim(t, pool, key, key, claim.RoleImplementer, 1, supervise.StateIdle, "")
		putWorkerClaim(t, pool, key, key, claim.RoleArchitect, 1, supervise.StateIdle, "")

		apply(t, pool, admission, "to-backlog", srMove(key, "backlog", 2), admission.engine)

		stopped := issue(t, pool, key)
		if stopped.Phase != phase.Implementing || stopped.Generation != 1 || stopped.Status != "backlog" {
			t.Fatalf("the stopped issue = %#v, want it in implementing at generation 1, backlog", stopped)
		}
		if holdsSlot(t, pool, key) {
			t.Fatalf("the stopped issue still holds its slot")
		}
		if suspended := superviseOps(t, pool, key, "suspend"); suspended < 2 {
			t.Fatalf("the stop queued %d suspends, want one for each claim that has run", suspended)
		}
		startsBefore := len(pwWorkerStarts(t, pool, key))

		apply(t, pool, admission, "back-to-todo", srMove(key, "todo", 3), admission.engine)

		back := issue(t, pool, key)
		if back.Phase != phase.Implementing || back.Generation != 1 {
			t.Fatalf("the returned issue = %#v, want it back in implementing at generation 1", back)
		}
		if row := spPhaseRow(t, pool, key, claim.RoleImplementer); row.Entry != 4 {
			t.Fatalf("the implementer's entry after the return = %d, want 4: the return raises the phase's entry", row.Entry)
		}
		starts := pwWorkerStarts(t, pool, key)
		if len(starts) != startsBefore+1 || starts[len(starts)-1].role != claim.RoleImplementer {
			t.Fatalf("the worker starts after the return = %+v, want one more, the implementer's", starts)
		}
		// The start carries the task of the new entry, which names the return, and is no resume of
		// a task the claim holds (record.SuperviseRequest.ResumeTask): the entry the stop ended is
		// refused where it commits, so the claim is handed this entry's work.
		start := pnWorkerStart(t, pool, key, claim.RoleImplementer)
		if start.Entry != 4 || start.Generation != 1 || start.Phase != phase.Implementing || start.ResumeTask ||
			!strings.Contains(start.Task, "came back into Legion's work after a stop") {
			t.Fatalf("the implementer's start = %+v, want entry 4 of generation 1 in implementing on the return's task", start)
		}
		// The architect's claim comes back by a plain start of the phase the issue stands in, with
		// no task (workflow's reviveClaims), which the effects reader passes over, so the row is
		// counted where it stands.
		var revived int
		if err := pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'supervise'
			and payload->>'op' = 'start' and payload->>'role' = 'architect'`, key).Scan(&revived); err != nil {
			t.Fatal(err)
		}
		if revived != 1 {
			t.Fatalf("the return started %d architect claims, want the architect's session brought back once", revived)
		}
		if !holdsSlot(t, pool, key) {
			t.Fatalf("the returned issue holds no slot, want the one the line gives the work that came back")
		}
	})
	t.Run("a parent in awaiting_children keeps the start it holds", func(t *testing.T) {
		const key = "LEGION-510"
		pool := migratedPool(t)
		admission := quiet(t, 2)
		held := "Continue " + key + ". Issue: " + key + ". Phase: implementing."
		spWaiting(t, pool, key, phase.Implementing, held, "children of "+key+" were open", func(issue *record.Issue) { issue.LastDispatchSeq = 1 })
		seedMirrorChild(t, pool, "LEGION-511", "todo", key)

		apply(t, pool, admission, "to-backlog", srMove(key, "backlog", 2), admission.engine)
		apply(t, pool, admission, "back-to-todo", srMove(key, "todo", 3), admission.engine)

		back := issue(t, pool, key)
		if back.Phase != phase.AwaitingChildren || back.ChildrenWait == nil || back.ChildrenWait.Start != phase.Implementing || back.ChildrenWait.Task != held {
			t.Fatalf("the returned parent = phase %s, held start %+v; want awaiting_children holding the implementing start", back.Phase, back.ChildrenWait)
		}
		if starts := pwWorkerStarts(t, pool, key); len(starts) != 0 {
			t.Fatalf("the returned parent's worker starts = %+v, want none while its child is open", starts)
		}
	})
	t.Run("the return of an issue above a waiting parent starts no role of it", func(t *testing.T) {
		const above, key = "LEGION-530", "LEGION-531"
		pool := migratedPool(t)
		admission := quiet(t, 2)
		spSeedParent(t, pool, above, phase.Implementing, true, func(issue *record.Issue) { issue.LastDispatchSeq = 1 })
		held := "Continue " + key + ". Issue: " + key + ". Phase: implementing."
		spWaiting(t, pool, key, phase.Implementing, held, "children of "+key+" were open", func(issue *record.Issue) {
			issue.Tree, issue.Parent, issue.LastDispatchSeq = above, new(above), 1
		})
		seedMirrorChild(t, pool, "LEGION-532", "todo", key)

		apply(t, pool, admission, "above-to-backlog", srMove(above, "backlog", 2), admission.engine)
		apply(t, pool, admission, "above-back-to-todo", srMove(above, "todo", 3), admission.engine)

		// The issue above came back into work at the phase it stood in; its own open child, the
		// waiting parent, holds its start (R9), which is no part of this case.
		if back := issue(t, pool, above); !record.InLegionsWork(back) || back.Generation != 1 {
			t.Fatalf("the issue above after its return = phase %s, status %s, generation %d; want it back in Legion's work at generation 1: the case's premise",
				back.Phase, back.Status, back.Generation)
		}
		waiting := issue(t, pool, key)
		if waiting.Phase != phase.AwaitingChildren || waiting.ChildrenWait == nil || waiting.ChildrenWait.Task != held {
			t.Fatalf("the waiting parent = phase %s, held start %+v; want awaiting_children holding its start", waiting.Phase, waiting.ChildrenWait)
		}
		if starts := pwWorkerStarts(t, pool, key); len(starts) != 0 {
			t.Fatalf("the waiting parent's worker starts after the return above it = %+v, want none while its child is open", starts)
		}
	})
}

// A merged issue the daemon closed opens a new run when a person reopens it into work (decision 8
// of dispatch://INFRA-4730, gap F16): its generation goes up by one from the first phase, and the
// facts of the run that ended are cleared. A move of an issue the daemon closed from another status
// than done into work, such as R25's fix moving an issue from the backlog to in_progress, opens
// nothing, and a reopen of a closed issue whose pull request did not merge opens nothing either.
func TestAMergeClosedIssueReopenedOpensANewRun(t *testing.T) {
	seed := func(t *testing.T, status string, merged bool) (*Admission, func() record.Issue) {
		t.Helper()
		const key = "LEGION-520"
		pool := migratedPool(t)
		admission := quiet(t, 2)
		putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
			Title: key, Tree: key, Phase: phase.Done, Generation: 1, Status: status, DispatchStatus: status, Rank: key, HandedOver: true,
			LastDispatchSeq: 1, HoldsGate: true, TakenBack: true})
		state := record.PullRequestClosed
		if merged {
			state = record.PullRequestMerged
		}
		pwPullRequest(t, pool, key, state)
		apply(t, pool, admission, "reopened", srMove(key, "in_progress", 2), admission.engine)
		return admission, func() record.Issue { return issue(t, pool, key) }
	}
	t.Run("reopened from done with its pull request merged", func(t *testing.T) {
		_, got := seed(t, "done", true)
		again := got()
		if again.Generation != 2 || again.Phase != phase.Admitted || again.TakenBack {
			t.Fatalf("the reopened issue = %#v, want a new run: generation 2 at admitted with its run's mark cleared", again)
		}
	})
	t.Run("moved from the backlog into work", func(t *testing.T) {
		_, got := seed(t, "backlog", true)
		if again := got(); again.Generation != 1 || again.Phase != phase.Done {
			t.Fatalf("the issue moved from the backlog = %#v, want it left at phase done at generation 1", again)
		}
	})
	t.Run("reopened from done with its pull request not merged", func(t *testing.T) {
		_, got := seed(t, "done", false)
		if again := got(); again.Generation != 1 || again.Phase != phase.Done {
			t.Fatalf("the reopened issue = %#v, want it left at phase done at generation 1", again)
		}
	})
}

// An issue the release before left at phase done when it moved out of the workflow returns only on
// todo (decision 8 of dispatch://INFRA-4730): a move into any other status in work, R25's fix among
// them, leaves it out of Legion's work, and a todo opens a new run of it, which a handover queues
// and promotes as any root's first start.
func TestAnIssueTheOldReleaseLeftAtDoneReturnsOnlyOnTodo(t *testing.T) {
	const key = "LEGION-530"
	pool := migratedPool(t)
	admission := quiet(t, 2)
	putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: key, Tree: key, Phase: phase.Done, Generation: 3, Status: "backlog", DispatchStatus: "backlog", Rank: key, HandedOver: true,
		LastDispatchSeq: 1, HoldsGate: true})

	apply(t, pool, admission, "fix-in-progress", srMove(key, "in_progress", 2), admission.engine)

	if got := issue(t, pool, key); got.Generation != 3 || got.Phase != phase.Done || record.InLegionsWork(got) {
		t.Fatalf("the issue moved into in_progress = %#v, want it left at phase done, out of Legion's work", got)
	}
	if starts := startsOf(t, pool, key); len(starts) != 0 {
		t.Fatalf("starts of %s = %+v, want none", key, starts)
	}

	apply(t, pool, admission, "person-todo", srMove(key, "todo", 3), admission.engine)

	got := issue(t, pool, key)
	if got.Generation != 4 || got.Phase != phase.Admitted || got.Status != "in_progress" {
		t.Fatalf("the issue set to todo = %#v, want a new run at generation 4, admitted and promoted", got)
	}
	if starts := architectStarts(t, pool, key); starts != 1 {
		t.Fatalf("the new run started %d architects, want its promotion's one", starts)
	}
}
