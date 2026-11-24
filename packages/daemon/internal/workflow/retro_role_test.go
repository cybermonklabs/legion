package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// coderEngine is deploymentEngine under a deployment that also defines coder, a role of the type
// implementation.
func coderEngine() *Engine {
	engine := deploymentEngine()
	engine.cfg.Deployment.Roles["coder"] = plan.RoleDef{Name: "coder", Type: plan.TypeImplementation}
	return engine
}

// coderToken is the claim token of the agent coder of key, the agent of its step coder.
func coderToken(t *testing.T, key string) claim.Token {
	t.Helper()
	return claim.Token(treeRoleToken(t, key, "coder"))
}

// coderCheckerAtStep2 is the root standing at step 2 of `coder checker`, coder of the type
// implementation and checker of the type testing, its checker's report of pass on the checker's
// row, with heldParentChild beneath it in implementing where child is true.
func coderCheckerAtStep2(t *testing.T, pool *pgxpool.Pool, child bool) record.Issue {
	t.Helper()
	root := stepRoot(t, pool, phase.Step)
	if child {
		stepChild(t, pool, heldParentChild, phase.Implementing, nil)
	}
	seedStepsAt(t, pool, root.Key, root.Generation, 2, map[string]string{"coder": "implementation", "checker": "testing"}, "coder", "checker")
	seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: "checker", Claim: "checker-claim", HandoffCommit: stepHead, Verdict: "pass",
		StepTask: "a", StepPlace: 2})
	return root
}

// assertRetroStartsCoder fails unless key's retro started the agent coder alone: one start of the
// role coder in retro, the coder's row naming the coder's claim, and no start of the implementer
// in retro.
func assertRetroStartsCoder(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	if starts := startsIn(t, pool, key, "coder", phase.Retro); len(starts) != 1 {
		t.Fatalf("the retro of %s wrote %d starts of the coder, want one", key, len(starts))
	}
	if starts := startsIn(t, pool, key, claim.RoleImplementer, phase.Retro); len(starts) != 0 {
		t.Fatalf("the retro of %s wrote %d starts of the implementer, want none: its agent is the coder", key, len(starts))
	}
	if row := roleRowOf(t, pool, engine, key, "coder"); row.Claim != coderToken(t, key) || row.Entry != 1 {
		t.Fatalf("the coder's row of %s = %+v, want the coder's claim %s at entry 1", key, row, coderToken(t, key))
	}
}

// The retro of a task is run by the agent of its last step of the type implementation, as that
// step's role (record.PlaceWorker): in `coder checker`, coder of the type implementation, the
// checker's pass starts the coder's retro under the coder's own claim, and no implementer; the
// coder's completion at the retro, with its rating, ends the retro and moves the issue to the
// merge of its pull request; and a parent held before its retro for a child starts the coder's
// retro once the child leaves. The controls: the retro read as the implementer's (Walk.RoleAt of
// the retro, startHeldRetro's role) starts the implementer, and a completion judged by the role
// of the phase retro (phase.Of) leaves the coder's completion a report of a phase it left, the
// issue at its retro.
func TestTheRetroRunsAsTheRoleOfItsAgent(t *testing.T) {
	t.Run("the pass into the retro starts the coder", func(t *testing.T) {
		pool := migratedPool(t)
		engine := coderEngine()
		root := coderCheckerAtStep2(t, pool, false)
		move(t, pool, engine, root.Key, TriggerStepPassed, record.TaskPlace{}, record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
		assertRetroStartsCoder(t, pool, engine, root.Key)
	})

	t.Run("the coder's completion ends the retro", func(t *testing.T) {
		pool := migratedPool(t)
		engine := coderEngine()
		root := coderCheckerAtStep2(t, pool, false)
		move(t, pool, engine, root.Key, TriggerStepPassed, record.TaskPlace{}, record.PhaseRow{})
		fact := intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: "coder", Claim: coderToken(t, root.Key), Summary: "retro done",
			Commit: stepHead, Entry: 1, PlanRating: &record.PlanRating{Rating: 4, Reason: "the steps fit the work"}}
		if result := applyFacts(t, pool, engine)("coder-retro", fact); result.Refusal != nil {
			t.Fatalf("the coder's completion of the retro was refused: %+v", result.Refusal)
		}
		if got := issueOf(t, pool, engine, root.Key); got.Phase != phase.Merging {
			t.Fatalf("%s after the coder's completion of its retro stands in %s, want merging", root.Key, got.Phase)
		}
	})

	t.Run("a held retro starts the coder once the child leaves", func(t *testing.T) {
		pool := migratedPool(t)
		engine := coderEngine()
		root := coderCheckerAtStep2(t, pool, true)
		move(t, pool, engine, root.Key, TriggerStepPassed, record.TaskPlace{}, record.PhaseRow{})
		if held := issueOf(t, pool, engine, root.Key); !record.HeldForChildren(held) {
			t.Fatalf("%s after the checker's pass stands in %s with hold %+v, want held before its retro for its children", root.Key, held.Phase, held.Hold)
		}
		childLeavesDone(t, pool, engine)
		assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
		assertRetroStartsCoder(t, pool, engine, root.Key)
	})
}

