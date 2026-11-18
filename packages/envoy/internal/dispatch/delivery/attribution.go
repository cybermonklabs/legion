package delivery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A pull request's issue is the first issue its five ranked sources name that resolves to the
// repository the pull request is in: a candidate ranked above it that resolves to another
// repository, or to no repository at all, is passed over, and a pull request no candidate of
// which resolves here is attributed to nobody, with the source "none". Every candidate of one
// source ranks above any of the next (attributedTerms):
//
//  1. the issue whose external link is the pull request's own URL;
//  2. the issue whose external link is a GitHub issue the body cites, a full
//     github.com/<owner>/<repo>/issues/<n> URL or a bare #N in the pull request's own repository,
//     the first such citation in body order;
//  3. a key in its head branch name, whatever its case;
//  4. a key in its title or body, title first;
//  5. a key in its commit messages, in commit order.
//
// The prototype's collector/attribution.py tried the title and the body first, and ranks 1 to 3
// below them. R8 of INFRA-1522 moved them above: a branch name and a link are acts that name the
// issue — someone branched for it, someone linked it — while a key in a title or a body is a
// mention, which a pull request that reverts, follows or merely cites another issue's work
// carries as readily as one that does that work. The commit messages stay last: they are the
// weakest mention of all, and the only source that costs a GitHub call to read (storePullRequestFacts).
//
// The holding to the repository is the server's one repository-resolution rule, spliced into the
// same statement as the candidates (refs.IssueRepoTerms, attributionHeld), so what an
// attribution believes an issue's repository is can never disagree with the issue page.
//
// One statement reads the five sources at read time, from the texts of one read of GitHub and
// from the database (attributedTerms, below). Sources 1 and 2 read Dispatch's external links,
// every source reads whether the issue it names exists, and the holding reads where that issue
// sits. So a new link, a reparent or a new repository mapping reaches every stored pull request
// at its next read, with no read of GitHub and no walk of the table.

// attributionSource names the source a pull request's issue came from.
type attributionSource string

const (
	sourceTitleBody       attributionSource = "title_body"
	sourceExternalLink    attributionSource = "external_link"
	sourceGitHubIssueLink attributionSource = "github_issue_link"
	sourceBranch          attributionSource = "branch"
	sourceCommitMessage   attributionSource = "commit_message"
	sourceNone            attributionSource = "none"
)

// appendUnique appends each of values that list does not hold yet, in order.
func appendUnique(list []string, values ...string) []string {
	for _, value := range values {
		if !slices.Contains(list, value) {
			list = append(list, value)
		}
	}
	return list
}

// attributionCandidateBound is how many ranked candidates one attribution carries out of the
// union. Every candidate costs a repository resolution now — a walk up its parent chain and its
// project's default mapping (refs.IssueRepoTerms) — and a title and a body together can name
// many keys: a Legion pull request names one or two, while a body listing a tree's issues can
// name a dozen, so 25 is room for the shapes that occur and a bound on the work a pathological
// body can ask for. The bound drops only candidates ranked below it, so an issue is passed over
// for the bound's sake alone where the five sources name more than 25 keys and none of the
// first 25 is held to the pull request's repository.
const attributionCandidateBound = 25

// attributionHeld is the condition one candidate passes to be the pull request's issue: it
// resolves, through the server's one repository-resolution rule (refs.IssueRepoTerms, joined by
// refs.IssueRepoJoin in the enclosing statement), to the repository the expression repo reads.
// It is never null — a candidate whose issue row is missing, and one whose parent chain ran out
// of depth, is simply not held — so a caller may read it as a plain boolean and a `where` over
// it drops exactly the candidates that are not held. The comparison is lowercased on both sides
// because GitHub spells an owner and a repository as their owners do (`Acme/Widgets`), and a
// stored repository may be spelled either way.
//
// Both sides go through the server's one repository-renaming rule first
// (refs.CanonicalRepoSQL), so one repository under two names is one repository here: an issue
// recorded under a name that was renamed away is held to a pull request on the new name, and an
// issue recorded under the new name to a pull request Dispatch stored under the old one. A
// repository with no rename is its own canonical name, so a repository Dispatch maps no rename
// for is compared by that name alone.
func attributionHeld(repo string) string {
	return `coalesce(not ` + refs.IssueRepoTooDeep +
		` and lower(` + refs.CanonicalRepoSQL(refs.IssueRepoResolved) + `) = lower(` + refs.CanonicalRepoSQL(repo) + `), false)`
}

// attributionSubjects is the `subjects` term of the statement (attributedTerms): the distinct
// keys its candidates name, which is the set refs.IssueRepoTerms walks. `where key is not null`
// keeps a candidate with no key out of the walk.
const attributionSubjects = `subjects as (
	select distinct key from candidates where key is not null
)`

// One statement gives the issue of a pull request at read time, from its stored facts alone: the
// results row (pull_request_heads), the timeline row (delivery_pull_requests) and the texts of one
// read of GitHub (pull_request_texts), with the keys the database reads from those texts
// (attribution_keys, migration 20261117122117). Every reader by key and by issue runs it, so a
// change of the rule, a new link, a moved issue or a new issue reaches every stored pull request
// at its next read, with no read of GitHub.
//
// The text names the schema of the reading's two functions, which only the database knows, so it
// is built once at start (PrepareAttribution) and never per read.

