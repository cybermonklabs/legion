package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A search at a past time, the cut (decision 8 of INFRA-4620's plan), runs statements of its own.
// Leg builders of their own make them (searchAsOfKeywordLegs, searchAsOfMeaningBody), beside the
// present ones, so no statement of the related-item check changes (related_statements.go). They
// run under the cut, so the store binds them (asof.Bind): a table that a statement names bare is
// its shadow at the cut, and a table that it names with its schema is the present table.
//
//   - The keyword arms of issues, comments, messages and asks read the keyword table at the cut
//     (store.TextVersions, the CTE text_versions) in place of the items' search columns. Each arm
//     matches and ranks the version of the item that held at the cut. A comment or a message
//     that has a present deleted_at gives no hit (decision 14).
//   - The keyword arms of comments, asks and messages read each post's issue at the cut once for
//     all its matched posts, and a post's own columns from its present row; a column a write
//     changes they read from the shadow only where a filter or the page needs it
//     (searchAsOfPosts, searchAsOfShown). A search that narrows by no status, author or time and
//     whose query has a word to rank by reads the issue and the present row only of the posts that
//     can take one of their kind's places, ranked by the keyword table's columns first
//     (searchAsOfBoundedPosts).
//   - The document arm reads each document's newest version at the cut (searchAsOfDocumentLegs).
//   - The issue arms read the issues shadow, and every row filter reads the state at the cut.
//   - A meaning arm walks its windows nearest first and keeps a window only when its item did not
//     change after the cut, and only a window embedded at or before the cut (searchAsOfMeaningBody).
//   - Each hit's snippet comes from the text of its item at the cut (searchAsOfTexts).
//
// A search at a cut does not broaden (D2): search_term_stats counts the present text only.

// searchAsOfVersions is the members of a past-time statement's WITH list after q, for a search
// that ranks each post kind's matches first (ranksFirst: one that narrows by no status, author or
// time, and whose query has a word to rank by; searchParameters.ranksFirstAtACut) or for one that
// reads them whole:
//
//   - text_versions, the keyword table at the cut. It is NOT MATERIALIZED, so that each read of
//     it reaches the table's indexes. The arm of the issues the query names by key, the meaning
//     arms and the snippets read it.
//   - text_matched, its versions that the query matches, which the keyword arms read. It is
//     MATERIALIZED: it reads the table's GIN index once, with the query of q. As one relation
//     joined by each arm, the planner took the table's versions at the cut as the outer side, for
//     an estimate of a few dozen rows (it cannot read how many versions a later version ends),
//     joined each version to its item's shadow, and tested the match last: on a copy of
//     production's event log, every comment of a cut, and more than two minutes for a search at
//     the newest cut. It carries each match's rank (searchKeywordRank) in place of its vector, so
//     its rows are small, and not the relation's deleted_at, which the relation reads from the
//     post's present row by its primary key: text_standing reads the index of the deleted posts
//     when ranksFirst, and text_posts reads that column from the row itself when not. When
//     ranksFirst, it carries the version's project_key too, which text_standing narrows by.
//   - The matched posts at the cut: the bounded pipeline when ranksFirst (searchAsOfBoundedPosts,
//     whose text_bounds binds the acting credential at seen), or text_posts when not
//     (searchAsOfPosts).
//   - text_issues, the issues of text_posts at the cut (searchAsOfIssues).
func searchAsOfVersions(versions store.TextVersions, ranksFirst bool, seen int) string {
	columns, posts := "v.kind, v.id, ", searchAsOfPosts
	if ranksFirst {
		columns, posts = "v.kind, v.id, v.project_key, ", searchAsOfBoundedPosts(seen)
	}
	return "text_versions as not materialized " + versions.SQL() + ",\n" +
		"text_matched as materialized (select " + columns + searchKeywordRank("v.search") +
		" as r from text_versions v, q where v.search @@ q.tsq),\n" + posts + searchAsOfIssues
}

// searchAsOfPosts is text_posts for a search that reads each post kind's matches whole: one that
// narrows by a status, an author or a time, or whose query has no word to rank by
// (searchParameters.ranksFirstAtACut). The keyword arms of comments, asks and messages read it
// with text_issues (searchAsOfIssues) in place of the shadows of their items and of the items'
// issues (searchAsOfPostArms).
//
// text_posts is each matched version of a comment, an ask or a message, with the post's columns
// that no write changes after its insert - id, issue_key, artifact_id, author and created_at, as
// asof's commentsShadow, asksShadow and messagesShadow name them - read from its present row
// (public.comments, public.asks, public.messages) by its primary key, and a comment or a message
// whose present row has a deleted_at left out (decision 14; the relation's own deleted_at is that
// column of that row). The version at the cut proves the post was there at the cut: a post's
// first version starts at the event that made it, and a version holds at the cut only once it has
// started. So the present row stands for the shadow's in every column the arms rank and filter by.
// Every row is read: a filter may leave out any of them, and a query with no word to rank by
// ranks every row 0, so no bound on the rank can leave one out. A row a filter leaves out must
// take no place in its kind's ranking.
//
// Measured on a copy of production's event log, the keyword-only statement for `issue` at the
// copy's newest event (2026-11-24T13:41:25.812227Z): 55,084 versions match, 38,588 of them
// comments, 13,376 messages, 2,644 asks and 476 issues, and the 55,326 posts belong to 8,695
// issues. With each match joined to its post's shadow and its issue's, the comments shadow probed
// events_comment_state 38,588 times (7.0 s on a first run, 0.5 s on a second) and the issues
// shadow read events_issue_state and events_issue_activity 38,381 times for the comments alone
// (about 3.8 s and 1.5 s), and the statement took 31.1 s and 7.7 s. Read here, the issues shadow
// is read for 8,695 issues, and a post's shadow only where a filter or the page needs a column a
// write changes: its edited_at inside the updated_since filter (searchAsOfEditedAt), an ask's
// typed block inside the project filter when its issue does not decide it (searchAsOfAskProject),
// and the columns a hit shows (searchAsOfShown). The same EXPLAIN (ANALYZE, BUFFERS), run twice
// back to back on that copy before and after this change, where the second run of the old
// statement still read 215,231 blocks from outside shared_buffers: 37.3 s and 31.2 s before,
// 20.3 s and 3.3 s after. Of the 20.3 s, text_matched took 7.3 s, text_posts 6.5 s and
// text_issues 4.8 s; of the 3.3 s, 0.8 s, 0.9 s and 0.4 s.
var searchAsOfPosts = `text_posts as materialized (
  select 'comment' as kind, c.id, c.issue_key, c.artifact_id, c.author, c.created_at, v.r
    from text_matched v join public.comments c on c.id = ` + store.CanonicalUUID(`v.id`) + `
   where v.kind = 'comment' and c.deleted_at is null
  union all
  select 'ask', k.id, k.issue_key, k.artifact_id, k.author, k.created_at, v.r
    from text_matched v join public.asks k on k.id = ` + store.CanonicalUUID(`v.id`) + `
   where v.kind = 'ask'
  union all
  select 'message', m.id, m.issue_key, null::uuid, m.author, m.created_at, v.r
    from text_matched v join public.messages m on m.id = ` + store.CanonicalUUID(`v.id`) + `
   where v.kind = 'message' and m.deleted_at is null
),
`

// searchAsOfBoundedPosts is the post CTEs for a search that ranks each post kind first
// (searchParameters.ranksFirstAtACut): one that narrows by no status, author or time and whose
// query has a word to rank by. It reads a post's present row and its issue at the cut only for the
// posts that can take one of their kind's places. ranked orders each kind's rows by own, r,
// updated_at and id, and only its first $7 (contracts.SearchKindDepth) reach legs
// (searchQueryBodyReading, searchMeaningRankedPage, where the keyword rows of a kind are a list of
// their own). Every comment, ask and message row has own false, and updated_at, which the issue at
// the cut gives, orders only rows of equal r. So a row whose r is below the r of its kind's $7-th
// row by r takes no place, whatever its updated_at, and each kind's rows are ranked by r first,
// from columns the keyword table holds:
//
//   - text_standing is each matched version of a comment, an ask or a message that the project
//     filter keeps, by the version's own project_key, and that names no post a delete has marked,
//     an anti-join on the index of the deleted posts (migrations 20261124162630_comments_deleted
//     and 20261124162631_messages_deleted), so no row of a post that stands is read. Its id is a
//     uuid's canonical spelling, the one a present row's id joins (store.CanonicalUUID) and the
//     one the arms write as c.id::text.
//   - text_bounds is each kind's boundary: the least r of its first $7 rows by r that the acting
//     person may see (refs.VisibleKind, bound at seen), since kinds leaves out every other row
//     before it ranks. The predicate is read as the rows come in order of r, so for $7 rows and
//     those the person may not see among them, not for every row. A kind with fewer such rows has
//     the least r of them all as its boundary, and keeps every row the person may see.
//   - text_candidates keeps all rows of text_standing at or above the boundary, including ties.
//     It is materialized so the join to the post's present row (text_posts) cannot pull that
//     read below the rank comparison.
//
// The post arms read text_posts and text_issues for those rows, and pass each row below its
// kind's boundary with its id and its rank alone. These rows still pass through kinds' visibility
// check, so matches and totals count the same rows. A visible row below the boundary ranks after
// at least $7 visible rows, so it reaches no leg. If the boundary is null, no row of that kind
// was visible in the first place.
//
// text_standing keeps the rows searchAsOfPosts and searchAsOfPostArms keep for a search that
// narrows by nothing but a project, by what the present rows hold. Nothing moves a post or an
// issue between projects (project_scope.go), so the project_key the fill wrote for a version (its
// event's, else its issue's, else its document's) is the project the arms of searchAsOfPostArms
// test. No route removes a post row: deletion marks a comment or message instead, and an
// artifact with posts cannot be removed. The keyword table holds only issue messages, and a
// message that stood at the cut is on an issue that stood at the cut.
// If a write starts removing posts or moving them between projects, revisit this bound and
// the stub rows it passes to the final ranking.
func searchAsOfBoundedPosts(seen int) string {
	matched := store.CanonicalUUID(`v.id`)
	standing := func(kind, deleted string) string {
		arm := `  select v.kind, v.id, v.r
    from text_matched v
   where v.kind = '` + kind + `' and ($2 = '' or v.project_key = $2) and ` + matched + ` is not null`
		if deleted != "" {
			arm += `
     and not exists (select from public.` + deleted + ` d where d.id = ` + matched + ` and d.deleted_at is not null)`
		}
		return arm
	}
	bound := func(kind string) string {
		return `  select '` + kind + `' as kind, (
           select min(top.r) from (
             select s.r from (select w.id, w.r from text_standing w where w.kind = '` + kind + `' order by w.r desc offset 0) s
              where ` + refs.VisibleKind(kind, "s.id", searchParam(seen)) + `
              order by s.r desc limit $7) top) as r`
	}
	kept := func(kind, columns, table, alias string) string {
		return `  select w.kind, ` + columns + `, w.r
    from text_candidates w
    join public.` + table + ` ` + alias + ` on ` + alias + `.id = w.id
   where w.kind = '` + kind + `'`
	}
	return `text_standing as materialized (
` + standing("comment", "comments") + `
  union all
` + standing("ask", "") + `
  union all
` + standing("message", "messages") + `
),
text_bounds as materialized (
` + bound("comment") + `
  union all
` + bound("ask") + `
  union all
` + bound("message") + `
),
text_candidates as materialized (
  select w.kind, w.id::uuid as id, w.r
    from text_standing w join text_bounds b on b.kind = w.kind and w.r >= b.r
),
text_posts as materialized (
` + kept("comment", "c.id, c.issue_key, c.artifact_id, c.created_at", "comments", "c") + `
  union all
` + kept("ask", "k.id, k.issue_key, k.artifact_id, k.created_at", "asks", "k") + `
  union all
` + kept("message", "m.id, m.issue_key, null::uuid, m.created_at", "messages", "m") + `
),
`
}

// searchAsOfIssues is text_issues: each issue at the cut (the issues shadow) that a post of
// text_posts names, read once however many of its posts match, and each post arm joins it in
// place of the shadow.
const searchAsOfIssues = `text_issues as materialized (
  select i.key, i.project_key, i.title, i.status, i.updated_at
    from issues i
   where i.key in (select p.issue_key from text_posts p)
),
`

// searchAsOfLiteral is the cut of versions as a timestamptz literal (asof.Cut.Literal). It comes
// from the time value alone, never from the text of a request, as the relation writes it.
func searchAsOfLiteral(versions store.TextVersions) string {
	return asof.CutAt(versions.Cut).Literal()
}

// searchAsOfKeywordLegs is searchKeywordLegs at the cut, a timestamptz literal: the same arms,
// filters and rank (searchKeywordRank), over the version of each item at the cut that the query
// matches (text_matched, searchAsOfVersions, which carries each match's rank as r) in place of the
// item's search column. The issue arm joins the issues shadow by the version's id. The comment,
// ask and message arms read text_posts and text_issues: what ranks a row, scores it and orders the
// page (own, r, updated_at and id) and what the filters read comes from there. They are
// searchAsOfBoundedPostArms for a search that ranks first (ranksFirst, searchAsOfBoundedPosts) and
// searchAsOfPostArms for one that reads each post kind's matches whole. The columns a hit only
// shows that a write changes - a comment's document as its anchor names it, an ask's documents and
// its typed block - are null here and read for the page's rows alone (searchAsOfShown).
func searchAsOfKeywordLegs(cut string, ranksFirst bool) string {
	posts := searchAsOfPostArms
	if ranksFirst {
		posts = searchAsOfBoundedPostArms
	}
	rank := searchKeywordRank("v.search")
	return `
  select 'issue' as kind, 'keyword' as list, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
         null::text as window_id, null::text[] as covers,
         i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
         false as own, v.r as r
    from text_matched v join issues i on i.key = v.id, q
   where v.kind = 'issue' and not (i.key = any(q.own_keys)) and ($2 = '' or i.project_key = $2)` +
		searchRowFilters("i.status", "i.created_by", "i.updated_at") + `
  union all
  select 'issue', 'keyword', i.key, null::uuid, null::uuid, i.key, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         true, ` + rank + `
    from q, unnest(q.own_keys) as named(key), issues i, text_versions v
   where i.key = named.key and v.kind = 'issue' and v.id = i.key and ($2 = '' or i.project_key = $2)` +
		searchRowFilters("i.status", "i.created_by", "i.updated_at") + `
  union all
` + searchAsOfDocumentLegs(cut) + `
  union all
` + posts
}

// searchAsOfCommentArm, searchAsOfAskArm and searchAsOfMessageArm are each post kind's arm at the
// cut up to its last join: the columns of the kind's arm in searchKeywordLegs, read from text_posts
// and text_issues. searchAsOfPostArms and searchAsOfBoundedPostArms hold them once and each ends
// an arm with a tail of its own: the project test and the row filters in a search that reads each
// post kind whole, the kind test alone in a search that ranks first.
const (
	searchAsOfCommentArm = `  select 'comment' as kind, 'keyword', c.issue_key, c.artifact_id, null::uuid, c.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, c.r
    from text_posts c
    left join text_issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key`
	searchAsOfAskArm = `  select 'ask' as kind, 'keyword', k.issue_key, null::uuid, null::uuid, k.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, coalesce(i.updated_at, k.created_at),
         false, k.r
    from text_posts k
    left join text_issues i on i.key = k.issue_key`
	searchAsOfMessageArm = `  select 'message' as kind, 'keyword', m.issue_key, null::uuid, null::uuid, m.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         false, m.r
    from text_posts m join text_issues i on i.key = m.issue_key`
)

// searchAsOfPostArms is the comment, ask and message arms at the cut for a search that reads each
// post kind's matches whole (searchAsOfPosts): each matched post of text_posts, with its issue at
// the cut from text_issues, narrowed by the project and the three filters as the present arms
// narrow (searchKeywordLegs).
var searchAsOfPostArms = searchAsOfCommentArm + `, q
   where c.kind = 'comment' and ($2 = '' or coalesce(i.project_key, p.key) = $2)` +
	searchRowFilters("i.status", "c.author", "coalesce("+searchAsOfEditedAt("comments", "c")+", c.created_at)") + `
  union all
` + searchAsOfAskArm + `, q
   where k.kind = 'ask' and ($2 = '' or ` + searchAsOfAskProject + ` = $2)` +
	searchRowFilters("i.status", "k.author", "coalesce("+searchAsOfEditedAt("asks", "k")+", k.created_at)") + `
  union all
` + searchAsOfMessageArm + `, q
   where m.kind = 'message' and ($2 = '' or i.project_key = $2)` +
	searchRowFilters("i.status", "m.author", "coalesce("+searchAsOfEditedAt("messages", "m")+", m.created_at)")

// searchAsOfBoundedPostArms is the comment, ask and message arms at the cut for a search that
// ranks first (searchAsOfBoundedPosts): the columns of searchAsOfPostArms for each post at or above
// its kind's boundary, read from text_posts and text_issues, and each row of text_standing below
// its kind's boundary with its id and its rank alone, which no leg reaches and kinds counts. The
// search narrows by nothing but a project, and text_standing has applied that, so these arms test
// none.
const searchAsOfBoundedPostArms = searchAsOfCommentArm + `
   where c.kind = 'comment'
  union all
` + searchAsOfAskArm + `
   where k.kind = 'ask'
  union all
` + searchAsOfMessageArm + `
   where m.kind = 'message'
  union all
  select w.kind, 'keyword', null::text, null::uuid, null::uuid, w.id, null::text,
         null::text, null::text[],
         null::text, null::text, null::text, null::text, null::text, null::timestamptz,
         false, w.r
    from text_standing w join text_bounds b on b.kind = w.kind
   where w.r < b.r`

// searchAsOfEditedAt is the edited_at at the cut of the post aliased post (a row of text_posts),
// read from table's shadow (comments, asks or messages) by the post's id. It is the one column the
// row filters read of a post that a write changes, and only the updated_since filter reads it,
// which searchRowFilters writes as `q.updated_since is null or ...`: a search that does not narrow
// by that filter stops at the first term and reads no shadow of a post for it.
func searchAsOfEditedAt(table, post string) string {
	return "(select x.edited_at from " + table + " x where x.id = " + post + ".id)"
}

// searchAsOfAskProject is the project the ask aliased k (a row of text_posts) belongs to at the
// cut, as the present ask arm reads it, coalesce(i.project_key, p.key): its issue's (i, a row of
// text_issues), else the project of the document coalesce(artifact_id, block_artifact_id) names,
// which is its own document where it has one, else the document its typed block was in at the
// cut. A write changes block_artifact_id, so the asks shadow gives it. coalesce evaluates no value
// after the first that is not null, and an ask has an issue or a document of its own, never both
// (asks_one_owner), so the shadow is read only for an ask whose issue is not at the cut.
var searchAsOfAskProject = `coalesce(i.project_key, (
           select p.key from artifacts a join projects p on p.key = a.project_key
            where a.id = coalesce(k.artifact_id, (select b.block_artifact_id from asks b where b.id = k.id))))`

// searchAsOfDocumentLegs is searchDocumentKeywordLeg at the cut, a timestamptz literal: each
// document's newest version at the cut, the newest artifact_versions row with markdown whose
// insert began at or before the cut (the shadow). document_search holds each document's present
// newest version with markdown, and its created_at is that version's: trigger 20261101182454 and
// store.FillDocumentSearch both copy it from the version, and the boot's fill gives every document
// with markdown its row before the server serves. So a document's newest version at the cut is its
// present newest one exactly when that row's created_at is at or before the cut, and the two arms
// split the documents on it:
//
//   - At or before the cut: the row's own version, matched through document_search's GIN index.
//   - After the cut: the newest version at the cut, read through
//     artifact_versions_artifact_id_number_key, its own search column tested, which no index
//     holds.
//
// Each arm compares the row's created_at with the cut in place of reading the document's newest
// version at the cut to compare numbers: the second arm read it for every document, 43,910 probes
// of artifact_versions_artifact_id_number_key and 1.6 s of the first run of the keyword-only
// statement for `issue` at the newest event of a copy of production's event log, to find the
// documents whose newest version came later, of which that cut has none. Both arms carry the
// present arm's columns, filters and rank.
func searchAsOfDocumentLegs(cut string) string {
	columns := `  select 'document' as kind, 'keyword', a.issue_key, case when a.issue_key is null then a.id else null::uuid end, a.id, a.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, v.created_at),
         false, ` + searchKeywordRank("v.search")
	return columns + `
    from document_search v
    join artifacts a on a.id = v.artifact_id
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or p.key = $2) and v.created_at <= ` + cut +
		searchRowFilters("i.status", "a.created_by", "v.created_at") + `
  union all
` + columns + `
    from document_search d
    join artifacts a on a.id = d.artifact_id
    cross join lateral (select n.number, n.created_at, n.search from artifact_versions n
                         where n.artifact_id = a.id and n.markdown is not null
                         order by n.number desc limit 1) v
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key, q
   where a.kind = 'doc' and d.created_at > ` + cut + ` and v.search @@ q.tsq and ($2 = '' or p.key = $2)` +
		searchRowFilters("i.status", "a.created_by", "v.created_at")
}

// searchAsOfTexts is the page's texts at a cut (searchPageAndTailReading): a document hit's
// newest version at the cut (the artifact_versions shadow), and every other hit's version in the
// keyword table at the cut, the text its arm matched.
const searchAsOfTexts = `  -- At a cut, each hit's text as it stood at the cut: a document's newest version at the cut,
  -- and every other kind's version in the keyword table, which its arm matched.
  left join lateral (
    select case r.kind
      when 'document' then (select v.markdown from artifact_versions v where v.artifact_id = r.artifact_id order by v.number desc limit 1)
      else (select tv.body from text_versions tv where tv.kind = r.kind and tv.id = r.id)
    end as text
  ) txt on true
`

// searchAsOfRankedPage is the name that searchPageAndTailReading gives the ranked page at a cut,
// and searchAsOfShown reads the page from it. One constant names it in both, so the two cannot
// differ.
const searchAsOfRankedPage = "ranked_page"

// searchAsOfShown is the page at a cut (searchPageAndTailReading's shown): the ranked page, with
// the columns a comment's or an ask's hit shows read for the page's rows alone, from the shadows.
// They are the columns a write changes - a comment's anchor, an ask's typed block and the
// document it is in - which the keyword arms leave null (searchAsOfKeywordLegs), since reading
// them there read the shadow of every match. No order reads them, so reading them after the page
// is cut moves no row. They are what the present arms answer: a comment's document, its own or
// its anchor's; an ask's owner document where it has no issue, its document (its own, its block's
// or its anchor's), its typed block while that block is a node of the graph (askBlockNodeSQL), and
// the project, slug and name of its own or its block's document. A meaning arm's row carries the
// same values from the same shadows, so its rows take them from here too.
var searchAsOfShown = `,
page as (
  select r.kind, r.issue_key,
         case r.kind when 'ask' then shown_ask.owner_artifact_id else r.owner_artifact_id end as owner_artifact_id,
         case r.kind when 'ask' then shown_ask.artifact_id when 'comment' then shown_comment.artifact_id else r.artifact_id end as artifact_id,
         r.id, case r.kind when 'ask' then shown_ask.block_id else r.block_id end as block_id, r.covers,
         r.issue_title, r.issue_status,
         case r.kind when 'ask' then shown_ask.owner_project else r.owner_project end as owner_project,
         case r.kind when 'ask' then shown_ask.owner_slug else r.owner_slug end as owner_slug,
         case r.kind when 'ask' then shown_ask.owner_name else r.owner_name end as owner_name,
         r.updated_at, r.score, r.own_lead
    from ` + searchAsOfRankedPage + ` r
    left join lateral (
      select case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) end as owner_artifact_id,
             coalesce(k.artifact_id, k.block_artifact_id, ` + store.CastableUUID(`k.anchor->>'artifact_id'`) + `) as artifact_id,
             ` + askBlockNodeSQL + ` as block_id,
             p.key as owner_project, a.slug as owner_slug, a.name as owner_name
        from asks k
        left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
        left join projects p on p.key = a.project_key
       where r.kind = 'ask' and k.id = ` + store.CanonicalUUID(`r.id`) + `
      offset 0
    ) shown_ask on true
    left join lateral (
      select coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid) as artifact_id
        from comments c
       where r.kind = 'comment' and c.id = ` + store.CanonicalUUID(`r.id`) + `
      offset 0
    ) shown_comment on true
)`

// searchAsOfQuery is searchQuery at the cut of versions: the same parameters, the acting
// credential at searchSeenParam among them, over the keyword legs at the cut
// (searchAsOfKeywordLegs), their texts (searchAsOfTexts) and the page that reads what a hit shows
// (searchAsOfShown). Which nodes the acting person may see is read as it stands now
// (refs.VisibleKind and refs.VisibleEnd over graph_nodes, which keeps no history), so a past-time
// search answers no node the reader may not see today. ranksFirst is whether the search ranks each
// post kind first (searchParameters.ranksFirstAtACut), which chooses how the post arms read their
// posts (searchAsOfVersions): a statement built with ranksFirst true applies none of the three
// filters to its posts, so it serves only a search that narrows by none of them.
func searchAsOfQuery(versions store.TextVersions, ranksFirst bool) string {
	return searchQOpener(9) + searchAsOfVersions(versions, ranksFirst, searchSeenParam) +
		searchQueryBodyReading(searchAsOfKeywordLegs(searchAsOfLiteral(versions), ranksFirst), searchFusedSQL, "", searchAsOfTexts, searchAsOfShown, searchSeenParam)
}

// searchAsOfMeaningQuery is searchQueryMeaning at the cut of versions: the same seventeen
// parameters, over searchAsOfMeaningBody. It has one shape: each meaning arm joins its owner and
// narrows by the project and the three filters inside its candidate limit, as the joined shape
// does (meaningOneProjectJoined), which answers for every row whether or not the fill of
// embeddings.project_key labelled it. ranksFirst chooses its keyword legs' reading of the posts, as
// it does searchAsOfQuery's.
func searchAsOfMeaningQuery(versions store.TextVersions, ranksFirst bool) string {
	return searchQOpener(12) + searchAsOfVersions(versions, ranksFirst, searchMeaningSeenParam) +
		searchAsOfMeaningBody(searchAsOfLiteral(versions), ranksFirst, searchMeaningSeenParam)
}

// searchAsOfMeaningBody is searchQueryMeaningBody at the cut, a timestamptz literal: each meaning
// arm keeps a window only when the window was embedded at or before the cut with the query's
// model identity ($11), since a window embedded later was pending at the cut (D1), and only when
// its item did not change after the cut. The item's version at the cut (text_versions) is
// compared with its present row, as text and version and never as vectors: an edit of case or
// punctuation alone keeps the vector, and the fill lags, so the time a version ended is no
// evidence.
//
//   - An issue: its title, and its primary document and that document's version, equal the
//     present title, the present primary document and its newest version.
//   - A document: its newest version at the cut is its present newest version.
//   - A comment, a message or an ask: its text equals the present text (an ask's question, a
//     space and ask_search_suffix of its options and answer), and a post has no present
//     deleted_at.
//
// Each arm walks its windows nearest first and tests each one as the scan hands it up
// (searchAsOfWindows): the test holds what the arm's joins and filters held, so the arm's limit
// counts the windows that pass them all, in the same order, and the arm then joins the item's
// columns to the windows it kept. searchAsOfWindows says why it walks so and what that costs.
//
// The keyword legs are the ones at the cut (searchAsOfKeywordLegs, whose reading of the posts
// ranksFirst chooses), the snippets come from the texts at the cut (searchAsOfTexts) and the page
// reads what a hit shows (searchAsOfShown). seen is the parameter the acting credential is bound
// at: each walk's test binds its own kind's visibility predicate, as searchQueryMeaningBody's
// candidate CTEs do before their limits, so a node the acting person may not see takes no place
// in the cut.
func searchAsOfMeaningBody(cut string, ranksFirst bool, seen int) string {
	project := meaningOneProjectJoined.candidateProject
	return `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at, w.r
    from ` + searchAsOfWindows("issue", cut, `e.id`, `
                from issues i
                join text_versions v on v.kind = 'issue' and v.id = i.key
                join public.issues present_issue on present_issue.key = i.key
                left join public.artifacts present_spec on present_spec.issue_key = present_issue.key and present_spec.is_primary, q
               where i.key = e.id
                 and v.body = present_issue.title and v.spec_artifact_id is not distinct from present_spec.id
                 and v.spec_version is not distinct from (select max(n.number) from public.artifact_versions n where n.artifact_id = present_spec.id)
                 and `+refs.VisibleKind("issue", "i.key", searchParam(seen))+
		project("i.project_key")+searchRowFilters("i.status", "i.created_by", "i.updated_at"), "limit $7") + `
    join issues i on i.key = w.item
),
` + searchMeaningIssueProjection + `meaning_document_candidates as (
  select a.issue_key, a.id, w.window_id, w.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at, w.r
    from ` + searchAsOfWindows("document_block", cut, windowOwnerUUID, `
                from artifacts a
                left join issues i on i.key = a.issue_key
                join projects p on p.key = a.project_key
                join lateral (select v.number, v.created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
               where a.id = `+windowOwnerUUID+` and a.kind = 'doc'
                 and v.number = (select max(n.number) from public.artifact_versions n where n.artifact_id = a.id)
                 and `+refs.VisibleKind("artifact", "a.ref_key", searchParam(seen))+
		project("p.key")+searchRowFilters("i.status", "a.created_by", "v.created_at"), searchWindowLimit) + `
    join artifacts a on a.id = w.item
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select v.created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true
),
` + searchMeaningDocumentProjection + `meaning_comment_candidates as (
  select c.issue_key, c.artifact_id, c.anchor, c.id, w.window_id, w.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, c.created_at) as updated_at, w.r
    from ` + searchAsOfWindows("comment_block", cut, windowItemUUID, `
                from comments c
                join text_versions v on v.kind = 'comment' and v.id = c.id::text
                join public.comments present_comment on present_comment.id = c.id
                left join issues i on i.key = c.issue_key
                left join artifacts a on a.id = c.artifact_id
                left join projects p on p.key = a.project_key, q
               where c.id = `+windowItemUUID+`
                 and v.deleted_at is null and v.body = present_comment.body
                 and `+refs.VisibleKind("comment", "c.id::text", searchParam(seen))+
		project(searchMeaningCommentProject)+searchRowFilters("i.status", "c.author", "coalesce(c.edited_at, c.created_at)"), searchWindowLimit) + `
    join comments c on c.id = w.item
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key
),
` + searchMeaningCommentJoinedProjection + `,
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, ` + askBlockNodeSQL + ` as block_id, w.window_id, w.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at, w.r
    from ` + searchAsOfWindows("ask_part", cut, windowItemUUID, `
                from asks k
                join text_versions v on v.kind = 'ask' and v.id = k.id::text
                join public.asks present_ask on present_ask.id = k.id
                left join issues i on i.key = k.issue_key
                left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
                left join projects p on p.key = a.project_key, q
               where k.id = `+windowItemUUID+`
                 and v.body = present_ask.question || ' ' || ask_search_suffix(present_ask.options, present_ask.answer)
                 and `+refs.VisibleKind("ask", "k.id::text", searchParam(seen))+
		project("coalesce(i.project_key, p.key)")+searchRowFilters("i.status", "k.author", "coalesce(k.edited_at, k.created_at)"), searchWindowLimit) + `
    join asks k on k.id = w.item
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key
),
` + searchMeaningAskProjection + `meaning_message_candidates as (
  select m.issue_key, m.id, w.window_id, w.covers, i.title as issue_title, i.status as issue_status, i.updated_at, w.r
    from ` + searchAsOfWindows("message_block", cut, windowItemUUID, `
                from messages m
                join text_versions v on v.kind = 'message' and v.id = m.id::text
                join public.messages present_message on present_message.id = m.id
                join issues i on i.key = m.issue_key, q
               where m.id = `+windowItemUUID+`
                 and v.deleted_at is null and v.body = present_message.body
                 and `+refs.VisibleKind("message", "m.id::text", searchParam(seen))+
		project("i.project_key")+searchRowFilters("i.status", "m.author", "coalesce(m.edited_at, m.created_at)"), searchWindowLimit) + `
    join messages m on m.id = w.item
    join issues i on i.key = m.issue_key
),
` + searchMeaningMessageProjection + searchMeaningRankedPage(searchAsOfKeywordLegs(cut, ranksFirst), searchFusedSQL, "", searchAsOfTexts, searchAsOfShown, seen)
}

// searchAsOfWindows is one meaning arm's walk at the cut, under the alias w: the windows of
// embeddings of kind embedded at or before the cut with the query's model, nearest to the query's
// embedding ($9) first, each kept only when check holds of it, until limit of them are kept. check
// is the from list and the where clause of a select over the window e: the window's item, its
// version at the cut, its present row and the arm's filters, which the arm joined and filtered
// before. Each kept window carries its id, its covers, its similarity r and item, an expression
// over e that gives the key the arm joins its item by.
//
// The walk is two levels. The inner one reads the kind's windows with their cheap filters alone,
// nearest first; OFFSET 0 keeps it a subquery of its own, so the planner pushes no outer test into
// it. The outer one tests each window the walk hands up and stops at limit. So the test runs only
// on the windows the walk reaches before limit of them pass, whichever way the planner reads the
// kind: through embeddings_cosine, or, for a kind of few windows, by reading them all through
// embeddings_kind_project and sorting them by distance. With the test beside the scan in one
// level, a plan of the second kind tested every window of the kind before it sorted them. On
// TestAsOfMeaningCostOnASyntheticCorpus's corpus at production's counts, with jit off as Cloud
// SQL runs, at its newest event, where nearly every window was embedded before the cut, the
// search with meaning so took 6.1 s and its first run after a restart of PostgreSQL ended at
// searchMeaningStatementBound; walked so, 0.86 s, and 1.1 s on its first run, against 1.9 s for
// the same search at the present. At the two earlier cuts, where few windows were embedded before
// the cut, the walk planned for its first rows reads more of the kind: 0.95 s and 1.5 s against
// 0.43 s and 0.58 s with the test beside the scan, and 3.7 s at most on a first run, inside the
// bound. The outer level has the one subquery for its table and no ORDER BY, so the planner
// passes its LIMIT down and plans the walk for its first rows.
//
// The test is a scalar subquery in the outer where clause, so no plan can read the item side first
// and the windows again for each item, whatever the planner estimates of a join: the test runs
// once for each window the walk hands up. An EXISTS would not hold that: the planner turns an
// EXISTS of the where clause into a semi-join, a join whose order is its own again, and a join
// above the walk would have the planner plan the walk for every row. LIMIT 1 holds the test to
// one row, which its keys already do. On the corpus at scale 0.01, with the test joined inside
// the ordered, limited select, the planner read the comments first, expecting 1 and getting
// 1,478, and read all 2,796 comment windows again for each of them, so every search with meaning
// at the corpus's newest event ended at searchMeaningStatementBound and answered 500.
func searchAsOfWindows(kind, cut, item, check, limit string) string {
	return `(
      select e.id as window_id, e.covers, ` + item + ` as item, 1 - e.distance as r
        from (
          select x.id, x.covers, x.embedding <=> $9::vector as distance
            from embeddings x
           where x.kind = '` + kind + `' and x.embedding is not null and x.model = $11 and x.embedded_at <= ` + cut + `
           order by x.embedding <=> $9::vector
          offset 0
        ) e
       where (select true` + check + `
               limit 1)
       ` + limit + `
    ) w`
}

// searchAskBlocksMovedStatement reads, of the asks whose ids $1 holds (uuid[]), the id of each
// ask whose typed block at the cut (the asks shadow) is in a document whose newest version at the
// cut (the artifact_versions shadow) is not its present newest version (public.artifact_versions).
// The graph indexes each document's newest version, so the graph now holds the nodes of the
// document at the cut for every other ask's block.
const searchAskBlocksMovedStatement = `select k.id::text from asks k
	where k.id = any($1::uuid[]) and k.block_artifact_id is not null and k.block_id is not null
	  and (select max(n.number) from artifact_versions n where n.artifact_id = k.block_artifact_id)
	      is distinct from (select max(p.number) from public.artifact_versions p where p.artifact_id = k.block_artifact_id)`

// searchAsksAtTheCut names what a search at a cut leaves out of each ask hit of results, from one
// reading of the hits' asks:
//   - the href, where the ask's typed block is in a document that changed after the cut
//     (searchAskBlocksMovedStatement), with reason G. Such a hit's href links the block only while
//     the block is a node of the reference graph (askBlockNodeSQL), and the graph is the graph
//     now, which no record holds at the cut for that document. Every other ask hit's link is the
//     one the graph gave at the cut.
//   - the snippet, and its tree where the answer carries display trees (trees), where the ask's
//     wording at the cut no record gives (reason E, asof.AsksChangedAfterStatement). The snippet
//     is cut from the keyword table's version of the ask at the cut (searchAsOfTexts), which holds
//     the wording of the ask's newest event at the cut, and a restore changed that wording with
//     no event at a time no record gives. The keyword arm finds such an ask by that wording. The
//     meaning arm keeps an ask's window only where the ask's text at the cut is its present text
//     (searchAsOfWindows("ask_part", ...), v.body = present_ask.question || ' ' ||
//     ask_search_suffix(...)), so it skips an ask whose present wording is the one a restore gave
//     back. The snippet is left out whichever arm found the hit.
//
// A request at no cut leaves out nothing and sends no statement.
func (s *server) searchAsksAtTheCut(ctx context.Context, results []model.SearchResult, trees bool) error {
	cut, atCut := asof.CutFrom(ctx)
	if !atCut {
		return nil
	}
	var asks []string
	for _, result := range results {
		if result.Kind == "ask" {
			asks = append(asks, result.ID)
		}
	}
	if len(asks) == 0 {
		return nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, searchAskBlocksMovedStatement, asks)
	if err != nil {
		return fmt.Errorf("read which asks' block documents changed after the cut: %w", err)
	}
	moved, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read which asks' block documents changed after the cut: %w", err)
	}
	_, wordingUnknown, err := s.asksChangedAfterTheCut(ctx, asks, cut)
	if err != nil {
		return fmt.Errorf("read which asks of a search have no wording at the cut: %w", err)
	}
	for index, result := range results {
		if result.Kind != "ask" {
			continue
		}
		place := "/results/" + strconv.Itoa(index)
		if slices.Contains(moved, result.ID) {
			leaveOut(ctx, place+"/href")
		}
		if wordingUnknown[result.ID] {
			leaveOut(ctx, place+"/snippet")
			if trees {
				leaveOut(ctx, place+"/snippet_tree")
			}
		}
	}
	return nil
}

// writeSearchNotFilled refuses a search at a cut that the keyword table's fill has not reached
// (decision 7): 409 AS_OF_SEARCH_NOT_FILLED, which names the time the fill reached in its message
// and in Dispatch-As-Of-Filled-Through, and no result. A fill that has reached no time names
// none.
func writeSearchNotFilled(w http.ResponseWriter, cut asof.Cut, refusal *store.TextVersionsNotReachedError) {
	if !refusal.Reached {
		writeError(w, contracts.DispatchAsOfSearchNotFilled, http.StatusConflict,
			"as_of: a search at "+cut.String()+" reads the keyword index of past text, and its fill has reached no time yet; search again later")
		return
	}
	reach := asof.CutAt(refusal.Reach).String()
	w.Header().Set(contracts.DispatchAsOfFilledThroughHeader, reach)
	writeError(w, contracts.DispatchAsOfSearchNotFilled, http.StatusConflict,
		"as_of: a search at "+cut.String()+" reads the keyword index of past text, and its fill has reached "+reach+
			" only; search at "+reach+" or before, or search again later")
}
