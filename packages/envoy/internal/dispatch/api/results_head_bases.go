package api

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// results_head_bases.go builds what a `before_merge` check compares its results with
// (comparisonReader.headBases): the head of each pull request of the check's issue, and where
// none is open, the head of each branch a CI job pushed for it, with the subjects a pass must
// stand at among them. The rest of the comparison, the branches an `after_merge` or a
// `default_branch` check compares with and where each result's commit stands, is
// results_comparison.go's.

// branchHeadAnswer is what GitHub said, on this read, about the head of one branch a CI job
// pushed: the commit the branch is at; gone, where GitHub holds no such branch
// (githubapp.ErrNoBranch), which is a branch deleted once its change merged; or the reason
// Dispatch could not read it. A gone branch is kept apart from an unread one because the two
// part the answers: a deleted branch is no basis and leaves no question open, so a result on it
// stands where a pull request head names its commit and at another commit otherwise, while a
// head Dispatch could not read may be any commit, so a commit no basis names is unknown.
type branchHeadAnswer struct {
	sha    string
	gone   bool
	reason string
}

// headBases is the heads a `before_merge` check compares its results with, and among them its
// subjects (passStandsOf). The bases are the head of each pull request of the check's issue, open
// or merged, newest learned first (pullRequestBases), and, where none is open, after them the head
// of each branch a CI job pushed for that issue (collectChangeBranches) that belongs to it
// (branchOfIssue), in name order (branchHeadBases). Where an open pull request is, the subjects
// are the head of each open one alone: a pull request under review is the change a gate judges, so
// no other head speaks for it while it is open, and a merged head stays a basis that is no
// subject. Where none is open, they are the final head of the merged one that merged last
// (mergedInOrder), and each branch head. Where Dispatch cannot tell which merged one merged last,
// no merged head is a subject, the comparison is marked unordered, and each merged head stays a
// basis. A branch whose head a pull request head already names adds nothing, since that commit is
// a basis already, and a branch GitHub no longer holds adds nothing either. An issue with neither
// compares with nothing, and nothingReason says which of the three kinds of nothing it is; where
// the one thing it might compare with is a branch head Dispatch could not read, the reason is that
// read's.
//
// The pull request heads answer whether or not Dispatch can read the repository. A head
// Dispatch holds is a fact about the work whatever the App can see today, so a result at one
// stands at it, and only a result at no head of the list needs the repository read at all
// (commitStands). A branch head is GitHub's answer now and nothing Dispatch holds, so a branch of
// an issue resolving to no repository is never read, and a resolve that did not answer leaves
// every branch unread for model.ReasonReadFailed.
//
// A read-only caller's read passes the answers its own read-throughs learned, and the statement
// reads the stored rows with them laid on (answers), which is what a writer's read reads once its
// read-throughs have stored them; a writer's read passes none and reads the stored heads alone.
func (c *comparisonReader) headBases(ctx context.Context, issueKey string) comparison {
	heads, err := c.server.deps.Attributor.HeadsAttributedTo(ctx, c.server.deps.Store.Pool, issueKey, c.answers)
	if err != nil {
		slog.Warn("dispatch: read the pull request heads of a check's issue", "issue", issueKey, "error", err)
		return noBases(model.ReasonReadFailed)
	}
	var issueRepo *string
	if len(heads) > 0 {
		issueRepo, _ = c.repositoryOf(ctx, issueKey)
	}
	open := slices.ContainsFunc(heads, func(head delivery.AttributedHead) bool { return head.Open })
	ordered := open || c.mergedInOrder(ctx, heads)
	bases, subjects := pullRequestBases(heads, issueRepo, open, ordered)
	unread := ""
	if !open {
		bases, subjects, unread = c.branchHeadBases(ctx, issueKey, heads, bases, subjects)
	}
	switch {
	case len(bases) > 0:
		return comparison{bases: bases, unread: unread, subjects: subjects, unordered: !ordered}
	case unread != "":
		return noBases(unread)
	}
	return noBases(c.nothingReason(ctx, issueKey))
}

// mergedInOrder reports whether heads, the merged pull request heads of an issue none of whose
// pull requests is open, can be ordered by when each merged: it asks the read's fill
// (fillMergeTimes, over the reader's memo fills) and gives each head the time of its merge the
// fill holds, so pullRequestBases orders them as the fill does.
func (c *comparisonReader) mergedInOrder(ctx context.Context, heads []delivery.AttributedHead) bool {
	rows := make([]delivery.MergeTime, len(heads))
	for index, head := range heads {
		rows[index] = head.MergeTime()
	}
	ordered := c.server.fillMergeTimes(ctx, &c.fills, rows)
	for index := range heads {
		heads[index].MergedAt = rows[index].MergedAt
	}
	return ordered
}

