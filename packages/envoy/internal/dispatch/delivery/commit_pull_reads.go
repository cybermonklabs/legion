package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// commit_pull_reads.go bounds how often one commit's pull requests are read through
// (ReadThroughCommitsPulls, and ReadThroughCommitPulls for one commit). It is branch.go's twin:
// that file holds how long an answer about a repository and a commit's place on one of its
// branches is believed, this one how long an answer about the pull requests of a commit stands
// before GitHub is asked again. Its one caller is the results read (api/results_read_through.go,
// comparisonReader.readThroughHeads), which keeps a memo for the span of one request and no
// policy. The bound holds across processes:
// each process keeps the moment in its memory (CommitPullReads), and the database holds the claim
// every process shares (commit_pull_reads, ClaimAcrossProcesses). A writer's read claims a commit
// here; a read-only caller's read takes no claim and only reads the writer's, in this process's
// memory and in the database (RefusesAcrossProcesses), since it stores nothing and keeps nothing
// after its request. A claim stands on GitHub's answer only once its read has stored that answer
// (AnswerAcrossProcesses, SettleAcrossProcesses). Until then — while its read is under way, after
// GitHub failed it, or after its process stopped — a read the claim refuses reads the commit as a
// read that did not answer, never as what the stored rows alone say (HeldAcrossProcesses). A
// moment this process's memory holds is no such claim: it may be another process's moment, which a
// give-back there does not reach, so a refused read reads the claim itself, and claims the commit
// again where no claim stands (ReclaimAcrossProcesses).

// CommitPullReadStaleAfter is how long one commit's read-through stands before another read may
// run for that commit. Each read costs the commit's share of one GraphQL query, which asks about
// up to CommitPullsBatch commits of a repository (ReadThroughCommitsPulls), or one GET
// /repos/{owner}/{repo}/commits/{sha}/pulls for a commit GitHub names more than
// CommitPullsPerCommit pull requests for, and one more call per pull request GitHub names whose
// title, body, branch and links name nobody held to the repository, since that one's commit
// messages are paged too (storePullRequestFacts). The Verification tab refetches an issue's
// results on every result.recorded, so without a bound every refetch would spend those calls
// again for every result's commit — for ever, since the
// read reads a commit that a stored pull request heads too, to learn a second pull request at
// that head. Five minutes is the span every other answer about a commit is believed for
// (BranchCommitStaleAfter) and the longest the reconcile waits between two passes
// (ReconcileInterval), so a pull request opened at a recorded commit is read as that result's
// basis within it.
const CommitPullReadStaleAfter = 5 * time.Minute

// CommitPullSettledFor is how long the answer "GitHub names a pull request for this commit whose
// head is another commit" is believed before the commit may be read through again (Settle). That
// answer is worth more than a plain read-through's and less than for ever, and the hour is the
// trade between its two costs: a commit whose pull request GitHub puts elsewhere costs one read
// an hour rather than one every CommitPullReadStaleAfter, and a pull request opened later with
// its head at that commit is seen within an hour rather than never. The second cost is the one
// an unbounded settling got wrong — a commit a push left behind is a commit a later pull request
// can open at — and an hour of `other_commit` on such a result is one refetch's wait away from
// `at_basis`, where never is a restart away.
const CommitPullSettledFor = time.Hour

// CommitPullReads is the moment before which one commit of one repository is not read through
// again, held across reads so a commit costs at most one read-through per span rather than one
// per read of the results. Its zero value is an empty cache, and the server holds one for the
// process (api.server.commitPulls), as it holds one Branches: the answer is a commit's, not a
// request's, and every goroutine reading results shares it. It is the process's first gate: a
// moment it holds refuses with no statement. The bound across processes is the claim in the
// database (ClaimAcrossProcesses, SettleAcrossProcesses), which the memory only spares asking.
//
// Only the read-time read-through records here, and the one a recorded result makes
// (api/results.go's readThroughPullRequests) must not: a result recorded while no pull request
// carries its commit would leave a fresh read behind, and the read a moment after the pull
// request opened would be refused a read-through of its own and answer `other_commit` — which
// is the whole of what dispatch://INFRA-1240 repairs.
//
// One decision, one lock: Claim tests the moment held and records the next one together, so no
// caller can hold the two apart and two reads of one process that ask at once cannot both be
// granted; the claim in the database does the same for two processes, in one statement. One
// entry per repository-and-commit pair carries both bounds, which differ only in their span:
// Claim records CommitPullReadStaleAfter past the read it grants, and Settle CommitPullSettledFor
// past GitHub's answer that the pull request's head is elsewhere, which is the longer answer and
// the one that overwrites the claim that bought it.
//
// What bounds the map is that one span. A granted claim drops every entry whose moment has
// passed once the map has passed commitPullReadSweepAt, so nothing is kept for ever and a
// process that reads the results of many issues for weeks holds one entry per commit its last
// hour asked about. That sweep walks the whole map under the lock, and it runs only on a claim
// that is granting a GitHub call, so what waits for it is already waiting for a round trip.
type CommitPullReads struct {
	mu        sync.Mutex
	notBefore map[commitPullKey]time.Time
}

