package delivery

// A pull request whose read of GitHub has not yet answered for a change Dispatch knows of is held
// in pull_request_reads_owed until a read of GitHub answers for it. GitHub's answer is the only
// source of a head, so an event whose read of GitHub failed
// would otherwise leave its pull request with no head for good: the reconcile reads only rows
// that exist, reads no closed row again, and reads a merged row only while its texts do not
// describe its head or its commit messages are not read (Reconcile.readFactsLine); and the
// read-through runs only for a result's commit. The row holds which pull request changed, why
// its read is owed, how often the reconcile has read it, when it was last owed and its revision,
// and nothing an envelope says: no head, no state, no issue.
//
// Four writers record one. Intake.deliver records a failure at the delivery of an envelope whose
// head write failed, and acknowledges the envelope once that record is written; a later read that
// writes the head deletes what was owed before it began (settleOwedRead). recordPullRequestHead
// records a refusal for GitHub's refusal of a read, a pull request it does not know or a repository
// the App cannot read, since GitHub's answer to a read made a moment after its own webhook may not
// know the pull request yet. And two triggers of pull_request_heads record a failure in the same
// statement as a write no read of GitHub has the last word on: pull_request_heads_older_read, for
// upsertHeadRow's write of GitHub's answer over a row no read of GitHub wrote whose time is later
// than the answer's, or over a row of the same GitHub second with other facts (during a roll the
// release before stores GitHub's newer answer from an envelope, and a slower read of this build can
// land an older answer after it, and two answers of one second say nothing of which is the newer);
// and pull_request_heads_unread_write, for a write of the release before that changes a fact of a
// row a read of GitHub wrote. An insert of the release before, and its update of a row no read
// marked, owe nothing: the reconcile's open arm reads such an open row, its facts line a merged
// one while its texts do not describe its head, and a closed one stays closed, which is no basis.
// And a link by hand records a failure for each pull request that it adds and that has no head
// row (OweLinkedPullRequestReads). No event carries a link. An envelope leaves the intake with no
// record of its pull request only where it names no pull request GitHub can have
// (Intake.deliver), which is checked before anything is read or written (pullRequestNamed). Where
// its repository's canonical name is no name GitHub can have, the read is owed under the name the
// envelope gave (owePullRequestRead); a GitHub answer that holds a value no head row holds is a
// head not stored, and owed (errNoRowHolds). Every failure of a statement of the record is the
// database not taking it now,
// so the envelope is handed back, and stays with NATS for as long as the stream keeps it, 72
// hours, as one a stop hands back and one a lapsed hold leaves outstanding for the next holder do.
//
// No time decides which owed read a read answers. Each owe gives the row a new revision, a value
// of the table's own sequence, and a read deletes the row only at the revision it read before it
// read GitHub (settleOwedRead): the owes that row recorded were made before the read began, so the
// read's answer covers them, and an owe made after that moment, by any transaction and whatever
// any clock says, gave the row another revision and stays for the next read. owed_at is when the
// read was last owed: the order a pass reads the rows in, a schedule and nothing more.
//
// The reconcile's head pass reads the owed pull requests from GitHub before the stored rows
// (Reconcile.readOwedPullRequests): first the failures, at most PullRequestHeadBatch of them, then
// the refusals, at most RefusedReadsPerPass, each kind the row read the fewest times first and
// then the one owed longest. An answer writes the row, creating it where none existed, and
// settles the read; a failure is read again by the next pass, so it is read every pass until
// GitHub answers. A refusal settles on evidence and never by its age, and the evidence is one
// answer of GitHub (pullRequestAbsent): asked with the token of the read GitHub refused, GitHub
// shows in one answer that the repository is readable and that it holds no such pull request, and
// the read ends. Every other answer is no evidence: Dispatch cannot tell a missing pull request
// from one it is not let see, and the read stays owed. The intake takes the events of every
// repository (githubIntakeSubject), so no repository leaves its rule and ends a read either. A
// read that answers the other way moves the row to the other kind (recordOwedReadOutcome).
//
// The two budgets are what keep a forged envelope from costing a real one its head. Any bearer of
// Envoy's /v1 publish route can name a pull request GitHub never had, and each such envelope owes
// a refusal. Outside a GitHub outage, refusals take
// no read from a failure. During an outage, a pass reads up to RefusedReadsPerPass refusals and
// each read fails, so each such row becomes a failure with fewer reads than the real failure and
// goes before it: after the outage, each PullRequestHeadBatch of such rows can delay the real head
// by one more pass. A real pull request whose own read was refused, because GitHub's answer had
// not caught up with its webhook, is a refusal itself, and refusals go fewest reads first, then
// owed longest. So the first pass after the lag reads it, where no pass has read it yet, only when
// fewer than RefusedReadsPerPass refusals with no reads were owed before it, and each further
// RefusedReadsPerPass of them delay it by one pass. Where a pass reads it while GitHub still lags
// and the repository is readable, that 404 ends the read, and the head waits for the pull request's
// next event; where the repository is not readable, every later refusal with no reads goes before
// it, so a flood that continues can hold it back with no fixed bound. Its next event reads GitHub
// at once regardless.
//
// The readers of a head read pull_request_heads, pull_request_texts and delivery_pull_requests
// through one statement (attributedTerms). While a read is owed, Dispatch does not know that the
// stored head is the head. So three readers by issue read the owed row of each pull request that
// they answer, through the renaming rule (owedReadJoin): HeadsAttributedTo answers the pull
// request with HeadKnown false, HeadAttributedTo answers false for its stored head, and
// NewestPullRequestHead answers ReadOwed true. So the stored head of a pull request owed a read is
// no basis, and no check run is read at it. BaseBranchMergedFor does not read this table: GitHub
// never changes the state or the base branch of a merged pull request, so a merged row answers
// its base branch also while a read is owed for it. The readers by key and by repository
// (PullRequestIssues, PullRequestsNotKnown) do not read it either. A refusal row names how GitHub
// refuses the read (refused_as) and since when (refused_since), so a reader can name the state
// and its way out. A release before this one neither reads nor writes the two columns.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
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

