package api

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// results_comparison.go says what each check compares its results with, where each result's
// commit stands against that, and whether a pass of the check stands. It holds no policy about
// GitHub: what is asked, how long an answer is believed and what is written back are
// delivery's (delivery/branch.go, delivery/commit_pull_reads.go, delivery.HeadsAttributedTo,
// delivery.ReadThroughCommitPulls). What lives here is the choice of bases (comparedWith,
// measuredBranches) and one memo per read, so a tab listing twenty results against a handful
// of commits costs a handful of round trips.

// comparisonReader answers one read of one issue's results: for each check the bases its
// results are compared with, and for each result where its commit stands among them.
//
// A `before_merge` check is compared with every pull request head Dispatch holds for the
// check's own issue (delivery.HeadsAttributedTo), open or merged. The issue a result was
// recorded on counts for nothing here, so a child's pull request never answers for a check of
// its parent. A child carrying part of a parent's requirement proves it with a check of its
// own whose `serves` names that requirement.
//
// An `after_merge` check is compared with the branch the check's issue's pull request merged
// into — the base branch of the newest merged pull request attributed to that issue
// (delivery.BaseBranchMergedFor) — and with the repository's default branch. An issue whose
// work merged into an integration branch has reached that branch, so a commit that branch
// carries proves the work before the integration branch itself lands. Once that branch has
// landed the trunk carries the work too, and answers for it where the base branch — deleted on
// the merge, as a repository that deletes every merged branch does — can answer nothing at
// all. So a base branch GitHub no longer holds reads as "this commit is not on it" and never
// as unknown, and the default branch decides (onNamedBranch, measuredBranches). Where Dispatch
// knows of no merge of that issue the default branch answers alone, which is every issue
// merged the ordinary way before Dispatch was told of the merge and every issue whose merge
// Dispatch was never told of.
//
// The merge's own commit goes on standing at the base branch after the branch is gone, because
// Dispatch recorded it as on that branch when it recorded the merge
// (delivery.UpsertPullRequestHead) and a yes is kept for good. In a repository that merges by
// squash that commit never lands on the default branch at all, so without that record a result
// at the merge commit would stand at no basis for ever once the base branch was deleted.
//
// A `default_branch` check is compared with the default branch of the check's issue's resolved
// repository alone, whatever branch the pull request merged into. That is the whole of the
// difference between the two `when`s, and it is why a check that must speak for the trunk is
// written `default_branch` rather than `after_merge`.
//
// Two answers the surface used to collapse into one are kept apart here. A check with nothing
// to compare with — an issue that resolves to no repository, and a `before_merge` check whose
// issue holds no pull request at all — names model.ReasonNoPullRequestOrBranch, and its
// results stand at no basis, so no pass of it stands. A check Dispatch can compare nothing of
// names model.ReasonRepositoryUnreadable, and every result of it is unknown: a `before_merge`
// check whose repository the App cannot read, where nothing can have taught Dispatch that
// commit's pull request, so "no head" is no evidence that the work has moved on; and a branch
// check of a repository no read of this process ever named a default branch for, where there
// is no branch to ask a stored answer about. A branch check Dispatch can name a branch for
// goes on comparing with it while the App cannot read the repository, since the stored answer
// is keyed by that name and a yes stands for good (branchBases). Dispatch's own failed read is
// model.ReasonReadFailed for the same reason, the read of the branch an issue's pull request
// merged into among them. A database hiccup must not make a reviewer refuse an approval.
//
// Where the reader was given a commit (?at=), that one commit is the only basis of every check
// of the issue and no read of GitHub is made at all: the reader asked what the results say
// about one commit, and nothing about that question turns on what GitHub holds now.
//
// The repository each issue resolves to, its default branch, the branch each issue's pull
// request merged into, the pull request heads of each issue, the stored heads of each
// repository-and-commit pair and GitHub's answer about each repository, branch and commit are
// each read once per read of the results. The branch is in that last key, and not the
// repository and the commit alone, because one issue's Verification document may hold an
// `after_merge` check and a `default_branch` check with a result at the same commit: two
// questions, with two answers, that one key for both would collapse into whichever was asked
// first. Across reads, a repository's default branch, a commit's place on a branch and the
// last read-through of a commit's pull requests are held by delivery (server.branches,
// branch_commits and server.commitPulls).
type comparisonReader struct {
	server *server
	// at is the one commit the reader named, and noCommitNamed where it named none.
	at           string
	repos        map[string]*string
	defaults     map[string]defaultBranchAnswer
	baseBranches map[string]baseBranchAnswer
	bases        map[[2]string]comparison
	readThroughs map[[2]string]struct{}
	storedHeads  map[[2]string]bool
	commits      map[[3]string]delivery.BranchAnswer
}

