package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// mergeHoldsChanged publishes the message of a merge holds change row (record.MergeHoldsChanged)
// to the Envoy topic of the issue's merger, and to nothing else: the project's merge holds
// document changed, so the merger reads it again, then merges or waits again (step 1 of its role
// prompt). It stops nothing it reaches.
//
// A merger that a line of the document holds ends its turn with a WAITING line and subscribes to
// nothing, because a Dispatch document event does not wake a released pod. So this row is the one
// thing that tells it the hold was lifted. It is held as a merge-queue removal is held
// (mergeQueueRemoval): it is delivered as deliverHeld delivers a held message, so a released
// merger is woken, and the row waits until a session holds the merger's role.
//
// Each attempt first reads whether the issue still works merging (mergingNow), and the row
// finishes undelivered, with one log line naming why, once it does not: the issue stands in
// another phase, it is out of the workflow, its tree lingers, or its pull request has merged. What
// follows - a claim that failed or retired, the design gate, the wake and the publish - is
// deliverHeld's, as it is a merge-queue removal's: it ends on a failed or retired merger, and holds
// a released merger under mergerWaitsForTheDesignGate, while the root's design gate is closed or
// the news of the approved design the merger is owed is not yet recorded.
func (r *outbox) mergeHoldsChanged(ctx context.Context, row record.OutboxRow, payload record.MergeHoldsChanged) error {
	if r.notices == nil {
		return errors.New("merge-holds change executor has no Envoy publisher")
	}
	if r.supervisor == nil {
		return errors.New("merge-holds change executor has no claim supervisor")
	}
	token, err := r.workerToken(ctx, row, claim.RoleMerger, "")
	if err != nil {
		return err
	}
	issue, left, err := r.mergingNow(ctx, row.Issue)
	if err != nil {
		return err
	}
	attributes := []any{"row", row.ID, "issue", row.Issue, "project", payload.Project}
	if left != "" {
		r.log.Info("outbox merge holds change finished undelivered: the issue left merging", append(attributes, "why", left)...)
		return nil
	}
	return r.deliverHeld(ctx, row, heldMessage{issue: issue, token: token, role: claim.RoleMerger,
		what: "the merge holds change of " + row.Issue, message: payload.Message(row.Issue), attributes: attributes,
		claimEnded: "outbox merge holds change finished undelivered: the merger's claim has failed or retired",
		delivered:  "outbox merge holds change delivered to the merger"})
}

// mergingNow reads, in one transaction, the issue a merge holds change row is for and why the
// workflow no longer works its merging phase, "" while it does (workflow.MergingNow), the rule the
// fact that wrote the row judged the issue by. An issue the record does not hold has left merging
// too.
func (r *outbox) mergingNow(ctx context.Context, key string) (record.Issue, string, error) {
	var issue *record.Issue
	var left string
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, left, err = workflow.MergingNow(ctx, r.records, tx, key)
		return err
	}); err != nil {
		return record.Issue{}, "", fmt.Errorf("read the merging phase of %s: %w", key, err)
	}
	if issue == nil {
		return record.Issue{}, "the issue is not recorded", nil
	}
	return *issue, left, nil
}