// githubURLBound is the most bytes a pull request's own URL may have for a link to match it. It
// is the bound of the partial index pull_request_heads_url_lower (migration 20261117111040): a
// longer URL is no URL of GitHub, and no index holds it. Each arm that matches a link with the
// pull request's own URL takes the bound (urlBound), the arm of a reader by key as the arm of a
// reader by issue, so both match the same URLs and the reader by issue reads that index.
const githubURLBound = 1024

// urlBound is the condition each arm on a pull request's own URL takes: the predicate of the
// partial index, so the planner reads it.
func urlBound(column string) string {
	return `octet_length(` + column + `) <= ` + strconv.Itoa(githubURLBound)
}

// linkURLAsCited is a link's URL folded the way the reading folds a cited URL: ASCII lower case,
// under the collation "C", whatever the database's own collation folds besides. The cited list
// is in that fold, so an arm that compares a link with it compares in that fold too, by key and by
// issue alike, and the answer is the same in every database the guard of migration 20261117111040
// admits. A pair equal in that fold is equal under lower() too, so the arm by key keeps lower() as
// its first term, which issue_external_links_url_lower serves. The folded text takes the
// database's collation again, the collation of the cited list and of its index
// (pull_request_texts_keys): an overlap of the list with an array under "C" reads no index.
const linkURLAsCited = `(lower(l.url collate "C") collate "default")`

// attributedTerms is the statement's terms over judged, a query of (repo, number) under any
// stored name, for the schema that holds the reading. A caller adds its own select over
// `attributed`, one row for each judged pull request: repo (the canonical name), number,
// stored_repo and timeline_repo (the names its results row and its timeline row are stored under,
// null where it has none), has_facts, known, fallback, issue_key and source.
//
//   - now_reading: the identity of the reading that runs now, and whether any texts row is on
//     another. Each of min and max is one probe of pull_request_texts_reading, whatever the
//     statistics say.
//   - judged: each judged pull request once, under its canonical name (refs.CanonicalRepoSQL).
//   - heads and timeline: its results row and its timeline row under any name the renaming rule
//     reads as that name, the row under the canonical name first, then the results row read from
//     GitHub last. limit 1 keeps each join one probe of the index on number.
//   - batch: its texts, joined to that results row. The fallback is the results row's stored
//     answer, read or unread, also when that is nobody, and the timeline row's issue only where
//     no results row exists: the one fallback of every reader. The texts are facts only where they
//     describe the results row's head (texts_current). A row's stored keys stand only where its
//     reading is the one that runs now; else the keys are read from its texts here. offset 0 keeps
//     the planner from making that condition a join filter, which would run the reading for every
//     row.
//   - named to picked: the five sources in the order of the list at the top of this file, from
//     facts whose head is the pull request's own repository's (own_head), bounded at
//     attributionCandidateBound and held to the pull request's repository (attributionHeld). A
//     tie of rank and place, two links of one URL in two letter cases, is ended by the key in byte
//     order, so no collation, plan or row order picks the issue.
//   - attributed: one of three states. The rule's answer where the pull request has facts: its
//     texts are current, and its messages are read or a held candidate of a higher source
//     decides. Not known where its texts are current, its messages are not read and no held
//     candidate decides: no issue, the source none, has_facts false and known false. Messages
//     that were not read decide nothing, and the issue that a write stored is no answer either.
//     R4's fallback where the pull request has no current texts.
func attributedTerms(schema, judged string) string {
	return attributedTermsOver(schema, judged, "pull_request_texts")
}

