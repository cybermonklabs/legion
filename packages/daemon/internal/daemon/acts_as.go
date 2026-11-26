package daemon

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// ownerActsAs is the daemon's one runtime.ActsAs, which both runtimes are given: for the token of
// an architect that is the owner of the issue it names (record.OwnerClaim), the role of the act
// that owner does now, whose App its slot holds (ownerSlotRole). It is the one list of the owner's
// acts: a merge forward asked of the owner, a step of its own plan, and its retro of its own
// implementation step; the runtimes and the outbox read it here and restate it nowhere. It answers
// false for every other token, an issue the daemon does not record, and an issue whose owner is
// another claim.
func ownerActsAs(pool *store.Pool, records record.Store) runtime.ActsAs {
	return func(ctx context.Context, token claim.Token) (claim.Role, bool, error) {
		return ownerSlotRole(ctx, pool, records, token)
	}
}

// ownerSlotRole reads, in one read-only transaction, the role of the act token's owner does now,
// whose App its slot holds while it does it, and false where it does none: the implementer while
// the record holds a merge forward asked of it that has not ended (ownersMergeForward), whatever
// place the owner works then, since the owner merges the base forward and pushes the issue branch
// as the agent of the task's implementation step would; else the role its place is done as
// (record.OwnerActsAs: a step of the key owner as the step's role, the owner's retro of its own
// implementation step as that step's role), with the plan rows of the issue's task read at its
// retro alone (taskSteps). So its slot holds that role's App while it does the act, and its own
// role's App again after. A token that is no architect's, or no issue's, or not the issue's owner,
// does nothing. It is read at each write of a slot's gh files that asks (runtime.SlotRole), and by
// the outbox before it tells the owner an act (ownerSlotFor). The key is the token's own
// (claim.Token.IssueKey), and the owner check then holds the token to the issue's own project as
// well.
func ownerSlotRole(ctx context.Context, pool *store.Pool, records record.Store, token claim.Token) (claim.Role, bool, error) {
	_, agent, ok := token.Cut()
	if !ok || agent != claim.Agent(claim.RoleArchitect) {
		return "", false, nil
	}
	key, ok := token.IssueKey()
	if !ok {
		return "", false, nil
	}
	var role claim.Role
	var acts bool
	err := store.TransactWith(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := records.Issue(ctx, tx, key)
		if err != nil || issue == nil {
			return err
		}
		owner, err := record.OwnerClaim(*issue)
		if err != nil || owner != token {
			return err
		}
		asked, err := ownersMergeForward(ctx, tx, records, *issue)
		if err != nil || asked {
			role, acts = claim.RoleImplementer, asked
			return err
		}
		var steps []record.PlanStep
		if issue.Phase == phase.Retro {
			if steps, err = taskSteps(ctx, tx, records, *issue); err != nil {
				return err
			}
		}
		role, acts = record.OwnerActsAs(*issue, steps)
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return role, acts, nil
}

// ownersMergeForward is whether the record holds a merge forward of issue's pull request asked of
// issue's owner that has not ended (record.OwnersMergeForward), read in tx, in a tree that does not
// linger (record.TreeLingers): every role of a tree that lingers is suspended, and the owner's next
// launch writes its own App, so the linger is the daemon's to read beside the one predicate, which
// reads none.
func ownersMergeForward(ctx context.Context, tx pgx.Tx, records record.Store, issue record.Issue) (bool, error) {
	pr, err := records.PullRequest(ctx, tx, issue.Key)
	if err != nil || !record.OwnersMergeForward(issue, pr) {
		return false, err
	}
	lingers, err := record.TreeLingers(ctx, records, tx, issue.Tree)
	return err == nil && !lingers, err
}

// taskSteps is the plan rows of the one task of issue's run, which the role its retro is done as is
// read from (record.RetroActsAs), and nil for a run with no plan rows.
func taskSteps(ctx context.Context, tx pgx.Tx, records record.Store, issue record.Issue) ([]record.PlanStep, error) {
	stored, err := records.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return nil, err
	}
	task, err := stored.Task()
	if err != nil {
		return nil, err
	}
	return stored.Steps[task.Task], nil
}
