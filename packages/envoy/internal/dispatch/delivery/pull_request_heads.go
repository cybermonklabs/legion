package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

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
// the merged population delivery_pull_requests keeps. pull_request_texts holds the facts GitHub
// answered with that head: the head repository, the base, the title, the body, and the commit
// messages where a read stored them. No write stores an issue. One statement gives the issue at
// each read, from those facts (attributedTerms): the first issue its ranked sources name that
// resolves to the repository the pull request is in, and only where the head branch is in that
// same repository, never a fork's. A child's pull request is attributed to the child, so it
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

// PullRequestHead is one pull request's current head, as pull_request_heads stores it, and the
// one shape every writer of that row fills in (UpsertPullRequestHead).
type PullRequestHead struct {
	Repo    string
	Number  int
	HeadSHA string
	HeadRef string
	BaseRef string
	State   string
	Title   string
	URL     string
	EventAt *time.Time
	// CheckedAt is when a read of GitHub last read every source of the pull request's issue that
	// it needs. A write with none keeps the stored value.
	CheckedAt *time.Time
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
	// Texts is what the same read of GitHub answered about the pull request besides its head: the
	// facts the statement reads its issue from. A nil value means that this write read no texts,
	// and leaves the stored texts row as it is.
	Texts *PullRequestTexts
	// MergeCommitSHA is the commit GitHub made when this pull request merged. It is set only
	// for a merged pull request, and only by a writer that read it; one that did not read it
	// leaves it empty, which asks for nothing. It is written to no column of
	// pull_request_heads: it exists to seed branch_commits with the one fact the merge itself
	// proves, that the base branch held this commit (UpsertPullRequestHead).
	MergeCommitSHA string
}

// PullRequestTexts is what a read of GitHub answered about a pull request besides its head row:
// the facts the statement reads the pull request's issue from (attributedTerms). HeadRepo is the
// repository the head branch is in, "" where GitHub names none (a deleted fork). BaseRef is the
// base branch. CommitMessages are the messages of the head's commits. MessagesRead is false where
// no read stored them, and the texts row then stores commit_messages null. The head commit and
// its branch come from the head row itself.
type PullRequestTexts struct {
	HeadRepo, BaseRef, Title, Body string
	CommitMessages                 []string
	MessagesRead                   bool
}

// freeText is s with each byte that is not UTF-8, and each NUL, written as U+FFFD. Postgres
// stores no NUL in a text value (SQLSTATE 22021), and a title, a body or a commit message can hold
// one. The reading reads U+FFFD as it reads a space, so the keys of the stored text are the keys
// of GitHub's text with each NUL read as a separator.
func freeText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "\uFFFD")
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
// the repository: the head commit (pull_request_heads_issue_head, pull_request_heads_repo_head),
// the base branch and the merge commit, which a merge writes into branch_commits' primary key
// (repo, branch, sha), and the URL, whose lower() pull_request_heads_url_lower holds. It is so
// much that no commit GitHub names (40 or 64 hexadecimal digits) and no branch GitHub keeps comes
// near it, and so little that the widest of those entries, with a repository of 140 bytes and a
// branch and a commit of 1,024 each, stays under the btree's 2,704 bytes (SQLSTATE 54000 past
// it). The lower() of a URL can be half as long again in bytes, and 1,536 bytes stays under that
// limit too. No index holds the head branch or the title, so neither is bounded.
const headRefBound = 1024

// headValue is one value of identity of a head row and its texts, as checkHeadRow checks it:
// bounded where an index holds it.
type headValue struct {
	name, text string
	bounded    bool
}

