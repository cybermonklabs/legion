package delivery

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// commit_held.go holds the two reads the result handler makes before it stores a check result
// (api/results_linked.go, refuseUnknownCommit): whether a repository holds a commit at all, and whether
// a pull request an issue links is open or merged, which says whether that pull request's
// repository is one a result of the issue may name a commit of. Neither is an answer about a
// branch, which branch.go owns alone, and neither keeps anything: GitHub is asked each time.

// RepositoryHoldsCommit answers whether repoFull holds sha, through the App: it mints the
// repository's installation token, through the repositoryRead prologue askGitHubCommitOnBranch
// and repositoryDefaultBranch share, and makes githubapp.Client.HasCommit's read.
//
// A false with no error is the one 422 whose message begins `No commit found for SHA`, the
// only answer that proves the repository does not hold the commit (githubapp.Client.HasCommit
// and its noCommitForSHA: GitHub answers the same status for a validation failure and for an
// endpoint it has throttled, so the message is what tells them apart). Every other failure is
// the error, which says only that Dispatch cannot tell whether the commit is there: any other
// 422, a name GitHub's names do not allow (refs.SplitGitHubRepo), a deployment with no App key, a
// repository the App cannot read (RepositoryUnreadable), a rate limit, a 5xx, a transport
// failure. The results route reads the unreadable repository by its kind. The issue's own leaves
// the answer to the linked reads (api/results_linked.go, refuseUnknownCommit): the result is
// stored under a linked repository GitHub says holds the commit, and where none does, stored as
// before where every linked read answered, and refused 502 GITHUB_UNREADABLE beside a linked read
// that did not answer. A linked one counts for nothing, as it does where its pull request cannot
// be read (linkedRepositoryHolds).
func RepositoryHoldsCommit(ctx context.Context, client *githubapp.Client, repoFull, sha string) (bool, error) {
	owner, repo, token, err := repositoryRead(ctx, client, repoFull)
	if err != nil {
		return false, err
	}
	return client.HasCommit(ctx, token, owner, repo, sha)
}

// PullRequestOpenOrMerged answers whether pull request number of repoFull is open or merged,
// through the App: the same repositoryRead prologue, then fetchPullRequestPayload's
// `GET /repos/{owner}/{repo}/pulls/{number}`. The state is the one a head row stores
// (PullRequestHeadState), so a pull request this answers no for is one the head rows call
// closed: it closed and did not merge.
//
// Every failure is the error: a name GitHub's names do not allow (refs.SplitGitHubRepo's
// refs.ErrNotGitHubRepo), a deployment with no App key, a repository the App is not installed on
// (githubapp.ErrNoInstallation, the 404 of its installation lookup) or cannot read otherwise
// (githubapp.ErrRepositoryUnreadable, a 403 that is no rate limit), a pull request GitHub answers
// 404 or 410 for (ErrPullRequestNotFound), a rate limit, a 5xx, a transport failure. Four of them
// are answers no retry changes, and the rest say only that Dispatch cannot tell for now.
// ErrPullRequestNotFound says there is no such pull request; RepositoryUnreadable's two say the
// App cannot read the repository until someone gives it the repository; and a name GitHub's
// names do not allow is no repository GitHub could hold. The results route reads the first as a
// pull request that does not count and the other three as a repository that does not count,
// whose other pull requests it reads no further; every other error leaves the record to be made
// again, unless another repository is shown to hold the commit (api/results_linked.go,
// linkedRepositoryHolds).
func PullRequestOpenOrMerged(ctx context.Context, client *githubapp.Client, repoFull string, number int) (bool, error) {
	owner, repo, token, err := repositoryRead(ctx, client, repoFull)
	if err != nil {
		return false, err
	}
	payload, err := fetchPullRequestPayload(ctx, client, token, owner, repo, number)
	if err != nil {
		return false, err
	}
	return PullRequestHeadState(payload.State, payload.Merged || payload.MergedAt != nil) != "closed", nil
}
