package record

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// TaskPullRequestRowsOf reads what TaskPullRequestRows reads for each of issues, by issue key, in
// one statement whatever the number of issues: each issue's rows in task order, the rows of one
// task in branch order, as rows alone, each marked with this read (PullRequest.readBy), so
// PutPullRequest refuses it. An issue the record holds no pull request for has no entry, and no
// issues reads nothing: the query is not made. It serves a caller that reads the pull requests of
// every task of many issues and may make no read per issue (the daemon's fillRepositories, at
// each boot). An error names how many issues the read was of and the first of them, not every key.
func (s *Postgres) TaskPullRequestRowsOf(ctx context.Context, tx pgx.Tx, issues []string) (map[string][]PullRequest, error) {
	if len(issues) == 0 {
		return nil, nil
	}
	describe := "read the pull request rows of " + issues[0]
	if len(issues) > 1 {
		describe = fmt.Sprintf("read the pull request rows of %d issues from %s", len(issues), issues[0])
	}
	rows, err := tx.Query(ctx, "select "+pullRequestColumns+" from pull_requests where issue = any($1) order by issue, branch", issues)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	prs, err := collectPullRequests(rows, describe, "TaskPullRequestRowsOf")
	if err != nil {
		return nil, err
	}
	byIssue := make(map[string][]PullRequest, len(issues))
	for _, pr := range prs {
		byIssue[pr.Issue] = append(byIssue[pr.Issue], pr)
	}
	for _, of := range byIssue {
		slices.SortStableFunc(of, func(a, b PullRequest) int { return strings.Compare(a.Task, b.Task) })
	}
	return byIssue, nil
}
