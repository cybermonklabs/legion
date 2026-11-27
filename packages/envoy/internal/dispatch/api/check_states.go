package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// check_states.go publishes the state of each `before_merge` check at each open pull request head
// of an issue, and at each former head of an open pull request, one check.state_changed event for
// each change of a state. A former head is a head a pull request left while it stays open
// (formerHeadsOf): the tester may have tested that head while the pull request moved on, and the
// testing move reads the states at the head the tester tested, so a result recorded there after
// the move still publishes its state there. Each write of pull_request_heads that can move a pull
// request's history appends a record of it, by a trigger in the writer's own transaction
// (pull_request_former_heads_moves, migration 20261126005348), so a head is a former head whether
// or not a settle ran while the pull request was there; a settle applies the records of its
// issue's open pull requests before it reads their former heads (the drain, settleRound), and each
// pass applies the rest (foldLeftRecords). A head an open pull request left before that migration
// is known where a result was recorded at it: the migration seeds those commits as candidates, and
// a result recorded since at a commit that is no head of the pull request Dispatch knows is
// appended as owed to the issue by its own trigger and left as a candidate by the issue's drain,
// each of which the settler checks against the pull request's commits (verifyFormerHeads). A head's
// states stop, and its rows go, once no open pull request is or was at it: its pull requests
// closed or merged.
//
// A write that can change a state does not publish anything itself: a row trigger on each table a
// state is computed from marks the issue owed, in the writer's own transaction
// (check_states_owe, migration 20261126005334, and the triggers after it), and notifies
// CheckStatesOwedChannel, which Postgres delivers at the writer's commit and never for a rollback.
// So no writer runs code of this file or takes a lock it did not take before: the triggers of the
// former heads only read, append and mark too, and the settler alone rewrites what they append, so
// none of them closes a cycle with the locks the writers take in their own orders. A write of the
// release before this one is marked too, and a writer that a later change adds to a watched table
// is marked without code of its own.
//
// The settler (checkStatesSettler) is woken on that channel in every process. A settle of one
// issue runs under the issue's own lease, so one settle at a time computes its states: it reads
// the marks that are due, computes the states of the issue's `before_merge` checks alone with the
// computation GET /api/v1/issues/{key}/results answers (answerIssueChecks), so it asks GitHub
// nothing about a check it publishes no state of, compares each with the state it published last
// (check_states), and in one transaction appends one event for each difference, stores what it
// published and deletes the marks it read. Changes that land between two computes count as one
// change, and the event names the state Dispatch computed then.
//
// A test handoff waits for the states its results caused before it is recorded: that fence,
// which the handoff route runs and the settler does not, is check_states_fence.go's.

// CheckStatesOwedChannel is the Postgres channel check_states_owe notifies each time it marks an
// issue's check states owed: the wake of the settler's loop (runCheckStates).
const CheckStatesOwedChannel = "dispatch_check_states_owed"

// checkStatesLeasePrefix is the start of the name of each issue's lease, which the issue key
// ends: a settle runs under its issue's lease alone, so two processes settle two issues at once
// and never one issue twice at once.
const checkStatesLeasePrefix = "check-states:"

// checkStatesSchedule is the settler's timing, a value so a test shortens it. minGap is the least
// time between the starts of two passes, the wait before a pass that skipped an issue, read a
// full batch or left a due mark tries again, and the wait of a mark a settle keeps for a former
// head it did not read (publish); sweep the longest time between two passes, so a mark
// whose notification no process received is settled within it; retry the wait after a pass that
// failed, and the first wait of a mark kept after a failed read of GitHub, which doubles with each
// failure up to retryMax; fenceWait and fencePoll the fence of a test handoff
// (awaitCheckStatesSettled); batch the most issues one pass reads; rounds the most rounds one
// settle runs.
type checkStatesSchedule struct {
	minGap, sweep, retry, retryMax, fenceWait, fencePoll time.Duration
	batch, rounds                                        int
}

// checkStatesProduction is the schedule a server runs. The fence waits 30 s, half the minute the
// dispatch command's client waits for an answer, so the client reads the fence's refusal.
var checkStatesProduction = checkStatesSchedule{
	minGap: time.Second, sweep: time.Minute, retry: 30 * time.Second, retryMax: 10 * time.Minute,
	fenceWait: 30 * time.Second, fencePoll: 100 * time.Millisecond, batch: 20, rounds: 5,
}

// checkStatesHooks hold a settle at fixed points in a test; nil in production.
type checkStatesHooks struct {
	afterMarks   func(issue string) // the due marks are read, the compute has not started
	afterCompute func(issue string) // the compute is done, the transaction has not begun
}

// checkStatesSettler settles the issues whose check states are owed.
type checkStatesSettler struct {
	server    *server
	leases    duty.Leases        // the "check states" reservation of the lease pool
	exclusive duty.ExclusiveFunc // duty.Exclusive
	schedule  checkStatesSchedule
	hooks     checkStatesHooks
}

// runCheckStates is the settler's loop (Background.RunCheckStates), which every process runs on
// its reservation of the lease pool and its wake on CheckStatesOwedChannel.
func (s *server) runCheckStates(ctx context.Context, leases duty.Leases, wake <-chan struct{}) {
	settler := &checkStatesSettler{server: s, leases: leases, exclusive: duty.Exclusive, schedule: s.checkStatesSchedule}
	settler.run(ctx, wake)
}

// run is a woken loop (duty.RunWoken) with no lease of its own, since each issue has its lease:
// one pass at start, which settles what a process that stopped left owed, one on each wake, and one
// at each due time a pass answers, at most sweep after the pass before it, which settles a mark
// whose notification no process received. The background passes run on this goroutine, under the
// issues' leases, so the loop needs one connection of the lease pool and no more.
func (c *checkStatesSettler) run(ctx context.Context, wake <-chan struct{}) {
	ctx = store.WithTransactionTracking(ctx)
	duty.RunWoken(ctx, duty.WokenLoop{Wake: wake, MinGap: c.schedule.minGap, Pass: c.pass})
}

// pass settles up to batch issues that hold a due mark, the issue of the oldest mark first, each
// under its own lease, then applies the records of up to batch pull requests no settle applied
// (foldLeftRecords), and answers when the loop next runs with no wake: minGap from now where it
// skipped an issue another process held, read a full batch, left a due mark after an issue's last
// round or applied the records of a full batch of pull requests; retry from now where a settle or
// that apply failed; else the earliest due time of a mark not due yet, and sweep from now at the
// latest.
func (c *checkStatesSettler) pass(ctx context.Context) (next time.Time) {
	issues, err := c.owedIssues(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}
		}
		slog.Warn("dispatch: read the issues whose check states are owed", "error", err)
		return time.Now().Add(c.schedule.retry)
	}
	soon := len(issues) >= c.schedule.batch
	failed := false
	for _, issue := range issues {
		ran, settleFailed, owed := c.settle(ctx, issue)
		if ctx.Err() != nil {
			return time.Time{}
		}
		failed = failed || settleFailed
		soon = soon || !ran || owed
	}
	folded, err := c.foldLeftRecords(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}
		}
		slog.Warn("dispatch: apply the records of the pull requests no settle applied", "error", err)
		failed = true
	}
	soon = soon || folded
	switch {
	case failed:
		return time.Now().Add(c.schedule.retry)
	case soon:
		return time.Now().Add(c.schedule.minGap)
	}
	wait, held, err := c.nextDue(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}
		}
		slog.Warn("dispatch: read the next due check states mark", "error", err)
		return time.Now().Add(c.schedule.retry)
	}
	if !held || wait > c.schedule.sweep {
		wait = c.schedule.sweep
	}
	return time.Now().Add(wait)
}