// attributedTermsOver is attributedTerms with the texts read from texts, a relation with the
// columns of pull_request_texts: the table itself for every reader, and the facts of a results
// row's own head alone for the order of the facts line (titleAndBranchTexts).
func attributedTermsOver(schema, judged, texts string) string {
	hasFacts := `b.texts_current and (b.messages_read or p.key is not null)`
	return `with recursive now_reading as materialized (
		select r.id, coalesce((select pg_catalog.min(x.reading) from pull_request_texts x), r.id) <> r.id
			or coalesce((select pg_catalog.max(x.reading) from pull_request_texts x), r.id) <> r.id as other
		from (select ` + schema + `.attribution_reading() as id) r
	), judged_raw as (` + judged + `
	), judged as (
		select distinct on (lower(c.repo), c.number) c.repo, c.number
		from (select ` + refs.CanonicalRepoSQL("r.repo") + ` as repo, r.number from judged_raw r) c
		order by lower(c.repo), c.number, c.repo
	), heads as (
		select j.repo as j_repo, j.number as j_number, h.*
		from judged j cross join lateral (
			select h.repo, h.number, h.url, h.issue_key, h.attribution_source, h.head_sha, h.base_ref, h.event_at,
				h.head_ref, h.title, h.state, h.checked_at
			from pull_request_heads h
			where h.number = j.number and lower(` + refs.CanonicalRepoSQL("h.repo") + `) = lower(j.repo)
			order by lower(h.repo) = lower(j.repo) desc, h.github_read_at desc nulls last, h.repo
			limit 1) h
	), timeline as (
		select j.repo as j_repo, j.number as j_number, d.repo, d.issue_key
		from judged j cross join lateral (
			select d.repo, d.issue_key from delivery_pull_requests d
			where d.number = j.number and lower(` + refs.CanonicalRepoSQL("d.repo") + `) = lower(j.repo)
			order by lower(d.repo) = lower(j.repo) desc, d.repo
			limit 1) d
	), batch as (
		select j.repo, j.number, h.url,
			case when h.repo is null then tl.issue_key else h.issue_key end as fallback,
			h.repo as stored_repo, tl.repo as timeline_repo, h.attribution_source as stored_source,
			coalesce(t.head_sha = h.head_sha and t.base_ref = h.base_ref and t.updated_at is not distinct from h.event_at
				and t.head_ref = h.head_ref and t.title = h.title, false) as texts_current,
			coalesce(t.commit_messages is not null, false) as messages_read,
			coalesce(t.head_repo <> '' and lower(` + refs.CanonicalRepoSQL("t.head_repo") + `) = lower(j.repo), false) as own_head,
			case when t.reading = r.id then t.title_keys else k.title_keys end as title_keys,
			case when t.reading = r.id then t.cited_issues else k.cited_issues end as cited_issues,
			case when t.reading = r.id then t.branch_keys else k.branch_keys end as branch_keys,
			case when t.reading = r.id then t.commit_keys else k.commit_keys end as commit_keys
		from judged j cross join now_reading r
			left join heads h on h.j_repo = j.repo and h.j_number = j.number
			left join ` + texts + ` t on t.repo = h.repo and t.number = h.number
			left join timeline tl on tl.j_repo = j.repo and tl.j_number = j.number
			left join lateral (select f.* from ` + schema + `.attribution_keys(t.repo, t.head_ref, t.title, t.body, t.commit_messages) f
				where t.reading <> r.id offset 0) k on true
	), named as (
		select src.repo as pr_repo, src.number as pr_number, l.issue_key as key, '` + string(sourceExternalLink) + `' as source, 0 as rank, 0::bigint as n
		from batch src cross join lateral (select l.issue_key from issue_external_links l where lower(l.url) = lower(src.url) offset 0) l
		where src.texts_current and src.own_head and ` + urlBound("src.url") + `
		union all
		select src.repo, src.number, l.issue_key, '` + string(sourceGitHubIssueLink) + `', 1, u.n
		from batch src cross join lateral unnest(src.cited_issues) with ordinality u(url, n)
			cross join lateral (select l.issue_key from issue_external_links l
				where lower(l.url) = lower(u.url) and ` + linkURLAsCited + ` = u.url offset 0) l
		where src.texts_current and src.own_head
		union all
		select src.repo, src.number, u.key, '` + string(sourceBranch) + `', 2, u.n
		from batch src cross join lateral unnest(src.branch_keys) with ordinality u(key, n) join issues i on i.key = u.key
		where src.texts_current and src.own_head
		union all
		select src.repo, src.number, u.key, '` + string(sourceTitleBody) + `', 3, u.n
		from batch src cross join lateral unnest(src.title_keys) with ordinality u(key, n) join issues i on i.key = u.key
		where src.texts_current and src.own_head
		union all
		select src.repo, src.number, u.key, '` + string(sourceCommitMessage) + `', 4, u.n
		from batch src cross join lateral unnest(src.commit_keys) with ordinality u(key, n) join issues i on i.key = u.key
		where src.texts_current and src.own_head
	), candidates as (
		select pr_repo, pr_number, key, source, rank, n
		from (select named.*, row_number() over (partition by pr_repo, pr_number order by rank, n, key collate "C") place from named) r
		where place <= ` + strconv.Itoa(attributionCandidateBound) + `
	), ` + attributionSubjects + `, ` + refs.IssueRepoTerms + `, held as (
		select c.pr_repo, c.pr_number, c.key, c.source, c.rank, c.n
		from candidates c left join issues i on i.key = c.key ` + refs.IssueRepoJoin + `
		where ` + attributionHeld("c.pr_repo") + `
	), picked as (
		select distinct on (pr_repo, pr_number) pr_repo, pr_number, key, source
		from held order by pr_repo, pr_number, rank, n, key collate "C"
	), attributed as (
		select b.repo, b.number, b.stored_repo, b.timeline_repo, b.fallback, ` + hasFacts + ` as has_facts,
			(` + hasFacts + `) or not b.texts_current as known,
			case when ` + hasFacts + ` then p.key when b.texts_current then null else b.fallback end as issue_key,
			case when ` + hasFacts + ` then coalesce(p.source, '` + string(sourceNone) + `')
				when b.texts_current then '` + string(sourceNone) + `'
				else coalesce(b.stored_source, '` + string(sourceNone) + `') end as source
		from batch b left join picked p on p.pr_repo = b.repo and p.pr_number = b.number
	)`
}

// headsByIssue is the judged rows of a read by issue $1: every pull request whose answer can be
// $1, under the name it is stored under. Each arm but the last reads one index of migration
// 20261117111040 or of the tables it reads: the results rows and the timeline rows that store
// $1; the texts rows whose stored keys name it; the results rows whose own URL a link of $1
// names, under the bound of the partial index; the texts rows that cite a URL a link of $1 names,
// folded as the reading folds it (linkURLAsCited); and, only while some texts row is on another
// reading, every such row, whose stored keys say nothing. The last is a one-time filter, so it
// reads no row outside that window. A row an arm takes that the statement answers for another
// issue is dropped by the reader's own `issue_key = $1`.
var headsByIssue = `select repo, number from pull_request_heads where issue_key = $1
	union select repo, number from delivery_pull_requests where issue_key = $1
	union select repo, number from pull_request_texts
		where branch_keys @> array[$1::text] or title_keys @> array[$1::text] or commit_keys @> array[$1::text]
	union select h.repo, h.number from pull_request_heads h
		where lower(h.url) in (select lower(l.url) from issue_external_links l where l.issue_key = $1) and ` + urlBound("h.url") + `
	union select repo, number from pull_request_texts
		where cited_issues && array(select ` + linkURLAsCited + ` from issue_external_links l where l.issue_key = $1)
	union select t.repo, t.number from pull_request_texts t
		where (select r.other from now_reading r) and t.reading <> (select r.id from now_reading r)`

