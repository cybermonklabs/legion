package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ownerSteps is every owner-step notice key's outbox holds, oldest first.
func ownerSteps(t *testing.T, pool *pgxpool.Pool, key string) []record.Notice {
	t.Helper()
	return noticesOf(t, pool, key, record.OwnerStepNotice)
}

// assertNoClaimStarts fails when key's outbox holds a start of any claim: an owner's step is told
// to the owner, never started as a worker.
func assertNoClaimStarts(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	if starts := superviseRowsOf(t, pool, key, "start"); len(starts) != 0 {
		t.Fatalf("%s's outbox holds the starts %+v, want none: the owner's step starts no claim", key, starts)
	}
}

// stepStartsAt is each start record queued for key's step of kind at place, in order: one for
// each start of the place, over every run of it.
func stepStartsAt(t *testing.T, pool *pgxpool.Pool, key, kind string, place int) []record.StepRecord {
	t.Helper()
	var starts []record.StepRecord
	for _, write := range writesAt(stepWrites(t, pool, key), kind, place) {
		if isStart(write) {
			starts = append(starts, write)
		}
	}
	return starts
}

// A step or a retro whose agent is the owner of its task is told to the owner, the architect that
// owns the issue (here the root's own), as one notice owner-step that carries the step's task, from
// each path that starts a step: a transition (the gate's opening into `implementer:owner`), a task
// started again after its stop (restartTask), the retro of a parent held for its child
// (startHeldRetro) and the move into the owner's acceptance of the default steps. Each writes the
// run's one start record under the owner's claim and the entry on the step's phases row, and
// starts no claim. An issue with no plan rows at the move into accepting writes today's start record
// under the architect alone, raises no entry and tells no owner-step: the reviewer's phase-finished
// tells it, as today. The operator's retry of the owner's claim at its step (restartPhase) tells the
// owner its step again with the retry's reason once the owner-step it was told is published, and
// starts no second run, as a worker's retry hands it its entry again; while an owner-step of the
// step still waits in the outbox, which names the same run, the retry tells nothing more, so the
// owner hears its step once. (A re-admission's promotion is admit's
// TestATreeAdmittedAgainTellsTheOwnerItsStep.)
//
// The controls: start as it was (the caller's role decides; the architect starts nothing) starts the
// implementer and tells no owner-step in the first three paths, and writes no start record at the
// two moves into accepting; the owner's branch taken only where the caller passes the architect
// starts the implementer at the first and a claim of acceptance at the fourth; restart as it was
// (the architect's phase starts nobody) tells nothing at the retry; the retry that told the owner
// whatever stood queued writes a second owner-step of the same run beside a queued one.
func TestAnOwnerStepIsToldToTheOwnerFromEachStartPath(t *testing.T) {
	owner := treeRoleToken(t, "LEGION-208", claim.RoleArchitect)

	t.Run("a transition", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:owner", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})

		notices := ownerSteps(t, pool, root.Key)
		if len(notices) != 1 || notices[0].Role != claim.RoleImplementer || notices[0].Phase != phase.Implementing {
			t.Fatalf("the gate's opening told %+v, want one owner-step of the implementer's step in implementing", notices)
		}
		if step := notices[0].OwnerStep; step == nil || step.Task != "a" || step.Kind != "step" || step.Place != 1 {
			t.Fatalf("the owner-step names the run %+v, want task a's step 1", notices[0].OwnerStep)
		}
		assertNoClaimStarts(t, pool, root.Key)
		if starts := stepStartsAt(t, pool, root.Key, "step", 1); len(starts) != 1 || starts[0].Agent != owner {
			t.Fatalf("the start records of step 1 = %+v, want one naming the owner %s", starts, owner)
		}
		if row := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer); row.Claim != claim.Token(owner) || row.Entry != 1 || row.StepPlace != 1 {
			t.Fatalf("the implementer's row = %+v, want the owner's claim at entry 1 of step 1", row)
		}
	})

	t.Run("a task started again after its stop", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:owner", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		if result := applyFacts(t, pool, engine)("stop", ownersStop(t, root, "a")); result.Refusal != nil {
			t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
		}
		if result := applyFacts(t, pool, engine)("todo", personsTodo(root.Key)); result.Refusal != nil {
			t.Fatalf("the todo was refused: %+v", result.Refusal)
		}

		if notices := ownerSteps(t, pool, root.Key); len(notices) != 2 || notices[1].Role != claim.RoleImplementer {
			t.Fatalf("the restart told %+v, want a second owner-step of step 1", notices)
		}
		if starts := stepStartsAt(t, pool, root.Key, "step", 1); len(starts) != 2 || starts[1].Agent != owner {
			t.Fatalf("the start records of step 1 = %+v, want two, the second naming the owner", starts)
		}
		if starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(starts) != 0 {
			t.Fatalf("the restart wrote the implementer's starts %+v, want none", starts)
		}
	})

	t.Run("the retro of a parent held for its child", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Testing)
		stepChild(t, pool, heldParentChild, phase.Implementing, nil)
		seedStepsAt(t, pool, root.Key, root.Generation, 1, nil, "tester")
		seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleTester, Claim: "tester-claim", HandoffCommit: stepHead, Verdict: "pass",
			StepTask: "a", StepPlace: 1})
		move(t, pool, engine, root.Key, TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
		if held := issueOf(t, pool, engine, root.Key); !record.HeldForChildren(held) {
			t.Fatalf("%s after the tester's pass stands in %s with hold %+v, want held before its retro", root.Key, held.Phase, held.Hold)
		}
		childLeavesDone(t, pool, engine)

		notices := ownerSteps(t, pool, root.Key)
		if len(notices) != 1 || notices[0].Role != "" || notices[0].Phase != phase.Retro {
			t.Fatalf("the child's leave told %+v, want one owner-step of the retro", notices)
		}
		if step := notices[0].OwnerStep; step == nil || step.Task != "a" || step.Kind != "retro" || step.Place != 0 {
			t.Fatalf("the retro's owner-step names the run %+v, want task a's retro", notices[0].OwnerStep)
		}
		if row := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer); row.Claim != claim.Token(owner) || row.StepPlace != 0 || row.Entry == 0 {
			t.Fatalf("the implementer's row = %+v, want the owner's retro on it at place 0", row)
		}
		if starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(starts) != 0 {
			t.Fatalf("the retro wrote the implementer's starts %+v, want none: the owner does it", starts)
		}
	})

	t.Run("the owner's acceptance of the default steps", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Reviewing)
		seedStepsAt(t, pool, root.Key, root.Generation, 3, nil, "implementer", "tester", "reviewer", "acceptance:owner")
		move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})

		if notices := ownerSteps(t, pool, root.Key); len(notices) != 1 || notices[0].Role != claim.RoleAcceptance || notices[0].Phase != phase.Accepting {
			t.Fatalf("the approval told %+v, want one owner-step of the acceptance", notices)
		}
		if starts := stepStartsAt(t, pool, root.Key, "step", 4); len(starts) != 1 || starts[0].Agent != owner {
			t.Fatalf("the start records of step 4 = %+v, want one naming the owner", starts)
		}
		if starts := roleStartsOf(t, pool, root.Key, claim.RoleAcceptance); len(starts) != 0 {
			t.Fatalf("the approval wrote starts of the role acceptance %+v, want none", starts)
		}
	})

	t.Run("an issue with no plan rows at accepting", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Reviewing)
		move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})

		if notices := ownerSteps(t, pool, root.Key); len(notices) != 0 {
			t.Fatalf("the approval told %+v, want no owner-step on an issue with no plan rows", notices)
		}
		if starts := stepStartsAt(t, pool, root.Key, "step", 5); len(starts) != 1 || starts[0].Agent != owner {
			t.Fatalf("the start records of step 5 = %+v, want one naming the architect %s", starts, owner)
		}
		if row := roleRowOf(t, pool, engine, root.Key, claim.RoleArchitect); row.Entry != 0 {
			t.Fatalf("the architect's row = %+v, want no entry raised", row)
		}
		if notices := noticesOf(t, pool, root.Key, "phase-finished"); len(notices) != 1 {
			t.Fatalf("the approval told %d phase-finished notices, want the reviewer's one", len(notices))
		}
	})

	// retried is LEGION-208 at the owner's implementer step of `implementer:owner reviewer` after
	// the operator's retry of the owner's claim, with the gate's owner-step published (deleted from
	// the outbox, as a published row is) first where published is true.
	retried := func(t *testing.T, published bool) (*pgxpool.Pool, record.Issue) {
		t.Helper()
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:owner", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		if published {
			if _, err := pool.Exec(t.Context(), `delete from outbox where kind = 'notice' and issue = $1 and payload->>'kind' = $2`,
				root.Key, string(record.OwnerStepNotice)); err != nil {
				t.Fatalf("publish the gate's owner-step: %v", err)
			}
		}
		if result := applyFacts(t, pool, engine)("retry", intake.RestartPhase{Issue: root.Key, Role: claim.RoleArchitect}); result.Refusal != nil {
			t.Fatalf("the operator's retry of the owner was refused: %+v", result.Refusal)
		}
		if starts := stepStartsAt(t, pool, root.Key, "step", 1); len(starts) != 1 {
			t.Fatalf("the start records of step 1 = %+v, want the one of the gate's opening: a retry starts no run", starts)
		}
		assertNoClaimStarts(t, pool, root.Key)
		return pool, root
	}

	t.Run("the operator's retry of the owner's claim at its step", func(t *testing.T) {
		pool, root := retried(t, true)
		notices := ownerSteps(t, pool, root.Key)
		if len(notices) != 1 || notices[0].Role != claim.RoleImplementer || notices[0].Phase != phase.Implementing {
			t.Fatalf("the retry told %+v, want one owner-step of the implementer's step", notices)
		}
		if step := notices[0].OwnerStep; step == nil || step.Task != "a" || step.Kind != "step" || step.Place != 1 {
			t.Fatalf("the retry's owner-step names the run %+v, want task a's step 1", notices[0].OwnerStep)
		}
	})

	t.Run("the operator's retry of the owner's claim while its owner-step is still queued", func(t *testing.T) {
		pool, root := retried(t, false)
		notices := ownerSteps(t, pool, root.Key)
		if len(notices) != 1 {
			t.Fatalf("the owner-steps after the retry = %+v, want the gate's one alone: the queued row already tells the run's step", notices)
		}
	})
}

