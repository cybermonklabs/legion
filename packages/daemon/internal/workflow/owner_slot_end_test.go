package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// assertOwnersSlotCredentials fails unless each of rows is a supervise slot_credential of issue's
// owner, its own architect, in issue's tree at issue's generation.
func assertOwnersSlotCredentials(t *testing.T, rows []record.SuperviseRequest, issue record.Issue) {
	t.Helper()
	owner := claim.Token(treeRoleToken(t, issue.Key, claim.RoleArchitect))
	for _, row := range rows {
		if row.Claim != owner || row.Role != claim.RoleArchitect || row.Tree != issue.Key || row.Generation != issue.Generation {
			t.Fatalf("the slot_credential row %+v, want one of the owner %s, the architect, in the tree %s at generation %d", row, owner, issue.Key, issue.Generation)
		}
	}
}

// The end of a step its owner ran gives the owner's slot its own App back at once where the step
// acted as another App: on `implementer:owner tester:owner`, the gate's opening, which ends no step,
// queues no supervise slot_credential, and the owner's report of step 1 ends the owner's implementer
// step, which acts as the implement App, and queues one, of the owner's claim, the architect's role,
// in the issue's tree, though the owner's next step starts in the same move. The owner's report of
// step 2 ends its tester step, which acts as the review App, the owner's own (appauth.AppRoleFor),
// and queues none: its slot held its own App through that step.
func TestTheEndOfAnOwnersStepGivesItsSlotItsOwnApp(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:owner", "tester:owner")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	written := func(t *testing.T, want int) {
		t.Helper()
		rows := superviseRowsOf(t, pool, root.Key, "slot_credential")
		if len(rows) != want {
			t.Fatalf("%s's outbox holds the slot_credential rows %+v, want %d", root.Key, rows, want)
		}
		assertOwnersSlotCredentials(t, rows, root)
	}
	written(t, 0)

	ownerReportsStep1(t, pool, engine, root.Key)
	assertWalksAt(t, pool, engine, root.Key, phase.Testing, stepPlace(2))
	written(t, 1)

	owner := claim.Token(treeRoleToken(t, root.Key, claim.RoleArchitect))
	tested := ownersReport(root.Key, owner, claim.RoleTester, 2, "test-"+root.Key)
	tested.Verdict, tested.Entry = "pass", roleRowOf(t, pool, engine, root.Key, claim.RoleTester).Entry
	if result := applyFacts(t, pool, engine)("step-2", tested); result.Refusal != nil {
		t.Fatalf("the owner's report of step 2 was refused: %+v", result.Refusal)
	}
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase == phase.Testing {
		t.Fatalf("%s still stands in testing after the owner's report of its tester step: this case proves nothing", root.Key)
	}
	written(t, 1)
}

// Every end of an owner's step queues the write of the owner's slot back to its own App, the two
// ends the write-back once skipped among them. A child's step whose start named the architect
// above it (the child's own architect holds no claim, so the tree's architect runs the step,
// ownerAgent) ends, stopped by the owner's stop_task, with one slot_credential of the child's own
// architect, whose slot the owner-step wrote. A run whose plan rows the way back of a rollback
// dropped while the owner's step ran ends off its walk at the next settle (endOffWalk), with one
// slot_credential of the owner.
//
// The control: the end that queued a write-back only where the start named the issue's own
// architect, and the off-walk end that queued none, left both slots holding the step's App.
func TestEveryEndOfAnOwnersStepGivesItsSlotItsOwnApp(t *testing.T) {
	written := func(t *testing.T, pool *pgxpool.Pool, key string, owner claim.Token) {
		t.Helper()
		rows := superviseRowsOf(t, pool, key, "slot_credential")
		if len(rows) != 1 || rows[0].Claim != owner || rows[0].Role != claim.RoleArchitect {
			t.Fatalf("%s's outbox holds the slot_credential rows %+v, want one of its owner %s", key, rows, owner)
		}
	}

	t.Run("a child's step run by the architect above", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		child := childAtOwnersStep(t, pool, engine)
		above := treeRoleToken(t, "LEGION-208", claim.RoleArchitect)
		if starts := stepStartsAt(t, pool, child.Key, "step", 1); len(starts) != 1 || starts[0].Agent != above {
			t.Fatalf("the start records of the child's step 1 = %+v, want one naming the architect above, %s", starts, above)
		}
		stored := issueOf(t, pool, engine, child.Key)
		if result := applyFacts(t, pool, engine)("stop", ownersStop(t, stored, "a")); result.Refusal != nil {
			t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
		}
		written(t, pool, child.Key, claim.Token(treeRoleToken(t, child.Key, claim.RoleArchitect)))
	})

	t.Run("a run off its walk", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:owner", "tester")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		dropPlanRows(t, pool, root.Key)
		rebooted(t, pool)
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		if runs := stepRunsOf(t, pool, root.Key); len(runs) == 0 || runs[0] != "a step 1 1 ended" {
			t.Fatalf("the runs of %s = %v, want task a's step 1 ended off its walk", root.Key, runs)
		}
		written(t, pool, root.Key, claim.Token(treeRoleToken(t, root.Key, claim.RoleArchitect)))
	})
}