// commitPullReadSweepAt is the size the map is swept from. An entry whose moment has passed
// refuses nothing and is overwritten by the next claim for its pair, so it is dead weight; below
// this size the weight is not worth a walk, and above it a process that reads the results of
// many issues for weeks would otherwise hold one entry per commit it ever asked about.
const commitPullReadSweepAt = 1024

// commitPullKey is the pair one read-through is spent on: a repository, under the canonical name
// its caller resolved (refs.Renames.Canonical), and a commit.
type commitPullKey struct {
	repo string
	sha  string
}

// Claim reports whether sha's pull requests in repoFull may be read through at now — nothing is
// held for the pair, or the moment held has passed — and records the next such moment,
// CommitPullReadStaleAfter past now, when it answers yes, under the one lock that read the
// bound.
//
// A read that fails is claimed with the rest: GitHub was asked, and a repository whose every ask
// fails must not be asked again on every read. One read of one issue's results asks about a
// commit once whatever this answers (api/results_comparison.go, comparisonReader's own memo).
func (c *CommitPullReads) Claim(repoFull, sha string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := commitPullKey{repo: repoFull, sha: sha}
	if c.refusesLocked(key, now) {
		return false
	}
	if c.notBefore == nil {
		c.notBefore = map[commitPullKey]time.Time{}
	} else if len(c.notBefore) >= commitPullReadSweepAt {
		for swept, until := range c.notBefore {
			if !now.Before(until) {
				delete(c.notBefore, swept)
			}
		}
	}
	c.notBefore[key] = now.Add(CommitPullReadStaleAfter)
	return true
}

// Refuses reports whether Claim would refuse sha's pull requests in repoFull at now — a claim or a
// settle holds the pair past now — read under the lock Claim reads the bound under, and records
// nothing: no claim, and no sweep. It is RefusesAcrossProcesses' first gate, as Claim is
// ClaimAcrossProcesses'.
func (c *CommitPullReads) Refuses(repoFull, sha string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refusesLocked(commitPullKey{repo: repoFull, sha: sha}, now)
}

// RefusesAcrossProcesses reports whether ClaimAcrossProcesses would refuse sha's pull requests in
// repoFull at now, and records nothing: no claim in this process's memory or in the database, no
// hold and no sweep. A read-only caller's results read asks it where a writer's read claims
// (api/results_read_through.go, comparisonReader.readThroughHeads), so it asks GitHub nothing where
// a writer's read at that moment would be refused, and leaves every later read's claim as it found
// it. It asks this process's memory first (Refuses), and then reads the claim every process shares
// (commit_pull_reads): a moment held there past now refuses, as claimCommitPullReadSQL's guard
// refuses a writer's claim. A read that fails refuses too, as a writer's claim that fails grants
// nothing, and answers its error.
func (c *CommitPullReads) RefusesAcrossProcesses(ctx context.Context, pool *store.Pool, repoFull, sha string, now time.Time) (bool, error) {
	if c.Refuses(repoFull, sha, now) {
		return true, nil
	}
	var held bool
	if err := pool.QueryRow(ctx, `
		select exists (select 1 from commit_pull_reads where repo = $1 and sha = $2 and not_before > $3)
	`, repoFull, sha, now).Scan(&held); err != nil {
		return true, fmt.Errorf("read the claim of the read of the pull requests of %s at %s: %w", repoFull, sha, err)
	}
	return held, nil
}

// refusesLocked is whether the moment held for key is after now, the one test Claim and Refuses
// both read the bound by.
func (c *CommitPullReads) refusesLocked(key commitPullKey, now time.Time) bool {
	until, found := c.notBefore[key]
	return found && now.Before(until)
}

