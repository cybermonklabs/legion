package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logging"
)

// A pull request's head is what a recorded check result is measured against: a `before_merge`
// result's commit is compared with the head of an open pull request, or the final head of a
// merged one, attributed to the check's own issue. pull_request_heads holds that head for
// every pull request Dispatch has seen an event for, in any repository and by any author — not
// the merged population delivery_pull_requests keeps — and the issue Dispatch's own attribution
// picks for it (attribution.go): the first issue its ranked sources name that resolves to the
// repository the pull request is in, and only where the head branch is in that same repository,
// never a fork's (headAttribution). A child's pull request is attributed to the child, so it
// answers for the child's own checks alone and never for a check of an issue above it.
//
// Three writers keep it, and each writes what GitHub itself answers about the pull request: the
// `pull_request` envelope's handler (intake.go, every action), which takes from the envelope only
// which pull request changed and reads that pull request from GitHub (recordPullRequestHead); the
// read-through made for a commit that is no head Dispatch knows (ReadThroughCommitPulls) — by a
// recorded result, and by the read of an issue's results for a `before_merge` result's commit, at
// most once every CommitPullReadStaleAfter — and the reconcile's five-minute pass (reconcile.go),
// which reads the pull requests owed a read (pull_request_reads_owed.go), every failure before any
// refusal, creating each one's row where none exists, and then takes every open row read longer
// than five minutes ago and every merged row the rule this build judges by has not judged yet —
// one offer per rule version (AttributionRuleVersion, ListStalePullRequestHeads).
//
// A row a read of GitHub wrote is marked (github_read_at), and only a marked row's time stops a
// read of GitHub or wins against GitHub's answer (headReadCovers, upsertHeadRow): a release
// before this one wrote rows from an envelope's fields, and it writes them still
// while it serves beside this build.
//
// A write that moves a row from one issue to another, or gives an unattributed row an issue, or
// takes one away, appends AttributionEventType on the issue the row left and on the issue it
// reached, in the transaction that writes the row (upsertHeadRow, UpsertPullRequestHead): what
// an issue's `before_merge` results are measured against is the heads attributed to it, so no
// issue's measurement changes without a record of it on its own timeline.

// PullRequestHead is one pull request's current head, as pull_request_heads stores it, and the
// one shape every writer of that row fills in (UpsertPullRequestHead).
type PullRequestHead struct {
	Repo              string
	Number            int
	HeadSHA           string
	HeadRef           string
	BaseRef           string
	State             string
	IssueKey          *string
	AttributionSource string
	// AttributionComplete is whether IssueKey and AttributionSource come from a read of every
	// source this pull request has, which is what decides whether the write may replace the
	// attribution the row already holds. Only attributeHead can answer it, so the zero value is
	// the careful one: a writer that does not set it deletes nobody's issue.
	AttributionComplete bool
	Title               string
	URL                 string
	EventAt             *time.Time
	CheckedAt           *time.Time
	// GitHubReadAt is the reader's clock as the read of GitHub whose answer HeadSHA, State and
	// EventAt are began, taken before its request went out, with EventAt GitHub's updated_at; nil
	// where they are no answer of GitHub's, and only headRow sets it, so the zero value is the
	// careful one. A row a write of that kind stores is marked (github_read_at), and only a marked
	// row's time stops a read of GitHub or wins against GitHub's answer (headReadCovers,
	// upsertHeadRow). It orders nothing: GitHubReadOrder does. A release before this one wrote a
	// row's head, state and updated_at from a `pull_request` envelope, whose fields a caller sets,
	// and it can still write one while it serves beside this build.
	GitHubReadAt *time.Time
	// GitHubReadOrder is the place that read took in the order of every read of GitHub (startRead),
	// set where GitHubReadAt is: of two answers of one updated_at second, the one whose read took
	// the later place is the newer (the trigger pull_request_heads_older_read, as 20261117172917
	// replaces it).
	GitHubReadOrder *int64
	// LinkMark is the row's link_mark as the attribution of this write read it, before it read
	// the links (attributeHead, linkMarkOf): how many writes of a link to this pull request had
	// marked the row then (MarkHeadsLinked). A write whose LinkMark is not the row's link_mark
	// judged the issue from links a later write changed, so it leaves the row unread
	// (upsertHeadRow). Nil where the row had no mark or no row was stored.
	LinkMark *int64
	// MergeCommitSHA is the commit GitHub made when this pull request merged. It is set only
	// for a merged pull request, and only by a writer that read it; one that did not read it
	// leaves it empty, which asks for nothing. It is written to no column of
	// pull_request_heads: it exists to seed branch_commits with the one fact the merge itself
	// proves, that the base branch held this commit (UpsertPullRequestHead).
	MergeCommitSHA string
}

// PullRequestHeadState is the state GitHub's answer about a pull request gives its row: merged
// where GitHub says it merged, closed where GitHub's `state` is closed, and open otherwise. It
// reads GitHub's `state`, never an event's action: GitHub sends `labeled`, `edited`, `assigned`
// and other actions for a closed pull request too, and a row stored open from one of those would
// make the head of a pull request that will never merge a basis again.
func PullRequestHeadState(state string, merged bool) string {
	if merged {
		return "merged"
	}
	if state != "closed" {
		return "open"
	}
	return "closed"
}

// ErrHeadNotStored marks a failure that left a pull request's head row unwritten and that
// making the same attempt again may get past. The `pull_request` envelope's handler tests for it
// (Intake.handlePullRequestEnvelope), and Intake.deliver then records that the envelope's pull
// request is owed a read (owePullRequestRead), which the reconcile makes, and acknowledges the
// envelope: the reconcile's passes read the rows that exist, so a row no write ever made is
// offered to nothing else. Where that record cannot be written either, the envelope is handed
// back for redelivery instead, since both of the statements behind the write are `on conflict …
// do update` and a redelivery writes the same pair.
//
// It marks every failure on the way to that write that a later attempt may get past, because
// they all end the same way: a read of Postgres (the repository's canonical name, the stored row
// recordPullRequestHead compares the event with, and the attribution's read of the issues its
// candidates name), a read of GitHub that failed (a 5xx, a network failure, a rate limit), and a
// GitHub answer that holds a value no head row holds (errNoRowHolds). GitHub's refusals are not
// marked: a pull request it does not know and a repository the App cannot read write no row, are
// owed a read the reconcile makes, and are acknowledged (recordPullRequestHead).
var ErrHeadNotStored = errors.New("the pull request's head row was not stored")

// markHeadNotStored marks err as ErrHeadNotStored, keeping err itself inside the mark.
func markHeadNotStored(err error) error {
	return fmt.Errorf("%w: %w", ErrHeadNotStored, err)
}

// errNoRowHolds marks a pull request's head row that holds a value no row of pull_request_heads,
// or of what the same write makes, holds (checkHeadRow). It is ErrHeadNotStored: no read of GitHub
// that gives such a value is a read that succeeded, so the pull request stays owed a read, at the
// intake as at the reconcile, and a later read whose answer the row holds writes the head: no age
// of a failure and no count of tries ends that. It is made before
// any statement, so it is never a failure of the database read as the values' own.
var errNoRowHolds = fmt.Errorf("%w: a value no row holds", ErrHeadNotStored)

// headRefBound is the most bytes a value of a head row that an index holds may have, other than
// the repository: the head commit (pull_request_heads_issue_head, pull_request_heads_repo_head)
// and the base branch and the merge commit, which a merge writes into branch_commits' primary key
// (repo, branch, sha). It is so much that no commit GitHub names (40 or 64 hexadecimal digits) and
// no branch GitHub keeps comes near it, and so little that the widest of those entries, with a
// repository of 140 bytes and a branch and a commit of 1,024 each, stays under the btree's 2,704
// bytes (SQLSTATE 54000 past it). No index holds the head branch, the title or the URL, so none of
// them is bounded.
const headRefBound = 1024

// checkHeadRow is nil where every value of head is one the write of the row takes, and names the
// first that is not otherwise: the one check of a head row, made where every writer's row is
// written (UpsertPullRequestHead), so that no caller writes a row of a value it did not check and
// no failure of a statement is read as the values' own. The repository and the number key the row
// and the read its triggers owe (pull_request_heads_older_read, pull_request_heads_unread_write),
// so they must be a pull request GitHub can have (checkPullRequestKey, errNoPullRequestNamed): the
// repository is a canonical name, which repo_projects gives and no envelope does. Every text value
// must be UTF-8 holding no NUL, which no text or jsonb value of Postgres holds (SQLSTATE 22021 and
// 22P05; the attribution's event carries the URL in jsonb), the state one the column's check takes,
// and each value an index holds at most headRefBound bytes. Each of those is errNoRowHolds.
func checkHeadRow(head PullRequestHead) error {
	if err := checkPullRequestKey(head.Repo, head.Number); err != nil {
		return err
	}
	switch head.State {
	case "open", "closed", "merged":
	default:
		return fmt.Errorf("%w: the state of %s#%d is %.40q, not open, closed or merged", errNoRowHolds, head.Repo, head.Number, head.State)
	}
	issue := ""
	if head.IssueKey != nil {
		issue = *head.IssueKey
	}
	for _, value := range []struct {
		name, text string
		bounded    bool
	}{
		{"head commit", head.HeadSHA, true}, {"head branch", head.HeadRef, false},
		{"base branch", head.BaseRef, true}, {"merge commit", head.MergeCommitSHA, true},
		{"title", head.Title, false}, {"URL", head.URL, false},
		{"issue", issue, false}, {"attribution source", head.AttributionSource, false},
	} {
		switch {
		case !utf8.ValidString(value.text) || strings.ContainsRune(value.text, 0):
			return fmt.Errorf("%w: the %s of %s#%d is not UTF-8 without NUL bytes", errNoRowHolds, value.name, head.Repo, head.Number)
		case value.bounded && len(value.text) > headRefBound:
			return fmt.Errorf("%w: the %s of %s#%d is %d bytes, over %d", errNoRowHolds, value.name, head.Repo, head.Number, len(value.text), headRefBound)
		}
	}
	return nil
}

