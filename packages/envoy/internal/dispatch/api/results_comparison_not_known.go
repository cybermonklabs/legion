package api

import (
	"context"
	"log/slog"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// results_comparison_not_known.go collects what a check's answer turns on that Dispatch has not
// read or does not know (results_comparison.go uses it): whether a head that is not read, or a
// pull request whose issue is not known, can change an answer about the check's issue (decides);
// the pull requests of the repository whose issue is not known (notKnownOf), read once per
// repository; the possible head of a result (possibleHeadAt); and the merges into another branch
// that leave an `after_merge` result unknown (mergedElsewhere).

// notKnownAnswer is what one read of delivery.Attributor.PullRequestsNotKnown answered for a
// repository, and whether it answered at all. A failed read is kept as one, so every result it
// would decide reads model.ReasonReadFailed, as a failed read of the heads does.
type notKnownAnswer struct {
	pulls []delivery.NotKnownPullRequest
	read  bool
}

// appendIssueNotKnown is list with the pull request number, where list does not name it yet.
func appendIssueNotKnown(list []model.IssueNotKnown, number int) []model.IssueNotKnown {
	if slices.ContainsFunc(list, func(held model.IssueNotKnown) bool { return held.Number == number }) {
		return list
	}
	return append(list, model.IssueNotKnown{Number: number})
}

// decides is whether a head Dispatch has not read, and a pull request whose answer is not known,
// can change an answer about the check's issue: the issue resolves to a repository, the
// deployment holds an App key, and the App reads the repository (defaultBranchOf). Elsewhere each
// field answers as it did before Dispatch named either (rule 2): a repository Dispatch cannot
// read is unknown already, and an issue with no repository holds every head it will ever hold.
func (c *comparisonReader) decides(ctx context.Context, issueKey string) bool {
	repo, answered := c.repositoryOf(ctx, issueKey)
	if !answered || repo == nil || c.server.deps.GitHub == nil {
		return false
	}
	_, readable := c.defaultBranchOf(ctx, issueKey)
	return readable
}

// notKnownOf is the open and merged pull requests of the repository the check's issue resolves to
// whose answer is not known (delivery.Attributor.PullRequestsNotKnown), and whether the read
// answered, read once per repository per read of the results. An issue with no repository has none.
// Only answer reads it, after learn, so it holds the pull requests that the same read learned
// (review 18, note 3).
func (c *comparisonReader) notKnownOf(ctx context.Context, issueKey string) ([]delivery.NotKnownPullRequest, bool) {
	repo, answered := c.repositoryOf(ctx, issueKey)
	if !answered || repo == nil {
		return nil, true
	}
	if memoized, held := c.notKnown[*repo]; held {
		return memoized.pulls, memoized.read
	}
	pulls, err := c.server.deps.Attributor.PullRequestsNotKnown(ctx, c.server.deps.Store.Pool, *repo, c.answers)
	if err != nil {
		slog.Warn("dispatch: read the pull requests of a check's repository whose issue is not known",
			"repo", *repo, "error", err)
	}
	answer := notKnownAnswer{pulls: pulls, read: err == nil}
	c.notKnown[*repo] = answer
	return answer.pulls, answer.read
}

// possibleHeadAt is the pull request of the check's repository whose answer is not known and
// whose head is sha, nil where none is, and whether the read of those pull requests answered.
// It has the shape of a basis and is no basis: nothing showed that it is the issue's (decision
// D4).
func (c *comparisonReader) possibleHeadAt(ctx context.Context, issueKey, sha string) (*model.PossibleHead, bool) {
	pulls, read := c.notKnownOf(ctx, issueKey)
	if !read {
		return nil, false
	}
	repo, _ := c.repositoryOf(ctx, issueKey)
	for _, pull := range pulls {
		if pull.HeadSHA == sha {
			return &model.PossibleHead{Repo: *repo, PullRequest: pull.Number, Commit: sha}, true
		}
	}
	return nil, true
}

// mergedElsewhere is the numbers of the merged pull requests of the check's repository whose answer
// is not known and that merged into another base branch than the issue's: the branch the issue's
// newest merged pull request in that repository merged into (mergesIn, over
// delivery.Attributor.MergeOfIssue, newest merge first), or the default branch where the issue
// holds no merge there. Such a pull request can be the issue's own, so the issue's work can have
// reached that other branch. It is nil where decides is false, and for every check that no merge
// decides.
func (c *comparisonReader) mergedElsewhere(ctx context.Context, check model.CheckBlock) []int {
	if check.When != checkWhenAfterMerge || c.at != noCommitNamed || !c.decides(ctx, check.IssueKey) {
		return nil
	}
	pulls, read := c.notKnownOf(ctx, check.IssueKey)
	repo, _ := c.repositoryOf(ctx, check.IssueKey)
	merges := c.mergesIn(ctx, check.IssueKey, *repo, false)
	if !read || !merges.read {
		return nil
	}
	issueBranch := noMergedBranch
	if len(merges.pulls) > 0 {
		issueBranch = merges.pulls[0].BaseRef
	}
	if issueBranch == noMergedBranch {
		issueBranch, _ = c.defaultBranchOf(ctx, check.IssueKey)
	}
	var numbers []int
	for _, pull := range pulls {
		if pull.State == "merged" && pull.BaseRef != issueBranch {
			numbers = append(numbers, pull.Number)
		}
	}
	return numbers
}
