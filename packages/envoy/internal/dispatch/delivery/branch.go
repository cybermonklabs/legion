package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// branch.go owns every answer about whether one branch of one repository carries one commit,
// and about whether the App can read the repository at all. One place holds the GitHub reads,
// the branch_commits table and how long each answer is believed, so a change to any of the
// three is a change here and nowhere else. Nothing here reads or writes default_branch_commits,
// the table of the release before this one, which this release leaves exactly as it is: its
// drop is a contracting migration of a later release (dispatch://INFRA-1700). Its one caller is
// the results read (api/results_currency.go), which picks the branch each check is measured
// against and keeps a memo for the span of one request, and holds no policy of its own.

// BranchCommit is what Dispatch last learned about one commit of one branch of one repository:
// whether that branch's history carries it, and when that was read.
type BranchCommit struct {
	OnBranch  bool
	CheckedAt time.Time
}

// BranchCommitStaleAfter is how long a "no" is believed: a commit a branch does not carry today
// is one a merge may put on it in a minute. A "yes" is kept for good, on every branch and not
// the default branch alone, because a rewrite of a branch does not undo that the work reached
// it, which is the whole of what a result at that commit claims. A force-push that drops the
// commit afterwards leaves a yes standing that a fresh read would deny, and that is the
// accepted cost of not spending one GitHub compare read per result commit on every read of the
// Verification tab: the tab refetches an issue's results on every result.recorded, and a
// branch is rewritten far more rarely than its results are read. A repository's default branch,
// and the empty answer that says the App cannot read it, are believed for the same span.
const BranchCommitStaleAfter = 5 * time.Minute

// Branches holds each repository's default branch across reads. The Verification tab refetches
// the results on every result.recorded, and a read holding an `after_merge` or `default_branch`
// check, or a `before_merge` result at a commit heading no pull request Dispatch knows, would
// otherwise spend a GET /repos/{owner}/{repo} each time. Its zero value is an empty cache, and
// the server holds one for the process: the answers are a repository's, not a request's, and
// every goroutine reading results shares them. What a branch holds of a commit is cached in the
// database instead (branch_commits), since that answer outlives any one process.
type Branches struct {
	mu    sync.Mutex
	known map[string]cachedBranch
}

// cachedBranch is what this process last learned about one repository's default branch: the
// newest name GitHub gave for it, whether the newest read could say at all, and when that read
// was made. A read that cannot say holds the name it already had rather than replacing it with
// nothing: the name is what a stored answer in branch_commits is keyed under, so keeping it
// lets a commit Dispatch has already answered for go on answering while the App cannot be read
// (CommitOnBranch), which is what it did before an answer was keyed by a branch at all. The
// pair is held as long as any other answer, so an unreadable repository is not asked about on
// every read.
type cachedBranch struct {
	branch   string
	readable bool
	readAt   time.Time
}

// RepositoryReadable reports whether the App can read repoFull right now: it answered that
// repository's default branch, within BranchCommitStaleAfter of now. Reading the default branch
// is the one read that says whether Dispatch can see the repository at all, whatever branch a
// caller goes on to ask about (branchOf), which is why this question is asked of that read and
// of no other. It is false for a repository the App is not installed on, one its token is
// refused, a deployment holding no App key, and a read that failed; all of them mean Dispatch
// cannot see the repository, which is the unknown currency rather than a verdict about the work.
func (c *Branches) RepositoryReadable(ctx context.Context, client *githubapp.Client, repoFull string, now time.Time) bool {
	return c.branchOf(ctx, client, repoFull, now) != ""
}

