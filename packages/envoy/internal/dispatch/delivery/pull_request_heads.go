package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
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
// messages where a read stored them. No writer of those facts decides an issue. One statement
// gives the issue at each read, from those facts (attributedTerms): the issue that links the pull
// request's own URL, in any repository, and else the first issue its other ranked sources name
// that resolves to the repository the pull request is in; and only where the head branch is in
// the pull request's own repository, never a fork's. Where the commit messages are not read and
// no higher source decides, the issue is not known. Where no texts describe the stored head, the
// issue is the one the results row stores (R4's fallback). A child's pull request is attributed
// to the child, so it answers for the child's own checks alone and never for a check of an issue
// above it.
//
// Three writers keep it, and each writes what GitHub itself answers about the pull request: the
// `pull_request` envelope's handler (intake.go, every action), which takes from the envelope only
// which pull request changed and reads that pull request from GitHub (recordPullRequestHead); the
// read-through made for a result's commit (ReadThroughCommitPulls) — by a recorded result, and by
// the read of an issue's results for each `before_merge` result's commit, at most once every
// CommitPullReadStaleAfter — and the reconcile's five-minute pass (reconcile.go),
// which reads the pull requests owed a read (pull_request_reads_owed.go), every failure before any
// refusal, creating each one's row where none exists, then reads the open rows read longer ago
// than PullRequestHeadStaleAfter (every open row at every pass of one process, as far as the batch
// reaches, and as PullRequestHeadStaleAfter says with several; ListStalePullRequestHeads), and
// then reads the facts line (Reconcile.readFactsLine): each open or merged results row whose texts
// do not describe its head or whose commit messages are not read, and each timeline row with no
// results row. Those two arms read GitHub by one batched GraphQL query (readFactsBatch). Every
// writer writes a row through one function (upsertHeadUnderCanonicalName).
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
	// CheckedAt is when a read of GitHub last read the pull request. A read of the intake or of
	// the read-through sets it only where it read every source of the issue that it needs. A read
	// of the reconcile sets it at each read, because the reconcile's read comes again
	// (storePullRequestFacts, readFactsBatch), and a refused read of the reconcile stamps it too
	// (MarkPullRequestHeadChecked). A write with none keeps the stored value.
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
// they all end the same way: a read of Postgres (the stored row recordPullRequestHead compares
// the event with, and the stored commit messages and the statement's answer that
// storePullRequestFacts reads), a read of GitHub that failed (a 5xx, a network failure, a rate
// limit), and a GitHub answer that holds a value no head row holds (errNoRowHolds). The
// repository's canonical name is no read (Attributor.canonical, over the pairs the boot read).
// GitHub's refusals are not marked: a pull request it does not know and a repository the App
// cannot read write no row, are owed a read the reconcile makes, and are acknowledged
// (recordPullRequestHead).
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
// and the base branch and the merge commit, which a merge writes into branch_commits' primary key
// (repo, branch, sha). It is so much that no commit GitHub names (40 or 64 hexadecimal digits) and
// no branch GitHub keeps comes near it, and so little that the widest of those entries, with a
// repository of 140 bytes and a branch and a commit of 1,024 each, stays under the btree's 2,704
// bytes (SQLSTATE 54000 past it). No index holds the head branch, the title or the URL, so none of
// them is bounded here: each is a text column, as long as the GitHub answer it came from, and
// githubapp reads an answer of at most 1 MiB (responseLimit), the REST read of one pull request
// and the GraphQL batch alike.
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
// repository and the number key the row and the read its triggers owe
// (pull_request_heads_older_read, pull_request_heads_unread_write), so they must be a pull request
// GitHub can have (checkPullRequestKey, errNoPullRequestNamed): the repository is a canonical name,
// which the rename pairs the boot read give and no envelope does. Each other value of identity
// (the head commit and branch, the base branch, the merge commit, the URL, and the head repository
// and base branch of the texts) must be UTF-8 that holds no NUL, and each value an index holds
// must have at most headRefBound bytes. The state must be one the column's check takes. Each of
// those is errNoRowHolds.
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
		{"URL", head.URL, false},
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
func (a *Attributor) UpsertPullRequestHead(ctx context.Context, pool *store.Pool, head PullRequestHead) error {
	_, err := a.upsertHead(ctx, pool, head)
	return err
}

