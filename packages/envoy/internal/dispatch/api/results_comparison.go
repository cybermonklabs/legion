package api

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// results_comparison.go says what each check compares its results with, where each result's
// commit stands against that, and whether a pass of the check stands. It holds no policy about
// GitHub: what is asked, how long an answer is believed and what is written back are
// delivery's (delivery/branch.go, delivery/commit_pull_reads.go,
// delivery.Attributor.HeadsAttributedTo, delivery.Attributor.ReadThroughCommitPulls). What lives
// here is the choice of bases (comparedWith, measuredBranches) and one memo per read, so a tab
// listing twenty results against a handful of commits costs a handful of round trips.
//
// A read-only caller's read (model.Credential.ReadOnly) asks GitHub what a writer's read made at
// the same moment asks and answers what it answers, and stores nothing: no pull_request_heads,
// pull_request_texts or branch_commits row, no settle of an owed read and no event, and no claim,
// hold, seen mark or default branch in this process's caches or in commit_pull_reads, so it keeps
// nothing it learned after its request (plan D32). Two things of the process change as they do
// for any read, since neither changes an answer or a GitHub call: the GitHub client's token cache
// fills as its calls mint installation tokens (githubapp.Client.RepositoryToken), and a lapsed
// hold it reads is deleted, as a writer's read deletes it (delivery.Branches' unseenHeld and
// isDefaultBranch). It reads the writer's claims and settles of a commit
// (server.commitPulls.RefusesAcrossProcesses) without taking one. Each write a writer's
// read-through would make, its own read-through makes with the writer's own code in a
// transaction that it rolls back, and keeps for the rest of its own request (answers): each later
// read of the stored rows runs in such a transaction after those writes, so it reads the rows a
// writer's read would read at that place, and its branch reads carry the default branch and the
// pairs refused as unseen that its own reads found (carried), in the writer's order and before
// the same exits, so the two make the same GitHub calls and give the same answer.

// resultsNow is the moment a results read takes each of its moments from: server.resultsClock's,
// or time.Now's where that is nil, as it is on every server the deployment builds.
func (s *server) resultsNow() time.Time {
	if s.resultsClock != nil {
		return s.resultsClock()
	}
	return time.Now()
}