// UpsertPullRequestHead writes one pull request's head row. An event older than the one the row
// already holds changes nothing: GitHub redelivers and the reconcile reads concurrently, so the
// row keeps the newest `updated_at` either has seen rather than whichever write landed last. Of
// two answers of one `updated_at` second, which is all GitHub's time tells apart, the row keeps
// the one whose read of GitHub began later (GitHubReadAt, upsertHeadRow), and a pair that differs
// owes a read. Every writer of this build carries GitHub's own `updated_at` for the pull request,
// so a write with no event time is one whose GitHub answer carried none; it always applies, as
// does one landing on a row that has none.
//
// That rule trusts only a stored time a read of GitHub wrote (GitHubReadAt, the column
// github_read_at). GitHub's answer applies over a row that holds no such mark, whatever time the
// row holds: a release before this one stored an envelope's updated_at, which a
// caller sets, and a time an hour ahead would otherwise refuse every answer of GitHub's until
// GitHub's own clock passed it — for a closed or merged pull request, never. That release still
// writes rows while it serves beside this build, and its statement names no github_read_at, so a
// write of its that changes a fact of a marked row takes the mark away (the trigger
// pull_request_heads_unread_write, migration 20261116163242).
//
// Only a write that read every attribution source may replace the row's issue
// (AttributionComplete); an incomplete one leaves issue_key and attribution_source exactly as
// they stand. Two writes are incomplete: one whose payload named no head repository, which
// cannot tell a branch of this repository from a fork's and so attributes nobody, and one whose
// commit-message read failed with anything but a rate limit (attributeHead). Either would
// otherwise delete an attribution an earlier, fuller read made — on a merged row for good,
// since the reconcile's merged arm takes a merged row only until the current rule has judged it
// (ListStalePullRequestHeads) and a complete write judges it, so that issue's results at its
// own merged head would stand at no basis for ever. An incomplete write never carries an issue of
// its own, so keeping the stored pair costs it nothing. A write that did read every source and
// found nobody is complete, and clears: GitHub's answer is the whole truth about the row.
//
// A write carrying no read time leaves the row's checked_at as it was: nulling it would hand a
// row the reconcile has just read back to the next batch (ListStalePullRequestHeads orders the
// never-read first), which is why an incomplete write is held off the attribution rather than
// sent round again. attribution_rule is kept by the same test, and it is the other half of the
// merged arm's bound: a write that did not read GitHub leaves the stored rule alone, so a
// merged row an incomplete envelope wrote is still offered to the current rule, while a refresh
// that did read GitHub ends that offer whether or not it could attribute the row. The rule
// written is AttributionRuleVersion, bound here and carried in no field of PullRequestHead, so
// every write stamps the rule this build judges by and no writer can leave a row claiming a
// rule that never judged it. An insert writes it outright: a row this build has just made was
// judged by this build's rule. A test that needs a row of an older rule sets attribution_rule
// itself, with one update.
//
// A merged pull request's merge commit is recorded as on its base branch, for good, as soon as
// this row is written: GitHub completing the merge is itself the proof that the branch held
// that commit, and branch_commits keeps a yes for good (branch.go). Nothing can prove it
// later. In a repository that deletes a branch when its pull request merges and merges by
// squash, by the time an `after_merge` result of an issue that merged into an integration
// branch is measured — once the integration branch has itself landed on the default branch and
// been deleted — no compare read can show that commit on the base branch, because the branch is
// gone, and the default branch holds only a squash of it. Without the row written here such a
// result could never stand at its base branch. Only a merged row writes one: GitHub answers a
// test-merge commit for an open pull request too, and that commit is on no branch at all.
//
// The row is written whether or not the `where` guard above applied this write's facts. The
// guard is about which event's facts the row keeps, not about whether the merge happened, and
// GitHub's merge proves the fact whatever order the events arrive in. The time recorded is
// time.Now() rather than a clock threaded through every caller of this function, which would
// buy nothing: a yes never goes stale, so checked_at only records when the fact was learned
// and no reader measures a yes against it.
//
// The head row and that seed are written in one transaction, so the two land together or
// neither does and the caller is told. branch.go logs its own failed writes and carries on,
// which is right for every answer it writes: those are answers a compare read can earn again a
// moment later. This one can never be earned again — the paragraph above is the whole argument
// that nothing proves it once the base branch is gone — so a seed lost behind a Warn line
// leaves every `after_merge` result of that issue standing at no basis for good, with the
// merged head row stored beside it as if all was well. Failing the write instead puts that in
// front of the caller: the reconcile logs it and leaves the row for its next pass
// (reconcilePullRequestHeads), and the results read's read-through hands it back to the route
// (ReadThroughCommitPulls). Both statements are `on conflict … do update`, so a write made
// again writes the same pair and costs nothing.
//
// A write with nothing to pair and no event to append — an open pull request an incomplete read
// wrote, a merge whose base branch or merge commit nobody read — runs its one statement on the
// pool as it always did. A transaction around a single statement buys nothing and costs a round
// trip at each end, and every caller of this function makes far more writes of that shape than
// of the other.
//
// broker is what the attribution's own events are appended through, and it may be nil: a nil
// broker writes the row and appends nothing, so a caller that holds none hands over nothing
// else. The two writers that hold one hand it over — the `pull_request` envelope the intake's
// (recordPullRequestHead) and the reconcile's refresh the reconcile's (RefreshPullRequestHead)
// — so every move either of them makes is recorded, and so does the results read's own
// read-through (api/results.go, api/results_comparison.go). A test is the caller that passes nil.
//
// The events are appended only where this write may replace the row's issue (AttributionComplete
// above), and only where it did: the row's issue is read under its own lock before the write, and
// the two are compared after it. An incomplete write cannot move an attribution, a write the
// event-time guard refused changed no row, and a write that stores the issue the row already
// held moved nothing — none of the three appends anything.
//
// Every error this function answers is ErrHeadNotStored: nothing of the row, the seed and the
// events was written, and the caller may write all three again. A value no row holds
// (errNoRowHolds, checkHeadRow) is one of them, refused before any statement: a later read of
// GitHub may answer values the row holds. The one exception is errNoPullRequestNamed, a key that
// is no pull request GitHub can have, also refused before any statement: nothing was written
// either, and no attempt with the same key gets past it.
func UpsertPullRequestHead(ctx context.Context, pool *store.Pool, broker *events.Broker, head PullRequestHead) error {
	if err := checkHeadRow(head); err != nil {
		return err
	}
	seedsBranch := head.State == "merged" && head.BaseRef != "" && head.MergeCommitSHA != ""
	records := broker != nil && head.AttributionComplete
	if !seedsBranch && !records {
		_, err := upsertHeadRow(ctx, pool, head)
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: open the write of the head of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
	}
	// The rollback runs on a context of its own, not the caller's: the caller's is cancelled
	// exactly when the rollback matters — a webhook consumer shutting down mid-write, a results
	// read whose browser went away — and pgx answers a rollback on a cancelled context by
	// destroying the connection instead of returning it to the pool, which leaves a backend on
	// the database for as long as the process lives (api/duplicates.go makes the same argument
	// for its own read). A commit that already ran absorbs this one (store.Pool's trackedTx).
	defer func() {
		rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), headWriteRollback)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	var left *string
	if records {
		if left, err = headIssueBefore(ctx, tx, broker, head); err != nil {
			return err
		}
	}
	applied, err := upsertHeadRow(ctx, tx, head)
	if err != nil {
		return err
	}
	var moved []model.Event
	if records && applied && !sameIssue(left, head.IssueKey) {
		if moved, err = appendAttributionChange(ctx, tx, broker, head, left); err != nil {
			return fmt.Errorf("%w: %w", ErrHeadNotStored, err)
		}
	}
	if seedsBranch {
		if err := writeBranchCommit(ctx, tx, head.Repo, head.BaseRef, head.MergeCommitSHA, true, time.Now()); err != nil {
			return fmt.Errorf("%w: %w", ErrHeadNotStored, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: store the head of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
	}
	for _, event := range moved {
		broker.Publish(event)
	}
	return nil
}

// AttributionEventType is the event a head write appends when a pull request's issue changes:
// one on the issue the pull request left and one on the issue it reached, each carrying the
// same payload, so either issue's timeline shows the whole move. A pull request that reaches an
// issue from nobody appends one event, on the issue it reached, and one that is uncredited
// appends one on the issue it left.
const AttributionEventType = "issue.pull_request_attribution_changed"

// AttributionEventActor writes every issue.pull_request_attribution_changed: a system actor,
// since no person or session asked for the move — a link someone added, a branch GitHub
// reported, or this build's own ranking of the sources decided it. Broker.Notify is false for a
// system actor, which keeps the event out of inboxes and routed delivery; the outbox publishes
// it on the issue's subject as it does every unpublished event, and the event feed and the
// event log carry it.
var AttributionEventActor = model.Actor{Kind: "system", ID: "delivery-attribution"}

// IssuePullRequestAttributionEventPayload is an issue.pull_request_attribution_changed event's
// payload: the pull request (its repository, number and URL), the issue it left and the issue it
// reached — either is null, never both — and the attribution source that decided the new issue
// (attributionSource, "none" where the pull request is now credited to nobody).
type IssuePullRequestAttributionEventPayload struct {
	Repo   string  `json:"repo"`
	Number int     `json:"number"`
	URL    string  `json:"url"`
	From   *string `json:"from"`
	To     *string `json:"to"`
	Source string  `json:"source"`
}

// appendAttributionChange appends the move's event on each issue it concerns, in tx, and answers
// the appended events for the caller to publish once tx commits.
//
// One AppendAll of both events rather than an Append each: a transaction that appends for two
// owners must take both owner rows in one order or two such transactions moving a pull request
// in opposite directions would wait on each other's row (events.Broker.LockOwners), and
// AppendAll takes them in its own sorted order.
//
// Neither issue can be missing by the time this runs: pull_request_heads.issue_key references
// issues on delete set null, so the row's own lock, taken by headIssueBefore before the write,
// holds off the update that a delete of the issue it left would make, and the write's own
// foreign-key check refuses the row outright where the issue it reaches has gone.
func appendAttributionChange(
	ctx context.Context, tx pgx.Tx, broker *events.Broker, head PullRequestHead, left *string,
) ([]model.Event, error) {
	payload := IssuePullRequestAttributionEventPayload{
		Repo: head.Repo, Number: head.Number, URL: head.URL,
		From: left, To: head.IssueKey, Source: head.AttributionSource,
	}
	var moved []model.Event
	for _, issue := range []*string{left, head.IssueKey} {
		if issue == nil {
			continue
		}
		key := *issue
		moved = append(moved, model.Event{
			IssueKey: &key, Type: AttributionEventType, Actor: AttributionEventActor, Payload: payload,
		})
	}
	appended, err := broker.AppendAll(ctx, tx, moved)
	if err != nil {
		return nil, fmt.Errorf("record the attribution of %s#%d: %w", head.Repo, head.Number, err)
	}
	return appended, nil
}

// headIssueBefore is the issue the stored row credits the pull request to, read for no key
// update so that no other writer moves it between this read and the write that follows: the
// "from" of the move, and nil where no row is stored yet or the row credits nobody.
//
// It takes the issue rows a move of the row appends its events to (appendAttributionChange)
// before the row itself, in the broker's order (events.Broker.LockOwners): a write of an issue's
// links holds the issue's row and then marks the head rows of the pull requests it links or
// unlinks (api's patchIssue, MarkHeadsLinked), so a head write that took the head row first and
// the issue's row for its event after waited on that write while it waited on the head write, and
// PostgreSQL ended one of the two (SQLSTATE 40P01). The row's issue is read first with no lock,
// the issues of a move from it to head's issue are locked, and the row is read again under its
// lock. Where another writer moved the row in between, that attempt's locks are let go (a
// savepoint) and the next attempt takes the issues of the row as it stands; a row moved at every
// attempt is a head not stored, which the caller owes or reads again. A write that moves nothing
// locks no issue row.
func headIssueBefore(ctx context.Context, tx pgx.Tx, broker *events.Broker, head PullRequestHead) (*string, error) {
	read := func(q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}, lock string) (*string, error) {
		var issue *string
		err := q.QueryRow(ctx, `select issue_key from pull_request_heads where repo = $1 and number = $2 `+lock,
			head.Repo, head.Number).Scan(&issue)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: read the issue of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
		}
		return issue, nil
	}
	for range headIssueLockAttempts {
		seen, err := read(tx, "")
		if err != nil {
			return nil, err
		}
		attempt, err := tx.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: lock the issues of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
		}
		if !sameIssue(seen, head.IssueKey) {
			var owners []model.Event
			for _, issue := range []*string{seen, head.IssueKey} {
				if issue != nil {
					owners = append(owners, model.Event{IssueKey: issue})
				}
			}
			if err := broker.LockOwners(ctx, attempt, owners...); err != nil {
				_ = attempt.Rollback(ctx)
				return nil, fmt.Errorf("%w: lock the issues of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
			}
		}
		locked, err := read(attempt, "for no key update")
		if err != nil {
			_ = attempt.Rollback(ctx)
			return nil, err
		}
		if sameIssue(locked, seen) {
			if err := attempt.Commit(ctx); err != nil {
				return nil, fmt.Errorf("%w: lock the issues of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
			}
			return locked, nil
		}
		if err := attempt.Rollback(ctx); err != nil {
			return nil, fmt.Errorf("%w: lock the issues of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
		}
	}
	return nil, fmt.Errorf("%w: the issue of %s#%d changed at each of %d reads while its write took its locks",
		ErrHeadNotStored, head.Repo, head.Number, headIssueLockAttempts)
}

// headIssueLockAttempts bounds how many times headIssueBefore reads the row's issue and locks
// it: the first, and one more for a row another writer moved in between.
const headIssueLockAttempts = 2

// sameIssue reports whether two attributions name one issue, with nil "credited to nobody": the
// test that decides whether a write moved the row at all.
func sameIssue(left, right *string) bool {
	switch {
	case left == nil && right == nil:
		return true
	case left == nil || right == nil:
		return false
	default:
		return *left == *right
	}
}

// headWriteRollback bounds the rollback of that transaction, which runs on a context of its
// own and so is bounded by nothing else. Two seconds is longer than the one statement each
// rollback here can be undoing and far inside any caller's own patience.
const headWriteRollback = 2 * time.Second

// upsertHeadRow is the head row's own statement, which UpsertPullRequestHead's doc comment
// explains in full: it is here, on a refs.Execer — the pool, or a transaction already open on
// it — so that one statement runs on the pool where it is the whole write and inside the
// transaction where the merge's branch answer, the attribution's own events, or both go with
// it.
//
// It answers whether the write applied: false where the event-time guard below, or the order of
// two reads of one second (below), left the row exactly as it was, which are the two ways this
// statement writes nothing and reports no error. Only an applied write can have moved the row's
// attribution, so that is what UpsertPullRequestHead tests before it appends an event.
//
// github_read_at is the reader's clock as the read of GitHub whose answer the write carries began
// (PullRequestHead.GitHubReadAt), and null for a write that is no answer of GitHub's: a mark, and
// the time a reader sees. Every read of this build begins at a moment of its own, so a write of
// this build changes it, and the trigger that tells a write of the release before from this
// build's own reads a value that did not change as a write that named none. Two reads of one row
// that begin in one microsecond are the one exception, and it errs the safe way: the row loses its
// mark and is read again. github_read_order is that read's place in the order of every read of
// GitHub (PullRequestHead.GitHubReadOrder, startRead), null on a row the release before inserted.
// A write of the release before over a row a read of this build wrote clears github_read_at and
// leaves github_read_order (pull_request_heads_unread_write), which that release names nowhere.
//
// A write of GitHub's answer wins over a row no read of GitHub wrote, whatever the two times
// (the guard's last arm): a stored time a caller set must never refuse GitHub. The one answer it
// does not win with is one of the row's own second over a row the release before wrote over,
// whose read took an earlier place than the one the row keeps (below); that write owed a read,
// which writes GitHub's head at the first reconcile pass that reaches it. A row no read of GitHub
// wrote may also hold GitHub's newer answer, which the release before stored from an envelope
// while it served beside this build, and a slower read of this build can land an older answer
// after it. So where the write replaces such a row and the row's time is later than the answer's,
// the trigger pull_request_heads_older_read records, inside this statement, that the pull request
// is owed a read of kind failure: the reconcile's next pass reads it before any refusal and
// whatever its state, a closed or merged row included, and writes GitHub's answer then, which is
// newer than both.
//
// GitHub's updated_at is whole seconds, so the order two answers of one second arrive in says
// nothing of which is the newer: a read that began first and was answered slowly would land its
// older head over the newer one (acceptance of cybermonklabs/legion#4209 at 3a21edb2c9, D2). The
// order the reads took their places in does: an answer of the stored second whose read took an
// earlier place than the stored row's read replaces nothing, and the same trigger (as
// 20261117172917 replaces it) skips that update, which every writer's head goes through, the
// intake's, the reconcile's and a result's read-through alike. The place is a value of one
// sequence, and not a clock of the process that read, whose clock may be behind another's or step
// back (the deep audit of cybermonklabs/legion#4209 at ac562e9837, finding 2). No order decides
// it for certain: a read that took the later place can still be answered with the older state,
// where its request reached GitHub first, or where its first request failed and a later try was
// answered (readGitHubPage). So two answers of one
// second that differ in a fact GitHub answers (the head, its branch, the base, the state, the
// title or the URL; not the attribution, which is this build's judgement of them) owe a read
// whichever of them the row keeps, and the next read writes what GitHub holds then. The rule and
// the owe are the trigger's, and not this statement's `where`, because a row the `where` refuses
// fires no trigger, so nothing could owe the read of the answer it refused; and the trigger
// compares with the row the update replaces (OLD), which nothing else in the statement sees: a
// CTE reads the statement's snapshot, and a CTE's `for update` skips a row the statement's own
// update has changed.
//
// A write leaves the row unread (checked_at null) where the row's link_mark is not the one its
// attribution read before it read the links (PullRequestHead.LinkMark): a write of a link to
// this pull request committed after that read, so the issue the write carries was judged from
// links that no longer stand, and the first reconcile pass that reaches the row judges it again
// (MarkHeadsLinked). The mark is a count on the row the write lands on, compared in the statement
// that holds that row's lock, so where the row is stored before the link is written no order of
// the two commits loses it. An insert compares nothing: a link written while the pull request's
// first row is written, or while its row moves from a repository's old name to the canonical one
// (upsertHeadUnderCanonicalName), marks no row, and the insert stamps the row read with the issue
// judged from the links before. The tasks of the attribution rule, which judge a pull request's
// issue from its stored facts at each read, replace the mark.
func upsertHeadRow(ctx context.Context, db refs.Execer, head PullRequestHead) (bool, error) {
	tag, err := db.Exec(ctx, `
		insert into pull_request_heads (
			repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source,
			title, url, event_at, checked_at, attribution_rule, github_read_at, github_read_order
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $14, $15, $16)
		on conflict (repo, number) do update set
			head_sha = excluded.head_sha,
			head_ref = excluded.head_ref,
			base_ref = excluded.base_ref,
			state = excluded.state,
			issue_key = case when $13::boolean
				then excluded.issue_key else pull_request_heads.issue_key end,
			attribution_source = case when $13::boolean
				then excluded.attribution_source else pull_request_heads.attribution_source end,
			title = excluded.title,
			url = excluded.url,
			event_at = coalesce(excluded.event_at, pull_request_heads.event_at),
			checked_at = case when pull_request_heads.link_mark is distinct from $17::bigint then null
				else coalesce(excluded.checked_at, pull_request_heads.checked_at) end,
			attribution_rule = case when excluded.checked_at is null
				then pull_request_heads.attribution_rule else excluded.attribution_rule end,
			github_read_at = excluded.github_read_at,
			github_read_order = excluded.github_read_order
		where excluded.event_at is null
		   or pull_request_heads.event_at is null
		   or excluded.event_at >= pull_request_heads.event_at
		   or (excluded.github_read_at is not null and pull_request_heads.github_read_at is null)
	`,
		head.Repo, head.Number, head.HeadSHA, head.HeadRef, head.BaseRef, head.State,
		head.IssueKey, head.AttributionSource, head.Title, head.URL, head.EventAt, head.CheckedAt,
		head.AttributionComplete, AttributionRuleVersion, head.GitHubReadAt, head.GitHubReadOrder,
		head.LinkMark,
	)
	if err != nil {
		return false, fmt.Errorf("%w: store the head of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
	}
	return tag.RowsAffected() > 0, nil
}

// upsertHeadUnderCanonicalName writes head, which every caller has already keyed by the
// canonical name of its repository (refs.CanonicalRepo), and drops the row the same pull
// request is stored under as storedUnder when that is another name. A row keyed under a
// repository's old name can be read at all only under the canonical one — the App answers 404
// for the old name's installation, so nothing can refresh it and no issue is ever held to it
// (attributionHeld) — so one pull request keeps one row and one attribution.
//
// Where both rows exist, because the pull request arrived under both names, the canonical row
// wins and the old row goes: the write above lands on the canonical row and its own event-time
// rule decides which facts stand there (UpsertPullRequestHead keeps the newest event either has
// seen), and the old row's facts are dropped with it rather than merged, since they are the same
// pull request read at some earlier moment.
//
// The write comes before the delete, so a failure between them leaves two rows rather than
// none: the next pass is handed the old row's key again and re-keys it again, while losing the
// row would leave the pull request unknown until another event arrived.
func upsertHeadUnderCanonicalName(
	ctx context.Context, pool *store.Pool, broker *events.Broker, head PullRequestHead, storedUnder string,
) error {
	if err := UpsertPullRequestHead(ctx, pool, broker, head); err != nil {
		return err
	}
	if strings.EqualFold(storedUnder, head.Repo) {
		return nil
	}
	if _, err := pool.Exec(ctx, `
		delete from pull_request_heads where repo = $1 and number = $2
	`, storedUnder, head.Number); err != nil {
		return fmt.Errorf("drop the head of %s#%d, now %s#%d: %w", storedUnder, head.Number, head.Repo, head.Number, err)
	}
	return nil
}

// HeadAttributedTo reports whether sha is the head of a pull request attributed to issueKey that
// is open or merged: the test a `before_merge` result's commit passes to stand at one of the
// heads its check is compared with. A closed-unmerged pull request's head proves nothing. A
// pull request attributed to another issue never answers for this one; the results read asks
// this about the check's own issue, so a child's pull request never answers for a check above
// it (api/results_comparison.go's comparisonReader). The issue is the statement's
// (attributedTerms), and only a pull request with a results row has a head to answer.
func HeadAttributedTo(ctx context.Context, pool *store.Pool, issueKey, sha string) (bool, error) {
	statements, err := attribution()
	if err != nil {
		return false, err
	}
	var found bool
	if err := pool.QueryRow(ctx, statements.headAttributedTo, issueKey, sha).Scan(&found); err != nil {
		return false, fmt.Errorf("read the pull request heads of %s at %s: %w", issueKey, sha, err)
	}
	return found, nil
}

const headAttributedToSelect = `
	select exists (
		select 1 from attributed a join heads h on h.j_repo = a.repo and h.j_number = a.number
		where a.issue_key = $1 and h.head_sha = $2 and h.state in ('open', 'merged')
	)`

// AttributedHead is one pull request Dispatch holds for an issue, as the results read names it
// among the heads a `before_merge` check compares its results with: the pull request's number,
// the commit its head is, and the two times Dispatch may have learned that head by. CheckedAt
// is when Dispatch last read the pull request from GitHub and EventAt is the pull request's
// own update time on GitHub, as the read that wrote the head answered it; each is nil where no
// writer has carried it. The reader shows the later of the two and names which it is
// (model.BasisLearnedReadAt, model.BasisLearnedUpdatedAt). HeadKnown is false for a pull request
// Dispatch holds no results row of, only a timeline row: it has no head, HeadSHA is "" and both
// times are nil.
type AttributedHead struct {
	Number    int
	HeadSHA   string
	HeadKnown bool
	CheckedAt *time.Time
	EventAt   *time.Time
}

// HeadsAttributedTo is every open or merged pull request attributed to issueKey, newest learned
// first, and then every pull request of the issue whose head Dispatch has not read, by number
// descending: the whole list HeadAttributedTo answers one commit of, which the results read names
// as what a `before_merge` check's results are compared with (R3). The issue is the statement's
// (attributedTerms), so the two reads keep one rule, and a commit HeadAttributedTo answers yes for
// is a commit this list holds.
//
// The order is by the later of the two times either way, with a row carrying neither last and
// the pull request's own number breaking a tie, so the list does not turn on whichever row the
// planner returned first. A reader taking the first head takes the one Dispatch learned last.
func HeadsAttributedTo(ctx context.Context, pool *store.Pool, issueKey string) ([]AttributedHead, error) {
	statements, err := attribution()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, statements.headsAttributedTo, issueKey)
	if err != nil {
		return nil, fmt.Errorf("read the pull request heads of %s: %w", issueKey, err)
	}
	defer rows.Close()
	var heads []AttributedHead
	for rows.Next() {
		var head AttributedHead
		if err := rows.Scan(&head.Number, &head.HeadSHA, &head.HeadKnown, &head.CheckedAt, &head.EventAt); err != nil {
			return nil, fmt.Errorf("read the pull request heads of %s: %w", issueKey, err)
		}
		heads = append(heads, head)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the pull request heads of %s: %w", issueKey, err)
	}
	return heads, nil
}

// headsAttributedToSelect is HeadsAttributedTo's select over the statement's terms by issue: a
// left join with heads, so a pull request with a timeline row only is listed with no head.
const headsAttributedToSelect = `
	select a.number, coalesce(h.head_sha, ''), h.repo is not null, h.checked_at, h.event_at
	from attributed a left join heads h on h.j_repo = a.repo and h.j_number = a.number
	where a.issue_key = $1 and (h.repo is null or h.state in ('open', 'merged'))
	order by h.repo is null, greatest(h.checked_at, h.event_at) desc nulls last, a.number desc`

// NewestHead is the one pull request head the results read derives a check CI proves at: the
// repository's canonical name, the pull request's number and its head commit.
type NewestHead struct {
	Repo    string
	Number  int
	HeadSHA string
}

// NewestPullRequestHead is the head of the open pull request of the highest number attributed
// to issueKey, or, where none is open, the final head of its merged pull request of the highest
// number, and false where Dispatch holds neither. GitHub's numbers rise, so the highest is the
// newest. Both are heads HeadsAttributedTo lists, so a result at the commit answered here stands
// at one of the heads a `before_merge` check compares its results with. A closed-unmerged pull
// request is no head, and a pull request with no results row has none. The repository is the
// canonical name (refs.CanonicalRepoSQL), which GitHub answers the head's check runs under
// (api/results_ci.go), whatever name the row is stored under.
func NewestPullRequestHead(ctx context.Context, pool *store.Pool, issueKey string) (NewestHead, bool, error) {
	statements, err := attribution()
	if err != nil {
		return NewestHead{}, false, err
	}
	var head NewestHead
	err = pool.QueryRow(ctx, statements.newestPullRequestHead, issueKey).Scan(&head.Repo, &head.Number, &head.HeadSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return NewestHead{}, false, nil
	}
	if err != nil {
		return NewestHead{}, false, fmt.Errorf("read the newest pull request head of %s: %w", issueKey, err)
	}
	return head, true, nil
}

const newestPullRequestHeadSelect = `
	select a.repo, a.number, h.head_sha
	from attributed a join heads h on h.j_repo = a.repo and h.j_number = a.number
	where a.issue_key = $1 and h.state in ('open', 'merged')
	order by h.state = 'open' desc, a.number desc
	limit 1`

// OpenHeadAt reports whether sha is the head of an open pull request stored for repoFull,
// whatever issue the row is attributed to. The results read asks it before it reads GitHub for a
// commit (api/results_comparison.go, comparisonReader.readThroughHead), since a read from there
// repairs nothing an open row needs.
//
// An open row is the reconcile's: the first pass that reaches it once PullRequestHeadStaleAfter
// has passed since its last read reads it again (ListStalePullRequestHeads) and attributes it with
// Dispatch's links, issues, parents and mappings as they stand then. It is left to that pass
// whatever issue it credits because a child's result on a parent's check heads the child's open
// row, credited to the child, and a read of GitHub for it would be made once a
// CommitPullReadStaleAfter for each such commit, for as long as the parent's results are read.
//
// A merged row is no such answer. Nothing but its next event, a raised AttributionRuleVersion and
// a write of a link to its own URL (MarkHeadsLinked) reads it again, so a tie made after its last
// read (a link to a GitHub issue the pull request's body cites, a new parent, a changed repository
// mapping, a link moved to another issue) is followed only by a read, and the results read makes
// it, once a CommitPullReadStaleAfter for each commit, as it did for every such commit while no
// row was stored for it. A merged row attributed to the check's own issue is a basis of the
// check, and a result at a basis is not read through (comparisonReader.learn).
//
// A closed-unmerged row is counted for nothing, as HeadAttributedTo counts it for nothing: it
// is no basis a result stands at, the reconcile keeps no such row in its passes
// (ListStalePullRequestHeads takes the open and the merged), and a pull request closed at a
// commit does not stop another being opened from the same head. Counting one would leave every
// result at that commit standing at no basis for good.
//
// repoFull is the canonical name of the repository (refs.CanonicalRepo), compared without case,
// as every other comparison of a repository here is (refs.CanonicalRepoSQL): a row written from
// a `pull_request` envelope carries GitHub's own spelling of the repository (`Acme/Widgets`),
// while the results read is passed the lowercased spelling the repository-to-project mapping
// stores (api's canonicalRepo), so an exact comparison would answer "no row" for a repository
// whose owner writes their name with capitals. The read is one index lookup on
// pull_request_heads_repo_head (migration 20261105112616), which is built on
// `(lower(repo), head_sha)` for that reason: pull_request_heads_issue_head is partial on
// `issue_key is not null` and keyed on the issue, so it serves no lookup by repository and head
// commit.
func OpenHeadAt(ctx context.Context, pool *store.Pool, repoFull, sha string) (bool, error) {
	var found bool
	if err := pool.QueryRow(ctx, `
		select exists (
			select 1 from pull_request_heads
			where lower(repo) = lower($1) and head_sha = $2 and state = 'open'
		)
	`, repoFull, sha).Scan(&found); err != nil {
		return false, fmt.Errorf("read the stored pull request heads of %s at %s: %w", repoFull, sha, err)
	}
	return found, nil
}

// BaseBranchMergedFor is the branch the merged pull request attributed to issueKey merged
// into, and "" where Dispatch holds no merge of it: the branch an `after_merge` result of that
// issue is measured against (api/results_comparison.go). An issue whose pull request merged
// into an integration branch has reached that branch and no other, so a result of it at a
// commit that branch's history carries stands at that branch. The repository's default branch
// answers nothing about the work until the integration branch itself lands. The merged row
// last updated answers: event_at is the pull request's updated_at as GitHub answered the read
// that wrote the row, which is the time of the merge only until a later change of that pull
// request (an edit, a label) moves it. The rows are ordered by event_at descending with the rows
// carrying none last, since a row with no update time says less than any row that has one, and
// by the pull request's own number descending where two share a time, so the answer does not
// turn on whichever row the planner returned first.
//
// The read is keyed by the issue and not by the result's commit because Legion merges with
// squash: the commit that lands on the base branch is the one the squash makes, which is no
// pull request's head and so no row's head_sha, and HeadAttributedTo — which asks whether a
// commit is such a head — can never answer for it. Keying by the issue costs nothing in
// honesty, since a pull request goes to an issue only where its head is a branch of the
// repository that issue resolves to: by the statement (own_head, attributionHeld), or, for a pull
// request with no facts, by the writer that stored its row (headAttribution). So the branch this
// answers is one the issue's own work merged into.
//
// An empty answer is "no branch to measure against", which a merged row carrying no base_ref
// gives as much as no merged row at all. The column is declared not null with the empty string
// as its default, and a `pull_request` envelope published before Dispatch carried the field
// writes exactly that, so such a row names no base branch; the empty answer leaves the caller
// on its own fallback, the repository's default branch, where handing that value on as a
// branch would have it build a GitHub compare read against a branch with no name. A timeline row
// stores no base branch, so a pull request with no results row names none either.
func BaseBranchMergedFor(ctx context.Context, pool *store.Pool, issueKey string) (string, error) {
	statements, err := attribution()
	if err != nil {
		return "", err
	}
	var branch string
	err = pool.QueryRow(ctx, statements.baseBranchMergedFor, issueKey).Scan(&branch)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the merged pull request base branch of %s: %w", issueKey, err)
	}
	return branch, nil
}