// upsertHead is UpsertPullRequestHead, and it answers whether the write of the results row
// applied (upsertHeadRow): false where the event-time guard or the order of two reads of one
// second kept the stored row, and so its texts. db is the pool, or a transaction a read-only
// read rolls back (HeadAnswers), where the write's own transaction is a savepoint.
func (a *Attributor) upsertHead(ctx context.Context, db refs.Pool, head PullRequestHead) (bool, error) {
	head, err := checkHeadRow(head)
	if err != nil {
		return false, err
	}
	seedsBranch := head.seedsBranch()
	if !seedsBranch && head.Texts == nil {
		return a.upsertHeadRow(ctx, db, head)
	}
	tx, err := db.Begin(ctx)
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
	applied, err := a.upsertHeadRow(ctx, tx, head)
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

// seedsBranch is whether writing head records its merge commit as on its base branch, for good:
// the rule UpsertPullRequestHead's doc comment argues, by which upsertHead writes the
// branch_commits row in the head's own transaction.
func (head PullRequestHead) seedsBranch() bool {
	return head.State == "merged" && head.BaseRef != "" && head.MergeCommitSHA != ""
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
// rule (refs.CanonicalRepoSQL over the pairs the boot read), as the statement reads it
// (attributedTerms). An update names no issue. A write of texts clears the refusal and the proof
// of absence of a read of the facts ($13).
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
func (a *Attributor) upsertHeadRow(ctx context.Context, db refs.Execer, head PullRequestHead) (bool, error) {
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
					and lower(`+refs.CanonicalRepoSQL(a.renames, "h.repo")+`) = lower(`+refs.CanonicalRepoSQL(a.renames, "$1::text")+`)
				order by lower(h.repo) = lower(`+refs.CanonicalRepoSQL(a.renames, "$1::text")+`) desc, h.github_read_at desc nulls last, h.repo
				limit 1) o on true
			left join lateral (
				select t.issue_key from delivery_pull_requests t
				where t.number = $2::integer
					and lower(`+refs.CanonicalRepoSQL(a.renames, "t.repo")+`) = lower(`+refs.CanonicalRepoSQL(a.renames, "$1::text")+`)
				order by lower(t.repo) = lower(`+refs.CanonicalRepoSQL(a.renames, "$1::text")+`) desc, t.repo
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
// (Attributor.canonical), and drops the row the same pull request is stored under as storedUnder
// when that is another name. GitHub answers a read only under the canonical name — the App
// answers 404 for the old name's installation — so no read can refresh a row keyed under an old
// name, and every reader reads such a row under the canonical name (refs.CanonicalRepoSQL). So
// one pull request keeps one row.
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
func (a *Attributor) upsertHeadUnderCanonicalName(ctx context.Context, db refs.Pool, head PullRequestHead, storedUnder string) (bool, error) {
	applied, err := a.upsertHead(ctx, db, head)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(storedUnder, head.Repo) {
		return applied, nil
	}
	if _, err := db.Exec(ctx, `
		delete from pull_request_heads where repo = $1 and number = $2
	`, storedUnder, head.Number); err != nil {
		return false, fmt.Errorf("drop the head of %s#%d, now %s#%d: %w", storedUnder, head.Number, head.Repo, head.Number, err)
	}
	return applied, nil
}

// HeadAttributedTo reports whether sha is the head of a pull request attributed to issueKey that
// is open or merged: the test a `before_merge` result's commit passes to stand at one of the
// heads its check is compared with. A closed-unmerged pull request's head proves nothing. A
// pull request attributed to another issue never answers for this one, so a child's pull request
// never answers for a check above it. The issue is the statement's (attributedTerms), which gives
// a pull request the issue links by its URL in any repository, and only a pull request with a
// results row has a head to answer. While a read of GitHub is owed for the pull request
// (owedReadJoin), Dispatch does not know that the stored head is the head, so it answers false for
// it. It is the question HeadsAttributedTo answers for one commit; no route asks it, because each
// recorded result reads its commit through (api/results.go, readThroughPullRequests), and this
// package's tests read a basis through it.
func (a *Attributor) HeadAttributedTo(ctx context.Context, pool *store.Pool, issueKey, sha string) (bool, error) {
	statements, err := a.attribution()
	if err != nil {
		return false, err
	}
	var found bool
	if err := pool.QueryRow(ctx, statements.headAttributedTo, issueKey, sha).Scan(&found); err != nil {
		return false, fmt.Errorf("read the pull request heads of %s at %s: %w", issueKey, sha, err)
	}
	return found, nil
}

var headAttributedToSelect = `
	select exists (
		select 1 from attributed a join heads h on h.j_repo = a.repo and h.j_number = a.number
		` + owedReadJoin + `
		where a.issue_key = $1 and h.head_sha = $2 and h.state in ('open', 'merged') and o.reason is null
	)`

// AttributedHead is one pull request Dispatch holds for an issue, as the results read names it
// among the heads a `before_merge` check compares its results with: the pull request's number,
// the commit its head is, and the two times Dispatch may have learned that head by. CheckedAt
// is when Dispatch last read the pull request from GitHub and EventAt is the pull request's
// own update time on GitHub, as the read that wrote the head answered it; each is nil where no
// writer has carried it. The reader shows the later of the two and names which it is
// (model.BasisLearnedReadAt, model.BasisLearnedUpdatedAt). HeadKnown is false for a pull request
// Dispatch holds no results row of, only a timeline row: it has no head, HeadSHA is "" and both
// times are nil. HeadKnown is false too while a read of GitHub is owed for the pull request, and
// HeadSHA and the times are then those of its results row. RefusedAs and RefusedSince name how
// GitHub refuses that owed read and since when (model.HeadNotRead), and are empty unless the owed
// read is a refusal. LinkedRepo is the repository of a pull request the statement gives the issue
// by a link of its URL in a repository the issue does not resolve to (attributedTerms' linked),
// under its canonical name lowercased, and "" for every other pull request: the reader names that
// repository beside the number, which alone would read as a pull request of the issue's own
// repository. Repo is the canonical name of the pull request's repository (refs.CanonicalRepoSQL),
// for every pull request, which the record route reads to say which repository holds a commit
// that heads it (api/results_linked.go, refuseUnknownCommit).
type AttributedHead struct {
	Number       int
	Repo         string
	HeadSHA      string
	HeadKnown    bool
	CheckedAt    *time.Time
	EventAt      *time.Time
	RefusedAs    string
	RefusedSince *time.Time
	LinkedRepo   string
}

// HeadsAttributedTo is every open or merged pull request attributed to issueKey, newest learned
// first, and then every pull request of the issue whose head Dispatch has not read, by number
// descending: the whole list HeadAttributedTo answers one commit of, which the results read names
// as what a `before_merge` check's results are compared with (R3). The issue is the statement's
// (attributedTerms), so the two reads keep one rule, and a commit HeadAttributedTo answers yes for
// is a commit this list holds. The statement gives the issue a pull request it links by its URL
// in any repository, so the list holds such a pull request of another repository too, naming
// that repository (LinkedRepo). A pull request with no results row, and one owed a read of GitHub
// (owedReadJoin), is a head Dispatch has not read.
//
// The order of the heads that are read is by the later of the two times, with a row carrying
// neither last and the pull request's own number breaking a tie, so the list does not turn on
// whichever row the planner returned first. A reader taking the first head takes the one Dispatch
// learned last.
//
// q is the caller's querier: the pool, or a transaction of the caller's. The read is one select
// and writes nothing. The record route passes the pool, before its write's transaction opens
// (api/results_linked.go, refuseUnknownCommit), since it asks GitHub about the commit after this
// read, and no transaction is held open across a read of GitHub. answers is what the caller's own
// request learned from GitHub and stored nowhere (HeadAnswers): a writer's read passes none and
// reads the stored rows, and a read-only caller's read passes its own and reads the rows as its
// writes leave them, in a transaction it rolls back (HeadAnswers.Read).
func (a *Attributor) HeadsAttributedTo(ctx context.Context, q Querier, issueKey string, answers HeadAnswers) ([]AttributedHead, error) {
	statements, err := a.attribution()
	if err != nil {
		return nil, err
	}
	var heads []AttributedHead
	err = answers.Read(ctx, q, func(q Querier) error {
		heads, err = readAttributedHeads(ctx, q, statements.headsAttributedTo, issueKey)
		return err
	})
	return heads, err
}

// readAttributedHeads is HeadsAttributedTo's statement on q.
func readAttributedHeads(ctx context.Context, q Querier, statement, issueKey string) ([]AttributedHead, error) {
	rows, err := q.Query(ctx, statement, issueKey)
	if err != nil {
		return nil, fmt.Errorf("read the pull request heads of %s: %w", issueKey, err)
	}
	defer rows.Close()
	var heads []AttributedHead
	for rows.Next() {
		var head AttributedHead
		var refusedAs *string
		if err := rows.Scan(&head.Number, &head.HeadSHA, &head.HeadKnown, &head.CheckedAt, &head.EventAt,
			&refusedAs, &head.RefusedSince, &head.LinkedRepo, &head.Repo); err != nil {
			return nil, fmt.Errorf("read the pull request heads of %s: %w", issueKey, err)
		}
		if refusedAs != nil {
			head.RefusedAs = *refusedAs
		}
		heads = append(heads, head)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the pull request heads of %s: %w", issueKey, err)
	}
	return heads, nil
}

// headsAttributedToSelect is HeadsAttributedTo's select over the statement's terms by issue: a
// left join with heads, so a pull request with a timeline row only is listed with no head, and a
// left join with its owed read (owedReadJoin), so a pull request owed a read is listed with its
// stored head and HeadKnown false. A pull request the statement gives by a link in a repository
// the issue does not resolve to names that repository, lowercased.
var headsAttributedToSelect = `
	select a.number, coalesce(h.head_sha, ''), h.repo is not null and o.reason is null, h.checked_at, h.event_at,
		o.refused_as, o.refused_since, case when a.linked then lower(a.repo) else '' end, a.repo
	from attributed a left join heads h on h.j_repo = a.repo and h.j_number = a.number
	` + owedReadJoin + `
	where a.issue_key = $1 and (h.repo is null or h.state in ('open', 'merged'))
	order by h.repo is null or o.reason is not null,
		case when h.repo is not null and o.reason is null then greatest(h.checked_at, h.event_at) end desc nulls last,
		a.number desc`

// NewestHead is the one pull request head the results read derives a check CI proves at: the
// repository's canonical name, the pull request's number, its head commit and its state (open or
// merged). ReadOwed is true while a read of GitHub is owed for that pull request (owedReadJoin):
// Dispatch then does not know that HeadSHA is the head, and reads no check run at it.
type NewestHead struct {
	Repo     string
	Number   int
	HeadSHA  string
	State    string
	ReadOwed bool
}

// NewestPullRequestHead is the head of the open pull request of the highest number attributed
// to issueKey, or, where none is open, the final head of its merged pull request of the highest
// number, and false where Dispatch holds neither. GitHub's numbers rise, so the highest is the
// newest. A pull request of the repository the issue resolves to comes before one the issue links
// in another repository (attributedTerms' linked), since the numbers of two repositories say
// nothing of which is newer: an issue's own repository answers where it holds a pull request of
// the issue, as it did before a link could give the issue one of another repository. Both are
// heads HeadsAttributedTo lists, so a result at the commit answered here stands at one of the
// heads a `before_merge` check compares its results with. A closed-unmerged pull request is no
// head, and a pull request with no results row has none. The repository is the canonical name
// (refs.CanonicalRepoSQL), which GitHub answers the head's check runs under (api/results_ci.go),
// whatever name the row is stored under. A pull request owed a read is answered as before, with
// ReadOwed true.
func (a *Attributor) NewestPullRequestHead(ctx context.Context, q Querier, issueKey string, answers HeadAnswers) (NewestHead, bool, error) {
	statements, err := a.attribution()
	if err != nil {
		return NewestHead{}, false, err
	}
	var head NewestHead
	err = answers.Read(ctx, q, func(q Querier) error {
		return q.QueryRow(ctx, statements.newestPullRequestHead, issueKey).Scan(
			&head.Repo, &head.Number, &head.HeadSHA, &head.State, &head.ReadOwed)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return NewestHead{}, false, nil
	}
	if err != nil {
		return NewestHead{}, false, fmt.Errorf("read the newest pull request head of %s: %w", issueKey, err)
	}
	return head, true, nil
}

var newestPullRequestHeadSelect = `
	select a.repo, a.number, h.head_sha, h.state, o.reason is not null
	from attributed a join heads h on h.j_repo = a.repo and h.j_number = a.number
	` + owedReadJoin + `
	where a.issue_key = $1 and h.state in ('open', 'merged')
	order by a.linked, h.state = 'open' desc, a.number desc
	limit 1`

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
// commit is such a head — can never answer for it. Keying by the issue rests on the rule of the
// statement (own_head, attributionHeld). The rule gives a pull request to an issue only where its
// head is a branch of the pull request's own repository, and this reads the pull requests of the
// repository the issue resolves to alone: one the issue links in another repository is
// BaseBranchMergedIn's. So the branch this answers is one the issue's own work merged into. A
// pull request whose texts do not describe its stored head keeps the issue its results row
// stores (R4's fallback): an earlier answer of the statement (SyncAttributions), or what a writer
// of the release before stored.
//
// An empty answer is "no branch to measure against", which a merged row carrying no base_ref
// gives as much as no merged row at all. The column is declared not null with the empty string
// as its default, and a `pull_request` envelope published before Dispatch carried the field
// writes exactly that, so such a row names no base branch; the empty answer leaves the caller
// on its own fallback, the repository's default branch, where handing that value on as a
// branch would have it build a GitHub compare read against a branch with no name. A timeline row
// stores no base branch, so a pull request with no results row names none either. answers is the
// caller's request's, as HeadsAttributedTo takes them.
func (a *Attributor) BaseBranchMergedFor(ctx context.Context, q Querier, issueKey string, answers HeadAnswers) (string, error) {
	statements, err := a.attribution()
	if err != nil {
		return "", err
	}
	var branch string
	err = answers.Read(ctx, q, func(q Querier) error {
		return q.QueryRow(ctx, statements.baseBranchMergedFor, issueKey).Scan(&branch)
	})
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
	where a.issue_key = $1 and not a.linked and h.state = 'merged'
	order by h.event_at desc nulls last, a.number desc
	limit 1`

// BaseBranchMergedIn is BaseBranchMergedFor for repoFull, a repository issueKey does not resolve
// to: the branch the newest merged pull request that the statement gives the issue by a link of
// its URL in repoFull merged into (attributedTerms' linked), and "" where Dispatch holds no merge
// of such a pull request. An issue whose work spans two repositories links the pull request it
// has in the second one, and BaseBranchMergedFor reads the repository the issue resolves to
// alone, so it never names that merge. The results read measures an `after_merge` result whose
// commit the second repository holds against the branch answered here
// (api/results_comparison.go).
//
// The rows are ordered as BaseBranchMergedFor orders them, and the empty answer means what its
// empty answer means: no branch to measure against. A link to a pull request of the repository
// the issue resolves to is no linked pull request, so a repoFull the issue resolves to answers
// "". repoFull is compared without case with the canonical name of each pull request, as every
// other comparison of a repository here is. answers is the caller's request's, as
// HeadsAttributedTo takes them.
func (a *Attributor) BaseBranchMergedIn(ctx context.Context, q Querier, issueKey, repoFull string, answers HeadAnswers) (string, error) {
	statements, err := a.attribution()
	if err != nil {
		return "", err
	}
	var branch string
	err = answers.Read(ctx, q, func(q Querier) error {
		return q.QueryRow(ctx, statements.baseBranchMergedIn, issueKey, repoFull).Scan(&branch)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the merged linked pull request base branch of %s in %s: %w", issueKey, repoFull, err)
	}
	return branch, nil
}

const baseBranchMergedInSelect = `
	select h.base_ref
	from attributed a join heads h on h.j_repo = a.repo and h.j_number = a.number
	where a.issue_key = $1 and a.linked and lower(a.repo) = lower($2::text) and h.state = 'merged'
	order by h.event_at desc nulls last, a.number desc
	limit 1`

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

// PullRequestHeadBatch is the most open rows read again, and the most reads owed for a failure,
// that one pass reads (Reconcile.readOpenHeads, Reconcile.readOwedPullRequests).
const PullRequestHeadBatch = 200

// ListStalePullRequestHeads is at most limit open rows for the reconcile to read again, the
// longest unread first: every open row whose checked_at is older than PullRequestHeadStaleAfter,
// a row never read first. An open row comes round at every pass of one process, each starting at
// most five minutes after the one before it ended, and as PullRequestHeadStaleAfter says with
// several, so a head that an event never brought moves within one pass of its staleness, where
// fewer than limit open rows are due
// before it. A merged row is read by the facts line while its texts do not describe its head or
// its commit messages are not read (Reconcile.readFactsLine). After that the head pass reads it
// again only where a read is owed for it (Reconcile.readOwedPullRequests): the statement gives
// its issue from the stored facts at each read.
//
// Each ref names the pull request under the canonical name of its repository
// (refs.CanonicalRepoSQL), the name a read of GitHub answers under, and StoredUnder names the row
// as it is stored. The read writes the row under the canonical name and drops the row under the
// old name (upsertHeadUnderCanonicalName), so a row stored under a name that was renamed away is
// offered once. A row whose canonical name GitHub does not allow (refs.SplitGitHubRepo) is
// offered never: no read can reach it, so it is warned of and stamped read here, as a row of a
// repository the App cannot read is stamped (MarkPullRequestHeadChecked), and leaves the front of
// the next batches rather than holding one of its places every pass.
func (a *Attributor) ListStalePullRequestHeads(ctx context.Context, pool *store.Pool, limit int, now time.Time) ([]PullRequestRef, error) {
	rows, err := pool.Query(ctx, `
		select h.repo, `+refs.CanonicalRepoSQL(a.renames, "h.repo")+`, h.number from pull_request_heads h
		where h.state = 'open' and (h.checked_at is null or h.checked_at < $1)
		order by h.checked_at nulls first, h.repo, h.number
		limit $2
	`, now.Add(-PullRequestHeadStaleAfter), limit)
	if err != nil {
		return nil, fmt.Errorf("list the stale pull request heads: %w", err)
	}
	defer rows.Close()
	var stale, unnamed []PullRequestRef
	for rows.Next() {
		var stored, canonical string
		var number int
		if err := rows.Scan(&stored, &canonical, &number); err != nil {
			return nil, fmt.Errorf("list the stale pull request heads: %w", err)
		}
		owner, repo, err := refs.SplitGitHubRepo(canonical)
		if err != nil {
			logging.FromContext(ctx).Warn("dispatch delivery: a stale pull request head names no repository GitHub can have", "repo", stored, "number", number, "error", err)
			unnamed = append(unnamed, PullRequestRef{Number: number, StoredUnder: stored})
			continue
		}
		stale = append(stale, PullRequestRef{Owner: owner, Repo: repo, Number: number, StoredUnder: stored})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the stale pull request heads: %w", err)
	}
	rows.Close()
	for _, ref := range unnamed {
		if err := MarkPullRequestHeadChecked(ctx, pool, ref.StoredUnder, ref.Number, now); err != nil {
			logging.FromContext(ctx).Warn("dispatch delivery: stamp a stale pull request head no read can reach", "repo", ref.StoredUnder, "number", ref.Number, "error", err)
		}
	}
	return stale, nil
}

// MarkPullRequestHeadChecked stamps checked_at on a row the reconcile read but could not
// complete — the repository unreadable, the pull request gone — so one unreadable pull request
// does not hold the batch every pass.
func MarkPullRequestHeadChecked(ctx context.Context, db refs.Execer, repoFull string, number int, now time.Time) error {
	_, err := db.Exec(ctx, `
		update pull_request_heads set checked_at = $3
		where repo = $1 and number = $2
	`, repoFull, number, now)
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
// REST reader of GitHub stores a pull request through it: the intake (recordPullRequestHead), the
// read-through (ReadThroughCommitPulls) and the refresh (RefreshPullRequestHead). It writes
// through the one writer (upsertHeadUnderCanonicalName). The batched query reads the commit
// messages with the other facts, and its caller writes each answer through that writer too
// (readFactsBatch, Reconcile.readFactsOf). It takes four steps:
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
//
// f is where it reads and writes: the pool for every writer, and for a read-only caller's results
// read the request's answers, where each statement runs in a transaction that holds the request's
// writes and is rolled back, and each write is kept for the request (facts, HeadAnswers). Its read
// of the commit messages from GitHub is made outside any transaction.
func (a *Attributor) storePullRequestFacts(
	ctx context.Context, f facts, client *githubapp.Client, token, canonical, storedUnder string,
	payload pullRequestPayload, checkedAt *time.Time, read readStart, stampIncomplete, bounded bool,
) (*PullRequestTexts, error) {
	texts := &PullRequestTexts{HeadRepo: headRepositoryOf(payload), BaseRef: payload.Base.Ref, Title: payload.Title, Body: payload.Body}
	if !headInOwnRepository(canonical, texts.HeadRepo) {
		texts.CommitMessages, texts.MessagesRead = []string{}, true
	} else {
		var stored []string
		var found bool
		if err := f.read(ctx, func(q refs.Pool) error {
			var err error
			stored, found, err = storedMessages(ctx, q, canonical, payload.Number, payload.Head.SHA, payload.Base.Ref)
			return err
		}); err != nil {
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
	applied, err := a.keepFacts(ctx, f, head, storedUnder, bounded)
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
	var answers map[PullRequestKey]PullRequestAttribution
	if err := f.read(ctx, func(q refs.Pool) error {
		var err error
		answers, err = a.PullRequestIssues(ctx, q, []PullRequestKey{key})
		return err
	}); err != nil {
		return answer(), err
	}
	if answers[key].HasFacts {
		// A higher source decides, so the read read every source it needs.
		if stampIncomplete || checkedAt == nil {
			return answer(), nil
		}
		stamped, at := frozenHead(head), *checkedAt
		_, err := f.write(ctx, func(ctx context.Context, db refs.Pool) (bool, error) {
			return false, stampHeadRead(ctx, db, stamped, at, bounded)
		})
		return answer(), err
	}
	owner, repo, err := refs.SplitGitHubRepo(canonical)
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
		// The attempt that ctx carries notes this failed read (githubreads). Only the leased reconcile
		// pass carries an attempt, so the intakes and the results routes note nothing.
		githubreads.NoteFailuref(ctx, "read the commit messages of %s PR #%d: %w", canonical, payload.Number, readErr)
		return answer(), nil
	}
	if messages == nil {
		messages = []string{}
	}
	texts.CommitMessages, texts.MessagesRead = messages, true
	head.CheckedAt = checkedAt
	if _, err := a.keepFacts(ctx, f, head, canonical, bounded); err != nil {
		return answer(), err
	}
	return answer(), nil
}

// storedMessages is the commit messages the stored texts of repo's pull request number hold for
// the head headSHA on the base baseRef, and found false where they hold none for that pair.
func storedMessages(ctx context.Context, q refs.Queryer, repo string, number int, headSHA, baseRef string) (messages []string, found bool, err error) {
	err = q.QueryRow(ctx, `
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
func (a *Attributor) writeFacts(ctx context.Context, db refs.Pool, head PullRequestHead, storedUnder string, bounded bool) (bool, error) {
	if bounded {
		writing, cancel := owedTableBound(ctx)
		defer cancel()
		return a.upsertHeadUnderCanonicalName(writing, db, head, storedUnder)
	}
	return a.upsertHeadUnderCanonicalName(ctx, db, head, storedUnder)
}

// keepFacts is writeFacts through f (storePullRequestFacts): on the pool for a writer, and kept
// for a read-only caller's request. The write is of a copy of head, its texts copied too
// (frozenHead), so a change the caller makes to head after it is no change of the write kept.
func (a *Attributor) keepFacts(ctx context.Context, f facts, head PullRequestHead, storedUnder string, bounded bool) (bool, error) {
	head = frozenHead(head)
	return f.write(ctx, func(ctx context.Context, db refs.Pool) (bool, error) {
		return a.writeFacts(ctx, db, head, storedUnder, bounded)
	})
}

// stampHeadRead stamps the row of head with at, where the row still holds the answer that head
// wrote: a read that did not page the messages, because a higher source decides, read every
// source it needs. A row a later answer replaced in between keeps the stamp of that answer.
func stampHeadRead(ctx context.Context, db refs.Execer, head PullRequestHead, at time.Time, bounded bool) error {
	if bounded {
		var cancel context.CancelFunc
		ctx, cancel = owedTableBound(ctx)
		defer cancel()
	}
	if _, err := db.Exec(ctx, `
		update pull_request_heads set checked_at = $3
		where repo = $1 and number = $2 and head_sha = $4 and event_at is not distinct from $5
	`, head.Repo, head.Number, at, head.HeadSHA, head.EventAt); err != nil {
		return fmt.Errorf("%w: stamp the read of %s#%d: %w", ErrHeadNotStored, head.Repo, head.Number, err)
	}
	return nil
}

// RefreshPullRequestHead reads one pull request through the App and stores its facts
// (storePullRequestFacts): the reconcile's read of one pull request over REST. The reconcile
// makes it for each pull request owed a read (Reconcile.readOwedPullRequests), for each partial
// timeline row (Reconcile.completePartialPullRequest), and for each pull request whose batched
// answer is too large (readFactsBatch). It answers what it fetched and stored, nil where GitHub
// refused the read, and GitHub's refusal to give the pull request, nil where GitHub answered it,
// which the reconcile's read of a pull request an event owes a read of needs
// (readOwedPullRequests): only an answer, or a refusal that pullRequestAbsent proves final,
// settles such a read; the pass over the stored rows reads no refusal. A pull request GitHub
// cannot find (ErrPullRequestNotFound), and a repository the App cannot read (no installation,
// or a repository or resource its token is refused), are refusals and leave the row as it is,
// stamped so the batch moves on and does not warn about the same row every pass. A canonical
// name GitHub does not allow (refs.SplitGitHubRepo's refs.ErrNotGitHubRepo) is stamped the same
// way, before any GitHub call, and comes back as that error, so the reconcile warns of the row on
// the pass that stamps it and not on every pass; a rate limit comes back as the
// *githubapp.RateLimitError. The refusal carries the read GitHub refused: the
// repository's canonical owner and name, the number, and the installation token the read was
// refused with, which pullRequestAbsent asks GitHub with. The row is stamped even when the commit
// messages could not be read: this pass is itself the retry, so the next one reads it again in
// PullRequestHeadStaleAfter rather than holding the front of every batch.
//
// repoFull is the name the caller holds the pull request under (an owed read's, a timeline row's,
// or a batched ref's StoredUnder), which may be a name the repository has been renamed away from,
// and every step here but the stamp works on the canonical name instead (Attributor.canonical): the
// token is minted for it, since GitHub answers 404 for the installation of a name no longer in
// use; and the row is written under it, with the row under the old key dropped in the same step
// (upsertHeadUnderCanonicalName). The stamp stays on the key the caller handed over, since that is
// the row a later batch must stop offering: a canonical repository the App cannot read teaches
// this row nothing under either name.
//
// installations is the pass's lookups of the installation of each repository
// (passInstallations), nil for a caller that has no pass.
func (a *Attributor) RefreshPullRequestHead(
	ctx context.Context, pool *store.Pool, client *githubapp.Client,
	installations *passInstallations, repoFull string, number int, now time.Time,
) (fetched *FetchedFacts, refused *pullRequestRefusal, err error) {
	return a.refreshPullRequestHead(ctx, facts{pool: pool}, client, installations, repoFull, number, now)
}

// refreshPullRequestHead is RefreshPullRequestHead through f: on the pool for every writer, and
// kept for a read-only caller's results read (AnswerLinkedHead), whose stamps and writes are made
// in transactions it rolls back (facts).
func (a *Attributor) refreshPullRequestHead(
	ctx context.Context, f facts, client *githubapp.Client,
	installations *passInstallations, repoFull string, number int, now time.Time,
) (fetched *FetchedFacts, refused *pullRequestRefusal, err error) {
	mark := func() error {
		_, err := f.write(ctx, func(ctx context.Context, db refs.Pool) (bool, error) {
			return false, MarkPullRequestHeadChecked(ctx, db, repoFull, number, now)
		})
		return err
	}
	canonical := a.canonical(repoFull)
	owner, repo, err := refs.SplitGitHubRepo(canonical)
	if err != nil {
		// No read of GitHub can reach a name GitHub does not allow, so the row is stamped as an
		// unreadable one is, and the error goes back: the reconcile warns of it on the pass that
		// stamps it rather than on every pass.
		if stamp := mark(); stamp != nil {
			return nil, nil, errors.Join(err, stamp)
		}
		return nil, nil, err
	}
	token, err := installations.repositoryToken(ctx, client, owner, repo)
	if err != nil {
		if RepositoryUnreadable(err) {
			return nil, &pullRequestRefusal{err: err, owner: owner, repo: repo, number: number}, mark()
		}
		return nil, nil, fmt.Errorf("mint installation token for %s PR #%d: %w", canonical, number, err)
	}
	began, err := f.startRead(ctx)
	if err != nil {
		return nil, nil, err
	}
	payload, err := fetchPullRequestPayload(ctx, client, token, owner, repo, number)
	if err != nil {
		if errors.Is(err, ErrPullRequestNotFound) || RepositoryUnreadable(err) {
			return nil, &pullRequestRefusal{err: err, owner: owner, repo: repo, number: number, token: token}, mark()
		}
		return nil, nil, err
	}
	texts, err := a.storePullRequestFacts(ctx, f, client, token, canonical, repoFull, payload, &now, began, true, false)
	if err != nil {
		return nil, nil, err
	}
	return &FetchedFacts{Canonical: canonical, Owner: owner, Repo: repo, Token: token, Payload: payload, Texts: texts}, nil, nil
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
// (`GET /repos/{o}/{r}/commits/{sha}/pulls`) and stores each one's facts, so a result recorded
// at the head of a pull request no event has reached Dispatch for stands at it at once.
// It answers how many rows it wrote and whether any pull request it saw has sha as its head
// (atHead). A repository the App cannot read writes nothing and comes back as the App's own
// error, which each caller logs at Info and carries on from (api/results.go's
// readThroughPullRequests, api/results_comparison.go's readThroughHead): the answer about that
// result stands as its stored rows leave it.
//
// It reads a commit that a pull request Dispatch holds already heads, and writes every pull
// request of GitHub's answer there too: GitHub can name a second pull request at the same head,
// whose own event never arrived (R6 of the results lookup). A closed pull request that was not
// merged, and that Dispatch holds no row of, gets no row (pullRequestHeld): it is no head a result
// stands at, and its row would cost a read of its commit messages. A closed pull request that
// Dispatch holds a row of is written, so the row takes GitHub's state (R2).
//
// atHead parts the two answers GitHub gives about a commit that heads none of the pull requests
// it names. GitHub naming nothing leaves the question wide open — the pull request may open at
// that commit a moment later, which is the whole of what this read repairs — while GitHub naming
// a pull request whose head is another commit has answered it for now: the commit is behind that
// pull request, and only a pull request opened later can put a head back on it. So the read-time
// caller holds the pair off for CommitPullSettledFor on that answer rather than asking again
// every CommitPullReadStaleAfter (delivery.CommitPullReads.Settle), where the read wrote a row. A
// closed pull request that gets no row is not counted in written, so an answer that names only
// such pull requests holds the pair off for CommitPullReadStaleAfter alone. The rows written say
// nothing of it on their own: the row of a pull request whose head has moved on is written at
// the head it has now, not at the commit that was asked about.
//
// Each pull request of the list is stored as every REST reader of GitHub stores one
// (storePullRequestFacts), from the list's own answer: the one list read is the only read of
// GitHub's pull requests it makes, and it pages a pull request's commit messages only where no
// higher source names a held issue. A row whose commit messages could not be read keeps the stamp
// it had, and the reconcile reads it again rather than this read-through standing as the last
// word.
//
// A read-through settles what its answer covers. Before its list read it takes the name and the
// revision of each read owed in the repository (owedReadsIn), through the renaming rule. After it
// writes a pull request of GitHub's answer, it settles that pull request's owed read at that
// revision (settleOwedRead), as the intake does (recordPullRequestHead). An owe made after the
// read began has another revision, and it stays: so does the owe the write's own trigger makes
// (pull_request_heads_older_read). Without the settle, a pull request that is owed a read and has
// no row stays a head not read in the answer of the read that learns it.
//
// repoFull comes from outside — the repository the check's issue resolves to — so it is
// canonicalised before anything is asked of GitHub or written (refs.Renames.Canonical), and no
// row is ever written under a name a repository was renamed away from. The read-time caller has
// canonicalised that name already, and so has the record-time caller, which resolves through
// the same helper (api/issue_repo.go, server.checkIssueRepository); the rule reaches the
// canonical name in one step, so this read changes nothing for either. It is here because this
// function's key is its argument's, not its caller's, and a caller that hands over a name it
// has not canonicalised must still write no row under a name a repository was renamed away
// from.
//
// The read runs on a context its caller's end cannot cancel, bounded by ReadThroughTimeout. Both
// callers are requests (api/results.go, api/results_read_through.go), and a client that hangs up
// cancels its request: on the caller's own context that cut the head write short, and the read's
// claim (CommitPullReads) then held the commit off for CommitPullReadStaleAfter with nothing
// stored (2026-11-19T17:09:59Z, cybermonklabs/legion#4490, "context canceled"). A failure of
// Dispatch's own database before or after GitHub's answer is ErrReadThroughNotStored, which the
// read-time caller answers by giving its claim back (CommitPullReads.ReleaseAcrossProcesses); a
// failure of GitHub's is not, since GitHub was asked and the claim bounds how often it is. That
// holds for GitHub's rate limit on a pull request's commit messages too, which comes after the
// pull request's head is written (storePullRequestFacts): it is answered as GitHub's limit, not
// as a read that stored nothing.
//
// A writer's read calls this. A read-only caller's read of an issue's results stores nothing, and
// calls AnswerCommitPulls, which is this read with its writes kept for the request.
func (a *Attributor) ReadThroughCommitPulls(
	ctx context.Context, pool *store.Pool, client *githubapp.Client,
	repoFull, sha string, now time.Time,
) (written int, atHead bool, err error) {
	return a.readThroughCommitPulls(ctx, facts{pool: pool}, client, repoFull, sha, now)
}

// AnswerCommitPulls is ReadThroughCommitPulls for a read-only caller's results read, which stores
// nothing and keeps nothing after its request (plan D32): it asks GitHub what
// ReadThroughCommitPulls asks at that moment and answers what it answers, and makes each of its
// statements in a transaction of pool that first makes the writes answers holds and is rolled
// back, keeping each write it makes in answers (facts, HeadAnswers). So every later read of the
// request that passes answers reads the rows the writer's read-through would have stored. It takes
// no place in the order of reads (peekRead).
func (a *Attributor) AnswerCommitPulls(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, answers *HeadAnswers,
	repoFull, sha string, now time.Time,
) (written int, atHead bool, err error) {
	return a.readThroughCommitPulls(ctx, facts{pool: pool, answers: answers}, client, repoFull, sha, now)
}

// readThroughCommitPulls is ReadThroughCommitPulls through f (facts): its reads of GitHub, and
// each statement on the pool for a writer or kept for a read-only caller's request.
func (a *Attributor) readThroughCommitPulls(
	ctx context.Context, f facts, client *githubapp.Client,
	repoFull, sha string, now time.Time,
) (written int, atHead bool, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ReadThroughTimeout)
	defer cancel()
	canonical := a.canonical(repoFull)
	owner, repo, token, err := repositoryRead(ctx, client, canonical)
	if err != nil {
		return 0, false, err
	}
	var owed map[int][]owedRead
	if err := f.read(ctx, func(q refs.Pool) error {
		owed = a.owedReadsIn(ctx, q, canonical)
		return nil
	}); err != nil {
		return 0, false, readThroughNotStored(err)
	}
	began, err := f.startRead(ctx)
	if err != nil {
		return 0, false, readThroughNotStored(err)
	}
	pulls, err := fetchCommitPullPayloads(ctx, client, token, owner, repo, sha)
	if err != nil {
		return 0, false, err
	}
	for _, pull := range pulls {
		if pull.Head.SHA == sha {
			atHead = true
		}
		if PullRequestHeadState(pull.State, pull.Merged || pull.MergedAt != nil) == "closed" {
			var held bool
			if err := f.read(ctx, func(q refs.Pool) error {
				var err error
				held, err = a.pullRequestHeld(ctx, q, canonical, pull.Number)
				return err
			}); err != nil {
				return written, atHead, readThroughNotStored(err)
			}
			if !held {
				continue
			}
		}
		if _, err := a.storePullRequestFacts(ctx, f, client, token, canonical, repoFull, pull, &now, began, false, false); err != nil {
			if limited, ok := githubapp.AsRateLimit(err); ok {
				return written, atHead, limited
			}
			return written, atHead, readThroughNotStored(err)
		}
		written++
		for _, read := range owed[pull.Number] {
			if _, err := f.write(ctx, func(ctx context.Context, db refs.Pool) (bool, error) {
				return false, settleOwedRead(ctx, db, read.Repo, read.Number, read.Revision)
			}); err != nil {
				logging.FromContext(ctx).Warn("dispatch delivery: settle the read owed before this read-through; the reconcile reads the pull request once more",
					"repo", read.Repo, "number", read.Number, "error", err)
			}
		}
	}
	return written, atHead, nil
}

// ReadThroughTimeout bounds one read-through (ReadThroughCommitPulls) from its first read to its
// last write. What it waits for is GitHub's answers in turn, each bounded by
// githubapp.RequestTimeout (20 s): the installation token, the list of the commit's pull requests,
// and for a pull request whose texts name no held issue the first page of its commit messages,
// with Dispatch's own statements, which a healthy database answers in milliseconds, between them.
// Three such calls are a minute, so a read-through that GitHub answers within each call's bound
// writes the first pull request's head inside it; a slower read ends there, and a later read asks
// again once the claim it took has passed. It is intakeMessageTimeout's bound for the same work of
// one envelope.
const ReadThroughTimeout = time.Minute

// ErrReadThroughNotStored marks a read-through that failed in Dispatch's own database: before it
// asked GitHub (the start of the read) or after GitHub answered (a stored row read, the write of a
// pull request's facts); the canonical name is no read. No claim of the read may outlast it: what
// GitHub answered, or would have, is not stored, and nothing but another read stores it. A
// failure of GitHub's, its rate limit on a pull request's commit messages among them, is never
// marked so.
var ErrReadThroughNotStored = errors.New("the read-through's answer was not stored")

// readThroughNotStored marks err as ErrReadThroughNotStored, keeping err itself inside the mark.
func readThroughNotStored(err error) error {
	return fmt.Errorf("%w: %w", ErrReadThroughNotStored, err)
}

// pullRequestHeld reports whether Dispatch holds a results row of pull request number of the
// repository canonical, under any name the renaming rule reads as canonical and in any case
// (ReadThroughCommitPulls). It reads the rows as the statement's heads term reads them
// (attributedTerms): a row written from a `pull_request` envelope can carry GitHub's own spelling
// of the repository (`Acme/Widgets`), or a name the repository was renamed away from.
func (a *Attributor) pullRequestHeld(ctx context.Context, q refs.Queryer, canonical string, number int) (bool, error) {
	var held bool
	if err := q.QueryRow(ctx, `
		select exists (
			select 1 from pull_request_heads h
			where h.number = $2 and lower(`+refs.CanonicalRepoSQL(a.renames, "h.repo")+`) = lower($1)
		)
	`, canonical, number).Scan(&held); err != nil {
		return false, fmt.Errorf("read whether %s#%d is stored: %w", canonical, number, err)
	}
	return held, nil
}

// recordPullRequestHead writes the head row of the pull request one `pull_request` envelope
// names, whatever its action, author or repository, from GitHub's own answer about that pull
// request, and answers what it read of GitHub on the way (headRead): the pull request, the token
// it was read with and the commit messages the write stored for its head, or GitHub's lasting
// refusal, all empty where the read was spared. It runs before the merge-only guard in
// handlePullRequestEnvelope, so an `opened`, `synchronize`, `reopened` or `edited` event — which
// the delivery timeline ignores — still moves the head a check result is measured against.
//
// The envelope says which pull request changed, and nothing it carries is written: the head,
// the state, the branch, the title and the body all come from `GET
// /repos/{owner}/{repo}/pulls/{number}` (fetchPullRequestPayload), stored as every REST reader
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
// The name and the number were checked before this runs (pullRequestNamed). The canonical name
// comes from the rename pairs the boot read and not from the envelope, and it is held to the rule
// every read that puts a stored name into a GitHub path holds a name to (refs.SplitGitHubRepo)
// before anything is read of GitHub: a name given that the envelope's check takes and that rule
// refuses, such as an owner that starts with `_`, is a head not stored (markHeadNotStored, and not
// errNoPullRequestNamed), which Intake.deliver owes a read for. The boot's Renames holds no pair
// whose new name that rule refuses (refs.NewRenames), so a rename never gives such a name. A
// GitHub answer that holds a value no head row holds is a head not stored too (errNoRowHolds,
// checkHeadRow), which Intake.deliver owes a read for as for every other.
//
// The read is spared where GitHub has already answered for the event (headReadCovers): the
// stored row is GitHub's answer to a read whose updated_at is a later second than the envelope's.
// That is what makes the events of one pull request that waited together — the replay of the
// stream's backlog on a first start, or a burst of events a stopped process missed — cost one read
// of that pull request rather than one each, and one more for the events of the second GitHub's
// answer holds, since an answer of the event's own second may be one GitHub gave before the event.
//
// The envelope's own `repo` comes from outside — the topic a publisher built — so it is
// canonicalised before anything is read or written (Attributor.canonical), and no row is ever
// written under a name a repository was renamed away from.
func (in *Intake) recordPullRequestHead(ctx context.Context, payload map[string]string, number int) (headRead, error) {
	given := payload["repo"]
	repoFull := in.attributor.canonical(given)
	owner, repo, err := refs.SplitGitHubRepo(repoFull)
	if err != nil {
		return headRead{}, markHeadNotStored(fmt.Errorf("the canonical name of %s: %v", given, err))
	}
	covered, err := headReadCovers(ctx, in.pool, repoFull, number, envelopeTime(payload["updated_at"]))
	if err != nil {
		return headRead{}, markHeadNotStored(err)
	}
	if covered {
		return headRead{}, nil
	}
	// owed is the row of the read this pull request was owed as this read begins, nil where none
	// was owed or it could not be read, read before any call to GitHub: the delete below takes
	// only that row, by its own name, so an owe made after this moment stays for the reconcile.
	owed := owedRevision(ctx, in.pool, repoFull, number)
	token, err := in.github.RepositoryToken(ctx, owner, repo)
	if err != nil {
		if RepositoryUnreadable(err) {
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
		if errors.Is(err, ErrPullRequestNotFound) || RepositoryUnreadable(err) {
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
	texts, err := in.attributor.storePullRequestFacts(ctx, facts{pool: in.pool}, in.github, token, repoFull, given, pull, &now, began, false, true)
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
		err := settleOwedRead(settling, in.pool, owed.Repo, number, owed.Revision)
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
	if err := in.attributor.owePullRequestRead(ctx, in.pool, repoFull, number, owedRefusal, refusalOf(refused)); err != nil {
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
