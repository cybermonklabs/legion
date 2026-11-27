package delivery

import "context"

// A read of this package's that a request makes - the results read of an issue's checks
// (api/results_comparison.go), and a recorded result's read-through (api/results.go's
// readThroughPullRequests) - waits on GitHub between its reads and writes of Dispatch's own
// tables, and the request must hold no admission unit and no pinned request-pool connection across
// a wait on another server. So each stretch of GitHub calls runs through the GitHubWait the
// request's context carries (WithGitHubWait): api's awayOnNetwork gives the unit back for the
// stretch and takes it back after, on the request's own goroutine - awayOnNetworkNeverRefused for
// the record, which has committed its result - and the table reads and writes around the stretch,
// a transaction among them, run outside it, under the unit. A caller whose context carries none -
// the intake, the reconcile - runs every stretch on its own context, as before.

// GitHubWait runs call, one stretch of GitHub calls, on the context it hands call, and answers
// its own error where it could not let the request go for the stretch or take it back after.
type GitHubWait func(ctx context.Context, call func(context.Context) error) error

type gitHubWaitKey struct{}

// WithGitHubWait returns ctx carrying wait, which every GitHub stretch of this package's reads
// made under it runs through.
func WithGitHubWait(ctx context.Context, wait GitHubWait) context.Context {
	return context.WithValue(ctx, gitHubWaitKey{}, wait)
}

// onGitHub runs call, one stretch of GitHub calls, through the GitHubWait ctx carries, or on ctx
// itself where it carries none. call keeps GitHub's answer and error for itself, so what onGitHub
// answers is only the wait's own failure: the request was refused its unit back, or its context
// ended while it asked for it. A stretch that met one learned nothing for the request, which is
// answered that failure, so its caller keeps nothing of it and holds nothing for it.
func onGitHub(ctx context.Context, call func(context.Context)) error {
	wait, carried := ctx.Value(gitHubWaitKey{}).(GitHubWait)
	if !carried {
		call(ctx)
		return nil
	}
	return wait(ctx, func(ctx context.Context) error {
		call(ctx)
		return nil
	})
}
