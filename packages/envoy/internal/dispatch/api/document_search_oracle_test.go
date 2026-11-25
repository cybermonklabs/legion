package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// This file holds the statements of R1 as main writes them, copied from the text this change
// replaces: their document arm reads each document's latest version through a lateral join over
// artifact_versions and tests `v.search @@ q.tsq` there. They are the oracle of the equality
// tests in document_search_test.go, which run each statement beside the package's own and
// compare the rows.
//
// Every piece is a literal copy, including the pieces this change does not touch (the fused
// score, searchPageAndTailSQL, the meaning CTEs, relatedCoordinationRank) and the two expressions
// the package builds in Go (store.CastableUUID's cast and askBlockNodeSQL), so that nothing of
// the oracle is built from the package's own helpers: an oracle sharing a helper with the
// statement under test changes with it, and the comparison then holds nothing. The cost is that
// an edit to a part of these statements this change leaves alone has to be copied here too,
// which is what the equality tests report when it is not - INFRA-1042's one scored list across
// the kinds (mainFusedSQL for search and mainRelatedFusedSQL for the related-item check,
// ts_rank_cd's normalisation flag, a page that names no kind) is
// copied below for exactly that reason, so the only thing these statements still answer
// differently by is their document arm.
//
// INFRA-370 is such an edit, and two of its three parts are copied here. Every keyword arm now
// carries two more columns, window_id and covers; a keyword arm matched no window and has
// nothing to put in them, so main's arms carry them as nulls, for the column shape alone. The
// page names one output column more, the block a meaning hit matched, which the package computes
// in a lateral over covers; what stands in its place here is a literal null, which is that
// column's own value for every row this file's statements can produce, and is argued where it
// is written.
//
// The third part could not be copied. Meaning search itself moved from one vector per item to
// one per window of neighbouring blocks (migration 20261102233625), and this release's indexers
// write the window kinds and delete the whole-text rows main's meaning arms read. Four of
// mainSearchQueryMeaningBody's five arms therefore find nothing at all over this release's rows,
// so a meaning statement built from it decides nothing: it would hold a working statement
// against a blind one. The equality test's two meaning cases run against main's meaning
// statements as main ran them at 497581c7 instead (mainAt4975SearchQueryMeaningScoped and
// mainAt4975RelatedMeaningScopedQuery, search_oracle_test.go), which read the windows and are
// written out whole, so no line of either moves with the package's own text.
// mainSearchQueryMeaningBody is kept for the timing harness, which measures it on a database at
// the schema before this release, where it is still the text that ran.
//
// The end of the file is the meaning document arm's own oracle test (R2a,
// TestTheMeaningDocumentArmAnswersMainsRowsAndDates), which holds this build's arm to main's arm
// of 497581c7, the one search_oracle_test.go freezes: these older texts know no window and hold
// no such arm.

// mainSearchKeywordLegs is main's searchKeywordLegs: the five-kind keyword union whose document
// arm this change moves to document_search. That arm is a named piece of its own
// (mainDocumentKeywordLeg), the side the measurement holds this change's arm against.
//
// Each arm carries window_id and covers as nulls, which main's arms did not: INFRA-370 added the
// two columns to every arm of both unions, and a keyword arm has nothing to put in them.
// mainSearchQueryMeaningBody's meaning arms carry the same two nulls, so its union keeps one
// column shape. Nothing reads either column out of a keyword row.
//
// The issue arm's own column reads q.own_keys, the array INFRA-1042 made of main's scalar
// own_key: main's two openers below bind it as the one key, or the none, that main's own_key
// was, and `= any(<one key>)` is main's `= <that key>` row for row.
func mainSearchKeywordLegs(rank func(searchColumn string) string) string {
	return `
  select 'issue' as kind, 'keyword' as list, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
         null::text as window_id, null::text[] as covers,
         i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
         i.key = any(q.own_keys) as own, ` + rank("i.search") + ` as r
    from issues i, q where i.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
` + mainDocumentKeywordLeg(rank) + `
  union all
  select 'comment' as kind, 'keyword', c.issue_key, c.artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid), c.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, ` + rank("c.search") + `
    from comments c
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where c.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
  union all
  select 'ask' as kind, 'keyword', k.issue_key, case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) else null::uuid end,
         coalesce(k.artifact_id, k.block_artifact_id, (case when pg_input_is_valid(k.anchor->>'artifact_id', 'uuid') then (k.anchor->>'artifact_id')::uuid end)), k.id::text, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name, coalesce(i.updated_at, k.created_at),
         false, ` + rank("k.search") + `
    from asks k
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where k.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
  union all
  select 'message' as kind, 'keyword', m.issue_key, null::uuid, null::uuid, m.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         false, ` + rank("m.search") + `
    from messages m join issues i on i.key = m.issue_key, q
   where m.search @@ q.tsq and ($2 = '' or i.project_key = $2)`
}

