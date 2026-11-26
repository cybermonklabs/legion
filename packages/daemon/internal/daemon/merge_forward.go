package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// errMergeForwardWaits is a merge-forward message to an issue's owner that waits for the owner's
// slot to hold the App of the role it asks an act as (ownerSlotActs).
var errMergeForwardWaits = wait.New("the merge-forward message waits for its owner's slot")

// mergeForwardMessage publishes one message of a merge forward (record.MergeForwardMessage,
// dispatch://INFRA-4374 R1) to the Envoy topic of the claim it reaches on the row's issue, and to
// nothing else: the ask that the agent of the task's implementation step, or in merging the
// merger, merge the base forward and push, the hold that tells the worker of the step the issue
// stands at to stop writing the workspace while it does, and, once the merge lands, the landed to
// that worker and the review to the reviewer of the step that started last, on an issue where a
// reviewer step of its task already ran (payload.Message, composed from the pull request as this
// attempt reads it). The workflow gives each claim one message of each event (its
// addressMergeForward), the ask covering the hold and the review covering the landed
// (record.MergeForwardCovers, payload.Covers), and the landed is not sent to the claim the ask
// reached (record.MergeForward.Agent), which pushed the merge. It stops nothing it reaches.
//
// The row is held as a merge-queue removal's is (mergeQueueRemoval), not ended as a plain role
// message to a released claim is (roleMessage): the issue keeps its phase while the merge runs and
// no phase start carries the ask, so a released role that is not told - the agent of the
// implementation step, or the merger - never merges, and the conflicting head stands with no CI
// and nobody at work on it. So the row is delivered as deliverHeld delivers a held message: a
// released role is woken, and the row waits until a session holds the role.
//
// Each attempt first reads the issue, its pull request and whether its tree lingers in one
// transaction, and finishes the row undelivered, with one log line naming why, once the message is
// no longer to be delivered by the workflow's rule for these rows (workflow.MergeForwardAt): the
// record holds no such issue, the issue has left the workflow, its tree lingers, the row's role, or
// the slot of the claim it reaches, is one the deployment this daemon booted with no longer
// defines (r.deployment: a restart since the row was queued dropped it, and a wake would resume a
// claim whose launch then fails or waits for a pod with that slot), the record holds no pull
// request or one that is not open, the row is to the merger and the issue no longer stands in
// merging, the record holds no merge forward or one of another head, or the row is an ask or a
// hold and the merge forward has landed. Role-message rows run in no order of the issue's, so a
// hold still queued behind its landed would otherwise tell the worker to wait for a head that
// already came. What follows - the design gate, the wake and the publish - is deliverHeld's, as it
// is a merge-queue removal's. The message reaches the claim it names
// (record.MergeForwardMessage.Claim), the worker of its role's step when the workflow wrote it; a
// row the release before wrote names none and reaches the claim the record gives that step when it
// runs (workerToken). A message that reaches the issue's owner and asks an act as its role waits,
// once the owner's session runs, for the owner's slot to hold that role's App (ownerSlotActs).
//
// No row ends on its claim's end while the workflow still delivers it and nobody hears of it. A
// message to the issue's owner whose claim has failed or retired waits for the owner's next start,
// since the owner's own steps wait on the ask. The daemon starts a child's failed owner again at
// its failure where the child stands at a step or a retro only the owner ends (the workflow's
// restartFailedOwner), and else at the child's next phase start (reviveArchitect); a re-admission
// of a mid-phase child (admit's startMidPhaseMembers), an operator's retry and the controller's
// fresh_session start it too. An ask or a review to another agent's claim that failed or retired
// finishes undelivered, and the architect that owns the issue is told it will not reach that agent
// (mergeForwardLost). A hold or a landed to such a claim finishes undelivered with no more: its
// agent is the worker of the step the issue stands at, whose failed claim holds that step, and
// whose retry starts it again at the record's head.
func (r *outbox) mergeForwardMessage(ctx context.Context, row record.OutboxRow, payload record.MergeForwardMessage) error {
	if r.notices == nil {
		return errors.New("merge-forward message executor has no Envoy publisher")
	}
	if r.supervisor == nil {
		return errors.New("merge-forward message executor has no claim supervisor")
	}
	token, err := r.workerToken(ctx, row.Issue, payload.Role, payload.Claim)
	if err != nil {
		return err
	}
	var issue *record.Issue
	var pr *record.PullRequest
	var left string
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, pr, left, err = workflow.MergeForwardAt(ctx, r.records, tx, r.deployment, row.Issue, payload.Role, token, payload.Step, payload.Head)
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
	owner, err := record.OwnerClaim(*issue)
	owners := err == nil && owner == token
	delivered, err := r.deliverHeld(ctx, row, heldMessage{issue: *issue, token: token, role: payload.Role,
		what: fmt.Sprintf("the merge-forward %s of %s", payload.Step, row.Issue), message: payload.Message(row.Issue, *pr),
		attributes: attributes, delivered: "outbox merge-forward message delivered",
		holds: func(ctx context.Context) error { return r.ownerSlotActs(ctx, row, token, owners, payload) }})
	switch {
	case err != nil:
		return err
	case delivered.ended != "" && owners:
		// The owner's own steps wait on the message. The owner's claim is started again at its
		// failure at a step or a retro only it ends (the workflow's restartFailedOwner), at its
		// child's next phase start (reviveArchitect), at a re-admission, or by an operator's retry
		// or the controller's fresh_session: the row waits for that start.
		return wait.Errorf("the merge-forward %s of %s waits for its %s's claim, which has %s, to be started again", payload.Step, row.Issue, payload.Role, delivered.ended)
	case delivered.ended != "":
		if err := r.mergeForwardLost(ctx, row, *issue, token, payload, delivered.ended); err != nil {
			return err
		}
		r.log.Info(mergeForwardEndedLine, append(attributes, "state", delivered.ended)...)
		return nil
	case owners && payload.Step == record.MergeForwardAsk:
		return r.mergeForwardTold(ctx, row.Issue, payload.Head, token, delivered)
	}
	return nil
}

