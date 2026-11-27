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
// subjects (passStandsOf). The bases are the head of each pull request of the check's issue that
// Dispatch has read, open or merged, newest learned first (pullRequestBases). Where an open pull
// request is, the subjects are the head of each open one alone: a pull request under review is the
// change a gate judges, so no other head speaks for it while it is open, and a merged head stays a
// basis that is no subject. Where none is open, the subject is the final head of the merged one
// that merged last (mergedInOrder). Where Dispatch cannot tell which of them merged last, it names
// no subject and marks the comparison unordered, and each merged head stays a basis. An issue with
// no pull request compares with nothing, and nothingReason says which of the three kinds of
// nothing it is.
//
// The pull request heads answer whether or not Dispatch can read the repository. A head
// Dispatch holds is a fact about the work whatever the App can see today, so a result at one
// stands at it, and only a result at no head of the list needs the repository read at all
// (commitStands).
//
// A pull request whose head Dispatch has not read is no basis and no subject: it has no results
// row yet, or a read of GitHub is owed for it (delivery.Attributor.HeadsAttributedTo). It goes to
// headsNotRead, and no pass stands by it. Where Dispatch can read the repository (decides), such a
// head can be the head a result stands at: a result at no basis is then not known, never another
// commit, and a check with no basis names model.ReasonHeadNotRead (rule 1 of the shape of the
// answer).
//
// An issue with no pull request at all, in a repository that holds an open or merged pull
// request whose answer is not known, names model.ReasonIssueNotKnown and lists each of them
// (decision D2): one of them can be the issue's own, so "no pull request" says more than Dispatch
// knows. A failed read of those pull requests is model.ReasonReadFailed.
//
// A read-only caller's read passes the writes its own read-throughs would have made (answers),
// and the statement reads the stored rows after them, which is what a writer's read reads once
// its read-throughs have stored them; a writer's read passes none and reads the stored heads
// alone.
func (c *comparisonReader) headBases(ctx context.Context, issueKey string) comparison {
	heads, err := c.server.deps.Attributor.HeadsAttributedTo(ctx, c.server.deps.Store.Pool, issueKey, c.answers)
	if err != nil {
		slog.Warn("dispatch: read the pull request heads of a check's issue", "issue", issueKey, "error", err)
		return noBases(model.ReasonReadFailed)
	}
	known := make([]delivery.AttributedHead, 0, len(heads))
	var headsNotRead []model.HeadNotRead
	for _, head := range heads {
		if head.HeadKnown {
			known = append(known, head)
			continue
		}
		headsNotRead = append(headsNotRead, model.HeadNotRead{
			Number: head.Number, RefusedAs: head.RefusedAs, RefusedSince: head.RefusedSince,
		})
	}
	var issueRepo *string
	if len(known) > 0 {
		issueRepo, _ = c.repositoryOf(ctx, issueKey)
	}
	open := slices.ContainsFunc(known, func(head delivery.AttributedHead) bool { return head.Open })
	ordered := open || c.mergedInOrder(ctx, storedMerged(heads))
	bases, subjects := pullRequestBases(known, issueRepo, open, ordered)
	compared := comparison{bases: bases, subjects: subjects, headsNotRead: headsNotRead}
	if len(headsNotRead) > 0 {
		compared.headNotRead = c.decides(ctx, issueKey)
	}
	if len(bases) > 0 {
		compared.unordered = !ordered
		return compared
	}
	compared.reason = c.nothingReason(ctx, issueKey)
	switch {
	case compared.reason != model.ReasonNoPullRequestOrBranch:
	case compared.headNotRead:
		compared.reason = model.ReasonHeadNotRead
	case len(heads) == 0 && c.decides(ctx, issueKey):
		pulls, read := c.notKnownOf(ctx, issueKey)
		if !read {
			compared.reason = model.ReasonReadFailed
			break
		}
		for _, pull := range pulls {
			compared.issuesNotKnown = append(compared.issuesNotKnown, model.IssueNotKnown{Number: pull.Number})
		}
		if len(compared.issuesNotKnown) > 0 {
			compared.reason = model.ReasonIssueNotKnown
		}
	}
	return compared
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

// storedMerged is each of heads that is a merged pull request row Dispatch stores, its head read
// or a read of GitHub owed for it: the rows the statement orders by when each merged
// (delivery.AttributedHead.NewestMerged), which a read orders the same way (fillMergeTimes). A pull
// request with a timeline row alone has no head (its HeadSHA is "") and no merge to order, and an
// open row merged nothing.
func storedMerged(heads []delivery.AttributedHead) []delivery.AttributedHead {
	merged := make([]delivery.AttributedHead, 0, len(heads))
	for _, head := range heads {
		if head.HeadSHA != "" && !head.Open {
			merged = append(merged, head)
		}
	}
	return merged
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