// noCommitNamed is the `at` of a read that named no commit, which is every read but the one a
// reader pins to a commit of its own.
const noCommitNamed = ""

// comparison is what one check's results are compared with: the bases, in the order they are
// asked about, and the reason there are none where the list is empty. Exactly one of the two
// is filled in, and bases is never nil, so the answer carries an empty list rather than a
// null.
type comparison struct {
	bases  []model.CheckBasis
	reason string
}

// noBases is a check comparing with nothing, for the one reason given: the empty list is the
// invariant comparison states, and writing it out at each of its call sites is how one of them
// comes to answer a null.
func noBases(reason string) comparison {
	return comparison{bases: []model.CheckBasis{}, reason: reason}
}

// defaultBranchAnswer is what the App last said about one repository: the name of its default
// branch and whether that read could say at all (delivery.Branches.DefaultBranchOf). The name
// is kept beside the yes because the surface names the branch it compared against, and the
// walk asks about it under that name.
type defaultBranchAnswer struct {
	branch   string
	readable bool
}

// baseBranchAnswer is what Dispatch holds about the merge of one issue: the branch its newest
// merged pull request merged into, noMergedBranch where it holds no merge of that issue at
// all, and whether the read answered. The failed read is kept apart from the empty branch
// because they part the two ways: a read that failed leaves the check comparing with nothing
// it can name, while no merge found leaves the repository's default branch to answer alone
// (measuredBranches). One flag for both would turn a database fault into a verdict about the
// work.
type baseBranchAnswer struct {
	branch string
	read   bool
}

func newComparisonReader(s *server, at string) *comparisonReader {
	return &comparisonReader{
		server: s, at: at, repos: map[string]*string{},
		defaults:     map[string]defaultBranchAnswer{},
		baseBranches: map[string]baseBranchAnswer{},
		bases:        map[[2]string]comparison{},
		readThroughs: map[[2]string]struct{}{}, storedHeads: map[[2]string]bool{},
		commits: map[[3]string]delivery.BranchAnswer{},
	}
}

// noMergedBranch is what delivery.BaseBranchMergedFor answers for an issue Dispatch holds no
// merge of, and for a merged row whose base_ref is empty: no branch to compare with. It is
// spelled like delivery.DefaultBranch and means the opposite of it — "there is nothing here to
// ask about" rather than "ask about the repository's own default branch" — so each is named
// where it is used. measuredBranches tests for one and answers the other in its place, and
// naming both is what makes that a decision of this file's rather than an agreement between
// two packages that either could end without a compiler saying so.
const noMergedBranch = ""

// noDefaultBranchNamed is what delivery.Branches.DefaultBranchOf answers for a repository no
// read of this process ever named a default branch for, which a deployment holding no App key
// makes of every repository: no branch to compare with, and none to ask a stored answer about
// either, since a branch_commits row is keyed by one. It is spelled like delivery.DefaultBranch
// and means the opposite of it, as noMergedBranch does, so each is named where it is used.
const noDefaultBranchNamed = ""

// answer fills in what one check compared its results with, where each of its results stands,
// and whether a pass of it stands. The check's results are already in place, newest first,
// each carrying its retraction where one stands against it (loadCheckResultRetractions).
//
// The two facts a result states are kept apart and read apart. Where its commit stands is one
// question, answered against the bases, and whether the check's words moved is another,
// answered by the digest the result recorded beside the digest the check carries now. A result
// at the compared commit under an older wording is both, and one word for both would hide
// which of the two a reader is looking at.
func (c *comparisonReader) answer(ctx context.Context, check *model.CheckBlock) {
	compared := c.comparedWith(ctx, *check)
	check.ComparedWith = compared.bases
	check.ComparedNothing = compared.reason
	for index := range check.Results {
		result := &check.Results[index]
		moved := check.SHA256 != result.CheckSHA256
		result.CheckWordsMoved = &moved
		stands := c.commitStands(ctx, *check, compared, *result)
		result.CommitStands = &stands
	}
	check.PassStands = passStandsOf(*check, compared)
}

