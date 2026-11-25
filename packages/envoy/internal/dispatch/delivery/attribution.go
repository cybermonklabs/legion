package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A pull request's issue is the issue whose external link names the pull request, in any
// repository, and else the first issue its other four ranked sources name that resolves to the
// repository the pull request is in: a candidate of those four ranked above it that resolves to
// another repository, or to no repository at all, is passed over, and a pull request no candidate
// of which is held attributes nobody, with the source "none". Every candidate of one source ranks
// above any of the next (attributedTerms):
//
//  1. the issue whose external link names the pull request by the link write's rule
//     (linkedPullRequest): its /pull/ or /issues/ URL under any name of its repository, the one
//     it has today or one it was renamed away from, whatever repository the issue resolves to;
//  2. the issue whose external link is a GitHub issue the body cites, a full
//     github.com/<owner>/<repo>/issues/<n> URL or a bare #N in the pull request's own repository,
//     the first such citation in body order, where <n> is no pull request Dispatch holds
//     (citedPullRequest);
//  3. a key in its head branch name, whatever its case;
//  4. a key in its title or body, title first;
//  5. a key in its commit messages, in commit order.
//
// The prototype's collector/attribution.py tried the title and the body first, and ranks 1 to 3
// below them. R8 of INFRA-1522 moved them above: a branch name and a link are acts that name the
// issue — someone branched for it, someone linked it — while a key in a title or a body is a
// mention, which a pull request that reverts, follows or merely cites another issue's work
// carries as readily as one that does that work. The commit messages stay last: they are the
// weakest mention of all, and the only source that can cost a GitHub call of its own. A REST
// read pages them only where no higher source decides (storePullRequestFacts); the batched
// query reads the first 100 with the other facts (readFactsBatch).
//
// The holding to the repository is the server's one repository-resolution rule, spliced into the
// same statement as the candidates (refs.IssueRepoTerms, attributionHeld), so what an
// attribution believes an issue's repository is can never disagree with the issue page. The link
// to the pull request's own URL is exempt from it (attributionHeldFor): a link is a deliberate
// act, and Dispatch links one pull request to one issue only, so an issue whose work spans two
// repositories links the pull request it has in the second one, and that pull request is the
// issue's (R19 of the spec of several open pull requests). A name in a title, a body, a branch or
// a commit is no such act, and it is held to the pull request's repository. A fork's head is held
// nowhere (own_head).
//
// One statement reads the five sources at read time, from the texts of one read of GitHub and
// from the database (attributedTerms, below). Sources 1 and 2 read Dispatch's external links,
// every source reads whether the issue it names exists, and the holding reads where that issue
// sits. So a new link, a reparent or a project's new default repository reaches every stored pull
// request at its next read, with no read of GitHub and no walk of the table. The renaming rule is
// the pairs the process read at boot (refs.Renames), which the Attributor writes into each
// statement it builds (renameTerms), so no statement reads repo_projects.
//
// The rule answers only for a pull request whose texts describe its stored head (texts_current).
// A head branch in another repository, a fork's, names nobody (own_head). Where the commit
// messages are not read and no higher source decides, the answer is not known. Where no texts
// describe the stored head, the answer is the issue that the stored rows hold (attributedTerms).

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
// project's default repository (refs.IssueRepoTerms) — and a title and a body together can name
// many keys: a Legion pull request names one or two, while a body listing a tree's issues can
// name a dozen, so 25 is room for the shapes that occur and a bound on the work a pathological
// body can ask for. The bound drops only candidates ranked below it, so an issue is passed over
// for the bound's sake alone where the five sources name more than 25 keys and none of the
// first 25 is held to the pull request's repository.
const attributionCandidateBound = 25

// attributionHeld is the condition a candidate of every source but the issue's own link to the
// pull request's URL passes to be the pull request's issue (attributionHeldFor): it
// resolves, through the server's one repository-resolution rule (refs.IssueRepoTerms, joined by
// refs.IssueRepoJoin in the enclosing statement), to the repository the expression repo reads.
// It is never null — a candidate whose issue row is missing, and one whose parent chain ran out
// of depth, is simply not held — so a caller may read it as a plain boolean and a `where` over
// it drops exactly the candidates that are not held. The comparison is lowercased on both sides
// because GitHub spells an owner and a repository as their owners do (`Acme/Widgets`), and a
// stored repository may be spelled either way.
//
// Both sides go through the server's one repository-renaming rule first
// (refs.CanonicalRepoSQL over renames, the pairs the boot read), so one repository under two
// names is one repository here: an issue recorded under a name that was renamed away is held to a
// pull request on the new name, and an issue recorded under the new name to a pull request
// Dispatch stored under the old one. A repository with no pair is its own canonical name, so it
// is compared by that name alone.
func attributionHeld(renames refs.Renames, repo string) string {
	return attributionHeldAs(refs.CanonicalRepoSQL(renames, refs.IssueRepoResolved), refs.CanonicalRepoSQL(renames, repo))
}