// pullRequestKeys is the judged rows of PullRequestIssues: the keys its caller gave, $1 the
// repositories and $2 the numbers.
const pullRequestKeys = `select k.repo, k.number from unnest($1::text[], $2::int[]) k(repo, number)`

// pullRequestIssuesStatement is PullRequestIssues' statement: the answer of each key its caller
// gave, matched under the renaming rule, for each key with a results row or a timeline row.
func pullRequestIssuesStatement(schema string) string {
	return attributedTerms(schema, pullRequestKeys) + `
	select k.repo, k.number, a.issue_key, a.source, a.has_facts, a.known
	from (` + pullRequestKeys + `) k
	join attributed a on a.number = k.number and lower(` + refs.CanonicalRepoSQL("k.repo") + `) = lower(a.repo)
	where a.stored_repo is not null or a.timeline_repo is not null`
}

// attributionStatements are the texts of every reader of a pull request's issue, built for the
// schema that holds the reading.
type attributionStatements struct {
	pullRequestIssues     string
	headsAttributedTo     string
	headAttributedTo      string
	newestPullRequestHead string
	baseBranchMergedFor   string
	syncAttributions      string
	attributionCounts     string
	factsLine             string
	titleAndBranchAnswers string
	remakeKeys            string
	readingOther          string
}

func attributionStatementsFor(schema string) *attributionStatements {
	byIssue := attributedTerms(schema, headsByIssue)
	return &attributionStatements{
		pullRequestIssues:     pullRequestIssuesStatement(schema),
		headsAttributedTo:     byIssue + headsAttributedToSelect,
		headAttributedTo:      byIssue + headAttributedToSelect,
		newestPullRequestHead: byIssue + newestPullRequestHeadSelect,
		baseBranchMergedFor:   byIssue + baseBranchMergedForSelect,
		syncAttributions:      syncAttributionsStatement(schema),
		attributionCounts:     attributedTerms(schema, factsLineJudged) + attributionCountsSelect,
		factsLine:             attributedTerms(schema, factsLineJudged) + factsLineSelect,
		titleAndBranchAnswers: attributedTermsOver(schema, pullRequestKeys, titleAndBranchTexts) + titleAndBranchSelect,
		remakeKeys:            remakeKeysStatement(schema),
		readingOther:          readingOtherStatement(schema),
	}
}

var preparedAttribution atomic.Pointer[attributionStatements]

var errAttributionNotPrepared = errors.New("the statement of a pull request's issue is not built: PrepareAttribution runs after the migrations, before any reader")

func attribution() (*attributionStatements, error) {
	if statements := preparedAttribution.Load(); statements != nil {
		return statements, nil
	}
	return nil, errAttributionNotPrepared
}

// PrepareAttribution builds every reader's statement for the database q reads, once, after the
// migrations and before any reader. It finds the schema that holds pull_request_texts, which the
// statement names before each function of the reading, and then asks the guard of migration
// 20261117111040 (attribution_database_refusal) whether the database can read a pull request's
// issue. A logical restore brings schema_migrations with it and runs neither that migration nor
// its census, so this ask at each start is what refuses a database restored into another encoding
// or collation: the error names it, and the process stops before it listens.
func PrepareAttribution(ctx context.Context, q Querier) error {
	var schema string
	if err := q.QueryRow(ctx, `select pg_catalog.quote_ident(n.nspname) from pg_catalog.pg_class c
		join pg_catalog.pg_namespace n on n.oid = c.relnamespace where c.oid = 'pull_request_texts'::pg_catalog.regclass`).Scan(&schema); err != nil {
		return fmt.Errorf("find the schema of pull_request_texts: %w", err)
	}
	var refusal *string
	if err := q.QueryRow(ctx, `select `+schema+`.attribution_database_refusal()`).Scan(&refusal); err != nil {
		return fmt.Errorf("ask whether the database can read a pull request's issue: %w", err)
	}
	if refusal != nil {
		return errors.New(*refusal)
	}
	preparedAttribution.Store(attributionStatementsFor(schema))
	return nil
}

// PullRequestKey names one pull request as a caller holds it: a repository under any name it was
// stored under, and the number.
type PullRequestKey struct {
	Repo   string
	Number int
}

// PullRequestAttribution is the statement's answer for one pull request: its issue (nil for
// nobody), the source that named it, whether it was read from facts, and whether it is known.
// Without facts and with current texts the answer is not known: no issue, and Known false. Without
// current texts, the issue is the fallback and the source the results row's stored one.
type PullRequestAttribution struct {
	IssueKey *string
	Source   string
	HasFacts bool
	Known    bool
}

