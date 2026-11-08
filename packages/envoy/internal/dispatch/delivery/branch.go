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

// DefaultBranch is the branch argument that means "the repository's own default branch, which
// delivery resolves": a caller holding no branch name of its own passes it, and CommitOnBranch
// resolves the repository's own name through branchOf before it reads or writes anything under
// it. It is a named constant rather than a bare empty string at each call site because the
// empty string is what more than one answer about a branch is spelled with — the base branch of
// an issue Dispatch holds no merge of is the other (BaseBranchMergedFor), and it means "no
// branch to measure against" rather than this — so a caller that passed one for the other would
// fall back to the default branch with no error and no failing type.
const DefaultBranch = ""

// BranchAnswer is what Dispatch could say about one commit of one branch: the whole of what
// CommitOnBranch returns, and the whole of what a caller measuring a result's currency has to
// go on. The four cases are kept apart because each is a different thing to do: two say
// something about the commit, one says something about the branch and nothing about the
// commit, and one says nothing at all. A reader that folded the last two into "not on it"
// would turn a GitHub hiccup into a verdict about the work, which is the one thing the
// currency rule may not do (api/results_currency.go, of).
type BranchAnswer int

const (
	// BranchUnknown is Dispatch not being able to say anything: a compare read refused by a
	// rate limit, a 5xx, a timeout or a cancelled request; a repository whose own read failed,
	// for any of those reasons or because the App cannot see it, where the caller named no
	// branch and the stored rows answered nothing; and a repository whose default branch no
	// read of this process ever named, which a deployment holding no App key makes of every
	// repository. Wherever this is the last word the currency is unknown, and wherever it is
	// any word at all no later branch's "not on it" may make a result older: a fault of
	// Dispatch's or a hiccup of GitHub's must not make a reviewer refuse an approval.
	BranchUnknown BranchAnswer = iota
	// BranchCarries is the branch's history holding the commit: the commit is that branch's
	// head, or an ancestor of it. It is the whole answer wherever it is given, and no branch
	// after it is asked about.
	BranchCarries
	// BranchDoesNotCarry is the branch not holding the commit, read from GitHub or from the
	// branch_commits row a read of GitHub left.
	BranchDoesNotCarry
	// BranchUnseen is the App not being able to see that branch of that repository at all:
	// exactly cannotSeeRepository — GitHub's 403 or 404 to the compare read
	// (githubapp.CheckRepositoryResponse, which makes a rate limit behind either status the
	// *githubapp.RateLimitError instead, so a rate limit is the transient kind above) and a
	// deployment holding no App key — and the hold such a read leaves on the pair.
	//
	// GitHub answers 403 or 404 for a branch it no longer holds, for a repository the
	// installation has lost, and for a commit the repository does not hold. All three are
	// honest grounds for saying that this branch does not carry this commit: a branch that is
	// gone carries nothing, a repository Dispatch may not read carries nothing Dispatch can
	// show, and a commit GitHub cannot find one side of the comparison at is on no branch it
	// could name. That is why an earlier branch answering this way lets the walk go on to the
	// next branch, as a plain "not on it" does. None of the three is a reason to believe
	// anything about a later branch, though — the branch asked about next may be one GitHub
	// answers perfectly well for — which is why it is unknown wherever it is the last word.
	BranchUnseen
)

// String names the answer for a log line and a test's failure message, where the bare integer
// of the underlying type would say nothing.
func (a BranchAnswer) String() string {
	switch a {
	case BranchCarries:
		return "carries"
	case BranchDoesNotCarry:
		return "does not carry"
	case BranchUnseen:
		return "unseen"
	default:
		return "unknown"
	}
}

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