// comparedWith is the bases of one check, read once per issue and `when`: two `before_merge`
// checks of one issue compare with the same heads, and two `default_branch` checks with the
// same branch. Every head GitHub had to be asked for is in that memo before the first check
// is answered (learn), so the two checks name one list and not two.
//
// A reader that named a commit is answered that commit and nothing else, for every check of
// the issue and whatever its `when`, and no read of GitHub is made for it.
func (c *comparisonReader) comparedWith(ctx context.Context, check model.CheckBlock) comparison {
	if c.at != noCommitNamed {
		return comparison{bases: []model.CheckBasis{{Kind: model.CheckBasisNamed, Commit: c.at}}}
	}
	key := [2]string{check.IssueKey, check.When}
	if memoized, held := c.bases[key]; held {
		return memoized
	}
	var compared comparison
	if check.When == checkWhenBeforeMerge {
		compared = c.headBases(ctx, check.IssueKey)
	} else {
		compared = c.branchBases(ctx, check)
	}
	c.bases[key] = compared
	return compared
}

// headBases is every pull request head Dispatch holds for the check's issue, newest learned
// first, as the bases a `before_merge` check compares its results with. An issue holding none
// compares with nothing, and nothingReason says which of the two kinds of nothing it is.
//
// The heads answer whether or not Dispatch can read the repository. A head Dispatch holds is a
// fact about the work whatever the App can see today, so a result at one stands at it, and
// only a result at no head of the list needs the repository read at all (commitStands).
func (c *comparisonReader) headBases(ctx context.Context, issueKey string) comparison {
	heads, err := delivery.HeadsAttributedTo(ctx, c.server.deps.Store.Pool, issueKey)
	if err != nil {
		slog.Warn("dispatch: read the pull request heads of a check's issue", "issue", issueKey, "error", err)
		return noBases(model.ReasonReadFailed)
	}
	if len(heads) == 0 {
		return noBases(c.nothingReason(ctx, issueKey))
	}
	bases := make([]model.CheckBasis, 0, len(heads))
	for _, head := range heads {
		bases = append(bases, headBasis(head))
	}
	return comparison{bases: bases}
}

// headBasis is one stored pull request head as the surface names it: the commit, the pull
// request's number, and when Dispatch last learned that head with the kind of time it is. The
// later of the two times wins, so a head a webhook moved after the last complete read prints
// GitHub's own update time, named model.BasisLearnedUpdatedAt, and never an older read time. A
// row carrying neither time names none.
func headBasis(head delivery.AttributedHead) model.CheckBasis {
	number := head.Number
	basis := model.CheckBasis{Kind: model.CheckBasisPullRequest, Commit: head.HeadSHA, PullRequest: &number}
	switch {
	case head.EventAt != nil && (head.CheckedAt == nil || head.EventAt.After(*head.CheckedAt)):
		basis.LearnedAt = &model.BasisLearnedAt{At: *head.EventAt, Kind: model.BasisLearnedUpdatedAt}
	case head.CheckedAt != nil:
		basis.LearnedAt = &model.BasisLearnedAt{At: *head.CheckedAt, Kind: model.BasisLearnedReadAt}
	}
	return basis
}

// branchBases is the branches an `after_merge` or a `default_branch` check compares its
// results with, in the order they are asked about (measuredBranches), under the names a reader
// sees. The repository's default branch is named here rather than carried as
// delivery.DefaultBranch, since the surface says which branch it compared against.
//
// The branches are asked about whenever the issue resolves to a repository this process has
// named a default branch for, whether or not the App can read that repository just now. A
// stored answer is keyed by a branch and is read before any call to GitHub, a yes standing for
// good, so the name delivery keeps across a read that could say nothing is the whole of what
// the walk needs to reach it (delivery.Branches.CommitOnBranch). The merge commit of a squash
// merge whose base branch is deleted is what turns on this: nothing but that stored yes can
// ever place it, and refusing the walk while the App cannot be read would turn every
// `after_merge` and every `default_branch` result of the repository from `at_basis` to
// `unknown` for as long as the read fails. Where no branch of the walk holds a stored answer
// the result is unknown all the same, through onNamedBranch's own arms.
func (c *comparisonReader) branchBases(ctx context.Context, check model.CheckBlock) comparison {
	defaultBranch, _ := c.defaultBranchOf(ctx, check.IssueKey)
	if defaultBranch == noDefaultBranchNamed {
		// Either kind of nothing: no repository resolves, or no read of this process ever
		// named a default branch for the one that does, which leaves no branch to ask about.
		// nothingReason tells them apart from the same two memoized reads.
		return noBases(c.nothingReason(ctx, check.IssueKey))
	}
	branches, chosen := c.measuredBranches(ctx, check, defaultBranch)
	if !chosen {
		return noBases(model.ReasonReadFailed)
	}
	bases := make([]model.CheckBasis, 0, len(branches))
	for _, branch := range branches {
		bases = append(bases, model.CheckBasis{Kind: model.CheckBasisBranch, Branch: branch})
	}
	return comparison{bases: bases}
}

