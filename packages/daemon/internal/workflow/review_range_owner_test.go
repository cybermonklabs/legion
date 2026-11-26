package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ownerStepAgain moves key's work, back at step 1 after the owner's step returned it, on to the
// owner's step 2 again at newHead: the implementer pushes newHead and completes, as it does before
// a later review round, which is told the commit the round before it read.
func ownerStepAgain(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, newHead string) {
	t.Helper()
	assertWalksAt(t, pool, engine, key, phase.Implementing, stepPlace(1))
	applyFacts(t, pool, engine)("push-"+newHead, intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/" + key, HeadSHA: newHead})
	move(t, pool, engine, key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
}

// reviewedHead is the commit role's row of key holds as the one its step's run before read
// (record.PhaseRow.ReviewedHead), which the owner-step of that step's next run names.
func reviewedHead(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, role claim.Role) string {
	t.Helper()
	return roleRowOf(t, pool, engine, key, role).ReviewedHead
}

// A review step the owner does is told, at its next run, the commit its run before read, as a
// reviewer's later round is told the commit the round before it read: for `reviewer:owner`, the
// commit its complete_step fail named, which a review step's completion writes only where it is
// spelled as a head is (stepCommitStays), and for `acceptance:owner`, the head of the pull request
// its request_backward_move acted on, where the issue has one (recordOwnersReview), since that move
// names no commit of its own. Its first run names no commit read. A step of another type the owner
// does names none at any run.
//
// The controls: the reviewed commit cleared between the two runs (the rule that reads the
// reviewer's row alone writes none for acceptance, and the move back writes none): the second
// owner-step of acceptance names no commit; the rule of the reviewer's and the deployment's step
// alone (a commit named only at step) names none at accepting either.
func TestAReviewStepOfTheOwnerGetsTheCommitItsRunBeforeRead(t *testing.T) {
	t.Run("reviewer:owner", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := ownerAtReviewStep(t, pool, engine)
		if got := reviewedHead(t, pool, engine, root.Key, claim.RoleReviewer); got != "" {
			t.Fatalf("the reviewer's row before the owner's first run ended holds %q, want no commit read", got)
		}
		if result := applyFacts(t, pool, engine)("fail", ownersReview(t, pool, engine, root.Key, "fail", 1)); result.Refusal != nil {
			t.Fatalf("the owner's fail was refused: %+v", result.Refusal)
		}
		ownerStepAgain(t, pool, engine, root.Key, "head-2")
		if told := ownerSteps(t, pool, root.Key); len(told) != 2 {
			t.Fatalf("the owner was told %+v, want two owner-steps of step 2", told)
		}
		if got := reviewedHead(t, pool, engine, root.Key, claim.RoleReviewer); got != ownersReviewCommit {
			t.Fatalf("the reviewer's row at the second run holds %q, want %q, the commit the owner's fail named", got, ownersReviewCommit)
		}
	})

	t.Run("acceptance:owner", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedStepsAt(t, pool, root.Key, root.Generation, 1, nil, "implementer", "acceptance:owner")
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Accepting, stepPlace(2))
		if got := reviewedHead(t, pool, engine, root.Key, claim.RoleAcceptance); got != "" {
			t.Fatalf("the acceptance's row at its first run holds %q, want no commit read", got)
		}
		if result := applyFacts(t, pool, engine)("move-back", rootsMoveBack(t, pool, engine, root.Key, phase.Implementing, "the migration is not tested")); result.Refusal != nil {
			t.Fatalf("the owner's move back was refused: %+v", result.Refusal)
		}
		ownerStepAgain(t, pool, engine, root.Key, "head-2")
		told := ownerSteps(t, pool, root.Key)
		if len(told) != 2 || told[1].Role != claim.RoleAcceptance {
			t.Fatalf("the owner was told %+v, want two owner-steps of the acceptance", told)
		}
		if got := reviewedHead(t, pool, engine, root.Key, claim.RoleAcceptance); got != "head" {
			t.Fatalf("the acceptance's row at the second run holds %q, want %q, the head the move back acted on", got, "head")
		}
	})

	t.Run("tester:owner", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedStepsAt(t, pool, root.Key, root.Generation, 1, nil, "implementer", "tester:owner")
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Testing, stepPlace(2))
		fail := ownersReport(root.Key, claim.Token(treeRoleToken(t, root.Key, claim.RoleArchitect)), claim.RoleTester, 2, "test-1")
		fail.Verdict, fail.ReturnTo, fail.Commit = "fail", 1, ownersReviewCommit
		fail.Entry = roleRowOf(t, pool, engine, root.Key, claim.RoleTester).Entry
		if result := applyFacts(t, pool, engine)("fail", fail); result.Refusal != nil {
			t.Fatalf("the owner's test fail was refused: %+v", result.Refusal)
		}
		ownerStepAgain(t, pool, engine, root.Key, "head-2")
		if told := ownerSteps(t, pool, root.Key); len(told) != 2 {
			t.Fatalf("the owner was told %+v, want two owner-steps of the tester's step", told)
		}
		if got := reviewedHead(t, pool, engine, root.Key, claim.RoleTester); got != "" {
			t.Fatalf("the tester's row at the second run holds %q, want no commit read: the step reviews nothing", got)
		}
	})
}
