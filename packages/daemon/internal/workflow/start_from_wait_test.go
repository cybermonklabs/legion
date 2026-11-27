package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The merger's move back of one task out of merging, whose implementer's step waits for its agent
// while that agent works another task's step, raises the implementer's entry from merging once the
// agent is free, as a start that waited for nothing does (keepStartFrom, startAfterWait). So an
// admission pass that finds an open Dispatch child leaves the parent in implementing
// (StopForChildren, acceptedEntry): R9 holds only the starts each from a phase before retro. Of a
// (merges), at its merge with #7 open, and b, at its first step, each `implementer:k` and so one
// agent k, with the Dispatch child LEGION-209 open: the merger moves a back to implementing, and
// a's step waits for k with nothing of it started, keeping merging as the phase its move left. b
// passes: a's step starts for k on an entry raised from merging and marked a move back, and the
// pass leaves LEGION-208 in implementing.
//
// The fault it catches is a start after a wait that names no phase its move left (its control:
// startAfterWait passing "" as from, the revision before, under which the implementer's entry
// names none and the pass moves LEGION-208 to awaiting_children).
func TestAMoveBackOutOfMergingWhoseStartWaitedForItsAgentIsLeftByThePass(t *testing.T) {
	pool := migratedPool(t)
	engine, _ := tickingEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskRunning, place: atMerge},
		planTask{id: "b", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskRunning, place: atStep(1)})
	markMerges(t, pool, root.Key, "a")
	seedTaskBranch(t, pool, root.Key, "a", "legion/"+root.Key+".a")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 7,
		Branch: "legion/" + root.Key + ".a", HeadSHA: "h7", Failing: []string{}})
	const child = "LEGION-209"
	seedMirrorChild(t, pool, child, "todo", root.Key)

	mustSpend(t, "the merger's move back", applyFacts(t, pool, engine)("merger-back", intake.BackwardMove{Issue: root.Key, Requester: claim.RoleMerger,
		To: phase.Implementing, Reason: "the merge queue refused the head", By: claim.Token(treeRoleToken(t, root.Key, claim.RoleMerger))}))

	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "a", atStep(1), record.StepWaitAgent)
	assertNothingStarted(t, pool, root.Key, "a", atStep(1))
	if got := tasks["a"].StartFrom; got != phase.Merging {
		t.Fatalf("task a waits for its agent keeping %q as the phase its move left, want merging", got)
	}

	taskMoves(t, pool, engine, root.Key, "b", TriggerImplementationReady, "")
	joinsEnd(t, pool, engine, root.Key)

	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "b", record.TaskDone, atStep(1))
	assertWaits(t, tasks, "a", atStep(1), record.StepWaitNone)
	assertStartOf(t, newestStart(t, pool, root.Key, claim.RoleImplementer), root.Key, "a", "k")
	if row := storedPhaseRow(t, pool, root.Key, claim.RoleImplementer); row.EntryFrom != phase.Merging || !row.EntryMovedBack {
		t.Fatalf("the implementer's phases row after a's start = %+v, want its entry raised by the move back out of merging", row)
	}

	var moved bool
	onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) (err error) {
		moved, err = engine.StopForChildren(ctx, tx, issue, []record.OpenChild{{Key: child, Status: "todo"}}, nil)
		return err
	})
	if moved {
		t.Fatalf("the admission pass moved %s to awaiting_children, want it left: its implementer's entry was raised by a move back out of merging", root.Key)
	}
	if got := issueAt(t, pool, root.Key); got != phase.Implementing {
		t.Fatalf("%s stands in %s after the pass, want implementing", root.Key, got)
	}
}
