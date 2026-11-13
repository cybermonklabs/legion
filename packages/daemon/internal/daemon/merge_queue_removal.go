package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// mergeQueueRead runs a merge-queue read (record.MergeQueueRead): the daemon's one read of a pull
// request that GitHub reported leaving the merge queue at the row's head. GitHub's event names no
// reason for the removal, and GitHub's merge queue reports a removal in the same second as the
// merge it makes, so this read is what tells the merge from a removal.
//
// Each attempt first reads the issue, its tree's root and its pull request record, and finishes
// the row without reading GitHub once the issue has left merging at the row's head (mergingAt,
// workflow.LeftMergingAt): nobody would be told of what the read says. Otherwise it reads the pull
// request once, bounded by issueRead as the per-issue read bounds its own, and logs one line for
// that read with what it came to. The row's context has no deadline, and a running row renews its
// lease, so a read GitHub never answered would hold the issue's role-message lane without end; a
// read that runs out of issueRead is a failed read. A merged answer applies the merge as the
// webhook's merge would (intake.PullRequestMerged, which closes the issue, and changes nothing when
// the merge is already recorded), and nothing is sent to the merger. A failed read is read again
// after the runtime's wait (workflowRuntime.mergeStateWait), or after GitHub's Retry-After when
// that is longer, up to mergeStateLadderReads reads in all, as the merge-state ladder reads a state
// GitHub has not computed yet. Any other answer, or the last failed read, applies
// intake.MergeQueueReadDone, whose transaction writes the merger's message
// (record.MergeQueueRemoval) while the issue still merges at that head, Unread when no read
// succeeded. The read applies nothing else: no mergeability and no head move, since a fact of that
// kind can move a phase (workflow's mergeability), and a removal moves none
// (dispatch://INFRA-3274/spec R1).
//
// Both facts are applied under an event id that names the row, so a row run again after its finish
// was lost applies neither a second time. The read and the wait are the runtime's, which attach
// gives the outbox: an outbox a test builds by hand has neither, and the row fails with an error
// that names the missing read rather than reading nothing.
func (r *outbox) mergeQueueRead(ctx context.Context, row record.OutboxRow, payload record.MergeQueueRead) error {
	if r.readPullRequest == nil || r.mergeStateWait == nil {
		return errors.New("merge-queue read executor has no read of a pull request from GitHub")
	}
	_, pr, left, err := r.mergingAt(ctx, row.Issue, payload.Number, payload.Head)
	if err != nil {
		return err
	}
	if left != "" {
		r.log.Info("outbox merge-queue read finished: the issue left merging at that head",
			"row", row.ID, "issue", row.Issue, "pullRequest", payload.Number, "head", payload.Head, "why", left)
		return nil
	}
	read := row.Attempts + 1
	bounded, cancel := context.WithTimeout(ctx, issueRead)
	pull, failed := r.readPullRequest(bounded, *pr)
	cancel()
	attributes := []any{"row", row.ID, "issue", row.Issue, "pullRequest", payload.Number, "head", payload.Head, "read", read}
	switch {
	case failed == nil && pull.Merged:
		r.log.Info("outbox merge-queue read: GitHub answered merged; the merge is applied", append(attributes, "mergeCommit", pull.MergeCommitSHA)...)
		if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("merge-queue-merged:%s:%d", row.Issue, row.ID),
			intake.PullRequestMerged{Repo: pr.Repo, Number: payload.Number, MergeSHA: pull.MergeCommitSHA}, r.handlers...); err != nil {
			return fmt.Errorf("apply the merge of %s's pull request #%d: %w", row.Issue, payload.Number, err)
		}
		return nil
	case failed == nil:
		r.log.Info("outbox merge-queue read: GitHub answered not merged; the merger is told", append(attributes, "state", pull.State)...)
	case read < mergeStateLadderReads:
		after, _ := githubrest.RetryAfter(failed)
		delay := max(r.mergeStateWait(), after)
		r.log.Warn("outbox merge-queue read: the read of GitHub failed; it reads again",
			append(attributes, "reads", mergeStateLadderReads, "wait", delay, "error", failed)...)
		return wait.Until(r.now().UTC().Add(delay), fmt.Sprintf("the merge-queue read of %s's pull request #%d reads GitHub again in %s, after read %d of %d failed: %v",
			row.Issue, payload.Number, delay, read, mergeStateLadderReads, failed))
	default:
		r.log.Error("outbox merge-queue read: the last read of GitHub failed; the merger is told that no read succeeded",
			append(attributes, "reads", mergeStateLadderReads, "error", failed)...)
	}
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("merge-queue-read:%s:%d", row.Issue, row.ID),
		intake.MergeQueueReadDone{Issue: row.Issue, Number: payload.Number, Head: payload.Head, Unread: failed != nil}, r.handlers...); err != nil {
		return fmt.Errorf("apply the merge-queue read of %s's pull request #%d: %w", row.Issue, payload.Number, err)
	}
	return nil
}

