package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// mergeForwardMessage publishes one message of a merge forward (record.MergeForwardMessage,
// dispatch://INFRA-4374 R1) to the Envoy topic of the role it names on the row's issue, and to
// nothing else: the ask that the implementer merge the base forward and push, the hold that tells
// the worker of the phase the issue stays in to stop writing the workspace while it does, and, once
// the merge lands, the landed to that worker and the review to the reviewer of an issue past its
// review (payload.Message). It stops nothing it reaches.
//
// The row is held as a merge-queue removal's is (mergeQueueRemoval), not ended as a plain role
// message to a released claim is (roleMessage): the issue keeps its phase while the merge runs and
// no phase start carries the ask, so a released implementer that is not told never merges, and the
// conflicting head stands with no CI and nobody at work on it. So the row is delivered as
// deliverHeld delivers a held message: a released role is woken, and the row waits until a session
// holds the role.
//
// Each attempt first reads the issue, its pull request and whether its tree lingers in one
// transaction, and finishes the row undelivered, with one log line naming why, once the merge
// forward no longer stands at the row's head (mergeForwardLeft: the record holds no merge forward,
// or one of another head, or the issue has left the workflow, or its tree lingers); what follows -
// a claim that failed or retired, the design gate, the wake and the publish - is deliverHeld's, as
// it is a merge-queue removal's.
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
	var lingers bool
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		if issue, pr, err = issueRecordIn(ctx, tx, r.records, row.Issue); err != nil || issue == nil {
			return err
		}
		lingers, err = record.TreeLingers(ctx, r.records, tx, issue.Tree)
		return err
	}); err != nil {
		return fmt.Errorf("read the issue and the pull request of %s: %w", row.Issue, err)
	}
	attributes := []any{"row", row.ID, "issue", row.Issue, "role", payload.Role, "step", payload.Step, "head", payload.Head}
	if left := mergeForwardLeft(issue, lingers, pr, payload.Head); left != "" {
		r.log.Info("outbox merge-forward message finished undelivered: the merge forward no longer stands at that head", append(attributes, "why", left)...)
		return nil
	}
	return r.deliverHeld(ctx, row, heldMessage{issue: *issue, token: token, role: payload.Role,
		what: fmt.Sprintf("the merge-forward %s of %s", payload.Step, row.Issue), message: payload.Message(row.Issue, pr.Repo),
		attributes: attributes,
		claimEnded: "outbox merge-forward message finished undelivered: the role's claim has failed or retired",
		delivered:  "outbox merge-forward message delivered"})
}

// mergeForwardLeft says why a merge-forward message of head is no longer to be delivered, "" while
// it is: the record holds the issue in the workflow, its tree not lingering (lingers,
// record.TreeLingers), and a merge forward of head on its pull request
// (record.PullRequest.MergeForward). A merge forward of another head is a newer ask, which has its
// own messages, and the workflow drops the ask on a move into implementing and when the issue
// leaves the workflow (workflow's clearMergeForward), so a message about an ask that no longer
// stands would tell its role of work nobody wants any more.
func mergeForwardLeft(issue *record.Issue, lingers bool, pr *record.PullRequest, head string) string {
	switch {
	case issue == nil:
		return "the issue is not recorded"
	case !record.InWorkflow(*issue):
		return fmt.Sprintf("the issue has left the workflow, at status %s in %s", issue.Status, issue.Phase)
	case lingers:
		return "the issue's tree lingers or has closed"
	case pr == nil:
		return "the record holds no pull request for the issue"
	case pr.MergeForward == nil:
		return "the record holds no merge forward"
	case pr.MergeForward.Head != head:
		return fmt.Sprintf("the merge forward the record holds is of head %s", pr.MergeForward.Head)
	}
	return ""
}