// owedReason is why a pull request is owed a read: pull_request_reads_owed.reason.
type owedReason string

const (
	// owedFailure is a read GitHub did not answer: a 5xx, a timeout, a rate limit, a head write
	// that failed; or a write no read of GitHub has the last word on (the triggers above).
	// GitHub will answer it once it answers at all, so it is read before any refusal. A link by
	// hand of a pull request with no head row owes one too (OweLinkedPullRequestReads).
	owedFailure owedReason = "failure"
	// owedRefusal is a read GitHub answered with a refusal: it does not have the pull request
	// (404 or 410), or the App cannot read its repository.
	owedRefusal owedReason = "refusal"
)

// RefusedReadsPerPass is how many pull requests owed a refusal one reconcile pass reads, after
// every failure it reads. Outside a GitHub outage it bounds what forged envelopes cost: at most 50
// reads of a pull request a pass and, for each of them that GitHub answers 404, one question of
// GitHub's GraphQL API, one point of its budget, whether the repository holds it
// (pullRequestAbsent), and none of them taken from a failure. The failures a pass reads ask the
// same question for each that GitHub now answers 404, up to PullRequestHeadBatch more, so a pass
// asks up to 250 in all. A repository the App is not installed on costs one installation lookup a
// pass however many of its refusals the pass reads (passInstallations). During an outage the
// refusals a pass reads fail and become failures, so they take failure reads after it. A refusal
// of a number a repository the App reads does not have ends at its first read, so N such forged
// envelopes are gone after about N/50 passes; a refusal in a repository the App cannot read stays,
// and is read at most once a pass, the fewest reads first.
const RefusedReadsPerPass = 50

