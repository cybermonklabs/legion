package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// mergeForwardMessage publishes one message of a merge forward (record.MergeForwardMessage,
// dispatch://INFRA-4374 R1) to the Envoy topic of the role it names on the row's issue, and to
// nothing else: the ask that the implementer, or in merging the merger, merge the base forward and
// push, the hold that tells the worker of the phase the issue stays in to stop writing the
// workspace while it does, and, once the merge lands, the landed to that worker and the review to
// the reviewer of an issue whose task's reviewer step already ran (payload.Message,
// composed from the pull request as this attempt reads it). It stops nothing it reaches.
//
// The row is held as a merge-queue removal's is (mergeQueueRemoval), not ended as a plain role
// message to a released claim is (roleMessage): the issue keeps its phase while the merge runs and
// no phase start carries the ask, so a released role that is not told never merges, and the
// conflicting head stands with no CI and nobody at work on it. So the row is delivered as
// deliverHeld delivers a held message: a released role is woken, and the row waits until a session
// holds the role.
//
// Each attempt first reads the issue, its pull request and whether its tree lingers in one
// transaction, and finishes the row undelivered, with one log line naming why, once the message is
// no longer to be delivered by the workflow's rule for these rows (workflow.MergeForwardAt): the
// record holds no such issue, the issue has left the workflow, its tree lingers, the record holds
// no pull request or one that is not open, the row is to the merger and the issue no longer stands
// in merging, the record holds no merge forward or one of another head, or the row is an ask or a
// hold and the merge forward has landed. Role-message rows run in no order of the issue's, so a
// hold still queued behind its landed would otherwise tell the worker to wait for a head that
// already came. What follows - a claim that failed or retired, the design gate, the wake and the
// publish - is deliverHeld's, as it is a merge-queue removal's. The message reaches the claim it
// names (record.MergeForwardMessage.Claim), the worker of its role's step when the workflow wrote
// it; a row the release before wrote names none and reaches the claim the record gives that step
// when it runs (workerToken).
func (r *outbox) mergeForwardMessage(ctx context.Context, row record.OutboxRow, payload record.MergeForwardMessage) error {
	if r.notices == nil {
		return errors.New("merge-forward message executor has no Envoy publisher")
	}
	if r.supervisor == nil {
		return errors.New("merge-forward message executor has no claim supervisor")
	}
	token, err := r.workerToken(ctx, row, payload.Role, payload.Claim)
	if err != nil {
		return err
	}
	var issue *record.Issue
	var pr *record.PullRequest
	var left string
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, pr, left, err = workflow.MergeForwardAt(ctx, r.records, tx, row.Issue, payload.Role, payload.Step, payload.Head)
		return err
	}); err != nil {
		return fmt.Errorf("read the issue and the pull request of %s: %w", row.Issue, err)
	}
	if issue == nil {
		left = "the issue is not recorded"
	}
	attributes := []any{"row", row.ID, "issue", row.Issue, "role", payload.Role, "step", payload.Step, "head", payload.Head}
	if left != "" {
		r.log.Info("outbox merge-forward message finished undelivered: the workflow no longer delivers it", append(attributes, "why", left)...)
		return nil
	}
	return r.deliverHeld(ctx, row, heldMessage{issue: *issue, token: token, role: payload.Role,
		what: fmt.Sprintf("the merge-forward %s of %s", payload.Step, row.Issue), message: payload.Message(row.Issue, *pr),
		attributes: attributes,
		claimEnded: "outbox merge-forward message finished undelivered: the role's claim has failed or retired",
		delivered:  "outbox merge-forward message delivered"})
}