const baseBranchMergedForSelect = `
	select h.base_ref
	from attributed a join heads h on h.j_repo = a.repo and h.j_number = a.number
	where a.issue_key = $1 and h.state = 'merged'
	order by h.event_at desc nulls last, a.number desc
	limit 1`

// PullRequestHeadStaleAfter is how long an open pull request's row may go unread before the
// reconcile reads it again, and PullRequestHeadBatch how many it reads in one pass.
const (
	PullRequestHeadStaleAfter = 5 * time.Minute
	PullRequestHeadBatch      = 200
)

// ListStalePullRequestHeads is at most limit rows for the reconcile to read again, the longest
// unread first: every open row whose checked_at is older than PullRequestHeadStaleAfter (a row
// never read first), every merged row that no read has reached since it was written or marked
// (checked_at null: credited to nobody, written by a read that could not read every source, or
// marked by a write of a link to it, MarkHeadsLinked), and every merged row a rule older than this
// build's judged (AttributionRuleVersion).
//
// A merged row is revisited at all because nothing else ever revisits one. An open row comes
// round every five minutes; a merged row is written once more at most, by an envelope or a
// read-through, and a recorded result reads through only for a commit no head Dispatch knows.
// So a merged row left credited to nobody stays that way, and every `before_merge` result at
// that head stands at no basis for ever: a merge delivered by a `pull_request` envelope naming no
// head repository is one such row (recordPullRequestHead attributes nobody, since it cannot
// tell this repository's branch from a fork's), and so is every pull request merged under an
// older attribution rule that credited nobody — a title naming another repository's issue first
// was the whole of INFRA-1240.
//
// The rule half of the arm takes a row an older rule credited too, which is R8 of INFRA-1522:
// rule 2 ranks the head branch and the two links above the title and the body, so a child's
// pull request that rule 1 credited to the parent its title names belongs to the child, and
// only a read that judges the row again moves it. Under rule 1 that half was held to the rows
// credited to nobody, since nothing but the repository test could then pass a candidate over
// and a credited row would have kept its issue; a change of the ranking is exactly the change
// that can move one, so the rows already credited are read again with the rest.
//
// The bound is one offer per rule version, and the read ends the offer whatever it concluded.
// Only a reader of GitHub stamps attribution_rule and checked_at — the reconcile's own refresh
// (RefreshPullRequestHead), MarkPullRequestHeadChecked for a row it could not read at all, the
// read-through, and the intake only when its read of an envelope's pull request was complete
// (UpsertPullRequestHead keeps both where the write carries no read time). So a merged row is
// taken once per rule: the refresh stamps it at AttributionRuleVersion whether or not GitHub's
// answer attributed it, and it comes back only when a later build raises that constant, which is
// how a change to the attribution reaches the merged rows already stored. The `checked_at is
// null` half of the uncredited term keeps that bound: without it every merged row credited to
// nobody would come back every pass for ever.
//
// The arm is written as `checked_at is null or attribution_rule < $3` over the merged rows,
// rather than one condition, because the two terms are what the two partial indexes hold
// (pull_request_heads_merged_unread, as 20261117172917 builds it, and
// pull_request_heads_merged_rule, 20261111060005): written this way the planner takes a bitmap of
// each and ors them. The first term takes a merged row credited to an issue too since
// 20261117172917, since a link written to its pull request after its last read leaves the row
// unread (MarkHeadsLinked), and the issue the row credits is what that link can change.
//
// The rule half is read in a pass of its own, bounded by RejudgedHeadsPerPass, and the rest of
// limit goes to the rows a reader meets now: the open rows, whose head decides a live result,
// and the merged rows no read ever reached. Raising the rule offers every merged row of the
// deployment once, which on a production-shaped table measured for INFRA-1522 is 12,000 rows,
// about 12,000 GitHub reads and 2,400 an hour against an installation's 5,000 — half a
// deployment's budget for five hours, for a backfill nothing live waits on. At 50 a pass it is
// about 600 an hour and about a day, and a table of a few hundred merged rows is still a few
// passes. The two reads are ordered the same way and the bound is applied to their sum, so a
// pass never reads more rows than it did before.
func ListStalePullRequestHeads(ctx context.Context, pool *store.Pool, limit int, now time.Time) ([]PullRequestHead, error) {
	heads, err := listStaleHeads(ctx, pool, `
		select repo, number from pull_request_heads
		where (state = 'open' and (checked_at is null or checked_at < $1))
		   or (state = 'merged' and checked_at is null)
		order by checked_at nulls first, repo, number
		limit $2
	`, now.Add(-PullRequestHeadStaleAfter), limit)
	if err != nil {
		return nil, err
	}
	rejudge := min(RejudgedHeadsPerPass, limit-len(heads))
	if rejudge <= 0 {
		return heads, nil
	}
	rejudged, err := listStaleHeads(ctx, pool, `
		select repo, number from pull_request_heads
		where state = 'merged' and attribution_rule < $1
		order by checked_at nulls first, repo, number
		limit $2
	`, AttributionRuleVersion, rejudge)
	if err != nil {
		return nil, err
	}
	return append(heads, rejudged...), nil
}