// attributionHeldAs is attributionHeld over the two canonical names, which the caller reads: the
// canonical name of the issue's resolved repository and the pull request's. The statement reads
// both through its terms of the renaming rule (renameTerms), as joins of one relation.
func attributionHeldAs(issueCanonical, repoCanonical string) string {
	return `coalesce(not ` + refs.IssueRepoTooDeep + ` and lower(` + issueCanonical + `) = lower(` + repoCanonical + `), false)`
}

// attributionHeldFor is the condition one candidate, named by the source the expression source
// reads, passes to be the pull request's issue. The issue whose external link is the pull
// request's own URL is held in any repository: a person links an issue to one pull request, and
// the link names that pull request wherever it is (R19 of the spec of several open pull
// requests). A candidate of any other source is held where held, the condition attributionHeld
// or attributionHeldAs gives, holds it to the pull request's repository. Neither holds a pull
// request whose head is in a fork, which the statement's own_head refuses before it names any
// candidate.
func attributionHeldFor(source, held string) string {
	return `(` + source + ` = '` + string(sourceExternalLink) + `' or ` + held + `)`
}

// renameTerms are the statement's terms of the renaming rule over renames, the pairs the boot read
// (refs.Renames):
//
//   - renames: refs.RenamesSQL, the rule's one relation, materialized: one row for each pair, the
//     old name lowered as rename_source and the new name as rename_target, as
//     refs.CanonicalRepoSQL reads it. A name with no row here is its own canonical name.
//   - renamed_names: for each lowered target, the lowered names renamed to it, the relation's
//     inverse, which the statement alone reads.
//
// The pairs are literals of the statement's text, written once when the Attributor builds its
// statements, so the statement reads no table for the rule: repo_projects, which held the pairs,
// is read once at boot (store.ReadRepoRenames) and a later change drops it. Every read of the rule
// in the statement is a join of one of the two terms (renameJoin, namesJoin), which a hash join
// serves, so the statement reads the rule once however many names it judges; the one subquery the
// statement runs for each row, citedPullRequest, compares by refs.CanonicalRepoSQL instead, which
// reads no relation.
func renameTerms(renames refs.Renames) string {
	return `renames as materialized (` + refs.RenamesSQL(renames) + `
	), renamed_names as materialized (
		select lower(n.rename_target) as target, pg_catalog.array_agg(n.rename_source) as names
		from renames n group by lower(n.rename_target)
	)`
}

// renameJoin is the join of renames, under alias, that gives the canonical name of the expression
// name (canonicalOver): a left join, so a name no mapping renames keeps its row.
func renameJoin(alias, name string) string {
	return `left join renames ` + alias + ` on ` + alias + `.rename_source = lower(` + name + `)`
}

// canonicalOver is the canonical name of the expression name over renameJoin(alias, name):
// refs.CanonicalOverSQL of the target of its row in renames, which is refs.CanonicalRepoSQL(name).
func canonicalOver(alias, name string) string {
	return refs.CanonicalOverSQL(alias+`.rename_target`, name)
}

// namesJoin is the two joins, under alias, that give the names of the expression lowered, a
// lowered canonical name (namesOver).
func namesJoin(alias, lowered string) string {
	return `left join renames ` + alias + `_own on ` + alias + `_own.rename_source = ` + lowered + `
		left join renamed_names ` + alias + `_to on ` + alias + `_to.target = ` + lowered
}

// namesOver is, over namesJoin(alias, lowered), every lowered name whose canonical name is
// lowered in any case: the names renamed to it, and lowered itself where no mapping renames it.
// So lower(x) = any(namesOver(...)) holds exactly where lower(refs.CanonicalRepoSQL(x)) = lowered:
// a name x with a row in renames reads as its target, which is lowered only where x is renamed to
// it, and a name with no row reads as itself.
func namesOver(alias, lowered string) string {
	return `(coalesce(` + alias + `_to.names, '{}'::text[]) || case when ` + alias + `_own.rename_source is null then array[` + lowered + `] else '{}'::text[] end)`
}

// attributionSubjects is the `subjects` term of the statement (attributedTerms): the distinct
// keys its candidates name, which is the set refs.IssueRepoTerms walks. `where key is not null`
// keeps a candidate with no key out of the walk.
const attributionSubjects = `subjects as (
	select distinct key from candidates where key is not null
)`

// citedPullRequest is the condition that url, an expression of a URL of the cited list
// (https://github.com/<owner>/<repo>/issues/<n>), names a pull request Dispatch holds: a results
// row (pull_request_heads) with that number, stored under any name whose canonical name is the
// cited repository's, read through the statement's renames. GitHub numbers a repository's issues
// and pull requests in one sequence and answers /issues/<n> for a pull request too, so `#<n>` in a
// body, and the .../issues/<n> link Dispatch stores for the reference owner/repo#<n>, name pull
// request #<n> where it is one. A body that names another pull request mentions that pull
// request's work and cites no issue whose work it does: without this condition, a pull request
// whose body said "#<n> renamed the helper" went to the issue that links #<n>, above the issue its
// own branch names. A number Dispatch holds no results row of reads as an issue, so the answer
// changes at the next read once Dispatch stores that pull request. The URL is read by the link
// rule (LinkedPullRequestSQL), which bounds its number, so no citation casts past int's bound.
//
// The condition is a subquery the statement runs once for each cited URL, so it compares the two
// names by refs.CanonicalRepoSQL, the renaming rule as a case over the pairs written into the
// text, which reads no relation. A join of renames here is a scan of that relation for each cited
// URL: with the pairs as literals the planner knows how few rows the relation holds and rescans it
// in a nested loop, and the nodes that scan the rule ran 47 loops for 20 pull requests and 9 for
// one (TestTheStatementReadsTheRenamingRuleOnceWhateverItsPullRequests).
func citedPullRequest(renames refs.Renames, url string) string {
	return `exists (select 1 from ` + LinkedPullRequestSQL(url) + ` cited
		join pull_request_heads cited_pr on cited_pr.number = cited.number
		where lower(` + refs.CanonicalRepoSQL(renames, "cited_pr.repo") + `) = lower(` + refs.CanonicalRepoSQL(renames, "cited.repo") + `))`
}

