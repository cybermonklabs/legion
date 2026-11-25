package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A pull request's head is what a recorded check result is measured against: a `before_merge`
// result's commit is compared with the head of an open pull request, or the final head of a
// merged one, attributed to the check's own issue or linked from it in another repository
// (linkedPullRequestTerms). pull_request_heads holds that head for every pull request Dispatch
// has seen an event for, in any repository and by any author — not the merged population
// delivery_pull_requests keeps — and the issue Dispatch's own attribution picks for it
// (attribution.go): the first issue its ranked sources name that resolves to the repository the
// pull request is in, and only where the head branch is in that same repository, never a fork's
// (headAttribution). A child's pull request is attributed to the child, so it answers for the
// child's own checks alone and never for a check of an issue above it.
//
// Three writers keep it: the `pull_request` envelope (intake.go, every action), the read-through
// made for a commit that is no head Dispatch knows (ReadThroughCommitPulls) — by a recorded
// result, and by a writer's read of an issue's results for a `before_merge` result's commit, at
// most once every CommitPullReadStaleAfter — and the reconcile's five-minute pass (reconcile.go),
// which takes every open row read longer ago than PullRequestHeadStaleAfter (every open row at
// every pass of one process, as far as the batch reaches, and as PullRequestHeadStaleAfter says
// with several) and every merged row the rule this build judges by has not judged yet — one
// offer per rule version (AttributionRuleVersion, ListStalePullRequestHeads). A read-only
// caller's read of an issue's results reads GitHub the same way and writes nothing: it passes
// GitHub's answer to the rest of its own request alone (ReadCommitPulls, HeadAnswers), and each
// of the five reads of these rows on the results path runs its one statement over the stored rows
// with that request's answers laid on (headsStatement).
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
	// MergeCommitSHA is the commit GitHub made when this pull request merged, and MergedAt the
	// time it merged. Each is set only for a merged pull request, and only by a writer that read
	// it; one that did not read it leaves it empty, which keeps what the row holds
	// (upsertHeadRow). The row stores both (merge_commit_sha, merged_at), which is how a read
	// names the merge of the work and orders two merges by when each merged (MergeOfIssue), and
	// the merge commit also seeds branch_commits with the one fact the merge itself proves, that
	// the base branch held this commit (UpsertPullRequestHead).
	MergeCommitSHA string
	MergedAt       *time.Time
}

// PullRequestHeadState is the state a `pull_request` event leaves the row in: a pull request
// that merged is merged, whatever the action, one that closed without merging is closed, and
// every other action leaves it open. The merge is tested first: GitHub normalises every action
// of a pull request, so a `labeled` or `edited` event on an already merged one must not store
// its row as open again and put it back in the reconcile's stale list.
func PullRequestHeadState(action string, merged bool) string {
	if merged {
		return "merged"
	}
	if action != "closed" {
		return "open"
	}
	return "closed"
}

// ErrHeadNotStored marks a failure that left a pull request's head row unwritten and that
// making the same attempt again may get past, as against a failure to attribute one: nothing
// else in Dispatch repairs a head row that was never stored — the reconcile's passes read the
// rows that exist (ListStalePullRequestHeads), so a row no write ever made is offered to
// nothing. The `pull_request` envelope's handler tests for it and hands the envelope back for
// redelivery rather than acking it (Intake.handlePullRequestEnvelope, Intake.deliver), up to
// headWriteMaxDeliveries deliveries of it, since both of the statements behind the write are
// `on conflict … do update` and a redelivery writes the same pair.
//
// It marks every database failure on the way to that write and not the write alone, because
// they all end the same way and a connection-level outage reaches the earlier ones first. The
// envelope's path to the write is one read and the write: the attribution's own read of the
// issues its candidates name (headAttribution, through resolveAttributionCandidates). The
// canonical name of the repository is no read (refs.Renames.Canonical, over the pairs the boot
// read). A head row lost because Postgres refused that read is as lost as one lost at the
// insert, so recordPullRequestHead marks both (markHeadNotStored).
//
// One failure of that path is deliberately left unmarked, because no redelivery mends it. A
// read of GitHub the App refused is answered by the reconcile's own next pass, and handing
// every rate-limited envelope back would spend the whole limit over again — the rate limit is
// the only refusal of GitHub that comes back as an error at all, since attributeHead logs
// every other one at Debug and writes an incomplete row.
var ErrHeadNotStored = errors.New("the pull request's head row was not stored")

// markHeadNotStored marks err as ErrHeadNotStored unless it is the one failure that sentinel's
// doc comment leaves unmarked: a rate limit (githubapp.AsRateLimit). Everything else the steps
// before the head write answer is Postgres refusing a read, which is the fault the sentinel is
// for.
func markHeadNotStored(err error) error {
	if _, limited := githubapp.AsRateLimit(err); limited {
		return err
	}
	return fmt.Errorf("%w: %w", ErrHeadNotStored, err)
}

// UpsertPullRequestHead writes one pull request's head row. An event older than the one the row
// already holds changes nothing: GitHub redelivers and the reconcile reads concurrently, so the
// row keeps the newest `updated_at` either has seen rather than whichever write landed last.
// Every writer carries one — the envelope its own, the reconcile and the read-through the
// pull request's `updated_at` as GitHub answered it — so a write with no event time is only one
// whose envelope carried none; it always applies, as does one landing on a row that has none.
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
// events was written, and the caller may write all three again.
func UpsertPullRequestHead(ctx context.Context, pool *store.Pool, broker *events.Broker, head PullRequestHead) error {
	seedsBranch := head.seedsBranch()
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
		if left, err = headIssueBefore(ctx, tx, head.Repo, head.Number); err != nil {
			return err
		}
	}
	applied, err := upsertHeadRow(ctx, tx, head)
	if err != nil {
		return err
	}
	var moved []model.Event
	if records && applied && !sameText(left, head.IssueKey) {
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

// seedsBranch is whether writing head records its merge commit as on its base branch, for good:
// the one rule UpsertPullRequestHead's doc comment argues, kept here so a read-only read that
// stores nothing reads the same yes a writer's store of the same head would have written
// (mergeLearnedOnBranch).
func (head PullRequestHead) seedsBranch() bool {
	return head.State == "merged" && head.BaseRef != "" && head.MergeCommitSHA != ""
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
// Both owners are locked in one call before the first append rather than each Append taking its
// own as it reaches it: a transaction that appends for two owners must take them in one order or
// two such transactions moving a pull request in opposite directions would each hold the lock
// the other waits for (events.Broker.LockOwners, which sorts them by lock key).
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
	if err := broker.LockOwners(ctx, tx, moved...); err != nil {
		return nil, fmt.Errorf("record the attribution of %s#%d: %w", head.Repo, head.Number, err)
	}
	appended := make([]model.Event, 0, len(moved))
	for _, event := range moved {
		written, err := broker.Append(ctx, tx, event)
		if err != nil {
			return nil, fmt.Errorf("record the attribution of %s#%d: %w", head.Repo, head.Number, err)
		}
		appended = append(appended, written)
	}
	return appended, nil
}

// headIssueBefore is the issue the stored row credits the pull request to, read for no key
// update so that no other writer moves it between this read and the write that follows: the
// "from" of the move, and nil where no row is stored yet or the row credits nobody.
func headIssueBefore(ctx context.Context, tx pgx.Tx, repoFull string, number int) (*string, error) {
	var issue *string
	err := tx.QueryRow(ctx, `
		select issue_key from pull_request_heads where repo = $1 and number = $2 for no key update
	`, repoFull, number).Scan(&issue)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read the issue of %s#%d: %w", ErrHeadNotStored, repoFull, number, err)
	}
	return issue, nil
}

// headWriteRollback bounds the rollback of that transaction, which runs on a context of its
// own and so is bounded by nothing else. Two seconds is longer than the one statement each
// rollback here can be undoing and far inside any caller's own patience.
const headWriteRollback = 2 * time.Second

// headLaidColumns is the columns of a pull_request_heads row that the reads of the results path
// read besides its key, (repo, number), in the order headLaidRules writes them.
const headLaidColumns = `head_sha, base_ref, state, issue_key, event_at, checked_at, head_ref, merge_commit_sha, merged_at, attribution_rule`

// headLaidRules is upsertHeadRow's rule for each of headLaidColumns, one expression a column, in
// their order: stored is the row a write lands on and excluded the row it writes, as `on conflict
// … do update` names them, and complete is the SQL that says whether the write read every
// attribution source (PullRequestHead.AttributionComplete). The commit, the base branch and the
// state are the write's; the issue is the write's only where it read every source; and a time the
// write carries none of keeps the row's. upsertHeadRow's `do update set` runs them, and so does a
// read-only caller's read as it lays each answer of its request on the stored row
// (answeredHeadsRelation), so the two lay one answer one way.
func headLaidRules(complete, hasMerge string) []string {
	return []string{
		"excluded.head_sha", "excluded.base_ref", "excluded.state",
		"case when " + complete + " then excluded.issue_key else stored.issue_key end",
		"coalesce(excluded.event_at, stored.event_at)",
		"coalesce(excluded.checked_at, stored.checked_at)",
		"excluded.head_ref",
		"case when " + hasMerge + " then excluded.merge_commit_sha else stored.merge_commit_sha end",
		"coalesce(excluded.merged_at, stored.merged_at)",
		"case when excluded.checked_at is null then stored.attribution_rule else excluded.attribution_rule end",
	}
}

