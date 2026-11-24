package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// acceptingFirst is the root LEGION-208, with no pull request record, on a plan whose first step
// is an acceptance (words), its gate opened, so its task stands at step 1, accepting.
func acceptingFirst(t *testing.T, pool *pgxpool.Pool, engine *Engine, words ...string) record.Issue {
	t.Helper()
	root := rootWithNoPullRequest(t, pool, words...)
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	assertWalksAt(t, pool, engine, root.Key, phase.Accepting, stepPlace(1))
	return root
}

// assertStoppedAtTheAcceptance fails unless key's task a stopped at step 1, the acceptance that
// failed, with one task-stopped notice of that step's role and phase.
func assertStoppedAtTheAcceptance(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	if got := issueOf(t, pool, engine, key); got.Hold == nil || got.Hold.From != phase.Accepting {
		t.Fatalf("%s after the fail stands in %s with hold %+v, want its task stopped at accepting", key, got.Phase, got.Hold)
	}
	stopped := noticesOf(t, pool, key, "task-stopped")
	if len(stopped) != 1 || stopped[0].Role != claim.RoleAcceptance || stopped[0].Phase != phase.Accepting {
		t.Fatalf("the task-stopped notices of %s = %+v, want one of step 1's role acceptance at accepting", key, stopped)
	}
}

// An acceptance with no step before it is a step of the plan like any other (a plan is refused
// only where Legion cannot carry it out, and never for a step it lacks): its pass moves the task
// on with no report of an earlier step to hand on, since none ran, and its fail, which names no
// step to return to and has no step of the type implementation before it, stops the task and
// tells its owner, as the fail of any such step does. Both hold for the owner's acceptance
// (`acceptance:owner`), whose fail with no step to name is the step's own fail, and for the fresh
// agent of acceptance (`acceptance`).
//
// The control is the acceptance read as accepting the step before its place alone: the pass of
// either fails as an error on every retry, and the task never leaves step 1.
func TestAnAcceptanceWithNoStepBeforeItPassesAndFails(t *testing.T) {
	t.Run("the owner's pass moves on to step 2", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := acceptingFirst(t, pool, engine, "acceptance:owner", "implementer")
		if result := signsOff(t, pool, engine, root.Key); result.Refusal != nil {
			t.Fatalf("the owner's sign-off of step 1 was refused: %+v", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(2))
		if start := lastStartOf(t, pool, root.Key, claim.RoleImplementer); start.Phase != phase.Implementing {
			t.Fatalf("the implementer's last start = %+v, want step 2's", start)
		}
	})

	t.Run("the fresh agent's pass moves on to step 2", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := acceptingFirst(t, pool, engine, "acceptance", "implementer")
		if result := applyFacts(t, pool, engine)("pass", acceptanceVerdict(t, pool, engine, root.Key, "pass")); result.Refusal != nil {
			t.Fatalf("the fresh agent's pass of step 1 was refused: %+v", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(2))
	})

	t.Run("the fresh agent's fail stops the task", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := acceptingFirst(t, pool, engine, "acceptance", "implementer")
		if result := applyFacts(t, pool, engine)("fail", acceptanceVerdict(t, pool, engine, root.Key, "fail")); result.Refusal != nil {
			t.Fatalf("the fresh agent's fail of step 1 was refused: %+v", result.Refusal)
		}
		assertStoppedAtTheAcceptance(t, pool, engine, root.Key)
	})

	t.Run("the owner's fail stops the task", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := acceptingFirst(t, pool, engine, "acceptance:owner", "implementer")
		owner := claim.Token(treeRoleToken(t, root.Key, claim.RoleArchitect))
		row := roleRowOf(t, pool, engine, root.Key, claim.RoleAcceptance)
		fail := intake.HandoffComplete{Issue: root.Key, Role: claim.RoleAcceptance, Claim: owner, Generation: 1, Entry: row.Entry, Step: 1,
			Summary: "The issue asks for a rollback path, and the working copy has none.", Commit: stepHead, Verdict: "fail"}
		if result := applyFacts(t, pool, engine)("owner-fail", fail); result.Refusal != nil {
			t.Fatalf("the owner's fail of step 1 was refused: %+v", result.Refusal)
		}
		assertStoppedAtTheAcceptance(t, pool, engine, root.Key)
	})
}
