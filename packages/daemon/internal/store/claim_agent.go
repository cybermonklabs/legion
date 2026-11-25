package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// ClaimAgentFromSteps is the agent the steps of issue give the claim token, in the identity a
// start names an agent by (plan.Agent.ID), "" where they give none: the agent that ran on the claim
// last, where its latest kept start (step_starts) names a place whose plan row still holds the
// claim's name, else the agent of the latest plan row of that name. It is the rule of the database
// function claim_agent_from_steps, which migration 20261125053850 defines and fills each claim of
// the release before by, so the outbox's start of a claim that records no agent and has run nothing,
// from a row that names no agent, records the agent that migration would have recorded. It is one
// read-only transaction on pool (ReadOnly), the one form of a standalone read.
func ClaimAgentFromSteps(ctx context.Context, pool TxPool, issue string, token claim.Token) (plan.AgentID, error) {
	agent, err := ReadOnly(ctx, pool, fmt.Sprintf("read the agent the steps of %s give claim %s", issue, token),
		func(ctx context.Context, tx pgx.Tx) (string, error) {
			var agent string
			err := tx.QueryRow(ctx, `select coalesce(claim_agent_from_steps($1, $2), '')`, issue, string(token)).Scan(&agent)
			return agent, err
		})
	return plan.AgentID(agent), err
}
