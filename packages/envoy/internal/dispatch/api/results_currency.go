package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// results_currency.go says whether each recorded result still speaks for the work as it
// stands, and which branches each check is measured against. It holds no policy about GitHub:
// what is asked, how long an answer is believed and what is written back are delivery's
// (delivery/branch.go, delivery/commit_pull_reads.go, delivery.HeadAttributedTo,
// delivery.ReadThroughCommitPulls), and what lives here is the choice of branches
// (measuredBranches) and one memo per read, so a tab listing twenty results against a handful
// of commits costs a handful of round trips.

// currencyReader answers the currency of every result of one read of one issue's results.
//
// A `before_merge` result is current while its commit is the head of an open pull request
// attributed to the check's own issue, or the final head of a merged one, and the check's words
// have not changed since: a push that moves the head makes it older with no write of
// Dispatch's, and rewording the check makes its own results older and no other's. The issue a
// result was recorded on counts for nothing here, so a child's pull request never makes a
// result against its parent's check current; a child carrying part of a parent's requirement
// proves it with a check of its own whose `serves` names that requirement.
//
// An `after_merge` result is current while its commit is on the branch the check's issue's pull
// request merged into — the base branch of the newest merged pull request attributed to that
// issue (delivery.BaseBranchMergedFor) — or on the repository's default branch, and the words
// match. An issue whose work merged into an integration branch has reached that branch, so a
// commit that branch carries proves the work before the integration branch itself lands; and
// once that branch has landed, the trunk carries the work too, and answers for it where the
// base branch — deleted on the merge, as this repository deletes every merged branch — can
// answer nothing at all. So a base branch GitHub no longer holds is read as "this commit is
// not on it" and never as unknown, and the default branch decides (of, measuredBranches).
// Where Dispatch knows of no merge of that issue, the default branch answers alone, which is
// every issue merged the ordinary way before Dispatch was told of the merge and every issue
// whose merge Dispatch was never told of.
//
// The merge's own commit goes on answering on the base branch after the branch is gone,
// because Dispatch recorded it as on that branch when it recorded the merge
// (delivery.UpsertPullRequestHead) and a yes is kept for good. In a repository that merges by
// squash that commit never lands on the default branch at all, so without that record a result
// at the merge commit would read `older` for ever once the base branch was deleted.
//
// A `default_branch` result is current while its commit is on the default branch of the check's
// issue's resolved repository and the words match, whatever branch the pull request merged
// into. That is the whole of the difference between the two `when`s, and it is why a check that
// must speak for the trunk is written `default_branch` rather than `after_merge`.
//
// Where Dispatch cannot see the repository the check's issue resolves to — it resolves to none,
// the App is installed on none that covers it, or a read failed — the answer is unknown and the
// commit is shown with neither mark, whatever the check's `when`: nothing can have taught
// Dispatch that commit's pull request, so "no head" is no evidence that the work has moved on.
// Dispatch's own failed read is unknown for the same reason, the read of the branch an issue's
// pull request merged into among them: a database hiccup must not make a reviewer refuse an
// approval.
//
// The repository each issue resolves to, whether the App can read it, the branch each issue's
// pull request merged into, the pull request heads of each issue-and-commit pair, the stored
// heads of each repository-and-commit pair and GitHub's answer about each repository, branch
// and commit are each read once per read of the results. The branch is in that last key, and
// not the repository and the commit alone, because one issue's Verification document may hold
// an `after_merge` check and a `default_branch` check with a result at the same commit: two
// questions, with two answers, that one key for both would collapse into whichever was asked
// first. Across reads, a repository's default branch, a commit's place on a branch and the last
// read-through of a commit's pull requests are held by delivery (server.branches,
// branch_commits and server.commitPulls).
type currencyReader struct {
	server       *server
	repos        map[string]*string
	readable     map[string]bool
	baseBranches map[string]baseBranchAnswer
	heads        map[[2]string]bool
	storedHeads  map[[2]string]bool
	commits      map[[3]string]commitAnswer
}

// commitAnswer is what delivery said about one commit: whether the branch asked about carries
// it, and whether Dispatch could say at all.
type commitAnswer struct {
	onBranch bool
	known    bool
}

// baseBranchAnswer is what Dispatch holds about the merge of one issue: the branch its newest
// merged pull request merged into, noMergedBranch where it holds no merge of that issue at
// all, and whether the read answered. The failed read is kept apart from the empty branch
// because they part the two ways: a read that failed is the unknown currency, while no merge
// found leaves the repository's default branch to answer alone (measuredBranches). One flag
// for both would turn a database fault into a verdict about the work.
type baseBranchAnswer struct {
	branch string
	read   bool
}