// mainDocumentKeywordLeg is main's document arm alone, the before side of the one arm this
// change rewrote: it tests `v.search @@ q.tsq` against each document's latest version through a
// lateral over artifact_versions, which no index can answer. Like the arm that replaces it, the
// text carries no newline of its own at either end.
func mainDocumentKeywordLeg(rank func(searchColumn string) string) string {
	return `  select 'document' as kind, 'keyword', a.issue_key, case when a.issue_key is null then a.id else null::uuid end, a.id, a.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, v.created_at),
         false, ` + rank("v.search") + `
    from artifacts a
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select v.search, v.created_at from artifact_versions v
                  where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or p.key = $2)`
}

// mainSearchKeywordLegsSQL is main's searchKeywordLegsSQL, those legs ranked by ts_rank_cd with
// INFRA-1042's length normalisation (32).
var mainSearchKeywordLegsSQL = mainSearchKeywordLegs(func(searchColumn string) string {
	return "ts_rank_cd(" + searchColumn + ", q.tsq, 32)"
})

// mainSearchPageAndTailSQL is main's searchPageAndTailSQL, which this change leaves as it is: its
// own lateral over artifact_versions reads the page's documents' markdown for the snippet, and
// tests no tsquery.
//
// Two output columns are not main's. Where the package's page names the block a meaning hit
// matched (hit.block_id, INFRA-370), this writes a literal null. The package computes that column
// in a lateral over r.covers, and no row either body here hands this page carries any: a keyword
// leg writes null for covers, and so do main's meaning arms, which know no windows. unnest of no
// covers is no row, so the package's own lateral answers null for every one of those rows too.
// The null written here is therefore that column's own value rather than a stand-in, and the
// lateral's twenty-five lines are not copied into a place where they could only ever answer
// null. The statements whose rows can carry covers are this release's own meaning ones, and
// they are held against main's meaning statements as search_oracle_test.go freezes them, whose
// page carries that lateral. The other is the text the headline is cut from, which the package
// selects beside the headline for the snippet's reading (pmdoc.SearchSnippet); it is copied here
// as the package writes it, a lateral the headline reads too, since its value is a row's own.
func mainSearchPageAndTailSQL(legsSource string) string {
	return fmt.Sprintf(`page as (
  select l.*, f.score
    from %s l join fused f using (kind, id)
   order by f.score desc, l.updated_at desc, l.id
   limit $3 offset $6
)
select t.total, t.reachable, r.kind, case when r.owner_artifact_id is null then 'issue' else 'document' end,
       r.issue_key, r.issue_title, r.issue_status,
       r.owner_project, r.owner_slug, r.owner_artifact_id::text, r.owner_name,
       ar.slug, ar.name, coalesce(ar.is_primary, false), r.id, r.block_id, null::text, r.score::float8,
       ts_headline('english', src.text, q.tsq, $5) as headline, src.text
  from totals t cross join q
  left join (page r left join artifacts ar on ar.id = r.artifact_id) on true
  -- Mirrors the kind arms in kinds above (issue/document/comment/ask/message -> table and text
  -- column); the two must stay in sync. issue reuses r.issue_title, already carried from the
  -- same issues row by kinds, rather than re-reading it. ask reads the text its search vector is
  -- built from (asks_search: the question, then ask_search_suffix's option labels and
  -- descriptions, answer text and selected labels), so its snippet shows what the query matched.
  left join lateral (
    select case r.kind
      when 'issue' then r.issue_title
      when 'document' then (select v.markdown from artifact_versions v where v.artifact_id = r.artifact_id order by v.number desc limit 1) -- re-resolves the latest version kinds already found once; accepted, bounded by the page size
      when 'comment' then (select c.body from comments c where c.id = r.id::uuid)
      when 'ask' then (select k.question || ' ' || ask_search_suffix(k.options, k.answer) from asks k where k.id = r.id::uuid)
      when 'message' then (select m.body from messages m where m.id = r.id::uuid)
    end as text
  ) txt on true
  left join lateral (
    select search_text(case when q.term <> '' and strpos(lower(txt.text), lower(q.term)) > 0
             then substr(txt.text, greatest(1, strpos(lower(txt.text), lower(q.term)) - 1500), 4000)
             else left(txt.text, 4000) end) as text
  ) src on true
 order by r.score desc, r.updated_at desc, r.id
`, legsSource)
}

// mainSearchQOpener is main's searchQOpener, its own_key as the one-key array the legs above
// read (`= any(array[k])` is `= k`).
const mainSearchQOpener = `
with q as (select websearch_to_tsquery('english', $1) as tsq, $4::text as term, array[upper(btrim($1))]::text[] as own_keys),
`

// mainFusedSQL is main's fused CTE for search's own statement: the weighted sum of the legs'
// scores, searchFusedSQL's numbers written out.
const mainFusedSQL = `fused as (
  select kind, id,
         sum(case list when 'keyword' then 1 * r else 0.4 * r end) / ($8 + 1)
           + case when bool_or(own) then 1 else 0 end as score
    from legs group by kind, id
),
`