// CommitOnBranch answers whether branch of repoFull carries sha, and whether Dispatch could say
// at all. An empty branch means the repository's own default branch, resolved through branchOf:
// a `default_branch` check passes none and so is measured against that branch whatever the work
// merged into, while an `after_merge` check passes the branch its issue's pull request merged
// into (api/results_currency.go).
//
// What branch_commits holds for that repository, branch and commit answers first while it still
// stands — a yes for good, a no for BranchCommitStaleAfter — and GitHub is read against the
// resolved branch, and its answer written back under it, otherwise. A caller that names a
// branch spends no GET /repos/{owner}/{repo}: the default branch is resolved only for a caller
// that names none, and the stored row is read before any call to GitHub, so a named branch
// Dispatch has already answered for costs nothing at all. A read that failed is the unknown
// answer, as is a repository the App cannot read.
//
// A caller that names no branch, of a repository the App cannot read just now, is answered from
// the stored row alone, under the last default branch this process resolved for that repository
// (named). A row is keyed by a branch, so the name has to come from somewhere before the cache
// can be asked at all, and without this the first unreadable moment would turn every
// `default_branch` result, and every `after_merge` result of an issue Dispatch holds no merge
// of, from current to unknown for BranchCommitStaleAfter — even where the permanent yes is
// stored. A default branch renamed while the App cannot be read leaves that answer under the
// old name, which is the same staleness the retention rule already accepts of a yes. Where this
// process has resolved no name for the repository at all, there is nothing to ask the cache
// about and the answer is unknown.
func (c *Branches) CommitOnBranch(
	ctx context.Context,
	pool *store.Pool,
	client *githubapp.Client,
	repoFull, branch, sha string,
	now time.Time,
) (onBranch, known bool) {
	storedOnly := false
	if branch == "" {
		branch = c.branchOf(ctx, client, repoFull, now)
		if branch == "" {
			if branch = c.named(repoFull); branch == "" {
				return false, false
			}
			storedOnly = true
		}
	}
	stored, found, err := readBranchCommit(ctx, pool, repoFull, branch, sha)
	if err != nil {
		slog.Warn("dispatch: read a commit's branch answer", "repo", repoFull, "branch", branch, "commit", sha, "error", err)
	}
	if found && (stored.OnBranch || now.Sub(stored.CheckedAt) < BranchCommitStaleAfter) {
		return stored.OnBranch, true
	}
	if storedOnly {
		// The App could not read this repository a moment ago, so a compare read of it would
		// be refused too: the stored row was the whole of what could answer.
		return false, false
	}
	answer, err := askGitHubCommitOnBranch(ctx, client, repoFull, branch, sha)
	if err != nil {
		if !cannotSeeRepository(err) {
			slog.Warn("dispatch: ask whether a commit is on a branch", "repo", repoFull, "branch", branch, "commit", sha, "error", err)
		}
		return false, false
	}
	if err := writeBranchCommit(ctx, pool, repoFull, branch, sha, answer, now); err != nil {
		slog.Warn("dispatch: record a commit's branch answer", "repo", repoFull, "branch", branch, "commit", sha, "error", err)
	}
	return answer, true
}

// branchOf is repoFull's default branch, held for BranchCommitStaleAfter, and "" when Dispatch
// cannot say.
func (c *Branches) branchOf(ctx context.Context, client *githubapp.Client, repoFull string, now time.Time) string {
	if held, fresh := c.get(repoFull, now); fresh {
		return held
	}
	branch := ""
	if client != nil {
		read, err := repositoryDefaultBranch(ctx, client, repoFull)
		if err != nil {
			if !cannotSeeRepository(err) {
				slog.Warn("dispatch: read the default branch of a repository", "repo", repoFull, "error", err)
			}
		} else {
			branch = read
		}
	}
	c.put(repoFull, branch, now)
	return branch
}

// get is the branch held for repo and whether that answer still stands at now. A read that
// could not say is the empty branch, as it was when it was made: the name kept beside it is
// for named alone, and answering it here would tell RepositoryReadable that a repository the
// App cannot read is readable.
func (c *Branches) get(repo string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held, found := c.known[repo]
	if !found || now.Sub(held.readAt) >= BranchCommitStaleAfter {
		return "", false
	}
	if !held.readable {
		return "", true
	}
	return held.branch, true
}

// named is the last default branch this process resolved for repo, whatever its age and
// whatever the newest read said, and "" where it has resolved none. It is what CommitOnBranch
// keys the stored answer by while the App cannot read the repository.
func (c *Branches) named(repo string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.known[repo].branch
}