// ownersReport is the owner's report of step of key's task a, as the route complete_step applies
// it (intake.HandoffComplete): role, the role of the step's place, under claimed at the entry the
// start raised, with a handoff record of the implementer's phase.
func ownersReport(key string, claimed claim.Token, role claim.Role, step int, handoff string) intake.HandoffComplete {
	return intake.HandoffComplete{Issue: key, Role: role, Claim: claimed, Generation: 1, Entry: 1, Step: step,
		Summary: "implemented", Commit: stepHead, HandoffRecord: handoff, HandoffVersion: 1}
}

// ownerReportsStep1 applies the owner's report of step 1 of key's task a through engine, under the
// claim of key's own architect, failing the test on a refusal.
func ownerReportsStep1(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	owner := claim.Token(treeRoleToken(t, key, claim.RoleArchitect))
	if result := applyFacts(t, pool, engine)("step-1", ownersReport(key, owner, claim.RoleImplementer, 1, "implement-"+key)); result.Refusal != nil {
		t.Fatalf("the owner's report of step 1 was refused: %+v", result.Refusal)
	}
}

// The engine takes the owner's report of a step only for the step that runs, read in the fact's
// own transaction: on `implementer:owner tester:owner`, the owner's report of step 1 moves the
// task to step 2, the tester's step, which the owner does too; the same report of step 1 sent
// again while the task stands at step 2 is refused OWNER_STEP_NOT_RUNNING, naming the place the
// task stands at and its agent, and step 1's row keeps the handoff the first report named. At
// step 1, the report of a fresh implementer's claim is not the owner's, so it moves nothing and
// the row keeps the owner's claim.
//
// The controls: the engine without its check records the second report of step 1 as a
// completion of a phase left, or refuses it as one already reported; the rule of the role that
// works the place alone (completionOf) moves the fresh implementer's report to step 2.
func TestTheEngineTakesTheOwnersReportOnlyForTheStepThatRuns(t *testing.T) {
	owner := claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleArchitect))

	t.Run("the owner's report of the step that runs, then of a step that ran", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:owner", "tester:owner")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		apply := applyFacts(t, pool, engine)

		ownerReportsStep1(t, pool, engine, root.Key)
		assertWalksAt(t, pool, engine, root.Key, phase.Testing, stepPlace(2))

		result := apply("step-1-again", ownersReport(root.Key, owner, claim.RoleImplementer, 1, "implement-LEGION-208-again"))
		if result.Refusal == nil || result.Refusal.Code != "OWNER_STEP_NOT_RUNNING" {
			t.Fatalf("the owner's report of step 1 at step 2 = %+v, want OWNER_STEP_NOT_RUNNING", result.Refusal)
		}
		if row := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer); row.LastHandoff != "implement-LEGION-208" {
			t.Fatalf("the implementer's row after the refused report = %+v, want the first report's handoff kept", row)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Testing, stepPlace(2))
	})

	t.Run("a fresh implementer's report of the owner's step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:owner", "tester:owner")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		fresh := claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleImplementer))

		applyFacts(t, pool, engine)("fresh", ownersReport(root.Key, fresh, claim.RoleImplementer, 0, "implement-LEGION-208"))
		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
		if row := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer); row.Claim != owner || row.LastHandoff != "" {
			t.Fatalf("the implementer's row after the fresh claim's report = %+v, want the owner's claim on it and no handoff", row)
		}
	})
}