// mainRelatedFusedSQL is main's fused CTE for the related-item check's two statements:
// relatedFusedSQL's reciprocal rank fusion written out. The check's keyword leg is ranked by a
// sum of idf, which the weighted sum above cannot weigh against a cosine, so copying that sum
// here instead would hold the check's statement against an oracle scoring its rows by something
// the check does not run.
const mainRelatedFusedSQL = `fused as (
  select kind, id, bool_or(own) as own_lead, sum(1.0 / ($8 + pos)) as score
    from legs group by kind, id
),
`

// mainSearchQueryBody is main's searchQueryBody, over the fused CTE its caller scores with
// (mainFusedSQL for search's own statement, mainRelatedFusedSQL for the related-item check's),
// as the package's own searchQueryBody takes one.
func mainSearchQueryBody(keywordLegs, fused string) string {
	return `kinds as (` + keywordLegs + `
),
ranked as (
  select *, row_number() over (partition by kind order by own desc, r desc, updated_at desc, id) as pos,
         count(*) over (partition by kind) as matches
    from kinds
),
legs as (
  select * from ranked where pos <= $7
),
` + fused + `totals as (
  select (select coalesce(sum(matches), 0) from (select max(matches) as matches from legs group by kind) each_kind) as total,
         (select count(*) from fused) as reachable
),
` + mainSearchPageAndTailSQL("legs")
}

// mainSearchQueryMeaningBody is main's searchQueryMeaningBody. Its four non-issue arms read the
// whole-text embedding kinds ('document', 'comment', 'ask', 'message'), which this release's
// indexers no longer write and delete as each owner gains a window (migration 20261102233625),
// so those four answer nothing at all over this release's rows. That is why the equality test
// holds its two meaning statements against main's meaning statements of 497581c7
// (search_oracle_test.go) rather than against this text, and why this text is left here for the
// timing harness alone, which measures it on a database at the schema before this release.
//
// Its meaning arms carry window_id and covers as nulls for the reason mainSearchKeywordLegs' arms
// do: the keyword legs this body unions carry them, and a union's arms need one column shape.
func mainSearchQueryMeaningBody(keywordLegs string) string {
	return `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id
   where e.kind = 'issue' and e.embedding is not null and e.model = $11 and ($2 = '' or i.project_key = $2)
   order by e.embedding <=> $9::vector
   limit $7
),
meaning_issue as (
  select 'issue' as kind, 'meaning' as list, issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, id, null::text as block_id,
         null::text as window_id, null::text[] as covers,
         issue_title, issue_status, null::text as owner_project, null::text as owner_slug, null::text as owner_name, updated_at,
         false as own, r
    from meaning_issue_candidates where r >= $10
),
meaning_document_candidates as (
  select a.issue_key, a.id, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join artifacts a on a.id::text = e.id
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true
   where e.kind = 'document' and a.kind = 'doc' and e.embedding is not null and e.model = $11 and ($2 = '' or p.key = $2)
   order by e.embedding <=> $9::vector
   limit $7
),
meaning_document as (
  select 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         null::text, null::text[],
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
),
meaning_comment_candidates as (
  select c.issue_key, c.artifact_id, c.anchor, c.id, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, c.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join comments c on c.id::text = e.id
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key
   where e.kind = 'comment' and e.embedding is not null and e.model = $11 and ($2 = '' or coalesce(i.project_key, p.key) = $2)
   order by e.embedding <=> $9::vector
   limit $7
),
meaning_comment as (
  select 'comment' as kind, 'meaning' as list, issue_key, artifact_id, coalesce(artifact_id, (anchor->>'artifact_id')::uuid), id::text, null::text,
         null::text, null::text[],
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_comment_candidates where r >= $10
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id, i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id::text = e.id
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key
   where e.kind = 'ask' and e.embedding is not null and e.model = $11 and ($2 = '' or coalesce(i.project_key, p.key) = $2)
   order by e.embedding <=> $9::vector
   limit $7
),
meaning_ask as (
  select 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (case when pg_input_is_valid(anchor->>'artifact_id', 'uuid') then (anchor->>'artifact_id')::uuid end)), id::text, block_id,
         null::text, null::text[],
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
),
meaning_message_candidates as (
  select m.issue_key, m.id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join messages m on m.id::text = e.id
    join issues i on i.key = m.issue_key
   where e.kind = 'message' and e.embedding is not null and e.model = $11 and ($2 = '' or i.project_key = $2)
   order by e.embedding <=> $9::vector
   limit $7
),
meaning_message as (
  select 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         null::text, null::text[],
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
),
kinds as (` + keywordLegs + `
  union all select * from meaning_issue
  union all select * from meaning_document
  union all select * from meaning_comment
  union all select * from meaning_ask
  union all select * from meaning_message
),
ranked as (
  select *, row_number() over (partition by kind, list order by own desc, r desc, updated_at desc, id) as pos
    from kinds
),
legs as (
  select * from ranked where pos <= $7
),
fused as (
  select kind, id,
         sum(case list when 'keyword' then 1 * r else 0.4 * r end) / ($8 + 1)
           + case when bool_or(own) then 1 else 0 end as score
    from legs group by kind, id
),
legs_unique as (
  select distinct on (kind, id) kind, issue_key, owner_artifact_id, artifact_id, id, block_id,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at
    from legs
   order by kind, id
),
totals as (
  select (select count(*) from (select distinct kind, id from kinds) u) as total,
         (select count(*) from fused) as reachable
),
` + mainSearchPageAndTailSQL("legs_unique")
}