// Settle holds the asking about one repository and commit off for CommitPullSettledFor: GitHub
// has been read for the pair and named a pull request whose head is another commit, so this
// commit is behind that pull request and nothing a read within that span can learn about it
// repairs a result there (api/results_read_through.go, comparisonReader.readThroughHeads). Without it
// the pair would be claimed again every CommitPullReadStaleAfter for as long as the result is
// read, since nothing is stored at a commit no pull request heads.
//
// It carries a moment because the answer does go stale: GitHub has answered about this commit
// for now, and a pull request opened later can have its head at it, so the answer is believed
// for a span and then asked again. The moment is recorded in the same map a claim records in,
// which is what makes the settling a longer bound of the same kind rather than a second rule:
// the sweep drops it once it has passed, as it drops every other moment no claim can be refused
// by.
func (c *CommitPullReads) Settle(repoFull, sha string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.notBefore == nil {
		c.notBefore = map[commitPullKey]time.Time{}
	}
	c.notBefore[commitPullKey{repo: repoFull, sha: sha}] = now.Add(CommitPullSettledFor)
}

// hold records until as the moment before which this process does not claim repoFull and sha
// again: the moment another process holds in the database, or now, which refuses nothing.
func (c *CommitPullReads) hold(repoFull, sha string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.notBefore == nil {
		c.notBefore = map[commitPullKey]time.Time{}
	}
	c.notBefore[commitPullKey{repo: repoFull, sha: sha}] = until
}

// ClaimAcrossProcesses is Claim for every process that serves the results read. It asks this
// process's memory first (Claim), which refuses with no statement, and then the claim that every
// process shares (commit_pull_reads, claimCommitPullReadSQL). So N processes read one commit
// through at most once a CommitPullReadStaleAfter, as one process does, and each process sends
// the claim at most once a span for each commit. A claim the database refuses keeps the moment
// the database holds in this process's memory, so the process sends no claim of the pair again
// before that moment. That moment is another process's: a give-back there deletes the claim and
// cannot reach this memory, so a read this memory refuses reads the claim itself
// (HeldAcrossProcesses) and claims the pair again where none stands (ReclaimAcrossProcesses). A
// claim that fails grants nothing and leaves the pair free in this process's memory, so the next
// read asks again; the read then reads the commit as a read that did not answer. A granted claim
// then deletes the rows whose moment has passed (sweepCommitPullReadsSQL).
//
// The moments are each process's clock, as the memory's are, so two processes whose clocks
// differ by d can read one commit through twice in one span that is d shorter.
func (c *CommitPullReads) ClaimAcrossProcesses(ctx context.Context, pool *store.Pool, repoFull, sha string, now time.Time) (bool, error) {
	if !c.Claim(repoFull, sha, now) {
		return false, nil
	}
	var granted bool
	var notBefore time.Time
	err := pool.QueryRow(ctx, claimCommitPullReadSQL, repoFull, sha, now, now.Add(CommitPullReadStaleAfter)).Scan(&granted, &notBefore)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Another process inserted the pair after this statement's snapshot was taken, and its
		// moment refused this claim: it claimed the pair a moment ago.
		c.hold(repoFull, sha, now.Add(CommitPullReadStaleAfter))
		return false, nil
	case err != nil:
		c.hold(repoFull, sha, now)
		return false, fmt.Errorf("claim the read of the pull requests of %s at %s: %w", repoFull, sha, err)
	case !granted:
		c.hold(repoFull, sha, notBefore)
		return false, nil
	}
	if _, err := pool.Exec(ctx, sweepCommitPullReadsSQL, now); err != nil {
		// The claim stands: a row the sweep left refuses nothing, and the next granted claim
		// deletes it.
		slog.Warn("dispatch: delete the claims of reads of commits whose moment has passed; the next claim deletes them",
			"repo", repoFull, "commit", sha, "error", err)
	}
	return true, nil
}

// claimCommitPullReadSQL is the claim of one repository and commit that every process shares:
// $1 the repository, $2 the commit, $3 now and $4 the moment a granted claim records. The
// insert moves the moment only where the moment held has passed, so of two processes that ask
// at once one is granted: the second waits on the row the first wrote, for that one statement,
// and reads its moment. A granted claim is a read not yet made, so no answer of GitHub's stands
// behind it: it sets answered false over the mark an earlier claim's answered read left
// (AnswerAcrossProcesses). It answers one row, true and the new moment where it granted the
// claim, and false and the moment held where it refused it; no row where another process inserted
// the pair after this statement's snapshot. It locks that one row and no other.
const claimCommitPullReadSQL = `
	with granted as (
		insert into commit_pull_reads as held (repo, sha, not_before) values ($1, $2, $4)
		on conflict (repo, sha) do update set not_before = excluded.not_before, answered = false
		where held.not_before <= $3
		returning held.not_before
	)
	select true, not_before from granted
	union all
	select false, held.not_before from commit_pull_reads held
	where held.repo = $1 and held.sha = $2 and not exists (select from granted)`