// checkHeadRow is the one check of a head row and its texts, made where every writer's row is
// written (UpsertPullRequestHead), before any statement: no caller writes a value it did not
// check, and no failure of a statement is read as the values' own. It answers the row the write
// stores, or names the first value that no row holds.
//
// A text of free form (the title, and the title, the body and each commit message of the texts)
// goes through freeText, and the answered row holds the result: such a text fails no write. The
// repository and the number key the row and the read its triggers owe (pull_request_heads_older_read,
// pull_request_heads_unread_write), so they must be a pull request GitHub can have
// (checkPullRequestKey, errNoPullRequestNamed): the repository is a canonical name, which
// repo_projects gives and no envelope does. Each other value of identity (the head commit and
// branch, the base branch, the merge commit, the URL, and the head repository and base branch of
// the texts) must be UTF-8 that holds no NUL, and each value an index holds must have at most
// headRefBound bytes. The state must be one the column's check takes. Each of those is
// errNoRowHolds.
func checkHeadRow(head PullRequestHead) (PullRequestHead, error) {
	if err := checkPullRequestKey(head.Repo, head.Number); err != nil {
		return PullRequestHead{}, err
	}
	switch head.State {
	case "open", "closed", "merged":
	default:
		return PullRequestHead{}, fmt.Errorf("%w: the state of %s#%d is %.40q, not open, closed or merged", errNoRowHolds, head.Repo, head.Number, head.State)
	}
	values := []headValue{
		{"head commit", head.HeadSHA, true}, {"head branch", head.HeadRef, false},
		{"base branch", head.BaseRef, true}, {"merge commit", head.MergeCommitSHA, true},
		{"URL", head.URL, true},
	}
	if head.Texts != nil {
		values = append(values, headValue{"head repository", head.Texts.HeadRepo, false},
			headValue{"base branch of the texts", head.Texts.BaseRef, true})
	}
	for _, value := range values {
		switch {
		case !utf8.ValidString(value.text) || strings.ContainsRune(value.text, 0):
			return PullRequestHead{}, fmt.Errorf("%w: the %s of %s#%d is not UTF-8 without NUL bytes", errNoRowHolds, value.name, head.Repo, head.Number)
		case value.bounded && len(value.text) > headRefBound:
			return PullRequestHead{}, fmt.Errorf("%w: the %s of %s#%d is %d bytes, over %d", errNoRowHolds, value.name, head.Repo, head.Number, len(value.text), headRefBound)
		}
	}
	head.Title = freeText(head.Title)
	if head.Texts != nil {
		texts := freeTexts(*head.Texts)
		head.Texts = &texts
	}
	return head, nil
}

// freeTexts is texts with each text of free form through freeText, in a copy of its own: the
// messages a write stores where they are read, and none where they are not.
func freeTexts(texts PullRequestTexts) PullRequestTexts {
	texts.Title, texts.Body = freeText(texts.Title), freeText(texts.Body)
	if !texts.MessagesRead {
		texts.CommitMessages = nil
		return texts
	}
	messages := make([]string, len(texts.CommitMessages))
	for i, message := range texts.CommitMessages {
		messages[i] = freeText(message)
	}
	texts.CommitMessages = messages
	return texts
}

// UpsertPullRequestHead writes one pull request's head row, and its texts row where head.Texts is
// set. An event older than the one the row already holds changes nothing: GitHub redelivers and
// the reconcile reads concurrently, so the row keeps the newest `updated_at` either has seen rather
// than whichever write landed last. Of two answers of one `updated_at` second, which is all
// GitHub's time tells apart, the row keeps the one whose read of GitHub began later (GitHubReadAt,
// upsertHeadRow), and a pair that differs owes a read. Every writer of this build carries GitHub's
// own `updated_at` for the pull request, so a write with no event time is one whose GitHub answer
// carried none; it always applies, as does one landing on a row that has none.
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
// The write stores facts and no issue. An update names neither issue_key nor attribution_source
// nor attribution_rule. An insert writes issue_key from the fallback of the statement for the same
// pull request, the answer every reader gave before the row existed (R4): the issue of its results
// row under another name of the repository, the row under the canonical name first, else the issue
// of its timeline row, else none (upsertHeadRow). The statement reads that stored issue only for a
// pull request with no current texts.
//
// The texts row is written in the same transaction as the results row, with the row's head, branch,
// title and updated_at, and only where the write of the results row applied: a write the event-time
// guard refused leaves both rows as they are, so the stored texts stay those of the stored head. A
// write of texts clears the row's attribution_refused_at and attribution_absent_at, since it is a
// read of the facts that a refusal or a proof of absence no longer describes.
//
// A write carrying no read time leaves the row's checked_at as it was: nulling it would hand a
// row the reconcile has just read back to the next batch (ListStalePullRequestHeads orders the
// never-read first).
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
// The head row, its texts and that seed are written in one transaction, so they land together or
// none does and the caller is told. branch.go logs its own failed writes and carries on,
// which is right for every answer it writes: those are answers a compare read can earn again a
// moment later. This one can never be earned again — the paragraph above is the whole argument
// that nothing proves it once the base branch is gone — so a seed lost behind a Warn line
// leaves every `after_merge` result of that issue standing at no basis for good, with the
// merged head row stored beside it as if all was well. Failing the write instead puts that in
// front of the caller: the reconcile logs it and leaves the row for its next pass
// (reconcilePullRequestHeads), and the results read's read-through hands it back to the route
// (ReadThroughCommitPulls). The statements are `on conflict … do update`, so a write made
// again writes the same rows and costs nothing.
//
// A write with no texts and nothing to pair — a fixture's row, a merge whose base branch or merge
// commit nobody read — runs its one statement on the pool. A transaction around a single statement
// buys nothing and costs a round trip at each end.
//
// Every error this function answers is ErrHeadNotStored: nothing of the rows and the seed was
// written, and the caller may write them again. A value no row holds (errNoRowHolds, checkHeadRow)
// is one of them, refused before any statement: a later read of GitHub may answer values the row
// holds. The one exception is errNoPullRequestNamed, a key that is no pull request GitHub can
// have, also refused before any statement: nothing was written either, and no attempt with the
// same key gets past it.
func UpsertPullRequestHead(ctx context.Context, pool *store.Pool, head PullRequestHead) error {
	_, err := upsertHead(ctx, pool, head)
	return err
}