// MarkHeadsLinked marks each stored head row of the pull requests whose URL is one of urls
// unread (checked_at null) and counts the mark on the row (link_mark), in tx, the transaction of
// the write that added or removed those links of an issue (issue_external_links). A link to a pull
// request's own URL is the first source of its attribution (attribution.go), and it changes with
// no event of the pull request. A merged row is read again only while it is unread or a newer rule
// has not judged it (ListStalePullRequestHeads), so without the mark a link written once the pull
// request merged left the row with the issue the merge's read gave it for as long as the row
// stood. The first reconcile pass that reaches a marked row reads it from GitHub, the unread rows
// first by (repository, number), PullRequestHeadBatch rows a pass, and attributes it with the
// links as they stand then, where GitHub answers each read of that attribution: a pass whose read
// of the commit messages fails stamps the row read with the issue it held (RefreshPullRequestHead),
// and the link is not followed.
//
// The count is what a head write in flight compares: its attribution read link_mark before it
// read the links (linkMarkOf), and its upsert leaves the row unread where the count has moved
// since (upsertHeadRow), so where the row is stored before this write, a write that judged from
// links this write changed does not stamp the row read again, whichever of the two commits first.
// Where no row is stored yet, as at a pull request's first head write or a write that moves the row
// from a repository's old name, this marks no row and the link is not followed. The tasks of the
// attribution rule, which judge a pull request's issue from its stored facts at each read, replace
// the mark.
//
// The mark owes no read (pull_request_reads_owed): the head is not in doubt, only its issue, and
// the row's head, state and mark of a read of GitHub stay as they are, so neither trigger of the
// table owes anything for it. A URL that is no stored pull request's marks nothing; a link to a
// GitHub issue the pull request's body cites, an issue named later, a new parent and a changed
// repository mapping change an attribution too, but a head row stores none of them, so those are
// followed at the pass that reaches the open row, at the pull request's next event, or at a
// results read of a commit a merged row heads and no open row does (api's
// comparisonReader.readThroughHead). The URLs are compared lowercased, as the attribution compares
// them, over every row of the table, which holds a row per pull request Dispatch has seen.
//
// The rows are locked in (repository, number) order, after the issue rows the caller's
// transaction holds: a head write that moves a row takes those issue rows before the head row too
// (headIssueBefore), so the two writes never wait on each other in a cycle.
func MarkHeadsLinked(ctx context.Context, tx refs.Execer, urls []string) error {
	if len(urls) == 0 {
		return nil
	}
	lowered := make([]string, len(urls))
	for i, url := range urls {
		lowered[i] = strings.ToLower(url)
	}
	if _, err := tx.Exec(ctx, `
		update pull_request_heads h set checked_at = null, link_mark = coalesce(h.link_mark, 0) + 1
		from (
			select repo, number from pull_request_heads
			where lower(url) = any($1::text[])
			order by repo, number
			for no key update
		) linked
		where h.repo = linked.repo and h.number = linked.number
	`, lowered); err != nil {
		return fmt.Errorf("mark the heads of the linked pull requests unread: %w", err)
	}
	return nil
}

