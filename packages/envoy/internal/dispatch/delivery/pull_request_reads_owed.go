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
// And a link by hand records a failure for each pull request that it adds and whose head row is
// not stored (OweLinkedPullRequestReads). No event carries a link. An envelope leaves the
// intake with no record of its pull request only where it names no pull request GitHub can have
// (Intake.deliver), which is checked before anything is read or written (pullRequestNamed). A
// GitHub answer that holds a value no head row holds is a head not stored, and owed
// (errNoRowHolds). Every failure of a statement of the record is the database not taking it now,
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
// no basis, and no check run is read at it. An owe that a link by hand alone made is not such an
// owe: a link says nothing of the head, so the three readers skip a row whose linked_revision is
// its revision where a results row is stored. Each other owe gives the row a new revision, so the
// row hides the head again, whichever of the two owes came first (OweLinkedPullRequestReads).
// BaseBranchMergedFor does not read this table: GitHub never changes the state or the base branch
// of a merged pull request, so a merged row answers its base branch also while a read is owed for
// it. The readers by key and by repository (PullRequestIssues, PullRequestsNotKnown) do not read
// it either. A refusal row names how GitHub refuses the read (refused_as) and since when
// (refused_since), so a reader can name the state and its way out. A release before this one
// neither reads nor writes the three columns refused_as, refused_since and linked_revision.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
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
	// hand of a pull request owes one too (OweLinkedPullRequestReads).
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
// same question for each that GitHub now answers 404, up to PullRequestHeadBatch more, so the owed
// reads of a pass ask up to 250 in all (AbsenceQueriesPerPass), and the completions of the partial
// pull requests ask within what they leave. A repository the App is not installed on costs one
// installation lookup a pass however many of its refusals the pass reads (passInstallations).
// During an outage the refusals a pass reads fail and become failures, so they take failure reads
// after it. A refusal of a number a repository the App reads does not have ends at its first read,
// so N such forged envelopes are gone after about N/50 passes; a refusal in a repository the App
// cannot read stays, and is read at most once a pass, the fewest reads first.
const RefusedReadsPerPass = 50

// owedReadWriteTimeout bounds each statement of the intake that can wait on a lock of
// pull_request_reads_owed or of its revision sequence (owedTableBound), so a lock of that table,
// as a migration of it holds one, holds a delivery, and one of the intakeMaxAckPending slots its
// repository shares, for seconds and not for the message's whole deadline (acceptance of
// cybermonklabs/legion#4209 at 3a21edb2c9; its reviews at ac562e9837 and 5ea6b2ffb2, N1). Its
// value is 5 s in every build: seconds, against the message's deadline of a minute. A var, as
// intakeMessageTimeout is, so a test of a lock shrinks it (shrinkOwedReadWriteTimeout) and waits
// out a bound of a second rather than of five at each statement the lock holds.
var owedReadWriteTimeout = 5 * time.Second

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

// owedRevision is the row of the read repoFull's pull request number is owed as a read of GitHub
// begins, nil where none is owed: its own name, its number and its revision, the one row that
// read's answer covers and may delete (settleOwedRead, by that name). The row is found in any
// letter case (owedRowInAnyCase), as every other reader of an owed read finds it: a link writes
// the owe in the link's letters, and the event of the same pull request names the repository in
// GitHub's, so a read in exact case found no row, settled nothing, and the reconcile read the pull
// request once more. Where the read fails or runs past owedRevisionReadTimeout, it answers nil with
// one log line, so the read deletes nothing: the record stays, and the reconcile's next pass reads
// the pull request once more and settles it. That costs one read, and loses no owe.
func owedRevision(ctx context.Context, pool *store.Pool, repoFull string, number int) *owedRead {
	bounded, cancel := context.WithTimeout(ctx, owedRevisionReadTimeout)
	defer cancel()
	row := owedRead{Number: number}
	err := pool.QueryRow(bounded, `select o.repo, o.revision from `+owedRowInAnyCase, repoFull, number).Scan(&row.Repo, &row.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		logging.FromContext(ctx).Warn("dispatch delivery: read the revision of the read a pull request is owed; this read deletes no owed read, and the reconcile reads the pull request once more",
			"repo", repoFull, "number", number, "error", err)
		return nil
	}
	return &row
}

