package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// merge_holds.go owns what Dispatch knows of the merge of an issue's work: which merged pull
// requests are an issue's, in its own repository and in a repository it links one in, newest
// merge first (MergeOfIssue, MergeIn); which merged pull requests landed a branch on the base it
// merged into (LandingOf); one merged pull request by its number (MergedPullRequestOf); the
// refresh of one merged pull request a results read must order (RefreshMergedPullRequest); and
// whether a commit's history holds a merge commit (CommitHolds).
// The results read (api/results_merge.go) chooses from these the merge each branch a check is
// compared with must hold, and holds no store or GitHub read of its own.
//
// Every read here takes the caller's request's answers as HeadsAttributedTo takes them: a
// writer's read passes none and reads the stored rows, and a read-only caller's read reads the
// rows as its own kept writes leave them, in a transaction it rolls back (HeadAnswers.Read).

// MergedPullRequest is one merged pull request as pull_request_heads stores it: what a reader
// needs to name the merge it made and to order it among others. Repo is the canonical name of its
// repository for a read by issue (MergeOfIssue, MergeIn) and the row's own name for a read by
// repository (LandingOf, MergedPullRequestOf). HeadSHA is its final head, the commit the merge
// commit merged. MergeCommitSHA is "" where no write has carried it yet, and MergedAt nil where no
// write has carried the time, which a reader that orders two or more such rows refreshes first
// (api's fillMergeTimes). AttributionRule is the rule the release before this one last stamped on
// the row, which says whether a read of a release that stores merge times has read it
// (AttributionRuleVersion).
type MergedPullRequest struct {
	Repo            string
	Number          int
	BaseRef         string
	HeadSHA         string
	MergeCommitSHA  string
	MergedAt        *time.Time
	AttributionRule int
}

// MergeTime is pull's row as a read orders merged pull requests by when each merged
// (api's fillMergeTimes), as AttributedHead.MergeTime is a head's.
func (pull MergedPullRequest) MergeTime() MergeTime {
	return MergeTime{Repo: pull.Repo, Number: pull.Number, MergedAt: pull.MergedAt, AttributionRule: pull.AttributionRule}
}

// mergedColumns are the columns every read of merged rows by repository takes, in readMerged's
// order.
const mergedColumns = `repo, number, base_ref, head_sha, merge_commit_sha, merged_at, attribution_rule`

// newestMergeFirst is the order of every list of merged rows here, and of the merged heads the
// statement's readers by issue order the same way (headsAttributedToSelect,
// newestPullRequestHeadSelect, mergeOfIssueSelect): the latest merge first, a row whose time no
// write has carried after every row whose time one has, and the higher number first where two
// share a time, so a list does not turn on the order the planner returned rows in. It is not
// event_at, the newest event's updated_at, which an edit of a pull request that merged earlier
// moves past a later merge.
const newestMergeFirst = `merged_at desc nulls last, number desc`