// mergeQueueRemoval publishes the message of a merge-queue removal row (record.MergeQueueRemoval)
// to the Envoy topic of the issue's merger, and to nothing else: the pull request left the merge
// queue at the row's head, GitHub did not report it merged, and the event names no reason, so the
// merger reads the reason on GitHub and acts on it (step 5 of its role prompt). It stops nothing
// it reaches, as a phase-ended message stops nothing.
//
// The row is held as an architect's notice is held (notice_exceptions.go, noticeAnswered), not
// dropped as a phase-ended message nobody holds is (phaseEnded). A phase-ended message is worth
// nothing later, since the role's next task names its phase anyway. Nothing else tells a merger
// of this removal: its pull request waits out of the queue with nobody to enqueue it again or send
// the issue back, and a merger whose pod was released would learn of it only when the release's
// backstop wakes it (backstop_minutes, an hour by default). So a released merger is woken
// (wakeForDelivery), and the row waits on its backoff until the woken session holds the role, as
// it waits while the listener answers that nobody holds it (notify.ErrNoHolder) or that its holder
// is unresponsive (notify.ErrHolderUnresponsive). A merger with no claim yet, its start still
// owed, is no end either: the row waits for it.
//
// Each attempt reads, in this order, what ends or holds the row. The row finishes undelivered,
// with one log line naming why, once the issue has left merging at the row's head (mergingAt, the
// read row's own rule), and once the merger's claim has failed or retired, since that claim will
// not hold its role again. While the root's design gate is closed (designGateClosed) a released
// merger is neither woken nor told, and the row waits for the gate to open: a merger the gate holds
// may not know the gate closed, and woken by this message it could enqueue the pull request again
// against the gate, which holds the merge (mergerWaitsForTheDesignGate holds its start the same
// way). A merger whose session holds its role is told at once, gate or none. The three other
// answers of the confirmed publish route keep the ordinary retry, as they do for a phase-ended
// message: a holder that did not confirm in time (notify.ErrNotDelivered), one that does not
// advertise the delivery mode (notify.ErrRefusedByHolder), and a forward the listener's own NATS
// grant denies (notify.ErrPublishDenied).
func (r *outbox) mergeQueueRemoval(ctx context.Context, row record.OutboxRow, payload record.MergeQueueRemoval) error {
	if r.notices == nil {
		return errors.New("merge-queue removal executor has no Envoy publisher")
	}
	if r.supervisor == nil {
		return errors.New("merge-queue removal executor has no claim supervisor")
	}
	token, err := claim.NewToken(r.project, row.Issue, claim.RoleMerger)
	if err != nil {
		return fmt.Errorf("the merger of %s: %w", row.Issue, err)
	}
	issue, pr, left, err := r.mergingAt(ctx, row.Issue, payload.Number, payload.Head)
	if err != nil {
		return err
	}
	attributes := []any{"row", row.ID, "issue", row.Issue, "pullRequest", payload.Number, "head", payload.Head, "unread", payload.Unread}
	if left != "" {
		r.log.Info("outbox merge-queue removal finished undelivered: the issue left merging at that head", append(attributes, "why", left)...)
		return nil
	}
	held := r.supervisedClaim(token)
	if held.State == supervise.StateFailed || held.State == supervise.StateRetired {
		r.log.Info("outbox merge-queue removal finished undelivered: the merger's claim has failed or retired", append(attributes, "state", held.State)...)
		return nil
	}
	if held.Released() {
		closed, err := r.designGateClosed(ctx, issue)
		if err != nil {
			return err
		}
		if closed {
			return wait.Errorf("the merge-queue removal of %s waits for the design gate of %s to open before it wakes the released merger", row.Issue, issue.Tree)
		}
	}
	// This runs before the publish, which would otherwise answer ErrNoHolder for the released
	// merger on every attempt and wait for a session nothing brings back.
	if woken, err := r.wakeForDelivery(ctx, token, "the merge-queue removal of "+row.Issue); err != nil {
		return err
	} else if woken {
		return wait.Errorf("the merge-queue removal of %s waits for its merger's released pod to come back", row.Issue)
	}
	message := payload.Message(row.Issue, pr.Repo)
	published := r.notices.Publish(ctx, notify.RoleTopicPrefix+string(token), message, message, record.OutboxKey(row.ID))
	switch {
	case published == nil:
		r.log.Info("outbox merge-queue removal delivered to the merger", attributes...)
		return nil
	case errors.Is(published, notify.ErrNoHolder):
		return wait.Errorf("the merge-queue removal of %s waits for a session to hold its merger's role: %w", row.Issue, published)
	case errors.Is(published, notify.ErrHolderUnresponsive):
		return wait.Errorf("the merge-queue removal of %s waits for its merger, whose session the listener reports %s: %w",
			row.Issue, notify.StateUnresponsive, published)
	}
	return fmt.Errorf("publish the merge-queue removal of %s to its merger: %w", row.Issue, published)
}

// mergingAt reads, in one transaction, what a merge-queue row reads of its issue before it acts:
// the issue, the pull request the record holds for it, and why the workflow no longer works the
// issue's merging phase at head of pull request number, "" while it does (workflow.MergingAt).
// The read row and the delivery row read it at every attempt, as every fence of a row is read just
// before the row acts, and through the read the fact between them judges the issue by (workflow's
// mergeQueueReadDone), so none of the three tells a merger of an issue another of them found gone
// from merging. An issue the record does not hold has left merging too.
func (r *outbox) mergingAt(ctx context.Context, key string, number int, head string) (record.Issue, *record.PullRequest, string, error) {
	var issue *record.Issue
	var pr *record.PullRequest
	var left string
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		issue, pr, left, err = workflow.MergingAt(ctx, r.records, tx, key, number, head)
		return err
	}); err != nil {
		return record.Issue{}, nil, "", fmt.Errorf("read the merging phase of %s: %w", key, err)
	}
	if issue == nil {
		return record.Issue{}, nil, "the issue is not recorded", nil
	}
	return *issue, pr, left, nil
}