// linkMarkOf is the link_mark of repoFull's pull request number's stored row (MarkHeadsLinked),
// nil where the row has none or no row is stored: what a head write's attribution reads before it
// reads the links (attributeHead).
func linkMarkOf(ctx context.Context, pool *store.Pool, repoFull string, number int) (*int64, error) {
	var mark *int64
	err := pool.QueryRow(ctx, `select link_mark from pull_request_heads where repo = $1 and number = $2`, repoFull, number).Scan(&mark)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read the link mark of %s#%d: %w", repoFull, number, err)
	}
	return mark, nil
}

// RejudgedHeadsPerPass is how many merged rows one reconcile pass offers under the rule-version
// clause alone (ListStalePullRequestHeads). It bounds the one-off backfill a raised
// AttributionRuleVersion starts, which nothing live waits on, so the pass's own batch stays with
// the rows a reader meets now.
const RejudgedHeadsPerPass = 50

// listStaleHeads reads one of ListStalePullRequestHeads's two selects.
func listStaleHeads(ctx context.Context, pool *store.Pool, query string, args ...any) ([]PullRequestHead, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the stale pull request heads: %w", err)
	}
	defer rows.Close()
	var heads []PullRequestHead
	for rows.Next() {
		var head PullRequestHead
		if err := rows.Scan(&head.Repo, &head.Number); err != nil {
			return nil, fmt.Errorf("list the stale pull request heads: %w", err)
		}
		heads = append(heads, head)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the stale pull request heads: %w", err)
	}
	return heads, nil
}