// comparisonReader answers one read of one issue's results: for each check the bases its
// results are compared with, and for each result where its commit stands among them.
//
// A `before_merge` check is compared with every pull request head Dispatch holds for the
// check's own issue (delivery.Attributor.HeadsAttributedTo), open or merged: the pull requests
// attributed to it, and those its external links name in a repository it does not resolve to,
// which the attribution can never give it. The issue a result was recorded on counts for nothing
// here, so a child's pull request never answers for a check of its parent. A child carrying part
// of a parent's requirement proves it with a check of its own whose `serves` names that
// requirement.
//
// An `after_merge` check is compared with the branch the check's issue's pull request merged into —
// the base branch of the newest merged pull request attributed to that issue
// (delivery.Attributor.BaseBranchMergedFor) — and with the repository's default branch. An issue
// whose work merged into an integration branch has reached that branch, so a commit that branch
// carries proves the work before the integration branch itself lands. Once that branch has landed
// the trunk carries the work too, and answers for it where the base branch — deleted on the merge,
// as a repository that deletes every merged branch does — can answer nothing at all. So a base
// branch GitHub no longer holds reads as "this commit is not on it" and never as unknown, and the
// default branch decides (onNamedBranch, measuredBranches). Where Dispatch knows of no merge of
// that issue the default branch answers alone, which is every issue merged the ordinary way before
// Dispatch was told of the merge and every issue whose merge Dispatch was never told of.
//
// The merge's own commit goes on standing at the base branch after the branch is gone, because
// Dispatch recorded it as on that branch when it recorded the merge
// (delivery.Attributor.UpsertPullRequestHead) and a yes is kept for good. In a repository that
// merges by squash that commit never lands on the default branch at all, so without that record a
// result at the merge commit would stand at no basis for ever once the base branch was deleted.
//
// A `default_branch` check is compared with the default branch of the check's issue's resolved
// repository alone, whatever branch the pull request merged into. That is the whole of the
// difference between the two `when`s, and it is why a check that must speak for the trunk is
// written `default_branch` rather than `after_merge`.
//
// A branch check's result whose row names a linked repository — one the check's issue links a
// pull request in and does not resolve to — as its commit's is compared with that repository's
// branches instead (model.CheckResult.CommitRepo, which the record route stores where
// refuseUnknownCommit takes the commit to be that repository's; results_linked.go,
// onNamedBranch). An `after_merge` check asks the branch the merged pull request the issue links
// there merged into (delivery.Attributor.BaseBranchMergedIn) and then that repository's default
// branch, in the order and at the cost the issue's own repository is asked in, and a
// `default_branch` check asks that default branch alone. Asked of the issue's own repository,
// GitHub answers 404 for such a commit, and the result at the merge commit of the linked pull
// request would read unknown for good. A result whose row names no such repository — every
// result at a commit of the issue's own repository, and every row recorded before the column
// existed — is compared in the issue's own repository, at the reads it always cost. What the
// check names as its bases stays the issue's own repository's branches either way; a basis in a
// linked repository, a linked pull request's head or such a result's branch, names that
// repository (model.CheckBasis.Repo).
//
// Three answers the surface used to collapse into one are kept apart here. A check with nothing
// to compare with — an issue that resolves to no repository, and a `before_merge` check whose
// issue holds no pull request at all — names model.ReasonNoPullRequestOrBranch, and its
// results stand at no basis, so no pass of it stands. A check Dispatch can compare nothing of
// names model.ReasonRepositoryUnreadable, and every result of it is unknown: a `before_merge`
// check whose repository the App cannot read, where nothing can have taught Dispatch that
// commit's pull request, so "no head" is no evidence that the work has moved on; and a branch
// check of a repository no read of this process ever named a default branch for whose issue
// merged into no named branch either, where there is no branch at all to ask a stored answer
// about. A branch check Dispatch can name a branch for goes on comparing with it while the
// App cannot read the repository, since the stored answer is keyed by that name and a yes
// stands for good (branchBases). Dispatch's own failed read is model.ReasonReadFailed for the
// same reason: the read of the branch an issue's pull request merged into, and the resolve of
// the repository the check's issue names at all (repositoryOf), among them. A database hiccup
// must not make a reviewer refuse an approval.
//
// Two more answers say what Dispatch does not know yet. model.ReasonHeadNotRead names a pull
// request of the issue whose head Dispatch has not read: it has no results row, or a read of
// GitHub is owed for it (headBases). model.ReasonIssueNotKnown names a pull request of the
// repository whose answer is not known: its facts name no held issue, and its commit messages
// are not read (notKnownOf). Either can be the head a pass must stand at, so a result it can
// decide is unknown and never another commit. Both change an answer only where Dispatch holds
// the repository and can read it (decides), so a deployment with no App key answers as before.
//
// Where the reader was given a commit (?at=), that one commit is the only basis of every check
// of the issue and the comparison makes no read of GitHub at all: the reader asked what the
// results say about one commit, and nothing about that question turns on what GitHub holds now.
// The check runs of a check CI proves are read apart from this reader (deriveCIResults).
//
// The repository each issue resolves to, its default branch, the branch each issue's pull
// request merged into, the pull request heads of each issue and GitHub's answer about each
// repository, branch and commit are each read once per read of the results, and so is the branch
// each issue's linked pull request merged into in each linked repository a result's row takes its
// commit to be of, one GitHub said holds that commit when the result was recorded. The branch is
// in that last key of GitHub's, and not the repository and the commit alone, because one issue's
// Verification document may hold an `after_merge` check and a `default_branch` check with a
// result at the same commit: two questions, with two answers, that one key for both would
// collapse into whichever was asked first. The canonical name of such a linked repository is no
// read: it comes from the rename pairs Dispatch holds in memory (refs.Renames.Canonical over
// Deps.Renames). Across reads, a repository's default branch, a commit's place on a branch and
// the last read-through of a commit's pull requests are held by delivery (server.branches,
// branch_commits, server.commitPulls and commit_pull_reads). A read-only caller's read writes
// none of those: it reads them as a writer's read does, and carries what a writer's read would
// write for the rest of its own request alone (answers, carried).
type comparisonReader struct {
	server *server
	// readOnly is whether the request the reader reads for was made with a read-only credential
	// (model.Credential.ReadOnly), read once when the reader is made from the caller the read-only
	// gate put on the request's context (refuseReadOnlyToken): the gate puts every caller that can
	// be read-only there, a broker-issued token and a personal token alike, and a cookie, the
	// shared token and a service-account token never are. optionalActor answers the handler the
	// same caller, and it cannot change within one read.
	readOnly bool
	// at is the one commit the reader named, and noCommitNamed where it named none.
	at                 string
	repos              map[string]repositoryAnswer
	defaults           map[string]defaultBranchAnswer
	baseBranches       map[string]baseBranchAnswer
	linkedBaseBranches map[[2]string]baseBranchAnswer
	bases              map[[2]string]comparison
	readThroughs       map[[2]string]struct{}
	commits            map[[3]string]delivery.BranchAnswer
	// notKnown is the open and merged pull requests of each repository whose answer is not known
	// (notKnownOf), by canonical name. Only answer fills it, after learn, so a pull request that
	// the same read learns is in it.
	notKnown map[string]notKnownAnswer
	// answers is, for a read-only caller's read, each write its own read-throughs would have made
	// had a writer made them, in order, and stored nowhere (delivery.HeadAnswers): every read of
	// the stored heads, of the merged branch, in the issue's own repository and in a linked one,
	// of the newest head a check CI proves is derived at, and of the pull requests whose answer is
	// not known, runs in a transaction that makes those writes with the writer's own code and is
	// rolled back after the read (delivery.HeadAnswers.Read), and a commit's branch answer reads a
	// merge among them as its yes (delivery.Branches.ReadCommitOnBranch), which is what a writer's
	// read reads once its read-throughs have stored them. A writer's read stores what it finds and
	// passes this empty, which reads the stored rows alone. It is dropped with the read.
	answers delivery.HeadAnswers
	// carried is what a read-only caller's branch reads carry for this read alone in place of what
	// a writer's read writes on server.branches: the default branch its own read of a repository
	// found, and the pairs a compare read of its own was refused as unseen
	// (delivery.BranchCarry). It is dropped with the read.
	carried delivery.BranchCarry
	// askedPulls is each repository-and-commit pair a read-only caller's read has asked GitHub
	// for in this read: a second result at the same commit is answered as a writer's read
	// answers it, whose claim of that pair this read already holds (answerCommitPulls).
	askedPulls map[[2]string]struct{}
}

// noCommitNamed is the `at` of a read that named no commit, which is every read but the one a
// reader pins to a commit of its own.
const noCommitNamed = ""