// One statement gives the issue of a pull request at read time, from its stored facts alone: the
// results row (pull_request_heads), the timeline row (delivery_pull_requests) and the texts of one
// read of GitHub (pull_request_texts), with the keys the database reads from those texts
// (attribution_keys, migration 20261117122117). Every reader by key and by issue runs it, so a
// change of the rule, a new link, a moved issue or a new issue reaches every stored pull request
// at its next read, with no read of GitHub.
//
// The text names the schema of the reading's two functions, which only the database knows, so it
// is built once at start (PrepareAttribution) and never per read.

// foldAsCited is the expression text folded the way the reading folds a cited URL: ASCII lower
// case, under the collation "C", whatever the database's own collation folds besides. The cited
// list is in that fold, so an arm that compares a link with it compares in that fold too, by key
// and by issue alike, and the answer is the same in every database the guard of migration
// 20261117111040 admits. A pair equal in that fold is equal under lower() too, so the arm by key
// keeps lower() as its first term, which issue_external_links_url_lower serves. The folded text
// takes the database's collation again, the collation of the cited list and of its index
// (pull_request_texts_keys): an overlap of the list with an array under "C" reads no index.
func foldAsCited(text string) string {
	return `(lower(` + text + ` collate "C") collate "default")`
}

// linkURLAsCited is a link's URL in the fold of a cited URL (foldAsCited).
var linkURLAsCited = foldAsCited("l.url")

// citedSpellings is every spelling of url, an expression naming a URL, under each name of each
// rename pair that names it (refs.URLSpellingsFoldedSQL), each in the fold of a cited URL
// (foldAsCited), as a SQL text[]. The arm by key compares a link in that fold with the spellings
// of a cited URL, and the arm by issue overlaps the cited lists with the spellings of each link
// of the issue, so one relation of a link and a cited URL decides both: a link stored under a
// repository's new name matches a citation under its old name, and the other way round, by key
// and by issue alike. A cited URL is in that fold already, so its own spelling is itself.
func citedSpellings(renames refs.Renames, url string) string {
	return refs.URLSpellingsFoldedSQL(renames, url, foldAsCited)
}

// attributedTerms is the statement's terms over judged, a query of (repo, number) under any
// stored name, for the schema that holds the reading. A caller adds its own select over
// `attributed`, one row for each judged pull request: repo (the canonical name), number, names
// (every lowered name whose canonical name is repo, which a caller compares a stored row's name
// with, lower(x.repo) = any(a.names), instead of reading the renaming rule again), stored_repo and
// timeline_repo (the names its results row and its timeline row are stored under, null where it
// has none), has_facts, known, fallback, issue_key, source and linked (the issue is a link's, and
// does not resolve to the pull request's repository: a reader of the issue's own repository leaves
// the pull request out, and a reader of its heads names its repository).
//
//   - now_reading: the identity of the reading that runs now, and whether any texts row is on
//     another. Each of min and max is one probe of pull_request_texts_reading, whatever the
//     statistics say.
//   - renames and renamed_names: the renaming rule over renames, the pairs the boot read, as one
//     relation (renameTerms). Every comparison of a name below reads it through them, so the
//     statement reads the rule once however many pull requests it judges, and reads no table for
//     it.
//   - judged: each judged pull request once, under its canonical name (refs.CanonicalRepoSQL),
//     with names, every lowered name whose canonical name is that one (namesOver), held_as, the
//     lowered canonical name of that canonical name, which the hold and the links compare, and
//     held_names, the names of held_as. Where no pair renames a target, which the route that
//     wrote the pairs refused, held_as is the canonical name and held_names is names.
//   - heads and timeline: its results row and its timeline row under any name the renaming rule
//     reads as that name (names), the row under the canonical name first, then the results row
//     read from GitHub last. limit 1 keeps each join one probe of the index on number.
//   - batch: its texts, joined to that results row. The fallback is the results row's stored
//     answer, read or unread, also when that is nobody, and the timeline row's issue only where
//     no results row exists: the one fallback of every reader. The texts are facts only where they
//     describe the results row's head (texts_current). A row's stored keys stand only where its
//     reading is the one that runs now; else the keys are read from its texts here. offset 0 keeps
//     the planner from making that condition a join filter, which would run the reading for every
//     row.
//   - named to picked: the five sources in the order of the list at the top of this file, from
//     facts whose head is the pull request's own repository's (own_head), bounded at
//     attributionCandidateBound and held to the pull request's repository (attributionHeldAs), but
//     for a link that names the pull request, which is held in any repository. A link names it
//     by the one rule of the link write (linkedPullRequest): its /pull/ or its /issues/ URL, in
//     any letter case, under any name whose canonical name is the pull request's repository, so a
//     link written before a rename names it too (linkFormsOf over held_names, as
//     PullRequestLinksSQL gives them for one pull request, and LinkedPullRequestSQL). A citation
//     of a number that is a pull request Dispatch holds names no issue (citedPullRequest). A tie
//     of rank and place, two links of one pull request in two spellings, is ended by the key in
//     byte order, so no collation, plan or row order picks the issue.
//   - attributed: one of three states. The rule's answer where the pull request has facts: its
//     texts are current, and its messages are read or a held candidate of a higher source
//     decides. Not known where its texts are current, its messages are not read and no held
//     candidate decides: no issue, the source none, has_facts false and known false. Messages
//     that were not read decide nothing, and the issue that a write stored is no answer either.
//     R4's fallback where the pull request has no current texts.
func attributedTerms(schema string, renames refs.Renames, judged string) string {
	return attributedTermsOver(schema, renames, judged, "pull_request_texts")
}