// MarkPullRequestHeadChecked stamps checked_at on a row the reconcile read but could not
// complete — the repository unreadable, the pull request gone — so one unreadable pull request
// does not hold the batch every pass. It stamps attribution_rule with it, from the constant and
// not from a parameter, since the row the caller could not read is one the current rule has now
// been offered: that is the same bound the refresh carries, and without it a merged row credited
// to nobody whose pull request is gone would come back to the merged arm every pass for ever.
func MarkPullRequestHeadChecked(ctx context.Context, pool *store.Pool, repoFull string, number int, now time.Time) error {
	_, err := pool.Exec(ctx, `
		update pull_request_heads set checked_at = $3, attribution_rule = $4
		where repo = $1 and number = $2
	`, repoFull, number, now, AttributionRuleVersion)
	if err != nil {
		return fmt.Errorf("stamp the head of %s#%d: %w", repoFull, number, err)
	}
	return nil
}

// headInOwnRepository reports whether a pull request's head branch lives in the repository the
// pull request is in, which is what GitHub's `head.repo.full_name` names. A head outside it is a
// fork's, and an answer that names no head repository at all — GitHub's null for a fork whose
// repository was deleted — does not say that it is this repository, so neither one is.
func headInOwnRepository(repoFull, headRepo string) bool {
	return headRepo != "" && strings.EqualFold(headRepo, repoFull)
}

// headAttributionResult is one attribution attempt's answer: the issue it picked and the source
// that named it ("none" when none did), the commit messages it read (nil when it read none,
// which a merge that follows reuses rather than paging them again), whether the head is a
// branch of the pull request's own repository, and whether the attempt read every source there
// was. complete is the write's leave to replace a stored attribution: an attempt that could not
// read a source it wanted — a payload that named no head repository, a commit read GitHub
// refused — may add an attribution but never deletes one (UpsertPullRequestHead).
type headAttributionResult struct {
	issueKey *string
	source   string
	messages []string
	ownHead  bool
	complete bool
	// linkMark is the link_mark of the row the write lands on, read before the links were
	// (linkMarkOf), which the write carries as PullRequestHead.LinkMark.
	linkMark *int64
}

// headAttribution resolves the issue a pull request belongs to from what GitHub said about it,
// through the same ranked candidates every other attribution reads
// (resolveAttributionCandidates): the first of them whose issue resolves to the repository the
// pull request is in. It holds the head to that repository too.
//
// Both holdings are what keep the results read honest. A pull request's title, body and branch
// are text any account that can open one writes, this row is written for any author and any
// repository the App watches, and an attributed row is exactly what a `before_merge` result's
// commit stands at (HeadAttributedTo), which is the reviewer's approval condition. So a
// candidate whose issue resolves to another repository, or to none, is passed over for the next
// one and a pull request no candidate of which resolves here attributes nobody; and neither
// does a pull request whose head is not in this repository, since on a public repository anyone
// may open one from a fork at any commit of their choosing — including an earlier one that
// already carries a recorded pass — and title it after an issue, which would make that commit
// a basis the recorded pass stands at again. Either way the row is still written, since the
// reconcile and the head itself are wanted either way, but with no issue and the source "none".
//
// The head is tested before any candidate is taken, since a fork's head attributes nobody
// whatever the title names. This is the one place it is tested (headInOwnRepository), and the
// answer rides back as ownHead, which is what tells attributeHead whether reading the commit
// messages could change anything, and as complete, since an answer naming no head repository at
// all left a source unread and so may attribute nobody but delete nobody either.
//
// A log line is written per call, not per attribution: the fork's head names the first
// candidate it refused, and a pull request that passed candidates over names them and the issue
// it settled on, if any. attributeHead calls this a second time, after reading the commit
// messages, whenever the first call held no candidate, so on that path both calls log and the
// second line names the candidates the commit messages added.
func headAttribution(ctx context.Context, pool *store.Pool, facts attributionFacts, headRepo string) (headAttributionResult, error) {
	attributed := headAttributionResult{
		source:   string(sourceNone),
		ownHead:  headInOwnRepository(facts.Repo, headRepo),
		complete: headRepo != "",
	}
	candidates, err := resolveAttributionCandidates(ctx, pool, facts.Repo, facts.URL, attributionInputsFrom(facts))
	if err != nil || len(candidates) == 0 {
		return attributed, err
	}
	if !attributed.ownHead {
		logging.FromContext(ctx).Info("dispatch delivery: a pull request whose head is in another repository names an issue",
			"repo", facts.Repo, "head_repo", headRepo, "issue", candidates[0].key, "source", string(candidates[0].source))
		return attributed, nil
	}
	// passedOver is every candidate ranked above the one taken, and every candidate when none
	// was held: the keys the log line below names.
	passedOver := candidates
	if i := firstHeldCandidate(candidates); i >= 0 {
		attributed.issueKey, attributed.source = &candidates[i].key, string(candidates[i].source)
		passedOver = candidates[:i]
	}
	if len(passedOver) > 0 {
		keys := make([]string, 0, len(passedOver))
		for _, candidate := range passedOver {
			keys = append(keys, candidate.key)
		}
		settled := ""
		if attributed.issueKey != nil {
			settled = *attributed.issueKey
		}
		logging.FromContext(ctx).Info("dispatch delivery: a pull request names an issue of another repository",
			"repo", facts.Repo, "passed_over", strings.Join(keys, " "), "issue", settled, "source", attributed.source)
	}
	return attributed, nil
}

// attributeHead is headAttribution plus the one source the caller's facts cannot carry: the
// commit messages, rank 5, which are read — one GitHub call, or one per page of 100 commits —
// only when no candidate the title, body, branch and links name is held to this repository, as
// every source after the first is read only when the earlier ones name nobody the pull request
// may keep (attribution.go). A pull request whose head is not this repository's is not read at
// all: it attributes nobody whatever its commits say, which is headAttribution's own ownHead
// and not a second test here.
//
// A read the App refuses leaves the attribution weaker than it could be, so the answer is not
// complete: the caller's write adds no issue, deletes none (UpsertPullRequestHead), and leaves
// the row unstamped where the caller can, for the reconcile to read again. A rate limit is
// returned rather than swallowed, so no caller persists a weaker attribution and then stamps
// the row read.
//
// The row's link mark is read first, before any statement reads the links (linkMarkOf), and the
// answer carries it, so the write that follows can tell whether a write of a link committed after
// the links were read (upsertHeadRow).
func attributeHead(ctx context.Context, pool *store.Pool, client *githubapp.Client, token func() (string, error), facts attributionFacts, headRepo string, number int) (attributed headAttributionResult, err error) {
	mark, err := linkMarkOf(ctx, pool, facts.Repo, number)
	if err != nil {
		return headAttributionResult{source: string(sourceNone)}, err
	}
	defer func() { attributed.linkMark = mark }()
	attributed, err = headAttribution(ctx, pool, facts, headRepo)
	if err != nil || attributed.issueKey != nil || !attributed.ownHead {
		return attributed, err
	}
	owner, repo, err := splitRepo(facts.Repo)
	if err != nil {
		return headAttributionResult{source: string(sourceNone)}, err
	}
	read, _, readErr := fetchCommitMessagesWithToken(ctx, client, token, owner, repo, number)
	if readErr != nil {
		if limited, ok := githubapp.AsRateLimit(readErr); ok {
			return headAttributionResult{source: string(sourceNone)}, limited
		}
		logging.FromContext(ctx).Debug("dispatch delivery: a pull request head is attributed without its commit messages",
			"repo", facts.Repo, "number", number, "error", readErr)
		attributed.complete = false
		return attributed, nil
	}
	facts.CommitMessages = read
	attributed, err = headAttribution(ctx, pool, facts, headRepo)
	attributed.messages = read
	return attributed, err
}

// headRow is the row one pull request GitHub answered makes, with the attribution the read
// reached: the one builder every GitHub reader below writes through, so they cannot drift.
// The rule that judged it is the write's own (UpsertPullRequestHead binds
// AttributionRuleVersion). The list form of a pull request carries merged_at but not merged, so
// a merge is read from whichever of the two GitHub sent. The row is marked a read of GitHub that
// took its start read (GitHubReadAt and GitHubReadOrder) where GitHub's answer carried its
// updated_at, since the mark says the row's time is GitHub's own; each reader takes read before
// its request goes out (startRead).
func headRow(repoFull string, payload pullRequestPayload, attributed headAttributionResult, checkedAt *time.Time, read readStart) PullRequestHead {
	row := PullRequestHead{
		Repo: repoFull, Number: payload.Number, HeadSHA: payload.Head.SHA, HeadRef: payload.Head.Ref,
		BaseRef: payload.Base.Ref, State: PullRequestHeadState(payload.State, payload.Merged || payload.MergedAt != nil),
		IssueKey: attributed.issueKey, AttributionSource: attributed.source,
		AttributionComplete: attributed.complete, Title: payload.Title, URL: payload.HTMLURL,
		EventAt: payload.UpdatedAt, CheckedAt: checkedAt, LinkMark: attributed.linkMark,
		MergeCommitSHA: mergeCommitOf(payload),
	}
	if payload.UpdatedAt != nil {
		row.GitHubReadAt, row.GitHubReadOrder = &read.at, &read.order
	}
	return row
}

// readStart is a read of GitHub's place in the order of every read of GitHub, and the reader's
// clock as it took it: the two values of a head row the read writes (headRow).
type readStart struct {
	at    time.Time
	order int64
}

// startRead takes a read of GitHub's place, just before its request goes out: one statement, a
// value of the sequence the owed reads' revisions come from, which only rises, so every read of
// every process takes one place in one order whatever any process's clock says (the trigger
// pull_request_heads_older_read, as 20261117172917 replaces it). A read whose place cannot be
// taken does not start: its error is the database's, which each reader answers as a failure of
// the database before a head write (ErrHeadNotStored at the intake, a failed read at the
// reconcile, and no head at a result's read-through).
func startRead(ctx context.Context, pool *store.Pool) (readStart, error) {
	var read readStart
	if err := pool.QueryRow(ctx, `select nextval('pull_request_reads_owed_revision_seq')`).Scan(&read.order); err != nil {
		return readStart{}, fmt.Errorf("take the place of a read of GitHub: %w", err)
	}
	read.at = time.Now()
	return read, nil
}

