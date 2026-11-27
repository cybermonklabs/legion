package record

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// TaskPullRequestRowsOf reads what TaskPullRequestRows reads for each of issues, by issue key, in
// one read whatever the number of issues (issuePullRequestRows): each issue's task pull requests
// (pullRequestRow's ofTask) in task order, each task's own first among its rows (ownPullRequest),
// as rows alone, each marked with this read (PullRequest.readBy), so PutPullRequest refuses it. An
// issue the record holds no such pull request for has no entry, and no issues reads nothing: the
// query is not made. It serves a caller that reads the pull requests of every task of many issues
// and may make no read per issue (the daemon's fillRepositories, at each boot, and
// HandedOffWithoutOpenOrMergedPullRequest). An error names how many issues the read was of and the
// first of them, not every key.
func (s *Postgres) TaskPullRequestRowsOf(ctx context.Context, tx pgx.Tx, issues []string) (map[string][]PullRequest, error) {
	if len(issues) == 0 {
		return nil, nil
	}
	byIssue, err := issuePullRequestRows(ctx, tx, issues, issuesRead("read the pull request rows of", "read the pull request rows of", issues),
		"TaskPullRequestRowsOf")
	if err != nil {
		return nil, err
	}
	of := make(map[string][]PullRequest, len(byIssue))
	for issue, rows := range byIssue {
		if kept := taskPullRequestsOf(rows); len(kept) > 0 {
			of[issue] = rowRecords(kept)
		}
	}
	return of, nil
}