// owedReadWriteTimeout bounds each statement of the intake that can wait on a lock of
// pull_request_reads_owed or of its revision sequence (owedTableBound), so a lock of that table,
// as a migration of it holds one, holds a delivery, and one of the intakeMaxAckPending slots its
// repository shares, for seconds and not for the message's whole deadline (acceptance of
// cybermonklabs/legion#4209 at 3a21edb2c9; its reviews at ac562e9837 and 5ea6b2ffb2, N1).
const owedReadWriteTimeout = 5 * time.Second

// owedTableBound is ctx bounded by owedReadWriteTimeout, for one statement of the intake that can
// wait on a lock of pull_request_reads_owed or of its revision sequence: the owe of a read
// (owePullRequestRead, whose two statements share it), the read's place, a value of that sequence
// (startRead, which a migration that alters the sequence or sets the table logged or unlogged
// holds), the settle of the read owed before a head read (Intake.recordPullRequestHead), and the
// head write, whose trigger can owe a read (Intake.writeHead). The intake opens no
// transaction those statements share, so the bound is one per statement and not one
// `SET LOCAL lock_timeout`. The owe runs it on the handling context where the message's own
// deadline may be the delivery's failure (Intake.oweEnvelopesRead), and on the message's context
// where GitHub refused the read (Intake.oweRefusedRead).
func owedTableBound(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, owedReadWriteTimeout)
}

// owedRevisionReadTimeout bounds the intake's read of the revision of a pull request's owed read
// (owedRevision), which a healthy database answers in milliseconds. It is how long a head read
// waits for that table before it goes on with no revision, so a session that holds the table
// locked, as a migration of it would, holds no head read and no acknowledgement.
const owedRevisionReadTimeout = time.Second

// owedRevision is the revision of the read repoFull's pull request number is owed as a read of
// GitHub begins, nil where none is owed: the one row that read's answer covers and may delete
// (settleOwedRead). Where the read fails or runs past owedRevisionReadTimeout, it answers nil with
// one log line, so the read deletes nothing: the record stays, and the reconcile's next pass reads
// the pull request once more and settles it. That costs one read, and loses no owe.
func owedRevision(ctx context.Context, pool *store.Pool, repoFull string, number int) *int64 {
	bounded, cancel := context.WithTimeout(ctx, owedRevisionReadTimeout)
	defer cancel()
	var revision int64
	err := pool.QueryRow(bounded, `
		select revision from pull_request_reads_owed where repo = $1 and number = $2
	`, repoFull, number).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		logging.FromContext(ctx).Warn("dispatch delivery: read the revision of the read a pull request is owed; this read deletes no owed read, and the reconcile reads the pull request once more",
			"repo", repoFull, "number", number, "error", err)
		return nil
	}
	return &revision
}