// owedIssues is up to batch issues that hold a due mark, the issue of the oldest mark first.
func (c *checkStatesSettler) owedIssues(ctx context.Context) ([]string, error) {
	rows, err := c.server.deps.Store.Pool.Query(ctx, `
		select issue_key from check_states_owed where due_at <= now()
		group by issue_key order by min(id) limit $1`, c.schedule.batch)
	if err != nil {
		return nil, fmt.Errorf("read the issues whose check states are owed: %w", err)
	}
	issues, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the issues whose check states are owed: %w", err)
	}
	return issues, nil
}

// foldLeftRecords applies the records of up to batch pull requests that the writes of
// pull_request_heads appended (pull_request_former_heads_moves, migration 20261126005348), the
// pull request of the oldest record first, each in a statement of its own that takes that pull
// request's lock alone (pull_request_former_heads_fold), and answers whether it took a full batch.
// A record names the row it was written under; the fold finds the pull request that name stands
// for, under every name it goes by, so records written under two names of one pull request are
// applied together. A settle applies the records of the open pull requests of its own issue
// before it reads their former heads (pull_request_former_heads_drain); this applies the rest, a
// pull request no issue holds among them, whose records would otherwise grow with each of its
// writes.
func (c *checkStatesSettler) foldLeftRecords(ctx context.Context) (bool, error) {
	pool := c.server.deps.Store.Pool
	rows, err := pool.Query(ctx, `
		select repo, number from pull_request_former_heads_moves
		group by repo, number order by min(seq) limit $1`, c.schedule.batch)
	if err != nil {
		return false, fmt.Errorf("read the pull requests whose records no settle applied: %w", err)
	}
	type pullRequest struct {
		repo   string
		number int
	}
	pulls, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pullRequest, error) {
		var pull pullRequest
		return pull, row.Scan(&pull.repo, &pull.number)
	})
	if err != nil {
		return false, fmt.Errorf("read the pull requests whose records no settle applied: %w", err)
	}
	for _, pull := range pulls {
		if _, err := pool.Exec(ctx, `select pull_request_former_heads_fold($1, $2)`, pull.repo, pull.number); err != nil {
			return false, fmt.Errorf("apply the records of %s#%d: %w", pull.repo, pull.number, err)
		}
	}
	return len(pulls) >= c.schedule.batch, nil
}

// nextDue is how long until the earliest mark that is not due yet falls due, measured on the
// database's clock as every due time is written, and false where no such mark is held.
func (c *checkStatesSettler) nextDue(ctx context.Context) (time.Duration, bool, error) {
	var seconds *float64
	if err := c.server.deps.Store.Pool.QueryRow(ctx, `
		select extract(epoch from min(due_at) - now())::float8 from check_states_owed where due_at > now()`).Scan(&seconds); err != nil {
		return 0, false, fmt.Errorf("read the next due check states mark: %w", err)
	}
	if seconds == nil {
		return 0, false, nil
	}
	return time.Duration(*seconds * float64(time.Second)), true, nil
}

// settle runs the rounds of one issue under its lease, at most rounds of them. ran is false where
// the lease was not taken: another process holds it and settles the issue, and the wake of the
// same commit earns this process another pass. failed is true where a round failed before its
// commit, which keeps every mark it read; owed is true where a due mark is left after the last
// round.
func (c *checkStatesSettler) settle(ctx context.Context, issue string) (ran, failed, owed bool) {
	ran = c.exclusive(ctx, nil, c.leases, checkStatesLeasePrefix+issue, time.Time{}, 0, func(ctx context.Context) {
		for range c.schedule.rounds {
			more, err := c.settleRound(ctx, issue)
			if err != nil {
				failed = true
				if ctx.Err() == nil {
					slog.Warn("dispatch: settle the check states of an issue", "issue", issue, "error", err)
				}
				return
			}
			if !more {
				return
			}
		}
		owed = true
	})
	return ran, failed, owed
}

// settleRound is one round of a settle: it reads the issue's due marks, computes the issue's
// states, publishes each change in one transaction that deletes the marks it read, and answers
// whether a due mark is left. A mark is drawn its id at its insert and not at its commit, so a
// mark of a lower id can commit after the round read: the round deletes the marks it read by id,
// never a range of ids, and a mark it did not read stays for the next round.
func (c *checkStatesSettler) settleRound(ctx context.Context, issue string) (more bool, err error) {
	pool := c.server.deps.Store.Pool
	marks, failures, err := dueMarksOf(ctx, pool, issue)
	if err != nil || len(marks) == 0 {
		return false, err
	}
	if c.hooks.afterMarks != nil {
		c.hooks.afterMarks(issue)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `select exists (select 1 from issues where key = $1)`, issue).Scan(&exists); err != nil {
		return false, fmt.Errorf("read whether %s exists: %w", issue, err)
	}
	var published []model.Event
	var reads formerHeadsReads
	if !exists {
		// An issue nobody has any more has no state to publish, and its marks, its pending
		// candidates and the places of its results settle nothing.
		if _, err := pool.Exec(ctx, `
			with marks as (delete from check_states_owed where id = any($1)),
				pending as (delete from pull_request_former_heads_pending where issue_key = $2)
			delete from pull_request_former_heads_results where issue_key = $2`, marks, issue); err != nil {
			return false, fmt.Errorf("delete the marks of %s, which no longer exists: %w", issue, err)
		}
	} else {
		// Each mark read committed before this compute, in the transaction of its change, so the
		// compute sees every change the marks record. The drain first applies, under each pull
		// request's lock, the records the writers' triggers appended and the candidates they owe
		// the issue (pull_request_former_heads_drain, migration 20261126005348): the writers only
		// append, so the settle writes the former heads its compute reads.
		if _, err := pool.Exec(ctx, `select pull_request_former_heads_drain($1)`, issue); err != nil {
			return false, fmt.Errorf("drain the former heads owed to %s: %w", issue, err)
		}
		heads, err := c.server.deps.Attributor.HeadsAttributedTo(ctx, pool, issue, delivery.HeadAnswers{})
		if err != nil {
			return false, err
		}
		needed, err := c.formerHeadsNeeded(ctx, issue)
		if err != nil {
			return false, err
		}
		if reads, err = c.verifyFormerHeads(ctx, issue, heads, needed); err != nil {
			return false, err
		}
		former, err := formerHeadsOf(ctx, pool, issue, heads)
		if err != nil {
			return false, err
		}
		answer, held, err := c.server.answerIssueChecks(ctx, issue, noCommitNamed, nil, formerHeadsApart(former, heads), docs.CheckWhenBeforeMerge)
		if err != nil {
			return false, fmt.Errorf("compute the checks of %s: %w", issue, err)
		}
		computed, readFailed, err := checkStatesAtHeads(issue, heads, former, answer)
		if err != nil {
			return false, err
		}
		// A candidate a failed read of GitHub left unverified, one another settle's read holds, or
		// one whose answer applied nothing, keeps the obligation that asked for the read in the
		// fence's wait (publish).
		if c.hooks.afterCompute != nil {
			c.hooks.afterCompute(issue)
		}
		var artifact *string
		if held {
			artifact = &answer.VerificationArtifactID
		}
		if published, err = c.publish(ctx, issue, marks, failures, artifact, computed, readFailed, reads); err != nil {
			return false, err
		}
		c.server.publish(ctx, published...)
	}
	slog.Info("dispatch: check states settled", "issue", issue, "marks", len(marks), "events", len(published),
		"verifications", reads.verifications, "requests", reads.requests)
	if err := pool.QueryRow(ctx, `select exists (select 1 from check_states_owed where issue_key = $1 and due_at <= now())`, issue).Scan(&more); err != nil {
		return false, fmt.Errorf("read the marks of %s left: %w", issue, err)
	}
	return more, nil
}