// pullRequestBases is the basis of each of heads, the pull request heads of one issue in the order
// delivery.Attributor.HeadsAttributedTo lists them (headBasis), and among them the subjects, each
// with the repository of its row: where open, some pull request of heads is open, the head of each
// open one; otherwise, where ordered, the merged heads can be ordered by when each merged
// (mergedInOrder), the final head of the one that merged last (mergedAfter), the first of them
// where two share that place; and where they cannot be ordered, none.
func pullRequestBases(heads []delivery.AttributedHead, issueRepo *string, open, ordered bool) ([]model.CheckBasis, []headSubject) {
	bases := make([]model.CheckBasis, 0, len(heads))
	var subjects []headSubject
	newestMerged := -1
	for index, head := range heads {
		basis := headBasis(head, issueRepo)
		bases = append(bases, basis)
		switch {
		case open && head.Open:
			subjects = append(subjects, headSubject{basis: basis, repo: head.Repo})
		case !open && (newestMerged < 0 || mergedAfter(head, heads[newestMerged])):
			newestMerged = index
		}
	}
	if newestMerged >= 0 && ordered {
		subjects = append(subjects, headSubject{basis: bases[newestMerged], repo: heads[newestMerged].Repo})
	}
	return bases, subjects
}

// mergedAfter reports whether merged pull request a merged after b, in the order
// delivery.NewestPullRequestHead reads them by: the later time of merge first, a head holding no
// time after every head that holds one, and the higher number first between two of one time or
// of none.
func mergedAfter(a, b delivery.AttributedHead) bool {
	switch {
	case a.MergedAt != nil && b.MergedAt != nil && !a.MergedAt.Equal(*b.MergedAt):
		return a.MergedAt.After(*b.MergedAt)
	case (a.MergedAt == nil) != (b.MergedAt == nil):
		return a.MergedAt != nil
	}
	return a.Number > b.Number
}

// branchHeadBases is bases and subjects, the pull request heads of an issue none of whose pull
// requests is open (pullRequestBases of heads), with the head of each branch a CI job pushed for
// issueKey and GitHub holds added (issueBranchHead), in name order, and the reason the first
// branch Dispatch could not read was unread, "" where it read each. A branch head that a pull
// request head already names is that head's basis, of that head's repository, and is no second
// basis; any other is a head of the issue's repository. Each branch head is a subject, once for its
// commit. An issue resolving to no repository has no branch to read, and a resolve that did not
// answer leaves every branch unread for model.ReasonReadFailed.
func (c *comparisonReader) branchHeadBases(
	ctx context.Context, issueKey string, heads []delivery.AttributedHead, bases []model.CheckBasis, subjects []headSubject,
) ([]model.CheckBasis, []headSubject, string) {
	branches := c.changeBranches[issueKey]
	if len(branches) == 0 {
		return bases, subjects, ""
	}
	repo, answered := c.repositoryOf(ctx, issueKey)
	switch {
	case !answered:
		return bases, subjects, model.ReasonReadFailed
	case repo == nil:
		return bases, subjects, ""
	}
	pullRequestHeads := len(bases)
	unread := ""
	for _, branch := range branches {
		sha, reason := c.issueBranchHead(ctx, issueKey, *repo, branch)
		if reason != "" {
			if unread == "" {
				unread = reason
			}
			continue
		}
		if sha == "" {
			continue
		}
		subject := headSubject{basis: model.CheckBasis{Kind: model.CheckBasisBranchHead, Branch: branch, Commit: sha}, repo: *repo}
		if matched := slices.IndexFunc(bases[:pullRequestHeads], func(basis model.CheckBasis) bool { return basis.Commit == sha }); matched >= 0 {
			subject = headSubject{basis: bases[matched], repo: heads[matched].Repo}
		} else {
			bases = append(bases, subject.basis)
		}
		if !slices.ContainsFunc(subjects, func(held headSubject) bool { return held.basis.Commit == sha }) {
			subjects = append(subjects, subject)
		}
	}
	return bases, subjects, unread
}

// issueBranchHead is the commit at the head of branch of repo, where the branch belongs to
// issueKey (branchOfIssue) and GitHub holds it (branchHeadOf), or the reason Dispatch could not
// read either. It answers "" for both where the branch is another issue's, and where GitHub no
// longer holds it.
func (c *comparisonReader) issueBranchHead(ctx context.Context, issueKey, repo, branch string) (string, string) {
	belongs, read := c.branchOfIssue(ctx, issueKey, branch)
	switch {
	case !read:
		return "", model.ReasonReadFailed
	case !belongs:
		return "", ""
	}
	head := c.branchHeadOf(ctx, repo, branch)
	switch {
	case head.reason != "":
		return "", head.reason
	case head.gone:
		return "", ""
	}
	return head.sha, ""
}

