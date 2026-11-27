package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// retroBackStop moves task w, `implementer:k tester`, the task that merges, back out of its retro
// to its first step on LEGION-208, as its implementer does when the retro finds a defect, and then
// runs the admission pass's stop with an open Dispatch child of LEGION-208 (StopForChildren). Task
// m, `implementer:k`, which w needs, holds k at its step where busy, and w's step waits for k until
// m passes; else m is done and w's step starts at once. It answers the implementer's phases row
// once w's step has started, whether the pass moved LEGION-208, and LEGION-208 after the pass.
func retroBackStop(t *testing.T, busy bool) (record.PhaseRow, bool, record.Issue) {
	t.Helper()
	pool := migratedPool(t)
	engine, _ := tickingEngine()
	root := stepRoot(t, pool, phase.Retro)
	m := planTask{id: "m", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskDone, place: atStep(1)}
	if busy {
		m.state = record.TaskRunning
	}
	seedTasks(t, pool, root.Key, root.Generation, m,
		planTask{id: "w", steps: []string{"implementer:k", "tester"}, agents: []claim.Agent{"k", "tester"}, needs: []string{"m"},
			state: record.TaskRunning, place: record.TaskPlace{Kind: record.PlaceRetro}})
	seedTaskBranch(t, pool, root.Key, "w", "legion/"+root.Key+".w")
	seedTaskPR(t, pool, engine, root.Key, "w", 45)

	onTask(t, pool, engine, root.Key, "w", func(ctx context.Context, tx pgx.Tx, s TaskScope) error {
		pr, err := engine.taskPullRequest(ctx, tx, s)
		if err != nil {
			return err
		}
		row, err := engine.phaseRow(ctx, tx, root.Key, claim.RoleImplementer)
		if err != nil {
			return err
		}
		return engine.transition(ctx, tx, s, TriggerBackward, atStep(1), row, pr, "the retro found a defect to fix")
	})
	if busy {
		assertWaits(t, tasksOf(t, pool, engine, root.Key), "w", atStep(1), record.StepWaitAgent)
		assertNothingStarted(t, pool, root.Key, "w", atStep(1))
		taskMoves(t, pool, engine, root.Key, "m", TriggerImplementationReady, "")
		joinsEnd(t, pool, engine, root.Key)
	}
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "w", atStep(1), record.StepWaitNone)
	assertStartOf(t, newestStart(t, pool, root.Key, claim.RoleImplementer), root.Key, "w", "k")
	row := storedPhaseRow(t, pool, root.Key, claim.RoleImplementer)

	var moved bool
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, root.Key)
		if err != nil {
			return err
		}
		moved, err = engine.StopForChildren(ctx, tx, *issue, []record.OpenChild{{Key: "LEGION-209", Status: "todo"}}, nil)
		return err
	})
	return row, moved, issueOf(t, pool, engine, root.Key)
}

// A move back out of retro whose step waits for its agent raises, once the agent is free, the
// entry its start would have raised at once: an entry from retro (record.PhaseRow.EntryFrom), whose
// start an open Dispatch child does not hold (dispatch://INFRA-3320 R9, acceptedEntry). Task w, the
// task that merges, stands in its retro; its implementer moves it back to its first step, whose
// agent k does task m's step, so it waits for k; m passes, and w's step starts for k. The
// admission pass with an open child of the parent then leaves the parent in implementing, as it
// leaves it after the same move back where k was free.
//
// The fault it catches is a start after a wait that names no phase its move left (startAfterWait
// passing none), so the pass stops the implementer's phase and moves the parent to
// awaiting_children, holding a start R9 never holds (its control: the same move back where k is
// free, whose start raises the entry from retro at once and which the pass leaves).
func TestAMoveBackOutOfRetroWhoseStepWaitedForItsAgentIsNoStartAnOpenChildHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		busy bool
	}{
		{"its agent free (the control)", false},
		{"its agent busy with another task's step", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, moved, after := retroBackStop(t, tc.busy)
			if moved || after.Phase != phase.Implementing {
				t.Fatalf("the admission pass with an open child moved %t, and LEGION-208 stands in %s after it, want it left in implementing:"+
					" the implementer's entry after the move back out of retro is from %q (moved back %t), want from retro, which R9 does not hold",
					moved, after.Phase, row.EntryFrom, row.EntryMovedBack)
			}
			if row.EntryFrom != phase.Retro || !row.EntryMovedBack {
				t.Fatalf("the implementer's entry after the move back out of retro is from %q (moved back %t), want from retro and moved back",
					row.EntryFrom, row.EntryMovedBack)
			}
		})
	}
}