// dueMarksOf is the ids of issue's due marks, and of each mark a settle kept in the fence's wait
// (reads_from) however late it falls due, so any settle of the issue that resolves what such a
// mark waits for deletes it, and the most failures any of them counts.
func dueMarksOf(ctx context.Context, pool *store.Pool, issue string) ([]int64, int, error) {
	var marks []int64
	var failures int
	if err := pool.QueryRow(ctx, `
		select coalesce(array_agg(id), '{}'), coalesce(max(failures), 0)
		from check_states_owed where issue_key = $1 and (due_at <= now() or reads_from is not null)`, issue).Scan(&marks, &failures); err != nil {
		return nil, 0, fmt.Errorf("read the due marks of %s: %w", issue, err)
	}
	return marks, failures, nil
}

// formerHeadsHeld is the start of a statement over the open pull requests of an issue, given as
// the arrays repos and numbers, the statement's placeholders of their rows' names and numbers:
// held is each row with the identity of the pull request it stands for
// (pull_request_former_heads_pull_of, migration 20261126005348), null where the settler has met
// none of its names, and first is true on one row of each pull request, the row stored under its
// canonical name where it has one. A pull request's history is the identity's, whichever of its
// names its rows are stored under, so the settler reads it once, for that row.
func formerHeadsHeld(repos, numbers string) string {
	return `with held as (
		select stored.repo, stored.number, stored.pull,
			row_number() over (partition by stored.pull
				order by lower(stored.repo) = lower(pull_request_canonical_repo(stored.repo)) desc, stored.repo, stored.number) = 1 as first
		from (
			select named.repo, named.number, pull_request_former_heads_pull_of(named.repo, named.number) as pull
			from unnest(` + repos + `::text[], ` + numbers + `::integer[]) as named (repo, number)
		) stored
	)`
}

// openHeadsOf is the names and numbers of the rows of heads' open pull requests.
func openHeadsOf(heads []delivery.AttributedHead) (repos []string, numbers []int) {
	for _, head := range heads {
		if head.Open {
			repos, numbers = append(repos, head.Repo), append(numbers, head.Number)
		}
	}
	return repos, numbers
}

// formerHeadsOf is each former head of an open pull request of issue, heads being the pull
// requests delivery.HeadsAttributedTo answers: each head that pull request left while it was open
// (pull_request_former_heads, by the pull request's identity under every name it goes by, which
// the drain writes from the records each write of pull_request_heads appends in its own
// transaction), but its current head, and but a candidate that no read of the pull request's
// commits has confirmed yet (verifyFormerHeads). Each former head is the open pull request's row
// moved back to that commit, the row of its canonical name where both of its names are open at the
// issue (formerHeadsHeld): at a commit an open pull request of its repository heads now, it names
// its pull request among that open head's (checkStatesAtHeads), and else the computation reads it
// as it reads an open head (formerHeadsApart, answerIssueChecks), and an event there names the
// open pull requests that were at it. A closed or merged pull request keeps no former head, so
// publish deletes the rows of a head no open pull request is or was at, as it deletes those of an
// open head whose pull request closed.
//
// The head whose read of GitHub's check runs started longest ago comes first, a head no read
// started at before any (check_states.ci_read_at, which publish moves for each former head whose
// read started), and among those the head left last: the order deriveCIResults starts their reads
// in, so each settle reads first the heads the settle before did not reach.
func formerHeadsOf(ctx context.Context, pool *store.Pool, issue string, heads []delivery.AttributedHead) ([]delivery.AttributedHead, error) {
	repos, numbers := openHeadsOf(heads)
	if len(repos) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, formerHeadsHeld("$2", "$3")+`
		select held.repo, held.number, former.head_sha
		from held join pull_request_former_heads former on former.pull = held.pull
		left join check_states published on published.issue_key = $1 and published.repo = held.repo
			and published.head_sha = former.head_sha and published.check_block_id = ''
		where held.first and former.verified
		order by published.ci_read_at nulls first, former.left_at desc, held.repo, former.head_sha, held.number`,
		issue, repos, numbers)
	if err != nil {
		return nil, fmt.Errorf("read the former heads of %s: %w", issue, err)
	}
	defer rows.Close()
	var former []delivery.AttributedHead
	for rows.Next() {
		var repo, sha string
		var number int
		if err := rows.Scan(&repo, &number, &sha); err != nil {
			return nil, fmt.Errorf("read the former heads of %s: %w", issue, err)
		}
		at := slices.IndexFunc(heads, func(head delivery.AttributedHead) bool {
			return head.Open && head.Repo == repo && head.Number == number
		})
		if heads[at].HeadSHA == sha {
			continue
		}
		head := heads[at]
		head.HeadSHA = sha
		former = append(former, head)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the former heads of %s: %w", issue, err)
	}
	return former, nil
}