// attributedTermsOver is attributedTerms with the texts read from texts, a relation with the
// columns of pull_request_texts: the table itself for every reader, and the facts of a results
// row's own head alone for the order of the facts line (titleAndBranchTexts).
func attributedTermsOver(schema string, renames refs.Renames, judged, texts string) string {
	hasFacts := `b.texts_current and (b.messages_read or p.key is not null)`
	heldAs := `lower(` + canonicalOver("pr_own", "k.repo") + `)`
	held := attributionHeldAs(canonicalOver("issue_rename", refs.IssueRepoResolved), "c.pr_held_as")
	urlSpellings := refs.URLSpellingsSQL(renames, "u.url")
	return `with recursive now_reading as materialized (
		select r.id, coalesce((select pg_catalog.min(x.reading) from pull_request_texts x), r.id) <> r.id
			or coalesce((select pg_catalog.max(x.reading) from pull_request_texts x), r.id) <> r.id as other
		from (select ` + schema + `.attribution_reading() as id) r
	), ` + renameTerms(renames) + `, judged_raw as (` + judged + `
	), judged_keys as (
		select distinct on (lower(c.repo), c.number) c.repo, c.number
		from (select ` + canonicalOver("raw_rename", "r.repo") + ` as repo, r.number
			from judged_raw r ` + renameJoin("raw_rename", "r.repo") + `) c
		order by lower(c.repo), c.number, c.repo
	), judged as (
		select k.repo, k.number, ` + namesOver("pr", "lower(k.repo)") + ` as names,
			` + heldAs + ` as held_as, ` + namesOver("hold", heldAs) + ` as held_names
		from judged_keys k ` + namesJoin("pr", "lower(k.repo)") + `
			` + namesJoin("hold", heldAs) + `
	), heads as (
		select j.repo as j_repo, j.number as j_number, h.*
		from judged j cross join lateral (
			select h.repo, h.number, h.url, h.issue_key, h.attribution_source, h.head_sha, h.base_ref, h.event_at,
				h.head_ref, h.title, h.state, h.checked_at
			from pull_request_heads h
			where h.number = j.number and lower(h.repo) = any(j.names)
			order by lower(h.repo) = lower(j.repo) desc, h.github_read_at desc nulls last, h.repo
			limit 1) h
	), timeline as (
		select j.repo as j_repo, j.number as j_number, d.repo, d.issue_key
		from judged j cross join lateral (
			select d.repo, d.issue_key from delivery_pull_requests d
			where d.number = j.number and lower(d.repo) = any(j.names)
			order by lower(d.repo) = lower(j.repo) desc, d.repo
			limit 1) d
	), batch as (
		select j.repo, j.number, j.names, j.held_as, j.held_names, h.url,
			case when h.repo is null then tl.issue_key else h.issue_key end as fallback,
			h.repo as stored_repo, tl.repo as timeline_repo, h.attribution_source as stored_source,
			coalesce(t.head_sha = h.head_sha and t.base_ref = h.base_ref and t.updated_at is not distinct from h.event_at
				and t.head_ref = h.head_ref and t.title = h.title, false) as texts_current,
			coalesce(t.commit_messages is not null, false) as messages_read,
			coalesce(t.head_repo <> '' and lower(t.head_repo) = any(j.names), false) as own_head,
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
		select src.repo as pr_repo, src.number as pr_number, src.held_as as pr_held_as, l.issue_key as key,
			'` + string(sourceExternalLink) + `' as source, 0 as rank, 0::bigint as n
		from batch src cross join lateral (select l.issue_key from issue_external_links l
			where lower(l.url) = any(` + linkFormsOf("src.held_names", "src.number") + `)
				and exists (select 1 from ` + LinkedPullRequestSQL("l.url") + ` lp) offset 0) l
		where src.texts_current and src.own_head
		union all
		select src.repo, src.number, src.held_as, l.issue_key, '` + string(sourceGitHubIssueLink) + `', 1, u.n
		from batch src cross join lateral unnest(src.cited_issues) with ordinality u(url, n)
			cross join lateral (select l.issue_key from issue_external_links l
				where lower(l.url) = any(` + urlSpellings + `) and ` + linkURLAsCited + ` = any(` + citedSpellings(renames, "u.url") + `) offset 0) l
		where src.texts_current and src.own_head and not ` + citedPullRequest(renames, "u.url") + `
		union all
		select src.repo, src.number, src.held_as, u.key, '` + string(sourceBranch) + `', 2, u.n
		from batch src cross join lateral unnest(src.branch_keys) with ordinality u(key, n) join issues i on i.key = u.key
		where src.texts_current and src.own_head
		union all
		select src.repo, src.number, src.held_as, u.key, '` + string(sourceTitleBody) + `', 3, u.n
		from batch src cross join lateral unnest(src.title_keys) with ordinality u(key, n) join issues i on i.key = u.key
		where src.texts_current and src.own_head
		union all
		select src.repo, src.number, src.held_as, u.key, '` + string(sourceCommitMessage) + `', 4, u.n
		from batch src cross join lateral unnest(src.commit_keys) with ordinality u(key, n) join issues i on i.key = u.key
		where src.texts_current and src.own_head
	), candidates as (
		select pr_repo, pr_number, pr_held_as, key, source, rank, n
		from (select named.*, row_number() over (partition by pr_repo, pr_number order by rank, n, key collate "C") place from named) r
		where place <= ` + strconv.Itoa(attributionCandidateBound) + `
	), ` + attributionSubjects + `, ` + refs.IssueRepoTerms + `, held as (
		select c.pr_repo, c.pr_number, c.key, c.source, c.rank, c.n, ` + held + ` as in_repo
		from candidates c left join issues i on i.key = c.key ` + refs.IssueRepoJoin + `
			` + renameJoin("issue_rename", refs.IssueRepoResolved) + `
		where ` + attributionHeldFor("c.source", held) + `
	), picked as (
		select distinct on (pr_repo, pr_number) pr_repo, pr_number, key, source, in_repo
		from held order by pr_repo, pr_number, rank, n, key collate "C"
	), attributed as (
		select b.repo, b.number, b.names, b.stored_repo, b.timeline_repo, b.fallback, ` + hasFacts + ` as has_facts,
			(` + hasFacts + `) or not b.texts_current as known,
			case when ` + hasFacts + ` then p.key when b.texts_current then null else b.fallback end as issue_key,
			case when ` + hasFacts + ` then coalesce(p.source, '` + string(sourceNone) + `')
				when b.texts_current then '` + string(sourceNone) + `'
				else coalesce(b.stored_source, '` + string(sourceNone) + `') end as source,
			coalesce(` + hasFacts + ` and not p.in_repo, false) as linked
		from batch b left join picked p on p.pr_repo = b.repo and p.pr_number = b.number
	)`
}

