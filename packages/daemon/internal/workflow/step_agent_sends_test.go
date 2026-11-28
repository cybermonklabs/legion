package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The two starts that send a step's work again with no move of the task name the agent of the step
// where the issue stands (record.SuperviseRequest.Agent), as the entry that raised the number named
// it, beside the claim they are queued for (record.SuperviseRequest.Claim): the held phase's retry,
// here the operator's restart of the phase (restart), and the send of an owed entry
// (sendOwedEntry). The claim they reach starts that agent in a session of its own where its session
// is another agent's. In `implementer reviewer implementer` the task stands at step 3, whose agent
// is the agent of step 3 of task a, `step:3:a`, on the claim implementer-2. The fault: a start that
// names no agent, which takes whatever session the claim holds.
//
// The send of an owed entry acts on the claim the rule weighed, and names an agent of that claim
// alone: sent for implementer, the claim of step 1, it names no agent, so that claim resumes the
// session it holds. The fault there: the start names step 3's agent on step 1's claim, which would
// start step 3's agent in a new session on a claim that is not its own.
func TestARetryAndAnOwedEntryNameTheAgentOfTheStepWhereTheIssueStands(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, row record.PhaseRow)
		// resend is whether the start hands the entry its row names as a resend (restart).
		resend bool
		// claim and agent are the claim the start names and the agent it runs.
		claim claim.Agent
		agent plan.AgentID
	}{
		{name: "the retry of the phase", resend: true, claim: "implementer-2", agent: "step:3:a", send: func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, _ record.PhaseRow) {
			applyRefusingNothing(t, pool, engine, intake.RestartPhase{Issue: key, Role: claim.RoleImplementer})
		}},
		{name: "the send of an owed entry", claim: "implementer-2", agent: "step:3:a", send: func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, row record.PhaseRow) {
			issue := issueOf(t, pool, engine, key)
			applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
				return engine.SendOwedEntry(ctx, tx, issue, claim.RoleImplementer, row.Claim, row)
			})
		}},
		{name: "the send of an owed entry for another claim than the step's", claim: "implementer", agent: "", send: func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, row record.PhaseRow) {
			issue := issueOf(t, pool, engine, key)
			applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
				return engine.SendOwedEntry(ctx, tx, issue, claim.RoleImplementer, agentToken(t, "implementer"), row)
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := textEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer", "implementer")
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			completesAs(t, pool, engine, claim.RoleImplementer, agentToken(t, "implementer"), "implement", 1, "head")
			completesAs(t, pool, engine, claim.RoleReviewer, agentToken(t, "reviewer"), "review", 1, "head")
			move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
			assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(3))
			row := roleRowNow(t, pool, engine, root.Key, claim.RoleImplementer)
			if row.StepPlace != 3 || row.Claim != agentToken(t, "implementer-2") {
				t.Fatalf("the implementer's row now is at place %d on the claim %q, want step 3's on %q", row.StepPlace, row.Claim, agentToken(t, "implementer-2"))
			}
			outboxRan(t, pool)

			tc.send(t, pool, engine, root.Key, row)

			starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer)
			if len(starts) != 1 {
				t.Fatalf("the implementer's starts = %+v, want the one the send wrote", starts)
			}
			start := starts[0]
			if start.Claim != agentToken(t, tc.claim) || start.Agent != tc.agent || start.Entry != row.Entry || start.ResendEntry != tc.resend {
				t.Fatalf("the start = %+v, want it to name the claim %q and the agent %q at entry %d (resend %t)",
					start, agentToken(t, tc.claim), tc.agent, row.Entry, tc.resend)
			}
		})
	}
}