// mainRelatedQOpener is main's relatedQOpener, its own_key as the empty array the legs above
// read: main's empty string matched no issue's key, and neither does an array of none.
const mainRelatedQOpener = `
with q as (select $1::tsquery as tsq, $4::text as term, '{}'::text[] as own_keys),
`

// mainRelatedTermWeights is main's relatedTermWeights.
func mainRelatedTermWeights(lexemes int) string {
	return fmt.Sprintf(`term_weights as (
  select quote_literal(w.lexeme)::tsquery as tq, w.idf from unnest($%d::text[], $%d::float8[]) w(lexeme, idf)
),
`, lexemes, lexemes+1)
}

// mainRelatedCoordinationRank is main's relatedCoordinationRank.
func mainRelatedCoordinationRank(searchColumn string) string {
	return "((select sum(w.idf) from term_weights w where " + searchColumn + " @@ w.tq) + 0.001 * ts_rank_cd(" + searchColumn + ", q.tsq, 32))"
}

// The whole statements of R1 the bodies above make. Main's two keyword statements are the oracle
// of the equality test's keyword cases, as mainRelatedProbeSQL is of its probe case; the two
// meaning statements are the timing harness's alone, for the reason given at the top of this
// file.
var (
	mainSearchQuery        = mainSearchQOpener + mainSearchQueryBody(mainSearchKeywordLegsSQL, mainFusedSQL)
	mainSearchQueryMeaning = mainSearchQOpener + mainSearchQueryMeaningBody(mainSearchKeywordLegsSQL)
	mainRelatedKeywordLegs = mainSearchKeywordLegs(mainRelatedCoordinationRank)
	// mainRelatedDocumentKeywordLeg is the document arm of those legs alone, the before side the
	// measurement holds relatedDocumentKeywordLeg against.
	mainRelatedDocumentKeywordLeg = mainDocumentKeywordLeg(mainRelatedCoordinationRank)
	mainRelatedKeywordQuery       = mainRelatedQOpener + mainRelatedTermWeights(9) + mainSearchQueryBody(mainRelatedKeywordLegs, mainRelatedFusedSQL)
	mainRelatedMeaningQuery       = mainRelatedQOpener + mainRelatedTermWeights(12) + mainSearchQueryMeaningBody(mainRelatedKeywordLegs)
)

// mainRelatedProbeSQL is main's relatedProbeSQL, the fifth statement of R1.
const mainRelatedProbeSQL = `
with probe as (
  select l.lexeme, quote_literal(l.lexeme)::tsquery as tq from unnest($2::text[]) l(lexeme)
),
any_probe as (
  select string_agg(quote_literal(lexeme), ' | ')::tsquery as tq from probe
),
document_hits as (
  select distinct u.lexeme
    from artifacts a
    join lateral (select v.search from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true
    cross join any_probe
    cross join lateral unnest(v.search) u
   where a.kind = 'doc' and a.project_key = $1
     and a.issue_key is distinct from $3 and a.id is distinct from $4::uuid
     and v.search @@ any_probe.tq and u.lexeme = any($2::text[])
)
select p.lexeme from probe p
 where exists (select 1 from issues i where i.search @@ p.tq and i.project_key = $1 and i.key <> $3)
    or exists (select 1 from document_hits d where d.lexeme = p.lexeme)
    or exists (select 1 from comments c
                 left join issues i on i.key = c.issue_key
                 left join artifacts a on a.id = c.artifact_id
                where c.search @@ p.tq and coalesce(i.project_key, a.project_key) = $1
                  and c.issue_key is distinct from $3 and ($4::uuid is null or c.artifact_id is distinct from $4::uuid))
    or exists (select 1 from asks k
                 left join issues i on i.key = k.issue_key
                 left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
                where k.search @@ p.tq and coalesce(i.project_key, a.project_key) = $1
                  and k.issue_key is distinct from $3
                  and ($4::uuid is null or coalesce(k.artifact_id, k.block_artifact_id) is distinct from $4::uuid))
    or exists (select 1 from messages m join issues i on i.key = m.issue_key
                where m.search @@ p.tq and i.project_key = $1 and m.issue_key <> $3)`

