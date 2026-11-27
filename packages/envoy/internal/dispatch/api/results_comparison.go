package api

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// results_comparison.go says what each check compares its results with and where each result's
// commit stands against that; whether a pass of the check stands is results_pass_stands.go's, the
// heads a `before_merge` check compares with are results_head_bases.go's (headBases), and the merge
// of the work a branch check's commit must hold is results_merge.go's (mergeOn, holds). It holds
// no policy about GitHub: what is asked, how long an answer is believed and what is written back
// are delivery's (delivery/branch.go, delivery/merge_holds.go, delivery/commit_pull_reads.go,
// delivery.Attributor.HeadsAttributedTo, delivery.Attributor.ReadThroughCommitPulls). What lives
// here is the choice of bases (comparedWith, branchBases, branchesIn) and one memo per read, so a
// tab listing twenty results against a handful of commits costs a handful of round trips.
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
// An `after_merge` check is compared with the branch the check's issue's newest merged pull
// request merged into, and with the repository's default branch where the work has reached it,
// and a result counts at a branch only at a commit whose history holds the merge of the work
// there (R36; branchBases, results_merge.go's mergeOn): on the branch the pull request merged
// into, its own merge commit, and on the default branch, where it merged into an integration
// branch, the merge commit of the pull request that landed that branch there. So a commit of the
// branch from before the merge is another commit. Each branch is named with that merge commit
// (model.CheckBasis.MergeCommit), and the default branch is named only once the work has reached
// it. A base branch GitHub no longer holds reads as "this commit is not on it" and never as
// unknown, and the next branch decides (onNamedBranch). An issue Dispatch holds no merge of at
// all, in its own repository or through a pull request it links, compares with nothing for
// model.ReasonNotMerged.
//
// The merge's own commit goes on standing at the base branch after the branch is gone, because
// Dispatch recorded it as on that branch when it recorded the merge
// (delivery.Attributor.UpsertPullRequestHead) and a yes is kept for good. In a repository that
// merges by squash that commit never lands on the default branch at all, so without that record a
// result at the merge commit would stand at no basis for ever once the base branch was deleted.
//
// A `default_branch` check is compared with the default branch of the check's issue's resolved
// repository alone, whatever branch the pull request merged into, at the merge of the work there.
// That is the whole of the difference between the two `when`s, and it is why a check that must
// speak for the trunk is written `default_branch` rather than `after_merge`. Where the issue has
// no merged pull request anywhere, every commit of the default branch counts.
//
// An issue whose work spans two repositories links the pull request it has in the second one,
// and its merge there is the newest merged pull request its links name in that repository. Each
// linked repository where the issue has such a merge adds its own branches to what the check
// names (comparison.linked, each naming that repository, model.CheckBasis.Repo), whether or not a
// result of that repository exists. A branch check's result whose row names a linked repository as
// its commit's (model.CheckResult.CommitRepo, which the record route stores where
// refuseUnknownCommit takes the commit to be that repository's; results_linked.go) is walked
// against that repository's branches alone, and every other result against the issue's own
// repository's alone: asked of the issue's own repository, GitHub answers 404 for a commit only
// the linked one holds, and a merge commit of one repository asked of the other answers 404 too.
// Where the issue's only merges are linked, its own repository's branches are named with no merge,
// and a result at a commit of the issue's own repository is another commit with no read.
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
// same reason: the read of the merges of an issue, and the resolve of the repository the check's
// issue names at all (repositoryOf), among them. A database hiccup must not make a reviewer
// refuse an approval.
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
// The repository each issue resolves to, its default branch, the merged pull requests of each
// issue in each repository and the merge of its work on each branch, the pull request heads of
// each issue and GitHub's answer about each repository, branch and commit, and about each commit
// holding each merge, are each read once per read of the results, and so are the canonical name
// of each linked repository a result's row takes its commit to be of, one GitHub said holds that
// commit when the result was recorded, and the repositories each issue links a merged pull
// request in. The branch is in the key of GitHub's answer about a commit, and not the repository
// and the commit alone, because one issue's Verification document may hold an `after_merge`
// check and a `default_branch` check with a result at the same commit: two questions, with two
// answers, that one key for both would collapse into whichever was asked first. Across reads, a
// repository's default branch, a commit's place on a branch, whether a commit holds a merge and
// the last read-through of a commit's pull requests are held by delivery (server.branches,
// branch_commits, commit_holds, server.commitPulls and commit_pull_reads). A read-only caller's
// read writes none of those: it reads them as a writer's read does, and carries what a writer's
// read would write for the rest of its own request alone (answers, carried).
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
	at             string
	repos          map[string]repositoryAnswer
	defaults       map[string]defaultBranchAnswer
	bases          map[[2]string]comparison
	headGeneration uint64
	linkedBases    map[[3]string]comparison
	linkedMerges   map[string]linkedMergesAnswer
	mergeSets      map[[2]string]mergeSet
	merges         map[[3]string]mergeAnswer
	holdAnswers    map[[3]string]delivery.HoldAnswer
	readThroughs   map[[2]string]struct{}
	commits        map[[3]string]delivery.BranchAnswer
	// notKnown is the open and merged pull requests of each repository whose answer is not known
	// (notKnownOf), by canonical name. Only answer fills it, after learn, so a pull request that
	// the same read learns is in it.
	notKnown map[string]notKnownAnswer
	// headCI is the read of each check CI proves at each head it was read at, by the check's block
	// id and then by the head's repository and commit (deriveCIResults), which answer gives each
	// subject of it.
	headCI map[string]map[ciHeadKey]*model.CheckCI
	// fills is the read's memo of the merged pull request rows it refreshed (fillMergeTimes,
	// refreshMerge), the one listCheckResults made before deriveCIResults, so the reader refreshes
	// no row twice.
	fills   *mergeTimeFills
	deploys map[string]currentDeploysAnswer
	// answers is, for a read-only caller's read, each write its own read-throughs would have made
	// had a writer made them, in order, and stored nowhere (delivery.HeadAnswers): every read of
	// the stored heads, of the merges of an issue, in the issue's own repository and in a linked
	// one, of the landings of a branch, of a refreshed merged pull request, of the heads a check
	// CI proves is derived at, of the pull requests whose answer is not known, and of a commit's
	// stored branch answer (delivery.Branches.ReadCommitOnBranch) runs in a transaction that makes
	// those writes with the writer's own code and is rolled back after the read
	// (delivery.HeadAnswers.Read), which is what a writer's read reads once its read-throughs have
	// stored them. A writer's read stores what it finds and passes this empty, which reads the
	// stored rows alone. It is dropped with the read.
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
// null. unread is set beside bases alone: the reason the head of a branch a CI job pushed could
// not be read, while the list holds other bases. A commit none of them names may be that head,
// so it is unknown for that reason and never another commit (commitStands).
//
// merges is set for a check compared with branches (branchesIn): the merge of the work on each
// branch of bases, at the same index, which a result's commit there must hold (onNamedBranch).
// linked is the branches of each linked repository where the issue has a merged linked pull
// request (linkedBranchBases), each naming its repository: the check names them after bases
// (served), and only a result of that repository walks them, never a result of the issue's own.
//
// subjects is set for a `before_merge` check compared with heads (headBases): the heads a pass
// must stand at, each of them, for the gate's answer to be yes (passStandsOf). Every subject is a
// basis; a merged pull request's head beside an open pull request, and a merged one older than
// the newest, is a basis and no subject. unordered is set for a `before_merge` check of an issue
// none of whose pull requests is open where Dispatch cannot tell which merged one merged last
// (headBases): it names no subject, and passStandsOf answers unknown.
//
// headsNotRead is each pull request of a `before_merge` check's issue whose head Dispatch has
// not read (headBases), whatever the reason: no basis and no subject. headNotRead says whether
// they leave a result at no basis unknown: Dispatch holds the repository and can read it (rule
// 2), so a head it has not read can be the head that a result stands at. issuesNotKnown is each
// pull request of the repository whose answer is not known, where the issue holds no pull
// request at all (decision D2).
//
// ci is set on one check's copy alone (answer), for a check CI proves: its read at each head, by
// the head's repository and commit, which the subject of that repository at that commit carries
// (subjectState).
type comparison struct {
	bases          []model.CheckBasis
	merges         []mergeAnswer
	linked         []model.CheckBasis
	reason         string
	unread         string
	subjects       []headSubject
	unordered      bool
	headsNotRead   []model.HeadNotRead
	headNotRead    bool
	issuesNotKnown []model.IssueNotKnown
	ci             map[ciHeadKey]*model.CheckCI
	live           bool
	program        string
	deploy         *model.CheckDeploy
	deployHolds    model.ResultCommitStands
	deployReason   string
}