// nothingReason tells the two kinds of nothing apart for a check that named no basis: an issue
// resolving to no repository has no pull request and no branch to compare with, while one
// whose repository the App cannot read has a comparison Dispatch cannot make. The first leaves
// a pass standing at nothing, which is a no. The second is unknown, and a reviewer reads it as
// a question Dispatch could not answer rather than as a verdict about the work.
func (c *comparisonReader) nothingReason(ctx context.Context, issueKey string) string {
	if repo := c.repositoryOf(ctx, issueKey); repo == nil {
		return model.ReasonNoPullRequestOrBranch
	}
	if _, readable := c.defaultBranchOf(ctx, issueKey); !readable {
		return model.ReasonRepositoryUnreadable
	}
	return model.ReasonNoPullRequestOrBranch
}

// learn teaches Dispatch the pull requests GitHub names for every result commit of the
// document that heads none of the pull requests it already holds, and reads the heads again
// where GitHub named one: that head is now a row, and so a basis.
//
// It is what stops a result recorded at a commit before any pull request carried it from
// standing at no basis for ever (dispatch://INFRA-1240). It runs over the whole document
// before any check is answered, so every `before_merge` check of one issue names the same
// list of heads: a head one check's result taught Dispatch is a basis the sibling check is
// read against too, whichever came first in the document.
//
// A reader that named a commit asks GitHub nothing at all, and learns nothing here.
//
// Only a `before_merge` check reaches GitHub this way, since a check compared with branches
// is compared with no head. The memo is the pair of the issue and the commit: a pair read
// through once in this read is never read through again, whichever check holds the result
// and whenever it was recorded, since the stored rows can have changed only by this read's
// own read-through.
func (c *comparisonReader) learn(ctx context.Context, checks []model.CheckBlock) {
	if c.at != noCommitNamed {
		return
	}
	for _, check := range checks {
		if check.When != checkWhenBeforeMerge {
			continue
		}
		compared := c.comparedWith(ctx, check)
		for _, result := range check.Results {
			if basisAt(compared.bases, result.CommitSHA) != nil {
				continue
			}
			key := [2]string{result.CheckIssueKey, result.CommitSHA}
			if _, asked := c.readThroughs[key]; asked {
				continue
			}
			named := c.readThroughHead(ctx, result)
			c.readThroughs[key] = struct{}{}
			if !named {
				continue
			}
			compared = c.headBases(ctx, check.IssueKey)
			c.bases[[2]string{check.IssueKey, check.When}] = compared
		}
	}
}

// commitStands is where one result's commit stands: at one of the bases its check named, at
// another commit, or not known with the reason. It says nothing about the check's words, which
// answer carries beside it.
//
// A commit one of the bases names is the whole answer, whatever Dispatch can read now. Where
// the check named no basis, the reason it named none decides: nothing to compare with leaves
// the commit at another commit, while a repository Dispatch could not read leaves it unknown.
// Where the check named bases and none is this commit, a `before_merge` result turns on
// whether the repository can be read at all, and a branch check walks the branches
// (onNamedBranch).
//
// An issue that resolves to no repository is never unknown. Dispatch holds every head it will
// ever hold for such an issue, since there is no repository to read one from, so a commit
// none of them names is another commit and not a question left open.
func (c *comparisonReader) commitStands(
	ctx context.Context, check model.CheckBlock, compared comparison, result model.CheckResult,
) model.ResultCommitStands {
	if basis := basisAt(compared.bases, result.CommitSHA); basis != nil {
		return model.ResultCommitStands{Answer: model.CommitAtBasis, Basis: basis}
	}
	switch compared.reason {
	case model.ReasonNoPullRequestOrBranch:
		return model.ResultCommitStands{Answer: model.CommitOtherCommit}
	case model.ReasonRepositoryUnreadable, model.ReasonReadFailed:
		return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: compared.reason}
	}
	if c.at != noCommitNamed {
		return model.ResultCommitStands{Answer: model.CommitOtherCommit}
	}
	if check.When == checkWhenBeforeMerge {
		// The heads are the whole of this check's comparison, and none of them is this commit.
		// A repository the App cannot read is the one case Dispatch cannot say: nothingReason
		// tells it from an issue that resolves to no repository, where the heads Dispatch
		// holds are every head it will ever hold.
		if c.nothingReason(ctx, check.IssueKey) == model.ReasonRepositoryUnreadable {
			return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: model.ReasonRepositoryUnreadable}
		}
		return model.ResultCommitStands{Answer: model.CommitOtherCommit}
	}
	return c.onNamedBranch(ctx, check, compared, result)
}