// put records what GitHub said about repo's default branch at now. A read that could not say
// leaves the name standing and marks the repository unreadable, so get answers nothing for it
// while named still answers the name.
func (c *Branches) put(repo, branch string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.known == nil {
		c.known = map[string]cachedBranch{}
	}
	held := c.known[repo]
	held.readAt = now
	held.readable = branch != ""
	if branch != "" {
		held.branch = branch
	}
	c.known[repo] = held
}

// cannotSeeRepository is repositoryUnreadable — the App is installed on no repository covering
// this one, or its token is refused it — and the deployment holding no App key at all. Each
// means Dispatch cannot see the repository, which is an answer rather than a fault worth
// logging; repositoryUnreadable alone is the reconcile's rule for a row it stamps instead of
// warning about, and a deployment with no key has no rows to stamp.
func cannotSeeRepository(err error) bool {
	return repositoryUnreadable(err) || errors.Is(err, githubapp.ErrNoAppKey)
}

// readBranchCommit is what branch_commits holds for one commit of one branch, and whether it
// holds anything at all.
func readBranchCommit(ctx context.Context, pool *store.Pool, repoFull, branch, sha string) (BranchCommit, bool, error) {
	var answer BranchCommit
	err := pool.QueryRow(ctx, `
		select on_branch, checked_at from branch_commits where repo = $1 and branch = $2 and sha = $3
	`, repoFull, branch, sha).Scan(&answer.OnBranch, &answer.CheckedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return BranchCommit{}, false, nil
	}
	if err != nil {
		return BranchCommit{}, false, fmt.Errorf("read whether %s@%s is on %s: %w", repoFull, sha, branch, err)
	}
	return answer, true, nil
}

// writeBranchCommit records GitHub's answer about one commit of one branch.
func writeBranchCommit(ctx context.Context, pool *store.Pool, repoFull, branch, sha string, onBranch bool, now time.Time) error {
	_, err := pool.Exec(ctx, `
		insert into branch_commits (repo, branch, sha, on_branch, checked_at)
		values ($1, $2, $3, $4, $5)
		on conflict (repo, branch, sha) do update set on_branch = excluded.on_branch, checked_at = excluded.checked_at
	`, repoFull, branch, sha, onBranch, now)
	if err != nil {
		return fmt.Errorf("record whether %s@%s is on %s: %w", repoFull, sha, branch, err)
	}
	return nil
}

// repositoryRead is the prologue of every read the App makes about one repository, here, in
// commit_held.go and in pull_request_heads.go's ReadThroughCommitPulls: it splits repoFull
// into the owner and repo GitHub's paths take and mints that repository's installation token.
// A malformed name comes back as splitRepo's own error and a refused mint as that error
// wrapped, which cannotSeeRepository and repositoryUnreadable classify.
func repositoryRead(ctx context.Context, client *githubapp.Client, repoFull string) (owner, repo, token string, err error) {
	owner, repo, err = splitRepo(repoFull)
	if err != nil {
		return "", "", "", err
	}
	token, err = client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return "", "", "", fmt.Errorf("mint installation token for %s: %w", repoFull, err)
	}
	return owner, repo, token, nil
}

// repositoryDefaultBranch is the name of repoFull's default branch, read through the App.
func repositoryDefaultBranch(ctx context.Context, client *githubapp.Client, repoFull string) (string, error) {
	owner, repo, token, err := repositoryRead(ctx, client, repoFull)
	if err != nil {
		return "", err
	}
	return client.DefaultBranch(ctx, token, owner, repo)
}

// askGitHubCommitOnBranch asks the App whether sha is on branch of repoFull. It is the read
// CommitOnBranch makes when nothing stored answers, named for the call it spends so the two
// cannot be mistaken for one another.
func askGitHubCommitOnBranch(ctx context.Context, client *githubapp.Client, repoFull, branch, sha string) (bool, error) {
	owner, repo, token, err := repositoryRead(ctx, client, repoFull)
	if err != nil {
		return false, err
	}
	return client.CommitOnBranch(ctx, token, owner, repo, branch, sha)
}