// readMerged runs statement, which selects mergedColumns' values in their order, on q with
// answers laid on (HeadAnswers.Read), and reads its rows; what names them in an error.
func readMerged(ctx context.Context, q Querier, answers HeadAnswers, what, statement string, parameters ...any) ([]MergedPullRequest, error) {
	var merged []MergedPullRequest
	err := answers.Read(ctx, q, func(q Querier) error {
		rows, err := q.Query(ctx, statement, parameters...)
		if err != nil {
			return err
		}
		defer rows.Close()
		merged = nil
		for rows.Next() {
			var pull MergedPullRequest
			if err := rows.Scan(&pull.Repo, &pull.Number, &pull.BaseRef, &pull.HeadSHA, &pull.MergeCommitSHA,
				&pull.MergedAt, &pull.AttributionRule); err != nil {
				return err
			}
			merged = append(merged, pull)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	return merged, nil
}

// MergeOfIssue is every merged pull request the statement gives issueKey in the repository it
// resolves to (attributedTerms, not linked), newest merge first (newestMergeFirst): the merges of
// the issue's work there, each on the branch it merged into. The read is keyed by the issue and
// not by a result's commit because Legion merges with squash: the commit that lands on the base
// branch is the one the squash makes, which is no pull request's head and so no row's head_sha.
// Keying by the issue rests on the rule of the statement (own_head, attributionHeld), which gives
// a pull request to an issue only where its head is a branch of the pull request's own
// repository. A merged row carrying no base_ref names no branch; a timeline row stores no base
// branch, so a pull request with no results row is not listed.
func (a *Attributor) MergeOfIssue(ctx context.Context, q Querier, issueKey string, answers HeadAnswers) ([]MergedPullRequest, error) {
	statements, err := a.attribution()
	if err != nil {
		return nil, err
	}
	return readMerged(ctx, q, answers, "the merged pull requests of "+issueKey, statements.mergeOfIssue, issueKey)
}

// mergeOfIssueSelect is MergeOfIssue's select over the statement's terms by issue.
const mergeOfIssueSelect = `
	select a.repo, a.number, h.base_ref, h.head_sha, h.merge_commit_sha, h.merged_at, h.attribution_rule
	from attributed a join heads h on h.j_repo = a.repo and h.j_number = a.number
	where a.issue_key = $1 and not a.linked and h.state = 'merged'
	order by h.merged_at desc nulls last, a.number desc`

// MergeIn is MergeOfIssue for repoFull, a repository issueKey does not resolve to: every merged
// pull request the statement gives the issue by a link of its URL in repoFull (attributedTerms'
// linked), newest merge first. An issue whose work spans two repositories links the pull request
// it has in the second one, and MergeOfIssue reads the repository the issue resolves to alone. A
// link to a pull request of the repository the issue resolves to is no linked pull request, so a
// repoFull the issue resolves to answers none. repoFull is compared without case with the
// canonical name of each pull request.
func (a *Attributor) MergeIn(ctx context.Context, q Querier, issueKey, repoFull string, answers HeadAnswers) ([]MergedPullRequest, error) {
	statements, err := a.attribution()
	if err != nil {
		return nil, err
	}
	return readMerged(ctx, q, answers, fmt.Sprintf("the merged pull requests %s links in %s", issueKey, repoFull),
		statements.mergeIn, issueKey, repoFull)
}

// mergeInSelect is MergeIn's select over the statement's terms by issue.
const mergeInSelect = `
	select a.repo, a.number, h.base_ref, h.head_sha, h.merge_commit_sha, h.merged_at, h.attribution_rule
	from attributed a join heads h on h.j_repo = a.repo and h.j_number = a.number
	where a.issue_key = $1 and a.linked and lower(a.repo) = lower($2::text) and h.state = 'merged'
	order by h.merged_at desc nulls last, a.number desc`

// LandingOf is every merged pull request of repo whose head was branch, newest merge first: the
// pull requests that landed branch on the base each merged into. A reader that follows an
// issue's merge on an integration branch to the default branch asks it of that branch, and takes
// the newest one whose final head holds the merge it follows (api/results_merge.go). repo is
// compared without case, since a row an envelope wrote carries GitHub's spelling; the branch is
// compared as it is, since a git branch name is case-sensitive. The read is one lookup on
// pull_request_heads_merged_head_ref (migration 20261125125702).
func LandingOf(ctx context.Context, q Querier, repo, branch string, answers HeadAnswers) ([]MergedPullRequest, error) {
	return readMerged(ctx, q, answers, fmt.Sprintf("the merged pull requests of %s from %s", repo, branch), landingQuery, repo, branch)
}

var landingQuery = `
	select ` + mergedColumns + ` from pull_request_heads
	where lower(repo) = lower($1) and head_ref = $2 and state = 'merged'
	order by ` + newestMergeFirst

// MergedPullRequestOf is pull request number of repo as pull_request_heads stores it where it
// merged, and false where no merged row of it is stored: the read a results read makes of a row
// it has just refreshed (RefreshMergedPullRequest, which answers only whether the refresh failed,
// and nothing where GitHub refused it and the row was stamped), to learn the merge commit and the
// time of merge the refresh stored (api/results_merge.go). repo is compared without case, as
// LandingOf compares it.
func MergedPullRequestOf(ctx context.Context, q Querier, repo string, number int, answers HeadAnswers) (MergedPullRequest, bool, error) {
	merged, err := readMerged(ctx, q, answers, fmt.Sprintf("the merged pull request %s#%d", repo, number), mergedPullRequestOfQuery, repo, number)
	if err != nil || len(merged) == 0 {
		return MergedPullRequest{}, false, err
	}
	return merged[0], true, nil
}

var mergedPullRequestOfQuery = `
	select ` + mergedColumns + ` from pull_request_heads
	where lower(repo) = lower($1) and number = $2 and state = 'merged'`

// RefreshMergedPullRequest reads pull request number of repoFull from GitHub as the reconcile's
// refresh does (refreshPullRequestHead), for a results read that must order the merges of an
// issue and holds one with no merge time (api's fillMergeTimes). A writer's read passes no
// answers and stores what GitHub answered, the merge commit and the merge time among it (headRow,
// upsertHeadRow), and stamps a row GitHub refused (MarkPullRequestHeadChecked), as the refresh
// does. A read-only caller's read passes its request's answers, which keep every write of the
// same code for that request and store nothing (facts, HeadAnswers). It answers the read's error,
// which a refusal is not.
func (a *Attributor) RefreshMergedPullRequest(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, answers *HeadAnswers, repoFull string, number int, now time.Time,
) error {
	_, _, err := a.refreshPullRequestHead(ctx, facts{pool: pool, answers: answers}, client, nil, repoFull, number, now)
	return err
}

// HoldAnswer is what Dispatch can say about one commit's history holding one merge commit.
// HoldUnknown is that it cannot say now: the read failed, or the App cannot read the repository.
// It is kept apart from DoesNotHold, as BranchAnswer keeps BranchUnknown apart, because a read
// that failed is no verdict about the work.
type HoldAnswer int

const (
	// HoldUnknown is no answer: nothing stored answers, and GitHub could not be read.
	HoldUnknown HoldAnswer = iota
	// Holds is that the commit is the merge commit or a later commit whose history holds it.
	Holds
	// DoesNotHold is that the commit's history does not hold the merge commit.
	DoesNotHold
)

// String names the answer for a log line and a test's failure message.
func (a HoldAnswer) String() string {
	switch a {
	case Holds:
		return "holds"
	case DoesNotHold:
		return "does not hold"
	default:
		return "unknown"
	}
}

// CommitHolds answers whether commit's history holds merge, in repoFull, the repository of the
// commit. A stored answer answers first; else GitHub's compare of commit with merge
// (githubapp.Client.CommitOnBranch with commit as its base side: `behind` and `identical` say the
// commit holds the merge) is asked, and its answer stored in commit_holds and kept for good, a no
// as a yes is, since two commits never change. A read that fails is stored nowhere and answers
// HoldUnknown, so the next read asks GitHub again: a 403 or a 404 (the App cannot read the
// repository, or the repository does not hold one of the commits) as a rate limit, a 5xx or a
// timeout does. The repository is lowercased in the key, as branch_commits keys its own
// (readBranchCommit), since one repository reaches here under GitHub's spelling and under the
// lowercased one an issue's repository is stored under.
func (c *Branches) CommitHolds(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, repoFull, commit, merge string, now time.Time, readOnly bool,
) HoldAnswer {
	var holds bool
	err := pool.QueryRow(ctx, `
		select holds from commit_holds where repo = lower($1) and commit_sha = $2 and merge_sha = $3
	`, repoFull, commit, merge).Scan(&holds)
	switch {
	case err == nil:
		return holdAnswerOf(holds)
	case !errors.Is(err, pgx.ErrNoRows):
		slog.Warn("dispatch: read whether a commit holds a merge", "repo", repoFull, "commit", commit, "merge", merge, "error", err)
	}
	owner, repo, token, err := repositoryRead(ctx, client, repoFull)
	if err == nil {
		holds, err = client.CommitOnBranch(ctx, token, owner, repo, commit, merge)
	}
	if err != nil {
		if !cannotSeeRepository(err) {
			slog.Warn("dispatch: ask whether a commit holds a merge", "repo", repoFull, "commit", commit, "merge", merge, "error", err)
		}
		return HoldUnknown
	}
	if readOnly {
		return holdAnswerOf(holds)
	}
	if _, err := pool.Exec(ctx, `
		insert into commit_holds (repo, commit_sha, merge_sha, holds, checked_at)
		values (lower($1), $2, $3, $4, $5)
		on conflict (repo, commit_sha, merge_sha) do nothing
	`, repoFull, commit, merge, holds, now); err != nil {
		slog.Warn("dispatch: record whether a commit holds a merge", "repo", repoFull, "commit", commit, "merge", merge, "error", err)
	}
	return holdAnswerOf(holds)
}

// holdAnswerOf is GitHub's yes or no as a HoldAnswer.
func holdAnswerOf(holds bool) HoldAnswer {
	if holds {
		return Holds
	}
	return DoesNotHold
}
