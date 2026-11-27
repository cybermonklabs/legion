package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

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
// started at before any (check_state_sets.ci_read_at, which publish moves for each former head whose
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
		left join check_state_sets published on published.issue_key = $1 and published.repo = held.repo
			and published.head_sha = former.head_sha
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