// comparison is what one check's results are compared with: the bases, in the order they are
// asked about, and the reason there are none where the list is empty. Exactly one of the two
// is filled in, and bases is never nil, so the answer carries an empty list rather than a
// null.
//
// headsNotRead is each pull request of a `before_merge` check's issue whose head Dispatch has
// not read (headBases), whatever the reason. headNotRead says whether they leave a result at no
// basis unknown: Dispatch holds the repository and can read it (rule 2), so a head it has not
// read can be the head that a result stands at. issuesNotKnown is each pull request of the
// repository whose answer is not known, where the issue holds no pull request at all (decision
// D2).
type comparison struct {
	bases          []model.CheckBasis
	reason         string
	headsNotRead   []model.HeadNotRead
	headNotRead    bool
	issuesNotKnown []model.IssueNotKnown
}

// noBases is a check comparing with nothing, for the one reason given: the empty list is the
// invariant comparison states, and writing it out at each of its call sites is how one of them
// comes to answer a null.
func noBases(reason string) comparison {
	return comparison{bases: []model.CheckBasis{}, reason: reason}
}

// repositoryAnswer is what one resolve of a check's issue said: the repository it resolves to,
// nil where it resolves to none, and whether the resolve answered at all
// (server.checkIssueRepository). The resolve that did not answer is kept apart from the issue
// that resolves to no repository because the two part every answer below: nothing to compare
// with is a fact about the work and leaves no pass standing, while a read Dispatch could not
// make is model.ReasonReadFailed and leaves the gate's question open. One nil for both is how
// a database hiccup came to read as "this work has moved on" (dispatch://INFRA-1522).
type repositoryAnswer struct {
	repo     *string
	answered bool
}

// defaultBranchAnswer is what the App last said about one repository: the name of its default
// branch and whether that read could say at all (delivery.Branches.DefaultBranchOf). The name
// is kept beside the yes because the surface names the branch it compared against, and the
// walk asks about it under that name.
type defaultBranchAnswer struct {
	branch   string
	readable bool
}

// baseBranchAnswer is what Dispatch holds about the merge of one issue in one repository: the
// branch its newest merged pull request there merged into, noMergedBranch where it holds no
// merge of that issue there at all, and whether the read answered. The failed read is kept
// apart from the empty branch because they part the two ways: a read that failed leaves the
// check comparing with nothing it can name, while no merge found leaves the repository's
// default branch to answer alone (measuredBranches). One flag for both would turn a database
// fault into a verdict about the work.
type baseBranchAnswer struct {
	branch string
	read   bool
}

func newComparisonReader(ctx context.Context, s *server, at string) *comparisonReader {
	return &comparisonReader{
		server: s, readOnly: readOnlyCredential(ctx) != nil, at: at, repos: map[string]repositoryAnswer{},
		defaults:           map[string]defaultBranchAnswer{},
		baseBranches:       map[string]baseBranchAnswer{},
		linkedBaseBranches: map[[2]string]baseBranchAnswer{},
		bases:              map[[2]string]comparison{},
		readThroughs:       map[[2]string]struct{}{},
		commits:            map[[3]string]delivery.BranchAnswer{},
		notKnown:           map[string]notKnownAnswer{},
		askedPulls:         map[[2]string]struct{}{},
	}
}

// noMergedBranch is what delivery.Attributor.BaseBranchMergedFor answers for an issue Dispatch
// holds no merge of, and for a merged row whose base_ref is empty: no branch to compare with. It is
// spelled like delivery.DefaultBranch and means the opposite of it — "there is nothing here to ask
// about" rather than "ask about the repository's own default branch" — so each is named where it is
// used. measuredBranches tests for one and answers the other in its place, and naming both is what
// makes that a decision of this file's rather than an agreement between two packages that either
// could end without a compiler saying so.
const noMergedBranch = ""

// noDefaultBranchNamed is what delivery.Branches.DefaultBranchOf answers for a repository no
// read of this process ever named a default branch for, which a deployment holding no App key
// makes of every repository: that branch is no branch to compare with, and none to ask a
// stored answer about either, since a branch_commits row is keyed by a name. The branch an
// issue merged into is named from the database and stands in its place where there is one
// (measuredBranches). It is spelled like delivery.DefaultBranch and means the opposite of it,
// as noMergedBranch does, so each is named where it is used.
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
//
// The check names each head it has not read (model.CheckBlock.HeadsNotRead), and each pull
// request whose answer is not known that its answer turns on (model.CheckBlock.IssuesNotKnown):
// those of an issue with no pull request (decision D2), the possible head of each result, and the
// merges into another branch that leave an `after_merge` result unknown (mergedElsewhere).
func (c *comparisonReader) answer(ctx context.Context, check *model.CheckBlock) {
	compared := c.comparedWith(ctx, *check)
	check.ComparedWith = compared.bases
	check.ComparedNothing = compared.reason
	check.HeadsNotRead = compared.headsNotRead
	check.IssuesNotKnown = nil
	for _, notKnown := range compared.issuesNotKnown {
		check.IssuesNotKnown = appendIssueNotKnown(check.IssuesNotKnown, notKnown.Number)
	}
	for index := range check.Results {
		result := &check.Results[index]
		moved := check.SHA256 != result.CheckSHA256
		result.CheckWordsMoved = &moved
		stands := c.commitStands(ctx, *check, compared, *result)
		result.CommitStands = &stands
		if stands.Reason != model.ReasonIssueNotKnown {
			continue
		}
		if stands.PossibleHead != nil {
			check.IssuesNotKnown = appendIssueNotKnown(check.IssuesNotKnown, stands.PossibleHead.PullRequest)
			continue
		}
		for _, number := range c.mergedElsewhere(ctx, *check) {
			check.IssuesNotKnown = appendIssueNotKnown(check.IssuesNotKnown, number)
		}
	}
	check.PassStands = passStandsOf(*check, compared)
}