// branchOfIssue is whether branch belongs to issueKey (delivery.BranchOfIssue), and whether the
// read answered, held for the rest of the read: a stored row's branch counts for the check's
// issue only where it does, so a row a job of another issue's branch left adds no head. It reads
// the caller's request's answers as headBases does.
func (c *comparisonReader) branchOfIssue(ctx context.Context, issueKey, branch string) (bool, bool) {
	key := [2]string{issueKey, branch}
	if held, read := c.branchIssues[key]; read {
		return held[0], held[1]
	}
	belongs, err := delivery.BranchOfIssue(ctx, c.server.deps.Store.Pool, issueKey, branch, c.answers)
	if err != nil {
		slog.Warn("dispatch: read whether a branch belongs to a check's issue", "issue", issueKey, "branch", branch, "error", err)
	}
	c.branchIssues[key] = [2]bool{belongs, err == nil}
	return belongs, err == nil
}

// collectChangeBranches records, for the issue of each `before_merge` check of the document, the
// branch whose push a CI job ran on for each result recorded on that issue against one of its
// checks (model.CheckResult.Branch), each branch once and in name order. A role's result and a
// derived one name no branch. A result recorded on another issue — a child's against its
// parent's check — names no branch of the check's issue, as a child's pull request is no head of
// it. It runs before any check is compared, so every `before_merge` check of one issue compares
// with one list of heads, whichever of them holds the result that names a branch.
func (c *comparisonReader) collectChangeBranches(checks []model.CheckBlock) {
	for _, check := range checks {
		if check.When != docs.CheckWhenBeforeMerge {
			continue
		}
		for _, result := range check.Results {
			branches := c.changeBranches[check.IssueKey]
			if result.Branch == "" || result.IssueKey != check.IssueKey || slices.Contains(branches, result.Branch) {
				continue
			}
			c.changeBranches[check.IssueKey] = append(branches, result.Branch)
		}
	}
	for _, branches := range c.changeBranches {
		slices.Sort(branches)
	}
}

// branchHeadOf is the head of one branch of repo, read once per read of the results. Nothing
// holds it across reads: a push moves a branch, and the next read must see where it went.
func (c *comparisonReader) branchHeadOf(ctx context.Context, repo, branch string) branchHeadAnswer {
	key := [2]string{repo, branch}
	if held, read := c.branchHeads[key]; read {
		return held
	}
	head := c.server.readBranchHead(ctx, repo, branch)
	c.branchHeads[key] = head
	return head
}

// headBasis is one stored pull request head as the surface names it: the commit, the pull
// request's number, whether it merged (its row is not open, so the commit is its final head),
// and when Dispatch last learned that head with the kind of time it is. The later of the two
// times wins, so a head a webhook moved after the last complete read prints GitHub's own update
// time, named model.BasisLearnedUpdatedAt, and never an older read time. A row carrying neither
// time names none.
//
// issueRepo is the repository the check's issue resolves to (repositoryOf). A head of a pull
// request in another repository, which an issue's link makes its own (R19), names its repository
// beside its number, compared without case (R18); a head of the issue's own repository names
// none, and nor does any head where Dispatch cannot name the issue's repository (a nil issueRepo),
// so each prints with its number alone, as before. The head of a pull request the issue links in
// a repository it does not resolve to names that repository under the canonical name the link is
// read by (AttributedHead.LinkedRepo), whatever issueRepo is, so a reader can tell it from a pull
// request of the same number in the issue's own repository.
func headBasis(head delivery.AttributedHead, issueRepo *string) model.CheckBasis {
	number := head.Number
	basis := model.CheckBasis{Kind: model.CheckBasisPullRequest, Commit: head.HeadSHA, PullRequest: &number, Merged: !head.Open}
	switch {
	case head.LinkedRepo != "":
		basis.Repo = head.LinkedRepo
	case issueRepo != nil && !strings.EqualFold(head.Repo, *issueRepo):
		basis.Repo = head.Repo
	}
	switch {
	case head.EventAt != nil && (head.CheckedAt == nil || head.EventAt.After(*head.CheckedAt)):
		basis.LearnedAt = &model.BasisLearnedAt{At: *head.EventAt, Kind: model.BasisLearnedUpdatedAt}
	case head.CheckedAt != nil:
		basis.LearnedAt = &model.BasisLearnedAt{At: *head.CheckedAt, Kind: model.BasisLearnedReadAt}
	}
	return basis
}