// headsByIssue is the judged rows of a read by issue $1: every pull request whose answer can be
// $1, under the name it is stored under. Each arm but the last reads one index of migration
// 20261117111040 or of the tables it reads: the results rows and the timeline rows that store
// $1; the texts rows whose stored keys name it; the pull request each link of $1 names, under
// the name the link gives (LinkedPullRequestSQL), which judged reads under its canonical name;
// the texts rows that cite a URL a link of $1 names, under either name of each rename pair of
// renames, folded as the reading folds it (citedSpellings); and, only while some texts row is on
// another reading, every such row, whose stored keys say nothing. The last is a one-time filter,
// so it reads no row outside that window. A row an arm takes that the statement answers for
// another issue is dropped by the reader's own `issue_key = $1`.
func headsByIssue(renames refs.Renames) string {
	return byIssueJudged(renames,
		func(column string) string { return column + ` = $1` },
		func(keys string) string { return keys + ` @> array[$1::text]` })
}

// timelineByIssues is the judged rows of a read by the issues of the array $1: the arms of
// headsByIssue for each of them (ListPullRequestsOfIssues).
func timelineByIssues(renames refs.Renames) string {
	return byIssueJudged(renames,
		func(column string) string { return column + ` = any($1::text[])` },
		func(keys string) string { return keys + ` && $1::text[]` })
}

// byIssueJudged is the arms of a read by issue, in the order headsByIssue gives them: is(column)
// matches a column that holds one issue, and names(keys) a key array of pull_request_texts that
// names the issue. One function writes both reads, so their arms cannot drift apart.
func byIssueJudged(renames refs.Renames, is, names func(string) string) string {
	return `select repo, number from pull_request_heads where ` + is("issue_key") + `
	union select repo, number from delivery_pull_requests where ` + is("issue_key") + `
	union select repo, number from pull_request_texts
		where ` + names("branch_keys") + ` or ` + names("title_keys") + ` or ` + names("commit_keys") + `
	union select lp.repo, lp.number from issue_external_links l cross join lateral ` + LinkedPullRequestSQL("l.url") + ` lp
		where ` + is("l.issue_key") + `
	union select repo, number from pull_request_texts
		where cited_issues && array(select s from issue_external_links l cross join lateral unnest(` + citedSpellings(renames, "l.url") + `) s
			where ` + is("l.issue_key") + `)
	union select t.repo, t.number from pull_request_texts t
		where (select r.other from now_reading r) and t.reading <> (select r.id from now_reading r)`
}

