package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// closeRead reads Dispatch's states of the row's issue's checks for the judge of its close
// (record.CloseRead) and carries them back as one intake.CloseChecksRead fact of the issue, which
// the workflow stores as the issue's newest read and judges the close on. It never runs inside a
// fact's transaction, as the merge-queue read does not.
//
// The read's time is the moment the row begins its first Dispatch call, so a read in flight across
// an entry into verifying is not taken as one after it. The read is:
//   - the issue's results (dispatch.Client.IssueResults);
//   - the results at the newest merge's final head (IssueResultsAt, the record's Merge.Head), only
//     where a before_merge check has subjects and none of them is at that head: another pull
//     request of the issue is open, and the close takes only that check's answer at the head;
//   - the issue's children (GetIssue), which the close reads beside its record;
//   - the Verification document's newest approved version and its latest version, each version's
//     blocks as Dispatch reads them from its own text (Approval, ApprovedVersionBefore and
//     PlanDocument), so the close can tell a check removed or reworded since the approval.
//
// A read that fails carries the fact with the failure and no answer, so the record says the read
// failed, and fails the row, so the outbox reads again on its backoff. The failure is kept as the
// daemon stores a far side's error text (storetext.FarSideError): an upstream error page is cut to
// its start, since the record, the close's wait and its notices all carry it.
func (r *outbox) closeRead(ctx context.Context, row record.OutboxRow, payload record.CloseRead) error {
	if r.dispatch == nil {
		return errors.New("close read executor has no Dispatch client")
	}
	readAt := r.now().UTC()
	var issue *record.Issue
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(ctx, tx, row.Issue)
		return err
	}); err != nil {
		return fmt.Errorf("read the record of %s for the read of its checks: %w", row.Issue, err)
	}
	if issue == nil {
		r.log.Warn("outbox close read found no record of its issue; the row finishes", "row", row.ID, "issue", row.Issue)
		return nil
	}
	answer, readErr := r.readCloseChecks(ctx, row.Issue, issue.Merge.Head)
	fact := intake.CloseChecksRead{Issue: row.Issue, Generation: payload.Generation, ReadAt: readAt}
	if readErr != nil {
		fact.Failure = storetext.FarSideError(readErr.Error())
	} else {
		fact.Answer = &answer
	}
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("close-read:%s:%d:%d", row.Issue, row.ID, row.Attempts+1), fact, r.handlers...); err != nil {
		return errors.Join(readErr, fmt.Errorf("apply the read of the checks of %s: %w", row.Issue, err))
	}
	return readErr
}

// readCloseChecks makes the Dispatch reads of one close read of issue, whose newest merge's final
// head is mergeHead ("" before its first merge), and answers them in the close's stored layout.
func (r *outbox) readCloseChecks(ctx context.Context, issue, mergeHead string) (record.CheckReadAnswer, error) {
	results, err := r.dispatch.IssueResults(ctx, issue)
	if err != nil {
		return record.CheckReadAnswer{}, fmt.Errorf("read the results of %s: %w", issue, err)
	}
	answer := record.CheckReadAnswer{Results: results}
	if needsReadAtHead(results, mergeHead) {
		atHead, err := r.dispatch.IssueResultsAt(ctx, issue, mergeHead)
		if err != nil {
			return record.CheckReadAnswer{}, fmt.Errorf("read the results of %s at its merge head %s: %w", issue, mergeHead, err)
		}
		answer.AtMergeHead = &atHead
	}
	read, err := r.dispatch.GetIssue(ctx, issue)
	if err != nil {
		return record.CheckReadAnswer{}, fmt.Errorf("read the children of %s: %w", issue, err)
	}
	answer.Children = read.Children
	if results.VerificationArtifactID != "" {
		approval, err := r.readCheckApproval(ctx, results.VerificationArtifactID)
		if err != nil {
			return record.CheckReadAnswer{}, fmt.Errorf("read the approval of %s's Verification document: %w", issue, err)
		}
		answer.Approval = &approval
	}
	return answer, nil
}

// needsReadAtHead says whether a before_merge check of results has subjects and none of them at
// mergeHead: another pull request of the issue is open, so the state at the newest merge's final
// head is read with ?at=. Before the first merge there is no head to read.
func needsReadAtHead(results dispatch.IssueResults, mergeHead string) bool {
	if mergeHead == "" {
		return false
	}
	for _, check := range results.Checks {
		if check.When != dispatch.CheckBeforeMerge || len(check.PassStands.Subjects) == 0 {
			continue
		}
		atHead := false
		for _, subject := range check.PassStands.Subjects {
			if strings.EqualFold(subject.Basis.Commit, mergeHead) {
				atHead = true
				break
			}
		}
		if !atHead {
			return true
		}
	}
	return false
}

// readCheckApproval reads the Verification document artifactID's newest approved version and its
// latest version, with the blocks of each: Version 0 and no approved blocks where no version is
// approved.
func (r *outbox) readCheckApproval(ctx context.Context, artifactID string) (record.CheckApproval, error) {
	approval, err := r.dispatch.Approval(ctx, artifactID)
	if err != nil {
		return record.CheckApproval{}, err
	}
	if approval.LatestVersion < 1 {
		return record.CheckApproval{}, fmt.Errorf("Dispatch names no latest version of %s", artifactID)
	}
	approved, err := r.dispatch.ApprovedVersionBefore(ctx, artifactID, approval.LatestVersion+1)
	if err != nil {
		return record.CheckApproval{}, err
	}
	current, err := r.dispatch.PlanDocument(ctx, artifactID, approval.LatestVersion)
	if err != nil {
		return record.CheckApproval{}, fmt.Errorf("read version %d: %w", approval.LatestVersion, err)
	}
	read := record.CheckApproval{Version: approved, Current: current.Blocks}
	if approved > 0 {
		earlier, err := r.dispatch.PlanDocument(ctx, artifactID, approved)
		if err != nil {
			return record.CheckApproval{}, fmt.Errorf("read the approved version %d: %w", approved, err)
		}
		read.Approved = earlier.Blocks
	}
	return read, nil
}