// headWriteApplies is upsertHeadRow's event-time guard, over stored and excluded as headLaidRules
// names them: a write applies unless it carries an event older than the one the row holds. A
// read-only caller's read tests it as it lays an answer of its request on the stored row, and
// keeps the row as it stands where it is false (answeredHeadsRelation).
const headWriteApplies = `(excluded.event_at is null or stored.event_at is null or excluded.event_at >= stored.event_at)`

// upsertHeadQuery is upsertHeadRow's statement: its parameters are the head's columns in the
// order of the insert, then $13 the attribution's completeness and $14 AttributionRuleVersion.
var upsertHeadQuery = `
	insert into pull_request_heads as stored (
		repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source,
		title, url, event_at, checked_at, attribution_rule, merge_commit_sha, merged_at
	)
	values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $14, $15, $16)
	on conflict (repo, number) do update set
		(` + headLaidColumns + `) = (` + strings.Join(headLaidRules("$13::boolean", "excluded.merge_commit_sha <> ''"), ", ") + `),
		attribution_source = case when $13::boolean
			then excluded.attribution_source else stored.attribution_source end,
		title = excluded.title,
		url = excluded.url
	where ` + headWriteApplies

// upsertHeadRow is the head row's own statement (upsertHeadQuery), which UpsertPullRequestHead's
// doc comment explains in full: it is here, on a refs.Execer — the pool, or a transaction already
// open on it — so that one statement runs on the pool where it is the whole write and inside the
// transaction where the merge's branch answer, the attribution's own events, or both go with
// it.
//
// It answers whether the write applied: false where the event-time guard (headWriteApplies) left
// the row exactly as it was, which is the one way this statement writes nothing and reports no
// error. Only an applied write can have moved the row's attribution, so that is what
// UpsertPullRequestHead tests before it appends an event.
//
// The merge commit and the merge time are written only for a merged row, and a write that
// carries neither keeps what the row holds: an envelope of the release before this one carries
// no merge time, and an edit of a merged pull request is a write too, so a write that carries
// none says nothing about the merge, which never changes once made. An open row stores neither:
// GitHub answers a merge_commit_sha for an open pull request too, its test merge (mergeCommitOf),
// which is no merge.
func upsertHeadRow(ctx context.Context, db refs.Execer, head PullRequestHead) (bool, error) {
	mergeCommit, mergedAt := "", (*time.Time)(nil)
	if head.State == "merged" {
		mergeCommit, mergedAt = head.MergeCommitSHA, head.MergedAt
	}
	tag, err := db.Exec(ctx, upsertHeadQuery,
		head.Repo, head.Number, head.HeadSHA, head.HeadRef, head.BaseRef, head.State,
		head.IssueKey, head.AttributionSource, head.Title, head.URL, head.EventAt, head.CheckedAt,
		head.AttributionComplete, AttributionRuleVersion, mergeCommit, mergedAt,
	)
	if err != nil {
		return false, fmt.Errorf("%w: store the head of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
	}
	return tag.RowsAffected() > 0, nil
}

// upsertHeadUnderCanonicalName writes head, which every caller has already keyed by the
// canonical name of its repository (refs.Renames.Canonical), and drops the row the same pull
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

// AttributedHead is one pull request Dispatch holds for an issue, as the results read names it
// among the heads a `before_merge` check compares its results with: the repository its row is
// stored under, the pull request's number, whether it is open, the commit its head is, and the
// two times Dispatch may have learned that head by. Repo is the row's own, as NewestHead.Repo is:
// the repository to read the head's check runs from, which for a pull request an issue links may
// be another than the issue's. CheckedAt is when Dispatch last read the pull request from GitHub
// and EventAt is the pull request's own update time as its newest webhook event carried it; each
// is nil where no writer has carried it. The reader shows the later of the two and names which it
// is (model.BasisLearnedReadAt, model.BasisLearnedUpdatedAt). LinkedRepo is the repository of a
// pull request the issue links in a repository it does not resolve to (linkedPullRequestTerms),
// under the canonical name those terms read, and "" for a pull request attributed to it: the
// reader names that repository beside the number, which alone would read as a pull request of
// the issue's own repository.
//
// MergedAt is when a merged pull request merged, as GitHub gave it (pull_request_heads.merged_at),
// and nil where Dispatch holds no time of its merge; AttributionRule is the rule that last judged
// the row (AttributionRuleVersion). The results read orders an issue's merged pull requests by the
// two (MergeTime): the newest is the one that merged last.
type AttributedHead struct {
	Repo    string
	Number  int
	HeadSHA string
	// Open is whether the pull request is open; a merged one's head is its final head.
	Open            bool
	CheckedAt       *time.Time
	EventAt         *time.Time
	LinkedRepo      string
	MergedAt        *time.Time
	AttributionRule int
	// NewestMerged is selected by SQL with newestMergeFirst, not a second Go ordering rule.
	NewestMerged bool
}

// MergeTime is one merged pull request row as a read that orders merged pull requests by when
// each merged takes it: the row's repository and number, when it merged (nil where the row holds
// no time of its merge), and the rule that last judged the row (AttributionRuleVersion), which
// says whether a read of GitHub may still give it a time. An AttributedHead gives one (MergeTime).
type MergeTime struct {
	Repo            string
	Number          int
	MergedAt        *time.Time
	AttributionRule int
}

// MergeTime is head's row as a read orders merged pull requests by when each merged.
func (head AttributedHead) MergeTime() MergeTime {
	return MergeTime{Repo: head.Repo, Number: head.Number, MergedAt: head.MergedAt, AttributionRule: head.AttributionRule}
}

// The results path reads pull_request_heads in five places, each through one statement and one
// function that both of its callers call: headsAttributedQuery (Attributor.HeadsAttributedTo),
// headStoredAtQuery (HeadStoredAt), baseBranchMergedForQuery (BaseBranchMergedFor),
// baseBranchMergedInQuery (Attributor.BaseBranchMergedIn) and newestPullRequestHeadQuery
// (NewestPullRequestHead). Each function takes the answers GitHub gave the caller's own request
// (HeadAnswers). A writer's read passes none, and its statement reads the table; a read-only
// caller's read passes what its read-throughs learned and stored nowhere, and its statement reads
// the stored rows with those answers laid on (answeredHeadsRelation), as the rows would stand had
// a writer's read stored each answer when it learned it. The statement after the WITH list is one
// text, so a change to it reaches both callers, and no copy of a read in Go answers for either.
// The two that read the issue's linked pull requests read them through linkedPullRequestTerms
// over their Attributor's rename pairs in both forms, so a read-only caller's read names a linked
// repository under the canonical name a writer's read names it under.

// headsStatement is one of those reads whole, in its two forms: writer, over the stored rows, and
// answered, over the stored rows with one request's answers laid on.
type headsStatement struct {
	writer, answered string
}

// newHeadsStatement makes both forms of the read whose text after the WITH list is statement:
// `with recursive`, then terms, which is linkedPullRequestTerms over an Attributor's rename pairs
// for a statement that reads linked_pulls and "" for one that does not, then the relation
// pull_request_heads (storedHeadsRelation, or answeredHeadsRelation), then statement. args is how
// many parameters statement takes, and the answers' parameters are numbered after them. keeps is
// answeredHeadsRelation's: "" for a read that lays every answer of its request.
func newHeadsStatement(terms, statement string, args int, keeps string) headsStatement {
	with := "with recursive "
	if terms != "" {
		with += terms + ",\n\t"
	}
	return headsStatement{
		writer:   with + storedHeadsRelation + "\n" + statement,
		answered: with + answeredHeadsRelation(args+1, keeps) + "\n" + statement,
	}
}

// query is the form of the read to run with answers, and its arguments, args first: the writer's
// form where answers holds none, which is every writer's read, and else the answered form, with
// the answers' arrays after args.
func (s headsStatement) query(answers HeadAnswers, args ...any) (string, []any) {
	if answers.empty() {
		return s.writer, args
	}
	return s.answered, append(args, answers.args()...)
}

// storedHeadsRelation is the relation a writer's read of the results path reads its heads from:
// the table alone, named with its schema since the relation takes its name. `not materialized`
// inlines it at each reference, so the statement's own predicates reach the table's indexes as
// they would were the table read by name.
const storedHeadsRelation = `pull_request_heads (repo, number, ` + headLaidColumns + `) as not materialized (
		select repo, number, ` + headLaidColumns + ` from public.pull_request_heads)`

// answeredHeadsRelation is the relation a read-only caller's read of the results path reads its
// heads from: the stored rows with each answer of its request laid on (HeadAnswers), the arrays of
// HeadAnswers.args its parameters, numbered from first. keeps is "" where the read lays every
// answer, and else the SQL, over one answer named `answer` (its repo, number, head_sha and
// renamed_from), that names the answers of the pull requests the read lays (headStoredAtAnswers):
// a pull request one of whose answers it names is laid with all of its answers, and the answers of
// every other pull request are left out, as though the request had not read them. Its terms:
//
//   - answers is the arrays, one row: every other term reads them from it, so the statement names
//     each parameter once, and a plan made for the parameters' values holds one copy of each.
//   - answered_heads is the answers, one row an answer, numbered in the order the request read
//     them (place, the answer's place in the arrays), each numbered among its pull request's
//     answers in that order (step), with the place of its pull request's next answer (next_place,
//     0 for the last), over the answers alone. It reads only the arrays that name the pull request
//     and the name the read was asked under, so its rows stay narrow.
//   - kept_pulls, where keeps is given, is the pull requests the read lays, listed once.
//   - answer_places is next_place by place, one array made once, so each step of laid_heads
//     reaches its pull request's next answer by its place in the arrays and never scans
//     answered_heads again: a step that joined answered_heads would scan it once a step, and the
//     laying's time would grow with the square of the answers of one pull request.
//   - laid_heads lays each kept pull request's answers on its stored row in turn, by upsertHeadRow's
//     own rules: the row stored for the pull request under the canonical name its answers carry,
//     read once by its key, and then, with no row stored, the answer as the insert writes it, and
//     with a row, headLaidRules where the event-time guard holds (headWriteApplies) and the row as
//     it stands where it does not, each column in one expression, so a step is one row made from
//     the row before it. So two answers of one request for one pull request give what the upsert
//     gives when it stores both in turn. Each row names the place of the answer it lays next
//     (next_place, 0 after the last). The rules carry each text column over and never read it, so
//     a row holds in each text column the place of the answer whose value it is, or 0 for the
//     stored row's (carried along beside it, as stored_head_sha and the like), and a step reads
//     from the arrays only the columns the rules read, each by its place. PostgreSQL finds an
//     element by its place at once only in an array of one-size elements that holds no null: in a
//     text array, or in any array that holds a null, it walks every element before it, and a step
//     that did so would make a long laying's time grow with the square of its answers. So the
//     arrays a step reads hold no null. An answer may carry no event time (headRow takes it from
//     pullRequestPayload.UpdatedAt, nil where GitHub's answer has no updated_at) and no read time
//     (ReadCommitPulls stamps none where the attribution was incomplete): event_at and checked_at
//     hold the zero time there, and has_event_at and has_checked_at, false there, give the step
//     back its null (answerArray.present). On the one path that makes answers today complete is
//     false exactly where checked_at is null, but the statement does not rely on that. Plan D33's
//     measure of the five reads, at 2,000 and 8,000 answers of one pull request: 9 to 14 ms and 35
//     to 59 ms with every tenth answer incomplete and every twentieth with no event time, 9 to 21
//     and 35 to 62 with one incomplete answer, and 9 to 14 and 38 to 66 with none. With
//     checked_at's null in the array, one incomplete answer took 83 to 117 ms at 8,000.
//   - pull_request_heads is two arms of a `union all`, the stored rows and each pull request's last
//     laid row, its text columns read at their places, less the stored rows an answer lands on or
//     drops. An answer of a read asked under a name its repository was renamed away from drops the
//     row kept under that name, as the store does (upsertHeadUnderCanonicalName). The arms carry no
//     predicate of their own, since PostgreSQL keeps a `union all` arm that has one as a subquery
//     that a join cannot drive by an index (a linked read then scanned the whole table, 95 ms on
//     60,000 rows): with none, the stored arm takes the statement's predicates, its joins' among
//     them, to the table's indexes. The rows an answer replaces are left out by a filter over both
//     arms, which the laid arm passes, `not in` the keys of the kept pull requests, each once, and
//     the names their reads were asked under: a list PostgreSQL hashes once and which holds no null.
func answeredHeadsRelation(first int, keeps string) string {
	parameters := make([]string, len(answerArrays))
	arrayOf := make(map[string]answerArray, len(answerArrays))
	for index, array := range answerArrays {
		parameters[index] = fmt.Sprintf("$%d::%s[] as %s", first+index, array.kind, array.name)
		arrayOf[array.name] = array
	}
	// Each of headLaidColumns as laid_heads' first row of a pull request holds it (started), as
	// the answer a step lays gives it (excluded), as the step lays it (laying), and as the last
	// laid row gives it to the read (laid): a text column by place, beside the stored row's value
	// (stored, carried), and every other by value.
	columns := strings.Split(headLaidColumns, ", ")
	rules := headLaidRules("excluded.complete", "excluded.has_merge_commit")
	started := make([]string, len(columns))
	excluded := make([]string, len(columns), len(columns)+2)
	laying := make([]string, len(columns))
	laid := make([]string, len(columns))
	var storedNames, stored, carried []string
	for index, column := range columns {
		laying[index] = fmt.Sprintf("case when not stored.found then excluded.%s when %s then %s else stored.%s end",
			column, headWriteApplies, rules[index], column)
		if arrayOf[column].kind == "text" {
			started[index] = "0::bigint"
			excluded[index] = "stored.next_place as " + column
			laid[index] = fmt.Sprintf("case laid.%s when 0 then laid.stored_%s else %s end",
				column, column, arrayOf[column].at("laid."+column))
			storedNames = append(storedNames, "stored_"+column)
			stored = append(stored, "stored."+column)
			carried = append(carried, "stored.stored_"+column)
			continue
		}
		started[index] = "stored." + column
		excluded[index] = arrayOf[column].at("stored.next_place") + " as " + column
		laid[index] = "laid." + column
	}
	excluded = append(excluded, arrayOf["complete"].at("stored.next_place")+" as complete",
		arrayOf["has_merge_commit"].at("stored.next_place")+" as has_merge_commit")
	// unnest is the answers as rows, with only the columns names lists, and their places.
	unnest := func(names ...string) string {
		arrays := make([]string, len(names))
		for index, column := range names {
			arrays[index] = arrayOf[column].sql()
		}
		return `unnest(` + strings.Join(arrays, ", ") + `) with ordinality as answer (` +
			strings.Join(names, ", ") + `, place)`
	}
	// kept is whether an answer's pull request is one the read lays, and keptPulls, where keeps
	// names some, the term that lists them, once for the statement.
	kept, keptPulls := "true", ""
	if keeps != "" {
		kept = "(answer.repo, answer.number) in (select pull.repo, pull.number from kept_pulls pull)"
		keptPulls = `kept_pulls as materialized (
		select distinct answer.repo, answer.number from ` + unnest("repo", "number", "head_sha", "renamed_from") + `
		where ` + keeps + `
	),
	`
	}
	return `answers as materialized (
		select ` + strings.Join(parameters, ", ") + `
	),
	answered_heads as (
		select answer.*,
			row_number() over pull as step,
			lead(answer.place, 1, 0::bigint) over pull as next_place
		from ` + unnest("repo", "number", "renamed_from") + `
		window pull as (partition by answer.repo, answer.number order by answer.place)
	),
	` + keptPulls + `answer_places as materialized (
		select array_agg(answer.next_place order by answer.place) as next_places from answered_heads answer
	),
	laid_heads (repo, number, next_place, found, ` + headLaidColumns + `, ` + strings.Join(storedNames, ", ") + `) as (
		select answer.repo, answer.number, answer.place, stored.repo is not null, ` + strings.Join(started, ", ") + `,
			` + strings.Join(stored, ", ") + `
		from answered_heads answer
		left join public.pull_request_heads stored on stored.repo = answer.repo and stored.number = answer.number
		where answer.step = 1 and ` + kept + `
		union all
		select stored.repo, stored.number, (select places.next_places from answer_places places)[stored.next_place],
			true, ` + strings.Join(laying, ",\n\t\t\t") + `,
			` + strings.Join(carried, ", ") + `
		from laid_heads stored
		cross join lateral (select ` + strings.Join(excluded, ", ") + `) excluded
		where stored.next_place <> 0
	),
	pull_request_heads (repo, number, ` + headLaidColumns + `) as not materialized (
		select repo, number, ` + headLaidColumns + ` from (
			select repo, number, ` + headLaidColumns + `, false as laid from public.pull_request_heads
			union all
			select laid.repo, laid.number, ` + strings.Join(laid, ", ") + `, true from laid_heads laid
			where laid.next_place = 0
		) heads
		where heads.laid or (heads.repo, heads.number) not in (
			select answer.repo, answer.number from answered_heads answer where answer.step = 1 and ` + kept + `
			union all
			select answer.renamed_from, answer.number from answered_heads answer
			where answer.renamed_from is not null and ` + kept + `)
	)`
}

// HeadAnswers is the pull request heads one read-only results read's read-throughs learned from
// GitHub and stored nowhere (ReadCommitPulls), as that request's reads of the results path take
// them: column by column, the parameters of answeredHeadsRelation, in the order the request read
// them, and the merges among them, each of whose merge commits is on its base branch by the yes a
// writer's store of it would have written (mergeLearnedOnBranch). The request's reader makes one
// and drops it with the request, so no answer reaches another request. Its zero value holds no
// answer, which is what a writer's read passes, and every read over it answers as a writer's read
// of the stored rows does.
type HeadAnswers struct {
	columns answerColumns
	merges  []PullRequestHead
	// last is the place in columns of the last answer held for each pull request (Add).
	last map[pullRequestKey]int
}

// pullRequestKey is one pull request as pull_request_heads keys its row: the repository, as
// written, and the number.
type pullRequestKey struct {
	repo   string
	number int
}

// answerColumns is the answers column by column, in the order the request read them, as
// answeredHeadsRelation unnests them: the head's own columns, as the row its answer would write
// (ReadCommitPulls), and, as renamed_from, the name the read was asked under where the store would
// drop the row kept under it (CommitPulls.storedUnder), else null. eventAt and checkedAt are
// times an answer may carry none of, each held as moments, so that their arrays hold no null
// (answerArray.present).
type answerColumns struct {
	repo, headSHA, baseRef, state, headRef, mergeCommit []string
	number, attributionRule                             []int32
	issueKey, renamedFrom                               []*string
	complete, hasMerge                                  []bool
	eventAt, checkedAt, mergedAt                        moments
}

// answerArray is one array HeadAnswers.args answers: its name, which answeredHeadsRelation gives
// the parameter, the type of its elements, and, for an array of times an answer may carry none of,
// present, the name of the boolean array that is false for each answer that carries none.
// HeadAnswers.args passes the times and the present of each moments as such a pair: event_at with
// has_event_at, and checked_at with has_checked_at. The times array holds no null, so a step of
// answeredHeadsRelation finds its element at once by its place, and the step reads the null back
// from present (at).
type answerArray struct {
	name, kind, present string
}

// answerArrays is each array HeadAnswers.args answers, in its order.
var answerArrays = [...]answerArray{
	{"repo", "text", ""}, {"number", "integer", ""}, {"head_sha", "text", ""}, {"base_ref", "text", ""},
	{"state", "text", ""}, {"issue_key", "text", ""}, {"complete", "boolean", ""},
	{"event_at", "timestamptz", "has_event_at"}, {"checked_at", "timestamptz", "has_checked_at"},
	{"renamed_from", "text", ""}, {"has_event_at", "boolean", ""}, {"has_checked_at", "boolean", ""},
	{"head_ref", "text", ""}, {"merge_commit_sha", "text", ""},
	{"merged_at", "timestamptz", "has_merged_at"}, {"has_merged_at", "boolean", ""},
	{"attribution_rule", "integer", ""}, {"has_merge_commit", "boolean", ""},
}

// sql is the array as answeredHeadsRelation's terms read it: from the one row of its answers
// term.
func (a answerArray) sql() string {
	return "(select answers." + a.name + " from answers)"
}

// at is the SQL of the array's element at place: the element, or null where the array names a
// present array and that array is false at place.
func (a answerArray) at(place string) string {
	element := a.sql() + "[" + place + "]"
	if a.present == "" {
		return element
	}
	return "case when " + answerArray{name: a.present}.sql() + "[" + place + "] then " + element + " end"
}

// Add lays pulls, the answer of one read-through of the request, after the answers the request
// read before it: each head GitHub named, in the order it named them, as the row StoreCommitPulls
// would write. A read that failed part-way is passed the part it attributed, which a writer's read
// stores all the same (ReadThroughCommitPulls).
//
// A head equal in every column but checked_at to the last answer held for its pull request
// (answerColumns.equalsButCheckedAt) replaces that answer rather than following it: the answer
// held takes the head's checked_at, where the head carries one. A writer's upserts of the two
// leave the row as one upsert of the answer so replaced does: the event-time guard lets the second
// through on the event time it let the first through on, every other column it writes is the
// same, and checked_at is the second's where it carries one (headLaidRules). So a run of equal
// answers, which every result commit behind one pull request's head makes, is laid on as one, and
// the cost of laying a request's answers grows with its distinct answers alone (plan D33). The
// merge of each head is kept all the same, as a writer's store of it writes its branch answer.
func (a *HeadAnswers) Add(pulls CommitPulls) {
	c := &a.columns
	for _, head := range pulls.Heads {
		if head.State != "merged" {
			head.MergeCommitSHA, head.MergedAt = "", nil
		}
		if head.seedsBranch() && !pulls.stamped {
			a.merges = append(a.merges, head)
		}
		var renamedFrom *string
		if !strings.EqualFold(pulls.storedUnder, head.Repo) {
			under := pulls.storedUnder
			renamedFrom = &under
		}
		pull := pullRequestKey{repo: head.Repo, number: head.Number}
		if last, held := a.last[pull]; held && c.equalsButCheckedAt(last, head, renamedFrom) {
			if head.CheckedAt != nil {
				c.checkedAt.set(last, *head.CheckedAt)
			}
			continue
		}
		if a.last == nil {
			a.last = map[pullRequestKey]int{}
		}
		a.last[pull] = len(c.repo)
		c.repo = append(c.repo, head.Repo)
		c.number = append(c.number, int32(head.Number))
		c.headSHA = append(c.headSHA, head.HeadSHA)
		c.baseRef = append(c.baseRef, head.BaseRef)
		c.state = append(c.state, head.State)
		c.issueKey = append(c.issueKey, head.IssueKey)
		c.complete = append(c.complete, head.AttributionComplete)
		c.eventAt.add(head.EventAt)
		c.checkedAt.add(head.CheckedAt)
		c.headRef = append(c.headRef, head.HeadRef)
		c.mergeCommit = append(c.mergeCommit, head.MergeCommitSHA)
		c.mergedAt.add(head.MergedAt)
		c.attributionRule = append(c.attributionRule, int32(AttributionRuleVersion))
		c.hasMerge = append(c.hasMerge, head.MergeCommitSHA != "")
		c.renamedFrom = append(c.renamedFrom, renamedFrom)
	}
}

// equalsButCheckedAt is whether the answer at place of c equals head, of a read asked under
// renamedFrom, in every column answeredHeadsRelation reads but checked_at: the commit, the base
// branch, the state, the issue, whether the attribution was complete, the event time and
// renamed_from. The pull request itself, repo and number, is the caller's to match.
func (c *answerColumns) equalsButCheckedAt(place int, head PullRequestHead, renamedFrom *string) bool {
	return c.headSHA[place] == head.HeadSHA && c.baseRef[place] == head.BaseRef && c.state[place] == head.State &&
		c.headRef[place] == head.HeadRef && c.mergeCommit[place] == head.MergeCommitSHA &&
		sameText(c.issueKey[place], head.IssueKey) && c.complete[place] == head.AttributionComplete &&
		sameMoment(c.mergedAt.at(place), head.MergedAt) &&
		sameMoment(c.eventAt.at(place), head.EventAt) && sameText(c.renamedFrom[place], renamedFrom)
}

// sameText is whether two nullable text columns hold one value, null equal to null.
func sameText(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// sameMoment is whether two nullable times are one moment, null equal to null.
func sameMoment(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

// moments is one column of times an answer may carry none of, as answeredHeadsRelation reads it:
// times, each answer's time, and present, whether the answer carries one. Where it carries none,
// times holds the zero time and present false, so times holds no null (answerArray.present).
type moments struct {
	times   []time.Time
	present []bool
}

// add appends moment, the time of one answer, nil where the answer carries none.
func (m *moments) add(moment *time.Time) {
	var held time.Time
	if moment != nil {
		held = *moment
	}
	m.times, m.present = append(m.times, held), append(m.present, moment != nil)
}

// set makes moment the time of the answer at place.
func (m *moments) set(place int, moment time.Time) {
	m.times[place], m.present[place] = moment, true
}

// at is the time of the answer at place, and nil where it carries none.
func (m *moments) at(place int) *time.Time {
	if !m.present[place] {
		return nil
	}
	return &m.times[place]
}

// empty is whether a holds no answer.
func (a HeadAnswers) empty() bool {
	return len(a.columns.repo) == 0
}

// args is the columns in answerArrays' order, as answeredHeadsRelation numbers its
// parameters.
func (a HeadAnswers) args() []any {
	c := a.columns
	return []any{
		c.repo, c.number, c.headSHA, c.baseRef, c.state, c.issueKey, c.complete, c.eventAt.times, c.checkedAt.times,
		c.renamedFrom, c.eventAt.present, c.checkedAt.present,
		c.headRef, c.mergeCommit, c.mergedAt.times, c.mergedAt.present, c.attributionRule, c.hasMerge,
	}
}

// headsAttributedQuery is HeadsAttributedTo's statement after its WITH list, its two arms
// HeadAttributedTo's: the attributed rows and the linked ones, the linked ones naming their
// repository. `distinct on` the four columns other than linked_repo drops a row both arms read,
// which a stale attribution can make — one judged while the issue resolved to the pull request's
// repository — and `linked_repo desc` keeps the linked arm's copy: the row is of a repository the
// issue does not resolve to, which is what the name says.
var headsAttributedQuery = `
	select repo, number, head_sha, is_open, checked_at, event_at, linked_repo, merged_at, attribution_rule,
		(not is_open and row_number() over (partition by is_open order by ` + newestMergeFirst + `) = 1) as newest_merged from (
		select distinct on (number, head_sha, checked_at, event_at)
			repo, number, head_sha, is_open, checked_at, event_at, linked_repo, merged_at, attribution_rule
		from (
			select repo, number, head_sha, state = 'open' as is_open, checked_at, event_at, '' as linked_repo,
				merged_at, attribution_rule
			from pull_request_heads
			where issue_key = $1 and state in ('open', 'merged')
			union all
			select h.repo, h.number, h.head_sha, h.state = 'open', h.checked_at, h.event_at, l.repo,
				h.merged_at, h.attribution_rule
			from linked_pulls l
			` + linkedHeadJoin + `
			where h.state in ('open', 'merged')
		) arms
		order by number, head_sha, checked_at, event_at, linked_repo desc
	) heads
	order by greatest(checked_at, event_at) desc nulls last, number desc`

// headsAttributedRead is headsAttributedQuery whole, for both callers, its linked pull requests
// read over renames (linkedPullRequestTerms). NewAttributor builds it once from its rename pairs.
func headsAttributedRead(renames refs.Renames) headsStatement {
	return newHeadsStatement(linkedPullRequestTerms(renames), headsAttributedQuery, 1, "")
}

// HeadsAttributedTo is every open or merged pull request that is issueKey's — attributed to it,
// or linked from it in a repository it does not resolve to (linkedPullRequestTerms) — newest
// learned first: the whole list HeadAttributedTo answers one commit of, which the results read
// names as what a `before_merge` check's results are compared with (R3). The two reads keep one
// rule, so a commit HeadAttributedTo answers yes for is a commit this list holds.
//
// The order is by the later of the two times either way, with a row carrying neither last and
// the pull request's own number breaking a tie, so the list does not turn on whichever row the
// planner returned first. A reader taking the first head takes the one Dispatch learned last.
//
// The attributed rows are read through pull_request_heads_issue_head's leading column, as
// MergeOfIssue's are: the rows one issue's key holds are the handful of pull requests
// attributed to that issue. The linked rows are read through pull_request_heads_repo_head's
// leading column, one repository's rows for each linked pull request.
//
// answers is what the caller's own request learned from GitHub and stored nowhere (HeadAnswers): a
// writer's read passes none and reads the stored rows, and a read-only caller's read passes its
// own and reads the stored rows with them laid on.
func (a *Attributor) HeadsAttributedTo(ctx context.Context, q refs.Queryer, issueKey string, answers HeadAnswers) ([]AttributedHead, error) {
	query, args := a.headsRead.query(answers, issueKey)
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the pull request heads of %s: %w", issueKey, err)
	}
	defer rows.Close()
	var heads []AttributedHead
	for rows.Next() {
		var head AttributedHead
		if err := rows.Scan(&head.Repo, &head.Number, &head.HeadSHA, &head.Open, &head.CheckedAt, &head.EventAt,
			&head.LinkedRepo, &head.MergedAt, &head.AttributionRule, &head.NewestMerged); err != nil {
			return nil, fmt.Errorf("read the pull request heads of %s: %w", issueKey, err)
		}
		heads = append(heads, head)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the pull request heads of %s: %w", issueKey, err)
	}
	return heads, nil
}

// NewestHead is the one pull request head the results read derives a check CI proves at: the
// repository the row is stored under, the pull request's number and its head commit.
type NewestHead struct {
	Repo    string
	Number  int
	HeadSHA string
}

// NewestPullRequestHead is the head of the open pull request of the highest number attributed
// to issueKey, or, where none is open, the final head of its merged pull request that merged
// last, and false where Dispatch holds neither. GitHub's numbers rise, so the highest is the
// newest open one; a merge's time is when GitHub merged it (merged_at), and among merged pull
// requests a row holding no time comes after every row that holds one, the higher number first
// between two of one time or of none. A read that must not take the newest of rows it cannot
// order asks first whether each holds a time (api's fillMergeTimes). Both are heads
// HeadsAttributedTo lists, so a result at the commit answered here stands at one of the heads a
// `before_merge` check compares its results with. A closed-unmerged pull request is no head. The
// repository is the row's own, which is the one to read the head's check runs from
// (api/results_ci.go).
//
// The read uses pull_request_heads_issue_head's leading column, as HeadsAttributedTo's does, and
// takes the caller's request's answers as HeadsAttributedTo takes them.
func NewestPullRequestHead(ctx context.Context, q refs.Queryer, issueKey string, answers HeadAnswers) (NewestHead, bool, error) {
	query, args := newestPullRequestHeadRead.query(answers, issueKey)
	var head NewestHead
	err := q.QueryRow(ctx, query, args...).Scan(&head.Repo, &head.Number, &head.HeadSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return NewestHead{}, false, nil
	}
	if err != nil {
		return NewestHead{}, false, fmt.Errorf("read the newest pull request head of %s: %w", issueKey, err)
	}
	return head, true, nil
}

// newestPullRequestHeadQuery is NewestPullRequestHead's statement after its WITH list.
var newestPullRequestHeadQuery = `
	select repo, number, head_sha from pull_request_heads
	where issue_key = $1 and state in ('open', 'merged')
	order by state = 'open' desc, ` + newestMergeFirst + `
	limit 1`

// newestPullRequestHeadRead is newestPullRequestHeadQuery whole, for both callers.
var newestPullRequestHeadRead = newHeadsStatement("", newestPullRequestHeadQuery, 1, "")

// HeadStoredAt reports whether sha is the head of an open or merged pull request stored for
// repoFull, whatever issue it is attributed to: whether Dispatch holds a pull request at that
// commit that its own passes keep reading. It is a weaker question than HeadAttributedTo's and
// it is asked for another purpose — the results read asks it before it reads GitHub for a commit,
// since a commit such a row already heads is one the reconcile re-reads and a read from there
// repairs nothing (api/results_comparison.go, comparisonReader.readThroughHead).
//
// A closed-unmerged row is counted for nothing, as HeadAttributedTo counts it for nothing: it
// is no basis a result stands at, the reconcile keeps no such row in its passes
// (ListStalePullRequestHeads takes the open and the merged), and a pull request closed at a
// commit does not stop another being opened from the same head. Counting one would leave every
// result at that commit standing at no basis for good.
//
// repoFull is the canonical name of the repository (refs.Renames.Canonical), compared without
// case, as every other comparison of a repository here is: a row written from a `pull_request`
// envelope carries GitHub's own spelling of the repository (`Acme/Widgets`), while the results
// read is passed the lowercased spelling an issue's own repository and a project's default
// repository are stored under (refs.ExternalRepo), so an exact comparison would answer "no row"
// for a repository whose owner writes their name with capitals. The read is one index lookup on
// pull_request_heads_repo_head (migration 20261105112616), which is built on
// `(lower(repo), head_sha)` for that reason: pull_request_heads_issue_head is partial on
// `issue_key is not null` and keyed on the issue, so it serves no lookup by repository and head
// commit.
//
// answers is the caller's request's, as HeadsAttributedTo takes them: a head an earlier
// read-through of a read-only caller's request learned holds that read off GitHub, as the row a
// writer's read-through stored holds the writer's off. Of them it lays only the answers of the
// pull requests that can change its answer (headStoredAtAnswers), so a learn that reads many
// commits through lays each commit's few pull requests and not every head learned before it. Each
// call still unnests, sorts and aggregates every answer of the request to pick those
// (answered_heads and answer_places in answeredHeadsRelation), so one learn of N commits costs N
// times the request's answers.
func HeadStoredAt(ctx context.Context, q refs.Queryer, repoFull, sha string, answers HeadAnswers) (bool, error) {
	query, args := headStoredAtRead.query(answers, repoFull, sha)
	var found bool
	if err := q.QueryRow(ctx, query, args...).Scan(&found); err != nil {
		return false, fmt.Errorf("read the stored pull request heads of %s at %s: %w", repoFull, sha, err)
	}
	return found, nil
}

// headStoredAtQuery is HeadStoredAt's statement after its WITH list.
var headStoredAtQuery = `
	select exists (
		select 1 from pull_request_heads
		where lower(repo) = lower($1) and head_sha = $2 and state in ('open', 'merged')
	)`

// headStoredAtRead is headStoredAtQuery whole, for both callers.
var headStoredAtRead = newHeadsStatement("", headStoredAtQuery, 2, headStoredAtAnswers)

// headStoredAtAnswers is the answers HeadStoredAt lays (answeredHeadsRelation's keeps), over one
// answer named `answer`, $1 the repository and $2 the commit: those of each pull request of the
// repository that an answer names at the commit, or whose row stored under the canonical name its
// answer carries, or under the name its read was asked under (renamed_from), heads the commit.
// Only these can change the answer: an answer can move a stored row's head off the commit, close
// it, or drop the row kept under the name its read was asked under, and a pull request none of
// whose rows or answers heads the commit lays no row that heads it. The stored rows at the commit
// are one lookup on pull_request_heads_repo_head, which PostgreSQL hashes once for the statement.
const headStoredAtAnswers = `lower(answer.repo) = lower($1) and answer.head_sha = $2
	or (answer.repo, answer.number) in (` + headsStoredAtCommit + `)
	or (answer.renamed_from, answer.number) in (` + headsStoredAtCommit + `)`

// headsStoredAtCommit is the key of each row stored for repository $1, compared without case, whose
// head is commit $2.
const headsStoredAtCommit = `select repo, number from public.pull_request_heads where lower(repo) = lower($1) and head_sha = $2`

// PullRequestHeadStaleAfter is how long an open pull request's row may go unread before the
// reconcile reads it again.
//
// The bound is duty.LeasePeriod(ReconcileInterval), ReconcileInterval less a tenth of it: the
// shortest wait the reconcile leaves after a pass, whose waits are drawn in
// (ReconcileInterval - ReconcileInterval/10, ReconcileInterval] (duty.RunJittered,
// duty.JitteredWithin). A row a pass reads is stamped (checked_at) no later than that pass ends.
// With one process the next pass starts more than this bound after that end, so every pass
// re-reads every open row the pass before it read, as far as the batch reaches. With several
// processes serving one database the next pass can be another process's, starting less than this
// bound after the read, and it leaves the row as fresh. The first pass that starts more than this
// bound after the read reads it, and the deployment's passes come at most ReconcileInterval
// apart, so two reads of an open row are at most about this bound plus ReconcileInterval apart,
// nine and a half minutes, plus the passes' own run time. A bound of the whole ReconcileInterval
// would leave such a row not yet due at one process's next pass whenever that pass's wait, with
// the time the pass before ran on after the read, came to five minutes or less, which a quick pass
// almost always does, and the row would wait for the pass after: about ten minutes between two
// reads of it rather than five.
var PullRequestHeadStaleAfter = duty.LeasePeriod(ReconcileInterval)

// PullRequestHeadBatch is how many rows the reconcile reads again in one pass.
const PullRequestHeadBatch = 200

// ListStalePullRequestHeads is at most limit rows for the reconcile to read again, the longest
// unread first: every open row whose checked_at is older than PullRequestHeadStaleAfter (a row
// never read first), every merged row credited to nobody that no read has reached, and every
// merged row a rule older than this build's judged (AttributionRuleVersion).
//
// A merged row is revisited at all because nothing else ever revisits one. An open row comes
// round at every pass of one process, each starting at most five minutes after the one before it
// ended, and as PullRequestHeadStaleAfter says with several; a merged row is written once more at
// most, by an envelope or a read-through, and a recorded result reads through only for a commit
// no head Dispatch knows. So a merged row left credited to nobody stays that way, and every
// `before_merge` result at that head stands at no basis for ever: a merge delivered by a
// `pull_request` envelope naming no head repository is one such row (recordPullRequestHead
// attributes nobody, since it cannot tell this repository's branch from a fork's), and so is
// every pull request merged under an older attribution rule that credited nobody — a title
// naming another repository's issue first was the whole of INFRA-1240.
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
// read-through, and an envelope only when its payload was a complete read (UpsertPullRequestHead
// keeps both where the write carries no read time). So a merged row is taken once per rule: the
// refresh stamps it at AttributionRuleVersion whether or not GitHub's answer attributed it, and
// it comes back only when a later build raises that constant, which is how a change to the
// attribution reaches the merged rows already stored, and how a column a merged row gains is
// filled: rule 6's refresh stores each merged row's merge commit and merge time from GitHub's
// answer (headRow, upsertHeadRow). The `checked_at is null` half of the uncredited term keeps
// that bound: without it every merged row credited to nobody would come back every pass for
// ever.
//
// The arm is written as `(issue_key is null and checked_at is null) or attribution_rule < $3`
// rather than the conjunction it is equal to — `(issue_key is null or attribution_rule < $3)
// and (checked_at is null or attribution_rule < $3)` — because the two terms are what the two
// partial indexes hold (pull_request_heads_merged_unread and pull_request_heads_merged_rule,
// 20261111060005): written this way the planner takes a bitmap of each and ors them, and
// written the other way it reads every row of the table. Both forms select the same rows.
//
// The rule half is read in a pass of its own, bounded by RejudgedHeadsPerPass, and the rest of
// limit goes to the rows a reader meets now: the open rows, whose head decides a live result,
// and the merged rows no read ever reached. Raising the rule offers every merged row of the
// deployment once, which on a production-shaped table measured for INFRA-1522 is 12,000 rows,
// about 12,000 GitHub reads and 2,400 an hour against an installation's 5,000 — half a
// deployment's budget for five hours, for a backfill nothing live waits on. At 50 a pass, and at
// most one pass in each 4.5-minute period of the reconcile's lease, it is about 600 to 670 an
// hour and about a day, and a table of a few hundred merged rows is still a few passes. The two
// reads are ordered the same way and the bound is applied to their sum, so a pass never reads
// more rows than it did before.
func ListStalePullRequestHeads(ctx context.Context, pool *store.Pool, limit int, now time.Time) ([]PullRequestHead, error) {
	heads, err := listStaleHeads(ctx, pool, `
		select repo, number from pull_request_heads
		where (state = 'open' and (checked_at is null or checked_at < $1))
		   or (state = 'merged' and issue_key is null and checked_at is null)
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
// repository was deleted, or a `pull_request` envelope published before Dispatch carried the
// field — does not say that it is this repository, so neither one is.
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
func (a *Attributor) headAttribution(ctx context.Context, pool *store.Pool, facts attributionFacts, headRepo string) (headAttributionResult, error) {
	attributed := headAttributionResult{
		source:   string(sourceNone),
		ownHead:  headInOwnRepository(facts.Repo, headRepo),
		complete: headRepo != "",
	}
	candidates, err := a.resolveAttributionCandidates(ctx, pool, facts.Repo, facts.URL, a.attributionInputsFrom(facts))
	if err != nil || len(candidates) == 0 {
		return attributed, err
	}
	if !attributed.ownHead {
		slog.Info("dispatch delivery: a pull request whose head is in another repository names an issue",
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
		slog.Info("dispatch delivery: a pull request names an issue of another repository",
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
// the row read. The read given up is noted on the attempt ctx carries (githubreads), which only
// the reconcile's leased pass carries: the intakes and the results routes call this too, and
// their contexts carry none, so the note changes nothing there.
func (a *Attributor) attributeHead(ctx context.Context, pool *store.Pool, client *githubapp.Client, token func() (string, error), facts attributionFacts, headRepo string, number int) (headAttributionResult, error) {
	attributed, err := a.headAttribution(ctx, pool, facts, headRepo)
	if err != nil || attributed.issueKey != nil || !attributed.ownHead {
		return attributed, err
	}
	owner, repo, err := refs.SplitGitHubRepo(facts.Repo)
	if err != nil {
		return headAttributionResult{source: string(sourceNone)}, err
	}
	read, _, readErr := fetchCommitMessagesWithToken(ctx, client, token, owner, repo, number)
	if readErr != nil {
		if limited, ok := githubapp.AsRateLimit(readErr); ok {
			return headAttributionResult{source: string(sourceNone)}, limited
		}
		slog.Debug("dispatch delivery: a pull request head is attributed without its commit messages",
			"repo", facts.Repo, "number", number, "error", readErr)
		githubreads.NoteFailuref(ctx, "attribute the head of %s PR #%d without its commit messages: %w", facts.Repo, number, readErr)
		attributed.complete = false
		return attributed, nil
	}
	facts.CommitMessages = read
	attributed, err = a.headAttribution(ctx, pool, facts, headRepo)
	attributed.messages = read
	return attributed, err
}

// headRow is the row one pull request GitHub answered makes, with the attribution the read
// reached: the one builder both GitHub readers below write through, so the two cannot drift.
// The rule that judged it is the write's own (UpsertPullRequestHead binds
// AttributionRuleVersion). The list form of a pull request carries merged_at but not merged, so
// a merge is read from whichever of the two GitHub sent; merged_at is also the time the row
// stores for the merge, so the reconcile's refresh of a merged row fills it (upsertHeadRow).
func headRow(repoFull string, payload pullRequestPayload, attributed headAttributionResult, checkedAt *time.Time) PullRequestHead {
	return PullRequestHead{
		Repo: repoFull, Number: payload.Number, HeadSHA: payload.Head.SHA, HeadRef: payload.Head.Ref,
		BaseRef: payload.Base.Ref, State: PullRequestHeadState(payload.State, payload.Merged || payload.MergedAt != nil),
		IssueKey: attributed.issueKey, AttributionSource: attributed.source,
		AttributionComplete: attributed.complete, Title: payload.Title, URL: payload.HTMLURL,
		EventAt: payload.UpdatedAt, CheckedAt: checkedAt, MergeCommitSHA: mergeCommitOf(payload),
		MergedAt: payload.MergedAt,
	}
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
// caller's installation token. The one place the two GitHub readers below build an attribution
// from a payload, so a sixth source, or a sixth fact a payload carries, is added in one.
func (a *Attributor) attributePayload(ctx context.Context, pool *store.Pool, client *githubapp.Client, token, repoFull string, payload pullRequestPayload) (headAttributionResult, error) {
	return a.attributeHead(ctx, pool, client, func() (string, error) { return token, nil },
		attributionFacts{
			Repo: repoFull, URL: payload.HTMLURL, Title: payload.Title, Body: payload.Body,
			HeadRef: payload.Head.Ref,
		}, headRepositoryOf(payload), payload.Number)
}

// RefreshPullRequestHead reads one pull request through the App and writes its head row: the
// reconcile's per-row step. A pull request GitHub cannot find, and a repository the App cannot
// read (no installation, or a repository or resource its token is refused), leave the row as it
// is, stamped so the batch moves on and does not warn about the same row every pass. A canonical
// name GitHub does not allow (refs.SplitGitHubRepo's refs.ErrNotGitHubRepo) is stamped the same
// way, before any GitHub call, and comes back as that error, so the reconcile warns of the row on
// the pass that stamps it and not on every pass; a rate limit comes back as the
// *githubapp.RateLimitError, which stops the batch. The row is stamped even when the commit
// messages could not be read: this pass is itself the retry, so the next one reads it again in
// PullRequestHeadStaleAfter rather than holding the front of every batch, and that write, being
// incomplete, leaves any attribution the row already carries standing.
//
// repoFull is the key ListStalePullRequestHeads read off the row, which may be a name the
// repository has been renamed away from, and every step here but the stamp works on the
// canonical name instead (refs.Renames.Canonical): the token is minted for it, since GitHub
// answers 404 for the installation of a name no longer in use; the attribution holds the pull
// request's issue to it; and the row is written under it, with the row under the old key
// dropped in the same step (upsertHeadUnderCanonicalName). The stamp stays on the key the batch
// handed over, since that is the row the batch must stop offering: a canonical repository the
// App cannot read teaches this row nothing under either name.
//
// broker is the reconcile's own, so a row this pass re-judges under a newer attribution rule
// records the move on both issues (UpsertPullRequestHead).
func (a *Attributor) RefreshPullRequestHead(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, broker *events.Broker,
	repoFull string, number int, now time.Time,
) error {
	pulls, stamp, err := a.readPullRequestHead(ctx, pool, client, repoFull, number, now)
	if stamp {
		return errors.Join(err, MarkPullRequestHeadChecked(ctx, pool, repoFull, number, now))
	}
	if err != nil {
		return err
	}
	_, err = StoreCommitPulls(ctx, pool, broker, pulls)
	return err
}

// readPullRequestHead is the shared GitHub read of a refresh. stamp means that the caller
// must mark the existing row checked, without replacing it or recording its merge again.
func (a *Attributor) readPullRequestHead(
	ctx context.Context, pool *store.Pool, client *githubapp.Client,
	repoFull string, number int, now time.Time,
) (CommitPulls, bool, error) {
	canonical := a.canonical(repoFull)
	owner, repo, err := refs.SplitGitHubRepo(canonical)
	if err != nil {
		return CommitPulls{}, true, err
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		if RepositoryUnreadable(err) {
			return CommitPulls{}, true, nil
		}
		return CommitPulls{}, false, fmt.Errorf("mint installation token for %s PR #%d: %w", canonical, number, err)
	}
	payload, err := fetchPullRequestPayload(ctx, client, token, owner, repo, number)
	if err != nil {
		if errors.Is(err, ErrPullRequestNotFound) || RepositoryUnreadable(err) {
			return CommitPulls{}, true, nil
		}
		return CommitPulls{}, false, err
	}
	attributed, err := a.attributePayload(ctx, pool, client, token, canonical, payload)
	if err != nil {
		return CommitPulls{}, false, err
	}
	return CommitPulls{Heads: []PullRequestHead{headRow(canonical, payload, attributed, &now)}, storedUnder: repoFull}, false, nil
}

// ReadPullRequestHead reads exactly what RefreshPullRequestHead reads, but writes nothing.
// Its caller adds the answer to its request's HeadAnswers instead of the shared store.
func (a *Attributor) ReadPullRequestHead(
	ctx context.Context, pool *store.Pool, client *githubapp.Client,
	repoFull string, number int, now time.Time, answers HeadAnswers,
) (CommitPulls, error) {
	pulls, stamp, err := a.readPullRequestHead(ctx, pool, client, repoFull, number, now)
	if !stamp {
		return pulls, err
	}
	// Stamp the row the writer would now see, including this request's earlier answers.
	// No attribution, merge metadata or branch fact is replaced by a stale physical row.
	head, found, readErr := headForRefresh(ctx, pool, repoFull, number, answers)
	if readErr != nil || !found {
		return CommitPulls{}, errors.Join(err, readErr)
	}
	head.CheckedAt = &now
	return CommitPulls{Heads: []PullRequestHead{head}, storedUnder: repoFull, stamped: true}, err
}

// RepositoryUnreadable reports whether err is the App saying it cannot read a repository: it is
// installed on none that covers it (githubapp.ErrNoInstallation), or its token is refused the
// repository or the resource (githubapp.ErrRepositoryUnreadable, a 403 or 404 that
// githubapp.RateLimit does not read as a rate limit; the pull request read makes its own 404
// ErrPullRequestNotFound instead). Both are GitHub's answer about the repository and not a read
// that failed: no retry changes either until someone gives the App the repository, while a rate
// limit, a 5xx and a transport failure are reads to make again. So the reconcile stamps the row
// read rather than warning about it and leaving it at the front of every batch, and the results
// route counts a pull request an issue links in such a repository for nothing, as it counts one
// GitHub answers 404 for (api/results_linked.go, linkedRepositoryHolds).
func RepositoryUnreadable(err error) bool {
	return errors.Is(err, githubapp.ErrNoInstallation) || errors.Is(err, githubapp.ErrRepositoryUnreadable)
}

// ReadThroughCommitPulls teaches Dispatch the pull requests GitHub names for one commit
// (`GET /repos/{o}/{r}/commits/{sha}/pulls`) and writes each one's head row, so a result
// recorded at the head of a pull request no event has reached Dispatch for stands at it at once.
// It is its two halves in turn: ReadCommitPulls asks GitHub and answers the rows, and
// StoreCommitPulls writes them. It answers how many rows it wrote and whether any pull request
// it saw has sha as its head (atHead). A repository the App cannot read writes nothing and comes
// back as the App's own error, which each caller logs at Info and carries on from (api/results.go's
// readThroughPullRequests, api/results_comparison.go's readThroughHead): the answer about that
// result stands as its stored rows leave it.
//
// A writer's read calls this. A read-only caller's read of an issue's results stores nothing, so
// it calls ReadCommitPulls alone and passes the answer to the rest of its own request
// (HeadAnswers.Add).
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
// An attribution that fails part-way leaves the rows attributed before it written, as it always
// has, and answers its error. A write that fails stops the store and answers its error; by then
// every pull request GitHub named has been attributed, since the read comes first, so that
// failure can cost the commit-message reads of the pull requests after the failed row, which
// change nothing stored.
//
// broker is the caller's own event broker, which a row this read re-attributes records its move
// through (UpsertPullRequestHead); a caller that holds none hands over nil and the move is
// written with no event.
func (a *Attributor) ReadThroughCommitPulls(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, broker *events.Broker,
	repoFull, sha string, now time.Time,
) (written int, atHead bool, err error) {
	pulls, readErr := a.ReadCommitPulls(ctx, pool, client, repoFull, sha, now)
	written, err = StoreCommitPulls(ctx, pool, broker, pulls)
	if err == nil {
		err = readErr
	}
	return written, pulls.AtHead, err
}

// CommitPulls is what one read of GitHub found for one commit (Attributor.ReadCommitPulls): the
// head row each pull request GitHub named would be written as, in the order GitHub named them,
// and whether any of them has the commit as its head (AtHead). A writer's read stores it
// (StoreCommitPulls). A read-only caller's read passes it to the rest of its own request instead
// (HeadAnswers.Add), whose reads of the results path lay it on the stored rows
// (Branches.ReadCommitOnBranch among them), so both reads answer alike.
type CommitPulls struct {
	Heads  []PullRequestHead
	AtHead bool
	// storedUnder is the name of the repository the read was asked under, which may be one the
	// repository was renamed away from: the store drops the row a pull request is kept under by
	// that name where it is not the canonical one the heads are keyed by
	// (upsertHeadUnderCanonicalName).
	storedUnder string
	// stamped carries only the checked marker of an unreadable existing row, not a new merge.
	stamped bool
}

// ReadCommitPulls is the read half of ReadThroughCommitPulls, and writes nothing: it asks GitHub
// for the pull requests of one commit (`GET /repos/{o}/{r}/commits/{sha}/pulls`) and answers the
// head row each one would be written as. An attribution that fails answers the rows attributed
// before it with its error, so a writer's read still stores those.
//
// Each row is attributed through all five sources, the commit messages included (attributeHead),
// exactly as the envelope writer attributes one. Attributing from fewer and calling it complete
// would delete an attribution this read-through cannot make: the write carries the pull
// request's own updated_at, so it lands on a row a `pull_request` event or the reconcile
// attributed by its commit messages and would overwrite that issue with none — for a merged row
// permanently, since the reconcile's merged arm takes a merged row only until the current rule
// has judged it, and a complete write judges it.
// A row whose commit messages could not be read is answered incomplete and unstamped instead: it
// keeps the attribution the row holds (UpsertPullRequestHead), and the reconcile reads it again
// rather than this read-through standing as the last word. A complete row is stamped read at now.
//
// repoFull comes from outside — the repository the check's issue resolves to — so it is
// canonicalised before anything is asked of GitHub (refs.Renames.Canonical), and no row is ever
// keyed under a name a repository was renamed away from. The read-time caller has canonicalised
// that name already, and so has the record-time caller, which resolves through the same helper
// (api/issue_repo.go, server.checkIssueRepository); the rule reaches the canonical name in one
// step, so this read changes nothing for either. It is here because this function's key is its
// argument's, not its caller's, and a caller that hands over a name it has not canonicalised
// must still have no row keyed under a name a repository was renamed away from.
func (a *Attributor) ReadCommitPulls(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, repoFull, sha string, now time.Time,
) (CommitPulls, error) {
	canonical := a.canonical(repoFull)
	owner, repo, token, err := repositoryRead(ctx, client, canonical)
	if err != nil {
		return CommitPulls{}, err
	}
	payloads, err := fetchCommitPullPayloads(ctx, client, token, owner, repo, sha)
	if err != nil {
		return CommitPulls{}, err
	}
	pulls := CommitPulls{storedUnder: repoFull}
	for _, payload := range payloads {
		if payload.Head.SHA == sha {
			pulls.AtHead = true
		}
		attributed, err := a.attributePayload(ctx, pool, client, token, canonical, payload)
		if err != nil {
			return pulls, err
		}
		var checkedAt *time.Time
		if attributed.complete {
			checkedAt = &now
		}
		pulls.Heads = append(pulls.Heads, headRow(canonical, payload, attributed, checkedAt))
	}
	return pulls, nil
}

// StoreCommitPulls is the store half of ReadThroughCommitPulls: it writes each head row pulls
// holds, in order, under the canonical name its read keyed it by, dropping the row the same pull
// request is kept under by the name the read was asked under where that is another
// (upsertHeadUnderCanonicalName). Each write records its attribution move through broker and a
// merge's branch answer with it (UpsertPullRequestHead). It answers how many rows it wrote, and
// stops at the first write that fails.
func StoreCommitPulls(ctx context.Context, pool *store.Pool, broker *events.Broker, pulls CommitPulls) (int, error) {
	written := 0
	for _, head := range pulls.Heads {
		if err := upsertHeadUnderCanonicalName(ctx, pool, broker, head, pulls.storedUnder); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// mergeLearnedOnBranch is whether StoreCommitPulls, storing the answers of the caller's request,
// would record sha as on branch of repoFull: the yes UpsertPullRequestHead writes for a merged pull
// request's merge commit on its base branch (seedsBranch), whether or not its event-time guard
// applies the head row, compared as branch_commits keys it, the repository without case and the
// branch and the commit exactly (readBranchCommit).
func mergeLearnedOnBranch(answers HeadAnswers, repoFull, branch, sha string) bool {
	for _, head := range answers.merges {
		if strings.EqualFold(head.Repo, repoFull) && head.BaseRef == branch && head.MergeCommitSHA == sha {
			return true
		}
	}
	return false
}

// recordPullRequestHead writes the head row one `pull_request` envelope reports, whatever its
// action, author or repository, and answers the commit messages it read, nil when it read none.
// It runs before the merge-only guard in handlePullRequestEnvelope, so an `opened`,
// `synchronize`, `reopened` or `edited` event — which the delivery timeline ignores — still
// moves the head a check result is measured against.
//
// The envelope carries the title, body, branch and head commit, which are four of the
// attribution's five sources and outrank the fifth; the fifth, the commit messages, is read only
// when no candidate those four name is held to the repository the pull request is in
// (attributeHead), and a merge that follows reuses what was read rather than paging the same
// commits again (completePullRequest). A read the App refuses attributes the row from the
// envelope alone and makes the write an incomplete one, which deletes no attribution the row
// already carries and leaves it unstamped; a rate limit is returned, as the reconcile's own
// refresh returns it, so the caller does not persist a weaker attribution and then stamp the
// row read.
//
// The envelope also carries the head's own repository (`head_repo`), which the attribution
// holds the head to. An envelope that does not name it — one a publisher older than this field
// made — attributes nobody, since a head that cannot be shown to be this repository's is a
// fork's as far as Dispatch knows. That write is incomplete too: it leaves the row unstamped, so
// the reconcile's own read of GitHub, which always names the head repository, attributes the row
// on its next pass, and it leaves any attribution an earlier, fuller read stored exactly where
// it is rather than clearing it on the way through — on a merge, which nothing but that arm of
// the reconcile ever revisits, clearing it would be for good.
//
// The envelope's own `repo` comes from outside — the topic a publisher built — so it is
// canonicalised before anything is read or written (refs.Renames.Canonical), and no row is ever
// written under a name a repository was renamed away from. It is also what the head is held to:
// an envelope keyed by an old name carries a `head_repo` GitHub spells under the new one, and
// comparing those two would make every such pull request a fork's (headInOwnRepository).
//
// The split, the attribution and the write can fail here, and each failure is marked, or left
// bare, by what a redelivery of the envelope would do with it (markHeadNotStored,
// ErrHeadNotStored). The attribution is a read of Postgres before any row is written, so its
// failure loses the head row exactly as a refused insert does and is marked; the attribution's
// own exception, a rate limit, is not. The split this function makes for itself is the one
// failure it returns bare without asking markHeadNotStored at all: a name that is not
// `owner/repo` is malformed in the envelope, so the next delivery of the same envelope carries
// the same name and would fail on it again. The canonical name is no read at all
// (refs.Renames.Canonical), so it cannot fail.
func (in *Intake) recordPullRequestHead(ctx context.Context, payload map[string]string, number int) ([]string, error) {
	given := payload["repo"]
	repoFull := in.attributor.canonical(given)
	owner, repo, err := refs.SplitGitHubRepo(repoFull)
	if err != nil {
		return nil, err
	}
	attributed, err := in.attributor.attributeHead(ctx, in.pool, in.github,
		func() (string, error) { return in.github.RepositoryToken(ctx, owner, repo) },
		attributionFacts{
			Repo: repoFull, URL: payload["url"], Title: payload["title"], Body: payload["body"],
			HeadRef: payload["head_ref"],
		}, payload["head_repo"], number)
	if err != nil {
		return nil, markHeadNotStored(err)
	}
	// The envelope is itself a fresh read of the head, so the row is stamped read and the
	// reconcile does not spend three GitHub calls re-reading what the event just delivered —
	// unless the read was incomplete, because the commit messages were wanted and could not be
	// had or the envelope named no head repository, which the next pass settles either way.
	var checkedAt *time.Time
	if attributed.complete {
		now := time.Now()
		checkedAt = &now
	}
	return attributed.messages, upsertHeadUnderCanonicalName(ctx, in.pool, in.events, PullRequestHead{
		Repo: repoFull, Number: number, HeadSHA: payload["head_sha"], HeadRef: payload["head_ref"],
		BaseRef: payload["base_ref"], State: PullRequestHeadState(payload["action"], payload["merged"] == "true"),
		IssueKey: attributed.issueKey, AttributionSource: attributed.source,
		AttributionComplete: attributed.complete, Title: payload["title"], URL: payload["url"],
		EventAt: envelopeTime(payload["updated_at"]), CheckedAt: checkedAt,
		MergeCommitSHA: payload["merge_commit_sha"], MergedAt: envelopeTime(payload["merged_at"]),
	}, given)
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