// formerHeadsNeeded is each commit a result of a `before_merge` check of issue's Verification
// document was recorded at, in either result table, whichever issue recorded it: the commits at
// which a state the settler publishes is computed from a result, and so the only candidates the
// settle reads GitHub about (verifyFormerHeads). The checks are the document's now, as the settle
// computes them (answerIssueChecks), so a check whose `when` became `before_merge` needs its
// results' commits from the settle the change of the document owes on. None where the issue has no
// Verification document or no `before_merge` check.
func (c *checkStatesSettler) formerHeadsNeeded(ctx context.Context, issue string) ([]string, error) {
	s := c.server
	pool := s.deps.Store.Pool
	artifact, held, err := s.verificationDocument(ctx, pool, issue, "")
	if err != nil || !held {
		return nil, err
	}
	checks, err := s.checkBlocksOf(ctx, pool, artifact, nil)
	if err != nil {
		return nil, err
	}
	var blocks []string
	for _, check := range checks {
		if check.When == docs.CheckWhenBeforeMerge {
			blocks = append(blocks, check.BlockID)
		}
	}
	if len(blocks) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `
		select commit_sha from check_results where check_artifact_id = $1::uuid and check_block_id = any($2)
		union
		select commit_sha from ci_check_results where check_artifact_id = $1::uuid and check_block_id = any($2)`,
		artifact.ID, blocks)
	if err != nil {
		return nil, fmt.Errorf("read the commits of the before_merge results of %s: %w", issue, err)
	}
	needed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the commits of the before_merge results of %s: %w", issue, err)
	}
	return needed, nil
}

// formerHeadsReadClaim is how long a settle holds a pull request's verification read
// (pull_request_former_heads_reads) before another may take it over: several times the bound of
// the read itself (ciReadDeadline), so it outlasts every read a live settle makes, and short, so a
// read whose process stopped is made again soon after.
const formerHeadsReadClaim = time.Minute

// formerHeadsReads is what verifyFormerHeads made of the candidates of an issue's open pull
// requests. unverified is true where a read GitHub did not answer left a candidate undecided and a
// retry can change that (not delivery.MembershipUnreadable), where GitHub's read of the pull
// request names another head or base revision than the row stores, or where the row stores no base
// revision: a read made again at once is refused the same
// way until a write stores the revisions, so all wait the failure backoff; pending where another
// settle's claim holds a pull request's read, or where an answer applied nothing because what it
// was read about moved before it was applied (a claim run out or taken over, a lifecycle ended, a
// head or a base revision stored anew, a candidate replaced), which a read made again at once can
// apply, so a later read decides those candidates. verifications counts the logical reads, one for
// each pull request whose candidates were read, and requests the requests to GitHub those reads
// sent: a read of several pages, a page asked again or a comparison is one verification and
// several requests.
type formerHeadsReads struct {
	unverified, pending     bool
	verifications, requests int
}