// Branches holds, across reads, each repository's default branch and what the compare reads of
// each repository-and-branch pair last did. The Verification tab refetches the results on every
// result.recorded, and a read holding an `after_merge` or `default_branch` check, or a
// `before_merge` result at a commit heading no pull request Dispatch knows, would otherwise
// spend a GET /repos/{owner}/{repo} each time, and one compare read per result commit of a
// branch the App cannot see. Its zero value is an empty cache, and the server holds one for the
// process: the answers are a repository's, not a request's, and every goroutine reading results
// shares them. What a branch holds of a commit is cached in the database instead
// (branch_commits), since that answer outlives any one process; what the compare reads of a
// pair did is not, since a refusal is a failure to learn anything rather than something learned.
type Branches struct {
	mu       sync.Mutex
	known    map[string]cachedBranch
	compares map[repoBranch]compareHold
}

// repoBranch is one repository and one branch of it: the pair a compare read is made about, and
// the key the pairs no compare read could answer are held under.
type repoBranch struct {
	repo   string
	branch string
}

// compareHold is what the compare reads of one repository-and-branch pair last did: when one
// was last refused as unseen, which holds the pair for BranchCommitStaleAfter, and when one was
// last answered, which a hold arriving late loses to.
//
// Both moments are kept because a read that is slow to fail would otherwise hold a pair a
// faster read has since been answered about. The failing read passes the moment it began, and a
// hold is recorded only where no answer has been recorded since (markUnseen); an answer is
// recorded for every pair and not only for a pair under a hold, since a read that recorded
// nothing could beat nothing.
//
// So the map holds one entry per repository-and-branch pair this process has made a compare
// read about: the repositories whose results are read, times the branches their issues merged
// into and their own default branches. An entry read once nothing about it still stands is
// deleted there and then (unseenHeld); an entry whose pair nothing asks about again lingers for
// the life of the process, as known's entry for a repository does.
type compareHold struct {
	unseenAt   time.Time
	answeredAt time.Time
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
	_, readable := c.branchOf(ctx, client, repoFull, now)
	return readable
}