// basisAt is the basis naming that commit, and nil where none does. A branch basis names no
// commit, so it is never the answer here: a branch that carries the commit is the walk's
// (onNamedBranch).
func basisAt(bases []model.CheckBasis, sha string) *model.CheckBasis {
	for _, basis := range bases {
		if basis.Commit == sha {
			found := basis
			return &found
		}
	}
	return nil
}

// onNamedBranch walks the branches a check compares with, in the order branchBases named them,
// and answers where the commit stands.
//
// The walk stops at the first branch that carries the commit (delivery.BranchCarries). A yes
// is the whole answer, and no branch after it is asked about, which is what holds an ordinary
// issue's result to the one compare read it cost before a second branch was asked about at
// all.
//
// Where no branch carries the commit, the cases turn on telling a branch the App cannot see
// apart from a read Dispatch could not make at all (delivery.BranchAnswer):
//
//   - Every branch answering delivery.BranchDoesNotCarry is another commit: GitHub said so of
//     each.
//   - An earlier branch answering delivery.BranchUnseen says "this branch does not carry it"
//     and nothing more, and the walk goes on. That is what keeps a base branch GitHub no
//     longer holds, whose compare read answers 404, from turning a result the default branch
//     answers for into an unknown.
//   - An earlier branch answering delivery.BranchUnknown decides nothing either way. The walk
//     goes on, since a later branch may yet carry the commit and that is the whole answer. But
//     where none does the answer is unknown and never another commit. A database fault or a
//     GitHub hiccup must not make a reviewer refuse an approval.
//   - Either of those two as the last word is unknown: a branch that cannot be seen is no
//     evidence about a branch that was never asked about, and a read that failed is no
//     evidence about anything.
func (c *comparisonReader) onNamedBranch(
	ctx context.Context, check model.CheckBlock, compared comparison, result model.CheckResult,
) model.ResultCommitStands {
	repo := c.repositoryOf(ctx, check.IssueKey)
	if repo == nil {
		return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: model.ReasonReadFailed}
	}
	// unknown is whether any branch of the walk answered that Dispatch could not say. It is
	// kept apart from the last branch's own answer because an unknown anywhere outranks every
	// "not on it" after it, while a branch that carries the commit outranks it in turn.
	unknown := false
	answer := delivery.BranchUnknown
	for index, basis := range compared.bases {
		answer = c.onBranch(ctx, *repo, basis.Branch, result.CommitSHA)
		if answer == delivery.BranchCarries {
			carried := compared.bases[index]
			return model.ResultCommitStands{Answer: model.CommitAtBasis, Basis: &carried}
		}
		unknown = unknown || answer == delivery.BranchUnknown
	}
	if unknown || answer == delivery.BranchUnseen {
		return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: model.ReasonReadFailed}
	}
	return model.ResultCommitStands{Answer: model.CommitOtherCommit}
}

// passStandsOf is the one line that answers the gate's question for a check: whether a pass of
// it stands at what it compared with, against the check's words now, and from which roles.
//
// A pass stands when its commit is at one of the bases, the check's words have not moved since
// it was recorded, and no retraction stands against it. The roles are those of the passes that
// stand, in the order the results come, which is newest first.
//
// A check whose repository Dispatch could not read answers unknown, since it could compare
// nothing at all. So does a check whose standing pass is held up only by a commit Dispatch
// could not place. A check that compared with nothing because there is no pull request and no
// branch answers no: there is nothing for a pass to stand at, and that is a fact about the
// work rather than a failed read.
func passStandsOf(check model.CheckBlock, compared comparison) model.CheckPassStands {
	if compared.reason == model.ReasonRepositoryUnreadable {
		return model.CheckPassStands{Answer: model.PassStandsUnknown}
	}
	var roles []string
	unknown := false
	for _, result := range check.Results {
		if result.Verdict != "pass" || result.Retracted != nil {
			continue
		}
		if result.CheckWordsMoved == nil || *result.CheckWordsMoved || result.CommitStands == nil {
			continue
		}
		switch result.CommitStands.Answer {
		case model.CommitAtBasis:
			if !slices.Contains(roles, result.Role) {
				roles = append(roles, result.Role)
			}
		case model.CommitUnknown:
			unknown = true
		}
	}
	switch {
	case len(roles) > 0:
		return model.CheckPassStands{Answer: model.PassStandsYes, Roles: roles}
	case unknown:
		return model.CheckPassStands{Answer: model.PassStandsUnknown}
	}
	return model.CheckPassStands{Answer: model.PassStandsNo}
}