// verifyFormerHeads confirms the candidates (pull_request_former_heads rows whose verified is
// false, migration 20261126005348): the commits of results the settle of an open pull request's
// issue reads, recorded on any issue against a check of its Verification document, which the
// install seeded, or the drain left for a result recorded since at a commit that is no head of the
// pull request Dispatch knows, or for a row that started at the issue since, and which may or may
// not have been heads of that pull request. For each open pull request of heads holding one, it
// asks GitHub which of them are the pull request's own commits, once, and applies the answer: the
// commits the head the row stores holds and the base revision it stores does not, which GitHub's
// comparison of those two commits answers (delivery.PullRequestMembership), so the answer is about
// the two revisions Dispatch stores by construction, however GitHub moves the pull request while
// it is read. Only an answer GitHub gave about a commit decides it: a commit no read answered for,
// whether a deployment with no App key, a repository the App cannot read, a 404 that says nothing
// alone, a 5xx or a bound that ended withheld the answer, stays as it is. Where a retry can change
// the answer (a 404, a 5xx, a bound that ended, a rate limit), unverified answers true, and the
// settle keeps a mark that reads it again after the backoff, so the read comes again once GitHub
// answers. Where no retry changes it (delivery.MembershipUnreadable: no App key, no installation,
// a token GitHub refuses the repository), unverified stays false and no mark waits for the read, so
// no test handoff waits for it either: the candidates stay as they are, and the next settle of the
// issue, which any later write owes, reads them again, once access or configuration returns. Until
// a read decides a candidate it is no former head. Only a failed read of Dispatch's own database is
// an error.
//
// It reads only the candidates needed names (formerHeadsNeeded): the commits a result of a
// `before_merge` check of the issue's Verification document was recorded at, the only commits a
// state the settler publishes is computed from a result at. A candidate whose results are all of a
// check of another `when` is kept as history, unread, and leaves no obligation and nothing the
// test handoff's fence waits for, so an issue whose set of checks is empty is not held by its
// candidates; once a check that has a result there becomes a `before_merge` check, the change of
// the document owes the issue a settle, which reads it.
//
// The read is made once per pull request, whichever issue's settle makes it: an issue's lease
// serializes the settles of one issue, and a pull request moves between issues. So a settle first
// claims the pull request's read, keyed by its identity, which is the same under every issue and
// every name (pull_request_former_heads_claim), and the claim answers what the read is about: the
// claim's token, the pull request's generation, a snapshot of its open rows' heads, base branches
// and the base revisions Dispatch stores for them (pull_request_heads.base_sha, unknown where a
// row holds none), the head and the base revision of the row the read goes by, read with that
// snapshot, and each candidate with its incarnation, read after the claim, since a settle that
// held the claim before may have decided some since. A claim another settle holds leaves the
// candidates to that settle: pending answers true, and the settle keeps a mark due after the
// backoff with no failure counted, which reads them again should that settle stop before it
// decides them. A claim is taken over once formerHeadsReadClaim has passed, so a process that
// stopped mid-read holds no pull request for long. The read goes by the name of the pull
// request's row formerHeadsHeld picks, its canonical name where it has one, which is GitHub's
// route to it, and compares that row's head with its base revision. A row that stores no base
// revision, as a writer of the release before this one left it, is not read: the reconcile's read
// of the pull request stores one. The reconcile tries that read within the bound
// delivery.ListStalePullRequestHeads states: of n rows due in its first selection, the open rows
// due and the merged rows no read has reached (the merged rows a rule older than the build's
// judged take only the room those leave, at most delivery.RejudgedHeadsPerPass), each is tried
// within ceil(n/delivery.PullRequestHeadBatch) passes that try their whole batch, each stamp
// acknowledged, whatever their reads answer, one more for each batch of rows first written
// meanwhile, since a row never tried goes first; a pass a rate limit stops tries part of its
// batch, and the rows it did not try keep their places, so the next pass takes them first. Each
// pass of one process starts at most five
// minutes after the one before it ended, a row due once its last read is
// delivery.PullRequestHeadStaleAfter old: with no more than a batch due and no pass a rate limit
// stops, about nine and a half minutes after that read, plus the passes' run time. A try that
// fails, or that a stopped pass never makes, stores nothing, and the row comes again the same way;
// a try whose stamp's call times out or loses its connection may still commit the stamp with no
// read, at a moment the reconcile never learns, so the bound counts no pass with such a stamp and
// holds again only once the stamp has settled, and while such stamps recur no number of passes
// bounds the row's read; the stamp stores no base revision. Until a read stores a base revision
// the candidates stay unverified.
// A read of the pull request that finds GitHub at another head or base revision than the row
// stores decides nothing either (delivery.CommitMembership.Stale): GitHub moved it on where no
// write has stored it yet. Both are unverified, read again after the failure backoff, since a read
// made again at once is refused the same way until a write stores the revisions, and a base
// branch's move alone owes no issue a settle.
//
// Every answer, positive or negative, is applied by one statement, pull_request_former_heads_answer,
// and no other path changes a candidate: it brings back everything the claim answered, gives the
// claim back by its token, and applies only where that claim is still live, the generation and the
// snapshot are still the pull request's, so the row still stores the two revisions compared, and
// each candidate still has the incarnation read; any other answer applies nothing (pending), so a
// later settle reads the candidates again, and publish keeps the obligation that asked for the
// read. Where a candidate became a former head and the pull request now belongs to another issue
// than the one settling, it marks that issue owed, so its settle publishes there.
func (c *checkStatesSettler) verifyFormerHeads(ctx context.Context, issue string, heads []delivery.AttributedHead, needed []string) (formerHeadsReads, error) {
	var reads formerHeadsReads
	pool := c.server.deps.Store.Pool
	repos, numbers := openHeadsOf(heads)
	if len(repos) == 0 || len(needed) == 0 {
		return reads, nil
	}
	rows, err := pool.Query(ctx, formerHeadsHeld("$1", "$2")+`
		select held.pull, held.repo, held.number from held
		where held.first and exists (
			select 1 from pull_request_former_heads former
			where former.pull = held.pull and not former.verified and former.head_sha = any($3))
		order by held.pull`, repos, numbers, needed)
	if err != nil {
		return reads, fmt.Errorf("read the unverified former heads of %s: %w", issue, err)
	}
	type pullRequest struct {
		pull   int
		repo   string
		number int
	}
	pulls, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pullRequest, error) {
		var held pullRequest
		return held, row.Scan(&held.pull, &held.repo, &held.number)
	})
	if err != nil {
		return reads, fmt.Errorf("read the unverified former heads of %s: %w", issue, err)
	}
	for _, held := range pulls {
		var claim struct {
			token        int64
			generation   int
			snapshot     string
			head, base   string
			candidates   []string
			incarnations []int64
		}
		switch err := pool.QueryRow(ctx, `
			select token, generation, snapshot, head, base, candidates, incarnations from pull_request_former_heads_claim($1, $2, $3, $4)`,
			held.pull, formerHeadsReadClaim.Seconds(), held.repo, held.number).Scan(
			&claim.token, &claim.generation, &claim.snapshot, &claim.head, &claim.base, &claim.candidates, &claim.incarnations); {
		case errors.Is(err, pgx.ErrNoRows):
			reads.pending = true
			slog.Info("dispatch: check states left the candidate former heads of a pull request to the settle reading them",
				"issue", issue, "repo", held.repo, "number", held.number)
			continue
		case err != nil:
			return reads, fmt.Errorf("claim the read of %s#%d's commits: %w", held.repo, held.number, err)
		}
		incarnation := make(map[string]int64, len(claim.candidates))
		for index, sha := range claim.candidates {
			incarnation[sha] = claim.incarnations[index]
		}
		claim.candidates = slices.DeleteFunc(claim.candidates, func(sha string) bool { return !slices.Contains(needed, sha) })
		unstored := claim.head == "" || claim.base == ""
		var membership delivery.CommitMembership
		var readErr error
		var sent int64
		if len(claim.candidates) > 0 && !unstored {
			bounded, cancel := ciReadBound(ctx)
			counted, requests := githubreads.CountRequests(bounded)
			membership, readErr = delivery.PullRequestMembership(counted, c.server.deps.GitHub, held.repo, held.number, claim.head, claim.base, claim.candidates)
			cancel()
			sent = requests.Load()
			reads.verifications++
			reads.requests += int(sent)
		}
		incarnationsOf := func(shas []string) []int64 {
			read := make([]int64, len(shas))
			for index, sha := range shas {
				read[index] = incarnation[sha]
			}
			return read
		}
		var applied bool
		var kept, dropped, owedElsewhere int
		if err := pool.QueryRow(ctx, `
			select applied, kept, dropped, owed from pull_request_former_heads_answer($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			held.pull, claim.token, claim.generation, claim.snapshot,
			nonNilStrings(membership.Members), incarnationsOf(membership.Members),
			nonNilStrings(membership.Outside), incarnationsOf(membership.Outside), issue).Scan(&applied, &kept, &dropped, &owedElsewhere); err != nil {
			return reads, fmt.Errorf("verify the former heads of %s#%d: %w", held.repo, held.number, err)
		}
		if len(claim.candidates) == 0 {
			continue
		}
		attrs := []any{"issue", issue, "repo", held.repo, "number", held.number, "head", claim.head, "base", claim.base,
			"kept", kept, "dropped", dropped, "unknown", len(membership.Unknown), "owed_elsewhere", owedElsewhere,
			"complete", membership.Complete, "pages", membership.Pages, "pull_reads", membership.PullReads,
			"compares", membership.Compares, "commit_reads", membership.CommitReads, "requests", sent}
		switch {
		case !applied:
			reads.pending = true
			slog.Info("dispatch: check states applied nothing of an answer about the candidate former heads of a pull request that moved since its read",
				attrs...)
		case unstored:
			reads.unverified = true
			slog.Info("dispatch: check states left the candidate former heads of a pull request unread until Dispatch stores its head and base revision",
				attrs...)
		case membership.Stale:
			reads.unverified = true
			slog.Info("dispatch: check states left the candidate former heads of a pull request undecided: GitHub names another head or base revision than Dispatch stores",
				attrs...)
		case membership.Unreadable:
			// No retry changes the answer, so no mark waits for it: the candidates stay no former
			// heads, and a later settle of the issue reads them again once access returns.
			slog.Warn("dispatch: check states cannot read which candidate former heads are a pull request's commits, and no retry changes that: no mark waits for the read",
				append(attrs, "error", readErr)...)
		case len(membership.Unknown) > 0:
			reads.unverified = true
			slog.Warn("dispatch: read which candidate former heads are a pull request's commits", append(attrs, "error", readErr)...)
		default:
			slog.Info("dispatch: check states verified the candidate former heads of a pull request", attrs...)
		}
	}
	return reads, nil
}

// formerHeadsApart is each of former, formerHeadsOf's, at a commit of a repository no open head of
// heads names, which the computation reads as a head of its own (answerIssueChecks). A former head
// at an open head's commit is that open head, with one state, computed once: it adds its pull
// request to the pull requests that head's events name and nothing to the computation.
func formerHeadsApart(former, heads []delivery.AttributedHead) []delivery.AttributedHead {
	var apart []delivery.AttributedHead
	for _, head := range former {
		if !slices.ContainsFunc(heads, func(open delivery.AttributedHead) bool {
			return open.Open && open.Repo == head.Repo && open.HeadSHA == head.HeadSHA
		}) {
			apart = append(apart, head)
		}
	}
	return apart
}

// headStates is what the settler computed at one head of an issue, open or former: the head's
// repository and commit, the open pull requests of the issue at it or that were at it, in number
// order, the block ids of the issue's `before_merge` checks in document order, which is the head's
// set of checks, and the state of each of them there, in the same order. At a former head, kept is
// each check CI proves whose read of GitHub's check runs had not started when its batch's bound
// ended (ciCaseNotStarted), which has no state in states and keeps the one published, and whose
// head holds the marks of its settle until a later settle reads it (publish); ciRead is whether a
// read of that head's check runs started.
type headStates struct {
	repo, head   string
	pullRequests []int
	checks       []string
	states       []checkStateAt
	kept         []string
	ciRead       bool
}

// checkStateAt is the state of one check at one head: its subject there, as passStandsOf computed
// it (model.PassStandsSubject).
type checkStateAt struct {
	check, state, reason, ciCase string
	version                      int
	roles, results               []string
}

// checkStatesAtHeads is the states of issue at each of its open heads and its former heads: heads
// are the pull requests delivery.HeadsAttributedTo answers, former formerHeadsOf's, and answer is
// answerIssueChecks's, given those of former apart from the open heads (formerHeadsApart). The open
// pull requests that are or were at one commit of one repository are one head with one state,
// whose events name each of them, whichever is at it now. The state of a check at a head is its
// subject whose basis is one of that head's pull requests and its commit. readFailed says whether
// a state reads read_failed: a check CI proves whose check runs GitHub could not be read there.
//
// The heads are read apart from the computation's own read of them, so a head that moved between
// the two reads is a head the computation holds no subject of. That is an error, which keeps every
// mark: the move marked the issue owed after these marks were read, and the round after the next
// wake computes both from one read.
func checkStatesAtHeads(issue string, heads, former []delivery.AttributedHead, answer issueCheckResults) ([]headStates, bool, error) {
	type headKey struct{ repo, head string }
	grouped := map[headKey]*headStates{}
	linked := map[headKey][]string{}
	formerOnly := map[headKey]bool{}
	var order []headKey
	add := func(head delivery.AttributedHead, isFormer bool) {
		key := headKey{head.Repo, head.HeadSHA}
		group, held := grouped[key]
		if !held {
			group = &headStates{repo: head.Repo, head: head.HeadSHA}
			grouped[key] = group
			formerOnly[key] = isFormer
			order = append(order, key)
		}
		group.pullRequests = append(group.pullRequests, head.Number)
		if head.LinkedRepo != "" {
			linked[key] = append(linked[key], head.LinkedRepo)
		}
	}
	for _, head := range heads {
		if head.Open {
			add(head, false)
		}
	}
	for _, head := range former {
		add(head, true)
	}
	slices.SortFunc(order, func(a, b headKey) int {
		return strings.Compare(a.repo+"\x00"+a.head, b.repo+"\x00"+b.head)
	})
	var checks []model.CheckBlock
	for _, check := range answer.Checks {
		if check.When == docs.CheckWhenBeforeMerge {
			checks = append(checks, check)
		}
	}
	readFailed := false
	computed := make([]headStates, 0, len(order))
	for _, key := range order {
		group := grouped[key]
		slices.Sort(group.pullRequests)
		group.checks = make([]string, 0, len(checks))
		for _, check := range checks {
			subject, found := subjectAtHead(check, *group, linked[key])
			if !found {
				return nil, false, fmt.Errorf("the computation of %s holds no state of %s at %s of %s#%v: the head moved after its read",
					issue, check.BlockID, group.head, group.repo, group.pullRequests)
			}
			group.checks = append(group.checks, check.BlockID)
			if subject.CI != nil && formerOnly[key] {
				if subject.CI.Case == ciCaseNotStarted {
					group.kept = append(group.kept, check.BlockID)
					continue
				}
				group.ciRead = true
			}
			state := checkStateAt{
				check: check.BlockID, version: check.Version, state: subject.Answer, reason: subject.Reason,
				roles: nonNilStrings(subject.Roles), results: nonNilStrings(subject.Results),
			}
			if subject.CI != nil {
				state.ciCase = subject.CI.Case
			}
			readFailed = readFailed || subject.Reason == model.PassSubjectReasonReadFailed
			group.states = append(group.states, state)
		}
		computed = append(computed, *group)
	}
	return computed, readFailed, nil
}

// subjectAtHead is check's subject at head: a pull request basis at head's commit whose pull request
// is one of head's, of head's repository. A subject says its repository in one of two places. The
// read of a check CI proves at a head carries the repository of that head's row, where its check
// runs were read (CheckCI.Repo), and that read is the one part of a state that differs between two
// repositories at one commit. A basis names its repository only where it is another than the
// issue's (headBasis): the canonical name of the link that made the pull request the issue's, or
// the row's own repository, and none at all where the issue resolves to no repository. So two
// repositories with an open #70 at one commit can hold two subjects whose bases are alike, and the
// read's repository, then the basis's, is what tells them apart. A subject that names no
// repository either way carries no read of a check CI proves: its state is the state of its commit,
// from the results that count there whatever repository recorded them, the same at every head at
// that commit, so it serves head where no subject names head's repository.
func subjectAtHead(check model.CheckBlock, head headStates, linked []string) (model.PassStandsSubject, bool) {
	var unnamed *model.PassStandsSubject
	for index := range check.PassStands.Subjects {
		subject := &check.PassStands.Subjects[index]
		basis := subject.Basis
		if basis.Kind != model.CheckBasisPullRequest || basis.Merged || basis.Commit != head.head ||
			basis.PullRequest == nil || !slices.Contains(head.pullRequests, *basis.PullRequest) {
			continue
		}
		repo := basis.Repo
		if subject.CI != nil && subject.CI.Repo != "" {
			repo = subject.CI.Repo
		}
		switch {
		case repo == "":
			if unnamed == nil {
				unnamed = subject
			}
		case strings.EqualFold(repo, head.repo) || slices.ContainsFunc(linked, func(link string) bool { return strings.EqualFold(repo, link) }):
			return *subject, true
		}
	}
	if unnamed != nil {
		return *unnamed, true
	}
	return model.PassStandsSubject{}, false
}

// checkStateKey is one row of check_states: a check at a head of a repository, "" for the head's
// set row.
type checkStateKey struct {
	repo, head, check string
}

// publishedCheckState is one row of check_states as the settler compares it.
type publishedCheckState struct {
	state  *string
	reason string
	roles  []string
	ciCase string
	checks []string
}

// sameState reports whether the published row holds the state computed: a change of the state,
// its reason, its roles or its CI case is a change, and a change of its results or its check's
// version alone is none.
func (p publishedCheckState) sameState(state checkStateAt) bool {
	return p.state != nil && *p.state == state.state && p.reason == state.reason && p.ciCase == state.ciCase &&
		sameStringSet(p.roles, state.roles)
}

// publish appends one check.state_changed event for each state that differs from the one published
// at that head, and the set form for each head whose set of checks moved with no check's event to
// carry it, in one transaction under the issue's owner lock: it raises the issue's serial by one
// for each event, stores what each event published, deletes the rows of heads neither open nor
// former (checkStatesAtHeads computed no state there) and of checks no longer in a head's set,
// with no event, and settles the marks the round read. The row of a check a former head kept
// (headStates.kept) stays as published, and each former head whose read of GitHub's check runs
// started takes the time of this settle (check_states.ci_read_at).
//
// A mark is settled, and deleted, once each former head has a read of its check runs that started
// after the mark's change committed: a read in this settle, which read the mark after that commit,
// or in a settle at or after the one that first kept the mark (check_states_owed.reads_from). A
// head this settle did not read may have none, and then the state published there may be from
// before that change: the mark stays, due a minimum gap on, and the fence of a test handoff
// (awaitCheckStatesSettled) waits for it. The next settle reads those heads before the heads read
// in this one, so each of N former heads is read within ceil(N/ciHeadReadConcurrency) settles, and
// the mark goes then. A mark kept so carries the failure of a state that reads read_failed. With
// no mark kept, a state of the issue that reads read_failed once this settle's writes are done
// leaves one new mark, due later as retryAfter answers and with no notification, which the fence
// does not wait for, since read_failed is what that state publishes: a state this settle computed
// from a read that failed, and as much one a settle before kept at a head this settle did not
// read (headStates.kept), so the retry a failed read owes stays until that head is read again or
// leaves the issue's heads, whatever the last settle read. A failure is counted only for a read
// that failed in this settle (readFailed, reads.unverified).
//
// A membership verifyFormerHeads left unresolved is another matter, since no state says so: a
// candidate a read GitHub did not answer left undecided where a retry can change that
// (reads.unverified), or one another settle's claim holds or whose answer applied nothing
// (reads.pending), is no former head, so nothing published replaces the state a reader holds at
// that commit. Such a settle keeps every mark it read, each an obligation that asked for the read,
// with reads_from, so the fence waits for each whatever its backoff, due after retryAfter for a
// failed read and a minimum gap otherwise; the settle that resolves the membership deletes them.
// It keeps them all, not one for the rest: a handoff waits for the marks it read when it began,
// and a mark of a lower id can commit after that, so a mark kept in the others' place may be one no
// handoff waits for, and the marks it waits for would go while the read is unresolved. A read no
// retry answers (delivery.MembershipUnreadable: no App key, no installation, a token GitHub refuses
// the repository) keeps no mark, so a test handoff waits for it no longer than the fence's own wait,
// whatever GitHub answers: a candidate of such a pull request stays no former head, and a state
// published at that commit before stays until a settle reads it. Only the settler writes
// check_states, under the issue's lease, so the rows it reads here are the newest published. It
// answers the events, which the caller publishes once the transaction has committed.
func (c *checkStatesSettler) publish(
	ctx context.Context, issue string, marks []int64, failures int, artifact *string, computed []headStates, readFailed bool,
	reads formerHeadsReads,
) ([]model.Event, error) {
	s := c.server
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	owner := issueOwner(issue)
	if err := s.deps.Events.LockOwners(ctx, tx, owner.event(model.CheckStateChangedEventType, model.CheckStatesActor, nil)); err != nil {
		return nil, err
	}
	published, err := publishedCheckStates(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	var serial int64
	if err := tx.QueryRow(ctx, `select coalesce((select serial from check_state_serials where issue_key = $1), 0)`, issue).Scan(&serial); err != nil {
		return nil, fmt.Errorf("read the check states serial of %s: %w", issue, err)
	}
	var events []model.Event
	appendState := func(payload model.CheckStateChangedEventPayload) (int64, error) {
		serial++
		payload.Serial = serial
		event, err := s.appendEvent(ctx, tx, owner.event(model.CheckStateChangedEventType, model.CheckStatesActor, payload))
		if err != nil {
			return 0, err
		}
		events = append(events, event)
		return int64(event.Seq), nil
	}
	kept := map[checkStateKey]bool{}
	var readRepos, readHeads, unreadRepos, unreadHeads []string
	for _, head := range computed {
		if len(head.kept) > 0 {
			unreadRepos, unreadHeads = append(unreadRepos, head.repo), append(unreadHeads, head.head)
		}
		for _, check := range head.kept {
			kept[checkStateKey{head.repo, head.head, check}] = true
		}
		if head.ciRead {
			readRepos, readHeads = append(readRepos, head.repo), append(readHeads, head.head)
		}
		var lastSeq int64
		for _, state := range head.states {
			key := checkStateKey{head.repo, head.head, state.check}
			kept[key] = true
			if row, held := published[key]; held && row.sameState(state) {
				continue
			}
			check, version, answer := state.check, state.version, state.state
			seq, err := appendState(model.CheckStateChangedEventPayload{
				Issue: issue, CheckArtifactID: artifact, Repo: head.repo, Head: head.head,
				PullRequests: head.pullRequests, Checks: head.checks, Check: &check, CheckVersion: &version,
				State: &answer, Reason: state.reason, Roles: state.roles, Results: state.results, CICase: state.ciCase,
			})
			if err != nil {
				return nil, err
			}
			lastSeq = seq
			if err := upsertCheckState(ctx, tx, issue, key, artifact, &version, &answer, state.reason, state.roles, state.ciCase, head, seq); err != nil {
				return nil, err
			}
		}
		key := checkStateKey{head.repo, head.head, ""}
		kept[key] = true
		if row, held := published[key]; held && sameStringSet(row.checks, head.checks) {
			continue
		}
		// The head's set of checks moved, or the head is new. A check's event at this head carries
		// the new set; with none, the set form tells it, so a reader can tell a head with no check
		// from a head no event has reached yet.
		if lastSeq == 0 {
			if lastSeq, err = appendState(model.CheckStateChangedEventPayload{
				Issue: issue, CheckArtifactID: artifact, Repo: head.repo, Head: head.head,
				PullRequests: head.pullRequests, Checks: head.checks,
			}); err != nil {
				return nil, err
			}
		}
		if err := upsertCheckState(ctx, tx, issue, key, artifact, nil, nil, "", nil, "", head, lastSeq); err != nil {
			return nil, err
		}
	}
	for key := range published {
		if kept[key] {
			continue
		}
		if _, err := tx.Exec(ctx, `delete from check_states where issue_key = $1 and repo = $2 and head_sha = $3 and check_block_id = $4`,
			issue, key.repo, key.head, key.check); err != nil {
			return nil, fmt.Errorf("delete the published state of %s at %s of %s: %w", key.check, key.head, issue, err)
		}
	}
	if len(readRepos) > 0 {
		// Each former head whose read of GitHub's check runs started goes behind every head whose
		// read started earlier or never, in the order formerHeadsOf gives the next settle.
		if _, err := tx.Exec(ctx, `
			update check_states set ci_read_at = now()
			where issue_key = $1 and check_block_id = '' and (repo, head_sha) in (select * from unnest($2::text[], $3::text[]))`,
			issue, readRepos, readHeads); err != nil {
			return nil, fmt.Errorf("store when %s's former heads were read: %w", issue, err)
		}
	}
	if len(events) > 0 {
		if _, err := tx.Exec(ctx, `
			insert into check_state_serials (issue_key, serial) values ($1, $2)
			on conflict (issue_key) do update set serial = excluded.serial`, issue, serial); err != nil {
			return nil, fmt.Errorf("store the check states serial of %s: %w", issue, err)
		}
	}
	// The marks a former head this settle did not read still holds: that head's last read started
	// before the mark's change committed, or never. Empty, not nil, so the delete below compares
	// with an empty array rather than with null.
	unsettled := []int64{}
	if len(unreadRepos) > 0 {
		if err := tx.QueryRow(ctx, `
			select coalesce(array_agg(owed.id), '{}') from check_states_owed owed
			where owed.id = any($2) and exists (
				select 1 from check_states published
				join unnest($3::text[], $4::text[]) as unread (repo, head_sha)
					on unread.repo = published.repo and unread.head_sha = published.head_sha
				where published.issue_key = $1 and published.check_block_id = ''
					and (published.ci_read_at is null or published.ci_read_at < coalesce(owed.reads_from, now())))`,
			issue, marks, unreadRepos, unreadHeads).Scan(&unsettled); err != nil {
			return nil, fmt.Errorf("read the marks of %s a head this settle did not read holds: %w", issue, err)
		}
	}
	// A membership left unresolved (reads.unverified, reads.pending) keeps every mark the round
	// read: a candidate no answer decided is no former head yet, so nothing published replaces a
	// state a reader holds at that commit, a pass among them, and only the fence's wait keeps that
	// state from deciding. A handoff waits for the marks it read when it began, and a mark of a
	// lower id can commit after that, so no one of the marks stands for the others: each stays
	// until a settle resolves the read.
	held := unsettled
	if reads.unverified || reads.pending {
		held = marks
	}
	if _, err := tx.Exec(ctx, `delete from check_states_owed where id = any($1) and not id = any($2)`, marks, held); err != nil {
		return nil, fmt.Errorf("delete the settled marks of %s: %w", issue, err)
	}
	// Each row of a head neither open nor former is deleted above, so a row read here that reads
	// read_failed is at a head the issue still holds, whose read has not succeeded since it failed.
	var unresolved bool
	if err := tx.QueryRow(ctx, `
		select exists (select 1 from check_states where issue_key = $1 and reason = $2)`,
		issue, model.PassSubjectReasonReadFailed).Scan(&unresolved); err != nil {
		return nil, fmt.Errorf("read whether a state of %s still reads %s: %w", issue, model.PassSubjectReasonReadFailed, err)
	}
	counted := failures
	if readFailed || reads.unverified {
		counted++
	}
	switch {
	case len(held) > 0:
		// The backoff only spaces the reads: reads_from keeps each mark in the fence's wait however
		// late it falls due, and any settle of the issue reads it again (dueMarksOf). A head this
		// settle did not read is read again after a minimum gap; a read of a pull request's commits
		// that failed alone, after the backoff.
		wait := c.schedule.minGap
		if len(unsettled) == 0 && reads.unverified {
			wait = c.retryAfter(failures)
		}
		if _, err := tx.Exec(ctx, `
			update check_states_owed set reads_from = coalesce(reads_from, now()),
				due_at = now() + make_interval(secs => $2), failures = $3
			where id = any($1)`, held, wait.Seconds(), counted); err != nil {
			return nil, fmt.Errorf("keep the marks of %s a read this settle did not finish holds: %w", issue, err)
		}
	case readFailed || unresolved:
		if _, err := tx.Exec(ctx, `
			insert into check_states_owed (issue_key, due_at, failures) values ($1, now() + make_interval(secs => $2), $3)`,
			issue, c.retryAfter(failures).Seconds(), counted); err != nil {
			return nil, fmt.Errorf("keep a mark of %s to read GitHub again: %w", issue, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return events, nil
}

// retryAfter is how long a mark kept after a failed read waits: retry, doubled for each failure
// the marks it replaces counted, at most retryMax.
func (c *checkStatesSettler) retryAfter(failures int) time.Duration {
	wait := c.schedule.retry
	for range failures {
		if wait >= c.schedule.retryMax {
			break
		}
		wait *= 2
	}
	return min(wait, c.schedule.retryMax)
}

// publishedCheckStates is every row of check_states of issue, read under its owner lock.
func publishedCheckStates(ctx context.Context, tx pgx.Tx, issue string) (map[checkStateKey]publishedCheckState, error) {
	rows, err := tx.Query(ctx, `
		select repo, head_sha, check_block_id, state, reason, roles, ci_case, checks
		from check_states where issue_key = $1`, issue)
	if err != nil {
		return nil, fmt.Errorf("read the published check states of %s: %w", issue, err)
	}
	defer rows.Close()
	published := map[checkStateKey]publishedCheckState{}
	for rows.Next() {
		var key checkStateKey
		var row publishedCheckState
		if err := rows.Scan(&key.repo, &key.head, &key.check, &row.state, &row.reason, &row.roles, &row.ciCase, &row.checks); err != nil {
			return nil, fmt.Errorf("read the published check states of %s: %w", issue, err)
		}
		published[key] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the published check states of %s: %w", issue, err)
	}
	return published, nil
}

// upsertCheckState stores what one event published at key: a check's state, or the set row,
// whose version and state are null.
func upsertCheckState(
	ctx context.Context, tx pgx.Tx, issue string, key checkStateKey, artifact *string, version *int, state *string,
	reason string, roles []string, ciCase string, head headStates, seq int64,
) error {
	if _, err := tx.Exec(ctx, `
		insert into check_states (issue_key, repo, head_sha, check_block_id, check_artifact_id, check_version, state,
			reason, roles, ci_case, checks, pull_requests, event_seq)
		values ($1, $2, $3, $4, $5::uuid, $6, $7, $8, $9, $10, $11, $12, $13)
		on conflict (issue_key, repo, head_sha, check_block_id) do update set
			check_artifact_id = excluded.check_artifact_id, check_version = excluded.check_version,
			state = excluded.state, reason = excluded.reason, roles = excluded.roles, ci_case = excluded.ci_case,
			checks = excluded.checks, pull_requests = excluded.pull_requests, event_seq = excluded.event_seq`,
		issue, key.repo, key.head, key.check, artifact, version, state, reason, nonNilStrings(roles), ciCase,
		nonNilStrings(head.checks), head.pullRequests, seq); err != nil {
		return fmt.Errorf("store the published state of %s at %s of %s: %w", key.check, key.head, issue, err)
	}
	return nil
}

// sameStringSet reports whether a and b hold the same strings, in any order.
func sameStringSet(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}