// sweepCommitPullReadsSQL deletes every row whose moment has passed at $1, which refuses nothing
// (commit_pull_reads_not_before), so the table holds about one row for each commit asked about
// in the last CommitPullSettledFor. A granted claim runs it, in a statement of its own after the
// claim, and it skips each row another statement holds, so it waits on no row. A claim, a settle
// and a release each lock the one row of their own pair, so no two statements on this table wait
// on each other. When the sweep ran in the claim's own statement, two claims of two passed pairs
// each held its own row and waited on the other's, and Postgres ended one of the two as a
// deadlock (TestTwoClaimsOfTwoPassedCommitsMadeAtOnceAreBothGranted).
const sweepCommitPullReadsSQL = `
	delete from commit_pull_reads
	where (repo, sha) in (
		select repo, sha from commit_pull_reads
		where not_before < $1
		for update skip locked)`

// SettleAcrossProcesses is Settle for every process: it records CommitPullSettledFor past now in
// this process's memory and in the claim every process shares, where it keeps the later of that
// moment and the one held. A settle follows an answer of GitHub's that the read stored, so it
// marks the claim answered (AnswerAcrossProcesses). The write runs on a context its caller's end
// cannot cancel, bounded by commitPullRecordTimeout: its caller is a request, and a client that
// hangs up after GitHub answered must not cost the next read a second ask within the hour.
func (c *CommitPullReads) SettleAcrossProcesses(ctx context.Context, pool *store.Pool, repoFull, sha string, now time.Time) error {
	c.Settle(repoFull, sha, now)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitPullRecordTimeout)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		insert into commit_pull_reads as held (repo, sha, not_before, answered) values ($1, $2, $3, true)
		on conflict (repo, sha) do update set not_before = greatest(held.not_before, excluded.not_before), answered = true`,
		repoFull, sha, now.Add(CommitPullSettledFor)); err != nil {
		return fmt.Errorf("settle the read of the pull requests of %s at %s: %w", repoFull, sha, err)
	}
	return nil
}

// AnswerAcrossProcesses marks the claims ClaimAcrossProcesses granted at now for each of shas in
// repoFull as claims an answer of GitHub's stands behind: the read they were granted for has
// stored GitHub's answer about each commit. Only then does a read the claim refuses, in any
// process, read the commit from the stored rows (HeldAcrossProcesses); before it, the claim is a
// read under way, a read GitHub failed, or a read whose process stopped, and none of those says
// anything about the commit. It marks only those claims: a moment later than theirs, which a
// settle or another process's later claim recorded, is another read's. It is one statement for all
// of shas. The write runs on a context its caller's end cannot cancel, bounded by
// commitPullRecordTimeout, as the settle's does.
func (c *CommitPullReads) AnswerAcrossProcesses(ctx context.Context, pool *store.Pool, repoFull string, shas []string, now time.Time) error {
	if len(shas) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitPullRecordTimeout)
	defer cancel()
	if _, err := pool.Exec(ctx, `update commit_pull_reads set answered = true where repo = $1 and sha = any($2) and not_before <= $3`,
		repoFull, shas, now.Add(CommitPullReadStaleAfter)); err != nil {
		return fmt.Errorf("mark the reads of the pull requests of %d commits of %s answered: %w", len(shas), repoFull, err)
	}
	return nil
}

// HeldAcrossProcesses reads the claim every process shares of each of shas in repoFull that is
// held past now, and answers, for each commit that has one, whether an answer of GitHub's stands
// behind it (AnswerAcrossProcesses, SettleAcrossProcesses). A commit absent from the answer has no
// claim held past now. A results read asks it for the commits a claim refused, in this process's
// memory or in the database, so it reads:
//   - a commit whose claim is answered from the stored rows, which hold that answer;
//   - a commit whose claim is not answered as a read that did not answer, since the read behind
//     the claim is under way, failed at GitHub, or stopped with its process;
//   - and a commit with no claim as one this process's memory refused on a moment no claim holds
//     any more: a claim another process gave back, or a moment the memory holds longer than the
//     database does. It claims such a commit again (ReclaimAcrossProcesses).
//
// It records nothing, so a read-only caller's read asks it as a writer's does, and it is one
// statement for all of shas. A read of it that fails answers its error, and the caller reads each
// of shas as a read that did not answer: Dispatch could not say whether an answer stands behind
// the claim.
func (c *CommitPullReads) HeldAcrossProcesses(ctx context.Context, pool *store.Pool, repoFull string, shas []string, now time.Time) (map[string]bool, error) {
	held := map[string]bool{}
	if len(shas) == 0 {
		return held, nil
	}
	rows, err := pool.Query(ctx, `
		select sha, answered from commit_pull_reads where repo = $1 and sha = any($2) and not_before > $3
	`, repoFull, shas, now)
	if err != nil {
		return nil, fmt.Errorf("read the claims of the reads of %d commits of %s: %w", len(shas), repoFull, err)
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		var answered bool
		if err := rows.Scan(&sha, &answered); err != nil {
			return nil, fmt.Errorf("read the claims of the reads of %d commits of %s: %w", len(shas), repoFull, err)
		}
		held[sha] = answered
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the claims of the reads of %d commits of %s: %w", len(shas), repoFull, err)
	}
	return held, nil
}

// ReclaimAcrossProcesses is ClaimAcrossProcesses for a commit this process's memory refused at now
// although no claim of it is held past now in the database (HeldAcrossProcesses): the moment the
// memory holds was another process's claim, which that process gave back, or it outlasts the
// claim the database holds. It forgets that moment and claims the pair again, so the read asks
// GitHub at once, as the give-back meant. A claim another process took since is refused as any
// claim is.
func (c *CommitPullReads) ReclaimAcrossProcesses(ctx context.Context, pool *store.Pool, repoFull, sha string, now time.Time) (bool, error) {
	c.mu.Lock()
	delete(c.notBefore, commitPullKey{repo: repoFull, sha: sha})
	c.mu.Unlock()
	return c.ClaimAcrossProcesses(ctx, pool, repoFull, sha, now)
}

// ReleaseAcrossProcesses gives back the claim ClaimAcrossProcesses granted at now, in this
// process's memory and in the claim every process shares, so the next read of the pair asks
// GitHub at once. Its one caller is the results read, for a commit whose claim no answer of
// GitHub's stands behind and no failure of GitHub's either: one the read stopped before, in an
// earlier batch or earlier in its own query's answer, and one whose answer was not stored
// (ErrReadThroughNotStored), or not marked answered. The claim bounds how often GitHub is asked,
// and such a read has left nothing for the bound to protect, so holding it would leave the result
// not known for CommitPullReadStaleAfter for no saving. Another process's memory may still hold
// the claim's moment, which this cannot reach: that process reads the claim itself and finds it
// gone (HeldAcrossProcesses, ReclaimAcrossProcesses). It gives back
// only that claim: a moment later than it, which a settle or another process's later claim
// recorded, stays. The write runs on a
// context its caller's end cannot cancel, bounded by commitPullRecordTimeout, as the settle's does.
func (c *CommitPullReads) ReleaseAcrossProcesses(ctx context.Context, pool *store.Pool, repoFull, sha string, now time.Time) error {
	claimed := now.Add(CommitPullReadStaleAfter)
	key := commitPullKey{repo: repoFull, sha: sha}
	c.mu.Lock()
	if until, found := c.notBefore[key]; found && !until.After(claimed) {
		delete(c.notBefore, key)
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitPullRecordTimeout)
	defer cancel()
	if _, err := pool.Exec(ctx, `delete from commit_pull_reads where repo = $1 and sha = $2 and not_before <= $3`,
		repoFull, sha, claimed); err != nil {
		return fmt.Errorf("give back the claim of the read of the pull requests of %s at %s: %w", repoFull, sha, err)
	}
	return nil
}

// commitPullRecordTimeout bounds one write of the claim every process shares that a read-through
// makes after GitHub answered or failed (SettleAcrossProcesses, AnswerAcrossProcesses,
// ReleaseAcrossProcesses). Each is one statement, which a healthy database answers in
// milliseconds, as githubapp bounds its one write of a limit (limitRecordTimeout).
const commitPullRecordTimeout = 5 * time.Second