// PullRequestIssues is the statement's answer for each of keys, keyed by each key as the caller
// gave it: two keys of one pull request under two names get one answer. A key matches its rows
// under the renaming rule. A key with neither a results row nor a timeline row is absent from the
// map.
func PullRequestIssues(ctx context.Context, q Querier, keys []PullRequestKey) (map[PullRequestKey]PullRequestAttribution, error) {
	statements, err := attribution()
	if err != nil {
		return nil, err
	}
	repos := make([]string, len(keys))
	numbers := make([]int, len(keys))
	for i, key := range keys {
		repos[i], numbers[i] = key.Repo, key.Number
	}
	rows, err := q.Query(ctx, statements.pullRequestIssues, repos, numbers)
	if err != nil {
		return nil, fmt.Errorf("read the issues of %d pull requests: %w", len(keys), err)
	}
	defer rows.Close()
	answers := make(map[PullRequestKey]PullRequestAttribution, len(keys))
	for rows.Next() {
		var key PullRequestKey
		var answer PullRequestAttribution
		if err := rows.Scan(&key.Repo, &key.Number, &answer.IssueKey, &answer.Source, &answer.HasFacts, &answer.Known); err != nil {
			return nil, fmt.Errorf("read the issues of %d pull requests: %w", len(keys), err)
		}
		answers[key] = answer
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the issues of %d pull requests: %w", len(keys), err)
	}
	return answers, nil
}

// PullRequestIssue is the issue of one timeline row as its readers show it: Key is the
// statement's issue, nil for nobody, and NotKnown says that the stored facts decide no issue yet.
type PullRequestIssue struct {
	Key      *string
	NotKnown bool
}

// attributedIssues is the statement's answer for each of prs, in the same order: the readers of
// the timeline (ListPullRequestsInWindow, ListPullRequestsWaitingAt, listUnrecordedShipped) read
// a pull request's issue here, so the timeline and the results name one issue. One statement
// reads all of prs. A pull request with neither a results row nor a timeline row has no answer and
// keeps its stored IssueKey.
func attributedIssues(ctx context.Context, q Querier, prs []DeliveryPullRequest) ([]PullRequestIssue, error) {
	issues := make([]PullRequestIssue, len(prs))
	if len(prs) == 0 {
		return issues, nil
	}
	keys := make([]PullRequestKey, len(prs))
	for i, pr := range prs {
		keys[i] = PullRequestKey{Repo: pr.Repo, Number: pr.Number}
	}
	answers, err := PullRequestIssues(ctx, q, keys)
	if err != nil {
		return nil, err
	}
	for i, pr := range prs {
		answer, found := answers[keys[i]]
		if !found {
			issues[i] = PullRequestIssue{Key: pr.IssueKey}
			continue
		}
		issues[i] = PullRequestIssue{Key: answer.IssueKey, NotKnown: !answer.Known}
	}
	return issues, nil
}

// One pass keeps every stored row of a pull request on the answer of the statement: the
// release before reads pull_request_heads.issue_key and delivery_pull_requests.issue_key, and
// both hold what this build's readers answer after the pass (SyncAttributions). A results row
// that takes another issue queues a move (pull_request_attribution_moves), and the pass appends
// the events of at most AttributionMovesPerPass moves (AppendAttributionEvents).

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

// AttributionMovesPerPass is the most queued moves one pass appends the events of. One drain of
// 500 rows took 8 ms to 9 ms. So n changes need ceil(n / 500) passes for their events.
const AttributionMovesPerPass = 500

// AttributionSync is what one SyncAttributions wrote: the moves it queued, one for each pull
// request whose results row took another issue, and the timeline rows it wrote.
type AttributionSync struct{ Changes, TimelineRows int }

// syncJudged is the judged rows of SyncAttributions: every stored pull request, under each name
// it is stored under.
const syncJudged = `select repo, number from pull_request_heads union select repo, number from delivery_pull_requests`

// syncAttributionsStatement is SyncAttributions' one statement. It writes the answer to each
// results row of the pull request, under each of its names, where it differs; the source too
// where the pull request has facts. It writes the answer to each timeline row of a pull request
// that has a results row. And it queues one move for each pull request whose chosen results row
// (the row that heads takes) held another issue, keyed by the canonical name: a second move before
// the events keeps the first from_key. A pull request whose answer is not known writes nothing and
// queues nothing. All terms read one snapshot, so from_key is the stored issue before the write.
func syncAttributionsStatement(schema string) string {
	return attributedTerms(schema, syncJudged) + `, heads_written as (
		update pull_request_heads h set issue_key = a.issue_key,
			attribution_source = case when a.has_facts then a.source else h.attribution_source end
		from attributed a
		where a.known and h.number = a.number and lower(` + refs.CanonicalRepoSQL("h.repo") + `) = lower(a.repo)
			and (h.issue_key is distinct from a.issue_key or (a.has_facts and h.attribution_source is distinct from a.source))
		returning 1
	), timeline_written as (
		update delivery_pull_requests d set issue_key = a.issue_key
		from attributed a
		where a.known and a.stored_repo is not null
			and d.number = a.number and lower(` + refs.CanonicalRepoSQL("d.repo") + `) = lower(a.repo)
			and d.issue_key is distinct from a.issue_key
		returning 1
	), queued as (
		insert into pull_request_attribution_moves (repo, number, from_key, to_key, source)
		select a.repo, a.number, a.fallback, a.issue_key, a.source from attributed a
		where a.known and a.stored_repo is not null and a.fallback is distinct from a.issue_key
		on conflict (repo, number) do update set to_key = excluded.to_key, source = excluded.source, queued_at = excluded.queued_at
		returning 1
	)
	select (select count(*) from queued), (select count(*) from timeline_written), (select count(*) from heads_written)`
}