// CommitOnBranch answers what Dispatch can say about branch of repoFull carrying sha: one of
// the four BranchAnswer cases, which is the whole of what the currency read has to go on.
// DefaultBranch — the empty branch — means the repository's own default branch, resolved
// through branchOf: a `default_branch` check names no branch of its own and so is measured
// against that branch whatever the work merged into, while an `after_merge` check asks about
// the branch its issue's pull request merged into and then about the default branch
// (api/results_currency.go).
//
// What branch_commits holds for that repository, branch and commit answers first while it still
// stands — a yes for good, a no for BranchCommitStaleAfter — and GitHub is read against the
// resolved branch, and its answer written back under it, otherwise. A caller that names a
// branch spends no GET /repos/{owner}/{repo}: the default branch is resolved only for a caller
// that names none, and the stored row is read before any call to GitHub, so a named branch
// Dispatch has already answered for costs nothing at all.
//
// A compare read the App is refused is BranchUnseen, and where the caller named the branch it
// holds that repository and branch for BranchCommitStaleAfter: every caller naming the pair
// meanwhile is answered BranchUnseen without a second call (unseenHeld). Two pairs would
// otherwise cost one refused GitHub call per result commit on every read of the Verification
// tab, for as long as the issue exists: a base branch deleted when its pull request merged,
// which a repository that deletes merged branches does on every merge and which a compare read
// answers 404 for, and a repository the App can no longer read, whose refusal nothing else
// caches (githubapp.Client.RepositoryToken caches the tokens it minted, not the mints it was
// refused). Neither failure says anything about the commit, so nothing is written to
// branch_commits for it; the hold is this process's alone, and it is released as soon as a
// compare read of the pair is answered (markSeen).
//
// Nothing is held for a caller that named no branch. A compare read of the repository's own
// default branch is made only just after branchOf read that repository and GitHub named the
// branch, so a 403 or 404 to it is not a branch that is gone; and it is a repository the App
// has lost for no longer than that read's own BranchCommitStaleAfter, after which branchOf
// reads again, learns nothing, and this answers before any compare read is spent. What is left
// is GitHub's third reason for those two statuses: a commit the repository does not hold. One
// such commit must not hold the branch that every `default_branch` result of the repository,
// and every `after_merge` result of it no stored row answers, is measured against.
//
// Every other failure of the compare read — a rate limit, a 5xx, a timeout, a request whose
// caller went away — is BranchUnknown and holds nothing at all, so the next read asks GitHub
// again. A hold there would make one bad second of GitHub's five minutes in which every result
// of the repository reads unknown, which is a reviewer refusing approvals over a hiccup.
//
// A caller that names no branch, of a repository the App cannot read just now, is answered from
// the stored row alone, under the last default branch this process resolved for that repository
// (branchOf's name). A row is keyed by a branch, so the name has to come from somewhere before
// the cache can be asked at all, and without this the first unreadable moment would turn every
// `default_branch` result, and every `after_merge` result of an issue Dispatch holds no merge
// of, from current to unknown for BranchCommitStaleAfter — even where the permanent yes is
// stored. A default branch renamed while the App cannot be read leaves that answer under the
// old name, which is the same staleness the retention rule already accepts of a yes. Where this
// process has resolved no name for the repository at all, there is nothing to ask the cache
// about and the answer is BranchUnknown.
func (c *Branches) CommitOnBranch(
	ctx context.Context,
	pool *store.Pool,
	client *githubapp.Client,
	repoFull, branch, sha string,
	now time.Time,
) BranchAnswer {
	// named is whether the caller holds a branch of its own, which is what decides whether a
	// refused compare read holds the pair. readable is what the newest read of the repository
	// could say, which only a caller naming no branch asks for; a caller that names a branch
	// has asked nothing of the repository yet, so the compare read below is what finds out.
	named := branch != DefaultBranch
	readable := true
	if !named {
		if branch, readable = c.branchOf(ctx, client, repoFull, now); branch == "" {
			return BranchUnknown
		}
	}
	stored, found, err := readBranchCommit(ctx, pool, repoFull, branch, sha)
	if err != nil {
		slog.Warn("dispatch: read a commit's branch answer", "repo", repoFull, "branch", branch, "commit", sha, "error", err)
	}
	if found && (stored.OnBranch || now.Sub(stored.CheckedAt) < BranchCommitStaleAfter) {
		if stored.OnBranch {
			return BranchCarries
		}
		return BranchDoesNotCarry
	}
	if !readable {
		// The newest read of this repository could say nothing, so a compare read of it would
		// be refused too: the stored row was the whole of what could answer. It is
		// BranchUnknown and not BranchUnseen because that read fails for every reason a read
		// fails for — a 5xx and a rate limit as much as the App having lost the repository —
		// and branchOf keeps no account of which (cachedBranch).
		return BranchUnknown
	}
	pair := repoBranch{repo: repoFull, branch: branch}
	if c.unseenHeld(pair, now) {
		return BranchUnseen
	}
	answer, err := askGitHubCommitOnBranch(ctx, client, repoFull, branch, sha)
	if err != nil {
		if !cannotSeeRepository(err) {
			slog.Warn("dispatch: ask whether a commit is on a branch", "repo", repoFull, "branch", branch, "commit", sha, "error", err)
			return BranchUnknown
		}
		if named {
			c.markUnseen(pair, now)
		}
		return BranchUnseen
	}
	c.markSeen(pair, now)
	if err := writeBranchCommit(ctx, pool, repoFull, branch, sha, answer, now); err != nil {
		slog.Warn("dispatch: record a commit's branch answer", "repo", repoFull, "branch", branch, "commit", sha, "error", err)
	}
	if answer {
		return BranchCarries
	}
	return BranchDoesNotCarry
}