// pullRequestsOfIssuesSelect is ListPullRequestsOfIssues' select over the terms of
// timelineByIssues: every timeline row, under each name it is stored under, of each judged pull
// request whose answer is one of $1, with that answer after PullRequestColumns.
var pullRequestsOfIssuesSelect = `
	select ` + PullRequestColumns + `, answer from (
		select d.*, a.issue_key as answer
		from attributed a join delivery_pull_requests d
			on d.number = a.number and lower(d.repo) = any(a.names)
		where a.issue_key = any($1::text[])) x
	order by answer, number, repo`

// pullRequestKeys is the judged rows of PullRequestIssues: the keys its caller gave, $1 the
// repositories and $2 the numbers.
const pullRequestKeys = `select k.repo, k.number from unnest($1::text[], $2::int[]) k(repo, number)`

// pullRequestIssuesStatement is PullRequestIssues' statement: the answer of each key its caller
// gave, matched under the renaming rule, for each key with a results row or a timeline row. Each
// key's canonical name is a join of renames, so the match is a hash join of the keys and the
// answers.
func pullRequestIssuesStatement(schema string, renames refs.Renames) string {
	return attributedTerms(schema, renames, pullRequestKeys) + `
	select k.repo, k.number, a.issue_key, a.source, a.has_facts, a.known
	from (` + pullRequestKeys + `) k ` + renameJoin("key_rename", "k.repo") + `
	join attributed a on a.number = k.number and lower(` + canonicalOver("key_rename", "k.repo") + `) = lower(a.repo)
	where a.stored_repo is not null or a.timeline_repo is not null`
}

// attributionStatements are the texts of every reader of a pull request's issue, built for the
// schema that holds the reading and the rename pairs of one Attributor.
type attributionStatements struct {
	pullRequestIssues     string
	headsAttributedTo     string
	headAttributedTo      string
	newestPullRequestHead string
	baseBranchMergedFor   string
	baseBranchMergedIn    string
	pullRequestsNotKnown  string
	pullRequestsOfIssues  string
	syncAttributions      string
	attributionCounts     string
	factsLine             string
	titleAndBranchAnswers string
}

func attributionStatementsFor(schema string, renames refs.Renames) *attributionStatements {
	byIssue := attributedTerms(schema, renames, headsByIssue(renames))
	return &attributionStatements{
		pullRequestIssues:     pullRequestIssuesStatement(schema, renames),
		headsAttributedTo:     byIssue + headsAttributedToSelect,
		headAttributedTo:      byIssue + headAttributedToSelect,
		newestPullRequestHead: byIssue + newestPullRequestHeadSelect,
		baseBranchMergedFor:   byIssue + baseBranchMergedForSelect,
		baseBranchMergedIn:    byIssue + baseBranchMergedInSelect,
		pullRequestsNotKnown:  attributedTerms(schema, renames, notKnownJudged(renames)) + notKnownSelect,
		pullRequestsOfIssues:  attributedTerms(schema, renames, timelineByIssues(renames)) + pullRequestsOfIssuesSelect,
		syncAttributions:      syncAttributionsStatement(schema, renames),
		attributionCounts:     attributedTerms(schema, renames, factsLineJudged) + attributionCountsSelect,
		factsLine:             attributedTerms(schema, renames, factsLineJudged) + factsLineSelect,
		titleAndBranchAnswers: attributedTermsOver(schema, renames, pullRequestKeys, titleAndBranchTexts) + titleAndBranchSelect,
	}
}

// notKnownJudged is the judged rows of PullRequestsNotKnown: the open and merged results rows of
// $1, the canonical name, under each of its names (refs.NamesSQL over renames), whose texts hold
// no commit messages. The index pull_request_texts_messages_not_read holds exactly the texts rows
// whose messages are not read, so the read costs those rows of $1 and no more.
func notKnownJudged(renames refs.Renames) string {
	return `select t.repo, t.number from pull_request_texts t
		join pull_request_heads h on h.repo = t.repo and h.number = t.number
		where lower(t.repo) = any(` + refs.NamesSQL(renames, "$1::text") + `)
		and t.commit_messages is null and h.state in ('open', 'merged')`
}

// notKnownSelect is PullRequestsNotKnown's select over the terms of notKnownJudged: each judged
// pull request whose answer is not known, by number.
const notKnownSelect = `
	select a.number, h.head_sha, h.state, h.base_ref, h.event_at
	from attributed a join heads h on h.j_repo = a.repo and h.j_number = a.number
	where not a.known and h.state in ('open', 'merged')
	order by a.number`

// NotKnownPullRequest is one open or merged pull request whose answer is not known: its texts are
// current, its commit messages are not read, and no held candidate decides its issue
// (attributedTerms). It belongs to no issue, so no reader by issue lists it. It can be the issue's
// own pull request of each issue of its repository.
type NotKnownPullRequest struct {
	Number         int
	HeadSHA, State string
	BaseRef        string
	EventAt        *time.Time
}

