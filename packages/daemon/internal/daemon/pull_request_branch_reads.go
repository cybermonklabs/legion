package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// pullRequestBranchRead runs the read of the branches of an issue's tasks
// (record.PullRequestBranchRead, dispatch://INFRA-5910 D5, R2 of dispatch://INFRA-4091): the close
// of the last open pull request of the issue's set queued it (workflow's closed), and its fact
// decides that close (intake.PullRequestBranchReadDone, workflow's pullRequestBranchRead). The
// start's read looks a task's branch up only where the record holds no open or merged pull request
// for that task (readIssuesAtStart), so this read is what finds a pull request an earlier daemon's
// record lost, or whose opened the daemon never got, before that close is told.
//
// Each attempt first reads the issue's record, and finishes the row without reading GitHub where
// the record holds no such issue, holds it at another generation than the row's, or names no GitHub
// repository of it: the fact would decide nothing, and the workflow judges the rest of what moved
// on itself. Otherwise it lists, in the issue's repository with the implement App, the open pull
// requests on each branch the row names (openPullRequestsOn, as the per-issue read lists a task's
// branch, issuePullRequest), each list bounded by issueRead, since the row's context has no
// deadline and a running row renews its lease, and logs one line for each pull request it finds.
// What it found is the fact: each pull request listed, in number order, the order GitHub opened
// them in, as the opened its webhook would have carried (intake.PullRequestOpened), naming the
// repository as that webhook's fact would (heardSpelling). A list that fails is read again after the
// runtime's wait, or after GitHub's Retry-After when that is longer (readAgainAfter), as the
// merge-queue read reads again, up to mergeStateLadderReads attempts in all; the last attempt
// applies its failure in the fact (Unread) beside what the other branches showed, so the close is
// told whatever GitHub does. The fact is applied under an event id that names the row, so a row run
// again after its finish was lost applies it once. The list and the wait are the runtime's, which
// attach gives the outbox: an outbox a test builds by hand has neither, and the row fails with an
// error that names the missing read rather than reading nothing.
func (r *outbox) pullRequestBranchRead(ctx context.Context, row record.OutboxRow, payload record.PullRequestBranchRead) error {
	if r.openPullRequestsOn == nil || r.heardSpelling == nil || r.mergeStateWait == nil {
		return errors.New("branch read executor has no read of a branch's open pull requests from GitHub")
	}
	var issue *record.Issue
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(ctx, tx, row.Issue)
		return err
	}); err != nil {
		return fmt.Errorf("read the record of %s: %w", row.Issue, err)
	}
	closed := fmt.Sprintf("%s#%d", payload.Repo, payload.Number)
	if issue == nil || issue.Generation != payload.Generation || issue.GitHubRepo == "" {
		r.log.Info("outbox branch read finished: the issue moved on since the close of its last open pull request, so nothing is read",
			"row", row.ID, "issue", row.Issue, "closed", closed, "generation", payload.Generation)
		return nil
	}
	attempt := row.Attempts + 1
	repo := r.heardSpelling(issue.GitHubRepo)
	var open []intake.PullRequestOpened
	var failed []string
	var lastErr error
	for _, branch := range payload.Branches {
		bounded, cancel := context.WithTimeout(ctx, issueRead)
		pulls, err := r.openPullRequestsOn(bounded, issue.GitHubRepo, branch)
		cancel()
		if err != nil {
			failed, lastErr = append(failed, fmt.Sprintf("%s: %v", branch, err)), err
			continue
		}
		for _, pull := range pulls {
			r.log.Info("outbox branch read: GitHub holds an open pull request on a branch of the issue's tasks",
				"row", row.ID, "issue", row.Issue, "closed", closed, "repo", repo, "pullRequest", pull.Number, "branch", branch, "head", pull.Head.SHA)
			open = append(open, intake.PullRequestOpened{Repo: repo, Number: pull.Number, Branch: branch, Base: pull.Base.Ref, HeadSHA: pull.Head.SHA,
				HeadRepo: pull.HeadRepository(), Body: pull.Body, URL: pull.HTMLURL, UpdatedAt: pull.UpdatedAt})
		}
	}
	if len(failed) > 0 && attempt < mergeStateLadderReads {
		delay := r.readAgainAfter(lastErr)
		r.log.Warn("outbox branch read: a read of GitHub failed; it reads again",
			"row", row.ID, "issue", row.Issue, "closed", closed, "attempt", attempt, "attempts", mergeStateLadderReads, "wait", delay, "failed", failed)
		return wait.Until(r.now().UTC().Add(delay), fmt.Sprintf("the read of %s's branches reads GitHub again in %s, after attempt %d of %d failed: %s",
			row.Issue, delay, attempt, mergeStateLadderReads, strings.Join(failed, "; ")))
	}
	unread := strings.Join(failed, "; ")
	if unread != "" {
		r.log.Error("outbox branch read: the read of the last attempt failed; the close is decided on what the other branches showed",
			"row", row.ID, "issue", row.Issue, "closed", closed, "attempts", mergeStateLadderReads, "failed", failed)
	}
	slices.SortFunc(open, func(a, b intake.PullRequestOpened) int { return cmp.Compare(a.Number, b.Number) })
	fact := intake.PullRequestBranchReadDone{Issue: row.Issue, Generation: payload.Generation, Repo: payload.Repo, Number: payload.Number,
		Open: open, Unread: unread}
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("branch-read:%s:%d", row.Issue, row.ID), fact, r.handlers...); err != nil {
		return fmt.Errorf("apply the read of %s's branches after the close of %s: %w", row.Issue, closed, err)
	}
	return nil
}