// The end of the owner's retro gives the owner's slot its own App back, as the end of each of its
// steps does: on `implementer:owner reviewer` the owner's retro is done as the implementer, whose
// step was the owner's (record.OwnerActsAs), so its slot holds the implement App through the
// retro, and the retro's end, the move to merging, queues one more supervise slot_credential of the
// owner's claim. It does so too where the way back of a rollback dropped the plan rows while the
// retro ran, so the retro ends off its walk (endOffWalk). The controls: on `implementer reviewer`
// the implementer does the retro, whose end queues none, since the owner's slot was never written
// for it; and on `acceptance:owner` alone the owner does the retro as itself, so its slot keeps
// its own App through the retro and the retro's end queues none either. Only the off-walk end of a
// retro, whose plan rows are gone, cannot tell the two retros of the owner apart, and queues the
// write-back for both (endOffWalk).
func TestTheEndOfTheOwnersRetroGivesItsSlotItsOwnApp(t *testing.T) {
	for _, tc := range []struct {
		name    string
		steps   []string
		dropped bool
		queued  int
	}{
		{name: "the owner's retro of its own implementer step", steps: []string{"implementer:owner", "reviewer"}, queued: 1},
		{name: "the same retro, its plan rows dropped while it ran", steps: []string{"implementer:owner", "reviewer"}, dropped: true, queued: 1},
		{name: "control: the retro of a fresh implementer", steps: []string{"implementer", "reviewer"}},
		{name: "control: the owner's retro of acceptance:owner alone", steps: []string{"acceptance:owner"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, tc.steps...)
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			if tc.steps[0] == "acceptance:owner" {
				assertWalksAt(t, pool, engine, root.Key, phase.Accepting, stepPlace(1))
				move(t, pool, engine, root.Key, TriggerSignOff, record.TaskPlace{}, record.PhaseRow{})
			} else {
				if tc.steps[0] == "implementer:owner" {
					ownerReportsStep1(t, pool, engine, root.Key)
				} else {
					move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
				}
				assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(2))
				move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
			}
			assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
			before := superviseRowsOf(t, pool, root.Key, "slot_credential")
			if tc.dropped {
				dropPlanRows(t, pool, root.Key)
				rebooted(t, pool)
			}
			move(t, pool, engine, root.Key, TriggerRetroCompleted, record.TaskPlace{}, record.PhaseRow{})
			if got := issueOf(t, pool, engine, root.Key); got.Phase != phase.Merging || got.Hold != nil {
				t.Fatalf("%s stands in %s (hold %+v) after its retro, want merging", root.Key, got.Phase, got.Hold)
			}
			rows := superviseRowsOf(t, pool, root.Key, "slot_credential")
			if len(rows) != len(before)+tc.queued {
				t.Fatalf("%s's outbox holds the slot_credential rows %+v after its retro ended, %+v before it, want %d more", root.Key, rows, before, tc.queued)
			}
			assertOwnersSlotCredentials(t, rows[len(before):], root)
		})
	}
}