// PullRequestsNotKnown is every open or merged pull request of repoFull, a canonical name, whose
// answer is not known, by number: the path of such a pull request to the results route, by its
// repository and never by a stored issue (api/results_comparison.go, api/results_ci.go). It reads
// the rows stored under repoFull and under each name renamed away from it. answers is the caller's
// request's, as HeadsAttributedTo takes them.
func (a *Attributor) PullRequestsNotKnown(ctx context.Context, q Querier, repoFull string, answers HeadAnswers) ([]NotKnownPullRequest, error) {
	statements, err := a.attribution()
	if err != nil {
		return nil, err
	}
	var pulls []NotKnownPullRequest
	err = answers.Read(ctx, q, func(q Querier) error {
		pulls, err = readNotKnown(ctx, q, statements.pullRequestsNotKnown, repoFull)
		return err
	})
	return pulls, err
}

// readNotKnown is PullRequestsNotKnown's statement on q.
func readNotKnown(ctx context.Context, q Querier, statement, repoFull string) ([]NotKnownPullRequest, error) {
	rows, err := q.Query(ctx, statement, repoFull)
	if err != nil {
		return nil, fmt.Errorf("read the pull requests of %s whose issue is not known: %w", repoFull, err)
	}
	defer rows.Close()
	var pulls []NotKnownPullRequest
	for rows.Next() {
		var pull NotKnownPullRequest
		if err := rows.Scan(&pull.Number, &pull.HeadSHA, &pull.State, &pull.BaseRef, &pull.EventAt); err != nil {
			return nil, fmt.Errorf("read the pull requests of %s whose issue is not known: %w", repoFull, err)
		}
		pulls = append(pulls, pull)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the pull requests of %s whose issue is not known: %w", repoFull, err)
	}
	return pulls, nil
}

// Attributor reads and writes a pull request's issue by the rename pairs this process read at
// boot (refs.Renames): it builds every reader's statement of the issue (attributedTerms) with
// those pairs written in as literals, keys every pull_request_heads row it writes by the canonical
// name of the row's repository (canonical), and owes each read under that name. cmd/dispatch's
// main builds one Attributor and gives it to the delivery reconcile (NewReconcile), the intakes
// (StartIntakes) and the Legion metrics; the API builds its own of the same pairs (api.NewDeps).
// No package variable holds the pairs, so a test builds an Attributor from pairs of its own.
//
// The statements also name the schema of the reading's two functions, which only the database
// knows and PrepareAttribution finds once at start, so an Attributor builds them at its first read
// after that and keeps them.
type Attributor struct {
	renames refs.Renames
	built   atomic.Pointer[builtAttribution]
}

// builtAttribution is an Attributor's statements and the schema they were built for.
type builtAttribution struct {
	schema     string
	statements *attributionStatements
}

// NewAttributor is the Attributor of renames.
func NewAttributor(renames refs.Renames) *Attributor {
	return &Attributor{renames: renames}
}

// canonical is repo under the name it is called today (refs.Renames.Canonical).
func (a *Attributor) canonical(repo string) string {
	return a.renames.Canonical(repo)
}

// attribution is a's statements for the schema PrepareAttribution found.
func (a *Attributor) attribution() (*attributionStatements, error) {
	prepared, err := preparedReading()
	if err != nil {
		return nil, err
	}
	if built := a.built.Load(); built != nil && built.schema == prepared.schema {
		return built.statements, nil
	}
	built := &builtAttribution{schema: prepared.schema, statements: attributionStatementsFor(prepared.schema, a.renames)}
	a.built.Store(built)
	return built.statements, nil
}

// readingStatements are the schema that holds the reading and the statements of the remake of
// stored keys (RemakeKeys), which read no repository name, as PrepareAttribution built them.
type readingStatements struct {
	schema       string
	remakeKeys   string
	readingOther string
}

var prepared atomic.Pointer[readingStatements]

var errAttributionNotPrepared = errors.New("the statement of a pull request's issue is not built: PrepareAttribution runs after the migrations, before any reader")

func preparedReading() (*readingStatements, error) {
	if statements := prepared.Load(); statements != nil {
		return statements, nil
	}
	return nil, errAttributionNotPrepared
}

// PrepareAttribution finds, once, after the migrations and before any reader, the schema that
// holds pull_request_texts, which every reader's statement names before each function of the
// reading (Attributor), and builds the statements of the remake of stored keys. It then asks the
// guard of migration
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
	prepared.Store(&readingStatements{schema: schema, remakeKeys: remakeKeysStatement(schema), readingOther: readingOtherStatement(schema)})
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
func (a *Attributor) PullRequestIssues(ctx context.Context, q Querier, keys []PullRequestKey) (map[PullRequestKey]PullRequestAttribution, error) {
	return a.pullRequestIssues(ctx, q, keys)
}

