package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
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
// conflicting head stands with no CI and nobody at work on it. So a released role is woken
// (wakeForDelivery), and the row waits on its backoff until the woken session holds the role, or,
// while the wake pacing holds that resume, for its turn, reading the claim again at most
// parkedWakeBound apart (parkedWait), as it waits while the listener answers that nobody holds the
// role (notify.ErrNoHolder) or that its holder is unresponsive (notify.ErrHolderUnresponsive). A
// role with no claim yet is no end either: the row waits for it.
//
// Each attempt reads, in this order, what ends or holds the row. The row finishes undelivered,
// with one log line naming why, once the merge forward no longer stands at the row's head
// (mergeForwardLeft: the record holds no merge forward, or one of another head, or the issue has
// left the workflow), and once the role's claim has failed or retired, since that claim will not
// hold its role again. While the root's design gate is closed (designGateClosed) a released role is
// neither woken nor told, and the row waits for the gate to open, as a merge-queue removal waits:
// a role the gate holds may not know the gate closed, and woken by this message it would work
// against the gate. A role whose session holds it is told at once, gate or none. The three other
// answers of the confirmed publish route keep the ordinary retry: a holder that did not confirm in
// time (notify.ErrNotDelivered), one that does not advertise the delivery mode
// (notify.ErrRefusedByHolder), and a forward the listener's own NATS grant denies
// (notify.ErrPublishDenied).
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
	issue, pr, lingers, err := r.issueRecords(ctx, row.Issue)
	if err != nil {
		return err
	}
	attributes := []any{"row", row.ID, "issue", row.Issue, "role", payload.Role, "step", payload.Step, "head", payload.Head}
	if left := mergeForwardLeft(issue, lingers, pr, payload.Head); left != "" {
		r.log.Info("outbox merge-forward message finished undelivered: the merge forward no longer stands at that head", append(attributes, "why", left)...)
		return nil
	}
	held := r.supervisedClaim(token)
	if held.State == supervise.StateFailed || held.State == supervise.StateRetired {
		r.log.Info("outbox merge-forward message finished undelivered: the role's claim has failed or retired", append(attributes, "state", held.State)...)
		return nil
	}
	what := fmt.Sprintf("the merge-forward %s of %s", payload.Step, row.Issue)
	if held.Released() {
		closed, err := r.designGateClosed(ctx, *issue)
		if err != nil {
			return err
		}
		if closed {
			return wait.Errorf("%s waits for the design gate of %s to open before it wakes the released %s", what, issue.Tree, payload.Role)
		}
	}
	// This runs before the publish, which would otherwise answer ErrNoHolder for the released
	// role on every attempt and wait for a session nothing brings back. A launch the wake pacing
	// holds is waited for until it runs, the claim read again at its turn or parkedWakeBound on,
	// whichever comes first (parkedWait).
	if waits, turn, err := r.wakeForDelivery(ctx, token, what); err != nil {
		return err
	} else if !turn.IsZero() {
		return r.parkedWait(turn, fmt.Sprintf("%s waits for its %s's launch, which the wake pacing holds until %s",
			what, payload.Role, turn.UTC().Format(time.RFC3339)))
	} else if waits {
		return wait.Errorf("%s waits for its %s's released pod to come back", what, payload.Role)
	}
	message := payload.Message(row.Issue, pr.Repo)
	published := r.notices.Publish(ctx, notify.RoleTopicPrefix+string(token), message, message, record.OutboxKey(row.ID))
	switch {
	case published == nil:
		r.log.Info("outbox merge-forward message delivered", attributes...)
		return nil
	case errors.Is(published, notify.ErrNoHolder):
		return wait.Errorf("%s waits for a session to hold its %s's role: %w", what, payload.Role, published)
	case errors.Is(published, notify.ErrHolderUnresponsive):
		return wait.Errorf("%s waits for its %s, whose session the listener reports %s: %w", what, payload.Role, notify.StateUnresponsive, published)
	}
	return fmt.Errorf("publish %s to its %s: %w", what, payload.Role, published)
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
