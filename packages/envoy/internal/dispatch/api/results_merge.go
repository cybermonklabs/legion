package api

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// results_merge.go holds what one results read knows of the merge of an issue's work: the merge on
// each branch a check compared with a branch is compared with (mergeOn), whether a result's commit
// holds it (holds), and whether a set of merged pull request rows can be ordered by when each
// merged (fillMergeTimes), each over a memo of the read. The store and GitHub reads behind them
// are delivery's (delivery/merge_holds.go, delivery.Attributor.RefreshPullRequestHead); the bases
// they make are results_comparison.go's (branchBases, branchesIn) and results_linked.go's
// (linkedBranchBases).

// mergeLandings is the most pull requests mergeOn follows from the branch an issue's pull request
// merged into to the branch it is asked about, each one landing the branch before it on its own
// base: an integration branch and a release branch on the way to the trunk are two, and eight
// leaves room for a deeper chain while bounding what one branch's merge can cost a read.
const mergeLandings = 8

// mergeState is what a branch basis of a check compared with a branch asks of a result's commit.
type mergeState int

const (
	// mergeNotAsked is the default branch of a `default_branch` check of an issue Dispatch holds
	// no merged pull request of anywhere: every commit of it counts.
	mergeNotAsked mergeState = iota
	// mergeHeld is a branch whose merge of the work Dispatch holds (mergeAnswer.commit): a commit
	// of it counts where its history holds that merge (holds).
	mergeHeld
	// mergeAbsent is a branch no commit of which can count: the work has not reached it, or the
	// branch is of a repository Dispatch holds no merge of the work in while it holds one in
	// another. The walk asks GitHub nothing about it.
	mergeAbsent
	// mergeUnknown is a branch whose merge Dispatch cannot learn or order now: a result at a commit
	// the branch carries is not known, for read_failed, and never another commit.
	mergeUnknown
)

// mergeAnswer is the merge of the work on one branch (mergeOn): its state, and the merge commit
// where the state is mergeHeld.
type mergeAnswer struct {
	state  mergeState
	commit string
}

// mergeTimeFills is one results read's memo of the merged pull request rows it refreshed from
// GitHub (refreshMerge), by the row's repository, lowercased, and number: listCheckResults makes it
// before deriveCIResults, and the comparisonReader holds the same one (comparisonReader.fills), so
// the read refreshes no row twice, whichever reader asked first.
type mergeTimeFills struct {
	refreshed map[mergeRow]refreshedMerge
	readOnly  bool
	answers   *delivery.HeadAnswers
}

// mergeRow is one pull request row's key in mergeTimeFills.
type mergeRow struct {
	repo   string
	number int
}

// refreshedMerge is a row as the store held it after a refresh: found is false where it holds no
// merged row of it, a failed read of it included.
type refreshedMerge struct {
	pull  delivery.MergedPullRequest
	found bool
}

// fillMergeTimes reports whether rows, the merged pull request rows one read orders by when each
// merged, can be ordered (ordered), and gives each row the time of its merge the read learned. A
// set of fewer than two rows needs no order. A row with no time of merge is refreshed once
// (refreshMerge) where the rule that last judged it is below this build's
// (delivery.AttributionRuleVersion), since the reconcile will still offer it; a row the current rule
// already offered and GitHub did not answer (delivery.MarkPullRequestHeadChecked) is not asked
// again. A set holding a row that still has no time after that cannot be ordered, since the row
// ordered last may have merged last, so no read names an older merge as the newest: the fill
// stops there and refreshes no row after it.
func (s *server) fillMergeTimes(ctx context.Context, fills *mergeTimeFills, rows []delivery.MergeTime) (ordered bool) {
	if len(rows) < 2 {
		return true
	}
	for index := range rows {
		row := &rows[index]
		if row.MergedAt == nil {
			if _, refreshed := fills.refreshed[mergeRowOf(row.Repo, row.Number)]; refreshed || row.AttributionRule < delivery.AttributionRuleVersion {
				row.MergedAt = s.refreshMerge(ctx, fills, row.Repo, row.Number).pull.MergedAt
			}
		}
		if row.MergedAt == nil {
			return false
		}
	}
	return true
}

// mergeRowOf is the key of pull request number of repo in mergeTimeFills.
func mergeRowOf(repo string, number int) mergeRow {
	return mergeRow{repo: strings.ToLower(repo), number: number}
}