// branchOf is the default branch this process holds for repoFull and whether the newest read of
// it could say at all. The name is the newest GitHub gave, kept across a read that could not say
// and "" where no read ever named one (cachedBranch); both are held for BranchCommitStaleAfter.
//
// The two are returned together because each caller wants a different one. RepositoryReadable
// wants only whether the newest read could say, since that is the whole of what "can Dispatch
// see this repository" means. CommitOnBranch wants the name even when the newest read could not
// say, since a branch_commits row is keyed by a branch and the name has to come from somewhere
// before that row can be asked for at all.
func (c *Branches) branchOf(ctx context.Context, client *githubapp.Client, repoFull string, now time.Time) (string, bool) {
	if held, fresh := c.get(repoFull, now); fresh {
		return held.branch, held.readable
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
	return c.put(repoFull, branch, now), branch != ""
}

// get is what this process holds about repo's default branch, exactly as put left it, and
// whether that answer still stands at now.
func (c *Branches) get(repo string, now time.Time) (cachedBranch, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held, found := c.known[repo]
	if !found || now.Sub(held.readAt) >= BranchCommitStaleAfter {
		return cachedBranch{}, false
	}
	return held, true
}

// put records what GitHub said about repo's default branch at now and answers the name held
// afterwards. A read that could not say leaves the name standing and marks the repository
// unreadable, for the reason cachedBranch gives.
func (c *Branches) put(repo, branch string, now time.Time) string {
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
	return held.branch
}

// unseenHeld reports whether a compare read of pair was refused as unseen within
// BranchCommitStaleAfter of now, the span every other answer here is believed for. The refusal
// is held against the pair and not against the commit that happened to be asked about, because
// that is what it is about: a branch GitHub cannot compare against answers for no commit, and
// the next result commit of the same branch would spend the same refused call.
//
// An entry nothing about which still stands is deleted as it is read: the hold has run out, and
// the pair was last answered longer ago than that same span, so no compare read still in flight
// can have begun before that answer — one request of this client lives at most
// githubapp.RequestTimeout, which is far inside BranchCommitStaleAfter. That is what keeps a
// branch deleted once from costing an entry for the life of the process, which only a pair
// nothing reads again now does.
func (c *Branches) unseenHeld(pair repoBranch, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	held, found := c.compares[pair]
	if !found {
		return false
	}
	if now.Sub(held.unseenAt) < BranchCommitStaleAfter {
		return true
	}
	if now.Sub(held.answeredAt) >= BranchCommitStaleAfter {
		delete(c.compares, pair)
	}
	return false
}

// markUnseen holds pair as one the App cannot see, a compare read begun at readAt having been
// refused. Only the refusal cannotSeeRepository names reaches here, and only from a caller that
// named the branch: a rate limit, a 5xx, a timeout and a cancelled request hold nothing, so the
// next read asks GitHub again rather than taking one bad second for five minutes.
//
// The hold is recorded only where no compare read of the pair has been answered since readAt.
// A read that is slow to fail and one that is quick to succeed race, and the slow one must not
// hold a pair GitHub has just answered about: what the slow read learned, it learned before
// that answer, and the answer is the newer fact however the two returned. readAt is the
// reader's own now, taken before the call went out, so it is at or before the moment GitHub was
// asked; the few milliseconds in it only ever lose a hold that was nearly beaten anyway, which
// is the side to be wrong on. It dates the hold too, so a hold runs out a hair early rather
// than a hair late.
func (c *Branches) markUnseen(pair repoBranch, readAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.compares == nil {
		c.compares = map[repoBranch]compareHold{}
	}
	held := c.compares[pair]
	if held.answeredAt.After(readAt) {
		return
	}
	held.unseenAt = readAt
	c.compares[pair] = held
}

// markSeen records that a compare read of pair begun at readAt was answered, which drops
// whatever hold the pair was under: the map says which pairs cannot be seen now rather than
// which could not be seen once. The moment outlives the hold it drops, since it is what a read
// still in flight that fails loses to (markUnseen); an older moment never replaces a newer one,
// so two reads answered out of order leave the newer standing.
func (c *Branches) markSeen(pair repoBranch, readAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.compares == nil {
		c.compares = map[repoBranch]compareHold{}
	}
	held := c.compares[pair]
	held.unseenAt = time.Time{}
	if readAt.After(held.answeredAt) {
		held.answeredAt = readAt
	}
	c.compares[pair] = held
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
