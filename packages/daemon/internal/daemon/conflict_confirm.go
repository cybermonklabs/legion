package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// conflictConfirm runs the second read of a head a first read found conflicting with its base
// (record.ConflictConfirm, dispatch://INFRA-4374 R2). GitHub computes mergeability in a background
// job and can answer CONFLICTING about a base it has not caught up with, so the workflow acts on no
// first read: it records it (record.PullRequest.ConflictRead) and queues this row a minute after it
// (workflow's mergeability, conflictConfirmWait). The row reads the pull request again, and the
// workflow decides on the answer, as on every read's: CONFLICTING about the same head and base
// branch confirms the conflict, which R0 then decides on with the base's rules this read carries,
// and MERGEABLE releases the first read.
//
// Each attempt first reads the issue and its pull request record in one transaction
// (issueRecords), and finishes the row without reading GitHub, with one line naming why, once the
// pull request is not open at the row's head or the record holds no first read of that head
// (conflictReadLeft): a later read already confirmed or released it, or a new head left it behind,
// and a second read of it would decide nothing. Otherwise it reads the pull request once, bounded
// by issueRead as the per-issue read bounds its own, and applies GitHub's answer through
// applyMergeability, under an event id that names the row and the attempt, so a row run again
// after its finish was lost applies no answer twice; it logs one line for the attempt with what it
// came to. A failed read, or GitHub's UNKNOWN (`mergeable: null`, its job still running), is read
// again after the runtime's wait (workflowRuntime.mergeStateWait), or after GitHub's Retry-After
// when that is longer, up to mergeStateLadderReads attempts in all, as the merge-queue read reads
// its pull request (mergeQueueRead). The bound is on attempts, each making at most one read. When
// the last attempt's read failed or still answered UNKNOWN, the row applies
// intake.ConflictConfirmDone (Unread for a failed read), which releases the first read so that the
// next read that finds the head conflicting starts the two reads again, and then hands the head to
// the runtime's merge-state reader to follow (queueMergeStateFollow, dispatch://INFRA-4374 R4):
// reads of the same head, the first after twice the runtime's wait, each wait twice the one before
// up to ten minutes, until GitHub answers a state, the record's head moves or the pull request is
// no longer open (followMergeState). Without them the record would read whatever the last attempt
// left for as long as GitHub takes, and nothing would read the head again until its next event. A
// last read that failed starts them too, since the record then follows GitHub no better. Their
// CONFLICTING answer is applied as every read's is, so it records a first conflicting read of the
// head and queues a new row of this kind. They are held in memory, as the ladder's own reads are.
//
// The read, its apply, the wait and the follow are the runtime's, which attach gives the outbox:
// an outbox a test builds by hand has none of them, and the row fails with an error that names the
// missing read rather than reading nothing.
func (r *outbox) conflictConfirm(ctx context.Context, row record.OutboxRow, payload record.ConflictConfirm) error {
	if r.readPullRequest == nil || r.applyMergeability == nil || r.mergeStateWait == nil || r.queueMergeStateFollow == nil {
		return errors.New("conflict confirm executor has no read of a pull request from GitHub")
	}
	issue, pr, _, err := r.issueRecords(ctx, row.Issue)
	if err != nil {
		return err
	}
	attributes := []any{"row", row.ID, "issue", row.Issue, "pullRequest", payload.Number, "head", payload.Head}
	if left := conflictReadLeft(issue, pr, payload); left != "" {
		r.log.Info("outbox conflict confirm finished without a read: the record holds no first read of that head",
			append(attributes, "why", left)...)
		return nil
	}
	attempt := row.Attempts + 1
	bounded, cancel := context.WithTimeout(ctx, issueRead)
	github, pull, failed := r.readPullRequest(bounded, *pr)
	cancel()
	readAt := r.now().UTC()
	attributes = append(attributes, "attempt", attempt, "attempts", mergeStateLadderReads)
	if failed == nil {
		attributes = append(attributes, "mergeable", string(pull.mergeability()), "mergeState", string(pull.mergeState()), "readHead", pull.Head.SHA)
		if err := r.applyMergeability(ctx, github, *pr, pull, readAt, fmt.Sprintf("conflict-confirm:%s:%d:%d", row.Issue, row.ID, attempt)); err != nil {
			return fmt.Errorf("apply the second read of %s's pull request #%d: %w", row.Issue, payload.Number, err)
		}
		if pull.mergeability() != record.MergeabilityUnknown {
			r.log.Info("outbox conflict confirm: GitHub answered; the workflow decides on the answer", attributes...)
			return nil
		}
	}
	if attempt < mergeStateLadderReads {
		delay := r.mergeStateWait()
		if failed != nil {
			after, _ := githubrest.RetryAfter(failed)
			delay = max(delay, after)
			r.log.Warn("outbox conflict confirm: the read of GitHub failed; it reads again", append(attributes, "wait", delay, "error", failed)...)
		} else {
			r.log.Info("outbox conflict confirm: GitHub answered unknown; it reads again", append(attributes, "wait", delay)...)
		}
		return wait.Until(readAt.Add(delay), fmt.Sprintf("the second read of %s's pull request #%d reads GitHub again in %s, after attempt %d of %d read no answer that decides: %v",
			row.Issue, payload.Number, delay, attempt, mergeStateLadderReads, readOutcome(failed)))
	}
	if failed != nil {
		r.log.Error("outbox conflict confirm: the read of the last attempt failed; the first read is released and the merge state reads follow the head", append(attributes, "error", failed)...)
	} else {
		r.log.Info("outbox conflict confirm: GitHub answered unknown at the last attempt; the first read is released and the merge state reads follow the head", attributes...)
	}
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("conflict-confirm-done:%s:%d", row.Issue, row.ID),
		intake.ConflictConfirmDone{Issue: row.Issue, Number: payload.Number, Head: payload.Head, Unread: failed != nil}, r.handlers...); err != nil {
		return fmt.Errorf("apply the end of the second read of %s's pull request #%d: %w", row.Issue, payload.Number, err)
	}
	r.queueMergeStateFollow(row.Issue, payload.Head)
	return nil
}