func newCurrencyReader(s *server) *currencyReader {
	return &currencyReader{
		server: s, repos: map[string]*string{}, readable: map[string]bool{},
		baseBranches: map[string]baseBranchAnswer{},
		heads:        map[[2]string]bool{}, storedHeads: map[[2]string]bool{},
		commits: map[[3]string]commitAnswer{},
	}
}

// noMergedBranch is what delivery.BaseBranchMergedFor answers for an issue Dispatch holds no
// merge of, and for a merged row whose base_ref is empty: no branch to measure against. It is
// spelled like delivery.DefaultBranch and means the opposite of it — "there is nothing here to
// ask about" rather than "ask about the repository's own default branch" — so each is named
// where it is used. The fallback below reads one for the other on purpose, and naming both is
// what makes that a decision of this file's rather than an agreement between two packages that
// either could end without a compiler saying so.
const noMergedBranch = ""

// of is the currency of one of a check's results. It reads that one result and that one check:
// what the check's other results hold counts for nothing here, since a result is measured
// against the work as it stands and never against another result (headOf).
//
// The check's `when` picks what the result is measured against: the pull request heads of the
// check's own issue for a `before_merge` check, and one or more branches for the other two,
// which measuredBranches chooses once and this walks in order. The issue measured against is
// the check's, never the one the result was recorded on, which is the rule headOf keeps for a
// pull request head and measuredBranches keeps for a merge.
//
// The walk stops at the first branch that carries the commit: a yes is the whole answer, and
// no branch after it is asked about, which is what holds an ordinary issue's result to the one
// compare read it cost before a second branch was asked about at all. Where no branch carries
// it, the last read's own answer stands — a no is `older`, and only an unknown from that last
// read is the unknown currency. An earlier branch Dispatch could not get an answer about says
// "not on that branch" and nothing more: that is what keeps a base branch GitHub no longer
// holds, whose compare read answers 404 and whose repository the client therefore reads as
// unreadable, from turning a result the default branch answers for into an unknown.
func (c *currencyReader) of(ctx context.Context, check model.CheckBlock, result model.CheckResult) string {
	if check.When == checkWhenBeforeMerge {
		atHead, known := c.headOf(ctx, result)
		switch {
		case !known:
			return model.CurrencyUnknown
		case atHead:
			if check.SHA256 == result.CheckSHA256 {
				return model.CurrencyCurrent
			}
			return model.CurrencyOlder
		case !c.repositoryReadable(ctx, result.CheckIssueKey):
			return model.CurrencyUnknown
		}
		return model.CurrencyOlder
	}
	repo := c.repositoryOf(ctx, result.CheckIssueKey)
	if repo == nil {
		return model.CurrencyUnknown
	}
	branches, chosen := c.measuredBranches(ctx, check, result)
	if !chosen {
		return model.CurrencyUnknown
	}
	var onBranch, known bool
	for _, branch := range branches {
		onBranch, known = c.onBranch(ctx, *repo, branch, result.CommitSHA)
		if onBranch {
			break
		}
	}
	if !known {
		return model.CurrencyUnknown
	}
	if onBranch && check.SHA256 == result.CheckSHA256 {
		return model.CurrencyCurrent
	}
	return model.CurrencyOlder
}

// measuredBranches is the branches a result of a check measured against a branch is current at
// a commit of, in the order of asks about them, and whether Dispatch could choose them at all.
// Only an `after_merge` and a `default_branch` check reach it, since of answers a
// `before_merge` one from the pull request heads before it asks about any branch.
//
// A `default_branch` check is measured against the repository's default branch alone, whatever
// branch its issue's pull request merged into. That is the whole of the difference between the
// two `when`s, and it is why a check that must speak for the trunk is written `default_branch`
// rather than `after_merge`.
//
// An `after_merge` check is measured against the branch its issue's pull request merged into
// and the repository's default branch, in that order. The merge's own branch comes first
// because it is the branch the work reached, and because asking it first is what keeps the
// ordinary issue — the one that merged into the default branch itself — at one compare read:
// the second question is then about the same branch under its resolved name, and
// delivery.CommitOnBranch reads the branch_commits row the first question wrote before it asks
// GitHub anything. The default branch follows because an integration branch that has landed
// has carried the work onto the trunk, where the base branch itself is gone and can answer for
// nothing.
//
// Where Dispatch holds no merge of the issue at all, or holds one whose row names no base
// branch (noMergedBranch), there is no branch but the default one to ask about. A read that
// failed chooses no branches and answers false, which of reads as the unknown currency: a
// database fault must not make a reviewer refuse an approval.
func (c *currencyReader) measuredBranches(
	ctx context.Context, check model.CheckBlock, result model.CheckResult,
) ([]string, bool) {
	switch check.When {
	case checkWhenAfterMerge:
		merged, read := c.baseBranchMergedFor(ctx, result.CheckIssueKey)
		if !read {
			return nil, false
		}
		if merged == noMergedBranch {
			return []string{delivery.DefaultBranch}, true
		}
		return []string{merged, delivery.DefaultBranch}, true
	default:
		return []string{delivery.DefaultBranch}, true
	}
}