// pullRequestIssues is PullRequestIssues with options of the read, a pgx.QueryExecMode, which go
// before its arguments.
func (a *Attributor) pullRequestIssues(ctx context.Context, q Querier, keys []PullRequestKey, options ...any) (map[PullRequestKey]PullRequestAttribution, error) {
	statements, err := a.attribution()
	if err != nil {
		return nil, err
	}
	repos := make([]string, len(keys))
	numbers := make([]int, len(keys))
	for i, key := range keys {
		repos[i], numbers[i] = key.Repo, key.Number
	}
	rows, err := q.Query(ctx, statements.pullRequestIssues, append(options, repos, numbers)...)
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
//
// The statement is planned at each call for the keys of that call (pgx.QueryExecModeCacheDescribe,
// an unnamed statement), where every other reader runs the statement its connection has prepared.
// A prepared statement takes a generic plan from its sixth run, and Postgres prices that plan for
// an array of 10 keys, so its joins over the keys' rows are nested loops. One window holds one
// pull request or thousands: on 5,000 stored pull requests, a call of 3,000 keys took 1.50 s by a
// connection's generic plan and 0.23 s by a plan of its own keys, and one of 1,000 keys 0.30 s and
// 0.11 s. A call of 10 keys costs its plan, 8.9 ms against 7.6 ms.
func (a *Attributor) attributedIssues(ctx context.Context, q Querier, prs []DeliveryPullRequest) ([]PullRequestIssue, error) {
	issues := make([]PullRequestIssue, len(prs))
	if len(prs) == 0 {
		return issues, nil
	}
	keys := make([]PullRequestKey, len(prs))
	for i, pr := range prs {
		keys[i] = PullRequestKey{Repo: pr.Repo, Number: pr.Number}
	}
	answers, err := a.pullRequestIssues(ctx, q, keys, pgx.QueryExecModeCacheDescribe)
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

// ListPullRequestsOfIssues is every timeline row of a pull request whose issue the statement gives
// as one of issueKeys, under each name the row is stored under, by issue and then number. Each
// row's IssueKey is that answer, the issue the timeline's other readers name (attributedIssues):
// a link, a reparent or a facts read since the last pass counts at this read, and a pull request
// whose answer is not known belongs to none of the issues. It never matches the stored issue_key
// alone. The Legion merge times read it (legion/metrics).
func (a *Attributor) ListPullRequestsOfIssues(ctx context.Context, q Querier, issueKeys []string) ([]DeliveryPullRequest, error) {
	if len(issueKeys) == 0 {
		return nil, nil
	}
	statements, err := a.attribution()
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, statements.pullRequestsOfIssues, issueKeys)
	if err != nil {
		return nil, fmt.Errorf("read the pull requests of %d issues: %w", len(issueKeys), err)
	}
	defer rows.Close()
	var prs []DeliveryPullRequest
	for rows.Next() {
		var answer string
		pr, err := scanPullRequest(rows, &answer)
		if err != nil {
			return nil, fmt.Errorf("read the pull requests of %d issues: %w", len(issueKeys), err)
		}
		pr.IssueKey = &answer
		prs = append(prs, pr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the pull requests of %d issues: %w", len(issueKeys), err)
	}
	return prs, nil
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
		` + renameJoin("timeline_rename", "d.repo") + `
		` + namesJoin("timeline", "lower("+canonicalOver("timeline_rename", "d.repo")+")") + `
		where d.unfetchable_at is null and not exists (
			select 1 from pull_request_heads x
			where x.number = d.number and lower(x.repo) = any(` + namesOver("timeline", "lower("+canonicalOver("timeline_rename", "d.repo")+")") + `))`

// factsLineSelect is the facts line's select over its terms: each entry under its canonical name,
// the name of a stored results row that is not the canonical name, the name of its timeline row,
// and what the line orders it by.
var factsLineSelect = `
	select a.repo, a.number,
		coalesce((select x.repo from pull_request_heads x
			where x.number = a.number and lower(x.repo) = any(a.names) and lower(x.repo) <> lower(a.repo)
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
// attribution_keys with an empty body and an empty list of commit messages: the keys of the
// title and the branch alone.
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

// remakeChunk is how many texts rows one statement of RemakeKeys remakes: 1,000 in every build. A
// var, and the statement's argument, so a test of the chunks shrinks it (shrinkRemakeChunk) and
// makes a remake of several chunks from hundreds of rows rather than thousands.
var remakeChunk = 1000

// remakeKeysStatement is one chunk of RemakeKeys: at most $1 (remakeChunk) texts rows on another
// reading, a row that a writer holds skipped, each written with its own title so the trigger
// pull_request_texts_keys writes its keys and the reading that runs now.
func remakeKeysStatement(schema string) string {
	return `with id as (select ` + schema + `.attribution_reading() as id), chunk as (
		select t.repo, t.number from pull_request_texts t, id
		where t.reading < id.id or t.reading > id.id
		limit $1
		for update of t skip locked
	) update pull_request_texts x set title = x.title from chunk c, id
	-- The test of the reading below repeats the chunk's and is not its guard: the chunk's
	-- "for update of t skip locked" checks the condition again on each row it locks, and this
	-- statement holds that lock until its update, so the repeat excludes no row.
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
	statements, err := preparedReading()
	if err != nil {
		return 0, err
	}
	remade := 0
	for {
		tag, err := pool.Exec(ctx, statements.remakeKeys, remakeChunk)
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
	statements, err := preparedReading()
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
