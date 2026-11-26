package store

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// warnSharedNewsCredit observes a new confirmed holder credit before its delivery
// replaces the previous mark. All issues in the carried credit share one read;
// each issue's canonical running steps are read and decoded once. It changes no
// credit or delivery: the warning waits for the caller's transaction to commit, and
// goes to the log of that transaction's pool (logOf), as the commit's own lines do.
func warnSharedNewsCredit(ctx context.Context, tx pgx.Tx, token claim.Token, d supervise.Delivery, carried []byte) error {
	if d.ConfirmedAt.IsZero() || len(carried) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `with credits as materialized (
		select c.issue, c.link->>'step' as step, max((c.link->>'to')::integer) as version
		from design_news_credits($1::jsonb) c where c.audience = 'holder'
		group by c.issue, c.link->>'step'),
		versions as materialized (
			select issue, jsonb_object_agg(step, version) as versions from credits group by issue)
		select i.key, c.versions, `+pods.RunningColumn("i.key", "i.generation")+`
		from versions c join issues i on i.key = c.issue
		where not exists (select 1 from pending_task_deliveries old
			where old.claim_token = $2 and old.delivery_id = $3
			and old.confirmed_at is not null and old.carried_news = $1::jsonb)
		order by i.key`, carried, string(token), d.ID)
	if err != nil {
		return fmt.Errorf("read live task agents for news credit on %s: %w", token, err)
	}
	defer rows.Close()
	_, toldAgent, _ := token.Cut()
	for rows.Next() {
		var issue string
		var versions map[string]int
		var raw []byte
		if err := rows.Scan(&issue, &versions, &raw); err != nil {
			return fmt.Errorf("read shared news credit on %s: %w", token, err)
		}
		running, _, err := pods.DecodeRunning(raw)
		if err != nil {
			return fmt.Errorf("read running task agents of %s: %w", issue, err)
		}
		seen := make(map[claim.Agent]struct{}, len(running))
		others := make([]claim.Agent, 0, len(running))
		for _, step := range running {
			if step.Held || plan.AgentOf(step.Task, step.Position, plan.Step{Role: step.Role, Key: step.Key}).Owner() {
				continue
			}
			agent := claim.Agent(step.Agent)
			if agent == "" {
				// The release before task agents stored no agent column; its
				// role named its agent (record.PlanStep.AgentName).
				agent = claim.Agent(step.Role)
			}
			if _, duplicate := seen[agent]; duplicate {
				continue
			}
			seen[agent] = struct{}{}
			if agent != toldAgent {
				others = append(others, agent)
			}
		}
		if len(seen) <= 1 || len(others) == 0 {
			continue
		}
		steps := make([]string, 0, len(versions))
		for step := range versions {
			steps = append(steps, step)
		}
		slices.Sort(steps)
		for _, step := range steps {
			version := versions[step]
			log := logOf(scopeOf(ctx).pool)
			OnCommit(ctx, func() {
				log.WarnContext(ctx, "shared news credit: other live task agents are not recipients of this delivery",
					"issue", issue, "step", step, "version", version,
					"told_agent", toldAgent, "told_claim", token, "delivery", d.ID,
					"not_recipients", others)
			})
		}
	}
	return rows.Err()
}