// headOf reports whether a result's commit is the head of a pull request attributed to the
// check's own issue, and whether Dispatch's own read could say at all.
//
// A commit that heads no such row may be one Dispatch has never been told a pull request for at
// all: the read-through a record makes (results.go's readThroughPullRequests) ran before the
// pull request was opened, a deployment holding no delivery settings binds no `pull_request`
// durable and so consumes no such event ever, and the reconcile's head pass only re-reads rows
// that already exist. So a "no" is read through wherever GitHub's answer can repair it
// (readThroughHead), which is how a result recorded at a commit before any pull request carried
// it stops reading `older` for ever (dispatch://INFRA-1240).
//
// The memo is keyed by the issue and the commit, and every input of the answer is in that key:
// the stored rows are asked about that pair, and whether GitHub is asked is decided from the
// issue's repository and the commit alone (readThroughHead). So the answer the first result of
// an issue gets at a commit is the answer every result of that issue gets at that commit,
// whichever check recorded it and whenever it was recorded — which is what makes one memo per
// pair sound, and what a rule reading anything a result holds of its own would break.
//
// The stored rows are read once, and again only after a read-through that named a pull request
// at that commit: nothing else can have changed them between the two reads. Either read failing
// is the unknown answer, handled in the one place both reach.
func (c *currencyReader) headOf(ctx context.Context, result model.CheckResult) (bool, bool) {
	key := [2]string{result.CheckIssueKey, result.CommitSHA}
	if atHead, memoized := c.heads[key]; memoized {
		return atHead, true
	}
	attributed := func() (bool, error) {
		return delivery.HeadAttributedTo(ctx, c.server.deps.Store.Pool, result.CheckIssueKey, result.CommitSHA)
	}
	atHead, err := attributed()
	if err == nil && !atHead && c.readThroughHead(ctx, result) {
		atHead, err = attributed()
	}
	if err != nil {
		slog.Warn("dispatch: read a result's pull request head", "issue", result.CheckIssueKey, "error", err)
		return false, false
	}
	c.heads[key] = atHead
	return atHead, true
}

