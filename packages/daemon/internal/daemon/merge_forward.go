package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
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
// no longer to be delivered by the workflow's rule for these rows (workflow.MergeForwardAt, whose
// leftMergeForwardAt lists each case). Role-message rows run in no order of the issue's, so a hold
// still queued behind its landed would otherwise tell the worker to wait for a head that already
// came.
// What follows - a claim that failed or retired, the design gate, the wake and the publish - is
// deliverHeld's, as it is a merge-queue removal's.
//
// A row of an ask ends in a transaction of its own that holds the fact lock (endMergeForwardAsk),
// whether it ends undelivered or delivered, because the ready of a launch of the asked role sends
// the ask only where no row of it is queued, and leaves the ask to such a row (the workflow's
// askAgainAtLaunch). Every read this attempt ends the row on is made before the end lands - the
// rule above, the claim deliverHeld reads failed, the launch the publish reached - and the role
// can be relaunched and its new launch come ready in between. So the end reads the claim again
// under that lock, and the workflow sends the ask to a launch the row did not reach
// (workflow.MergeForwardAskEnded). The row is the workflow's, and the executor writes no row of
// the ask and no launch of the record's ask: it deletes its own row and hands the workflow what
// it read. The other steps end as they always did: no ready leaves anything to them.
func (r *outbox) mergeForwardMessage(ctx context.Context, row record.OutboxRow, payload record.MergeForwardMessage) error {
	if r.notices == nil {
		return errors.New("merge-forward message executor has no Envoy publisher")
	}
	if r.supervisor == nil {
		return errors.New("merge-forward message executor has no claim supervisor")
	}
	token, err := roleToken(r.project, row, payload.Role)
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
	var end func(context.Context, bool, uint64) error
	if payload.Step == record.MergeForwardAsk {
		end = func(ctx context.Context, delivered bool, told uint64) error {
			return r.endMergeForwardAsk(ctx, row, payload, token, delivered, told, attributes)
		}
	}
	if left != "" {
		if err := r.endUndelivered(ctx, row, end); err != nil {
			return err
		}
		r.log.Info("outbox merge-forward message finished undelivered: the workflow no longer delivers it", append(attributes, "why", left)...)
		return nil
	}
	return r.deliverHeld(ctx, row, heldMessage{issue: *issue, token: token, role: payload.Role,
		what: fmt.Sprintf("the merge-forward %s of %s", payload.Step, row.Issue), message: payload.Message(row.Issue, *pr),
		attributes: attributes,
		claimEnded: "outbox merge-forward message finished undelivered: the role's claim has failed or retired",
		delivered:  "outbox merge-forward message delivered", end: end})
}

// endMergeForwardAsk ends row, a row of payload's ask, in one transaction that holds the fact lock
// (intake.SerializeFacts): it deletes the row under the lease this attempt holds
// (record.Store.FinishLeasedOutbox), reads the asked role's claim there, and hands the workflow
// the row's payload, whether the row was delivered, told, the launch the delivery reached (0 for
// a row that ends undelivered, and for a delivery the listener named no holder of), and the
// claim's launch as read under the lock, 0 where the claim has failed or retired or the daemon
// supervises none (workflow.MergeForwardAskEnded). A ready applied before the lock was taken is
// one the claim read shows the launch of, since a launch's ready comes after the decision that
// relaunched the claim published that launch (supervise.Machine.View); one applied after it finds
// the row gone. The settle that follows finds the row finished. A lease another attempt took since
// is record.ErrOutboxLeaseLost, and that attempt ends the row.
func (r *outbox) endMergeForwardAsk(ctx context.Context, row record.OutboxRow, payload record.MergeForwardMessage, token claim.Token, delivered bool, told uint64, attributes []any) error {
	var launch uint64
	var asked bool
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return err
		}
		if err := r.records.FinishLeasedOutbox(ctx, tx, row.ID, row.LeaseToken); err != nil {
			return err
		}
		launch = 0
		if held := r.supervisedClaim(token); !record.ClaimEnded(held.State) {
			launch = held.Generation
		}
		var err error
		asked, err = workflow.MergeForwardAskEnded(ctx, r.records, tx, row.Issue, payload, delivered, told, launch, r.now().UTC())
		return err
	}); err != nil {
		return fmt.Errorf("end the merge-forward ask of %s to its %s: %w", row.Issue, payload.Role, err)
	}
	if asked {
		r.log.Info("outbox merge-forward ask sent again, to a launch its row did not reach", append(attributes, "told", told, "launch", launch)...)
	}
	return nil
}
