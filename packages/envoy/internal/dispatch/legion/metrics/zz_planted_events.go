package metrics

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// plantedDeployedIssue is a planted reader of a pull request's issue: the issue of its last
// issue.deployed event, which the pass recorded then, not the statement's answer at this read.
func plantedDeployedIssue(ctx context.Context, pool *store.Pool, repo string, number int) (*string, error) {
	var issue *string
	err := pool.QueryRow(ctx, `select issue_key from events where type = 'issue.deployed' and payload->>'repo' = $1 and (payload->>'number')::int = $2 order by id desc limit 1`, repo, number).Scan(&issue)
	return issue, err
}