// meaningDocumentDateBase is the instant the date fixture's dates are counted from.
var meaningDocumentDateBase = time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)

// meaningDocumentDateWord is in every version of the date fixture's documents and in its issues'
// titles, so the keyword legs of the statements over that fixture answer rows beside the meaning
// arms.
const meaningDocumentDateWord = "heron"

// meaningDocumentDateFixture is the corpus of R2a: twelve documents of three versions each, each
// version written five days after the one before it and each document's first version a day
// after the one before it, so a document's three versions have three different created_at.
// Documents one to six are owned by the six issues of project, seven to ten by project itself
// and eleven and twelve by otherProject. Each document has two windows embedded near the query's
// vector, at a distance of their own.
//
// since lies between two versions' dates: after the second version and before the third of
// documents four to eight, and after the first and before the second of nine to twelve, while
// all three versions of documents one to three are older. So a filter reading the newest
// version's date keeps nine documents of the twelve, and one reading an older version's keeps
// fewer.
type meaningDocumentDateFixture struct {
	database              *store.Store
	embedder              *fakeEmbedder
	project, otherProject string
	// issueKey is the issue the related-item check's excluding statements leave out.
	issueKey  string
	since     time.Time
	documents []meaningDocumentDateDocument
}

// meaningDocumentDateDocument is one document of the fixture as the database holds it: its
// project, the updated_at of the issue owning it (nil for a project's document), and its
// versions' created_at in the order of their numbers.
type meaningDocumentDateDocument struct {
	id, project    string
	issueUpdatedAt *time.Time
	versions       []time.Time
}