// readOutcome is what a read that decided nothing came to, for the wait a read row records: the
// error of a read that failed, or GitHub's unknown.
func readOutcome(failed error) any {
	if failed != nil {
		return failed
	}
	return "GitHub answered unknown"
}

// conflictReadLeft says why the second read of payload's head reads nothing, "" while it reads: the
// record holds the issue and its pull request payload.Number, open at payload.Head, with a first
// conflicting read of that head (record.PullRequest.ConflictRead).
func conflictReadLeft(issue *record.Issue, pr *record.PullRequest, payload record.ConflictConfirm) string {
	switch {
	case issue == nil:
		return "the issue is not recorded"
	case pr == nil:
		return "the record holds no pull request for the issue"
	case pr.Number != payload.Number:
		return fmt.Sprintf("the record holds pull request #%d for the issue", pr.Number)
	case pr.State != record.PullRequestOpen:
		return fmt.Sprintf("the pull request is %s", pr.State)
	case pr.HeadSHA != payload.Head:
		return fmt.Sprintf("the pull request stands at head %s", pr.HeadSHA)
	case pr.ConflictRead == nil:
		return "a later read confirmed or released the first read"
	case pr.ConflictRead.Head != payload.Head:
		return fmt.Sprintf("the first read the record holds is of head %s", pr.ConflictRead.Head)
	}
	return ""
}

// issueRecords reads, in one transaction, what a conflict or merge-forward row reads of its issue
// before it acts: the issue key names and the pull request the record holds for it, each nil when
// the record holds none, and whether the issue's tree lingers (record.TreeLingers), which every
// role of it is suspended by. Each row reads it at every attempt, as every fence of a row is read
// just before the row acts.
func (r *outbox) issueRecords(ctx context.Context, key string) (*record.Issue, *record.PullRequest, bool, error) {
	var issue *record.Issue
	var pr *record.PullRequest
	var lingers bool
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		if issue, err = r.records.Issue(ctx, tx, key); err != nil || issue == nil {
			return err
		}
		if lingers, err = record.TreeLingers(ctx, r.records, tx, issue.Tree); err != nil {
			return err
		}
		pr, err = r.records.PullRequest(ctx, tx, key)
		return err
	}); err != nil {
		return nil, nil, false, fmt.Errorf("read the issue and the pull request of %s: %w", key, err)
	}
	return issue, pr, lingers, nil
}