// measuredBranches is the branches a check measured against a branch compares its results
// with, in the order of asks about them, and whether Dispatch could choose them at all. Only
// an `after_merge` and a `default_branch` check reach it, since a `before_merge` check
// compares with pull request heads and asks about no branch.
//
// A `default_branch` check is compared with the repository's default branch alone, whatever
// branch its issue's pull request merged into. That is the whole of the difference between the
// two `when`s, and it is why a check that must speak for the trunk is written `default_branch`
// rather than `after_merge`.
//
// An `after_merge` check is compared with the branch its issue's pull request merged into and
// the repository's default branch, in that order. The merge's own branch comes first because
// it is the branch the work reached, and because asking it first is what keeps the ordinary
// issue — the one that merged into the default branch itself — at one compare read. That issue
// names one branch and not two, since the branch it merged into is the default branch under
// the name delivery resolved. The default branch follows a branch of its own because an
// integration branch that has landed has carried the work onto the trunk, where the base
// branch itself is gone and can answer for nothing.
//
// Where Dispatch holds no merge of the issue at all, or holds one whose row names no base
// branch (noMergedBranch), there is no branch but the default one to ask about. A read that
// failed chooses no branches and answers false, which branchBases reads as a check that can
// compare with nothing it could name: a database fault must not make a reviewer refuse an
// approval.
func (c *comparisonReader) measuredBranches(
	ctx context.Context, check model.CheckBlock, defaultBranch string,
) ([]string, bool) {
	switch check.When {
	case checkWhenAfterMerge:
		merged, read := c.baseBranchMergedFor(ctx, check.IssueKey)
		if !read {
			return nil, false
		}
		if merged == noMergedBranch || merged == defaultBranch {
			return []string{defaultBranch}, true
		}
		return []string{merged, defaultBranch}, true
	default:
		return []string{defaultBranch}, true
	}
}