// owePullRequestRead records that the pull request number of the repository given changed, that
// no read of GitHub has answered for that change, and why (reason). given is canonicalised first
// (Attributor.canonical, over the pairs the boot read), so one pull request is owed under one
// name. Recording it again gives the row a new revision, moves its time to now and its reason to
// the latest read's: the latest event is the one a later read must answer for, and a read that
// read the row's revision before this owe settles nothing (settleOwedRead). It keeps the count of
// the reconcile's reads, so publishing the same envelope again moves no row ahead of the rows
// that have been read as often.
//
// The row is built here, and its values are checked here, before any statement: the name given,
// with the number, must be a pull request GitHub can have (checkPullRequestKey), else the answer
// is errNoPullRequestNamed and nothing is written. The canonical name is the one value of the row
// no envelope gives: it is the new name of a rename pair, and the Renames the boot read holds no
// pair whose new name GitHub cannot hold (refs.NewRenames drops one, by refs.SplitGitHubRepo,
// which takes no name checkPullRequestKey refuses), so the canonical name of a name the check
// takes is one the check takes too, and a read is never owed under a name no read can reach. The
// reason is one of this package's two constants. So no value this writes is one the table
// refuses, and a failure of a statement is the database's alone, which the intake hands back
// (recordRefused). Its statements run within owedReadWriteTimeout of the call. The check and the
// canonical name are owedName's, which a link by hand shares (oweLinkedRead).
//
// refusedAs is the word of GitHub's refusal where reason is owedRefusal (refusalOf), and "" for a
// failure. A row that becomes a refusal takes the time of this write as refused_since, a row that
// stays one keeps its time, and a row that becomes a failure clears both columns (owedRefusalSet).
func (a *Attributor) owePullRequestRead(ctx context.Context, pool *store.Pool, given string, number int, reason owedReason, refusedAs string) error {
	ctx, cancel := owedTableBound(ctx)
	defer cancel()
	repoFull, err := a.owedName(given, number)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
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

// owedName is the name that the read of pull request number of the repository given is owed
// under (owePullRequestRead): its canonical name. Where given and number are no pull request
// GitHub can have, it answers errNoPullRequestNamed.
func (a *Attributor) owedName(given string, number int) (string, error) {
	if err := checkPullRequestKey(given, number); err != nil {
		return "", fmt.Errorf("the read %.140q#%d is owed: %w", given, number, err)
	}
	return a.canonical(given), nil
}

// pullRequestURLPrefix is how the URL of each pull request that GitHub gives (html_url) starts.
const pullRequestURLPrefix = "https://github.com/"

// linkedPullRequest is the repository and the number of the pull request link names, with ok
// true: the one rule of which link names which pull request. GitHub gives a pull request's own
// URL as https://github.com/<owner>/<repo>/pull/<number>, and serves the same pull request at
// https://github.com/<owner>/<repo>/issues/<number>, which is the URL
// `dispatch issue --external <owner>/<repo>#<number>` stores (api's externalURL): a pull request
// and an issue of one repository share one sequence of numbers, so either form names at most one
// of them. So this takes those two forms in any letter case, and no other: no other part, no
// character outside ASCII, and no zero before the number. An /issues/ link that names an issue
// owes one read, which GitHub answers as no pull request and the reconcile settles
// (pullRequestAbsent). ok is false for each other URL, for a number that no pull request of
// GitHub can have (checkPullRequestKey), and for an owner or a name that no repository of GitHub
// can have, by the rule every read of GitHub splits a name by (refs.SplitGitHubRepo): an owner
// that starts with `-` or `_`, and a name that is empty, `.` or `..` once a trailing `.git` is
// trimmed, such as `acme/.git` and `acme/..git`. No read can reach such a repository, so a link of
// it owes no read that each pass would make and refuse. The repository keeps the letters of the
// link.
//
// A link names the pull request under the name its repository is called today
// (Attributor.canonical), so a link written before a rename names the pull request of the
// repository it was renamed to. The link write owes the read under that name (owedName), and the
// statement gives the pull request to the linking issue under it (LinkedPullRequestSQL,
// PullRequestLinksSQL, attributedTerms).
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
	if len(parts) != 4 || !(strings.EqualFold(parts[2], "pull") || strings.EqualFold(parts[2], "issues")) {
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
	if _, _, err := refs.SplitGitHubRepo(repo); err != nil {
		return "", 0, false
	}
	return repo, number, true
}

// LinkedPullRequestSQL is linkedPullRequest in SQL, a subquery for a lateral join over url, an
// expression of a link's URL: one row (repo, number) where linkedPullRequest takes the link, the
// repository as the link spells it, and no row where it does not. The statement reads the links
// of an issue by it (byIssueJudged), and api's link check by it, so a reader of the database and
// the link write take one link to one pull request. TestTheLinkRuleInSQLIsTheLinkRuleInGo holds
// it to linkedPullRequest link by link.
//
// The owner's pattern is refs.GitHubOwnerPattern and the name's refs.GitHubNamePattern, with the
// name's bound (refs.GitHubNameMax), split at the slash, with the two parts of the path after
// them; the scheme and the host are compared in lower case, and `pull` and `issues` in any case.
// The name, lowercased and with one trailing `.git` trimmed, must not be empty, `.` or `..`, as
// refs.SplitGitHubRepo holds it. A link of a character outside ASCII is no link of a pull request,
// as in Go: octet_length counts the bytes of UTF-8 text and length its characters. The number is
// cast to int only where it is at most math.MaxInt32, inside a case, so no order the planner gives
// the conditions casts a number of ten digits past int's bound.
func LinkedPullRequestSQL(url string) string {
	prefix := strconv.Itoa(len(pullRequestURLPrefix))
	inBound := `p.m[4]::bigint <= ` + strconv.Itoa(math.MaxInt32)
	return `(select p.m[1] || '/' || p.m[2] as repo, case when ` + inBound + ` then p.m[4]::int end as number
		from (select regexp_match(lower(left(` + url + `, ` + prefix + `)) || substr(` + url + `, ` + prefix + ` + 1),
			'^https://github\.com/(` + refs.GitHubOwnerPattern + `)/(` + refs.GitHubNamePattern + `)/([A-Za-z]+)/([1-9][0-9]{0,9})$') as m
			where octet_length(` + url + `) = length(` + url + `)) p
		where lower(p.m[3]) in ('pull', 'issues') and p.m[2] not in ('.', '..') and length(p.m[2]) <= ` + strconv.Itoa(refs.GitHubNameMax) + `
			and regexp_replace(lower(p.m[2]), '\.git$', '') not in ('', '.', '..')
			and case when ` + inBound + ` then true else false end)`
}

// PullRequestLinksSQL is every link that names pull request number of repo (linkedPullRequest),
// lowercased, as an array: the /pull/ and the /issues/ form of number under each name whose
// canonical name is repo's (refs.CanonicalRepoSQL over renames, the pairs the boot read), that is
// the canonical name itself and each old name of a pair renamed to it (linkFormsOf). repo and
// number are expressions, a column or a bound parameter each. A reader compares lower() of a link
// with the array, which issue_external_links_url_lower serves with one probe a form, and takes
// the link only where LinkedPullRequestSQL takes it too: lower() of a character outside ASCII can
// be an ASCII letter, and a renamed name that GitHub cannot have names nothing. api's link check
// reads it for one link; the statement, which judges many pull requests, gives linkFormsOf the
// names it read once (attributedTerms, held_names), which are the names this array is made of.
//
// The names come from the rule's inverse (refs.NamesSQL): the lowered names whose canonical name
// is repo's in any case, which are the canonical name itself where it is its own canonical name
// and each old name of a pair renamed to it. The pairs are literals of the text, so the array
// reads no table. repo's canonical name is read once, as c.
func PullRequestLinksSQL(renames refs.Renames, repo, number string) string {
	return linkFormsOf(`(select `+refs.NamesSQL(renames, "c.canonical")+`
		from (select `+refs.CanonicalRepoSQL(renames, repo)+` as canonical) c)`, number)
}

// linkFormsOf is the lowered /pull/ and /issues/ link of pull request number under each name of
// names, an expression of a text array of names, as an array: the forms of PullRequestLinksSQL.
func linkFormsOf(names, number string) string {
	return `array(select lower('https://github.com/' || link_name.name || '/' || link_form.form || '/' || ` + number + `)
		from unnest(` + names + `) link_name(name) cross join (values ('pull'), ('issues')) link_form(form))`
}

// OweLinkedPullRequestReads owes a read of GitHub for each pull request that a person or an agent
// links to an issue by hand, where Dispatch holds no head of it. It writes in tx, the transaction
// of the link write (api's patchIssue). urls are the links that the write adds. This is R5 of the
// results lookup: Dispatch reads the linked pull request from GitHub and stores its head, as for
// an event. No event carries a link, so without the owe a pull request with no event and no
// result has no head row, and the checks of the issue compare with nothing.
//
// Each URL of a pull request owes the read (linkedPullRequest), in any repository, under the name
// the repository is called today (owedName): the read needs only that GitHub can read the
// repository, not that Dispatch maps it. Each other link owes nothing. Where GitHub does not let
// the App read the repository, the read stays owed until a read succeeds (R4), as a refusal after
// its first read (recordOwedReadOutcome).
//
// A link owes no read where a head row of the pull request is stored (oweLinkedRead): a link says
// nothing of the head, the row holds the head that a read of GitHub or an event gave, and the
// statement ties the pull request to the linking issue from the stored texts at the next read
// (R7), so a read would ask GitHub what Dispatch holds already. A pull request whose head moves
// gets its event, or the reconcile's open arm, as every other does.
//
// Where no head row is stored, a new owe takes its revision as linked_revision, and the readers by
// issue skip a row whose linked_revision is its revision where a results row is stored
// (owedReadJoin): a head row that another writer stores before the owed read settles is no head
// not read for the link's sake. A read of the row that fails or that GitHub refuses keeps its
// revision, so it hides nothing either. Each other owe gives the row a new revision: an event's, a
// refusal's, and that of a trigger of pull_request_heads. So the row hides the head from then on. A
// link over a row that is owed already gives it a new revision and changes nothing else: its
// reason, its refusal, and whether it hides the head stay. So no order of a link and another owe
// hides less than the other owe alone, and a read that began before the later owe settles neither
// (settleOwedRead).
//
// A new row is a failure, as for an event whose read failed. So the next reconcile pass reads it
// before any refusal (readOwedPullRequests), and a read of GitHub that answers settles it. A pull
// request is owed once in every letter case: a row owed already under another spelling takes the
// owe (oweLinkedRead), and two links of one pull request in two spellings owe one row, under the
// spelling first in byte order. The owes go in the order of the owed name in lower case, the
// number and the owed name. So two link writes lock the rows of pull_request_reads_owed in one
// order. The backfill of the links stored before this release owes what this owes for those links
// (migration 20261120171308_pull_request_reads_owed_link_backfill), which its test holds by
// running this over the same links (TestTheLinkBackfillOwesWhatALinkWriteOwes).
func (a *Attributor) OweLinkedPullRequestReads(ctx context.Context, tx pgx.Tx, urls []string) error {
	type owed struct {
		repo   string
		number int
	}
	var links []owed
	for _, url := range urls {
		given, number, ok := linkedPullRequest(url)
		if !ok {
			continue
		}
		repoFull, err := a.owedName(given, number)
		if err != nil {
			return err
		}
		links = append(links, owed{repo: repoFull, number: number})
	}
	slices.SortFunc(links, func(a, b owed) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.repo), strings.ToLower(b.repo)), cmp.Compare(a.number, b.number),
			strings.Compare(a.repo, b.repo))
	})
	for _, link := range links {
		if err := a.oweLinkedRead(ctx, tx, link.repo, link.number); err != nil {
			return err
		}
	}
	return nil
}

