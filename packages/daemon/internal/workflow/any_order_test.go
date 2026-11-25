package workflow

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// toldHandoff fails unless the task of start names the handoff word version version (handoff_phase
// and handoff_version), which is the report of the step before it, and no other.
func toldHandoff(t *testing.T, who string, start record.SuperviseRequest, at phase.Phase, word, version string) {
	t.Helper()
	told := readTaskOf(t, start.Task).fields
	if start.Phase != at || told["handoff_phase"] != word || told["handoff_version"] != version {
		t.Fatalf("the %s's start = %+v, want %s, told handoff_phase %s and handoff_version %s", who, start, at, word, version)
	}
}

// reportsHandoff writes on role's phases row of key the handoff record a completion of its step
// names, version version, at the pull request's head, as the completion route leaves it.
func reportsHandoff(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, role claim.Role, version int) {
	t.Helper()
	row := roleRowOf(t, pool, engine, key, role)
	row.Issue, row.Role = key, role
	row.HandoffRecord, row.HandoffVersion, row.HandoffCommit = string(role)+"-"+key, version, "head"
	seedPhase(t, pool, row)
}

// signsOff is the owner's sign_off of key, by its tree root's architect, as the route builds it
// (rootsSignOff).
func signsOff(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) intake.Result {
	t.Helper()
	result, err := intake.ApplyFact(t.Context(), pool, "api", "sign-off-"+key, rootsSignOff(t, pool, engine, key), engine, admissionStub{})
	if err != nil {
		t.Fatalf("the sign-off of %s: %v", key, err)
	}
	return result
}

// lastStartOf is the newest start of role in key's outbox, failing where there is none.
func lastStartOf(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) record.SuperviseRequest {
	t.Helper()
	starts := roleStartsOf(t, pool, key, role)
	if len(starts) == 0 {
		t.Fatalf("%s's outbox holds no start of the %s", key, role)
	}
	return starts[len(starts)-1]
}

// The steps of a task in another order than today's move on, each step told the report of the
// step that comes before it in the task. The step after the owner's acceptance is told the report
// of the step the owner accepted, which is the reviewer's only in today's order; a planner after
// the implementer moves the task on into Legion's own retro; and a child's first notice names the
// phase of its first step. The twin of each: the default steps, which an issue with no plan rows
// runs, stay as today.
func TestTheStepsOfAnyOrderMoveOn(t *testing.T) {
	t.Run("a: the reviewer after the owner's acceptance is told the implementer's report", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "acceptance:owner", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		reportsHandoff(t, pool, engine, root.Key, claim.RoleImplementer, 3)
		// A review of an earlier run stands on the reviewer's row, which this run's reviewer has
		// not reported over: the sign-off must not hand it on.
		seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleReviewer, Claim: "review-claim",
			HandoffRecord: "review-" + root.Key, HandoffVersion: 1, HandoffCommit: "old"})
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Accepting, record.TaskPlace{Kind: record.PlaceStep, Position: 2})

		if result := signsOff(t, pool, engine, root.Key); result.Refusal != nil {
			t.Fatalf("the sign-off was refused: %+v", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 3})
		start := lastStartOf(t, pool, root.Key, claim.RoleReviewer)
		toldHandoff(t, "reviewer", start, phase.Reviewing, "implement", "3")
		// Accepting holds no slot and reviewing keeps one, so the reviewer's start waits for one.
		if !start.NeedsSlot {
			t.Fatalf("the reviewer's start after the sign-off waits for no slot: %+v", start)
		}
	})

	t.Run("a: a parent whose child still runs is refused its sign-off", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "acceptance:owner", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		stepChild(t, pool, "LEGION-209", phase.Implementing, nil)
		if result := signsOff(t, pool, engine, root.Key); refusalCode(result) != "SIGNOFF_CHILD_IN_WORKFLOW" {
			t.Fatalf("the sign-off of a parent whose child runs = %+v, want SIGNOFF_CHILD_IN_WORKFLOW", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Accepting, record.TaskPlace{Kind: record.PlaceStep, Position: 2})
	})

	t.Run("b: the tester after the owner's acceptance of a review is told the review", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer", "acceptance:owner", "tester")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		reportsHandoff(t, pool, engine, root.Key, claim.RoleImplementer, 3)
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		reportsHandoff(t, pool, engine, root.Key, claim.RoleReviewer, 2)
		move(t, pool, engine, root.Key, TriggerReviewApproved, "", record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Accepting, record.TaskPlace{Kind: record.PlaceStep, Position: 3})

		if result := signsOff(t, pool, engine, root.Key); result.Refusal != nil {
			t.Fatalf("the sign-off was refused: %+v", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Testing, record.TaskPlace{Kind: record.PlaceStep, Position: 4})
		toldHandoff(t, "tester", lastStartOf(t, pool, root.Key, claim.RoleTester), phase.Testing, "review", "2")
	})

	t.Run("the twin: the default steps' sign-off starts the retro told the review", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		stepRoot(t, pool, phase.Accepting)
		seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim",
			HandoffRecord: "implement-LEGION-208", HandoffVersion: 3, HandoffCommit: "head"})
		seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim",
			HandoffRecord: "review-LEGION-208", HandoffVersion: 2, HandoffCommit: "head"})
		if result := signsOff(t, pool, engine, "LEGION-208"); result.Refusal != nil {
			t.Fatalf("the sign-off was refused: %+v", result.Refusal)
		}
		toldHandoff(t, "retro", lastStartOf(t, pool, "LEGION-208", claim.RoleImplementer), phase.Retro, "review", "2")
	})

	t.Run("c: a planner after the implementer moves the task on into its retro", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		root := stepRoot(t, pool, phase.Planning)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "planner")
		seedRecord(t, pool, func(tx pgx.Tx) error {
			return record.NewStore().MoveTask(t.Context(), tx, root.Key, root.Generation, "a",
				record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
		})
		move(t, pool, engine, root.Key, TriggerPlannerCompleted, "", record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
		if start := lastStartOf(t, pool, root.Key, claim.RoleImplementer); start.Phase != phase.Retro {
			t.Fatalf("the implementer's start after the planner = %+v, want its retro", start)
		}
	})

	t.Run("d: a child's first notice names the phase of its first step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		mustTake(t, "the child's plan", applyFacts(t, pool, engine)(planID("LEGION-209", 0, 1),
			ownersPlan(t, "LEGION-209", 0, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
		recordedAt(t, pool, "LEGION-209", "LEGION-208")
		recordedAt(t, pool, "LEGION-210", "LEGION-208")
		firstStep(t, pool, engine, "LEGION-209")
		firstStep(t, pool, engine, "LEGION-210")

		// The notice names the phase of the child's first step (phase), and that the child waited
		// on no issue: it reached the front of the waiting line and took an admission slot.
		for child, first := range map[string]phase.Phase{"LEGION-209": phase.Implementing, "LEGION-210": phase.Planning} {
			want := record.Notice{Kind: texts.ChildStatus, Issue: child, Change: texts.ChangeStarted, Tree: "LEGION-208", Phase: first}
			if got := noticesOf(t, pool, child, "child-status"); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Fatalf("the child-status notices of %s = %+v, want the one %+v", child, got, want)
			}
		}
	})
}