// headRepositoryOf is the repository a pull request's head branch lives in, as GitHub's answer
// names it, and "" when it names none (a deleted fork).
func headRepositoryOf(payload pullRequestPayload) string {
	if payload.Head.Repo == nil {
		return ""
	}
	return payload.Head.Repo.FullName
}

// mergeCommitOf is the commit GitHub made when a pull request merged, as its answer names it,
// and "" where it names none. GitHub answers this field for an open pull request too, with the
// test merge it keeps for the mergeability check, so only a merged pull request's value says
// anything about a branch (UpsertPullRequestHead tests the state before it believes it).
func mergeCommitOf(payload pullRequestPayload) string {
	if payload.MergeCommitSHA == nil {
		return ""
	}
	return *payload.MergeCommitSHA
}

// attributePayload attributes one pull request GitHub itself answered: the four facts its
// payload carries, the head repository it names (headRepositoryOf) and, when no candidate those
// four name is held to the repository the pull request is in, its commit messages, read with the
// caller's installation token. The one place the GitHub readers below build an attribution
// from a payload, so a sixth source, or a sixth fact a payload carries, is added in one.
func attributePayload(ctx context.Context, pool *store.Pool, client *githubapp.Client, token, repoFull string, payload pullRequestPayload) (headAttributionResult, error) {
	return attributeHead(ctx, pool, client, func() (string, error) { return token, nil },
		attributionFacts{
			Repo: repoFull, URL: payload.HTMLURL, Title: payload.Title, Body: payload.Body,
			HeadRef: payload.Head.Ref,
		}, headRepositoryOf(payload), payload.Number)
}

// RefreshPullRequestHead reads one pull request through the App and writes its head row: the
// reconcile's per-row step. It answers GitHub's refusal to give the pull request, nil where GitHub
// answered it, which the reconcile's read of a pull request an event owes a read of needs
// (readOwedPullRequests): only an answer, or a refusal that pullRequestAbsent proves final,
// settles such a read; the pass over the stored rows reads no refusal. A pull request GitHub
// cannot find (ErrPullRequestNotFound), and a repository the App cannot read (no installation,
// or a repository or resource its token is refused), are refusals and leave the row as it is,
// stamped so the batch moves on and does not warn about the same row every pass; a rate limit
// comes back as the *githubapp.RateLimitError. The refusal carries the read GitHub refused: the
// repository's canonical owner and name, the number, and the installation token the read was
// refused with, which pullRequestAbsent asks GitHub with. The row is stamped even when the commit
// messages could not be read: this pass is itself the retry, so the next one reads it again in
// PullRequestHeadStaleAfter rather than holding the front of every batch, and that write, being
// incomplete, leaves any attribution the row already carries standing.
//
// repoFull is the key ListStalePullRequestHeads read off the row, which may be a name the
// repository has been renamed away from, and every step here but the stamp works on the
// canonical name instead (refs.CanonicalRepo): the token is minted for it, since GitHub answers
// 404 for the installation of a name no longer in use; the attribution holds the pull request's
// issue to it; and the row is written under it, with the row under the old key dropped in the
// same step (upsertHeadUnderCanonicalName). The stamp stays on the key the batch handed over,
// since that is the row the batch must stop offering: a canonical repository the App cannot
// read teaches this row nothing under either name.
//
// broker is the reconcile's own, so a row this pass re-judges under a newer attribution rule
// records the move on both issues (UpsertPullRequestHead). installations is the pass's lookups of
// the installation of each repository (passInstallations), nil for a caller that has no pass.
func RefreshPullRequestHead(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, broker *events.Broker,
	installations *passInstallations, repoFull string, number int, now time.Time,
) (refused *pullRequestRefusal, err error) {
	canonical, err := refs.CanonicalRepo(ctx, pool, repoFull)
	if err != nil {
		return nil, err
	}
	owner, repo, err := splitRepo(canonical)
	if err != nil {
		return nil, err
	}
	token, err := installations.repositoryToken(ctx, client, owner, repo)
	if err != nil {
		if repositoryUnreadable(err) {
			return &pullRequestRefusal{err: err, owner: owner, repo: repo, number: number}, MarkPullRequestHeadChecked(ctx, pool, repoFull, number, now)
		}
		return nil, fmt.Errorf("mint installation token for %s PR #%d: %w", canonical, number, err)
	}
	began, err := startRead(ctx, pool)
	if err != nil {
		return nil, err
	}
	payload, err := fetchPullRequestPayload(ctx, client, token, owner, repo, number)
	if err != nil {
		if errors.Is(err, ErrPullRequestNotFound) || repositoryUnreadable(err) {
			return &pullRequestRefusal{err: err, owner: owner, repo: repo, number: number, token: token}, MarkPullRequestHeadChecked(ctx, pool, repoFull, number, now)
		}
		return nil, err
	}
	attributed, err := attributePayload(ctx, pool, client, token, canonical, payload)
	if err != nil {
		return nil, err
	}
	return nil, upsertHeadUnderCanonicalName(ctx, pool, broker, headRow(canonical, payload, attributed, &now, began), repoFull)
}

// repositoryUnreadable reports whether err is the App saying it cannot read a repository: it is
// installed on none that covers it (githubapp.ErrNoInstallation), or its token is refused the
// repository or the resource (githubapp.ErrRepositoryUnreadable). Neither is this row's own
// problem and neither mends itself within a pass, so the row is stamped read rather than warned
// about and left at the front of every batch.
func repositoryUnreadable(err error) bool {
	return errors.Is(err, githubapp.ErrNoInstallation) || errors.Is(err, githubapp.ErrRepositoryUnreadable)
}

// ReadThroughCommitPulls teaches Dispatch the pull requests GitHub names for one commit
// (`GET /repos/{o}/{r}/commits/{sha}/pulls`) and writes each one's head row, so a result
// recorded at the head of a pull request no event has reached Dispatch for stands at it at once.
// It answers how many rows it wrote and whether any pull request it saw has sha as its head
// (atHead). A repository the App cannot read writes nothing and comes back as the App's own
// error, which each caller logs at Info and carries on from (api/results.go's
// readThroughPullRequests, api/results_comparison.go's readThroughHead): the answer about that
// result stands as its stored rows leave it.
//
// atHead is the difference between the two answers GitHub gives about a commit no pull request
// of Dispatch's heads. GitHub naming nothing leaves the question wide open — the pull request
// may open at that commit a moment later, which is the whole of what this read repairs — while
// GitHub naming a pull request whose head is another commit has answered it for now: the commit
// is behind that pull request, and only a pull request opened later can put a head back on it.
// So the read-time caller holds the pair off for CommitPullSettledFor on that answer rather
// than asking again every CommitPullReadStaleAfter (delivery.CommitPullReads.Settle). The rows
// written say nothing of it on their own: the row of a pull request whose head has moved on is
// written at the head it has now, not at the commit that was asked about.
//
// Each row is attributed through all five sources, the commit messages included (attributeHead),
// exactly as the envelope writer attributes one. Attributing from fewer and calling it complete
// would delete an attribution this read-through cannot make: the write carries the pull
// request's own updated_at, so it lands on a row a `pull_request` event or the reconcile
// attributed by its commit messages and would overwrite that issue with none — for a merged row
// permanently, since the reconcile's merged arm takes a merged row only until the current rule
// has judged it, and a complete write judges it.
// A row whose commit messages could not be read is written incomplete and unstamped instead: it
// keeps the attribution the row holds (UpsertPullRequestHead), and the reconcile reads it again
// rather than this read-through standing as the last word.
//
// repoFull comes from outside — the repository the check's issue resolves to — so it is
// canonicalised before anything is asked of GitHub or written (refs.CanonicalRepo), and no row
// is ever written under a name a repository was renamed away from. The read-time caller has
// canonicalised that name already, and so has the record-time caller, which resolves through
// the same helper (api/results_comparison.go, server.checkIssueRepository); the rule reaches the
// canonical name in one step, so this read changes nothing for either. It is here because this
// function's key is its argument's, not its caller's, and a caller that hands over a name it
// has not canonicalised must still write no row under a name a repository was renamed away
// from.
//
// broker is the caller's own event broker, which a row this read re-attributes records its move
// through (UpsertPullRequestHead); a caller that holds none hands over nil and the move is
// written with no event.
func ReadThroughCommitPulls(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, broker *events.Broker,
	repoFull, sha string, now time.Time,
) (written int, atHead bool, err error) {
	canonical, err := refs.CanonicalRepo(ctx, pool, repoFull)
	if err != nil {
		return 0, false, err
	}
	owner, repo, token, err := repositoryRead(ctx, client, canonical)
	if err != nil {
		return 0, false, err
	}
	began, err := startRead(ctx, pool)
	if err != nil {
		return 0, false, err
	}
	pulls, err := fetchCommitPullPayloads(ctx, client, token, owner, repo, sha)
	if err != nil {
		return 0, false, err
	}
	for _, pull := range pulls {
		if pull.Head.SHA == sha {
			atHead = true
		}
		attributed, err := attributePayload(ctx, pool, client, token, canonical, pull)
		if err != nil {
			return written, atHead, err
		}
		var checkedAt *time.Time
		if attributed.complete {
			checkedAt = &now
		}
		if err := upsertHeadUnderCanonicalName(ctx, pool, broker, headRow(canonical, pull, attributed, checkedAt, began), repoFull); err != nil {
			return written, atHead, err
		}
		written++
	}
	return written, atHead, nil
}