// refreshMerge is pull request number of repo as the store holds it after one refresh from GitHub
// in this read (delivery.Attributor.RefreshPullRequestHead), read back by its number
// (delivery.MergedPullRequestOf) under the name the refresh writes it under, since the refresh
// answers only whether it failed. A second ask in the same read answers from fills and asks GitHub
// nothing. A refresh that failed leaves the row as it was, and a read that failed answers no row.
func (s *server) refreshMerge(ctx context.Context, fills *mergeTimeFills, repo string, number int) refreshedMerge {
	key := mergeRowOf(repo, number)
	if learned, refreshed := fills.refreshed[key]; refreshed {
		return learned
	}
	var refreshErr error
	if fills.readOnly {
		pulls, err := s.deps.Attributor.ReadPullRequestHead(ctx, s.deps.Store.Pool, s.deps.GitHub, repo, number, s.resultsNow())
		fills.answers.Add(pulls)
		refreshErr = err
	} else {
		refreshErr = s.deps.Attributor.RefreshPullRequestHead(ctx, s.deps.Store.Pool, s.deps.GitHub, s.deps.Events, repo, number, s.resultsNow())
	}
	if refreshErr != nil {
		slog.Warn("dispatch: refresh a merged pull request a results read orders", "repo", repo, "number", number, "error", refreshErr)
	}
	var learned refreshedMerge
	pull, found, err := delivery.MergedPullRequestOf(ctx, s.deps.Store.Pool, s.deps.Renames.Canonical(repo), number, *fills.answers)
	if err != nil {
		slog.Warn("dispatch: read a refreshed merged pull request", "repo", repo, "number", number, "error", err)
	} else {
		learned = refreshedMerge{pull: pull, found: found}
	}
	if fills.refreshed == nil {
		fills.refreshed = map[mergeRow]refreshedMerge{}
	}
	fills.refreshed[key] = learned
	return learned
}

// newerMerge orders merged pull requests newest merge first, as delivery lists them: the later
// time of merge first, a row holding no time after every row that holds one, and the higher number
// first between two of one time or of none.
func newerMerge(a, b delivery.MergeTime) int {
	switch {
	case a.MergedAt != nil && b.MergedAt != nil && !a.MergedAt.Equal(*b.MergedAt):
		return b.MergedAt.Compare(*a.MergedAt)
	case a.MergedAt != nil && b.MergedAt == nil:
		return -1
	case a.MergedAt == nil && b.MergedAt != nil:
		return 1
	}
	return b.Number - a.Number
}

// inMergeOrder gives pulls the times of merge the read can learn (fillMergeTimes) and orders them
// newest merge first by those times, and reports whether they can be ordered; pulls that cannot
// keep the order they came in.
func (c *comparisonReader) inMergeOrder(ctx context.Context, pulls []delivery.MergedPullRequest) bool {
	times := make([]delivery.MergeTime, len(pulls))
	for index, pull := range pulls {
		times[index] = pull.MergeTime()
	}
	if !c.server.fillMergeTimes(ctx, c.fills, times) {
		return false
	}
	for index := range pulls {
		pulls[index].MergedAt = times[index].MergedAt
	}
	slices.SortStableFunc(pulls, func(a, b delivery.MergedPullRequest) int { return newerMerge(a.MergeTime(), b.MergeTime()) })
	return true
}

// mergeSet is the merged pull requests of an issue in one repository, newest merge first
// (mergesIn): read is false where the store's read failed, and ordered false where the set cannot
// be ordered by when each merged (fillMergeTimes).
type mergeSet struct {
	pulls   []delivery.MergedPullRequest
	read    bool
	ordered bool
}

// mergeRepoKey is the repository part of a memo key of the merges of an issue: "" for the
// repository it resolves to, whose merges are read by the issue alone
// (delivery.Attributor.MergeOfIssue), and a linked repository lowercased.
func mergeRepoKey(repo string, linked bool) string {
	if !linked {
		return ""
	}
	return strings.ToLower(repo)
}

// mergesIn is the merged pull requests of issueKey in repo, read once per read of the results: for
// the repository the issue resolves to, the ones attributed to it
// (delivery.Attributor.MergeOfIssue), and for a linked one, the ones its external links name there
// (delivery.Attributor.MergeIn), each set ordered by when each merged (inMergeOrder).
func (c *comparisonReader) mergesIn(ctx context.Context, issueKey, repo string, linked bool) mergeSet {
	key := [2]string{issueKey, mergeRepoKey(repo, linked)}
	if set, memoized := c.mergeSets[key]; memoized {
		return set
	}
	var pulls []delivery.MergedPullRequest
	var err error
	if linked {
		pulls, err = c.server.deps.Attributor.MergeIn(ctx, c.server.deps.Store.Pool, issueKey, repo, c.answers)
	} else {
		pulls, err = c.server.deps.Attributor.MergeOfIssue(ctx, c.server.deps.Store.Pool, issueKey, c.answers)
	}
	set := mergeSet{read: err == nil}
	if err != nil {
		slog.Warn("dispatch: read the merges of a result's issue", "issue", issueKey, "repo", repo, "error", err)
	} else {
		set.pulls = pulls
		set.ordered = c.inMergeOrder(ctx, set.pulls)
	}
	c.mergeSets[key] = set
	return set
}

