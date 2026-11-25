package api

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// results_head_bases.go builds what a `before_merge` check compares its results with
// (comparisonReader.headBases): the head of each pull request of the check's issue, with the
// subjects a pass must stand at among them. The rest of the comparison, the branches an
// `after_merge` or a `default_branch` check compares with and where each result's commit stands,
// is results_comparison.go's.

// headBases is the heads a `before_merge` check compares its results with, and among them its
// subjects (passStandsOf). The bases are the head of each pull request of the check's issue, open
// or merged, newest learned first (pullRequestBases). Where an open pull request is, the subjects
// are the head of each open one alone: a pull request under review is the change a gate judges, so
// no other head speaks for it while it is open, and a merged head stays a basis that is no
// subject. Where none is open, the subject is the final head of the merged one that merged last
// (mergedInOrder). Where Dispatch cannot tell which of them merged last, it names no subject and
// marks the comparison unordered, and each merged head stays a basis. An issue with no pull
// request compares with nothing, and nothingReason says which of the three kinds of nothing it is.
//
// The pull request heads answer whether or not Dispatch can read the repository. A head
// Dispatch holds is a fact about the work whatever the App can see today, so a result at one
// stands at it, and only a result at no head of the list needs the repository read at all
// (commitStands).
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
	bases = slices.DeleteFunc(bases, func(basis model.CheckBasis) bool { return basis.Merged })
	unread := ""
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
	ordered := c.server.fillMergeTimes(ctx, c.fills, rows)
	for index := range heads {
		heads[index].MergedAt = rows[index].MergedAt
	}
	return ordered
}

// pullRequestBases is the basis of each of heads, the pull request heads of one issue in the order
// delivery.Attributor.HeadsAttributedTo lists them (headBasis), and among them the subjects, each
// with the repository of its row: where open, some pull request of heads is open, the head of each
// open one; otherwise, where ordered, the merged heads can be ordered by when each merged
// (mergedInOrder), the final head SQL marks newest by delivery's one merge ordering; and where
// they cannot be ordered, none.
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
		case !open && head.NewestMerged:
			newestMerged = index
		}
	}
	if newestMerged >= 0 && ordered {
		subjects = append(subjects, headSubject{basis: bases[newestMerged], repo: heads[newestMerged].Repo})
	}
	return bases, subjects
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