// readThroughHead teaches Dispatch the pull requests GitHub names for a result's commit
// (delivery.ReadThroughCommitPulls) and reports whether GitHub named one whose head is that
// commit, which is the only answer worth reading the stored heads again for.
//
// It asks GitHub only where GitHub's answer can repair the result, since listCheckResults
// places every result of every check and the Verification tab reads them all again on every
// result.recorded. Two conditions rule out the commits no answer of GitHub's can repair, and
// each is a fact about the repository and the commit, never about the result that is being
// answered:
//
//   - The commit heads an open or merged pull request Dispatch holds (headStoredAt), whatever
//     issue that row is attributed to and whether it is attributed at all. Such a commit is one
//     Dispatch's own passes keep reading — the reconcile re-reads an open row every five minutes
//     and offers a merged one once per attribution rule (delivery.ListStalePullRequestHeads) —
//     so a read from here repairs nothing. This is a child's result on its parent's tab: the row
//     is the child's own pull request, attributed to the child, and no answer of GitHub's puts
//     that commit among the parent's heads, since the issue a result was recorded on counts for
//     nothing here. A closed-unmerged row is counted for nothing, because the reconcile keeps
//     none and a pull request closed at a commit does not stop another opening from the same
//     head.
//   - Or a read within the last delivery.CommitPullSettledFor has asked GitHub about that
//     repository and commit and been told of a pull request whose head is another commit
//     (delivery.CommitPullReads.Settle, which that read records and Claim refuses from). GitHub
//     has answered about this commit for now — its answer put the pull request's head past it —
//     so the answer is believed for that span and nothing a read inside it can learn repairs the
//     result. It is a span and not for ever because a pull request opened later can have its
//     head at that commit, which the next read after the span sees. GitHub naming no pull
//     request at all is settled for no span at all, since one may open at the commit a moment
//     later.
//
// Three tests cost nothing and bound the rest. A deployment holding no App key can ask nothing.
// A repository Dispatch cannot see is the unknown answer whatever GitHub would say, and the
// read of its default branch is the test commitStands makes next, so it decides here first and
// costs nothing twice (defaultBranchOf's memo). It also answers false for an issue resolving to
// no repository, which is why the repository read below has one. And one commit is read through
// at most once every delivery.CommitPullReadStaleAfter, however many reads ask:
// server.commitPulls.Claim tests that bound and records the read together, so two reads that
// ask at once spend one call.
//
// The read is against the canonical repository the check's issue resolves to (repositoryOf),
// the same issue headBases asks the stored rows about. A read that fails leaves the bases
// exactly as the stored rows say, which is what a result reads with no read-through at all: a
// GitHub call that did not land is no evidence either way.
func (c *comparisonReader) readThroughHead(ctx context.Context, result model.CheckResult) bool {
	if c.server.deps.GitHub == nil {
		return false
	}
	if _, readable := c.defaultBranchOf(ctx, result.CheckIssueKey); !readable {
		return false
	}
	repo := c.repositoryOf(ctx, result.CheckIssueKey)
	if c.headStoredAt(ctx, *repo, result.CommitSHA) {
		return false
	}
	now := time.Now()
	if !c.server.commitPulls.Claim(*repo, result.CommitSHA, now) {
		return false
	}
	written, atHead, err := delivery.ReadThroughCommitPulls(
		ctx, c.server.deps.Store.Pool, c.server.deps.GitHub, c.server.deps.Events,
		*repo, result.CommitSHA, now)
	if err != nil {
		slog.Info("dispatch: GitHub named no pull request for a result's commit",
			"repo", *repo, "commit", result.CommitSHA, "error", err)
		return false
	}
	if written > 0 && !atHead {
		c.server.commitPulls.Settle(*repo, result.CommitSHA, now)
	}
	return atHead
}

// headStoredAt is whether Dispatch holds an open or merged pull request whose head is that
// commit of that repository (delivery.HeadStoredAt), read once per read of the results. A read
// that failed answers that it may hold one, so a database hiccup spends no GitHub call and the
// bases stand exactly as the stored rows leave them, as they do after any failed read here.
func (c *comparisonReader) headStoredAt(ctx context.Context, repo, sha string) bool {
	memo := [2]string{repo, sha}
	if stored, memoized := c.storedHeads[memo]; memoized {
		return stored
	}
	stored, err := delivery.HeadStoredAt(ctx, c.server.deps.Store.Pool, repo, sha)
	if err != nil {
		slog.Warn("dispatch: read the stored pull request heads of a commit",
			"repo", repo, "commit", sha, "error", err)
		stored = true
	}
	c.storedHeads[memo] = stored
	return stored
}

// checkIssueRepository is the repository a check's issue resolves to: its own, the nearest one
// above it, or its project's default (refs.IssueRepository), under the canonical name that
// repository is called today (refs.CanonicalRepo); nil when none resolves or either read
// failed, which is the unknown answer either way. q is the pool or transaction the two reads
// are made on.
//
// The canonical name is taken here, before anything asks GitHub, because everything downstream
// takes this answer: the results read's default branch and onBranch, and the record route's
// refusal of a commit the repository does not hold and its read-through of the pull requests
// GitHub names for that commit (results.go, refuseUnknownCommit and readThroughPullRequests).
// An issue recorded under a name that was renamed away would otherwise have the App asked
// about that name, which GitHub answers 404 for, and every result of that issue would read
// unknown. A repository with no rename is its own canonical name, so this costs one read and
// changes no answer for it.
//
// Both routes resolve through this one function: two hand-kept copies of this resolve are how
// a refusal and a result's answer come to name different repositories.
//
// It answers a repository whether or not Dispatch can read it — the deployment holding no App
// key, the App having lost the repository — because the results read needs it for an answer
// that asks GitHub nothing: onBranch answers from the branch_commits row Dispatch already
// holds while that row stands, with no call at all (delivery.Branches.CommitOnBranch), and a
// nil here would turn every such answer unknown. branchBases walks the branches for that row
// whenever Dispatch can name one with no read of GitHub, readable just now or not: an
// `after_merge` check whose issue merged asks the merge's own branch first, and every check
// compared with a default branch asks the name this process has already resolved once, which
// delivery keeps for the purpose. Only a repository no read of this process ever named a
// default branch for has no branch to ask the stored answer about, since a row is keyed by
// one, and that is the one repository a branch check compares nothing of. The record route
// makes the no-App-key check at its own call site, where every use of the answer is a GitHub
// round trip.
func (s *server) checkIssueRepository(ctx context.Context, q refs.Queryer, issueKey string) *string {
	repo, err := refs.IssueRepository(ctx, q, issueKey)
	if err != nil {
		slog.Warn("dispatch: resolve the repository of a check's issue", "issue", issueKey, "error", err)
		return nil
	}
	if repo == nil {
		return nil
	}
	canonical, err := refs.CanonicalRepo(ctx, q, *repo)
	if err != nil {
		slog.Warn("dispatch: canonical name of a check's repository", "issue", issueKey, "repo", *repo, "error", err)
		return nil
	}
	return &canonical
}

