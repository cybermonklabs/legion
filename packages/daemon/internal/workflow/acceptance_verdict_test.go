package workflow

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// freshAgentAccepting is the root LEGION-208 on `implementer reviewer acceptance`, with its open
// pull request #42 at head, moved by the review's approval into accepting, step 3, which the fresh
// agent of acceptance does: the move started that agent's claim.
func freshAgentAccepting(t *testing.T, pool *pgxpool.Pool, engine *Engine) record.Issue {
	t.Helper()
	root := stepRoot(t, pool, phase.Reviewing)
	seedStepsAt(t, pool, root.Key, root.Generation, 2, nil, "implementer", "reviewer", "acceptance")
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	assertWalksAt(t, pool, engine, root.Key, phase.Accepting, stepPlace(3))
	if starts := roleStartsOf(t, pool, root.Key, claim.RoleAcceptance); len(starts) != 1 {
		t.Fatalf("the move into accepting wrote %d starts of the fresh agent of acceptance, want one", len(starts))
	}
	return root
}

// acceptanceVerdict is the fresh agent of acceptance's completion of its step of key with verdict,
// under the claim and at the entry its start wrote on its row, reading head, as the route hands it
// to the engine: no handoff record, since accepting has none.
func acceptanceVerdict(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, verdict string) intake.HandoffComplete {
	t.Helper()
	row := roleRowOf(t, pool, engine, key, claim.RoleAcceptance)
	return intake.HandoffComplete{Issue: key, Role: claim.RoleAcceptance, Claim: row.Claim, Generation: 1, Entry: row.Entry,
		Summary: "The rollback path is not in the change.", Commit: "head", Verdict: verdict}
}

// The fresh agent of acceptance ends its step by its verdict: its fail returns the work to the
// task's step of the type implementation before it, whose agent is told the report, and its pass
// is the step's sign-off, which starts the retro. The control is the accepting case as it was, no
// completion moving an issue at accepting: the issue stays at accepting whatever the verdict. The
// pass is the sign-off in its checks too: while an issue beneath is in the workflow it is refused
// SIGNOFF_CHILD_IN_WORKFLOW naming that issue, the issue stays at accepting, and the refusal leaves
// the report unspent, so the same report, applied once the issue beneath has left, starts the
// retro.
func TestAFreshAgentOfAcceptanceEndsByItsVerdict(t *testing.T) {
	t.Run("the fail returns the work to step 1", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := freshAgentAccepting(t, pool, engine)
		if result := applyFacts(t, pool, engine)("fail", acceptanceVerdict(t, pool, engine, root.Key, "fail")); result.Refusal != nil {
			t.Fatalf("the fresh agent's fail was refused: %+v", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
		if start := lastStartOf(t, pool, root.Key, claim.RoleImplementer); start.Phase != phase.Implementing {
			t.Fatalf("the implementer's start after the fail = %+v, want one in implementing", start)
		}
	})

	t.Run("the pass starts the retro", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := freshAgentAccepting(t, pool, engine)
		if result := applyFacts(t, pool, engine)("pass", acceptanceVerdict(t, pool, engine, root.Key, "pass")); result.Refusal != nil {
			t.Fatalf("the fresh agent's pass was refused: %+v", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
		if start := lastStartOf(t, pool, root.Key, claim.RoleImplementer); start.Phase != phase.Retro {
			t.Fatalf("the implementer's last start = %+v, want the retro's", start)
		}
	})

	t.Run("the pass while an issue beneath is in the workflow is refused", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := freshAgentAccepting(t, pool, engine)
		seedBeneath(t, pool, "LEGION-209", root.Key, root.Key, phase.Implementing, "in_progress")
		pass := acceptanceVerdict(t, pool, engine, root.Key, "pass")
		result := applyFacts(t, pool, engine)("pass", pass)
		if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != "SIGNOFF_CHILD_IN_WORKFLOW" ||
			!strings.Contains(result.Refusal.Message, "LEGION-209") || !result.Unspent {
			t.Fatalf("the fresh agent's pass with LEGION-209 beneath = %+v, want 409 SIGNOFF_CHILD_IN_WORKFLOW naming LEGION-209, unspent", result)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Accepting, stepPlace(3))
		leave(t, pool, "LEGION-209", phase.Done, "done")
		if result := applyFacts(t, pool, engine)("pass", pass); result.Refusal != nil {
			t.Fatalf("the same pass once LEGION-209 left was refused: %+v", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
	})
}