// owePullRequestRead records that the pull request number of the repository given changed, that
// no read of GitHub has answered for that change, and why (reason). given is canonicalised first
// (refs.CanonicalRepo), so one pull request is owed under one name, or under the name given where
// the canonical one is no name GitHub can have (below). Recording it again gives the
// row a new revision, moves its time to now and its reason to the latest read's: the latest event
// is the one a later read must answer for, and a read that read the row's revision before this
// owe settles nothing (settleOwedRead). It keeps the count of the reconcile's reads, so publishing
// the same envelope again moves no row ahead of the rows that have been read as often.
//
// The row is built here, and its values are checked here, before any statement: the name given,
// which the read of its canonical name is a statement of, with the number, must be a pull request
// GitHub can have (checkPullRequestKey), else the answer is errNoPullRequestNamed and nothing is
// written. The canonical name is the one value of the row no envelope gives: it is a
// repo_projects row's renamed_to, which the settings route holds to the same rule (api's
// parseRepoRename, IsGitHubRepositoryName), and which a target stored before that rule or by
// direct SQL holds to nothing. Where the canonical name is no name GitHub can have, the read is
// owed under the name given, which the check took: a rename to a name of 3,500 characters would
// otherwise fail the insert at the btree of the table (SQLSTATE 54000) at every delivery, and an
// owe refused for it would end the envelope with nothing owed, so the pull request's head never
// came, also after an operator corrected the target (acceptance of cybermonklabs/legion#4209 at
// 3a21edb2c9, D1). The reconcile reads the pull request under its canonical name each pass
// (RefreshPullRequestHead), so the read stays owed while the target stands and is read once it is
// corrected. The reason is one of this package's two constants. So no value this writes is one
// the table refuses, and a failure of a statement is the database's alone, which the intake hands
// back (recordRefused). Its statements run within owedReadWriteTimeout of the call. They run on
// db: the pool, or the transaction of a link write, which fails with them
// (OweLinkedPullRequestReads).
//
// refusedAs is the word of GitHub's refusal where reason is owedRefusal (refusalOf), and "" for a
// failure. A row that becomes a refusal takes the time of this write as refused_since, a row that
// stays one keeps its time, and a row that becomes a failure clears both columns (owedRefusalSet).
func owePullRequestRead(ctx context.Context, db owedWriter, given string, number int, reason owedReason, refusedAs string) error {
	ctx, cancel := owedTableBound(ctx)
	defer cancel()
	if err := checkPullRequestKey(given, number); err != nil {
		return fmt.Errorf("the read %.140q#%d is owed: %w", given, number, err)
	}
	repoFull, err := refs.CanonicalRepo(ctx, db, given)
	if err != nil {
		return err
	}
	if checkPullRequestKey(repoFull, number) != nil {
		repoFull = given
	}
	if _, err := db.Exec(ctx, `
		insert into pull_request_reads_owed (repo, number, reason, refused_as, refused_since)
		values ($1, $2, $3::text, case when $3::text = 'refusal' then nullif($4::text, '') end,
			case when $3::text = 'refusal' then now() end)
		on conflict (repo, number) do update
			set reason = excluded.reason, owed_at = excluded.owed_at, revision = excluded.revision,
				`+owedRefusalSet("excluded.reason", "excluded.refused_as", "excluded.refused_since", "pull_request_reads_owed.")+`
	`, repoFull, number, string(reason), refusedAs); err != nil {
		return fmt.Errorf("record the read %s#%d is owed: %w", repoFull, number, err)
	}
	return nil
}

// owedWriter is what owePullRequestRead writes through: the pool, or a transaction open on it.
type owedWriter interface {
	refs.Queryer
	refs.Execer
}

// pullRequestURLPrefix is how the URL of each pull request that GitHub gives (html_url) starts.
const pullRequestURLPrefix = "https://github.com/"

// linkedPullRequest is the repository and the number of the pull request whose own URL link is,
// with ok true. GitHub gives that URL as https://github.com/<owner>/<repo>/pull/<number>. The
// statement ties a link to a pull request where lower() of the two URLs is equal
// (attributedTerms). So this takes that form in any letter case, and no other form: no other part,
// and no zero before the number. ok is false for each other URL, and for a name or a number that
// no pull request of GitHub can have (checkPullRequestKey). The repository keeps the letters of
// the link.
func linkedPullRequest(link string) (repo string, number int, ok bool) {
	for i := range len(link) {
		if link[i] >= utf8.RuneSelf {
			return "", 0, false
		}
	}
	if len(link) <= len(pullRequestURLPrefix) || !strings.EqualFold(link[:len(pullRequestURLPrefix)], pullRequestURLPrefix) {
		return "", 0, false
	}
	parts := strings.Split(link[len(pullRequestURLPrefix):], "/")
	if len(parts) != 4 || !strings.EqualFold(parts[2], "pull") {
		return "", 0, false
	}
	digits := parts[3]
	if digits == "" || digits[0] == '0' || strings.Trim(digits, "0123456789") != "" {
		return "", 0, false
	}
	number, err := strconv.Atoi(digits)
	if err != nil {
		return "", 0, false
	}
	repo = parts[0] + "/" + parts[1]
	if checkPullRequestKey(repo, number) != nil {
		return "", 0, false
	}
	return repo, number, true
}