// repositoryOf is checkIssueRepository read once per read of the results. The memo keeps a nil
// as it keeps a repository, so an issue that resolves to none and a read that failed cost one
// resolve apiece rather than one per result.
func (c *comparisonReader) repositoryOf(ctx context.Context, issueKey string) *string {
	if repo, memoized := c.repos[issueKey]; memoized {
		return repo
	}
	repo := c.server.checkIssueRepository(ctx, c.server.deps.Store.Pool, issueKey)
	c.repos[issueKey] = repo
	return repo
}

// defaultBranchOf is the default branch of the repository the check's issue resolves to and
// whether Dispatch can read that repository at all: it resolves to one, and the App answered
// that repository's default branch (delivery.Branches.DefaultBranchOf). It is read once per
// repository per read of the results. An issue resolving to no repository answers no branch
// and no.
func (c *comparisonReader) defaultBranchOf(ctx context.Context, issueKey string) (string, bool) {
	repo := c.repositoryOf(ctx, issueKey)
	if repo == nil {
		return "", false
	}
	if answer, memoized := c.defaults[*repo]; memoized {
		return answer.branch, answer.readable
	}
	branch, readable := c.server.branches.DefaultBranchOf(ctx, c.server.deps.GitHub, *repo, time.Now())
	c.defaults[*repo] = defaultBranchAnswer{branch: branch, readable: readable}
	return branch, readable
}

// baseBranchMergedFor is the branch the check's issue's newest merged pull request merged into
// (delivery.BaseBranchMergedFor), read once per read of the results, and whether the read
// answered at all. An issue Dispatch holds no merge of answers noMergedBranch, which
// measuredBranches reads as no branch but the repository's default one to ask about.
//
// The memo keeps a failed read as it keeps an answered one, so a second result of the same
// issue neither reads again nor is quietly compared with the default branch because the first
// read failed. The key is the issue alone: the branch an issue's work merged into is a fact
// about that issue, not about any one result or commit of it.
func (c *comparisonReader) baseBranchMergedFor(ctx context.Context, issueKey string) (string, bool) {
	if answer, memoized := c.baseBranches[issueKey]; memoized {
		return answer.branch, answer.read
	}
	branch, err := delivery.BaseBranchMergedFor(ctx, c.server.deps.Store.Pool, issueKey)
	if err != nil {
		slog.Warn("dispatch: read the branch a result's issue merged into", "issue", issueKey, "error", err)
	}
	answer := baseBranchAnswer{branch: branch, read: err == nil}
	c.baseBranches[issueKey] = answer
	return answer.branch, answer.read
}

// onBranch is delivery's answer about one commit of one branch, read once per read of the
// results. The branch is named, since branchBases resolved the repository's default branch
// before it named the bases, and the name is in the memo's key as any other branch's is: one
// commit has as many answers as the branches asked about it — the two an `after_merge` and a
// `default_branch` check of one issue ask, and the two an `after_merge` check itself asks of a
// commit its base branch does not carry.
//
// The memo holds the answer's own kind, so a branch the App cannot see and a read that failed
// reach the walk as the different things they are: one is "not on that branch", the other is
// no answer at all (onNamedBranch).
func (c *comparisonReader) onBranch(ctx context.Context, repo, branch, sha string) delivery.BranchAnswer {
	memo := [3]string{repo, branch, sha}
	if answer, memoized := c.commits[memo]; memoized {
		return answer
	}
	answer := c.server.branches.CommitOnBranch(
		ctx, c.server.deps.Store.Pool, c.server.deps.GitHub, repo, branch, sha, time.Now())
	c.commits[memo] = answer
	return answer
}