// owedRowInAnyCase is the FROM clause of the one owed row of pull request $2 of the repository $1
// in any letter case, aliased o: the row first in byte order of its repository's name where an
// earlier release left two spellings. oweLinkedRead owes into that row, and UnreadableLinksOf reads
// it, so a link names the row its owe wrote however the link and the row are spelled. A pull
// request is owed once in any case, since GitHub reads a name in any case.
const owedRowInAnyCase = `pull_request_reads_owed o where lower(o.repo) = lower($1) and o.number = $2
	order by o.repo collate "C" limit 1`

// oweLinkedRead owes the read of pull request number of repoFull, the owed name of a link by hand
// (OweLinkedPullRequestReads), within the bound of owePullRequestRead, where no head row of that
// pull request is stored: a row whose repository, under its canonical name, is repoFull in any
// letter case, and whose number is number. Where one is stored it writes nothing. A row owed
// already under repoFull in another letter case is the row it owes: GitHub reads a name in any
// case, so a second row would cost a second read of one pull request. A new row is a failure
// whose linked_revision is its revision. A row that is owed already takes a new revision and the
// time of now, and keeps its reason, its refusal and its count of reads. It keeps linked_revision
// equal to its revision only where a link alone owed the read before.
func (a *Attributor) oweLinkedRead(ctx context.Context, tx pgx.Tx, repoFull string, number int) error {
	ctx, cancel := owedTableBound(ctx)
	defer cancel()
	var held bool
	var owedAs *string
	if err := tx.QueryRow(ctx, `
		select exists (select 1 from pull_request_heads h
			where h.number = $2 and lower(`+refs.CanonicalRepoSQL(a.renames, "h.repo")+`) = lower($1)),
			(select o.repo from `+owedRowInAnyCase+`)
	`, repoFull, number).Scan(&held, &owedAs); err != nil {
		return fmt.Errorf("read whether the head of %s#%d is stored for a link: %w", repoFull, number, err)
	}
	if held {
		return nil
	}
	if owedAs != nil {
		repoFull = *owedAs
	}
	return recordLinkedOwe(ctx, tx, repoFull, number)
}