// linkOwesARead is whether a link by hand owes a read of its pull request: $1 is the repository
// that the link names, and $2 is the link. The repository is one that Dispatch maps, by its own
// name or as a rename target (repo_projects). No stored head row has the link as its URL, by the
// comparison of the statement (attributedTerms), which pull_request_heads_url_lower serves.
const linkOwesARead = `select exists (select 1 from repo_projects p
			where lower(p.repo) = lower($1::text) or lower(p.renamed_to) = lower($1::text))
		and not exists (select 1 from pull_request_heads h
			where lower(h.url) = lower($2::text) and octet_length(h.url) <= 1024)`

// OweLinkedPullRequestReads owes a read of GitHub for each pull request that a person or an agent
// links to an issue by hand. It writes in tx, the transaction of the link write (api's
// patchIssue). urls are the links that the write adds. This is R5 of the results lookup. No event
// carries a link, so without the owe a pull request with no event and no result has no head row.
// Then the checks of the issue compare with nothing.
//
// A link owes the read only where three things are true. It is the URL of a pull request
// (linkedPullRequest). Dispatch maps its repository, and no stored head row has that URL
// (linkOwesARead). Each other link owes nothing. A stored row has a head that a read of GitHub
// gave, or one that the reconcile reads. While a read is owed, the readers answer that head as not
// read (owedReadJoin). So an owe for a stored row would remove a known head for up to one pass.
//
// The read is owed as a failure (owePullRequestRead), as for an event whose read failed. So the
// next reconcile pass reads it before any refusal (readOwedPullRequests). The owe stays until
// GitHub answers (R4). The owes go in the order of the repository and the number. So two link
// writes lock the rows of pull_request_reads_owed in one order.
func OweLinkedPullRequestReads(ctx context.Context, tx pgx.Tx, urls []string) error {
	type linked struct {
		url, repo string
		number    int
	}
	var links []linked
	for _, url := range urls {
		if repo, number, ok := linkedPullRequest(url); ok {
			links = append(links, linked{url: url, repo: repo, number: number})
		}
	}
	slices.SortFunc(links, func(a, b linked) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.repo), strings.ToLower(b.repo)), cmp.Compare(a.number, b.number))
	})
	for _, link := range links {
		var owes bool
		if err := tx.QueryRow(ctx, linkOwesARead, link.repo, link.url).Scan(&owes); err != nil {
			return fmt.Errorf("read whether the link %s owes a read of its pull request: %w", link.url, err)
		}
		if !owes {
			continue
		}
		if err := owePullRequestRead(ctx, tx, link.repo, link.number, owedFailure, ""); err != nil {
			return err
		}
	}
	return nil
}

// owedRefusalSet is the assignment of refused_as and refused_since where an owed row takes the
// reason next (an expression): the word refusedAs, or the row's own word where refusedAs is null;
// the time since, or the row's own time where the row is a refusal already; and null for both
// where the row becomes a failure. row prefixes the row's own columns ("" in an update).
func owedRefusalSet(next, refusedAs, since, row string) string {
	return `refused_as = case when ` + next + ` = 'refusal' then coalesce(` + refusedAs + `,
					case when ` + row + `reason = 'refusal' then ` + row + `refused_as end) end,
				refused_since = case when ` + next + ` <> 'refusal' then null
					when ` + row + `reason = 'refusal' then ` + row + `refused_since else ` + since + ` end`
}

// refusalOf is the word of GitHub's refusal err of a read (HeadNotRead.RefusedAs): the App is not
// installed on the repository, the App cannot read the repository, or GitHub has no such pull
// request. The errors carry no status as a value (githubapp's errors), so the word names the
// status. It is "" for any other error.
func refusalOf(err error) string {
	switch {
	case errors.Is(err, githubapp.ErrNoInstallation):
		return model.RefusedAsAppNotInstalled
	case errors.Is(err, githubapp.ErrRepositoryUnreadable):
		return model.RefusedAsRepositoryUnreadable
	case errors.Is(err, ErrPullRequestNotFound):
		return model.RefusedAsPullRequestNotFound
	}
	return ""
}