// upsertHead is UpsertPullRequestHead, and it answers whether the write of the results row
// applied (upsertHeadRow): false where the event-time guard or the order of two reads of one
// second kept the stored row, and so its texts.
func upsertHead(ctx context.Context, pool *store.Pool, head PullRequestHead) (bool, error) {
	head, err := checkHeadRow(head)
	if err != nil {
		return false, err
	}
	seedsBranch := head.State == "merged" && head.BaseRef != "" && head.MergeCommitSHA != ""
	if !seedsBranch && head.Texts == nil {
		return upsertHeadRow(ctx, pool, head)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: open the write of the head of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
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
	applied, err := upsertHeadRow(ctx, tx, head)
	if err != nil {
		return false, err
	}
	if applied && head.Texts != nil {
		if err := upsertTexts(ctx, tx, head); err != nil {
			return false, fmt.Errorf("%w: %w", ErrHeadNotStored, err)
		}
	}
	if seedsBranch {
		if err := writeBranchCommit(ctx, tx, head.Repo, head.BaseRef, head.MergeCommitSHA, true, time.Now()); err != nil {
			return false, fmt.Errorf("%w: %w", ErrHeadNotStored, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("%w: store the head of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
	}
	return applied, nil
}

// upsertTexts writes the texts row of head, which checkHeadRow answered, in the transaction of
// its results row: GitHub's facts of the head it describes, and its commit messages where they
// were read (null where not). The trigger pull_request_texts_keys writes the keys and the reading
// of the row from those texts in the same statement.
func upsertTexts(ctx context.Context, tx refs.Execer, head PullRequestHead) error {
	if _, err := tx.Exec(ctx, `
		insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		on conflict (repo, number) do update set
			head_sha = excluded.head_sha, head_ref = excluded.head_ref, head_repo = excluded.head_repo,
			base_ref = excluded.base_ref, title = excluded.title, body = excluded.body,
			commit_messages = excluded.commit_messages, updated_at = excluded.updated_at,
			read_at = clock_timestamp()
	`, head.Repo, head.Number, head.HeadSHA, head.HeadRef, head.Texts.HeadRepo, head.Texts.BaseRef,
		head.Title, head.Texts.Body, head.Texts.CommitMessages, head.EventAt); err != nil {
		return fmt.Errorf("store the texts of %s#%d: %w", head.Repo, head.Number, err)
	}
	return nil
}

// AttributionEventType is the event that records a change of a pull request's issue: one on the
// issue the pull request left and one on the issue it reached, each carrying the same payload, so
// either issue's timeline shows the whole move. A pull request that reaches an issue from nobody
// has one event, on the issue it reached, and one that is uncredited has one on the issue it left.
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

// headWriteRollback bounds the rollback of that transaction, which runs on a context of its
// own and so is bounded by nothing else. Two seconds is longer than the one statement each
// rollback here can be undoing and far inside any caller's own patience.
const headWriteRollback = 2 * time.Second

// upsertHeadRow is the head row's own statement, which UpsertPullRequestHead's doc comment
// explains in full: it is here, on a refs.Execer — the pool, or a transaction already open on
// it — so that one statement runs on the pool where it is the whole write and inside the
// transaction where the texts, the merge's branch answer, or both go with it.
//
// It answers whether the write applied: false where the event-time guard below, or the order of
// two reads of one second (below), left the row exactly as it was, which are the two ways this
// statement writes nothing and reports no error. Only an applied write writes the texts.
//
// An insert writes the fallback of the statement as the row's issue_key (UpsertPullRequestHead):
// the issue of a results row of the same pull request under another name of the repository, the
// row under the canonical name first and then the one read from GitHub last, else the issue of its
// timeline row, the row under the canonical name first. Each name is read through the renaming
// rule (refs.CanonicalRepoSQL), as the statement reads it (attributedTerms). An update names no
// issue. A write of texts clears the refusal and the proof of absence of a read of the facts
// ($13).
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
// title or the URL) owe a read whichever of them the row keeps, and the next read writes what
// GitHub holds then. The rule and
// the owe are the trigger's, and not this statement's `where`, because a row the `where` refuses
// fires no trigger, so nothing could owe the read of the answer it refused; and the trigger
// compares with the row the update replaces (OLD), which nothing else in the statement sees: a
// CTE reads the statement's snapshot, and a CTE's `for update` skips a row the statement's own
// update has changed.
func upsertHeadRow(ctx context.Context, db refs.Execer, head PullRequestHead) (bool, error) {
	tag, err := db.Exec(ctx, `
		insert into pull_request_heads (
			repo, number, head_sha, head_ref, base_ref, state, issue_key,
			title, url, event_at, checked_at, github_read_at, github_read_order
		)
		select $1::text, $2::integer, $3::text, $4::text, $5::text, $6::text, f.issue_key,
			$7::text, $8::text, $9::timestamptz, $10::timestamptz, $11::timestamptz, $12::bigint
		from (
			select case when o.repo is not null then o.issue_key else d.issue_key end as issue_key
			from (select 1) one
			left join lateral (
				select h.repo, h.issue_key from pull_request_heads h
				where h.number = $2::integer and h.repo <> $1::text
					and lower(`+refs.CanonicalRepoSQL("h.repo")+`) = lower(`+refs.CanonicalRepoSQL("$1::text")+`)
				order by lower(h.repo) = lower(`+refs.CanonicalRepoSQL("$1::text")+`) desc, h.github_read_at desc nulls last, h.repo
				limit 1) o on true
			left join lateral (
				select t.issue_key from delivery_pull_requests t
				where t.number = $2::integer
					and lower(`+refs.CanonicalRepoSQL("t.repo")+`) = lower(`+refs.CanonicalRepoSQL("$1::text")+`)
				order by lower(t.repo) = lower(`+refs.CanonicalRepoSQL("$1::text")+`) desc, t.repo
				limit 1) d on true
		) f
		on conflict (repo, number) do update set
			head_sha = excluded.head_sha,
			head_ref = excluded.head_ref,
			base_ref = excluded.base_ref,
			state = excluded.state,
			title = excluded.title,
			url = excluded.url,
			event_at = coalesce(excluded.event_at, pull_request_heads.event_at),
			checked_at = coalesce(excluded.checked_at, pull_request_heads.checked_at),
			github_read_at = excluded.github_read_at,
			github_read_order = excluded.github_read_order,
			attribution_refused_at = case when $13::boolean then null else pull_request_heads.attribution_refused_at end,
			attribution_absent_at = case when $13::boolean then null else pull_request_heads.attribution_absent_at end
		where excluded.event_at is null
		   or pull_request_heads.event_at is null
		   or excluded.event_at >= pull_request_heads.event_at
		   or (excluded.github_read_at is not null and pull_request_heads.github_read_at is null)
	`,
		head.Repo, head.Number, head.HeadSHA, head.HeadRef, head.BaseRef, head.State,
		head.Title, head.URL, head.EventAt, head.CheckedAt, head.GitHubReadAt, head.GitHubReadOrder,
		head.Texts != nil,
	)
	if err != nil {
		return false, fmt.Errorf("%w: store the head of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
	}
	return tag.RowsAffected() > 0, nil
}

// upsertHeadUnderCanonicalName is the one writer of a head row and its texts. It writes head,
// which every caller has already keyed by the canonical name of its repository
// (refs.CanonicalRepo), and drops the row the same pull request is stored under as storedUnder
// when that is another name. A row keyed under a repository's old name can be read at all only
// under the canonical one — the App answers 404 for the old name's installation, so nothing can
// refresh it and no issue is ever held to it (attributionHeld) — so one pull request keeps one row.
//
// Where both rows exist, because the pull request arrived under both names, the canonical row
// wins and the old row goes: the write above lands on the canonical row and its own event-time
// rule decides which facts stand there (UpsertPullRequestHead keeps the newest event either has
// seen), and the old row's facts are dropped with it rather than merged, since they are the same
// pull request read at some earlier moment. Its texts row goes with it (on delete cascade).
//
// The write comes before the delete, so a failure between them leaves two rows rather than
// none: the next pass is handed the old row's key again and re-keys it again, while losing the
// row would leave the pull request unknown until another event arrived. The insert of the
// canonical row reads the old row's issue as its fallback before the delete. It answers whether
// the write of the canonical row applied (upsertHead).
func upsertHeadUnderCanonicalName(ctx context.Context, pool *store.Pool, head PullRequestHead, storedUnder string) (bool, error) {
	applied, err := upsertHead(ctx, pool, head)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(storedUnder, head.Repo) {
		return applied, nil
	}
	if _, err := pool.Exec(ctx, `
		delete from pull_request_heads where repo = $1 and number = $2
	`, storedUnder, head.Number); err != nil {
		return false, fmt.Errorf("drop the head of %s#%d, now %s#%d: %w", storedUnder, head.Number, head.Repo, head.Number, err)
	}
	return applied, nil
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

// HeadStoredAt reports whether sha is the head of an open or merged pull request stored for
// repoFull, whatever issue it is attributed to: whether Dispatch holds a pull request at that
// commit. It is a weaker question than HeadAttributedTo's, and it is asked for another purpose:
// the results read asks it before it reads GitHub for a commit (api/results_comparison.go,
// comparisonReader.readThroughHead). A read from there repairs nothing for such a commit. The
// statement gives the row's issue from its stored facts at each read, with the links, issues,
// parents and mappings as they stand then, and the reconcile reads an open row again.
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
func HeadStoredAt(ctx context.Context, pool *store.Pool, repoFull, sha string) (bool, error) {
	var found bool
	if err := pool.QueryRow(ctx, `
		select exists (
			select 1 from pull_request_heads
			where lower(repo) = lower($1) and head_sha = $2 and state in ('open', 'merged')
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
// pass never reads more rows than it did before. The rule half takes no row the first read can
// take (a merged row with checked_at null), so no row is listed twice and read twice at once in
// one pass: every write of a merged row that a read of this build makes leaves attribution_rule
// at 0, so such a row is in both halves otherwise.
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
		where state = 'merged' and attribution_rule < $1 and checked_at is not null
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
// the write that added or removed those links of an issue (issue_external_links). No head write
// reads the mark: the statement reads the links at each read, so a link reaches every reader of
// a pull request with current texts at once. The mark hands the row to the next reconcile pass
// (ListStalePullRequestHeads), which reads the facts of a row that has none. The mark owes no
// read (pull_request_reads_owed): the head is not in doubt, and neither trigger of the table owes
// anything for it. A URL that is no stored pull request's marks nothing. The URLs are compared
// lowercased, as the statement compares them, and the rows are locked in (repository, number)
// order.
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

// headRow is the row one pull request GitHub answered makes: the one builder every GitHub reader
// below writes through, so they cannot drift. The list form of a pull request carries merged_at
// but not merged, so a merge is read from whichever of the two GitHub sent. The row is marked a
// read of GitHub that took its start read (GitHubReadAt and GitHubReadOrder) where GitHub's answer
// carried its updated_at, since the mark says the row's time is GitHub's own; each reader takes
// read before its request goes out (startRead).
func headRow(repoFull string, payload pullRequestPayload, checkedAt *time.Time, read readStart) PullRequestHead {
	row := PullRequestHead{
		Repo: repoFull, Number: payload.Number, HeadSHA: payload.Head.SHA, HeadRef: payload.Head.Ref,
		BaseRef: payload.Base.Ref, State: PullRequestHeadState(payload.State, payload.Merged || payload.MergedAt != nil),
		Title: payload.Title, URL: payload.HTMLURL, EventAt: payload.UpdatedAt, CheckedAt: checkedAt,
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

// FetchedFacts is one read of a pull request from GitHub, as RefreshPullRequestHead stored it. A
// batched read gives the same facts from its query, which its caller stores (readFactsBatch):
// its Payload holds the fields that the query reads, and its Texts are the texts to store.
type FetchedFacts struct {
	Canonical   string             // the repository's canonical name, which the row is stored under
	Owner, Repo string             // Canonical split
	Token       string             // the installation token of the read
	Payload     pullRequestPayload // GitHub's answer to GET /repos/{o}/{r}/pulls/{n}
	Texts       *PullRequestTexts  // the texts stored, with MessagesRead false where the messages are not read
}

// storePullRequestFacts stores what GitHub answered about one pull request in payload: its
// results row under canonical, its texts row, and its commit messages where they decide. Every
// reader of GitHub stores a pull request through it, and it writes through the one writer
// (upsertHeadUnderCanonicalName). It takes four steps:
//
//  1. It builds the texts from the payload. A fork's head reads no messages, since a fork's pull
//     request is nobody's whatever its commits say: they are read and empty. Else it reuses the
//     stored messages where the stored texts hold them for the payload's head and base. Else the
//     messages are not read.
//  2. It writes the row and its texts, and drops the row under storedUnder where that is another
//     name. Where the event-time guard keeps the stored row (a newer answer), the write stores no
//     texts and the read ends here: the stored texts describe another head.
//  3. Where the messages are not read, it reads the statement's answer for the pull request
//     (PullRequestIssues). Where that answer has no facts, no higher source names a held issue:
//     it pages the messages with token, and writes the texts again with them under canonical.
//     That is the rule of the release before: a writer reads the messages only where the title,
//     the body, the branch and the links name nobody the pull request may keep.
//  4. A rate limit of that read is returned. Any other failure of it leaves the messages not read:
//     the answer is not known until a later read stores them.
//
// checkedAt is the time of the read, and read is the place the read took before its request went
// out (startRead). A read that read every source it needs stamps the row with checkedAt. One whose
// read of the messages failed keeps the stamp the row had, so the reconcile reads it again, unless
// stampIncomplete is set: the reconcile's refresh is itself the read that comes again, so it
// stamps each read. bounded bounds each write by owedTableBound, as the intake bounds each of its
// statements that can wait on a lock of pull_request_reads_owed (the trigger
// pull_request_heads_older_read owes a read there).
//
// It answers the texts of GitHub's answer as the write stores them (freeTexts), with the messages
// it stored or reused, and MessagesRead false where it has none. A write the guard refused answers
// them too, though it stored none.
func storePullRequestFacts(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, token, canonical, storedUnder string,
	payload pullRequestPayload, checkedAt *time.Time, read readStart, stampIncomplete, bounded bool,
) (*PullRequestTexts, error) {
	texts := &PullRequestTexts{HeadRepo: headRepositoryOf(payload), BaseRef: payload.Base.Ref, Title: payload.Title, Body: payload.Body}
	if !headInOwnRepository(canonical, texts.HeadRepo) {
		texts.CommitMessages, texts.MessagesRead = []string{}, true
	} else {
		stored, found, err := storedMessages(ctx, pool, canonical, payload.Number, payload.Head.SHA, payload.Base.Ref)
		if err != nil {
			return nil, err
		}
		if found {
			texts.CommitMessages, texts.MessagesRead = stored, true
		}
	}
	head := headRow(canonical, payload, checkedAt, read)
	head.Texts = texts
	if !texts.MessagesRead && !stampIncomplete {
		head.CheckedAt = nil
	}
	applied, err := writeFacts(ctx, pool, head, storedUnder, bounded)
	if err != nil {
		return nil, err
	}
	answer := func() *PullRequestTexts {
		stored := freeTexts(*texts)
		return &stored
	}
	if texts.MessagesRead || !applied {
		return answer(), nil
	}
	key := PullRequestKey{Repo: canonical, Number: payload.Number}
	answers, err := PullRequestIssues(ctx, pool, []PullRequestKey{key})
	if err != nil {
		return answer(), err
	}
	if answers[key].HasFacts {
		// A higher source decides, so the read read every source it needs.
		if stampIncomplete || checkedAt == nil {
			return answer(), nil
		}
		return answer(), stampHeadRead(ctx, pool, head, *checkedAt, bounded)
	}
	owner, repo, err := splitRepo(canonical)
	if err != nil {
		return answer(), err
	}
	messages, _, readErr := fetchCommitMessagesWithToken(ctx, client, func() (string, error) { return token, nil }, owner, repo, payload.Number, 1)
	if readErr != nil {
		if limited, ok := githubapp.AsRateLimit(readErr); ok {
			return answer(), limited
		}
		logging.FromContext(ctx).Debug("dispatch delivery: the commit messages of a pull request are not read, so its answer is not known until a later read",
			"repo", canonical, "number", payload.Number, "error", readErr)
		return answer(), nil
	}
	if messages == nil {
		messages = []string{}
	}
	texts.CommitMessages, texts.MessagesRead = messages, true
	head.CheckedAt = checkedAt
	if _, err := writeFacts(ctx, pool, head, canonical, bounded); err != nil {
		return answer(), err
	}
	return answer(), nil
}

// storedMessages is the commit messages the stored texts of repo's pull request number hold for
// the head headSHA on the base baseRef, and found false where they hold none for that pair.
func storedMessages(ctx context.Context, pool *store.Pool, repo string, number int, headSHA, baseRef string) (messages []string, found bool, err error) {
	err = pool.QueryRow(ctx, `
		select commit_messages from pull_request_texts
		where repo = $1 and number = $2 and head_sha = $3 and base_ref = $4 and commit_messages is not null
	`, repo, number, headSHA, baseRef).Scan(&messages)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the stored commit messages of %s#%d: %w", repo, number, err)
	}
	if messages == nil {
		messages = []string{}
	}
	return messages, true, nil
}

// writeFacts is the write of head and its texts (upsertHeadUnderCanonicalName), bounded by
// owedTableBound where bounded is set (storePullRequestFacts). It answers whether the write of
// the results row applied.
func writeFacts(ctx context.Context, pool *store.Pool, head PullRequestHead, storedUnder string, bounded bool) (bool, error) {
	if bounded {
		writing, cancel := owedTableBound(ctx)
		defer cancel()
		return upsertHeadUnderCanonicalName(writing, pool, head, storedUnder)
	}
	return upsertHeadUnderCanonicalName(ctx, pool, head, storedUnder)
}

// stampHeadRead stamps the row of head with at, where the row still holds the answer that head
// wrote: a read that did not page the messages, because a higher source decides, read every
// source it needs. A row a later answer replaced in between keeps the stamp of that answer.
func stampHeadRead(ctx context.Context, pool *store.Pool, head PullRequestHead, at time.Time, bounded bool) error {
	if bounded {
		var cancel context.CancelFunc
		ctx, cancel = owedTableBound(ctx)
		defer cancel()
	}
	if _, err := pool.Exec(ctx, `
		update pull_request_heads set checked_at = $3
		where repo = $1 and number = $2 and head_sha = $4 and event_at is not distinct from $5
	`, head.Repo, head.Number, at, head.HeadSHA, head.EventAt); err != nil {
		return fmt.Errorf("%w: stamp the read of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
	}
	return nil
}

// RefreshPullRequestHead reads one pull request through the App and stores its facts
// (storePullRequestFacts): the reconcile's per-row step. It answers what it fetched and stored,
// nil where GitHub refused the read, and GitHub's refusal to give the pull request, nil where GitHub
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
// PullRequestHeadStaleAfter rather than holding the front of every batch.
//
// repoFull is the key ListStalePullRequestHeads read off the row, which may be a name the
// repository has been renamed away from, and every step here but the stamp works on the
// canonical name instead (refs.CanonicalRepo): the token is minted for it, since GitHub answers
// 404 for the installation of a name no longer in use; and the row is written under it, with the
// row under the old key dropped in the same step (upsertHeadUnderCanonicalName). The stamp stays
// on the key the batch handed over, since that is the row the batch must stop offering: a
// canonical repository the App cannot read teaches this row nothing under either name.
//
// installations is the pass's lookups of the installation of each repository
// (passInstallations), nil for a caller that has no pass.
func RefreshPullRequestHead(
	ctx context.Context, pool *store.Pool, client *githubapp.Client,
	installations *passInstallations, repoFull string, number int, now time.Time,
) (fetched *FetchedFacts, refused *pullRequestRefusal, err error) {
	canonical, err := refs.CanonicalRepo(ctx, pool, repoFull)
	if err != nil {
		return nil, nil, err
	}
	owner, repo, err := splitRepo(canonical)
	if err != nil {
		return nil, nil, err
	}
	token, err := installations.repositoryToken(ctx, client, owner, repo)
	if err != nil {
		if repositoryUnreadable(err) {
			return nil, &pullRequestRefusal{err: err, owner: owner, repo: repo, number: number}, MarkPullRequestHeadChecked(ctx, pool, repoFull, number, now)
		}
		return nil, nil, fmt.Errorf("mint installation token for %s PR #%d: %w", canonical, number, err)
	}
	began, err := startRead(ctx, pool)
	if err != nil {
		return nil, nil, err
	}
	payload, err := fetchPullRequestPayload(ctx, client, token, owner, repo, number)
	if err != nil {
		if errors.Is(err, ErrPullRequestNotFound) || repositoryUnreadable(err) {
			return nil, &pullRequestRefusal{err: err, owner: owner, repo: repo, number: number, token: token}, MarkPullRequestHeadChecked(ctx, pool, repoFull, number, now)
		}
		return nil, nil, err
	}
	texts, err := storePullRequestFacts(ctx, pool, client, token, canonical, repoFull, payload, &now, began, true, false)
	if err != nil {
		return nil, nil, err
	}
	return &FetchedFacts{Canonical: canonical, Owner: owner, Repo: repo, Token: token, Payload: payload, Texts: texts}, nil, nil
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
// (`GET /repos/{o}/{r}/commits/{sha}/pulls`) and stores each one's facts, so a result recorded
// at the head of a pull request no event has reached Dispatch for stands at it at once.
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
// Each pull request of the list is stored as every reader of GitHub stores one
// (storePullRequestFacts), from the list's own answer: the one list read is the only read of
// GitHub's pull requests it makes, and it pages a pull request's commit messages only where no
// higher source names a held issue. A row whose commit messages could not be read keeps the stamp
// it had, and the reconcile reads it again rather than this read-through standing as the last
// word.
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
func ReadThroughCommitPulls(
	ctx context.Context, pool *store.Pool, client *githubapp.Client,
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
		if _, err := storePullRequestFacts(ctx, pool, client, token, canonical, repoFull, pull, &now, began, false, false); err != nil {
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
// /repos/{owner}/{repo}/pulls/{number}` (fetchPullRequestPayload), stored as the reconcile
// stores a read (storePullRequestFacts). Two reasons, each
// enough alone. An envelope carries no state, only the action, and GitHub sends `labeled`,
// `edited` and `assigned` for a closed pull request too, so a state read from the action stored
// a closed pull request as open again and a pass at its head stood. And an envelope on a
// `notifications.github.` subject is not proof GitHub sent it: any Envoy /v1 bearer can publish
// one, which is why handleWorkflowEnvelope reads its run from GitHub too, and a head a caller
// set would be what a `before_merge` pass is judged against.
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
	// The write waits on a lock of pull_request_reads_owed no longer than the owe beside it does
	// (owedTableBound): its trigger owes a read where two answers of one GitHub second differ, or
	// where the write replaces a newer row no read of GitHub wrote (pull_request_heads_older_read).
	// A write that waits past the bound is a head not stored, so Intake.deliver owes the read, or
	// hands the envelope back while that owe cannot be written either; on the message's own
	// deadline it held a delivery, and one of the slots its repository shares, for that whole
	// minute. A read whose commit messages could not be had keeps the stamp the row had, so the
	// reconcile's next pass reads it again.
	now := time.Now()
	texts, err := storePullRequestFacts(ctx, in.pool, in.github, token, repoFull, given, pull, &now, began, false, true)
	read := headRead{pull: &pull, token: token}
	if texts != nil && texts.MessagesRead && headInOwnRepository(repoFull, texts.HeadRepo) {
		read.messages = texts.CommitMessages
	}
	if err != nil {
		if errors.Is(err, ErrHeadNotStored) || errors.Is(err, errNoPullRequestNamed) {
			return read, err
		}
		return headRead{}, markHeadNotStored(err)
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
// GitHub answered and the token it was read with, the commit messages the write stored for its
// head, and GitHub's lasting refusal to answer — a pull request it does not have, or a repository
// the App cannot read. All are empty where the read was spared (headReadCovers).
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