// mergeOn is the merge of issueKey's work on branch of repo, the issue's own repository or, where
// linked, one it links a merged pull request in (C6), read once per issue, repository and branch
// per read of the results. On the branch the issue's newest merged pull request there merged into,
// it is that pull request's merge commit. On another branch it is the merge commit of the pull
// request that landed that branch there: the newest merged pull request of repo whose head was
// the branch the merge is on and whose final head holds that merge (landingOf), followed through
// at most mergeLandings such pull requests. A branch the chain does not reach holds no merge of the
// work (mergeAbsent). Where the merges cannot be read or ordered, or a merge commit cannot be
// learned, the merge is not known (mergeUnknown), and a basis names none.
func (c *comparisonReader) mergeOn(ctx context.Context, issueKey, repo, branch string, linked bool) mergeAnswer {
	key := [3]string{issueKey, mergeRepoKey(repo, linked), branch}
	if answer, memoized := c.merges[key]; memoized {
		return answer
	}
	answer := c.readMergeOn(ctx, issueKey, repo, branch, linked)
	c.merges[key] = answer
	return answer
}

// readMergeOn is mergeOn's read, which the memo holds.
func (c *comparisonReader) readMergeOn(ctx context.Context, issueKey, repo, branch string, linked bool) mergeAnswer {
	set := c.mergesIn(ctx, issueKey, repo, linked)
	switch {
	case !set.read || !set.ordered:
		return mergeAnswer{state: mergeUnknown}
	case len(set.pulls) == 0:
		return mergeAnswer{state: mergeAbsent}
	}
	pull := set.pulls[0]
	for landed := 0; ; landed++ {
		merge, learned := c.mergeCommitOf(ctx, pull)
		switch {
		case !learned || pull.BaseRef == noMergedBranch:
			return mergeAnswer{state: mergeUnknown}
		case pull.BaseRef == branch:
			return mergeAnswer{state: mergeHeld, commit: merge}
		case landed == mergeLandings:
			return mergeAnswer{state: mergeAbsent}
		}
		var state mergeState
		if pull, state = c.landingOf(ctx, repo, pull.BaseRef, merge); state != mergeHeld {
			return mergeAnswer{state: state}
		}
	}
}

// landingOf is the pull request that landed branch of repo on its own base with merge in it: of the
// merged pull requests whose head was branch (delivery.LandingOf), newest merge first, the first
// whose final head holds merge (holds). It answers mergeAbsent where none does, and mergeUnknown
// where the landings cannot be read or ordered, or where whether a newer one's final head holds
// merge cannot be read, since that one may be the landing.
func (c *comparisonReader) landingOf(ctx context.Context, repo, branch, merge string) (delivery.MergedPullRequest, mergeState) {
	pulls, err := delivery.LandingOf(ctx, c.server.deps.Store.Pool, repo, branch, c.answers)
	if err != nil {
		slog.Warn("dispatch: read the landing of a branch", "repo", repo, "branch", branch, "error", err)
		return delivery.MergedPullRequest{}, mergeUnknown
	}
	if !c.inMergeOrder(ctx, pulls) {
		return delivery.MergedPullRequest{}, mergeUnknown
	}
	for _, pull := range pulls {
		switch c.holds(ctx, repo, pull.HeadSHA, merge) {
		case delivery.Holds:
			return pull, mergeHeld
		case delivery.HoldUnknown:
			return delivery.MergedPullRequest{}, mergeUnknown
		}
	}
	return delivery.MergedPullRequest{}, mergeAbsent
}

// mergeCommitOf is pull's merge commit, and whether Dispatch holds one: a row that holds none is
// refreshed from GitHub once per read (refreshMerge), and one GitHub's answer still names none for,
// or whose refresh failed, has none.
func (c *comparisonReader) mergeCommitOf(ctx context.Context, pull delivery.MergedPullRequest) (string, bool) {
	if pull.MergeCommitSHA != "" {
		return pull.MergeCommitSHA, true
	}
	learned := c.server.refreshMerge(ctx, c.fills, pull.Repo, pull.Number)
	return learned.pull.MergeCommitSHA, learned.found && learned.pull.MergeCommitSHA != ""
}

// holds is whether commit's history holds merge in repo, the repository of the commit, read once
// per read of the results. A commit holds itself, and that costs no read; every other pair is
// delivery's (delivery.Branches.CommitHolds), which keeps GitHub's answer for good.
func (c *comparisonReader) holds(ctx context.Context, repo, commit, merge string) delivery.HoldAnswer {
	if commit == merge {
		return delivery.Holds
	}
	key := [3]string{strings.ToLower(repo), commit, merge}
	if answer, memoized := c.holdAnswers[key]; memoized {
		return answer
	}
	answer := c.server.branches.CommitHolds(ctx, c.server.deps.Store.Pool, c.server.deps.GitHub, repo, commit, merge, c.server.resultsNow(), c.readOnly)
	c.holdAnswers[key] = answer
	return answer
}