// comparedWith is the bases of one check, read once per issue and `when`: two `before_merge`
// checks of one issue compare with the same heads, and two `default_branch` checks with the
// same branch. Every head GitHub had to be asked for is stored before the first check is
// answered (learn), which reads no bases, so the two checks name one list and not two.
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
	if check.When == docs.CheckWhenBeforeMerge {
		compared = c.headBases(ctx, check.IssueKey)
	} else {
		compared = c.branchBases(ctx, check)
	}
	c.bases[key] = compared
	return compared
}

// headBases is every pull request head Dispatch holds for the check's issue, newest learned
// first, as the bases a `before_merge` check compares its results with. An issue holding none
// compares with nothing, and nothingReason says which of the three kinds of nothing it is.
//
// The heads answer whether or not Dispatch can read the repository. A head Dispatch holds is a
// fact about the work whatever the App can see today, so a result at one stands at it, and
// only a result at no head of the list needs the repository read at all (commitStands).
//
// A pull request whose head Dispatch has not read is no basis: it has no results row yet, or a read
// of GitHub is owed for it (delivery.Attributor.HeadsAttributedTo). It goes to headsNotRead, and no
// pass stands by it. Where Dispatch can read the repository (decides), such a head can be the head
// a result stands at: a result at no basis is then not known, never another commit, and a check
// with no basis names model.ReasonHeadNotRead (rule 1 of the shape of the answer).
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
	compared := comparison{bases: make([]model.CheckBasis, 0, len(heads))}
	for _, head := range heads {
		if head.HeadKnown {
			compared.bases = append(compared.bases, headBasis(head))
			continue
		}
		compared.headsNotRead = append(compared.headsNotRead, model.HeadNotRead{
			Number: head.Number, RefusedAs: head.RefusedAs, RefusedSince: head.RefusedSince,
		})
	}
	if len(compared.headsNotRead) > 0 {
		compared.headNotRead = c.decides(ctx, issueKey)
	}
	if len(compared.bases) > 0 {
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

// headBasis is one stored pull request head as the surface names it: the commit, the pull
// request's number, and when Dispatch last learned that head with the kind of time it is. The
// later of the two times wins, so a head a webhook moved after the last complete read prints
// GitHub's own update time, named model.BasisLearnedUpdatedAt, and never an older read time. A
// row carrying neither time names none. The head of a pull request the issue links in a
// repository it does not resolve to names that repository too (AttributedHead.LinkedRepo), so a
// reader can tell it from a pull request of the same number in the issue's own repository.
func headBasis(head delivery.AttributedHead) model.CheckBasis {
	number := head.Number
	basis := model.CheckBasis{
		Kind: model.CheckBasisPullRequest, Repo: head.LinkedRepo, Commit: head.HeadSHA, PullRequest: &number,
	}
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
// The branches are asked about whenever the check's issue resolves to a repository and
// Dispatch can name a branch of it at all, whether or not the App can read that repository
// just now. A stored answer is keyed by a branch and is read before any call to GitHub, a yes
// standing for good, so a name delivery or the merge record keeps across a read that could say
// nothing is the whole of what the walk needs to reach it
// (delivery.Branches.CommitOnBranch). The merge commit of a squash merge whose base branch is
// deleted is what turns on this: nothing but that stored yes can ever place it, and refusing
// the walk while the App cannot be read would turn every `after_merge` and every
// `default_branch` result of the repository from `at_basis` to `unknown` for as long as the
// read fails. Where no branch of the walk holds a stored answer the result is unknown all the
// same, through onNamedBranch's own arms.
//
// An issue Dispatch can name no repository for names no branch at all, and that is settled
// here before any branch is chosen, for either of the two ways it can name none.
//
// An issue that resolves to no repository is the first. delivery.Attributor.BaseBranchMergedFor
// reads `base_ref` by issue key alone, so an issue whose project's default repository was cleared
// after its merge, or whose parent moved, keeps a merged row naming a branch of a repository it no
// longer resolves to; walking that name would reach onNamedBranch with no repository to ask
// GitHub or the stored rows about and answer unknown for every result of the check. There is
// nothing to compare with and nothing left to read, which is a fact about the work and not a
// failed read, so no pass of such a check stands (dispatch://INFRA-1522).
//
// A resolve that did not answer is the second, and parts from the first here rather than at
// the walk below: Dispatch does not know whether that issue has a repository, so it knows
// nothing of the branches either, and nothingReason names model.ReasonReadFailed for it. The
// merged row still naming a branch is no help, since no name of a repository is left to ask
// GitHub or the stored rows under.
//
// A repository no read of this process ever named a default branch for — the one whose first `GET
// /repos/{owner}/{repo}` after a restart failed, and every repository of a deployment holding no
// App key — still has the branch its issue's pull request merged into, which
// delivery.Attributor.BaseBranchMergedFor reads from the database and no call to GitHub. An
// `after_merge` check of such an issue compares with that branch alone, so the stored yes at it is
// reached; only a check no branch at all can be named for compares with nothing.
func (c *comparisonReader) branchBases(ctx context.Context, check model.CheckBlock) comparison {
	if repo, answered := c.repositoryOf(ctx, check.IssueKey); !answered || repo == nil {
		return noBases(c.nothingReason(ctx, check.IssueKey))
	}
	defaultBranch, _ := c.defaultBranchOf(ctx, check.IssueKey)
	branches, chosen := c.measuredBranches(check, defaultBranch, func() (string, bool) {
		return c.baseBranchMergedFor(ctx, check.IssueKey)
	})
	if !chosen {
		return noBases(model.ReasonReadFailed)
	}
	if len(branches) == 0 {
		// The repository resolves, so the nothing here is the other kind: no read of this
		// process ever named a default branch for it and no merge of the issue names one in
		// its place, which leaves no branch to ask about. nothingReason says whether the
		// App's own failed read is why, from the same memoized reads.
		return noBases(c.nothingReason(ctx, check.IssueKey))
	}
	return branchesNamed(branches)
}

// branchesNamed is the comparison of a branch check whose branches are named, in the order
// given, as the surface names a branch basis.
func branchesNamed(branches []string) comparison {
	bases := make([]model.CheckBasis, 0, len(branches))
	for _, branch := range branches {
		bases = append(bases, model.CheckBasis{Kind: model.CheckBasisBranch, Branch: branch})
	}
	return comparison{bases: bases}
}

// nothingReason tells the three kinds of nothing apart for a check that named no basis: a
// resolve of the issue's repository that did not answer, an issue resolving to no repository,
// which has no pull request and no branch to compare with, and one whose repository the App
// cannot read, which has a comparison Dispatch cannot make. The middle one leaves a pass
// standing at nothing, which is a no. The other two are unknown, and a reviewer reads each as
// a question Dispatch could not answer rather than as a verdict about the work: the App is
// the one to give the repository to, and a read of Dispatch's own that failed is one to make
// again.
//
// The resolve is read before the App's own read because an issue whose repository Dispatch
// cannot name has no repository for the App to be asked about: defaultBranchOf answers no
// branch and no for it, which read as the App's refusal would turn every failed resolve into
// model.ReasonRepositoryUnreadable.
func (c *comparisonReader) nothingReason(ctx context.Context, issueKey string) string {
	repo, answered := c.repositoryOf(ctx, issueKey)
	if !answered {
		return model.ReasonReadFailed
	}
	if repo == nil {
		return model.ReasonNoPullRequestOrBranch
	}
	if _, readable := c.defaultBranchOf(ctx, issueKey); !readable {
		return model.ReasonRepositoryUnreadable
	}
	return model.ReasonNoPullRequestOrBranch
}

// learn teaches Dispatch the pull requests GitHub names for every result commit of the
// document, before any check is answered: a pull request that GitHub named can be a basis now,
// at that commit or at another result's commit, and the same read compares with it (R5). It reads
// a commit that a basis of the check already is, too: GitHub can name a second pull request at
// the same head, whose own event never arrived (R6 of the results lookup). A read-only caller's
// read stores no pull request head: it keeps the writes a writer's read-through would have made
// (answerCommitPulls, answers), and every read after it reads the rows as they would stand had it
// made them, so the same heads are its bases too. So every check of one answer is answered on the
// heads as they stand after this read's read-throughs, both callers alike (plan D32).
//
// It is what stops a result recorded at a commit before any pull request carried it from
// standing at no basis for ever (dispatch://INFRA-1240). It runs over the whole document
// before any check is answered, so every `before_merge` check of one issue names the same
// list of heads: a head one check's result taught Dispatch is a basis the sibling check is
// read against too, whichever came first in the document. It runs before any check CI proves
// is derived, too (deriveCIResults), which reads the issue's newest pull request head after it
// (delivery.NewestPullRequestHead): a head learned here is the head such a check is derived at, as it
// is another check's basis. The checks' results it reads are the recorded ones, since a derived
// result is at that newest head and has nothing left to learn.
//
// A reader that named a commit asks GitHub nothing at all, and learns nothing here.
//
// Only a `before_merge` check reaches GitHub this way, since a check compared with branches
// is compared with no head. The memo is the pair of the issue and the commit: a pair read
// through once in this read is never read through again, whichever check holds the result
// and whenever it was recorded.
//
// learn reads no bases and no pull requests whose answer is not known. answer reads both after
// learn (comparedWith, notKnownOf), so each holds every row that this read's read-throughs wrote,
// and the same answer names a pull request it learned (review 18, note 3).
func (c *comparisonReader) learn(ctx context.Context, checks []model.CheckBlock) {
	if c.at != noCommitNamed {
		return
	}
	for _, check := range checks {
		if check.When != docs.CheckWhenBeforeMerge {
			continue
		}
		for _, result := range check.Results {
			key := [2]string{result.CheckIssueKey, result.CommitSHA}
			if _, asked := c.readThroughs[key]; asked {
				continue
			}
			c.readThroughs[key] = struct{}{}
			c.readThroughHead(ctx, result)
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
// whether the repository the commit is taken to be of can be read at all, and a branch check
// walks the branches of that repository (onNamedBranch). Those two, beside learn's read-through
// of a `before_merge` result's pull requests (readThroughHead), are the only places here the
// repository a result's row takes its commit to be of is read, each through the one resolver
// (commitRepositoryOf): a check that named no basis of its own leaves such a result where its
// reason leaves every other, as passStandsOf leaves the check. An `other_commit` or `unknown`
// answer a linked repository decided names it (model.ResultCommitStands.Repo), so a reader can
// tell it from one about the issue's own repository.
//
// A `before_merge` result whose commit is taken to be of a linked repository
// (commitRepositoryOf) is decided by that repository, as its record and its read-through are:
// unknown with model.ReasonRepositoryUnreadable where the App cannot read it, and another commit
// otherwise, since a read-through of that repository would have taught Dispatch a pull request
// headed at the commit (commitRepository.readable, the answer readThroughRepository reads for
// readThroughHead too). The issue's own repository has nothing to say about a commit of another
// repository: read through it, a result there would stand unknown for good wherever the App
// cannot read the issue's own.
//
// An issue that resolves to no repository is never unknown, whatever the check's `when`, but
// for a `before_merge` result whose row names a linked repository, which that repository decides
// as above, unknown where the App cannot read it. Dispatch holds every head it will ever hold for
// such an issue, since there is no repository to read one from, so a commit none of them names is
// another commit and not a question left open. A check compared with branches names no branch of
// such an issue either, whatever branch a merged row of it still carries (branchBases), so it is
// answered by the reason above and never by a walk.
//
// An issue whose resolve did not answer is unknown everywhere instead, and for the opposite
// reason: Dispatch cannot say whether there is a repository to read a head from, so a commit
// no head names is no evidence that the work moved on. Both nils were one answer here before
// dispatch://INFRA-1522, and a failed resolve read as the fact about the work.
//
// A `before_merge` result at no basis that the rules above leave at another commit can still be
// at a head Dispatch does not know of (atNoBasis). An `after_merge` result can stand at a branch
// that another merge did not reach (mergedElsewhere).
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
	if check.When == docs.CheckWhenBeforeMerge {
		// The heads are the whole of this check's comparison, and none of them is this commit.
		// Two answers Dispatch cannot give stand against reading that as another commit: a
		// repository the App cannot read, where nothing can have taught Dispatch this commit's
		// pull request, and a read of Dispatch's own that did not answer. The repository is
		// the one the commit is taken to be of: a linked one, whose own read gives the reason,
		// and else the issue's own, whose reason nothingReason gives and tells from an issue
		// that resolves to no repository, where the heads Dispatch holds are every head it will
		// ever hold.
		if owner := c.commitRepositoryOf(ctx, check.IssueKey, result.CommitRepo); owner.linked != "" {
			if !owner.readable {
				return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: model.ReasonRepositoryUnreadable, Repo: owner.linked}
			}
			return model.ResultCommitStands{Answer: model.CommitOtherCommit, Repo: owner.linked}
		}
		if reason := c.nothingReason(ctx, check.IssueKey); reason != model.ReasonNoPullRequestOrBranch {
			return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: reason}
		}
		return c.atNoBasis(ctx, check, compared, result)
	}
	stands := c.onNamedBranch(ctx, check, compared, result)
	if len(c.mergedElsewhere(ctx, check)) == 0 {
		return stands
	}
	// A merge whose answer is not known reached another branch than the issue's. It can be the
	// issue's own, so only the default branch, which every merge reaches in the end, decides.
	if defaultBranch, _ := c.defaultBranchOf(ctx, check.IssueKey); stands.Answer == model.CommitAtBasis &&
		stands.Basis != nil && stands.Basis.Branch == defaultBranch {
		return stands
	}
	return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: model.ReasonIssueNotKnown}
}

// atNoBasis is where a `before_merge` result's commit stands where no basis names it and the base
// answers another commit. Dispatch can read the repository there, and two pull requests it has
// not placed can still be the one at that commit. A pull request whose answer is not known and
// whose head is the commit makes it unknown with model.ReasonIssueNotKnown, and names it as the
// possible head (decision D4). Else a head of the issue that Dispatch has not read makes it
// unknown with model.ReasonHeadNotRead. Else it is another commit. A failed read of the pull
// requests whose answer is not known is model.ReasonReadFailed.
func (c *comparisonReader) atNoBasis(
	ctx context.Context, check model.CheckBlock, compared comparison, result model.CheckResult,
) model.ResultCommitStands {
	possible, read := c.possibleHeadAt(ctx, check.IssueKey, result.CommitSHA)
	switch {
	case !read:
		return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: model.ReasonReadFailed}
	case possible != nil:
		return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: model.ReasonIssueNotKnown, PossibleHead: possible}
	case compared.headNotRead:
		return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: model.ReasonHeadNotRead}
	}
	return model.ResultCommitStands{Answer: model.CommitOtherCommit}
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
// The walk is of the repository the result's commit is taken to be of. For every result but
// one, that is the repository the check's issue resolves to, and the branches are the bases the
// check named. The one is a result whose commit is taken to be of a linked repository, one the
// issue links a pull request in and does not resolve to (commitRepositoryOf, from the row's
// model.CheckResult.CommitRepo): it walks that repository's branches, chosen by the same rule
// (linkedBranchBases), and the basis it stands at is a branch of that repository, which names it.
// The issue's own repository holds no such commit, so its compare read answers 404 and the walk
// there could only end unknown. A row naming the issue's own repository is one whose issue has
// since come to resolve to that repository, and it is walked there as every other result of the
// issue is. A result whose row names none reads nothing more than it did before the row could
// name one. Every arm below is about the walked repository: its default branch, and whether the
// App can read it.
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
//   - A walk the default branch is missing from is unknown wherever no branch carried the
//     commit, whatever the branches it did ask answered. That is the `after_merge` check of a
//     repository no read of this process ever named a default branch for, which compares with
//     the branch its issue merged into alone (branchBases): "not on the branch the work merged
//     into" is no evidence about the trunk, which nothing asked.
//
// The reason an unknown carries is the App's where the App is the one that cannot read the
// repository (model.ReasonRepositoryUnreadable), and model.ReasonReadFailed otherwise. The
// walk runs while the App cannot read the repository — that is what reaches a stored yes no
// call could be made for — so a commit no stored row places is the ordinary end of it there,
// and the two words part what a reader does next: give the App the repository, or ask again.
func (c *comparisonReader) onNamedBranch(
	ctx context.Context, check model.CheckBlock, compared comparison, result model.CheckResult,
) model.ResultCommitStands {
	repo, answered := c.repositoryOf(ctx, check.IssueKey)
	if !answered || repo == nil {
		// No check reaches the walk without a repository: branchBases names no branch either
		// for an issue that resolves to none or for a resolve that did not answer, reading
		// this same memo, and a check that named no basis is answered before here, by the
		// reason it named none. The guard is what holds the walk below off a nil, and
		// read_failed is the one word for a state the arms above leave unreachable: a reader
		// is told to ask again, never that the work moved on.
		return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: model.ReasonReadFailed}
	}
	// named is the linked repository a walk of it names in an answer it gives, and "" for a
	// walk of the issue's own repository, whose answers name none.
	walked, bases, named := *repo, compared.bases, ""
	if owner := c.commitRepositoryOf(ctx, check.IssueKey, result.CommitRepo); owner.linked != "" {
		linkedCompared := c.linkedBranchBases(ctx, check, owner.linked)
		if len(linkedCompared.bases) == 0 {
			return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: linkedCompared.reason, Repo: owner.linked}
		}
		walked, bases, named = owner.linked, linkedCompared.bases, owner.linked
	}
	// The walked repository's own read, from the memo its bases were chosen from: whether the
	// App can read it, which names the reason below, and the default branch, whose absence
	// says the walk is short of the branch that would have decided.
	defaultBranch, readable := c.defaultBranchIn(ctx, walked)
	// unknown is whether any branch of the walk answered that Dispatch could not say. It is
	// kept apart from the last branch's own answer because an unknown anywhere outranks every
	// "not on it" after it, while a branch that carries the commit outranks it in turn.
	unknown := false
	answer := delivery.BranchUnknown
	for index, basis := range bases {
		answer = c.onBranch(ctx, walked, basis.Branch, result.CommitSHA)
		if answer == delivery.BranchCarries {
			carried := bases[index]
			return model.ResultCommitStands{Answer: model.CommitAtBasis, Basis: &carried}
		}
		unknown = unknown || answer == delivery.BranchUnknown
	}
	if unknown || answer == delivery.BranchUnseen || defaultBranch == noDefaultBranchNamed {
		reason := model.ReasonReadFailed
		if !readable {
			reason = model.ReasonRepositoryUnreadable
		}
		return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: reason, Repo: named}
	}
	return model.ResultCommitStands{Answer: model.CommitOtherCommit, Repo: named}
}