// newMeaningDocumentDateFixture writes the fixture as SQL, every version with the created_at the
// fixture gives it and every window settled at the fake embedder's identity: the issue rows,
// the documents, their versions (whose trigger, 20261101182454, holds each document's
// document_search row at its newest version), and their windows, each a vector as angledVector
// builds one (pairVectorSQL), with a cosine of its own to queryVector between 0.64 and 0.87.
func newMeaningDocumentDateFixture(t *testing.T) meaningDocumentDateFixture {
	t.Helper()
	embedder := &fakeEmbedder{vectors: map[string][]float32{}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	fixture := meaningDocumentDateFixture{
		database: database, embedder: embedder, project: "MARSH", otherProject: "DELTA", issueKey: "MARSH-1",
		since: meaningDocumentDateBase.Add(18*24*time.Hour + 12*time.Hour),
	}
	createTestProject(t, handler, fixture.project, fixture.otherProject)
	ctx := context.Background()
	for _, step := range []struct {
		what, statement string
		arguments       []any
	}{
		{"the issues", `
			insert into issues (key, project_key, number, title, status, created_by, rank, created_at, updated_at)
			select $1::text || '-' || n, $1::text, n, 'Heron survey ' || n, case when n % 2 = 0 then 'done' else 'todo' end,
			       '{"kind":"user","id":"alice"}', lpad(n::text, 20, '0'),
			       $2::timestamptz, $2::timestamptz + make_interval(days => n)
			from generate_series(1, 6) n
		`, []any{fixture.project, meaningDocumentDateBase}},
		{"the documents", `
			insert into artifacts (project_key, issue_key, slug, name, kind, created_by)
			select case when n <= 10 then $1::text else $2::text end, case when n <= 6 then $1::text || '-' || n end,
			       'heron-notes-' || n, 'Heron notes ' || n || '.md', 'doc', '{"kind":"user","id":"alice"}'
			from generate_series(1, 12) n
		`, []any{fixture.project, fixture.otherProject}},
		{"their versions", `
			insert into artifact_versions (artifact_id, number, markdown, authors, created_at)
			select a.id, v,
			       '# Heron notes ' || d.n || E'\n\n' || case when v = 3 then 'Heron counts at the marsh, sheet ' || d.n
			                                                  else 'Draft ' || v || ' of sheet ' || d.n end || E'.\n',
			       '[]', $1::timestamptz + make_interval(days => d.n + 5 * v)
			from artifacts a
			cross join lateral (select split_part(a.slug, '-', 3)::int as n) d
			cross join generate_series(1, 3) v
			where a.slug like 'heron-notes-%'
		`, []any{meaningDocumentDateBase}},
		{"their windows", `
			insert into embeddings (kind, id, text_snapshot, content_hash, embedded_hash, model, embedding, embedded_at, covers)
			select 'document_block', a.id::text || '#b' || w, 'Window b' || w || ' of ' || a.name,
			       md5(a.id::text || w), md5(a.id::text || w), $1,
			       ` + pairVectorSQL("1", "2 + 2 * d.n + w", "0.9 - 0.02 * d.n - 0.01 * w") + `,
			       now(), array['b' || w]
			from artifacts a
			cross join lateral (select split_part(a.slug, '-', 3)::int as n) d
			cross join generate_series(1, 2) w
			where a.slug like 'heron-notes-%'
		`, []any{embedder.Identity()}},
		{"the statistics", `analyze`, nil},
	} {
		if _, err := database.Pool.Exec(ctx, step.statement, step.arguments...); err != nil {
			t.Fatalf("write %s: %v", step.what, err)
		}
	}

	rows, err := database.Pool.Query(ctx, `
		select a.id::text, a.project_key, i.updated_at, array_agg(v.created_at order by v.number)
		from artifacts a
		left join issues i on i.key = a.issue_key
		join artifact_versions v on v.artifact_id = a.id
		where a.slug like 'heron-notes-%'
		group by a.id, a.project_key, i.updated_at`)
	if err != nil {
		t.Fatalf("read the documents back: %v", err)
	}
	defer rows.Close()
	straddled := 0
	for rows.Next() {
		var document meaningDocumentDateDocument
		if err := rows.Scan(&document.id, &document.project, &document.issueUpdatedAt, &document.versions); err != nil {
			t.Fatalf("scan a document: %v", err)
		}
		if newest := len(document.versions) - 1; newest > 0 && document.versions[newest-1].Before(fixture.since) && !document.versions[newest].Before(fixture.since) {
			straddled++
		}
		fixture.documents = append(fixture.documents, document)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the documents back: %v", err)
	}
	if len(fixture.documents) != 12 || straddled == 0 {
		t.Fatalf("the fixture holds %d documents, %d of them with since between their two newest versions; want 12, and some",
			len(fixture.documents), straddled)
	}
	return fixture
}

// pairVectorSQL is a vector of embed.Dimension as SQL, built as angledVector builds one in Go: a
// unit vector holding similarity at the 1-based position axis and the rest of its length at the
// position other, above axis, so that its cosine with the unit vector of axis is similarity. Each
// argument is an SQL expression, similarity a number between 0 and 1. A fixture writes a corpus's
// vectors with it in one statement, where a vector built in Go would be a parameter of its own
// per row.
func pairVectorSQL(axis, other, similarity string) string {
	return fmt.Sprintf(`('[' || repeat('0,', (%[1]s) - 1) || ((%[3]s)::float8)::text
	        || ',' || repeat('0,', (%[2]s) - (%[1]s) - 1) || sqrt(1 - ((%[3]s)::float8) ^ 2)::text
	        || repeat(',0', %[4]d - (%[2]s)) || ']')::vector`, axis, other, similarity, embed.Dimension)
}

// meaningDocumentArmStatement is one meaning statement R2a runs over the date fixture: this
// build's text and main's at 497581c7 (search_oracle_test.go), bound as the caller binds it,
// with the project and the updated_since that binding narrows by.
type meaningDocumentArmStatement struct {
	name, live, main string
	// mainArm is main's meaning_document_candidates as search_oracle_test.go names it apart, for
	// search's three statements; the related-item check's arm is not named apart there.
	mainArm   string
	project   string
	since     *time.Time
	arguments []any
}

// statements is search's three meaning statements, each for every project form its caller
// binds it with and each with no updated_since and with the fixture's, and the related-item
// check's six, whose q narrows by no updated_since (relatedQOpener): the unscoped ones for a
// check naming no project, the scoped and joined ones for the fixture's project, and the
// excluding ones leaving the fixture's first issue out, as an issue create leaves its own.
func (f meaningDocumentDateFixture) statements() []meaningDocumentArmStatement {
	word := meaningDocumentDateWord
	related := func(project string, excluding bool) []any {
		arguments := slices.Concat(
			[]any{"'" + word + "'", project, suggestionSearchDepth, word, headlineOptions, 0, contracts.SearchKindDepth, searchFusionK},
			[]any{embed.Literal(queryVector()), searchMeaningFloor, f.embedder.Identity()},
			[]any{[]string{word}, []float64{2.5}})
		if excluding {
			arguments = append(arguments, []string{"issue:" + f.issueKey})
		}
		return append(arguments, documentSearchSeen()...)
	}
	var statements []meaningDocumentArmStatement
	for _, search := range []struct {
		name, live, main, mainArm string
		projects                  []string
	}{
		{"searchQueryMeaning", searchQueryMeaning, mainAt4975SearchQueryMeaning, mainAt4975MeaningDocumentCandidates, []string{""}},
		{"searchQueryMeaningScoped", searchQueryMeaningScoped, mainAt4975SearchQueryMeaningScoped, mainAt4975MeaningDocumentCandidatesScoped,
			[]string{f.project}},
		{"searchQueryMeaningJoined", searchQueryMeaningJoined, mainAt4975SearchQueryMeaningJoined, mainAt4975MeaningDocumentCandidatesJoined,
			[]string{"", f.project}},
	} {
		for _, project := range search.projects {
			for _, since := range []*time.Time{nil, &f.since} {
				name := search.name + ", every project"
				if project != "" {
					name = search.name + ", " + project
				}
				if since != nil {
					name += ", updated since"
				}
				statements = append(statements, meaningDocumentArmStatement{
					name: name, live: search.live, main: search.main, mainArm: search.mainArm, project: project, since: since,
					arguments: searchMeaningQueryArguments(
						searchParameters{text: word, project: project, limit: searchDefaultLimit, since: since},
						queryVector(), f.embedder.Identity()),
				})
			}
		}
	}
	return append(statements,
		meaningDocumentArmStatement{name: "relatedMeaningQuery", live: relatedMeaningQuery, main: mainAt4975RelatedMeaningQuery,
			arguments: related("", false)},
		meaningDocumentArmStatement{name: "relatedMeaningScopedQuery", live: relatedMeaningScopedQuery, main: mainAt4975RelatedMeaningScopedQuery,
			project: f.project, arguments: related(f.project, false)},
		meaningDocumentArmStatement{name: "relatedMeaningJoinedQuery", live: relatedMeaningJoinedQuery, main: mainAt4975RelatedMeaningJoinedQuery,
			project: f.project, arguments: related(f.project, false)},
		meaningDocumentArmStatement{name: "relatedMeaningExcludingQuery", live: relatedMeaningExcludingQuery,
			main: mainAt4975RelatedMeaningExcludingQuery, arguments: related("", true)},
		meaningDocumentArmStatement{name: "relatedMeaningScopedExcludingQuery", live: relatedMeaningScopedExcludingQuery,
			main: mainAt4975RelatedMeaningScopedExcludingQuery, project: f.project, arguments: related(f.project, true)},
		meaningDocumentArmStatement{name: "relatedMeaningJoinedExcludingQuery", live: relatedMeaningJoinedExcludingQuery,
			main: mainAt4975RelatedMeaningJoinedExcludingQuery, project: f.project, arguments: related(f.project, true)},
	)
}

// meaningDocumentArmRow is one row of meaning_document_candidates as R2a reads it: the document,
// the window that matched, the date the arm gives the row, and the row's cosine to the query.
type meaningDocumentArmRow struct {
	id, window string
	updatedAt  time.Time
	r          float64
}

// The text a meaning statement holds its candidate CTEs between: the first of them opens after
// the statement's q (and the related-item check's term weights), and the document arm is
// followed by the arm over it, meaning_document.
const (
	meaningArmsStart        = "meaning_issue_candidates as ("
	meaningDocumentArmStart = "meaning_document_candidates as ("
	meaningDocumentArmEnd   = "\n),\nmeaning_document as ("
)

// meaningDocumentArmOf cuts statement into what opens it, everything before its first candidate
// CTE, and its meaning_document_candidates CTE from its name to its closing parenthesis, which
// is how search_oracle_test.go names main's arm apart.
func meaningDocumentArmOf(t *testing.T, statement string) (opener, arm string) {
	t.Helper()
	arms := strings.Index(statement, meaningArmsStart)
	start := strings.Index(statement, meaningDocumentArmStart)
	if arms < 0 || start < arms {
		t.Fatalf("the statement holds no %q after its opener:\n%s", meaningDocumentArmStart, statement)
	}
	length := strings.Index(statement[start:], meaningDocumentArmEnd)
	if length < 0 {
		t.Fatalf("the statement's document arm is followed by no %q:\n%s", meaningDocumentArmEnd, statement)
	}
	return statement[:arms], statement[start : start+length+len("\n)")]
}

// meaningDocumentArmRows runs statement's meaning_document_candidates alone, bound to arguments,
// and answers its rows ordered by document and window. The CTE runs behind the statement's own
// opener, and beside it a CTE that names every parameter cast to the type Postgres gives it in
// the whole statement (statementLiterals): the CTE reads only some of them, and a parameter no
// part of a statement reads has no type Postgres can bind it with. Nothing reads that CTE, so
// it is planned away.
func meaningDocumentArmRows(t *testing.T, connection *pgxpool.Conn, statement string, arguments []any) []meaningDocumentArmRow {
	t.Helper()
	ctx := context.Background()
	opener, arm := meaningDocumentArmOf(t, statement)
	types, _ := statementLiterals(t, connection, statement, arguments)
	typed := make([]string, len(types))
	for index, name := range types {
		typed[index] = fmt.Sprintf("$%d::%s", index+1, name)
	}
	rows, err := connection.Query(ctx, opener+arm+",\nevery_parameter as (select "+strings.Join(typed, ", ")+")\n"+
		"select id::text, window_id, updated_at, r from meaning_document_candidates order by id, window_id", arguments...)
	if err != nil {
		t.Fatalf("run the meaning document arm alone: %v", err)
	}
	defer rows.Close()
	var read []meaningDocumentArmRow
	for rows.Next() {
		var row meaningDocumentArmRow
		if err := rows.Scan(&row.id, &row.window, &row.updatedAt, &row.r); err != nil {
			t.Fatalf("scan a row of the meaning document arm: %v", err)
		}
		row.updatedAt = row.updatedAt.UTC()
		read = append(read, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the meaning document arm: %v", err)
	}
	return read
}

// requireDocumentsAndDates holds rows, this build's meaning document arm over the fixture for a
// statement narrowed to project (every project when empty) and to since (none when nil), to the
// fixture itself: both windows of every document in that project whose newest version is no
// older than since, and nothing else, each dated by its issue's updated_at, or by that newest
// version's created_at for a project's document, which is the date document_search holds.
func (f meaningDocumentDateFixture) requireDocumentsAndDates(t *testing.T, rows []meaningDocumentArmRow, project string, since *time.Time) {
	t.Helper()
	answered := map[string][]meaningDocumentArmRow{}
	for _, row := range rows {
		answered[row.id] = append(answered[row.id], row)
	}
	for _, document := range f.documents {
		windows := answered[document.id]
		delete(answered, document.id)
		newest := document.versions[len(document.versions)-1]
		if project != "" && document.project != project || since != nil && newest.Before(*since) {
			if len(windows) > 0 {
				t.Errorf("the arm answered document %s of %s, newest version %s, which the project or updated_since leaves out: %v",
					document.id, document.project, newest, windows)
			}
			continue
		}
		if len(windows) != 2 {
			t.Errorf("the arm answered %d windows of document %s, want its two: %v", len(windows), document.id, windows)
		}
		want, whose := newest, "its newest version's created_at"
		if document.issueUpdatedAt != nil {
			want, whose = *document.issueUpdatedAt, "its issue's updated_at"
		}
		for _, window := range windows {
			if !window.updatedAt.Equal(want) {
				t.Errorf("the arm dated window %s %s, want %s, %s", window.window, window.updatedAt, want, whose)
			}
		}
	}
	for id, windows := range answered {
		t.Errorf("the arm answered %s, no document of the fixture: %v", id, windows)
	}
}

// TestTheMeaningDocumentArmAnswersMainsRowsAndDates holds R2a and R2c: the meaning document arm
// reads each document's date from document_search.created_at and answers the rows main's arm
// does, which read the newest version's created_at through a lateral over artifact_versions. Over
// documents of three versions with different dates, owned by issues and by projects, it runs the
// arm alone (meaningDocumentArmRows) from this build's statement and from main's at 497581c7,
// and compares each row's document, window, date and cosine, for every project scope and with
// an updated_since between two versions' dates. Then it runs each whole statement, this build's
// and main's, under the settings the route runs meaning statements under
// (searchScanSettings(searchMeaningScan), the bound among them), and compares the answers row
// for row.
//
// The statements are search's three meaning statements and the related-item check's six, which
// share searchQueryMeaningBody and so the arm. For search's three, the arm cut from main's
// statement must be the arm search_oracle_test.go names apart, so what runs as main's arm is
// main's frozen text. The comparison decides something only while the arm answers rows, so this
// build's rows are also held to the fixture (requireDocumentsAndDates), and each whole answer
// must name a document of the fixture.
func TestTheMeaningDocumentArmAnswersMainsRowsAndDates(t *testing.T) {
	t.Parallel()
	fixture := newMeaningDocumentDateFixture(t)
	ctx := context.Background()
	connection, err := fixture.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the arm's reads: %v", err)
	}
	defer connection.Release()

	for _, statement := range fixture.statements() {
		t.Run(statement.name, func(t *testing.T) {
			if statement.mainArm != "" {
				if _, arm := meaningDocumentArmOf(t, statement.main); arm != statement.mainArm {
					t.Fatalf("the arm cut from main's statement is not main's frozen arm:\n%s\nwant\n%s", arm, statement.mainArm)
				}
			}
			want := meaningDocumentArmRows(t, connection, statement.main, statement.arguments)
			got := meaningDocumentArmRows(t, connection, statement.live, statement.arguments)
			if !slices.Equal(got, want) {
				t.Errorf("this build's meaning document arm answered\n%v\nwant main's\n%v", got, want)
			}
			fixture.requireDocumentsAndDates(t, got, statement.project, statement.since)

			settings := searchScanSettings(searchMeaningScan)
			wantAnswer := documentSearchRows(t, ctx, fixture.database, statement.main, statement.arguments, settings)
			gotAnswer := documentSearchRows(t, ctx, fixture.database, statement.live, statement.arguments, settings)
			if !slices.Equal(gotAnswer, wantAnswer) {
				t.Errorf("this build's statement answered\n%s\nwant main's\n%s", strings.Join(gotAnswer, "\n"), strings.Join(wantAnswer, "\n"))
			}
			answer := strings.Join(wantAnswer, "\n")
			if !slices.ContainsFunc(fixture.documents, func(document meaningDocumentDateDocument) bool {
				return strings.Contains(answer, document.id)
			}) {
				t.Errorf("main's statement named no document of the fixture, so the comparison decided nothing:\n%s", answer)
			}
		})
	}
}
