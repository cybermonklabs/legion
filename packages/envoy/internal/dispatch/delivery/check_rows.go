package delivery

import (
	"context"
	"time"
)

// ReadCheckRows reads CIRows.Checks for [from, to) of the deploy repository in q, by the read
// ReadCIRows makes of them: the PR-checks workflow's pull_request runs started in the window, each
// with every job it ran. It reads none of the deploy workflow's push runs, which ReadCIRows reads
// beside them, for a reader that counts the PR-checks runs alone (evals' span of pr-ci's days).
func ReadCheckRows(ctx context.Context, q Querier, deployRepo string, from, to time.Time) (RunsWithJobs, error) {
	return listRunsWithEveryJobStartedIn(ctx, q, deployRepo, DeliveryRunKindPRChecks, "", "pull_request", from, to)
}