// recordPullRequestHead writes the head row of the pull request one `pull_request` envelope
// names, whatever its action, author or repository, from GitHub's own answer about that pull
// request, and answers what it read of GitHub on the way (headRead): the pull request, the token
// it was read with and the commit messages the attribution paged, or GitHub's lasting refusal,
// all empty where the read was spared. It runs before the merge-only guard in
// handlePullRequestEnvelope, so an `opened`, `synchronize`, `reopened` or `edited` event — which
// the delivery timeline ignores — still moves the head a check result is measured against.
//
// The envelope says which pull request changed, and nothing it carries is written: the head,
// the state, the branch, the title and the body all come from `GET
// /repos/{owner}/{repo}/pulls/{number}` (fetchPullRequestPayload), attributed through all five
// sources as the reconcile attributes a row (attributePayload, headRow). Two reasons, each
// enough alone. An envelope carries no state, only the action, and GitHub sends `labeled`,
// `edited` and `assigned` for a closed pull request too, so a state read from the action stored
// a closed pull request as open again and a pass at its head stood. And an envelope on a
// `notifications.github.` subject is not proof GitHub sent it: any Envoy /v1 bearer can publish
// one, which is why handleWorkflowEnvelope reads its run from GitHub too, and a head a caller
// set would be what a `before_merge` pass is judged against.
//
// A pull request GitHub does not know (404 or 410) and a repository the App cannot read write
// no row: there is no answer of GitHub's to write, and nothing an envelope says is one. Both
// are acknowledged, and both record that the pull request is owed a read
// (owePullRequestRead): GitHub's answer to a read made a moment after its own webhook may not
// know the pull request yet, so the reconcile reads it again, and settles the read once GitHub
// answers for it, or once one answer of GitHub shows the repository readable and the pull request
// absent (pullRequestAbsent). A record that could not be written is marked ErrHeadNotStored, as
// every other failure is (markHeadNotStored): Intake.deliver then owes the read as a failure and
// acknowledges the envelope, or hands it back where the database cannot take that record now.
// The line that says the read is owed is written once the record is (oweRefusedRead), so it is
// true, and is written once for each envelope.
// The name and the number were checked before this runs (pullRequestNamed). The canonical name is
// checked the same way before anything is read of GitHub (checkPullRequestKey), since it comes
// from repo_projects and not from the envelope, and one that names no pull request GitHub can
// have (a rename target stored before the settings route held it to the rule, or by direct SQL)
// is a head not stored (markHeadNotStored, and not errNoPullRequestNamed): Intake.deliver owes the
// read under the name the envelope gave (owePullRequestRead), and the reconcile reads it once an
// operator corrects the target. A GitHub answer that holds a value no head row holds is a head
// not stored too (errNoRowHolds, checkHeadRow), which Intake.deliver owes a read for as for every
// other.
//
// The read is spared where GitHub has already answered for the event (headReadCovers): the
// stored row is GitHub's answer to a read whose updated_at is a later second than the envelope's.
// That is what makes the events of one pull request that waited together — the replay of the
// stream's backlog on a first start, or a burst of events a stopped process missed — cost one read
// of that pull request rather than one each, and one more for the events of the second GitHub's
// answer holds, since an answer of the event's own second may be one GitHub gave before the event.
//
// The envelope's own `repo` comes from outside — the topic a publisher built — so it is
// canonicalised before anything is read or written (refs.CanonicalRepo), and no row is ever
// written under a name a repository was renamed away from.
func (in *Intake) recordPullRequestHead(ctx context.Context, payload map[string]string, number int) (headRead, error) {
	given := payload["repo"]
	repoFull, err := refs.CanonicalRepo(ctx, in.pool, given)
	if err != nil {
		return headRead{}, markHeadNotStored(err)
	}
	if err := checkPullRequestKey(repoFull, number); err != nil {
		// A head not stored, and not errNoPullRequestNamed, which Intake.deliver ends with nothing
		// owed: the name the envelope gave is one GitHub can have, and the read is owed under it
		// (owePullRequestRead) until an operator corrects the target.
		return headRead{}, markHeadNotStored(fmt.Errorf("the canonical name of %s: %v", given, err))
	}
	owner, repo, _ := strings.Cut(repoFull, "/")
	covered, err := headReadCovers(ctx, in.pool, repoFull, number, envelopeTime(payload["updated_at"]))
	if err != nil {
		return headRead{}, markHeadNotStored(err)
	}
	if covered {
		return headRead{}, nil
	}
	// owed is the revision of the read this pull request was owed as this read begins, nil where
	// none was owed or it could not be read, read before any call to GitHub: the delete below
	// takes only that row, so an owe made after this moment stays for the reconcile.
	owed := owedRevision(ctx, in.pool, repoFull, number)
	token, err := in.github.RepositoryToken(ctx, owner, repo)
	if err != nil {
		if repositoryUnreadable(err) {
			return in.oweRefusedRead(ctx, repoFull, number, err)
		}
		return headRead{}, markHeadNotStored(fmt.Errorf("mint installation token for %s PR #%d: %w", repoFull, number, err))
	}
	// The place is a value of the owed table's revision sequence, which a migration that alters
	// that sequence, or sets the table logged or unlogged, holds locked until it commits: it waits
	// no longer than the owe beside it does (owedTableBound), so such a migration holds the
	// delivery, and one of the slots its repository shares, for seconds and not for the message's
	// whole deadline (the review of cybermonklabs/legion#4209 at 5ea6b2ffb2, N1).
	placing, cancelPlace := owedTableBound(ctx)
	began, err := startRead(placing, in.pool)
	cancelPlace()
	if err != nil {
		return headRead{}, markHeadNotStored(err)
	}
	pull, err := fetchPullRequestPayload(ctx, in.github, token, owner, repo, number)
	if err != nil {
		if errors.Is(err, ErrPullRequestNotFound) || repositoryUnreadable(err) {
			return in.oweRefusedRead(ctx, repoFull, number, err)
		}
		return headRead{}, markHeadNotStored(err)
	}
	attributed, err := attributePayload(ctx, in.pool, in.github, token, repoFull, pull)
	if err != nil {
		return headRead{}, markHeadNotStored(err)
	}
	// A read whose commit messages could not be had is written unstamped, so the reconcile's
	// next pass reads it again.
	var checkedAt *time.Time
	if attributed.complete {
		now := time.Now()
		checkedAt = &now
	}
	read := headRead{pull: &pull, token: token, messages: attributed.messages}
	if err := in.writeHead(ctx, headRow(repoFull, pull, attributed, checkedAt, began), given); err != nil {
		return read, err
	}
	// The head is GitHub's answer now, so the read owed before this read began, by an earlier
	// delivery of this envelope or any other writer, is settled. A failure to delete it costs the
	// reconcile one read, and no more. The delete waits on a lock of pull_request_reads_owed no
	// longer than the owe beside it does (owedReadWriteTimeout), so a migration of that table holds
	// the delivery, and one of the slots its repository shares, for seconds and not for the
	// message's whole deadline (the review of cybermonklabs/legion#4209 at ac562e9837, N1).
	if owed != nil {
		settling, cancel := owedTableBound(ctx)
		err := settleOwedRead(settling, in.pool, repoFull, number, *owed)
		cancel()
		if err != nil {
			logging.FromContext(ctx).Warn("dispatch delivery: delete the read owed before this read; the reconcile reads the pull request once more",
				"repo", repoFull, "number", number, "error", err)
		}
	}
	return read, nil
}

// writeHead is the intake's write of the head row GitHub answered (upsertHeadUnderCanonicalName),
// bounded as the owe beside it is (owedTableBound). The write can wait on a lock of
// pull_request_reads_owed: its trigger owes a read where two answers of one GitHub second differ,
// or where the write replaces a newer row no read of GitHub wrote (pull_request_heads_older_read).
// A write that moves the row to another issue also waits on the issue rows a write of an issue's
// links holds (headIssueBefore). A write that waits past the bound is a head not stored, so
// Intake.deliver owes the read, or hands the envelope back while that owe cannot be written
// either, and the bus keeps the event until the database takes one; on the message's own deadline
// it held a delivery, and one of the slots its repository shares, for that whole minute. The
// reconcile's and a result's writes of a head hold no slot of the intake and keep their callers'
// deadlines.
func (in *Intake) writeHead(ctx context.Context, head PullRequestHead, storedUnder string) error {
	writing, cancel := owedTableBound(ctx)
	defer cancel()
	return upsertHeadUnderCanonicalName(writing, in.pool, in.events, head, storedUnder)
}

// oweRefusedRead records that repoFull's pull request number is owed a read of kind refusal
// (owePullRequestRead) after GitHub refused the intake's read with refused, logs that once the
// record is written, and answers that refusal as what the intake read. A record that could not be
// written is marked ErrHeadNotStored, its text naming GitHub's refusal and then the record's
// failure, so Intake.deliver owes the read as a failure, or hands the envelope back with a line
// that names both, and nothing is logged here: until the record is written, nothing is owed. The
// refusal is named and not wrapped, so no caller reads the failed record as GitHub's refusal.
func (in *Intake) oweRefusedRead(ctx context.Context, repoFull string, number int, refused error) (headRead, error) {
	if err := owePullRequestRead(ctx, in.pool, repoFull, number, owedRefusal); err != nil {
		return headRead{}, markHeadNotStored(fmt.Errorf("%v, and %w", refused, err))
	}
	logging.FromContext(ctx).Info("dispatch delivery: GitHub gave no pull request for this read, so no head row is written; the read is owed, and the reconcile reads it again",
		"repo", repoFull, "number", number, "error", refused)
	return headRead{refused: refused}, nil
}

// headRead is what recordPullRequestHead learned from GitHub on the way to the head row, which
// the merge that follows reads nothing of again (handlePullRequestEnvelope): the pull request
// GitHub answered and the token it was read with, the commit messages the attribution paged,
// and GitHub's lasting refusal to answer — a pull request it does not have, or a repository the
// App cannot read. All are empty where the read was spared (headReadCovers).
type headRead struct {
	pull     *pullRequestPayload
	token    string
	messages []string
	refused  error
}

// headReadCovers answers whether the stored row of repoFull's pull request number already holds
// GitHub's answer about the change an event of updatedAt reports, so that reading the pull request
// again would teach nothing: the row is GitHub's answer to a read (github_read_at), and GitHub's
// own updated_at on it is a later second than the event's, so GitHub gave it after the event's
// change. An answer of the event's own second covers nothing: GitHub's updated_at is whole
// seconds, and that answer may be one GitHub gave before the event's change. A row no read of
// GitHub wrote covers nothing, whatever time it holds: a release before this one
// stored an envelope's updated_at, which a caller sets, and one an hour ahead would otherwise
// spare the read of every later event of that pull request. Neither does a row with no
// updated_at, a missing row, nor an event with no time.
func headReadCovers(ctx context.Context, pool *store.Pool, repoFull string, number int, updatedAt *time.Time) (bool, error) {
	var covered bool
	if err := pool.QueryRow(ctx, `
		select coalesce((
			select github_read_at is not null and event_at is not null and $3 < event_at
			from pull_request_heads where repo = $1 and number = $2
		), false)
	`, repoFull, number, updatedAt).Scan(&covered); err != nil {
		return false, fmt.Errorf("read the stored head of %s#%d: %w", repoFull, number, err)
	}
	return covered, nil
}

// envelopeTime reads an envelope's RFC3339 timestamp; an absent or malformed one is no time,
// which the upsert treats as "apply this write".
func envelopeTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil
	}
	return &at
}