// standingCommit is where a result's commit stands while the check's words have not moved
// and no retraction stands against it. It answers no standing without both comparison fields.
func standingCommit(result model.CheckResult) string {
	if result.Retracted != nil || result.CheckWordsMoved == nil || *result.CheckWordsMoved || result.CommitStands == nil {
		return ""
	}
	return result.CommitStands.Answer
}

// passStandsOf is the one line that answers the gate's question for a check: whether a pass of
// it stands at what it compared with, against the check's words now, and from which roles.
//
// A pass stands when its commit is at one of the bases, the check's words have not moved since
// it was recorded, and no retraction stands against it. The roles are those of the passes that
// stand, in the order the results come, which is newest first.
//
// A check whose repository Dispatch could not read answers unknown, since it could compare
// nothing at all. So does a check CI proves whose check runs GitHub could not be read for: it
// holds no result, and the read that would have made one failed. So does a check whose standing
// pass is held up only by a commit Dispatch could not place. A check that compared with nothing
// because there is no pull request and no branch answers no: there is nothing for a pass to
// stand at, and that is a fact about the work rather than a failed read.
//
// A check that compared with nothing because Dispatch has not read a head of its issue, or does
// not know whether a pull request of its repository is the issue's, answers unknown: that head
// can be the one a pass must stand at. So does a check with a head that Dispatch has not read and
// that can decide (comparison.headNotRead), where no pass stands at a basis. No answer says more
// than Dispatch knows: unknown, never no.
func passStandsOf(check model.CheckBlock, compared comparison) model.CheckPassStands {
	switch {
	case compared.reason == model.ReasonRepositoryUnreadable,
		compared.reason == model.ReasonHeadNotRead,
		compared.reason == model.ReasonIssueNotKnown,
		check.CI != nil && check.CI.State == model.CIStateUnread:
		return model.CheckPassStands{Answer: model.PassStandsUnknown}
	}
	var roles []string
	unknown := compared.headNotRead
	for _, result := range check.Results {
		if result.Verdict != "pass" {
			continue
		}
		switch standingCommit(result) {
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
// compares with pull request heads and asks about no branch, and only one whose issue
// resolves to a repository: branchBases answers an issue resolving to none before it, since
// the merge read of the issue's own repository is keyed by the issue alone and would name the
// base branch of a repository that issue no longer resolves to.
//
// defaultBranch is the default branch of the repository measured, and readMerge the read of the
// branch the issue's work merged into there, which is asked only for an `after_merge` check.
// Each caller passes its own: branchBases the repository the check's issue resolves to, whose
// merge is the one the pull requests attributed to the issue name (baseBranchMergedFor), and
// linkedBranchBases a linked repository, for the one result whose commit is taken to be of it,
// whose merge is the one the issue's linked pull requests there name (baseBranchMergedIn). The
// rule below is the same for both.
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
//
// Each branch is named only where there is a name for it, so a repository no read of this
// process ever named a default branch for (noDefaultBranchNamed) leaves an `after_merge`
// check of a merged issue comparing with the merge's own branch alone and every other check
// comparing with nothing at all (branchBases). The empty name is never a branch of the list:
// it is delivery.DefaultBranch, which would ask about the repository's default branch under
// no name and so reach no stored row, and the surface would print a branch called "".
func (c *comparisonReader) measuredBranches(
	check model.CheckBlock, defaultBranch string, readMerge func() (string, bool),
) ([]string, bool) {
	var branches []string
	if check.When == checkWhenAfterMerge {
		merged, read := readMerge()
		if !read {
			return nil, false
		}
		if merged != noMergedBranch && merged != defaultBranch {
			branches = append(branches, merged)
		}
	}
	if defaultBranch != noDefaultBranchNamed {
		branches = append(branches, defaultBranch)
	}
	return branches, true
}

// repositoryOf is checkIssueRepository read once per read of the results: the repository the
// check's issue resolves to, and whether the resolve answered at all. The memo keeps the three
// states apart as the resolve states them, so an issue that resolves to none, a resolve that
// failed and a repository each cost one resolve rather than one per result — and the failed
// one is never read again as an issue that resolves to no repository.
func (c *comparisonReader) repositoryOf(ctx context.Context, issueKey string) (*string, bool) {
	if memoized, held := c.repos[issueKey]; held {
		return memoized.repo, memoized.answered
	}
	repo, answered := c.server.checkIssueRepository(ctx, c.server.deps.Store.Pool, issueKey)
	c.repos[issueKey] = repositoryAnswer{repo: repo, answered: answered}
	return repo, answered
}

// defaultBranchOf is the default branch of the repository the check's issue resolves to and
// whether Dispatch can read that repository at all: it resolves to one, and the App answered
// that repository's default branch (delivery.Branches.DefaultBranchOf). It is read once per
// repository per read of the results. An issue resolving to no repository answers no branch
// and no, and so does a resolve that did not answer: there is no name to ask the App about
// either way. The two are told apart before this answer is read and never by it
// (nothingReason), so a resolve Dispatch could not make is never read as the App's own
// refusal.
func (c *comparisonReader) defaultBranchOf(ctx context.Context, issueKey string) (string, bool) {
	repo, answered := c.repositoryOf(ctx, issueKey)
	if !answered || repo == nil {
		return "", false
	}
	return c.defaultBranchIn(ctx, *repo)
}

// defaultBranchIn is defaultBranchOf for a repository named outright: the repository's default
// branch and whether the App answered it, read once per repository per read of the results.
// defaultBranchOf names the repository the check's issue resolves to, and commitRepositoryOf and
// onNamedBranch name the repository a result's row takes its commit to be of. The memo is the
// same for both, so a repository that is both is read once. A writer's read puts what GitHub
// answered in the process's cache (delivery.Branches.DefaultBranchOf); a read-only caller's read
// carries it for the rest of its own request instead (carried,
// delivery.Branches.ReadDefaultBranchOf), where its branch reads read it as a writer's read their
// cache entry.
func (c *comparisonReader) defaultBranchIn(ctx context.Context, repo string) (string, bool) {
	if answer, memoized := c.defaults[repo]; memoized {
		return answer.branch, answer.readable
	}
	var branch string
	var readable bool
	if c.readOnly {
		branch, readable = c.server.branches.ReadDefaultBranchOf(ctx, c.server.deps.GitHub, &c.carried, repo, c.server.resultsNow())
	} else {
		branch, readable = c.server.branches.DefaultBranchOf(ctx, c.server.deps.GitHub, repo, c.server.resultsNow())
	}
	c.defaults[repo] = defaultBranchAnswer{branch: branch, readable: readable}
	return branch, readable
}

// baseBranchMergedFor is the branch the check's issue's newest merged pull request in the
// repository it resolves to merged into, the pull requests attributed to the issue
// (delivery.Attributor.BaseBranchMergedFor), read once per read of the results, and whether the
// read answered at all. An issue Dispatch holds no merge of answers noMergedBranch, which
// measuredBranches reads as no branch but the repository's default one to ask about. A linked
// repository's merge is baseBranchMergedIn's. It reads the caller's request's answers as
// headBases does, so a merge a read-only caller's read-through learned is read as the row a
// writer's read-through stored.
//
// The memo keeps a failed read as it keeps an answered one, so a second result of the same
// issue neither reads again nor is quietly compared with the default branch because the first
// read failed. The key is the issue: the branch an issue's work merged into is a fact about
// that issue, not about any one result or commit of it.
func (c *comparisonReader) baseBranchMergedFor(ctx context.Context, issueKey string) (string, bool) {
	if answer, memoized := c.baseBranches[issueKey]; memoized {
		return answer.branch, answer.read
	}
	branch, err := c.server.deps.Attributor.BaseBranchMergedFor(ctx, c.server.deps.Store.Pool, issueKey, c.answers)
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
//
// A writer's read writes what a compare read answers to branch_commits
// (delivery.Branches.CommitOnBranch). A read-only caller's read writes nothing
// (delivery.Branches.ReadCommitOnBranch): it reads a merge its own read-throughs learned as the
// yes a writer's store would have written, and the default branch and the unseen pairs it carries
// where a writer's read reads its cache entry and its holds. This memo is where it answers a
// compare read's answer for the rest of its request, where a writer's later read in the same
// request reads the row its own read wrote; after the request it keeps none of it, so a writer's
// read after it still asks and writes the row.
func (c *comparisonReader) onBranch(ctx context.Context, repo, branch, sha string) delivery.BranchAnswer {
	memo := [3]string{repo, branch, sha}
	if answer, memoized := c.commits[memo]; memoized {
		return answer
	}
	var answer delivery.BranchAnswer
	if c.readOnly {
		answer = c.server.branches.ReadCommitOnBranch(
			ctx, c.server.deps.Store.Pool, c.server.deps.GitHub, c.answers, &c.carried, repo, branch, sha, c.server.resultsNow())
	} else {
		answer = c.server.branches.CommitOnBranch(
			ctx, c.server.deps.Store.Pool, c.server.deps.GitHub, repo, branch, sha, c.server.resultsNow())
	}
	c.commits[memo] = answer
	return answer
}