// settleOwedRead deletes the read repoFull's pull request number is owed where the row's revision
// is still revision, the one a read took before it read GitHub (owedRevision,
// listOwedPullRequestReads). That read's answer covers every owe that row recorded, and every owe
// made since gave the row another revision (owePullRequestRead, the triggers of
// pull_request_heads), so its record stays for the next read.
func settleOwedRead(ctx context.Context, pool *store.Pool, repoFull string, number int, revision int64) error {
	if _, err := pool.Exec(ctx, `
		delete from pull_request_reads_owed where repo = $1 and number = $2 and revision = $3
	`, repoFull, number, revision); err != nil {
		return fmt.Errorf("settle the read %s#%d is owed: %w", repoFull, number, err)
	}
	return nil
}

// owedRead is one pull request owed a read: its repository, its number, why, when the read was
// last owed, and the row's revision then.
type owedRead struct {
	Repo     string
	Number   int
	Reason   owedReason
	OwedAt   time.Time
	Revision int64
}

// listOwedPullRequestReads is at most limit pull requests owed a read for reason, the row the
// reconcile has read the fewest times first and then the one owed longest, so every row has its
// first read before any row has its second. The index pull_request_reads_owed_order serves the
// order, so the cost is the limit's rows and not the table's.
func listOwedPullRequestReads(ctx context.Context, pool *store.Pool, reason owedReason, limit int) ([]owedRead, error) {
	rows, err := pool.Query(ctx, `
		select repo, number, reason, owed_at, revision from pull_request_reads_owed
		where reason = $1
		order by reads, owed_at, repo, number
		limit $2
	`, reason, limit)
	if err != nil {
		return nil, fmt.Errorf("list the pull requests owed a read (%s): %w", reason, err)
	}
	defer rows.Close()
	var owed []owedRead
	for rows.Next() {
		var read owedRead
		if err := rows.Scan(&read.Repo, &read.Number, &read.Reason, &read.OwedAt, &read.Revision); err != nil {
			return nil, fmt.Errorf("list the pull requests owed a read (%s): %w", reason, err)
		}
		owed = append(owed, read)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the pull requests owed a read (%s): %w", reason, err)
	}
	return owed, nil
}

// owedReadOutcome is how the reconcile's read of an owed pull request ended.
type owedReadOutcome int

const (
	// owedReadAnswered: GitHub answered for the pull request, and its row is written; or one
	// answer of GitHub shows the repository readable and holding no such pull request
	// (pullRequestAbsent).
	owedReadAnswered owedReadOutcome = iota
	// owedReadFailed: GitHub did not answer (owedFailure).
	owedReadFailed
	// owedReadRefused: GitHub answered that it cannot give the pull request, and nothing shows
	// the refusal final (owedRefusal).
	owedReadRefused
)

// recordOwedReadOutcome records how the reconcile's read of read ended. An answer deletes it. Any
// other outcome counts the read and moves the row to the kind the read gave, so a failure GitHub
// now refuses becomes a refusal and a refusal GitHub now fails becomes a failure. Every statement
// names the revision the list read (settleOwedRead), so an event owed again while the reconcile
// read GitHub keeps its record, as that event recorded it, for the next pass. The count is no owe
// and gives no new revision, so an intake's read that read the row before it still deletes it.
// refusedAs is the word of GitHub's refusal for owedReadRefused (refusalOf), and "" otherwise. It
// sets refused_as and refused_since as owePullRequestRead does (owedRefusalSet).
func recordOwedReadOutcome(ctx context.Context, pool *store.Pool, read owedRead, outcome owedReadOutcome, refusedAs string) error {
	if outcome == owedReadAnswered {
		return settleOwedRead(ctx, pool, read.Repo, read.Number, read.Revision)
	}
	reason := owedFailure
	if outcome == owedReadRefused {
		reason = owedRefusal
	}
	if _, err := pool.Exec(ctx, `
		update pull_request_reads_owed set reason = $4::text, reads = reads + 1,
			`+owedRefusalSet("$4::text", "nullif($5::text, '')", "now()", "")+`
		where repo = $1 and number = $2 and revision = $3
	`, read.Repo, read.Number, read.Revision, string(reason), refusedAs); err != nil {
		return fmt.Errorf("count the read %s#%d is owed: %w", read.Repo, read.Number, err)
	}
	return nil
}