// A report of the retro that lands after the issue moved on without it is of the retro, whichever
// role's agent did the retro (record.PhaseLeftBy): in `coder checker`, coder of the type
// implementation, the coder's report of its retro, landing once the issue stands at its merge, is
// judged as a report of the retro the coder left, as the implementer's is under phase.LeftBy. It
// ends no retro: carrying a rating it is refused PLAN_RATING_UNEXPECTED, naming the retro, and
// without one it is recorded on the coder's row and moves nothing, the issue still at its merge.
// The control is the phase alone (phase.LeftBy), which reads the coder's report as one of a step.
func TestALateReportOfTheRetroOfARoleOfTheDeploymentIsOfTheRetro(t *testing.T) {
	pool := migratedPool(t)
	engine := coderEngine()
	root := coderCheckerAtStep2(t, pool, false)
	move(t, pool, engine, root.Key, TriggerStepPassed, record.TaskPlace{}, record.PhaseRow{})
	assertRetroStartsCoder(t, pool, engine, root.Key)
	coder := seedPhaseWorker(t, pool, root.Key, "coder", "")
	if coder != coderToken(t, root.Key) {
		t.Fatalf("the coder's claim is %s and its retro's start names %s: this test proves nothing", coder, coderToken(t, root.Key))
	}
	move(t, pool, engine, root.Key, TriggerRetroCompleted, record.TaskPlace{}, record.PhaseRow{})
	assertWalksAt(t, pool, engine, root.Key, phase.Merging, record.TaskPlace{Kind: record.PlaceMerge})
	if row := roleRowOf(t, pool, engine, root.Key, "coder"); row.Completed() {
		t.Fatalf("the coder's row before its late report = %+v, want no completion recorded: this test proves nothing", row)
	}
	apply := applyFacts(t, pool, engine)
	late := intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: "coder", Claim: coder, Summary: "retro done", Commit: stepHead, Entry: 1}

	rated := late
	rated.PlanRating = &record.PlanRating{Rating: 4, Reason: "the steps fit the work"}
	result := apply("late-rated", rated)
	if result.Refusal == nil || result.Refusal.Code != "PLAN_RATING_UNEXPECTED" {
		t.Fatalf("the coder's rated report after its retro = %+v, want PLAN_RATING_UNEXPECTED naming the retro it left", result.Refusal)
	}
	if result := apply("late", late); result.Refusal != nil {
		t.Fatalf("the coder's report after its retro = %+v, want it taken", result.Refusal)
	}
	if row := roleRowOf(t, pool, engine, root.Key, "coder"); !row.Completed() || row.HandoffCommit != stepHead {
		t.Fatalf("the coder's row after its late report = %+v, want the report recorded at %s", row, stepHead)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Merging, record.TaskPlace{Kind: record.PlaceMerge})
}