// readThroughHead teaches Dispatch the pull requests GitHub names for a result's commit
// (delivery.ReadThroughCommitPulls) and reports whether GitHub named one whose head is that
// commit, which is the only answer worth asking the stored heads again for.
//
// It asks GitHub only where GitHub's answer can repair the result, since listCheckResults reads
// the currency of every result of every check and the Verification tab reads them all again on
// every result.recorded. Two conditions rule out the commits no answer of GitHub's can repair,
// and each is a fact about the repository and the commit, never about the result that is being
// answered:
//
//   - The commit heads an open or merged pull request Dispatch holds (headStoredAt), whatever
//     issue that row is attributed to and whether it is attributed at all. Such a commit is one
//     Dispatch's own passes keep reading — the reconcile re-reads an open row every five minutes
//     and offers a merged one once per attribution rule (delivery.ListStalePullRequestHeads) —
//     so a read from here repairs nothing. This is a child's result on its parent's tab: the row
//     is the child's own pull request, attributed to the child, and no answer of GitHub's makes
//     that result current, since the issue a result was recorded on counts for nothing here. A
//     closed-unmerged row is counted for nothing, because the reconcile keeps none and a pull
//     request closed at a commit does not stop another opening from the same head.
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
// A repository Dispatch cannot see is the unknown answer whatever GitHub would say, and it is the
// test the `of` switch makes next, so it decides here first and costs nothing twice
// (repositoryReadable's memo); it also answers false for an issue resolving to no repository,
// which is why the repository read below has one. And one commit is read through at most once
// every delivery.CommitPullReadStaleAfter, however many reads ask: server.commitPulls.Claim
// tests that bound and records the read together, so two reads that ask at once spend one call.
//
// The read is against the canonical repository the check's issue resolves to (repositoryOf), the
// same pair headOf asks the stored rows about. A read that fails leaves the currency exactly as
// the stored rows say, which is what a result reads with no read-through at all: a GitHub call
// that did not land is no evidence either way.
func (c *currencyReader) readThroughHead(ctx context.Context, result model.CheckResult) bool {
	if c.server.deps.GitHub == nil || !c.repositoryReadable(ctx, result.CheckIssueKey) {
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
		ctx, c.server.deps.Store.Pool, c.server.deps.GitHub, *repo, result.CommitSHA, now)
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
// currency stands exactly as the stored rows leave it, as it does after any failed read here.
func (c *currencyReader) headStoredAt(ctx context.Context, repo, sha string) bool {
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
// takes this answer: the results read's repositoryReadable and onBranch, and the record
// route's refusal of a commit the repository does not hold and its read-through of the pull
// requests GitHub names for that commit (results.go, refuseUnknownCommit and
// readThroughPullRequests). An issue recorded under a name that was renamed away would
// otherwise have the App asked about that name, which GitHub answers 404 for, and every result
// of that issue would read unknown. A repository with no rename is its own canonical name, so
// this costs one read and changes no answer for it.
//
// Both routes resolve through this one function: two hand-kept copies of this resolve are how a
// refusal and a currency mark come to name different repositories.
//
// It answers a repository whether or not the deployment holds an App key, because the currency
// reader needs it for an answer that asks GitHub nothing: onBranch answers from the
// branch_commits row Dispatch already holds while that row stands, with no call at all
// (delivery.Branches.CommitOnBranch), and a nil here would turn every such answer unknown.
// That holds for a check measured against a branch Dispatch can name with no read of GitHub:
// an `after_merge` check whose issue merged, where the branch is the merge's own, and any
// check measured against a default branch this process has already resolved once, which
// delivery keeps for the purpose. Only a repository no read of this process ever named a
// default branch for has no branch to ask the stored answer about, since a row is keyed by
// one. The record route makes the no-App-key check at its own call site, where every use of
// the answer is a GitHub round trip.
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
func (c *currencyReader) repositoryOf(ctx context.Context, issueKey string) *string {
	if repo, memoized := c.repos[issueKey]; memoized {
		return repo
	}
	repo := c.server.checkIssueRepository(ctx, c.server.deps.Store.Pool, issueKey)
	c.repos[issueKey] = repo
	return repo
}

// repositoryReadable reports whether Dispatch can see the repository the check's issue resolves
// to: it resolves to one, and the App answered that repository's default branch.
func (c *currencyReader) repositoryReadable(ctx context.Context, issueKey string) bool {
	repo := c.repositoryOf(ctx, issueKey)
	if repo == nil {
		return false
	}
	if readable, memoized := c.readable[*repo]; memoized {
		return readable
	}
	readable := c.server.branches.RepositoryReadable(ctx, c.server.deps.GitHub, *repo, time.Now())
	c.readable[*repo] = readable
	return readable
}

// baseBranchMergedFor is the branch the check's issue's newest merged pull request merged into
// (delivery.BaseBranchMergedFor), read once per read of the results, and whether the read
// answered at all. An issue Dispatch holds no merge of answers noMergedBranch, which
// measuredBranches reads as no branch but the repository's default one to ask about.
//
// The memo keeps a failed read as it keeps an answered one, so a second result of the same
// issue neither reads again nor is quietly measured against the default branch because the
// first read failed. The key is the issue alone: the branch an issue's work merged into is a
// fact about that issue, not about any one result or commit of it.
func (c *currencyReader) baseBranchMergedFor(ctx context.Context, issueKey string) (string, bool) {
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
// results. delivery.DefaultBranch is the repository's default branch, which delivery resolves;
// it is in the memo's key as any other branch is, since one commit has as many answers as the
// branches asked about it — the two an `after_merge` and a `default_branch` check of one issue
// ask, and the two an `after_merge` check itself asks of a commit its base branch does not
// carry.
func (c *currencyReader) onBranch(ctx context.Context, repo, branch, sha string) (bool, bool) {
	memo := [3]string{repo, branch, sha}
	if answer, memoized := c.commits[memo]; memoized {
		return answer.onBranch, answer.known
	}
	onBranch, known := c.server.branches.CommitOnBranch(
		ctx, c.server.deps.Store.Pool, c.server.deps.GitHub, repo, branch, sha, time.Now())
	c.commits[memo] = commitAnswer{onBranch: onBranch, known: known}
	return onBranch, known
}