// recordLinkedOwe writes the owe of a link by hand of pull request number of repoFull, the owed
// name (oweLinkedRead), in tx. A new row is a failure whose linked_revision is its revision. A row
// that is owed already takes a new revision and the time of now, and keeps its reason, its refusal
// and its count of reads; it keeps linked_revision equal to its revision only where a link alone
// owed the read before. oweLinkedRead writes it only where no head row is stored, so a head row
// beside such a row is one that another writer stored after the link, whose read began before the
// owe and whose settle therefore leaves it (settleOwedRead).
func recordLinkedOwe(ctx context.Context, tx pgx.Tx, repoFull string, number int) error {
	if _, err := tx.Exec(ctx, `
		insert into pull_request_reads_owed (repo, number, reason, revision, linked_revision)
		select $1, $2, $3::text, r.revision, r.revision
		from (select nextval('pull_request_reads_owed_revision_seq') as revision) r
		on conflict (repo, number) do update
			set owed_at = excluded.owed_at, revision = excluded.revision,
				linked_revision = case when pull_request_reads_owed.linked_revision = pull_request_reads_owed.revision
					then excluded.revision end
	`, repoFull, number, string(owedFailure)); err != nil {
		return fmt.Errorf("record the read %s#%d is owed for a link: %w", repoFull, number, err)
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

// settleOwedRead deletes the read pull request number is owed under repoFull, the row's own name
// as the read that took its revision read it (owedRevision, listOwedPullRequestReads,
// owedReadsIn), where the row's revision is still revision. That read's answer covers every owe
// that row recorded, and every owe made since gave the row another revision (owePullRequestRead,
// the triggers of pull_request_heads), so its record stays for the next read.
func settleOwedRead(ctx context.Context, db refs.Execer, repoFull string, number int, revision int64) error {
	if _, err := db.Exec(ctx, `
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
// first read before any row has its second. A row recorded as unreadable at its revision is not
// listed (recordOwedReadOutcome): GitHub refused its read because the App is not installed on its
// repository, and only a new owe or an installation that comes to cover the repository makes a
// read of it worth asking again (wakeUnreadableReads). The index pull_request_reads_owed_order
// serves the order, so the cost is the limit's rows and the unreadable rows before them, not the
// table's.
func listOwedPullRequestReads(ctx context.Context, pool *store.Pool, reason owedReason, limit int) ([]owedRead, error) {
	rows, err := pool.Query(ctx, `
		select repo, number, reason, owed_at, revision from pull_request_reads_owed
		where reason = $1 and unreadable_revision is distinct from revision
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
//
// A refusal because the App is not installed on the repository, of a read that a link by hand
// alone owed (linked_revision is the revision), records the row as unreadable at its revision
// (unreadable_revision), and no pass reads it again (listOwedPullRequestReads). Such a link names
// a pull request or an issue of a repository the App does not cover, as a bug report of another
// project does, and every later read would be refused the same way. The row stays, so the record
// of the read is kept (R4), and the issue's results name the link (UnreadableLinksOf). A new
// link, an event or a trigger gives the row a new revision, so the next pass reads it again, and
// a pass makes it readable again once the App's installations cover the repository
// (wakeUnreadableReads). Every other outcome leaves the row readable.
func recordOwedReadOutcome(ctx context.Context, db refs.Execer, read owedRead, outcome owedReadOutcome, refusedAs string) error {
	if outcome == owedReadAnswered {
		return settleOwedRead(ctx, db, read.Repo, read.Number, read.Revision)
	}
	reason := owedFailure
	if outcome == owedReadRefused {
		reason = owedRefusal
	}
	if _, err := db.Exec(ctx, `
		update pull_request_reads_owed set reason = $4::text, reads = reads + 1,
			`+owedRefusalSet("$4::text", "nullif($5::text, '')", "now()", "")+`,
			unreadable_revision = case when $5::text = $6::text and linked_revision = revision then revision end
		where repo = $1 and number = $2 and revision = $3
	`, read.Repo, read.Number, read.Revision, string(reason), refusedAs, model.RefusedAsAppNotInstalled); err != nil {
		return fmt.Errorf("count the read %s#%d is owed: %w", read.Repo, read.Number, err)
	}
	return nil
}

// owedReadJoin is the lateral join of the owed read of each pull request `a` of the statement's
// terms answers (attributedTerms): the row owed under a.repo, the canonical name, or under an old
// name of a pair renamed to it, which a.names holds, read once a statement (namesOver). The
// owed row's own name goes through the same rule, so a read owed under an old name matches. A row
// that a link by hand alone owed, whose linked_revision is its
// revision, is skipped where the statement holds a results row of the pull request (h, the alias
// of each reader): a link says nothing of the head, so that owe leaves the stored head a basis
// (OweLinkedPullRequestReads). Where no results row is stored, there is no head to hide, and such
// a row names its refusal as each other row does. A refusal comes first. o.reason is null where no
// read is owed, and o.refused_as and o.refused_since are null unless the row is a refusal: a
// trigger that owes a failure over a refusal row leaves both as they were. The index
// pull_request_reads_owed_repo_number serves the probe.
var owedReadJoin = `left join lateral (
		select o.reason, case when o.reason = 'refusal' then o.refused_as end as refused_as,
			case when o.reason = 'refusal' then o.refused_since end as refused_since
		from pull_request_reads_owed o
		where lower(o.repo) = any(a.names)
			and octet_length(o.repo) <= 1024 and o.number = a.number
			and (o.linked_revision is distinct from o.revision or h.repo is null)
		order by o.reason = 'refusal' desc limit 1) o on true`

// owedReadsOfRepository is the read of the name and the revision of each read owed in one
// repository, $1 its canonical name, through the same rule as owedReadJoin, also each row that a
// link by hand alone owed: the snapshot that a read-through takes before its list read
// (ReadThroughCommitPulls). The names are the rule's inverse of $1 (refs.NamesSQL over renames).
func owedReadsOfRepository(renames refs.Renames) string {
	return `select o.repo, o.number, o.revision from pull_request_reads_owed o
	where lower(o.repo) = any(` + refs.NamesSQL(renames, "$1::text") + `)
		and octet_length(o.repo) <= 1024`
}

// owedReadsIn is the name, the number and the revision of each read owed in repoFull, the
// canonical name, as a read of GitHub begins (owedReadsOfRepository). A read that fails answers
// nothing, so the read-through settles nothing and the reconcile reads each one again.
func (a *Attributor) owedReadsIn(ctx context.Context, q refs.Queryer, repoFull string) map[int][]owedRead {
	bounded, cancel := context.WithTimeout(ctx, owedRevisionReadTimeout)
	defer cancel()
	rows, err := q.Query(bounded, owedReadsOfRepository(a.renames), repoFull)
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