// SyncAttributions writes the answer of the statement over every stored pull request to its
// stored rows, in one statement with no cap, and queues a move for each results row that takes
// another issue (syncAttributionsStatement). It reads no GitHub. The writes of a results row's
// issue and source owe no read and keep the row's mark: migration 20261117111040 narrowed the
// trigger pull_request_heads_unread_write to the facts of the head.
func SyncAttributions(ctx context.Context, pool *store.Pool) (AttributionSync, error) {
	statements, err := attribution()
	if err != nil {
		return AttributionSync{}, err
	}
	var synced AttributionSync
	var heads int
	if err := pool.QueryRow(ctx, statements.syncAttributions).Scan(&synced.Changes, &synced.TimelineRows, &heads); err != nil {
		return AttributionSync{}, fmt.Errorf("write the issue of every stored pull request: %w", err)
	}
	return synced, nil
}

// attributionMove is one queued move whose two keys differ, as AppendAttributionEvents takes it:
// fromLive and toLive say whether each key is still an issue, which an event needs.
type attributionMove struct {
	payload          IssuePullRequestAttributionEventPayload
	fromLive, toLive bool
}

// AppendAttributionEvents takes at most AttributionMovesPerPass queued moves, the oldest first,
// and deletes them, in one transaction. For each move whose two keys differ, it appends one
// issue.pull_request_attribution_changed on the issue left and one on the issue reached, each
// where it is set and still an issue, in one AppendAll, which takes the issues in one order. It
// publishes the events after the commit and answers how many it appended. A move whose keys are
// equal again, a move and the move back before the events, appends nothing. Two processes take
// disjoint moves (skip locked). A caller with no broker appends nothing, and the moves wait for a
// pass that has one.
func AppendAttributionEvents(ctx context.Context, pool *store.Pool, broker *events.Broker) (int, error) {
	if broker == nil {
		return 0, nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("open the append of attribution events: %w", err)
	}
	defer func() {
		rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), headWriteRollback)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	// The take locks and deletes rows of the moves table alone, so it names no owner table that
	// a `for update` would lock (api's TestNoForUpdateOnOwnerTables); the issues and the URLs are
	// read by the next statement.
	rows, err := tx.Query(ctx, `
		with taken as (
			select repo, number from pull_request_attribution_moves
			order by queued_at, repo, number
			limit $1
			for update skip locked
		)
		delete from pull_request_attribution_moves m using taken t
		where m.repo = t.repo and m.number = t.number
		returning m.repo, m.number, m.from_key, m.to_key, m.source, m.queued_at
	`, AttributionMovesPerPass)
	if err != nil {
		return 0, fmt.Errorf("take the queued attribution moves: %w", err)
	}
	type queuedMove struct {
		payload  IssuePullRequestAttributionEventPayload
		queuedAt time.Time
	}
	var taken []queuedMove
	for rows.Next() {
		var move queuedMove
		p := &move.payload
		if err := rows.Scan(&p.Repo, &p.Number, &p.From, &p.To, &p.Source, &move.queuedAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("take the queued attribution moves: %w", err)
		}
		if !equalIssue(p.From, p.To) {
			taken = append(taken, move)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("take the queued attribution moves: %w", err)
	}
	slices.SortFunc(taken, func(a, b queuedMove) int {
		return cmp.Or(a.queuedAt.Compare(b.queuedAt), strings.Compare(a.payload.Repo, b.payload.Repo), cmp.Compare(a.payload.Number, b.payload.Number))
	})
	repos, numbers := make([]string, len(taken)), make([]int, len(taken))
	froms, tos := make([]*string, len(taken)), make([]*string, len(taken))
	for i, move := range taken {
		repos[i], numbers[i], froms[i], tos[i] = move.payload.Repo, move.payload.Number, move.payload.From, move.payload.To
	}
	rows, err = tx.Query(ctx, `
		select coalesce(u.url, ''), exists (select 1 from issues i where i.key = k.from_key), exists (select 1 from issues i where i.key = k.to_key)
		from unnest($1::text[], $2::int[], $3::text[], $4::text[]) with ordinality k(repo, number, from_key, to_key, n)
		left join lateral (
			select h.url from pull_request_heads h
			where h.number = k.number and lower(`+refs.CanonicalRepoSQL("h.repo")+`) = lower(k.repo)
			order by lower(h.repo) = lower(k.repo) desc, h.repo
			limit 1) u on true
		order by k.n
	`, repos, numbers, froms, tos)
	if err != nil {
		return 0, fmt.Errorf("read the pull requests and issues of %d attribution moves: %w", len(taken), err)
	}
	moves := make([]attributionMove, 0, len(taken))
	for rows.Next() {
		move := attributionMove{payload: taken[len(moves)].payload}
		if err := rows.Scan(&move.payload.URL, &move.fromLive, &move.toLive); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read the pull requests and issues of %d attribution moves: %w", len(taken), err)
		}
		moves = append(moves, move)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read the pull requests and issues of %d attribution moves: %w", len(taken), err)
	}
	var moved []model.Event
	for _, move := range moves {
		for _, side := range []struct {
			key  *string
			live bool
		}{{move.payload.From, move.fromLive}, {move.payload.To, move.toLive}} {
			if side.key == nil || !side.live {
				continue
			}
			key := *side.key
			moved = append(moved, model.Event{IssueKey: &key, Type: AttributionEventType, Actor: AttributionEventActor, Payload: move.payload})
		}
	}
	var appended []model.Event
	if len(moved) > 0 {
		if appended, err = broker.AppendAll(ctx, tx, moved); err != nil {
			return 0, fmt.Errorf("append the events of %d attribution moves: %w", len(moves), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit the events of %d attribution moves: %w", len(moves), err)
	}
	for _, event := range appended {
		broker.Publish(event)
	}
	return len(appended), nil
}

// AttributionCounts is what GET /api/v1/delivery/attribution answers, counted at the request.
// ReadsLeft is the entries of the facts line, the pull requests a pass still reads; FactsLeft the
// part of them that has no facts, R4's number; FactsRefused the part whose last read was refused.
// Reading is the identity of the reading that runs now, and KeysOnAnotherReading the texts rows
// whose keys another reading made. MovesQueued is the moves that wait for their events.
// LinksInTwoCases is the folded URLs that more than one issue links. OwedFailures and
// OwedRefusals count the pull requests whose head is not read while a read is owed: GitHub did
// not answer the read, or GitHub refuses it. OwedFailuresOldestAt is the oldest time that a
// failure is owed since. OwedRefusalsSince is the oldest time that a refusal is refused since, or
// owed since where an older release owed it. Each time is nil where its count is 0.
type AttributionCounts struct {
	FactsLeft            int        `json:"facts_left"`
	ReadsLeft            int        `json:"reads_left"`
	FactsRefused         int        `json:"facts_refused"`
	Reading              string     `json:"reading"`
	KeysOnAnotherReading int        `json:"keys_on_another_reading"`
	MovesQueued          int        `json:"moves_queued"`
	LinksInTwoCases      int        `json:"links_in_two_cases"`
	OwedFailures         int        `json:"owed_failures"`
	OwedFailuresOldestAt *time.Time `json:"owed_failures_oldest_at,omitempty"`
	OwedRefusals         int        `json:"owed_refusals"`
	OwedRefusalsSince    *time.Time `json:"owed_refusals_since,omitempty"`
}

// factsLineJudged is the judged rows of the facts line: each results row that is open or merged,
// that GitHub has not proved absent, and whose texts are not current or whose messages are not
// read; and each timeline row that is not unfetchable and has no results row under any name.
var factsLineJudged = `select h.repo, h.number from pull_request_heads h
		left join pull_request_texts t on t.repo = h.repo and t.number = h.number
		where h.state in ('open', 'merged') and h.attribution_absent_at is null
			and not coalesce(t.head_sha = h.head_sha and t.base_ref = h.base_ref and t.updated_at is not distinct from h.event_at
				and t.head_ref = h.head_ref and t.title = h.title and t.commit_messages is not null, false)
	union select d.repo, d.number from delivery_pull_requests d
		where d.unfetchable_at is null and not exists (
			select 1 from pull_request_heads x
			where x.number = d.number and lower(` + refs.CanonicalRepoSQL("x.repo") + `) = lower(` + refs.CanonicalRepoSQL("d.repo") + `))`

// factsRefusedOf is the condition that an attributed pull request's last read of facts was
// refused: its results row's attribution_refused_at, or its timeline row's facts_refused_at where
// it has no results row.
const factsRefusedOf = `case when a.stored_repo is not null
		then exists (select 1 from pull_request_heads x where x.repo = a.stored_repo and x.number = a.number and x.attribution_refused_at is not null)
		else exists (select 1 from delivery_pull_requests x where x.repo = a.timeline_repo and x.number = a.number and x.facts_refused_at is not null) end`

// attributionCountsSelect is ReadAttributionCounts' select over the terms of the facts line.
const attributionCountsSelect = `
	select (select count(*) from attributed a where not a.has_facts),
		(select count(*) from attributed),
		(select count(*) from attributed a where ` + factsRefusedOf + `),
		(select r.id from now_reading r),
		(select count(*) from pull_request_texts t where t.reading <> (select r.id from now_reading r)),
		(select count(*) from pull_request_attribution_moves),
		(select count(*) from (select lower(url) from issue_external_links group by lower(url) having count(distinct issue_key) > 1) tie)`

// ReadAttributionCounts counts, at the call, what the attribution of the stored pull requests
// still waits on (AttributionCounts). It reads no setting and no GitHub. A pull request GitHub
// proved absent is in no count, and one whose texts are current and whose messages are not read
// is in ReadsLeft, and in FactsLeft only where its answer is not known.
func ReadAttributionCounts(ctx context.Context, pool *store.Pool) (AttributionCounts, error) {
	statements, err := attribution()
	if err != nil {
		return AttributionCounts{}, err
	}
	var counts AttributionCounts
	if err := pool.QueryRow(ctx, statements.attributionCounts).Scan(&counts.FactsLeft, &counts.ReadsLeft, &counts.FactsRefused,
		&counts.Reading, &counts.KeysOnAnotherReading, &counts.MovesQueued, &counts.LinksInTwoCases); err != nil {
		return AttributionCounts{}, fmt.Errorf("count the attribution's work left: %w", err)
	}
	return counts, nil
}

// factsLineSelect is the facts line's select over its terms: each entry under its canonical name,
// the name of a stored results row that is not the canonical name, the name of its timeline row,
// and what the line orders it by.
var factsLineSelect = `
	select a.repo, a.number,
		coalesce((select x.repo from pull_request_heads x
			where x.number = a.number and lower(` + refs.CanonicalRepoSQL("x.repo") + `) = lower(a.repo) and lower(x.repo) <> lower(a.repo)
			order by x.repo limit 1), a.repo),
		a.stored_repo is not null, coalesce(a.timeline_repo, ''),
		coalesce(hr.state = 'open', false), coalesce(hr.github_read_at is null, false),
		b.texts_current, b.messages_read, a.known, a.has_facts, a.fallback,
		case when a.stored_repo is not null then hr.attribution_refused_at else d.facts_refused_at end,
		coalesce(hr.event_at, d.merged_at)
	from attributed a
	join batch b on b.repo = a.repo and b.number = a.number
	left join pull_request_heads hr on hr.repo = a.stored_repo and hr.number = a.number
	left join delivery_pull_requests d on d.repo = a.timeline_repo and d.number = a.number`

// titleAndBranchTexts is a texts relation that holds, for each results row, the facts of its
// head row alone: its head, branch, base, title and time, no body, and messages read and empty.
// Its reading is never the one that runs now, so the statement reads its keys from
// attribution_keys(repo, head_ref, title, ”, '{}'), the keys of the title and the branch alone.
const titleAndBranchTexts = `(select g.repo, g.number, g.head_sha, g.head_ref, g.repo as head_repo, g.base_ref, g.title,
		''::text as body, '{}'::text[] as commit_messages, g.event_at as updated_at, ''::text as reading,
		'{}'::text[] as title_keys, '{}'::text[] as cited_issues, '{}'::text[] as branch_keys, '{}'::text[] as commit_keys
	from pull_request_heads g)`

// titleAndBranchSelect is the answer of the statement over the keys of the title and the branch
// of each judged pull request with a results row (titleAndBranchTexts).
const titleAndBranchSelect = `
	select a.repo, a.number, a.issue_key from attributed a where a.stored_repo is not null`

// KeyRemakeEvery is how often each process checks whether some texts row is on another reading
// (KeyRemakeLoop). A change of the identity by any cause reaches every row within it and one
// remake.
const KeyRemakeEvery = 30 * time.Second

// remakeChunk is how many texts rows one statement of RemakeKeys remakes.
const remakeChunk = 1000

// remakeKeysStatement is one chunk of RemakeKeys: at most remakeChunk texts rows on another
// reading, a row that a writer holds skipped, each written with its own title so the trigger
// pull_request_texts_keys writes its keys and the reading that runs now.
func remakeKeysStatement(schema string) string {
	return `with id as (select ` + schema + `.attribution_reading() as id), chunk as (
		select t.repo, t.number from pull_request_texts t, id
		where t.reading < id.id or t.reading > id.id
		limit ` + strconv.Itoa(remakeChunk) + `
		for update of t skip locked
	) update pull_request_texts x set title = x.title from chunk c, id
	where (x.repo, x.number) = (c.repo, c.number) and (x.reading < id.id or x.reading > id.id)
	returning 1`
}

// readingOtherStatement answers whether any texts row is on another reading: two probes of the
// index pull_request_texts_reading, whatever the statistics say (now_reading of attributedTerms).
func readingOtherStatement(schema string) string {
	return `select coalesce((select pg_catalog.min(x.reading) from pull_request_texts x), r.id) <> r.id
		or coalesce((select pg_catalog.max(x.reading) from pull_request_texts x), r.id) <> r.id
	from (select ` + schema + `.attribution_reading() as id) r`
}

// RemakeKeys remakes the keys of every texts row on another reading, one chunk a statement,
// until a chunk finds no row, and answers how many rows it remade. Two processes split the rows,
// since each chunk skips a row another holds, and a writer's own trigger stamps the reading that
// runs. After it remade a row, it analyzes pull_request_texts, so the plans of the reads see the
// new readings.
func RemakeKeys(ctx context.Context, pool *store.Pool) (int, error) {
	statements, err := attribution()
	if err != nil {
		return 0, err
	}
	remade := 0
	for {
		tag, err := pool.Exec(ctx, statements.remakeKeys)
		if err != nil {
			return remade, fmt.Errorf("remake the keys of the texts rows on another reading: %w", err)
		}
		if tag.RowsAffected() == 0 {
			break
		}
		remade += int(tag.RowsAffected())
	}
	if remade > 0 {
		if _, err := pool.Exec(ctx, `analyze pull_request_texts`); err != nil {
			return remade, fmt.Errorf("analyze pull_request_texts after a remake of its keys: %w", err)
		}
	}
	return remade, nil
}

// KeyRemakeLoop checks at once whether some texts row is on another reading, and then every
// every, until ctx ends. Where one is, it remakes the keys (RemakeKeys) and logs the rows and the
// time it took. A check or a remake that fails logs a warning, and the next check tries again.
// It needs no App key, no lease and no period of the reconcile: each process runs it from its
// start, after PrepareAttribution, so a change of the identity reaches every row within every
// and one remake, with no restart. A non-positive every is KeyRemakeEvery.
func KeyRemakeLoop(ctx context.Context, pool *store.Pool, every time.Duration) {
	if every <= 0 {
		every = KeyRemakeEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		remakeKeysOnce(ctx, pool)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// remakeKeysOnce is one check of KeyRemakeLoop.
func remakeKeysOnce(ctx context.Context, pool *store.Pool) {
	statements, err := attribution()
	if err != nil {
		slog.Warn("dispatch delivery: check the reading of the stored keys", "error", err)
		return
	}
	var other bool
	if err := pool.QueryRow(ctx, statements.readingOther).Scan(&other); err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch delivery: check the reading of the stored keys", "error", err)
		}
		return
	}
	if !other {
		return
	}
	started := time.Now()
	remade, err := RemakeKeys(ctx, pool)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch delivery: remake the keys of another reading", "rows", remade, "error", err)
		}
		return
	}
	slog.Info("dispatch delivery: keys remade", "rows", remade, "took", time.Since(started))
}