// served is what the check names as its bases (model.CheckBlock.ComparedWith): the bases of the
// issue's own repository, and after them each linked repository's (contract line 4).
func (c comparison) served() []model.CheckBasis {
	if len(c.linked) == 0 {
		return c.bases
	}
	return slices.Concat(c.bases, c.linked)
}

// headSubject is one subject of a check compared with heads (comparison.subjects): its basis, as
// the answer names it, and repo, the repository it is a head of, the pull request row's own. The
// basis names a repository only where it is another than the issue's (headBasis), so the read of
// a check CI proves at the subject is keyed by repo and the commit (ciHeadKey): two repositories
// at one commit are two subjects, each carrying the read of its own repository.
type headSubject struct {
	basis model.CheckBasis
	repo  string
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

func newComparisonReader(ctx context.Context, s *server, at string) *comparisonReader {
	reader := &comparisonReader{
		server: s, readOnly: readOnlyCredential(ctx) != nil, at: at, repos: map[string]repositoryAnswer{},
		defaults:     map[string]defaultBranchAnswer{},
		bases:        map[[2]string]comparison{},
		linkedBases:  map[[3]string]comparison{},
		linkedMerges: map[string]linkedMergesAnswer{},
		mergeSets:    map[[2]string]mergeSet{},
		merges:       map[[3]string]mergeAnswer{},
		holdAnswers:  map[[3]string]delivery.HoldAnswer{},
		readThroughs: map[[2]string]struct{}{},
		commits:      map[[3]string]delivery.BranchAnswer{},
		notKnown:     map[string]notKnownAnswer{},
		fills:        &mergeTimeFills{},
		askedPulls:   map[[2]string]struct{}{},
	}
	reader.fills.readOnly, reader.fills.answers = reader.readOnly, &reader.answers
	return reader
}

// noMergedBranch is the base branch of a merged pull request row whose base_ref is empty: no
// branch to compare with, and no branch a merge of the work is on (mergeOn). It is spelled like
// delivery.DefaultBranch and means the opposite of it — "there is nothing here to ask about"
// rather than "ask about the repository's own default branch" — so each is named where it is
// used, and naming both is what makes that a decision of this file's rather than an agreement
// between two packages that either could end without a compiler saying so.
const noMergedBranch = ""

// noDefaultBranchNamed is what delivery.Branches.DefaultBranchOf answers for a repository no
// read of this process ever named a default branch for, which a deployment holding no App key
// makes of every repository: that branch is no branch to compare with, and none to ask a
// stored answer about either, since a branch_commits row is keyed by a name. The branch an
// issue merged into is named from the database and stands in its place where there is one
// (branchesIn). It is spelled like delivery.DefaultBranch and means the opposite of it, as
// noMergedBranch does, so each is named where it is used.
const noDefaultBranchNamed = ""

// answer fills in what one check compared its results with, where each of its results stands,
// and whether a pass of it stands. The check's results are already in place, newest first,
// each carrying its retraction where one stands against it (loadCheckResultRetractions).
//
// The two facts a result states are kept apart and read apart. Where its commit stands is one
// question, answered against the bases, and whether the check's words moved is another,
// answered by the digest and the changed version the result recorded beside those the check
// carries now: the words moved when the digests differ and the versions differ too. A check's
// changed version moves with every change of its words. The one case where its digest moves
// while its changed version stays is refs.BlockIndexing.KeepRespelled keeping a block whose text
// recorded at that version another renderer version spelled and which reads as its text now, the
// same words in a new spelling. A result at the compared commit under an older wording is both,
// and one word for both would hide which of the two a reader is looking at.
//
// The check names each head it has not read (model.CheckBlock.HeadsNotRead), and each pull
// request whose answer is not known that its answer turns on (model.CheckBlock.IssuesNotKnown):
// those of an issue with no pull request (decision D2), the possible head of each result, and the
// merges into another branch that leave an `after_merge` result unknown (mergedElsewhere).
func (c *comparisonReader) answer(ctx context.Context, check *model.CheckBlock) {
	compared := c.comparedWith(ctx, *check)
	compared.ci = c.headCI[check.BlockID]
	check.ComparedWith = compared.served()
	check.ComparedNothing = compared.reason
	check.HeadsNotRead = compared.headsNotRead
	check.IssuesNotKnown = nil
	for _, notKnown := range compared.issuesNotKnown {
		check.IssuesNotKnown = appendIssueNotKnown(check.IssuesNotKnown, notKnown.Number)
	}
	for index := range check.Results {
		result := &check.Results[index]
		moved := check.SHA256 != result.CheckSHA256 && check.Version != result.CheckVersion
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
	markNotCounted(check, compared)
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
	for {
		if c.headGeneration != c.fills.generation {
			c.clearHeadComparisons()
		}
		generation := c.fills.generation
		compared, held := c.bases[key]
		if !held {
			if check.When == docs.CheckWhenBeforeMerge {
				compared = c.headBases(ctx, check.IssueKey)
			} else {
				compared = c.branchBases(ctx, check)
			}
		}
		answer := c.withLiveDeploy(ctx, check, compared)
		if generation != c.fills.generation {
			// A refresh can remove a head from this issue or change the row ordering. Re-read
			// the whole head set; patching only the old slice's merge time keeps stale subjects.
			continue
		}
		c.bases[key] = compared
		return answer
	}
}

// clearHeadComparisons keeps every head-derived memo on the same learned head generation: the
// pull requests whose answer is not known among them, since a refresh can change attribution.
func (c *comparisonReader) clearHeadComparisons() {
	clear(c.bases)
	clear(c.linkedBases)
	clear(c.linkedMerges)
	clear(c.mergeSets)
	clear(c.merges)
	clear(c.notKnown)
	c.headGeneration = c.fills.generation
}

// branchBases names the own branches and the merged linked repositories' branches. Placement
// walks only the bases of the result's repository, not this served union (onNamedBranch).
func (c *comparisonReader) branchBases(ctx context.Context, check model.CheckBlock) comparison {
	repo, answered := c.repositoryOf(ctx, check.IssueKey)
	if !answered || repo == nil {
		return noBases(c.nothingReason(ctx, check.IssueKey))
	}
	own := c.mergesIn(ctx, check.IssueKey, *repo, false)
	linked := c.linkedMerged(ctx, check.IssueKey)
	if !own.read || len(own.pulls) == 0 && !linked.read {
		return noBases(model.ReasonReadFailed)
	}
	merged := len(own.pulls) > 0 || len(linked.repos) > 0
	if !merged && check.When == checkWhenAfterMerge {
		return noBases(model.ReasonNotMerged)
	}
	compared := c.branchesIn(ctx, check, *repo, false, merged)
	if len(compared.bases) == 0 {
		return noBases(c.nothingReason(ctx, check.IssueKey))
	}
	for _, repo := range linked.repos {
		compared.linked = append(compared.linked, c.linkedBranchBases(ctx, check, repo).bases...)
	}
	return compared
}

// branchesIn chooses bases and their merge requirements in one repository. A default-branch
// check with no merge anywhere needs no merge. A repository with only linked work has no own
// merge to hold. An unordered set names its branches but never guesses their merge commits.
func (c *comparisonReader) branchesIn(ctx context.Context, check model.CheckBlock, repo string, linked, merged bool) comparison {
	set := c.mergesIn(ctx, check.IssueKey, repo, linked)
	if !set.read {
		return noBases(model.ReasonReadFailed)
	}
	defaultBranch, _ := c.defaultBranchIn(ctx, repo)
	compared := comparison{bases: []model.CheckBasis{}}
	add := func(branch string, merge mergeAnswer) {
		if branch == "" || slices.ContainsFunc(compared.bases, func(b model.CheckBasis) bool { return b.Branch == branch }) {
			return
		}
		basis := model.CheckBasis{Kind: model.CheckBasisBranch, Branch: branch}
		if linked {
			basis.Repo = repo
		}
		if merge.state == mergeHeld {
			basis.MergeCommit = merge.commit
		}
		compared.bases = append(compared.bases, basis)
		compared.merges = append(compared.merges, merge)
	}
	switch {
	case !merged:
		add(defaultBranch, mergeAnswer{state: mergeNotAsked})
	case len(set.pulls) == 0:
		add(defaultBranch, mergeAnswer{state: mergeAbsent})
	case !set.ordered:
		if check.When == checkWhenAfterMerge {
			for _, pull := range set.pulls {
				add(pull.BaseRef, mergeAnswer{state: mergeUnknown})
			}
		}
		add(defaultBranch, mergeAnswer{state: mergeUnknown})
	default:
		if check.When == checkWhenAfterMerge {
			branch := set.pulls[0].BaseRef
			add(branch, c.mergeOn(ctx, check.IssueKey, repo, branch, linked))
		}
		if defaultBranch != "" {
			merge := c.mergeOn(ctx, check.IssueKey, repo, defaultBranch, linked)
			if check.When != checkWhenAfterMerge || merge.state != mergeAbsent {
				add(defaultBranch, merge)
			}
		}
	}
	return compared
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
// is derived, too (deriveCIResults), which reads the issue's pull request heads after it
// (ciHeadsOf): a head learned here is a head such a check is derived at, as it is another
// check's basis. The checks' results it reads are the recorded ones, since a derived result is
// at such a head and has nothing left to learn.
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
//
// readThroughsPerRead bounds what one read spends: learn reads commits through one call after
// another, so an issue whose results stand at scores of commits no stored head names would pay a
// GitHub round trip for each of them before the first check is answered — a production read of
// an issue with many such results took longer than two minutes, where the same read with ?at=,
// which reads nothing through, took 8.3 s
// (dispatch://INFRA-3992/comment/56fe3d7a-64af-456c-bff5-cd1c977c6bc0). Past the bound the read
// answers each remaining result from the stored rows, exactly as a read that learned nothing
// about it does, and the next read reads more of them: each commit this read learned is claimed
// (delivery.CommitPullReads) and each one it left is not, so the reads walk the commits rather
// than one read paying for all of them. The Verification tab refetches on every result.recorded,
// and a reader's own refresh is another read.
const readThroughsPerRead = 12

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
			if len(c.readThroughs) >= readThroughsPerRead {
				return
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
// A commit one of the bases names is the whole answer, whatever Dispatch can read now. A result a
// head's check run derived stands at that head's basis (derivedAt), so where two repositories are
// open at one commit, each one's derived result names its own pull request. Where the check
// named no basis, the reason it named none decides: nothing to compare with leaves the commit at
// another commit, while a repository Dispatch could not read leaves it unknown.
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
	if basis := compared.derivedAt(result); basis != nil {
		return model.ResultCommitStands{Answer: model.CommitAtBasis, Basis: basis}
	}
	if basis := c.resultBasisAt(ctx, check.IssueKey, compared.bases, result); basis != nil {
		return model.ResultCommitStands{Answer: model.CommitAtBasis, Basis: basis}
	}
	switch compared.reason {
	case model.ReasonNoPullRequestOrBranch, model.ReasonNotMerged:
		return model.ResultCommitStands{Answer: model.CommitOtherCommit}
	case model.ReasonRepositoryUnreadable, model.ReasonReadFailed:
		return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: compared.reason}
	}
	if c.at != noCommitNamed {
		return model.ResultCommitStands{Answer: model.CommitOtherCommit}
	}
	if check.When == docs.CheckWhenBeforeMerge {
		// The heads are the whole of this check's comparison, and none of them is this commit.
		// Three answers Dispatch cannot give stand against reading that as another commit: the
		// head of a branch a CI job pushed that GitHub did not answer for, which may be this
		// commit; a repository the App cannot read, where nothing can have taught Dispatch this
		// commit's pull request; and a read of Dispatch's own that did not answer. The repository
		// is the one the commit is taken to be of: a linked one, whose own read gives the reason,
		// and else the issue's own, whose reason nothingReason gives and tells from an issue that
		// resolves to no repository, where the heads Dispatch holds are every head it will ever
		// hold.
		if compared.unread != "" {
			return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: compared.unread}
		}
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

// derivedAt is the basis of the subject whose read of a check CI proves derived result, the head
// its run was read at, and nil for every other result.
func (c comparison) derivedAt(result model.CheckResult) *model.CheckBasis {
	for _, subject := range c.subjects {
		if read := c.ci[ciHeadKeyOf(subject.repo, subject.basis.Commit)]; read != nil && read.Result != "" && read.Result == result.ID {
			basis := subject.basis
			return &basis
		}
	}
	return nil
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

// resultBasisAt is the basis naming result's commit among bases that is of the repository the
// commit is taken to be of (commitRepositoryName), and nil where none is: a basis naming no
// repository is of the issue's own, and one naming a repository is of that one (headBasis). So a
// result of the issue's own repository never stands at the head of a pull request of a linked
// one, nor a linked result at a head of the issue's own, whatever commit the two heads share. A
// commit no basis names reads nothing, and a basis a read names (`?at=`) is of every repository.
// Where the issue resolves to no repository, or its resolve did not answer, no basis can be told
// for the issue's own from a linked one, and the commit alone decides (basisAt).
func (c *comparisonReader) resultBasisAt(ctx context.Context, issueKey string, bases []model.CheckBasis, result model.CheckResult) *model.CheckBasis {
	found := basisAt(bases, result.CommitSHA)
	if found == nil || found.Kind == model.CheckBasisNamed {
		return found
	}
	if own, answered := c.repositoryOf(ctx, issueKey); !answered || own == nil {
		return found
	}
	repo := c.commitRepositoryName(ctx, issueKey, result.CommitRepo)
	for _, basis := range bases {
		if basis.Commit == result.CommitSHA && strings.EqualFold(basis.Repo, repo) {
			return &basis
		}
	}
	return nil
}

// onNamedBranch walks only the bases of the result's repository, even though the check serves
// the union of its own and linked bases. A branch counts the commit only if its merge requirement
// is met: no merge is needed for unmerged default-branch work, an absent merge cannot count,
// and a held merge must be in the commit's history. Equality holds without asking GitHub.
//
// A carried commit with an unknown merge or a failed containment read is unknown. A later
// branch can still place it. A failed branch read or a missing final branch leaves the answer
// unknown if none places it; branches with known absent merges are not asked about at all.
// Stored branch and containment answers remain usable when the App cannot read the repository.
// Unknown names repository_unreadable where the App lacks access, and read_failed otherwise.
func (c *comparisonReader) onNamedBranch(
	ctx context.Context, check model.CheckBlock, compared comparison, result model.CheckResult,
) model.ResultCommitStands {
	repo, answered := c.repositoryOf(ctx, check.IssueKey)
	if !answered || repo == nil {
		return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: model.ReasonReadFailed}
	}
	walked, named := *repo, ""
	if owner := c.commitRepositoryOf(ctx, check.IssueKey, result.CommitRepo); owner.linked != "" {
		compared = c.linkedBranchBases(ctx, check, owner.linked)
		if len(compared.bases) == 0 {
			return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: compared.reason, Repo: owner.linked}
		}
		walked, named = owner.linked, owner.linked
	}
	defaultBranch, readable := c.defaultBranchIn(ctx, walked)
	unknown, asked := false, false
	answer := delivery.BranchDoesNotCarry
	for index, basis := range compared.bases {
		merge := compared.merges[index]
		if merge.state == mergeAbsent {
			continue
		}
		asked = true
		answer = c.onBranch(ctx, walked, basis.Branch, result.CommitSHA)
		if answer == delivery.BranchCarries {
			switch merge.state {
			case mergeUnknown:
				unknown = true
				continue
			case mergeHeld:
				switch c.holds(ctx, walked, result.CommitSHA, merge.commit) {
				case delivery.DoesNotHold:
					answer = delivery.BranchDoesNotCarry
					continue
				case delivery.HoldUnknown:
					unknown = true
					continue
				}
			}
			carried := basis
			return model.ResultCommitStands{Answer: model.CommitAtBasis, Basis: &carried}
		}
		unknown = unknown || answer == delivery.BranchUnknown
	}
	if asked && (unknown || answer == delivery.BranchUnseen || defaultBranch == noDefaultBranchNamed) {
		reason := model.ReasonReadFailed
		if !readable && !unknown {
			reason = model.ReasonRepositoryUnreadable
		}
		return model.ResultCommitStands{Answer: model.CommitUnknown, Reason: reason, Repo: named}
	}
	return model.ResultCommitStands{Answer: model.CommitOtherCommit, Repo: named}
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