// mergeForwardEndedLine is the line of a merge-forward message that finished undelivered because
// the claim it reaches has failed or retired (mergeForwardMessage).
const mergeForwardEndedLine = "outbox merge-forward message finished undelivered: the role's claim has failed or retired"

// mergeForwardTold records whom the merge-forward ask of head reached agent, the issue's owner
// (delivered, deliverHeld's report of the publish), on the merge forward the record holds
// (record.Store.MarkMergeForwardTold), in a transaction of its own before the row is finished. The
// mark is built by the one rule an owner-step's run is marked by too (ownerToldMark), and the
// owner's catch-up at a later launch reads it by the one rule of told (workflow's toldNow), so the
// ask is named only to a session that was not told it (workflow's untoldMergeForward). A delivered
// row is deleted, so this is the one record of whom it reached. A mark that names nobody records
// nothing, and the owner's next launch is told the ask in its catch-up: a repeat, never an ask left
// untold.
func (r *outbox) mergeForwardTold(ctx context.Context, issue, head string, agent claim.Token, delivered heldDelivery) error {
	mark, ok := ownerToldMark(delivered.told, delivered.recipient)
	if !ok {
		return nil
	}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.MarkMergeForwardTold(ctx, tx, issue, head, agent, mark)
	}); err != nil {
		return fmt.Errorf("record whom the merge-forward ask of %s reached: %w", issue, err)
	}
	return nil
}

// ownerSlotActs is the hold of a merge-forward message that reaches token where token is the
// owner of the row's issue (owners, record.OwnerClaim): the message asks the owner for an act as
// the message's role, so the owner's slot holds the App of that role first (ownerSlotFor, the gate
// an owner-step passes too). The ask is an act as the agent of the task's implementation step, the
// merge forward the record holds asked of the owner, for which the slot acts as the implementer
// (record.OwnersMergeForward), so the runtime writes the slot and the ask waits until its gh files
// hold the implement App. The review a landing asks of the reviewer is an act as the reviewer, an
// approval GitHub refuses from the pull request's author, the implement App, so it waits while the
// owner's slot acts as an App of another role, through the owner's retro of its own implementation
// step and through a merge forward asked of it, and then until the write-back of the owner's own
// App has reached its gh files: it reaches the owner once, after that write-back. The landed told to
// the owner at its own step (record.Issue.StepWorker) says to go on with that step at the new
// head, so it waits the same way for that step's App. The hold asks no act, only that the worker
// stop writing the workspace, and goes at once. A message to another agent's claim waits for
// nothing here.
func (r *outbox) ownerSlotActs(ctx context.Context, row record.OutboxRow, token claim.Token, owners bool, payload record.MergeForwardMessage) error {
	if !owners || payload.Step == record.MergeForwardHold {
		return nil
	}
	return r.ownerSlotFor(ctx, row, token, payload.Role, errMergeForwardWaits, "merge-forward "+string(payload.Step)+" row")
}

// mergeForwardLost tells the architect that owns issue, with one merge-forward-lost notice
// (record.MergeForwardLostNotice) routed as every notice of the issue but an owner-step is, that an
// ask or a review of a merge forward will not reach token, its agent, another agent than the
// issue's owner, whose claim has failed or retired (state): the issue keeps its phase, nobody
// merges or reviews for that message, and the workflow asks once for each head, so nothing asks
// again at that head. The notice's role is the role not reached, and its reason names what starts
// that claim again: a move back into its step, or an operator's retry of it. A hold or a landed
// tells nobody more (mergeForwardMessage).
func (r *outbox) mergeForwardLost(ctx context.Context, row record.OutboxRow, issue record.Issue, token claim.Token, payload record.MergeForwardMessage, state supervise.ClaimState) error {
	var lost string
	switch payload.Step {
	case record.MergeForwardAsk:
		lost = fmt.Sprintf("nobody merges %s forward onto head %s", payload.Base, payload.Head)
	case record.MergeForwardReview:
		lost = fmt.Sprintf("nobody reviews the merge that landed at head %s", payload.Landed)
	default:
		return nil
	}
	reason := fmt.Sprintf("the merge-forward %s of head %s did not reach the %s: its claim %s has %s, so %s. A move back into that role's step starts the claim again, and so does an operator's retry of it",
		payload.Step, payload.Head, payload.Role, token, state, lost)
	return r.enqueueNotice(ctx, row.Issue, record.Notice{Kind: record.MergeForwardLostNotice, Role: payload.Role, Phase: issue.Phase, Reason: reason})
}