// owedReadJoin is the lateral join of the owed read of each pull request `a` of the statement's
// terms answers (attributedTerms): the row owed under a.repo, the canonical name, or under a name
// that repo_projects renames to it. The owed row's own name goes through the same rule, so a read
// owed under an old name matches, and so does one owed under the name given where the canonical
// name is no name GitHub can have (owePullRequestRead). A refusal comes first. o.reason is null
// where no read is owed, and o.refused_as and o.refused_since are null unless the row is a
// refusal: a trigger that owes a failure over a refusal row leaves both as they were. The index
// pull_request_reads_owed_repo_number serves the probe.
var owedReadJoin = `left join lateral (
		select o.reason, case when o.reason = 'refusal' then o.refused_as end as refused_as,
			case when o.reason = 'refusal' then o.refused_since end as refused_since
		from pull_request_reads_owed o
		where lower(o.repo) = any(array(select lower(a.repo) union select lower(r.repo) from repo_projects r
				where r.renamed_to is not null and lower(r.renamed_to) = lower(a.repo)))
			and octet_length(o.repo) <= 1024 and o.number = a.number
			and lower(` + refs.CanonicalRepoSQL("o.repo") + `) = lower(a.repo)
		order by o.reason = 'refusal' desc limit 1) o on true`

// owedReadsOfRepository is the read of the name and the revision of each read owed in one
// repository, $1 its canonical name, through the same rule as owedReadJoin: the snapshot that a
// read-through takes before its list read (ReadThroughCommitPulls).
var owedReadsOfRepository = `select o.repo, o.number, o.revision from pull_request_reads_owed o
	where lower(o.repo) = any(array(select lower($1::text) union select lower(r.repo) from repo_projects r
			where r.renamed_to is not null and lower(r.renamed_to) = lower($1::text)))
		and octet_length(o.repo) <= 1024
		and lower(` + refs.CanonicalRepoSQL("o.repo") + `) = lower($1::text)`

// owedReadsIn is the name, the number and the revision of each read owed in repoFull, the
// canonical name, as a read of GitHub begins (owedReadsOfRepository). A read that fails answers
// nothing, so the read-through settles nothing and the reconcile reads each one again.
func owedReadsIn(ctx context.Context, pool *store.Pool, repoFull string) map[int][]owedRead {
	bounded, cancel := context.WithTimeout(ctx, owedRevisionReadTimeout)
	defer cancel()
	rows, err := pool.Query(bounded, owedReadsOfRepository, repoFull)
	if err != nil {
		logging.FromContext(ctx).Warn("dispatch delivery: read the reads owed in a repository; this read settles none, and the reconcile reads each once more",
			"repo", repoFull, "error", err)
		return nil
	}
	defer rows.Close()
	owed := map[int][]owedRead{}
	for rows.Next() {
		var read owedRead
		if err := rows.Scan(&read.Repo, &read.Number, &read.Revision); err != nil {
			logging.FromContext(ctx).Warn("dispatch delivery: read the reads owed in a repository; this read settles none, and the reconcile reads each once more",
				"repo", repoFull, "error", err)
			return nil
		}
		owed[read.Number] = append(owed[read.Number], read)
	}
	if err := rows.Err(); err != nil {
		logging.FromContext(ctx).Warn("dispatch delivery: read the reads owed in a repository; this read settles none, and the reconcile reads each once more",
			"repo", repoFull, "error", err)
		return nil
	}
	return owed
}
