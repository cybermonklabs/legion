package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// This file holds search's statements as main ran them at commit 497581c7, frozen for this
// change: the keyword statement searchQuery, search's three meaning statements, the related-item
// check's six meaning statements, and the meaning document arm of search's three apart, and the
// rule main builds each of them for a set of kinds with (mainAt4975Statement). Their meaning
// document arm reads each document's latest version through a lateral over artifact_versions for
// its date, and they are the before side this change is held to: the plan tests fail on them,
// the measurement reads them beside the head's, and the equality tests hold the head's
// statements to their answers.
//
// Main's statements moved after df18d9be, the earlier base commit, so this file was
// frozen again at main's commit 497581c7: cybermonklabs/legion#3125 changed the headline window
// the page tail selects and added the kinds a search can leave out, and #3512 made the comment,
// ask and message arms' updated_since filters read coalesce(edited_at, created_at).
//
// The base was then moved to main's commit 588af9ce, and main's statements of the present did not
// move with it: each of the 32 sets' four statements and each statement of the related-item check
// that main builds at 588af9ce is byte for byte the text it built at 497581c7, so the mainAt4975
// texts are main's at 588af9ce as well and keep their names. By 588af9ce main had added a search at
// a past time (#4639), whose two statements run at a cut in place of search's own:
// searchAsOfQuery and searchAsOfMeaningQuery. They are frozen here too, at 588af9ce, with the
// prefix mainAt588a (mainAt588aAsOfStatement), and this change keeps them as main wrote them.
//
// Every piece is written out, including the ones the package builds in Go: refs' visibility
// predicate (refs.VisibleKind, refs.VisibleEnd) over each statement's own credential parameter,
// store.CanonicalUUID's and store.CastableUUID's casts, askBlockNodeSQL, searchWindowLimit's
// `limit 1000`, searchFusedSQL's weights and searchQOpener's parameter numbers. Nothing here
// calls the package's builders, since an oracle built from them would move with the change it
// is the oracle of. A piece several statements share is one constant, written once, and each
// statement is the concatenation of its pieces in the order the package's builders join them,
// so every statement is still a constant whose every byte is in this file.
//
// These are not the main* oracles of document_search_oracle_test.go, which are the release
// before document_search and keep their names; the mainAt4975 and mainAt588a prefixes say whose
// these are. They are frozen: they never follow an edit to the package's statements, since that
// edit is what they exist to be compared with.
//
// The last two output columns of every page tail here are not main's: where the headline's
// window cut the text it was read out of (cut_start, cut_end), which this branch's page measures
// beside the headline (searchPageAndTailReading). They are no part of what these oracles hold the
// head to, and what they answer is what the headline and the text answer, which both sides
// compute alike; so every tail holds the laterals that measure them, the excerpt and the headline
// among them, letter for letter as document_search_oracle_test.go's mainSearchPageAndTailSQL
// copies them, rather than each comparison taught to read past two columns. The columns and the
// laterals are two pieces every tail shares (searchOracleCutColumns, searchOracleCutLaterals).
//
// The end of the file is R3's two oracle tests. TestSearchAnswersMainsRowsOrderTotalAndReachable
// holds search's four statements to main's, for every set of kinds a request can name, over a
// corpus whose word matches more items of each kind than any arm's limit.
// TestASearchAtACutAnswersMainsRowsOrderTotalAndReachable holds search's two statements at a cut
// to main's at 588af9ce over a corpus written through the routes, so a cut has events to read.

// mainAt4975VisibilityHead is the opening of refs' visibility predicate as main rendered it, up
// to its comparison with the acting credential: the read of the node's graph_nodes row and the
// case that names the node's author. Each statement follows it with the comparison over its own
// credential parameter and the graph_nodes key of the node it tests.
const mainAt4975VisibilityHead = `coalesce((select visible_node.visibility <> 'author' or coalesce(case
		when coalesce(visible_node.made_by->>'owner', '') <> ''
			then jsonb_build_object('kind', 'person', 'id', case when 'person' = 'person' then user_threads_login(visible_node.made_by->>'owner') else visible_node.made_by->>'owner' end)
		when visible_node.made_by->>'kind' = 'user' and coalesce(visible_node.made_by->>'id', '') <> ''
			then jsonb_build_object('kind', 'person', 'id', case when 'person' = 'person' then user_threads_login(visible_node.made_by->>'id') else visible_node.made_by->>'id' end)
		when coalesce(visible_node.made_by->>'id', '') <> ''
		     and (coalesce(visible_node.made_by->>'kind', '') <> 'session' or coalesce((visible_node.made_by->>'proven')::boolean, false))
			then jsonb_build_object('kind', coalesce(visible_node.made_by->>'kind', ''), 'id', case when coalesce(visible_node.made_by->>'kind', '') = 'person' then user_threads_login(visible_node.made_by->>'id') else visible_node.made_by->>'id' end)
		when coalesce(visible_node.made_by->>'service', '') <> ''
			then jsonb_build_object('kind', 'service', 'id', case when 'service' = 'person' then user_threads_login(visible_node.made_by->>'service') else visible_node.made_by->>'service' end)`

// searchOracleCutColumns is the last output columns of every page tail here: the headline, the
// excerpt it was read out of, and whether the excerpt was cut at its start and at its end
// (cut_start, cut_end), which are not main's. Each tail puts it after its score.
const searchOracleCutColumns = `       hl.headline, excerpt.text,
       case when length(located.words) > 0 and located.at > 0 then excerpt.start > 1
            else (excerpt.start = 1 and edge.before !~ '[[:alnum:]]') is not true end as cut_start,
       case when length(located.words) > 0 and located.at > 0 then excerpt.start + 4000 <= length(txt.text)
            else (excerpt.start + 4000 > length(txt.text) and edge.after !~ '[[:alnum:]]') is not true end as cut_end`

// searchOracleCutLaterals is the laterals every page tail here reads searchOracleCutColumns from,
// after the lateral that reads its text (txt): the excerpt of that text, the excerpt's headline,
// where the headline's words stand in the excerpt (located) and, where they do not, the text
// either side of them (edge).
const searchOracleCutLaterals = `  left join lateral (
    select cut.start, search_text(substr(txt.text, cut.start, 4000)) as text
      from (select greatest(1, coalesce(nullif(case when q.term <> '' then strpos(lower(txt.text), lower(q.term)) end, 0), 1) - 1500) as start
            offset 0) cut
    offset 0
  ) excerpt on true
  left join lateral (select ts_headline('english', excerpt.text, q.tsq, $5) as headline offset 0) hl on true
  left join lateral (
    select shown.words, strpos(excerpt.text, shown.words) as at
      from (select replace(replace(hl.headline, E'\uE000', ''), E'\uE001', '') as words) shown
    offset 0
  ) located on true
  left join lateral (
    select left(printed.text, printed.at - 1) as before, substr(printed.text, printed.at + length(located.words)) as after
      from (select whole.text, strpos(whole.text, located.words) as at
              from (select ts_headline('english', excerpt.text, $$' '$$::tsquery, 'MaxWords=1000000, MinWords=999999, MaxFragments=1') as text
                    offset 0) whole) printed
     where length(located.words) > 0 and located.at = 0 and printed.at > 0
  ) edge on true`

// mainAt4975SearchKeywordLegs is main's searchKeywordLegsSQL: the five kinds' keyword arms, the
// issue kind twice (its matches and the issues the query names by key), each ranked by
// ts_rank_cd with length normalisation (32). The comment, ask and message arms' updated_since
// filters read coalesce(edited_at, created_at), as #3512 made them.
const mainAt4975SearchKeywordLegs = `
  select 'issue' as kind, 'keyword' as list, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
         null::text as window_id, null::text[] as covers,
         i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
         false as own, ts_rank_cd(i.search, q.tsq, 32) as r
    from issues i, q
   where i.search @@ q.tsq and not (i.key = any(q.own_keys)) and ($2 = '' or i.project_key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
  union all
  select 'issue', 'keyword', i.key, null::uuid, null::uuid, i.key, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         true, ts_rank_cd(i.search, q.tsq, 32)
    from q, unnest(q.own_keys) as named(key), issues i
   where i.key = named.key and ($2 = '' or i.project_key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
  union all
  select 'document' as kind, 'keyword', a.issue_key, case when a.issue_key is null then a.id else null::uuid end, a.id, a.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, v.created_at),
         false, ts_rank_cd(v.search, q.tsq, 32)
    from document_search v
    join artifacts a on a.id = v.artifact_id
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or p.key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
  union all
  select 'comment' as kind, 'keyword', c.issue_key, c.artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid), c.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, ts_rank_cd(c.search, q.tsq, 32)
    from comments c
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where c.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(c.edited_at, c.created_at) >= q.updated_since)
  union all
  select 'ask' as kind, 'keyword', k.issue_key, case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) else null::uuid end,
         coalesce(k.artifact_id, k.block_artifact_id, (case when pg_input_is_valid(k.anchor->>'artifact_id', 'uuid') then (k.anchor->>'artifact_id')::uuid end)), k.id::text, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name, coalesce(i.updated_at, k.created_at),
         false, ts_rank_cd(k.search, q.tsq, 32)
    from asks k
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where k.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(k.edited_at, k.created_at) >= q.updated_since)
  union all
  select 'message' as kind, 'keyword', m.issue_key, null::uuid, null::uuid, m.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         false, ts_rank_cd(m.search, q.tsq, 32)
    from messages m join issues i on i.key = m.issue_key, q
   where m.search @@ q.tsq and ($2 = '' or i.project_key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(m.edited_at, m.created_at) >= q.updated_since)`

// mainAt4975SearchQuery is main's searchQuery, the keyword statement: searchQOpener(9), then
// searchQueryBody over mainAt4975SearchKeywordLegs and searchFusedSQL with the acting credential
// at $15. It takes the fifteen parameters searchQueryArguments binds.
const mainAt4975SearchQuery = `
with q as (select
         case when $9 = '' then websearch_to_tsquery('english', $1)
              when $10 = '' then websearch_to_tsquery('english', $9)
              else websearch_to_tsquery('english', $9) && websearch_to_tsquery('english', $10)
         end as tsq,
         $4::text as term, $11::text[] as own_keys,
         $12::text as status_filter, $13::text as author_filter, $14::timestamptz as updated_since),
kinds as (select * from (` + mainAt4975SearchKeywordLegs + `
) arms where /* visibility predicate: every kind */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = case arms.kind when 'document' then 'artifact' else arms.kind end and visible_node.id = case case arms.kind when 'document' then 'artifact' else arms.kind end when 'artifact' then (select store_artifact.ref_key from artifacts store_artifact where store_artifact.id::text = arms.id::text) else arms.id::text end), true)
),
ranked as (
  select *, row_number() over (partition by kind order by own desc, r desc, updated_at desc, id) as pos,
         count(*) over (partition by kind) as matches
    from kinds
),
legs as (
  select * from ranked where pos <= $7
),
fused as (
  select kind, id, bool_or(own) as own_lead,
         sum(case list when 'keyword' then 1 * r else 0.4 * r end) / ($8 + 1)
           + case when bool_or(own) then 1 else 0 end as score
    from legs group by kind, id
),
totals as (
  select (select coalesce(sum(matches), 0) from (select max(matches) as matches from legs group by kind) each_kind) as total,
         (select count(*) from fused) as reachable
),
page as (
  select l.*, f.score, f.own_lead
    from legs l join fused f using (kind, id)
   order by f.score desc, case when f.own_lead then l.id end, l.updated_at desc, l.id
   limit $3 offset $6
)
select t.total, t.reachable, r.kind, case when r.owner_artifact_id is null then 'issue' else 'document' end,
       r.issue_key, r.issue_title, r.issue_status,
       r.owner_project, r.owner_slug, r.owner_artifact_id::text, r.owner_name,
       ar.slug, ar.name, coalesce(ar.is_primary, false), r.id, r.block_id, hit.block_id, r.score::float8,
` + searchOracleCutColumns + `
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
    offset 0
  ) txt on true
` + searchOracleCutLaterals + `
  -- The block of r.covers that matched, if the row carries a window. Each covered block's own
  -- text comes from the table its indexer writes it to: a document's, a comment's and an ask's
  -- block texts are kept per version or revision, the latest being the current text. A message
  -- records no revisions, so it keeps no text table (refs/item_blocks.go's table map) and its
  -- block is ranked on the excerpt its row holds, the block's head as refs.BlockExcerpt cuts it,
  -- which an edit of the message rewrites with the block it changed.
  left join lateral (
    select covered.block_id
      from unnest(r.covers) with ordinality as covered(block_id, ord)
      cross join lateral (
        select case r.kind
          when 'document' then (select x.text from document_block_texts x
                                 where x.artifact_id = r.artifact_id and x.block_id = covered.block_id
                                 order by x.version desc limit 1)
          when 'comment' then (select x.text from comment_block_texts x
                                 where x.comment_id = r.id::uuid and x.block_id = covered.block_id
                                 order by x.revision desc limit 1)
          when 'ask' then (select x.text from ask_block_texts x
                                 where x.ask_id = r.id::uuid and x.block_id = covered.block_id
                                 order by x.revision desc limit 1)
          when 'message' then (select x.excerpt from message_blocks x
                                 where x.message_id = r.id::uuid and x.block_id = covered.block_id)
        end as text
      ) block
     where /* visibility predicate: every kind */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = case r.kind when 'document' then 'block' else r.kind || '_block' end and visible_node.id = case r.kind when 'document' then ar.ref_key else r.kind || '/' || r.id end || '#' || covered.block_id), true)
     order by ts_rank_cd(to_tsvector('english', search_text(coalesce(block.text, ''))), q.tsq) desc, covered.ord
     limit 1
  ) hit on true
 order by r.score desc, case when r.own_lead then r.id end, r.updated_at desc, r.id
`

// mainAt4975SearchMeaningOpener is main's searchQOpener(12), the q search's three meaning
// statements open with.
const mainAt4975SearchMeaningOpener = `
with q as (select
         case when $12 = '' then websearch_to_tsquery('english', $1)
              when $13 = '' then websearch_to_tsquery('english', $12)
              else websearch_to_tsquery('english', $12) && websearch_to_tsquery('english', $13)
         end as tsq,
         $4::text as term, $14::text[] as own_keys,
         $15::text as status_filter, $16::text as author_filter, $17::timestamptz as updated_since),
`

// mainAt4975MeaningDocumentCandidates, mainAt4975MeaningDocumentCandidatesScoped and
// mainAt4975MeaningDocumentCandidatesJoined are main's meaning_document_candidates CTE in search's
// three meaning statements (every project, the row's own project_key, and the project read off
// the document's own row), with the acting credential at $18: the nearest document windows,
// each joined to its document, and the document's date read from its latest version through a
// lateral over artifact_versions. Each is the CTE from its name to its closing parenthesis, the
// text the statement holds between `meaning_issue`'s `),` and `meaning_document`.
const (
	mainAt4975MeaningDocumentCandidates = `meaning_document_candidates as (
  select a.issue_key, a.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join artifacts a on a.id = (case when split_part(e.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(e.id, '#', 1))::uuid end)
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where e.kind = 'document_block' and a.kind = 'doc' and e.embedding is not null and e.model = $11
     and /* visibility predicate: artifact */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'artifact' and visible_node.id = a.ref_key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
)`
	mainAt4975MeaningDocumentCandidatesScoped = `meaning_document_candidates as (
  select a.issue_key, a.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join artifacts a on a.id = (case when split_part(e.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(e.id, '#', 1))::uuid end)
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where e.kind = 'document_block' and a.kind = 'doc' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: artifact */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'artifact' and visible_node.id = a.ref_key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
)`
	mainAt4975MeaningDocumentCandidatesJoined = `meaning_document_candidates as (
  select a.issue_key, a.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join artifacts a on a.id = (case when split_part(e.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(e.id, '#', 1))::uuid end)
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where e.kind = 'document_block' and a.kind = 'doc' and e.embedding is not null and e.model = $11 and ($2 = '' or p.key = $2)
     and /* visibility predicate: artifact */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'artifact' and visible_node.id = a.ref_key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
)`
)

// mainAt4975SearchMeaningArms, mainAt4975SearchMeaningArmsScoped and
// mainAt4975SearchMeaningArmsJoined are the five meaning arms of searchQueryMeaningBody as main
// built them for search, in each of the three project scopes, with the acting credential at
// $18: each kind's candidate CTE and the arm over it, from meaning_issue_candidates through
// meaning_message. The document arm's candidates are the constants above. The joined scope's
// comment arm is the one whose joins stand below its candidate limit
// (searchMeaningCommentArmJoined).
const (
	mainAt4975SearchMeaningArms = `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id, q
   where e.kind = 'issue' and e.embedding is not null and e.model = $11
     and /* visibility predicate: issue */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'issue' and visible_node.id = i.key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
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
` + mainAt4975MeaningDocumentCandidates + `,
meaning_document as (
  select distinct on (meaning_document_candidates.id) 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
   order by meaning_document_candidates.id, r desc, window_id
),
meaning_comment_candidates as (
  select e.id as window_id, e.covers, (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end) as comment_id,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
   where e.kind = 'comment_block' and e.embedding is not null and e.model = $11
     and /* visibility predicate: comment */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'comment' and visible_node.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)::text), true)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_comment as (
  select distinct on (c.id) 'comment' as kind, 'meaning' as list, c.issue_key, c.artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid), c.id::text, null::text,
         meaning_comment_candidates.window_id, meaning_comment_candidates.covers,
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, meaning_comment_candidates.r
    from meaning_comment_candidates
    join comments c on c.id = meaning_comment_candidates.comment_id
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where meaning_comment_candidates.r >= $10
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(c.edited_at, c.created_at) >= q.updated_since)
   order by c.id, meaning_comment_candidates.r desc, meaning_comment_candidates.window_id
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id, e.id as window_id, e.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where e.kind = 'ask_part' and e.embedding is not null and e.model = $11
     and /* visibility predicate: ask */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'ask' and visible_node.id = k.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(k.edited_at, k.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_ask as (
  select distinct on (meaning_ask_candidates.id) 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (case when pg_input_is_valid(anchor->>'artifact_id', 'uuid') then (anchor->>'artifact_id')::uuid end)), id::text, block_id,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
   order by meaning_ask_candidates.id, r desc, window_id
),
meaning_message_candidates as (
  select m.issue_key, m.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join messages m on m.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    join issues i on i.key = m.issue_key, q
   where e.kind = 'message_block' and e.embedding is not null and e.model = $11
     and /* visibility predicate: message */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'message' and visible_node.id = m.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(m.edited_at, m.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_message as (
  select distinct on (meaning_message_candidates.id) 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
   order by meaning_message_candidates.id, r desc, window_id
),
`
	mainAt4975SearchMeaningArmsScoped = `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id, q
   where e.kind = 'issue' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: issue */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'issue' and visible_node.id = i.key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
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
` + mainAt4975MeaningDocumentCandidatesScoped + `,
meaning_document as (
  select distinct on (meaning_document_candidates.id) 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
   order by meaning_document_candidates.id, r desc, window_id
),
meaning_comment_candidates as (
  select e.id as window_id, e.covers, (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end) as comment_id,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
   where e.kind = 'comment_block' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: comment */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'comment' and visible_node.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)::text), true)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_comment as (
  select distinct on (c.id) 'comment' as kind, 'meaning' as list, c.issue_key, c.artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid), c.id::text, null::text,
         meaning_comment_candidates.window_id, meaning_comment_candidates.covers,
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, meaning_comment_candidates.r
    from meaning_comment_candidates
    join comments c on c.id = meaning_comment_candidates.comment_id
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where meaning_comment_candidates.r >= $10
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(c.edited_at, c.created_at) >= q.updated_since)
   order by c.id, meaning_comment_candidates.r desc, meaning_comment_candidates.window_id
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id, e.id as window_id, e.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where e.kind = 'ask_part' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: ask */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'ask' and visible_node.id = k.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(k.edited_at, k.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_ask as (
  select distinct on (meaning_ask_candidates.id) 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (case when pg_input_is_valid(anchor->>'artifact_id', 'uuid') then (anchor->>'artifact_id')::uuid end)), id::text, block_id,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
   order by meaning_ask_candidates.id, r desc, window_id
),
meaning_message_candidates as (
  select m.issue_key, m.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join messages m on m.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    join issues i on i.key = m.issue_key, q
   where e.kind = 'message_block' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: message */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'message' and visible_node.id = m.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(m.edited_at, m.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_message as (
  select distinct on (meaning_message_candidates.id) 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
   order by meaning_message_candidates.id, r desc, window_id
),
`
	mainAt4975SearchMeaningArmsJoined = `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id, q
   where e.kind = 'issue' and e.embedding is not null and e.model = $11 and ($2 = '' or i.project_key = $2)
     and /* visibility predicate: issue */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'issue' and visible_node.id = i.key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
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
` + mainAt4975MeaningDocumentCandidatesJoined + `,
meaning_document as (
  select distinct on (meaning_document_candidates.id) 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
   order by meaning_document_candidates.id, r desc, window_id
),
meaning_comment_candidates as (
  select c.issue_key, c.artifact_id, c.anchor, c.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, c.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join comments c on c.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where e.kind = 'comment_block' and e.embedding is not null and e.model = $11 and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and /* visibility predicate: comment */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'comment' and visible_node.id = c.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(c.edited_at, c.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_comment as (
  select distinct on (meaning_comment_candidates.id) 'comment' as kind, 'meaning' as list, issue_key, artifact_id, coalesce(artifact_id, (anchor->>'artifact_id')::uuid), id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_comment_candidates where r >= $10
   order by meaning_comment_candidates.id, r desc, window_id
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id, e.id as window_id, e.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where e.kind = 'ask_part' and e.embedding is not null and e.model = $11 and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and /* visibility predicate: ask */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'ask' and visible_node.id = k.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(k.edited_at, k.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_ask as (
  select distinct on (meaning_ask_candidates.id) 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (case when pg_input_is_valid(anchor->>'artifact_id', 'uuid') then (anchor->>'artifact_id')::uuid end)), id::text, block_id,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
   order by meaning_ask_candidates.id, r desc, window_id
),
meaning_message_candidates as (
  select m.issue_key, m.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join messages m on m.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    join issues i on i.key = m.issue_key, q
   where e.kind = 'message_block' and e.embedding is not null and e.model = $11 and ($2 = '' or i.project_key = $2)
     and /* visibility predicate: message */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'message' and visible_node.id = m.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(m.edited_at, m.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_message as (
  select distinct on (meaning_message_candidates.id) 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
   order by meaning_message_candidates.id, r desc, window_id
),
`
)

// mainAt4975SearchMeaningTail is the rest of searchQueryMeaningBody as main built it for search,
// the same in all three scopes: kinds over mainAt4975SearchKeywordLegs and the five meaning arms,
// the ranking, searchFusedSQL's fusion, legs_unique, the totals and the page, with the acting
// credential at $18.
const mainAt4975SearchMeaningTail = `kinds as (select * from (` + mainAt4975SearchKeywordLegs + `
  union all select * from meaning_issue
  union all select * from meaning_document
  union all select * from meaning_comment
  union all select * from meaning_ask
  union all select * from meaning_message
) arms where /* visibility predicate: every kind */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = case arms.kind when 'document' then 'artifact' else arms.kind end and visible_node.id = case case arms.kind when 'document' then 'artifact' else arms.kind end when 'artifact' then (select store_artifact.ref_key from artifacts store_artifact where store_artifact.id::text = arms.id::text) else arms.id::text end), true)
),
ranked as (
  select *, row_number() over (partition by kind, list order by own desc, r desc, updated_at desc, id) as pos
    from kinds
),
legs as (
  select * from ranked where pos <= $7
),
fused as (
  select kind, id, bool_or(own) as own_lead,
         sum(case list when 'keyword' then 1 * r else 0.4 * r end) / ($8 + 1)
           + case when bool_or(own) then 1 else 0 end as score
    from legs group by kind, id
),
legs_unique as (
  select distinct on (kind, id) kind, issue_key, owner_artifact_id, artifact_id, id, block_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at
    from legs
   order by kind, id, (window_id is null), r desc
),
totals as (
  select (select count(*) from (select distinct kind, id from kinds) u) as total,
         (select count(*) from fused) as reachable
),
page as (
  select l.*, f.score, f.own_lead
    from legs_unique l join fused f using (kind, id)
   order by f.score desc, case when f.own_lead then l.id end, l.updated_at desc, l.id
   limit $3 offset $6
)
select t.total, t.reachable, r.kind, case when r.owner_artifact_id is null then 'issue' else 'document' end,
       r.issue_key, r.issue_title, r.issue_status,
       r.owner_project, r.owner_slug, r.owner_artifact_id::text, r.owner_name,
       ar.slug, ar.name, coalesce(ar.is_primary, false), r.id, r.block_id, hit.block_id, r.score::float8,
` + searchOracleCutColumns + `
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
    offset 0
  ) txt on true
` + searchOracleCutLaterals + `
  -- The block of r.covers that matched, if the row carries a window. Each covered block's own
  -- text comes from the table its indexer writes it to: a document's, a comment's and an ask's
  -- block texts are kept per version or revision, the latest being the current text. A message
  -- records no revisions, so it keeps no text table (refs/item_blocks.go's table map) and its
  -- block is ranked on the excerpt its row holds, the block's head as refs.BlockExcerpt cuts it,
  -- which an edit of the message rewrites with the block it changed.
  left join lateral (
    select covered.block_id
      from unnest(r.covers) with ordinality as covered(block_id, ord)
      cross join lateral (
        select case r.kind
          when 'document' then (select x.text from document_block_texts x
                                 where x.artifact_id = r.artifact_id and x.block_id = covered.block_id
                                 order by x.version desc limit 1)
          when 'comment' then (select x.text from comment_block_texts x
                                 where x.comment_id = r.id::uuid and x.block_id = covered.block_id
                                 order by x.revision desc limit 1)
          when 'ask' then (select x.text from ask_block_texts x
                                 where x.ask_id = r.id::uuid and x.block_id = covered.block_id
                                 order by x.revision desc limit 1)
          when 'message' then (select x.excerpt from message_blocks x
                                 where x.message_id = r.id::uuid and x.block_id = covered.block_id)
        end as text
      ) block
     where /* visibility predicate: every kind */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($18::jsonb)->>'kind', 'id', case when ($18::jsonb)->>'kind' = 'person' then user_threads_login(($18::jsonb)->>'id') else ($18::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = case r.kind when 'document' then 'block' else r.kind || '_block' end and visible_node.id = case r.kind when 'document' then ar.ref_key else r.kind || '/' || r.id end || '#' || covered.block_id), true)
     order by ts_rank_cd(to_tsvector('english', search_text(coalesce(block.text, ''))), q.tsq) desc, covered.ord
     limit 1
  ) hit on true
 order by r.score desc, case when r.own_lead then r.id end, r.updated_at desc, r.id
`

// mainAt4975SearchQueryMeaning, mainAt4975SearchQueryMeaningScoped and
// mainAt4975SearchQueryMeaningJoined are main's searchQueryMeaning, searchQueryMeaningScoped and
// searchQueryMeaningJoined. Each takes the eighteen parameters searchMeaningQueryArguments binds.
const (
	mainAt4975SearchQueryMeaning       = mainAt4975SearchMeaningOpener + mainAt4975SearchMeaningArms + mainAt4975SearchMeaningTail
	mainAt4975SearchQueryMeaningScoped = mainAt4975SearchMeaningOpener + mainAt4975SearchMeaningArmsScoped + mainAt4975SearchMeaningTail
	mainAt4975SearchQueryMeaningJoined = mainAt4975SearchMeaningOpener + mainAt4975SearchMeaningArmsJoined + mainAt4975SearchMeaningTail
)

// mainAt4975KindNames are the five kinds a search can name, by the bit that leaves each out of a
// set, bit 0 first, spelled as main's builder wrote them. They are written out here and not read
// from searchIndexedKinds.
var mainAt4975KindNames = [...]string{"issue", "document", "comment", "ask", "message"}

// mainAt4975KindsFilter is where main's statements filter the kinds CTE's union: the line that
// closes the derived table and opens its `where`, which each of search's four statements holds
// once. A statement of a set that leaves kinds out holds the exclusion right after it
// (mainAt4975Statement).
const mainAt4975KindsFilter = "\n) arms where "

// mainAt4975Statement is main's text of one of search's four statements for the set of kinds
// whose bits are set, the kinds the search leaves out, one bit for each of mainAt4975KindNames,
// bit 0 first: base is main's text of the same statement leaving nothing out
// (mainAt4975SearchQuery or one of the three mainAt4975SearchQueryMeaning texts). Main builds a
// set that leaves kinds out as base with `not (kind in ('issue', 'comment')) and `, the left-out
// kinds in bit order, written between mainAt4975KindsFilter and the visibility predicate, and
// no other byte of it differs: this is the difference between main's texts of set 0 and of every
// other set at 497581c7. The set that leaves nothing out is base itself, and the set with every
// bit set leaves out all five kinds.
func mainAt4975Statement(base string, set uint8) string {
	if set >= 1<<len(mainAt4975KindNames) {
		panic(fmt.Sprintf("mainAt4975Statement: set %d has a bit past the five kinds", set))
	}
	if set == 0 {
		return base
	}
	if count := strings.Count(base, mainAt4975KindsFilter); count != 1 {
		panic(fmt.Sprintf("mainAt4975Statement: the statement holds %q %d times; want once", mainAt4975KindsFilter, count))
	}
	var left []string
	for bit, kind := range mainAt4975KindNames {
		if set&(1<<bit) != 0 {
			left = append(left, "'"+kind+"'")
		}
	}
	return strings.Replace(base, mainAt4975KindsFilter,
		mainAt4975KindsFilter+"not (kind in ("+strings.Join(left, ", ")+")) and ", 1)
}

// mainAt4975Statements is main's four statements for one set of kinds (mainAt4975Statement).
func mainAt4975Statements(set uint8) *searchStatements {
	return &searchStatements{
		keyword:       mainAt4975Statement(mainAt4975SearchQuery, set),
		meaningEvery:  mainAt4975Statement(mainAt4975SearchQueryMeaning, set),
		meaningScoped: mainAt4975Statement(mainAt4975SearchQueryMeaningScoped, set),
		meaningJoined: mainAt4975Statement(mainAt4975SearchQueryMeaningJoined, set),
	}
}

// mainAt4975RelatedMeaningOpener is main's relatedQOpener followed by relatedTermWeights(12), the
// opening the related-item check's six meaning statements share: q over a tsquery already built,
// and the chosen lexemes' idf at $12 and $13.
const mainAt4975RelatedMeaningOpener = `
with q as (select $1::tsquery as tsq, $4::text as term, '{}'::text[] as own_keys,
                  ''::text as status_filter, ''::text as author_filter, null::timestamptz as updated_since),
term_weights as (
  select quote_literal(w.lexeme)::tsquery as tq, w.idf from unnest($12::text[], $13::float8[]) w(lexeme, idf)
),
`

// mainAt4975RelatedKeywordLegs is main's relatedKeywordLegs: mainAt4975SearchKeywordLegs' arms,
// each ranked by relatedCoordinationRank, the sum of the idf of the chosen lexemes the row holds
// plus a thousandth of its normalised ts_rank_cd.
const mainAt4975RelatedKeywordLegs = `
  select 'issue' as kind, 'keyword' as list, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
         null::text as window_id, null::text[] as covers,
         i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
         false as own, ((select sum(w.idf) from term_weights w where i.search @@ w.tq) + 0.001 * ts_rank_cd(i.search, q.tsq, 32)) as r
    from issues i, q
   where i.search @@ q.tsq and not (i.key = any(q.own_keys)) and ($2 = '' or i.project_key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
  union all
  select 'issue', 'keyword', i.key, null::uuid, null::uuid, i.key, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         true, ((select sum(w.idf) from term_weights w where i.search @@ w.tq) + 0.001 * ts_rank_cd(i.search, q.tsq, 32))
    from q, unnest(q.own_keys) as named(key), issues i
   where i.key = named.key and ($2 = '' or i.project_key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
  union all
  select 'document' as kind, 'keyword', a.issue_key, case when a.issue_key is null then a.id else null::uuid end, a.id, a.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, v.created_at),
         false, ((select sum(w.idf) from term_weights w where v.search @@ w.tq) + 0.001 * ts_rank_cd(v.search, q.tsq, 32))
    from document_search v
    join artifacts a on a.id = v.artifact_id
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or p.key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
  union all
  select 'comment' as kind, 'keyword', c.issue_key, c.artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid), c.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, ((select sum(w.idf) from term_weights w where c.search @@ w.tq) + 0.001 * ts_rank_cd(c.search, q.tsq, 32))
    from comments c
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where c.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(c.edited_at, c.created_at) >= q.updated_since)
  union all
  select 'ask' as kind, 'keyword', k.issue_key, case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) else null::uuid end,
         coalesce(k.artifact_id, k.block_artifact_id, (case when pg_input_is_valid(k.anchor->>'artifact_id', 'uuid') then (k.anchor->>'artifact_id')::uuid end)), k.id::text, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name, coalesce(i.updated_at, k.created_at),
         false, ((select sum(w.idf) from term_weights w where k.search @@ w.tq) + 0.001 * ts_rank_cd(k.search, q.tsq, 32))
    from asks k
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where k.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(k.edited_at, k.created_at) >= q.updated_since)
  union all
  select 'message' as kind, 'keyword', m.issue_key, null::uuid, null::uuid, m.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         false, ((select sum(w.idf) from term_weights w where m.search @@ w.tq) + 0.001 * ts_rank_cd(m.search, q.tsq, 32))
    from messages m join issues i on i.key = m.issue_key, q
   where m.search @@ q.tsq and ($2 = '' or i.project_key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(m.edited_at, m.created_at) >= q.updated_since)`

// mainAt4975RelatedMeaningArms, mainAt4975RelatedMeaningArmsScoped and
// mainAt4975RelatedMeaningArmsJoined are mainAt4975SearchMeaningArms and its two scopes with the
// acting credential at $14, where the related-item check's plain meaning statements bind it.
const (
	mainAt4975RelatedMeaningArms = `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id, q
   where e.kind = 'issue' and e.embedding is not null and e.model = $11
     and /* visibility predicate: issue */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'issue' and visible_node.id = i.key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
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
  select a.issue_key, a.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join artifacts a on a.id = (case when split_part(e.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(e.id, '#', 1))::uuid end)
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where e.kind = 'document_block' and a.kind = 'doc' and e.embedding is not null and e.model = $11
     and /* visibility predicate: artifact */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'artifact' and visible_node.id = a.ref_key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_document as (
  select distinct on (meaning_document_candidates.id) 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
   order by meaning_document_candidates.id, r desc, window_id
),
meaning_comment_candidates as (
  select e.id as window_id, e.covers, (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end) as comment_id,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
   where e.kind = 'comment_block' and e.embedding is not null and e.model = $11
     and /* visibility predicate: comment */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'comment' and visible_node.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)::text), true)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_comment as (
  select distinct on (c.id) 'comment' as kind, 'meaning' as list, c.issue_key, c.artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid), c.id::text, null::text,
         meaning_comment_candidates.window_id, meaning_comment_candidates.covers,
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, meaning_comment_candidates.r
    from meaning_comment_candidates
    join comments c on c.id = meaning_comment_candidates.comment_id
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where meaning_comment_candidates.r >= $10
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(c.edited_at, c.created_at) >= q.updated_since)
   order by c.id, meaning_comment_candidates.r desc, meaning_comment_candidates.window_id
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id, e.id as window_id, e.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where e.kind = 'ask_part' and e.embedding is not null and e.model = $11
     and /* visibility predicate: ask */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'ask' and visible_node.id = k.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(k.edited_at, k.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_ask as (
  select distinct on (meaning_ask_candidates.id) 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (case when pg_input_is_valid(anchor->>'artifact_id', 'uuid') then (anchor->>'artifact_id')::uuid end)), id::text, block_id,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
   order by meaning_ask_candidates.id, r desc, window_id
),
meaning_message_candidates as (
  select m.issue_key, m.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join messages m on m.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    join issues i on i.key = m.issue_key, q
   where e.kind = 'message_block' and e.embedding is not null and e.model = $11
     and /* visibility predicate: message */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'message' and visible_node.id = m.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(m.edited_at, m.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_message as (
  select distinct on (meaning_message_candidates.id) 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
   order by meaning_message_candidates.id, r desc, window_id
),
`
	mainAt4975RelatedMeaningArmsScoped = `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id, q
   where e.kind = 'issue' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: issue */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'issue' and visible_node.id = i.key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
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
  select a.issue_key, a.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join artifacts a on a.id = (case when split_part(e.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(e.id, '#', 1))::uuid end)
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where e.kind = 'document_block' and a.kind = 'doc' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: artifact */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'artifact' and visible_node.id = a.ref_key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_document as (
  select distinct on (meaning_document_candidates.id) 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
   order by meaning_document_candidates.id, r desc, window_id
),
meaning_comment_candidates as (
  select e.id as window_id, e.covers, (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end) as comment_id,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
   where e.kind = 'comment_block' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: comment */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'comment' and visible_node.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)::text), true)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_comment as (
  select distinct on (c.id) 'comment' as kind, 'meaning' as list, c.issue_key, c.artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid), c.id::text, null::text,
         meaning_comment_candidates.window_id, meaning_comment_candidates.covers,
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, meaning_comment_candidates.r
    from meaning_comment_candidates
    join comments c on c.id = meaning_comment_candidates.comment_id
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where meaning_comment_candidates.r >= $10
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(c.edited_at, c.created_at) >= q.updated_since)
   order by c.id, meaning_comment_candidates.r desc, meaning_comment_candidates.window_id
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id, e.id as window_id, e.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where e.kind = 'ask_part' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: ask */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'ask' and visible_node.id = k.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(k.edited_at, k.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_ask as (
  select distinct on (meaning_ask_candidates.id) 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (case when pg_input_is_valid(anchor->>'artifact_id', 'uuid') then (anchor->>'artifact_id')::uuid end)), id::text, block_id,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
   order by meaning_ask_candidates.id, r desc, window_id
),
meaning_message_candidates as (
  select m.issue_key, m.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join messages m on m.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    join issues i on i.key = m.issue_key, q
   where e.kind = 'message_block' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: message */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'message' and visible_node.id = m.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(m.edited_at, m.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_message as (
  select distinct on (meaning_message_candidates.id) 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
   order by meaning_message_candidates.id, r desc, window_id
),
`
	mainAt4975RelatedMeaningArmsJoined = `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id, q
   where e.kind = 'issue' and e.embedding is not null and e.model = $11 and ($2 = '' or i.project_key = $2)
     and /* visibility predicate: issue */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'issue' and visible_node.id = i.key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
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
  select a.issue_key, a.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join artifacts a on a.id = (case when split_part(e.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(e.id, '#', 1))::uuid end)
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where e.kind = 'document_block' and a.kind = 'doc' and e.embedding is not null and e.model = $11 and ($2 = '' or p.key = $2)
     and /* visibility predicate: artifact */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'artifact' and visible_node.id = a.ref_key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_document as (
  select distinct on (meaning_document_candidates.id) 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
   order by meaning_document_candidates.id, r desc, window_id
),
meaning_comment_candidates as (
  select c.issue_key, c.artifact_id, c.anchor, c.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, c.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join comments c on c.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where e.kind = 'comment_block' and e.embedding is not null and e.model = $11 and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and /* visibility predicate: comment */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'comment' and visible_node.id = c.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(c.edited_at, c.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_comment as (
  select distinct on (meaning_comment_candidates.id) 'comment' as kind, 'meaning' as list, issue_key, artifact_id, coalesce(artifact_id, (anchor->>'artifact_id')::uuid), id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_comment_candidates where r >= $10
   order by meaning_comment_candidates.id, r desc, window_id
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id, e.id as window_id, e.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where e.kind = 'ask_part' and e.embedding is not null and e.model = $11 and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and /* visibility predicate: ask */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'ask' and visible_node.id = k.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(k.edited_at, k.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_ask as (
  select distinct on (meaning_ask_candidates.id) 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (case when pg_input_is_valid(anchor->>'artifact_id', 'uuid') then (anchor->>'artifact_id')::uuid end)), id::text, block_id,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
   order by meaning_ask_candidates.id, r desc, window_id
),
meaning_message_candidates as (
  select m.issue_key, m.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join messages m on m.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    join issues i on i.key = m.issue_key, q
   where e.kind = 'message_block' and e.embedding is not null and e.model = $11 and ($2 = '' or i.project_key = $2)
     and /* visibility predicate: message */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'message' and visible_node.id = m.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(m.edited_at, m.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_message as (
  select distinct on (meaning_message_candidates.id) 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
   order by meaning_message_candidates.id, r desc, window_id
),
`
)

// mainAt4975RelatedMeaningTail is mainAt4975SearchMeaningTail as the related-item check's plain
// meaning statements end: over mainAt4975RelatedKeywordLegs, fused by relatedFusedSQL's
// reciprocal rank fusion, with the acting credential at $14.
const mainAt4975RelatedMeaningTail = `kinds as (select * from (` + mainAt4975RelatedKeywordLegs + `
  union all select * from meaning_issue
  union all select * from meaning_document
  union all select * from meaning_comment
  union all select * from meaning_ask
  union all select * from meaning_message
) arms where /* visibility predicate: every kind */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = case arms.kind when 'document' then 'artifact' else arms.kind end and visible_node.id = case case arms.kind when 'document' then 'artifact' else arms.kind end when 'artifact' then (select store_artifact.ref_key from artifacts store_artifact where store_artifact.id::text = arms.id::text) else arms.id::text end), true)
),
ranked as (
  select *, row_number() over (partition by kind, list order by own desc, r desc, updated_at desc, id) as pos
    from kinds
),
legs as (
  select * from ranked where pos <= $7
),
fused as (
  select kind, id, bool_or(own) as own_lead, sum(1.0 / ($8 + pos)) as score
    from legs group by kind, id
),
legs_unique as (
  select distinct on (kind, id) kind, issue_key, owner_artifact_id, artifact_id, id, block_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at
    from legs
   order by kind, id, (window_id is null), r desc
),
totals as (
  select (select count(*) from (select distinct kind, id from kinds) u) as total,
         (select count(*) from fused) as reachable
),
page as (
  select l.*, f.score, f.own_lead
    from legs_unique l join fused f using (kind, id)
   order by f.score desc, case when f.own_lead then l.id end, l.updated_at desc, l.id
   limit $3 offset $6
)
select t.total, t.reachable, r.kind, case when r.owner_artifact_id is null then 'issue' else 'document' end,
       r.issue_key, r.issue_title, r.issue_status,
       r.owner_project, r.owner_slug, r.owner_artifact_id::text, r.owner_name,
       ar.slug, ar.name, coalesce(ar.is_primary, false), r.id, r.block_id, hit.block_id, r.score::float8,
` + searchOracleCutColumns + `
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
    offset 0
  ) txt on true
` + searchOracleCutLaterals + `
  -- The block of r.covers that matched, if the row carries a window. Each covered block's own
  -- text comes from the table its indexer writes it to: a document's, a comment's and an ask's
  -- block texts are kept per version or revision, the latest being the current text. A message
  -- records no revisions, so it keeps no text table (refs/item_blocks.go's table map) and its
  -- block is ranked on the excerpt its row holds, the block's head as refs.BlockExcerpt cuts it,
  -- which an edit of the message rewrites with the block it changed.
  left join lateral (
    select covered.block_id
      from unnest(r.covers) with ordinality as covered(block_id, ord)
      cross join lateral (
        select case r.kind
          when 'document' then (select x.text from document_block_texts x
                                 where x.artifact_id = r.artifact_id and x.block_id = covered.block_id
                                 order by x.version desc limit 1)
          when 'comment' then (select x.text from comment_block_texts x
                                 where x.comment_id = r.id::uuid and x.block_id = covered.block_id
                                 order by x.revision desc limit 1)
          when 'ask' then (select x.text from ask_block_texts x
                                 where x.ask_id = r.id::uuid and x.block_id = covered.block_id
                                 order by x.revision desc limit 1)
          when 'message' then (select x.excerpt from message_blocks x
                                 where x.message_id = r.id::uuid and x.block_id = covered.block_id)
        end as text
      ) block
     where /* visibility predicate: every kind */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($14::jsonb)->>'kind', 'id', case when ($14::jsonb)->>'kind' = 'person' then user_threads_login(($14::jsonb)->>'id') else ($14::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = case r.kind when 'document' then 'block' else r.kind || '_block' end and visible_node.id = case r.kind when 'document' then ar.ref_key else r.kind || '/' || r.id end || '#' || covered.block_id), true)
     order by ts_rank_cd(to_tsvector('english', search_text(coalesce(block.text, ''))), q.tsq) desc, covered.ord
     limit 1
  ) hit on true
 order by r.score desc, case when r.own_lead then r.id end, r.updated_at desc, r.id
`

// mainAt4975RelatedMeaningQuery, mainAt4975RelatedMeaningScopedQuery and
// mainAt4975RelatedMeaningJoinedQuery are main's relatedMeaningQuery, relatedMeaningScopedQuery
// and relatedMeaningJoinedQuery: the related-item check's meaning stage for a check that leaves
// nothing out, over the fourteen parameters relatedSearch binds it.
const (
	mainAt4975RelatedMeaningQuery       = mainAt4975RelatedMeaningOpener + mainAt4975RelatedMeaningArms + mainAt4975RelatedMeaningTail
	mainAt4975RelatedMeaningScopedQuery = mainAt4975RelatedMeaningOpener + mainAt4975RelatedMeaningArmsScoped + mainAt4975RelatedMeaningTail
	mainAt4975RelatedMeaningJoinedQuery = mainAt4975RelatedMeaningOpener + mainAt4975RelatedMeaningArmsJoined + mainAt4975RelatedMeaningTail
)

// mainAt4975RelatedMeaningExcludingArms, mainAt4975RelatedMeaningExcludingArmsScoped and
// mainAt4975RelatedMeaningExcludingArmsJoined are mainAt4975SearchMeaningArms and its two scopes
// with the acting credential at $15, where the related-item check's excluding meaning statements
// bind it, the owners they leave out being $14.
const (
	mainAt4975RelatedMeaningExcludingArms = `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id, q
   where e.kind = 'issue' and e.embedding is not null and e.model = $11
     and /* visibility predicate: issue */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'issue' and visible_node.id = i.key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
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
  select a.issue_key, a.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join artifacts a on a.id = (case when split_part(e.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(e.id, '#', 1))::uuid end)
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where e.kind = 'document_block' and a.kind = 'doc' and e.embedding is not null and e.model = $11
     and /* visibility predicate: artifact */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'artifact' and visible_node.id = a.ref_key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_document as (
  select distinct on (meaning_document_candidates.id) 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
   order by meaning_document_candidates.id, r desc, window_id
),
meaning_comment_candidates as (
  select e.id as window_id, e.covers, (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end) as comment_id,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
   where e.kind = 'comment_block' and e.embedding is not null and e.model = $11
     and /* visibility predicate: comment */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'comment' and visible_node.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)::text), true)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_comment as (
  select distinct on (c.id) 'comment' as kind, 'meaning' as list, c.issue_key, c.artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid), c.id::text, null::text,
         meaning_comment_candidates.window_id, meaning_comment_candidates.covers,
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, meaning_comment_candidates.r
    from meaning_comment_candidates
    join comments c on c.id = meaning_comment_candidates.comment_id
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where meaning_comment_candidates.r >= $10
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(c.edited_at, c.created_at) >= q.updated_since)
   order by c.id, meaning_comment_candidates.r desc, meaning_comment_candidates.window_id
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id, e.id as window_id, e.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where e.kind = 'ask_part' and e.embedding is not null and e.model = $11
     and /* visibility predicate: ask */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'ask' and visible_node.id = k.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(k.edited_at, k.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_ask as (
  select distinct on (meaning_ask_candidates.id) 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (case when pg_input_is_valid(anchor->>'artifact_id', 'uuid') then (anchor->>'artifact_id')::uuid end)), id::text, block_id,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
   order by meaning_ask_candidates.id, r desc, window_id
),
meaning_message_candidates as (
  select m.issue_key, m.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join messages m on m.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    join issues i on i.key = m.issue_key, q
   where e.kind = 'message_block' and e.embedding is not null and e.model = $11
     and /* visibility predicate: message */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'message' and visible_node.id = m.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(m.edited_at, m.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_message as (
  select distinct on (meaning_message_candidates.id) 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
   order by meaning_message_candidates.id, r desc, window_id
),
`
	mainAt4975RelatedMeaningExcludingArmsScoped = `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id, q
   where e.kind = 'issue' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: issue */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'issue' and visible_node.id = i.key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
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
  select a.issue_key, a.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join artifacts a on a.id = (case when split_part(e.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(e.id, '#', 1))::uuid end)
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where e.kind = 'document_block' and a.kind = 'doc' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: artifact */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'artifact' and visible_node.id = a.ref_key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_document as (
  select distinct on (meaning_document_candidates.id) 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
   order by meaning_document_candidates.id, r desc, window_id
),
meaning_comment_candidates as (
  select e.id as window_id, e.covers, (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end) as comment_id,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
   where e.kind = 'comment_block' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: comment */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'comment' and visible_node.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)::text), true)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_comment as (
  select distinct on (c.id) 'comment' as kind, 'meaning' as list, c.issue_key, c.artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid), c.id::text, null::text,
         meaning_comment_candidates.window_id, meaning_comment_candidates.covers,
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, meaning_comment_candidates.r
    from meaning_comment_candidates
    join comments c on c.id = meaning_comment_candidates.comment_id
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where meaning_comment_candidates.r >= $10
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(c.edited_at, c.created_at) >= q.updated_since)
   order by c.id, meaning_comment_candidates.r desc, meaning_comment_candidates.window_id
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id, e.id as window_id, e.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where e.kind = 'ask_part' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: ask */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'ask' and visible_node.id = k.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(k.edited_at, k.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_ask as (
  select distinct on (meaning_ask_candidates.id) 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (case when pg_input_is_valid(anchor->>'artifact_id', 'uuid') then (anchor->>'artifact_id')::uuid end)), id::text, block_id,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
   order by meaning_ask_candidates.id, r desc, window_id
),
meaning_message_candidates as (
  select m.issue_key, m.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join messages m on m.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    join issues i on i.key = m.issue_key, q
   where e.kind = 'message_block' and e.embedding is not null and e.model = $11 and e.project_key = $2
     and /* visibility predicate: message */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'message' and visible_node.id = m.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(m.edited_at, m.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_message as (
  select distinct on (meaning_message_candidates.id) 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
   order by meaning_message_candidates.id, r desc, window_id
),
`
	mainAt4975RelatedMeaningExcludingArmsJoined = `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id, q
   where e.kind = 'issue' and e.embedding is not null and e.model = $11 and ($2 = '' or i.project_key = $2)
     and /* visibility predicate: issue */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'issue' and visible_node.id = i.key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
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
  select a.issue_key, a.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join artifacts a on a.id = (case when split_part(e.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(e.id, '#', 1))::uuid end)
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where e.kind = 'document_block' and a.kind = 'doc' and e.embedding is not null and e.model = $11 and ($2 = '' or p.key = $2)
     and /* visibility predicate: artifact */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'artifact' and visible_node.id = a.ref_key), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_document as (
  select distinct on (meaning_document_candidates.id) 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
   order by meaning_document_candidates.id, r desc, window_id
),
meaning_comment_candidates as (
  select c.issue_key, c.artifact_id, c.anchor, c.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, c.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join comments c on c.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where e.kind = 'comment_block' and e.embedding is not null and e.model = $11 and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and /* visibility predicate: comment */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'comment' and visible_node.id = c.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(c.edited_at, c.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_comment as (
  select distinct on (meaning_comment_candidates.id) 'comment' as kind, 'meaning' as list, issue_key, artifact_id, coalesce(artifact_id, (anchor->>'artifact_id')::uuid), id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_comment_candidates where r >= $10
   order by meaning_comment_candidates.id, r desc, window_id
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id, e.id as window_id, e.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where e.kind = 'ask_part' and e.embedding is not null and e.model = $11 and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and /* visibility predicate: ask */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'ask' and visible_node.id = k.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(k.edited_at, k.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_ask as (
  select distinct on (meaning_ask_candidates.id) 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (case when pg_input_is_valid(anchor->>'artifact_id', 'uuid') then (anchor->>'artifact_id')::uuid end)), id::text, block_id,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
   order by meaning_ask_candidates.id, r desc, window_id
),
meaning_message_candidates as (
  select m.issue_key, m.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join messages m on m.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
    join issues i on i.key = m.issue_key, q
   where e.kind = 'message_block' and e.embedding is not null and e.model = $11 and ($2 = '' or i.project_key = $2)
     and /* visibility predicate: message */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'message' and visible_node.id = m.id::text), true)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(m.edited_at, m.created_at) >= q.updated_since)
   order by e.embedding <=> $9::vector
   limit 1000
),
meaning_message as (
  select distinct on (meaning_message_candidates.id) 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
   order by meaning_message_candidates.id, r desc, window_id
),
`
)

// mainAt4975RelatedMeaningExcludingTail is mainAt4975RelatedMeaningTail with relatedExcludedOwners'
// filter over the owners at $14 in kinds and the acting credential at $15.
const mainAt4975RelatedMeaningExcludingTail = `kinds as (select * from (` + mainAt4975RelatedKeywordLegs + `
  union all select * from meaning_issue
  union all select * from meaning_document
  union all select * from meaning_comment
  union all select * from meaning_ask
  union all select * from meaning_message
) arms where not (coalesce(case when owner_artifact_id is null then 'issue:' || issue_key
                     else 'document:' || owner_artifact_id end, '') = any($14::text[])
      or (kind = 'document' and 'document:' || id = any($14::text[]))
      or (kind = 'ask' and 'ask:' || id = any($14::text[]))) and /* visibility predicate: every kind */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = case arms.kind when 'document' then 'artifact' else arms.kind end and visible_node.id = case case arms.kind when 'document' then 'artifact' else arms.kind end when 'artifact' then (select store_artifact.ref_key from artifacts store_artifact where store_artifact.id::text = arms.id::text) else arms.id::text end), true)
),
ranked as (
  select *, row_number() over (partition by kind, list order by own desc, r desc, updated_at desc, id) as pos
    from kinds
),
legs as (
  select * from ranked where pos <= $7
),
fused as (
  select kind, id, bool_or(own) as own_lead, sum(1.0 / ($8 + pos)) as score
    from legs group by kind, id
),
legs_unique as (
  select distinct on (kind, id) kind, issue_key, owner_artifact_id, artifact_id, id, block_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at
    from legs
   order by kind, id, (window_id is null), r desc
),
totals as (
  select (select count(*) from (select distinct kind, id from kinds) u) as total,
         (select count(*) from fused) as reachable
),
page as (
  select l.*, f.score, f.own_lead
    from legs_unique l join fused f using (kind, id)
   order by f.score desc, case when f.own_lead then l.id end, l.updated_at desc, l.id
   limit $3 offset $6
)
select t.total, t.reachable, r.kind, case when r.owner_artifact_id is null then 'issue' else 'document' end,
       r.issue_key, r.issue_title, r.issue_status,
       r.owner_project, r.owner_slug, r.owner_artifact_id::text, r.owner_name,
       ar.slug, ar.name, coalesce(ar.is_primary, false), r.id, r.block_id, hit.block_id, r.score::float8,
` + searchOracleCutColumns + `
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
    offset 0
  ) txt on true
` + searchOracleCutLaterals + `
  -- The block of r.covers that matched, if the row carries a window. Each covered block's own
  -- text comes from the table its indexer writes it to: a document's, a comment's and an ask's
  -- block texts are kept per version or revision, the latest being the current text. A message
  -- records no revisions, so it keeps no text table (refs/item_blocks.go's table map) and its
  -- block is ranked on the excerpt its row holds, the block's head as refs.BlockExcerpt cuts it,
  -- which an edit of the message rewrites with the block it changed.
  left join lateral (
    select covered.block_id
      from unnest(r.covers) with ordinality as covered(block_id, ord)
      cross join lateral (
        select case r.kind
          when 'document' then (select x.text from document_block_texts x
                                 where x.artifact_id = r.artifact_id and x.block_id = covered.block_id
                                 order by x.version desc limit 1)
          when 'comment' then (select x.text from comment_block_texts x
                                 where x.comment_id = r.id::uuid and x.block_id = covered.block_id
                                 order by x.revision desc limit 1)
          when 'ask' then (select x.text from ask_block_texts x
                                 where x.ask_id = r.id::uuid and x.block_id = covered.block_id
                                 order by x.revision desc limit 1)
          when 'message' then (select x.excerpt from message_blocks x
                                 where x.message_id = r.id::uuid and x.block_id = covered.block_id)
        end as text
      ) block
     where /* visibility predicate: every kind */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ($15::jsonb)->>'kind', 'id', case when ($15::jsonb)->>'kind' = 'person' then user_threads_login(($15::jsonb)->>'id') else ($15::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = case r.kind when 'document' then 'block' else r.kind || '_block' end and visible_node.id = case r.kind when 'document' then ar.ref_key else r.kind || '/' || r.id end || '#' || covered.block_id), true)
     order by ts_rank_cd(to_tsvector('english', search_text(coalesce(block.text, ''))), q.tsq) desc, covered.ord
     limit 1
  ) hit on true
 order by r.score desc, case when r.own_lead then r.id end, r.updated_at desc, r.id
`

// mainAt4975RelatedMeaningExcludingQuery, mainAt4975RelatedMeaningScopedExcludingQuery and
// mainAt4975RelatedMeaningJoinedExcludingQuery are main's relatedMeaningExcludingQuery,
// relatedMeaningScopedExcludingQuery and relatedMeaningJoinedExcludingQuery: the meaning stage
// every write runs, over the fifteen parameters relatedSearch binds it.
const (
	mainAt4975RelatedMeaningExcludingQuery       = mainAt4975RelatedMeaningOpener + mainAt4975RelatedMeaningExcludingArms + mainAt4975RelatedMeaningExcludingTail
	mainAt4975RelatedMeaningScopedExcludingQuery = mainAt4975RelatedMeaningOpener + mainAt4975RelatedMeaningExcludingArmsScoped + mainAt4975RelatedMeaningExcludingTail
	mainAt4975RelatedMeaningJoinedExcludingQuery = mainAt4975RelatedMeaningOpener + mainAt4975RelatedMeaningExcludingArmsJoined + mainAt4975RelatedMeaningExcludingTail
)

// The pieces of main's statements at a cut, as main built them at commit 588af9ce: searchAsOfQuery
// and searchAsOfMeaningQuery, each for a search that ranks each post kind first (ranksFirst true,
// searchParameters.ranksFirstAtACut) and for one that reads each post kind's matches whole, over
// every kind. They are written out as the statements above are: every byte is in this file, refs'
// visibility predicate opens with mainAt4975VisibilityHead, which main renders the same at
// 588af9ce, and nothing here calls the package's builders. Each piece is one member of the WITH
// list, or a part of kinds (its issue and document arms, its post arms, the meaning arms' union
// and its visibility tail), named by the statements that share it: Keyword or Meaning for one of
// the two statements, RanksFirst or ReadWhole for one reading of the posts, and neither for a piece
// both share. Two values a statement takes are written as markers: the cut's literal as
// mainAt588aCutMarker, and the acting credential's parameter, $15 in the keyword statement and $18
// in the meaning one, as mainAt588aSeenMarker (mainAt588aAsOfStatement).
const (
	mainAt588aAsOfOpenerKeyword = `
with q as (select
         case when $9 = '' then websearch_to_tsquery('english', $1)
              when $10 = '' then websearch_to_tsquery('english', $9)
              else websearch_to_tsquery('english', $9) && websearch_to_tsquery('english', $10)
         end as tsq,
         $4::text as term, $11::text[] as own_keys,
         $12::text as status_filter, $13::text as author_filter, $14::timestamptz as updated_since),
`
	mainAt588aAsOfTextVersions = `text_versions as not materialized (select v.kind, v.id, v.issue_key, v.project_key, v.event_id, v.event_seq, v.started_at, v.body,
        v.spec_artifact_id, v.spec_version, v.search,
        case v.kind
          when 'comment' then (select c.deleted_at from public.comments c
                                where c.id = case when pg_input_is_valid(v.id, 'uuid') then v.id::uuid end)
          when 'message' then (select m.deleted_at from public.messages m
                                where m.id = case when pg_input_is_valid(v.id, 'uuid') then v.id::uuid end)
        end as deleted_at
   from public.search_text_versions v
  where v.started_at <= '{cut}'::timestamptz
    and not exists (select from public.search_text_versions l
                     where l.kind = v.kind and l.id = v.id and l.event_seq > v.event_seq
                       and l.started_at <= '{cut}'::timestamptz)),
`
	mainAt588aAsOfTextMatchedRanksFirst = `text_matched as materialized (select v.kind, v.id, v.project_key, ts_rank_cd(v.search, q.tsq, 32) as r from text_versions v, q where v.search @@ q.tsq),
`
	mainAt588aAsOfTextStandingRanksFirst = `text_standing as materialized (
  select v.kind, v.id, v.r
    from text_matched v
   where v.kind = 'comment' and ($2 = '' or v.project_key = $2) and (case when v.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (v.id)::uuid end) is not null
     and not exists (select from public.comments d where d.id = (case when v.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (v.id)::uuid end) and d.deleted_at is not null)
  union all
  select v.kind, v.id, v.r
    from text_matched v
   where v.kind = 'ask' and ($2 = '' or v.project_key = $2) and (case when v.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (v.id)::uuid end) is not null
  union all
  select v.kind, v.id, v.r
    from text_matched v
   where v.kind = 'message' and ($2 = '' or v.project_key = $2) and (case when v.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (v.id)::uuid end) is not null
     and not exists (select from public.messages d where d.id = (case when v.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (v.id)::uuid end) and d.deleted_at is not null)
),
`
	mainAt588aAsOfTextBoundsRanksFirst = `text_bounds as materialized (
  select 'comment' as kind, (
           select min(top.r) from (
             select s.r from (select w.id, w.r from text_standing w where w.kind = 'comment' order by w.r desc offset 0) s
              where /* visibility predicate: comment */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ({seen}::jsonb)->>'kind', 'id', case when ({seen}::jsonb)->>'kind' = 'person' then user_threads_login(({seen}::jsonb)->>'id') else ({seen}::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'comment' and visible_node.id = s.id), true)
              order by s.r desc limit $7) top) as r
  union all
  select 'ask' as kind, (
           select min(top.r) from (
             select s.r from (select w.id, w.r from text_standing w where w.kind = 'ask' order by w.r desc offset 0) s
              where /* visibility predicate: ask */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ({seen}::jsonb)->>'kind', 'id', case when ({seen}::jsonb)->>'kind' = 'person' then user_threads_login(({seen}::jsonb)->>'id') else ({seen}::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'ask' and visible_node.id = s.id), true)
              order by s.r desc limit $7) top) as r
  union all
  select 'message' as kind, (
           select min(top.r) from (
             select s.r from (select w.id, w.r from text_standing w where w.kind = 'message' order by w.r desc offset 0) s
              where /* visibility predicate: message */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ({seen}::jsonb)->>'kind', 'id', case when ({seen}::jsonb)->>'kind' = 'person' then user_threads_login(({seen}::jsonb)->>'id') else ({seen}::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'message' and visible_node.id = s.id), true)
              order by s.r desc limit $7) top) as r
),
`
	mainAt588aAsOfTextCandidatesRanksFirst = `text_candidates as materialized (
  select w.kind, w.id::uuid as id, w.r
    from text_standing w join text_bounds b on b.kind = w.kind and w.r >= b.r
),
`
	mainAt588aAsOfTextPostsRanksFirst = `text_posts as materialized (
  select w.kind, c.id, c.issue_key, c.artifact_id, c.created_at, w.r
    from text_candidates w
    join public.comments c on c.id = w.id
   where w.kind = 'comment'
  union all
  select w.kind, k.id, k.issue_key, k.artifact_id, k.created_at, w.r
    from text_candidates w
    join public.asks k on k.id = w.id
   where w.kind = 'ask'
  union all
  select w.kind, m.id, m.issue_key, null::uuid, m.created_at, w.r
    from text_candidates w
    join public.messages m on m.id = w.id
   where w.kind = 'message'
),
`
	mainAt588aAsOfTextIssues = `text_issues as materialized (
  select i.key, i.project_key, i.title, i.status, i.updated_at
    from issues i
   where i.key in (select p.issue_key from text_posts p)
),
`
	mainAt588aAsOfKindsHead = `kinds as (select * from (
  select 'issue' as kind, 'keyword' as list, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
         null::text as window_id, null::text[] as covers,
         i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
         false as own, v.r as r
    from text_matched v join issues i on i.key = v.id, q
   where v.kind = 'issue' and not (i.key = any(q.own_keys)) and ($2 = '' or i.project_key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
  union all
  select 'issue', 'keyword', i.key, null::uuid, null::uuid, i.key, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         true, ts_rank_cd(v.search, q.tsq, 32)
    from q, unnest(q.own_keys) as named(key), issues i, text_versions v
   where i.key = named.key and v.kind = 'issue' and v.id = i.key and ($2 = '' or i.project_key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
  union all
  select 'document' as kind, 'keyword', a.issue_key, case when a.issue_key is null then a.id else null::uuid end, a.id, a.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, v.created_at),
         false, ts_rank_cd(v.search, q.tsq, 32)
    from document_search v
    join artifacts a on a.id = v.artifact_id
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or p.key = $2) and v.created_at <= '{cut}'::timestamptz
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
  union all
  select 'document' as kind, 'keyword', a.issue_key, case when a.issue_key is null then a.id else null::uuid end, a.id, a.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, v.created_at),
         false, ts_rank_cd(v.search, q.tsq, 32)
    from document_search d
    join artifacts a on a.id = d.artifact_id
    cross join lateral (select n.number, n.created_at, n.search from artifact_versions n
                         where n.artifact_id = a.id and n.markdown is not null
                         order by n.number desc limit 1) v
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key, q
   where a.kind = 'doc' and d.created_at > '{cut}'::timestamptz and v.search @@ q.tsq and ($2 = '' or p.key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
  union all
`
	mainAt588aAsOfPostArmsRanksFirst = `  select 'comment' as kind, 'keyword', c.issue_key, c.artifact_id, null::uuid, c.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, c.r
    from text_posts c
    left join text_issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key
   where c.kind = 'comment'
  union all
  select 'ask' as kind, 'keyword', k.issue_key, null::uuid, null::uuid, k.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, coalesce(i.updated_at, k.created_at),
         false, k.r
    from text_posts k
    left join text_issues i on i.key = k.issue_key
   where k.kind = 'ask'
  union all
  select 'message' as kind, 'keyword', m.issue_key, null::uuid, null::uuid, m.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         false, m.r
    from text_posts m join text_issues i on i.key = m.issue_key
   where m.kind = 'message'
  union all
  select w.kind, 'keyword', null::text, null::uuid, null::uuid, w.id, null::text,
         null::text, null::text[],
         null::text, null::text, null::text, null::text, null::text, null::timestamptz,
         false, w.r
    from text_standing w join text_bounds b on b.kind = w.kind
   where w.r < b.r
`
	mainAt588aAsOfTail = `) arms where /* visibility predicate: every kind */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ({seen}::jsonb)->>'kind', 'id', case when ({seen}::jsonb)->>'kind' = 'person' then user_threads_login(({seen}::jsonb)->>'id') else ({seen}::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = case arms.kind when 'document' then 'artifact' else arms.kind end and visible_node.id = case case arms.kind when 'document' then 'artifact' else arms.kind end when 'artifact' then (select store_artifact.ref_key from artifacts store_artifact where store_artifact.id::text = arms.id::text) else arms.id::text end), true)
),
`
	mainAt588aAsOfRankedKeyword = `ranked as (
  select *, row_number() over (partition by kind order by own desc, r desc, updated_at desc, id) as pos,
         count(*) over (partition by kind) as matches
    from kinds
),
`
	mainAt588aAsOfLegs = `legs as (
  select * from ranked where pos <= $7
),
`
	mainAt588aAsOfFused = `fused as (
  select kind, id, bool_or(own) as own_lead,
         sum(case list when 'keyword' then 1 * r else 0.4 * r end) / ($8 + 1)
           + case when bool_or(own) then 1 else 0 end as score
    from legs group by kind, id
),
`
	mainAt588aAsOfTotalsKeyword = `totals as (
  select (select coalesce(sum(matches), 0) from (select max(matches) as matches from legs group by kind) each_kind) as total,
         (select count(*) from fused) as reachable
),
`
	mainAt588aAsOfRankedPageKeyword = `ranked_page as (
  select l.*, f.score, f.own_lead
    from legs l join fused f using (kind, id)
   order by f.score desc, case when f.own_lead then l.id end, l.updated_at desc, l.id
   limit $3 offset $6
),
`
	mainAt588aAsOfPage = `page as (
  select r.kind, r.issue_key,
         case r.kind when 'ask' then shown_ask.owner_artifact_id else r.owner_artifact_id end as owner_artifact_id,
         case r.kind when 'ask' then shown_ask.artifact_id when 'comment' then shown_comment.artifact_id else r.artifact_id end as artifact_id,
         r.id, case r.kind when 'ask' then shown_ask.block_id else r.block_id end as block_id, r.covers,
         r.issue_title, r.issue_status,
         case r.kind when 'ask' then shown_ask.owner_project else r.owner_project end as owner_project,
         case r.kind when 'ask' then shown_ask.owner_slug else r.owner_slug end as owner_slug,
         case r.kind when 'ask' then shown_ask.owner_name else r.owner_name end as owner_name,
         r.updated_at, r.score, r.own_lead
    from ranked_page r
    left join lateral (
      select case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) end as owner_artifact_id,
             coalesce(k.artifact_id, k.block_artifact_id, (case when pg_input_is_valid(k.anchor->>'artifact_id', 'uuid') then (k.anchor->>'artifact_id')::uuid end)) as artifact_id,
             case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id,
             p.key as owner_project, a.slug as owner_slug, a.name as owner_name
        from asks k
        left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
        left join projects p on p.key = a.project_key
       where r.kind = 'ask' and k.id = (case when r.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (r.id)::uuid end)
      offset 0
    ) shown_ask on true
    left join lateral (
      select coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid) as artifact_id
        from comments c
       where r.kind = 'comment' and c.id = (case when r.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (r.id)::uuid end)
      offset 0
    ) shown_comment on true
)
select t.total, t.reachable, r.kind, case when r.owner_artifact_id is null then 'issue' else 'document' end,
       r.issue_key, r.issue_title, r.issue_status,
       r.owner_project, r.owner_slug, r.owner_artifact_id::text, r.owner_name,
       ar.slug, ar.name, coalesce(ar.is_primary, false), r.id, r.block_id, hit.block_id, r.score::float8,
` + searchOracleCutColumns + `
  from totals t cross join q
  left join (page r left join artifacts ar on ar.id = r.artifact_id) on true
  -- At a cut, each hit's text as it stood at the cut: a document's newest version at the cut,
  -- and every other kind's version in the keyword table, which its arm matched.
  left join lateral (
    select case r.kind
      when 'document' then (select v.markdown from artifact_versions v where v.artifact_id = r.artifact_id order by v.number desc limit 1)
      else (select tv.body from text_versions tv where tv.kind = r.kind and tv.id = r.id)
    end as text
    offset 0
  ) txt on true
` + searchOracleCutLaterals + `
  -- The block of r.covers that matched, if the row carries a window. Each covered block's own
  -- text comes from the table its indexer writes it to: a document's, a comment's and an ask's
  -- block texts are kept per version or revision, the latest being the current text. A message
  -- records no revisions, so it keeps no text table (refs/item_blocks.go's table map) and its
  -- block is ranked on the excerpt its row holds, the block's head as refs.BlockExcerpt cuts it,
  -- which an edit of the message rewrites with the block it changed.
  left join lateral (
    select covered.block_id
      from unnest(r.covers) with ordinality as covered(block_id, ord)
      cross join lateral (
        select case r.kind
          when 'document' then (select x.text from document_block_texts x
                                 where x.artifact_id = r.artifact_id and x.block_id = covered.block_id
                                 order by x.version desc limit 1)
          when 'comment' then (select x.text from comment_block_texts x
                                 where x.comment_id = r.id::uuid and x.block_id = covered.block_id
                                 order by x.revision desc limit 1)
          when 'ask' then (select x.text from ask_block_texts x
                                 where x.ask_id = r.id::uuid and x.block_id = covered.block_id
                                 order by x.revision desc limit 1)
          when 'message' then (select x.excerpt from message_blocks x
                                 where x.message_id = r.id::uuid and x.block_id = covered.block_id)
        end as text
      ) block
     where /* visibility predicate: every kind */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ({seen}::jsonb)->>'kind', 'id', case when ({seen}::jsonb)->>'kind' = 'person' then user_threads_login(({seen}::jsonb)->>'id') else ({seen}::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = case r.kind when 'document' then 'block' else r.kind || '_block' end and visible_node.id = case r.kind when 'document' then ar.ref_key else r.kind || '/' || r.id end || '#' || covered.block_id), true)
     order by ts_rank_cd(to_tsvector('english', search_text(coalesce(block.text, ''))), q.tsq) desc, covered.ord
     limit 1
  ) hit on true
 order by r.score desc, case when r.own_lead then r.id end, r.updated_at desc, r.id
`
	mainAt588aAsOfTextMatchedReadWhole = `text_matched as materialized (select v.kind, v.id, ts_rank_cd(v.search, q.tsq, 32) as r from text_versions v, q where v.search @@ q.tsq),
`
	mainAt588aAsOfTextPostsReadWhole = `text_posts as materialized (
  select 'comment' as kind, c.id, c.issue_key, c.artifact_id, c.author, c.created_at, v.r
    from text_matched v join public.comments c on c.id = (case when v.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (v.id)::uuid end)
   where v.kind = 'comment' and c.deleted_at is null
  union all
  select 'ask', k.id, k.issue_key, k.artifact_id, k.author, k.created_at, v.r
    from text_matched v join public.asks k on k.id = (case when v.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (v.id)::uuid end)
   where v.kind = 'ask'
  union all
  select 'message', m.id, m.issue_key, null::uuid, m.author, m.created_at, v.r
    from text_matched v join public.messages m on m.id = (case when v.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (v.id)::uuid end)
   where v.kind = 'message' and m.deleted_at is null
),
`
	mainAt588aAsOfPostArmsReadWhole = `  select 'comment' as kind, 'keyword', c.issue_key, c.artifact_id, null::uuid, c.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, c.r
    from text_posts c
    left join text_issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where c.kind = 'comment' and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce((select x.edited_at from comments x where x.id = c.id), c.created_at) >= q.updated_since)
  union all
  select 'ask' as kind, 'keyword', k.issue_key, null::uuid, null::uuid, k.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, coalesce(i.updated_at, k.created_at),
         false, k.r
    from text_posts k
    left join text_issues i on i.key = k.issue_key, q
   where k.kind = 'ask' and ($2 = '' or coalesce(i.project_key, (
           select p.key from artifacts a join projects p on p.key = a.project_key
            where a.id = coalesce(k.artifact_id, (select b.block_artifact_id from asks b where b.id = k.id)))) = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce((select x.edited_at from asks x where x.id = k.id), k.created_at) >= q.updated_since)
  union all
  select 'message' as kind, 'keyword', m.issue_key, null::uuid, null::uuid, m.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         false, m.r
    from text_posts m join text_issues i on i.key = m.issue_key, q
   where m.kind = 'message' and ($2 = '' or i.project_key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce((select x.edited_at from messages x where x.id = m.id), m.created_at) >= q.updated_since)
`
	mainAt588aAsOfOpenerMeaning = `
with q as (select
         case when $12 = '' then websearch_to_tsquery('english', $1)
              when $13 = '' then websearch_to_tsquery('english', $12)
              else websearch_to_tsquery('english', $12) && websearch_to_tsquery('english', $13)
         end as tsq,
         $4::text as term, $14::text[] as own_keys,
         $15::text as status_filter, $16::text as author_filter, $17::timestamptz as updated_since),
`
	mainAt588aAsOfMeaningIssueCandidates = `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at, w.r
    from (
      select e.id as window_id, e.covers, e.id as item, 1 - e.distance as r
        from (
          select x.id, x.covers, x.embedding <=> $9::vector as distance
            from embeddings x
           where x.kind = 'issue' and x.embedding is not null and x.model = $11 and x.embedded_at <= '{cut}'::timestamptz
           order by x.embedding <=> $9::vector
          offset 0
        ) e
       where (select true
                from issues i
                join text_versions v on v.kind = 'issue' and v.id = i.key
                join public.issues present_issue on present_issue.key = i.key
                left join public.artifacts present_spec on present_spec.issue_key = present_issue.key and present_spec.is_primary, q
               where i.key = e.id
                 and v.body = present_issue.title and v.spec_artifact_id is not distinct from present_spec.id
                 and v.spec_version is not distinct from (select max(n.number) from public.artifact_versions n where n.artifact_id = present_spec.id)
                 and /* visibility predicate: issue */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ({seen}::jsonb)->>'kind', 'id', case when ({seen}::jsonb)->>'kind' = 'person' then user_threads_login(({seen}::jsonb)->>'id') else ({seen}::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'issue' and visible_node.id = i.key), true) and ($2 = '' or i.project_key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or i.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or i.updated_at >= q.updated_since)
               limit 1)
       limit $7
    ) w
    join issues i on i.key = w.item
),
`
	mainAt588aAsOfMeaningIssue = `meaning_issue as (
  select 'issue' as kind, 'meaning' as list, issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, id, null::text as block_id,
         null::text as window_id, null::text[] as covers,
         issue_title, issue_status, null::text as owner_project, null::text as owner_slug, null::text as owner_name, updated_at,
         false as own, r
    from meaning_issue_candidates where r >= $10
),
`
	mainAt588aAsOfMeaningDocumentCandidates = `meaning_document_candidates as (
  select a.issue_key, a.id, w.window_id, w.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at, w.r
    from (
      select e.id as window_id, e.covers, (case when split_part(e.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(e.id, '#', 1))::uuid end) as item, 1 - e.distance as r
        from (
          select x.id, x.covers, x.embedding <=> $9::vector as distance
            from embeddings x
           where x.kind = 'document_block' and x.embedding is not null and x.model = $11 and x.embedded_at <= '{cut}'::timestamptz
           order by x.embedding <=> $9::vector
          offset 0
        ) e
       where (select true
                from artifacts a
                left join issues i on i.key = a.issue_key
                join projects p on p.key = a.project_key
                join lateral (select v.number, v.created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
               where a.id = (case when split_part(e.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(e.id, '#', 1))::uuid end) and a.kind = 'doc'
                 and v.number = (select max(n.number) from public.artifact_versions n where n.artifact_id = a.id)
                 and /* visibility predicate: artifact */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ({seen}::jsonb)->>'kind', 'id', case when ({seen}::jsonb)->>'kind' = 'person' then user_threads_login(({seen}::jsonb)->>'id') else ({seen}::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'artifact' and visible_node.id = a.ref_key), true) and ($2 = '' or p.key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or a.created_by->>'id' = q.author_filter)
     and (q.updated_since is null or v.created_at >= q.updated_since)
               limit 1)
       limit 1000
    ) w
    join artifacts a on a.id = w.item
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select v.created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true
),
`
	mainAt588aAsOfMeaningDocument = `meaning_document as (
  select distinct on (meaning_document_candidates.id) 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
   order by meaning_document_candidates.id, r desc, window_id
),
`
	mainAt588aAsOfMeaningCommentCandidates = `meaning_comment_candidates as (
  select c.issue_key, c.artifact_id, c.anchor, c.id, w.window_id, w.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, c.created_at) as updated_at, w.r
    from (
      select e.id as window_id, e.covers, (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end) as item, 1 - e.distance as r
        from (
          select x.id, x.covers, x.embedding <=> $9::vector as distance
            from embeddings x
           where x.kind = 'comment_block' and x.embedding is not null and x.model = $11 and x.embedded_at <= '{cut}'::timestamptz
           order by x.embedding <=> $9::vector
          offset 0
        ) e
       where (select true
                from comments c
                join text_versions v on v.kind = 'comment' and v.id = c.id::text
                join public.comments present_comment on present_comment.id = c.id
                left join issues i on i.key = c.issue_key
                left join artifacts a on a.id = c.artifact_id
                left join projects p on p.key = a.project_key, q
               where c.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
                 and v.deleted_at is null and v.body = present_comment.body
                 and /* visibility predicate: comment */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ({seen}::jsonb)->>'kind', 'id', case when ({seen}::jsonb)->>'kind' = 'person' then user_threads_login(({seen}::jsonb)->>'id') else ({seen}::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'comment' and visible_node.id = c.id::text), true) and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or c.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(c.edited_at, c.created_at) >= q.updated_since)
               limit 1)
       limit 1000
    ) w
    join comments c on c.id = w.item
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key
),
`
	mainAt588aAsOfMeaningComment = `meaning_comment as (
  select distinct on (meaning_comment_candidates.id) 'comment' as kind, 'meaning' as list, issue_key, artifact_id, coalesce(artifact_id, (anchor->>'artifact_id')::uuid), id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_comment_candidates where r >= $10
   order by meaning_comment_candidates.id, r desc, window_id
),
`
	mainAt588aAsOfMeaningAskCandidates = `meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id, w.window_id, w.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at, w.r
    from (
      select e.id as window_id, e.covers, (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end) as item, 1 - e.distance as r
        from (
          select x.id, x.covers, x.embedding <=> $9::vector as distance
            from embeddings x
           where x.kind = 'ask_part' and x.embedding is not null and x.model = $11 and x.embedded_at <= '{cut}'::timestamptz
           order by x.embedding <=> $9::vector
          offset 0
        ) e
       where (select true
                from asks k
                join text_versions v on v.kind = 'ask' and v.id = k.id::text
                join public.asks present_ask on present_ask.id = k.id
                left join issues i on i.key = k.issue_key
                left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
                left join projects p on p.key = a.project_key, q
               where k.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
                 and v.body = present_ask.question || ' ' || ask_search_suffix(present_ask.options, present_ask.answer)
                 and /* visibility predicate: ask */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ({seen}::jsonb)->>'kind', 'id', case when ({seen}::jsonb)->>'kind' = 'person' then user_threads_login(({seen}::jsonb)->>'id') else ({seen}::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'ask' and visible_node.id = k.id::text), true) and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or k.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(k.edited_at, k.created_at) >= q.updated_since)
               limit 1)
       limit 1000
    ) w
    join asks k on k.id = w.item
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key
),
`
	mainAt588aAsOfMeaningAsk = `meaning_ask as (
  select distinct on (meaning_ask_candidates.id) 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (case when pg_input_is_valid(anchor->>'artifact_id', 'uuid') then (anchor->>'artifact_id')::uuid end)), id::text, block_id,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
   order by meaning_ask_candidates.id, r desc, window_id
),
`
	mainAt588aAsOfMeaningMessageCandidates = `meaning_message_candidates as (
  select m.issue_key, m.id, w.window_id, w.covers, i.title as issue_title, i.status as issue_status, i.updated_at, w.r
    from (
      select e.id as window_id, e.covers, (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end) as item, 1 - e.distance as r
        from (
          select x.id, x.covers, x.embedding <=> $9::vector as distance
            from embeddings x
           where x.kind = 'message_block' and x.embedding is not null and x.model = $11 and x.embedded_at <= '{cut}'::timestamptz
           order by x.embedding <=> $9::vector
          offset 0
        ) e
       where (select true
                from messages m
                join text_versions v on v.kind = 'message' and v.id = m.id::text
                join public.messages present_message on present_message.id = m.id
                join issues i on i.key = m.issue_key, q
               where m.id = (case when split_part(split_part(e.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(e.id, '#', 1), '/', 2))::uuid end)
                 and v.deleted_at is null and v.body = present_message.body
                 and /* visibility predicate: message */` + mainAt4975VisibilityHead + `
	end = jsonb_build_object('kind', ({seen}::jsonb)->>'kind', 'id', case when ({seen}::jsonb)->>'kind' = 'person' then user_threads_login(({seen}::jsonb)->>'id') else ({seen}::jsonb)->>'id' end), false)
		from graph_nodes visible_node where visible_node.kind = 'message' and visible_node.id = m.id::text), true) and ($2 = '' or i.project_key = $2)
     and (q.status_filter = '' or i.status = q.status_filter)
     and (q.author_filter = '' or m.author->>'id' = q.author_filter)
     and (q.updated_since is null or coalesce(m.edited_at, m.created_at) >= q.updated_since)
               limit 1)
       limit 1000
    ) w
    join messages m on m.id = w.item
    join issues i on i.key = m.issue_key
),
`
	mainAt588aAsOfMeaningMessage = `meaning_message as (
  select distinct on (meaning_message_candidates.id) 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
   order by meaning_message_candidates.id, r desc, window_id
),
`
	mainAt588aAsOfMeaningUnion = `  union all select * from meaning_issue
  union all select * from meaning_document
  union all select * from meaning_comment
  union all select * from meaning_ask
  union all select * from meaning_message
`
	mainAt588aAsOfRankedMeaning = `ranked as (
  select *, row_number() over (partition by kind, list order by own desc, r desc, updated_at desc, id) as pos
    from kinds
),
`
	mainAt588aAsOfLegsUniqueMeaning = `legs_unique as (
  select distinct on (kind, id) kind, issue_key, owner_artifact_id, artifact_id, id, block_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at
    from legs
   order by kind, id, (window_id is null), r desc
),
`
	mainAt588aAsOfTotalsMeaning = `totals as (
  select (select count(*) from (select distinct kind, id from kinds) u) as total,
         (select count(*) from fused) as reachable
),
`
	mainAt588aAsOfRankedPageMeaning = `ranked_page as (
  select l.*, f.score, f.own_lead
    from legs_unique l join fused f using (kind, id)
   order by f.score desc, case when f.own_lead then l.id end, l.updated_at desc, l.id
   limit $3 offset $6
),
`
)

// mainAt588aAsOfKeywordRanksFirst, mainAt588aAsOfKeywordReadWhole, mainAt588aAsOfMeaningRanksFirst
// and mainAt588aAsOfMeaningReadWhole are main's searchAsOfQuery and searchAsOfMeaningQuery at
// 588af9ce for a search that ranks each post kind first and for one that reads them whole, over
// every kind, each the concatenation of its pieces in the order main's builders join them, with
// the cut and the acting credential's parameter still written as markers.
const (
	mainAt588aAsOfKeywordRanksFirst = mainAt588aAsOfOpenerKeyword +
		mainAt588aAsOfTextVersions +
		mainAt588aAsOfTextMatchedRanksFirst +
		mainAt588aAsOfTextStandingRanksFirst +
		mainAt588aAsOfTextBoundsRanksFirst +
		mainAt588aAsOfTextCandidatesRanksFirst +
		mainAt588aAsOfTextPostsRanksFirst +
		mainAt588aAsOfTextIssues +
		mainAt588aAsOfKindsHead +
		mainAt588aAsOfPostArmsRanksFirst +
		mainAt588aAsOfTail +
		mainAt588aAsOfRankedKeyword +
		mainAt588aAsOfLegs +
		mainAt588aAsOfFused +
		mainAt588aAsOfTotalsKeyword +
		mainAt588aAsOfRankedPageKeyword +
		mainAt588aAsOfPage
	mainAt588aAsOfKeywordReadWhole = mainAt588aAsOfOpenerKeyword +
		mainAt588aAsOfTextVersions +
		mainAt588aAsOfTextMatchedReadWhole +
		mainAt588aAsOfTextPostsReadWhole +
		mainAt588aAsOfTextIssues +
		mainAt588aAsOfKindsHead +
		mainAt588aAsOfPostArmsReadWhole +
		mainAt588aAsOfTail +
		mainAt588aAsOfRankedKeyword +
		mainAt588aAsOfLegs +
		mainAt588aAsOfFused +
		mainAt588aAsOfTotalsKeyword +
		mainAt588aAsOfRankedPageKeyword +
		mainAt588aAsOfPage
	mainAt588aAsOfMeaningRanksFirst = mainAt588aAsOfOpenerMeaning +
		mainAt588aAsOfTextVersions +
		mainAt588aAsOfTextMatchedRanksFirst +
		mainAt588aAsOfTextStandingRanksFirst +
		mainAt588aAsOfTextBoundsRanksFirst +
		mainAt588aAsOfTextCandidatesRanksFirst +
		mainAt588aAsOfTextPostsRanksFirst +
		mainAt588aAsOfTextIssues +
		mainAt588aAsOfMeaningIssueCandidates +
		mainAt588aAsOfMeaningIssue +
		mainAt588aAsOfMeaningDocumentCandidates +
		mainAt588aAsOfMeaningDocument +
		mainAt588aAsOfMeaningCommentCandidates +
		mainAt588aAsOfMeaningComment +
		mainAt588aAsOfMeaningAskCandidates +
		mainAt588aAsOfMeaningAsk +
		mainAt588aAsOfMeaningMessageCandidates +
		mainAt588aAsOfMeaningMessage +
		mainAt588aAsOfKindsHead +
		mainAt588aAsOfPostArmsRanksFirst +
		mainAt588aAsOfMeaningUnion +
		mainAt588aAsOfTail +
		mainAt588aAsOfRankedMeaning +
		mainAt588aAsOfLegs +
		mainAt588aAsOfFused +
		mainAt588aAsOfLegsUniqueMeaning +
		mainAt588aAsOfTotalsMeaning +
		mainAt588aAsOfRankedPageMeaning +
		mainAt588aAsOfPage
	mainAt588aAsOfMeaningReadWhole = mainAt588aAsOfOpenerMeaning +
		mainAt588aAsOfTextVersions +
		mainAt588aAsOfTextMatchedReadWhole +
		mainAt588aAsOfTextPostsReadWhole +
		mainAt588aAsOfTextIssues +
		mainAt588aAsOfMeaningIssueCandidates +
		mainAt588aAsOfMeaningIssue +
		mainAt588aAsOfMeaningDocumentCandidates +
		mainAt588aAsOfMeaningDocument +
		mainAt588aAsOfMeaningCommentCandidates +
		mainAt588aAsOfMeaningComment +
		mainAt588aAsOfMeaningAskCandidates +
		mainAt588aAsOfMeaningAsk +
		mainAt588aAsOfMeaningMessageCandidates +
		mainAt588aAsOfMeaningMessage +
		mainAt588aAsOfKindsHead +
		mainAt588aAsOfPostArmsReadWhole +
		mainAt588aAsOfMeaningUnion +
		mainAt588aAsOfTail +
		mainAt588aAsOfRankedMeaning +
		mainAt588aAsOfLegs +
		mainAt588aAsOfFused +
		mainAt588aAsOfLegsUniqueMeaning +
		mainAt588aAsOfTotalsMeaning +
		mainAt588aAsOfRankedPageMeaning +
		mainAt588aAsOfPage
)

// mainAt588aCutMarker and mainAt588aSeenMarker are where the frozen statements at a cut write the
// two values a statement takes (mainAt588aAsOfStatement): the cut, as the quoted time of its
// timestamptz literal, and the parameter the acting credential is bound at.
const (
	mainAt588aCutMarker  = "'{cut}'"
	mainAt588aSeenMarker = "{seen}"
)

// mainAt588aAsOfStatement is main's text at 588af9ce of one of its statements at a cut: base, one
// of the four mainAt588aAsOf statements, with the cut written at each mainAt588aCutMarker as main
// writes a cut's literal (its time in UTC to the microsecond, with six fractional digits), the
// acting credential's parameter at each mainAt588aSeenMarker ($18 for a meaning statement, $15
// for a keyword one, searchQuery's and searchQueryMeaning's own numbers at 588af9ce), and the
// kinds the set leaves out where main writes them at the present too (mainAt4975Statement).
func mainAt588aAsOfStatement(base string, meaning bool, cut time.Time, set uint8) string {
	if !strings.Contains(base, mainAt588aCutMarker) || !strings.Contains(base, mainAt588aSeenMarker) {
		panic("mainAt588aAsOfStatement: the statement holds no cut or no credential to write")
	}
	seen := "$15"
	if meaning {
		seen = "$18"
	}
	written := strings.ReplaceAll(base, mainAt588aCutMarker, "'"+cut.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z07:00")+"'")
	return mainAt4975Statement(strings.ReplaceAll(written, mainAt588aSeenMarker, seen), set)
}

// searchResultsItems is how many items of each kind hold the kind's word in R3's fixture: more
// than the 1,000 matches of a kind R3 names, and more than searchWindowCandidates, the windows a
// meaning arm takes.
const searchResultsItems = 1500

// searchResultsKinds is R3's five kinds, each with the word its items hold and no other item
// does. A kind's place in the list is the dimension whose unit vector the fake embedder answers
// for its word, which the fixture's windows are written against (searchResultsVectorSQL).
var searchResultsKinds = []struct{ kind, word string }{
	{"issue", "wombat"}, {"document", "ibex"}, {"comment", "okapi"}, {"ask", "quokka"}, {"message", "tapir"},
}

// searchResultsBase is the instant R3's fixture dates its items from: item n of each kind is
// written n minutes after it.
var searchResultsBase = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// searchResultsFixture is R3's corpus (newSearchResultsFixture).
type searchResultsFixture struct {
	database *store.Store
	embedder *fakeEmbedder
	// projects are the two projects the items alternate between: the first holds every odd item
	// of each kind, and so the oldest.
	projects [2]string
	// since is an updated_since that keeps the newer half of each kind's items and not the oldest.
	since time.Time
	// oldest is each kind's first item, by the id the statements answer for it (r.id).
	oldest map[string]string
}

// searchResultsItem is what item n of every kind of R3's fixture is written with, over the
// parameters each insert of an item takes ($1 and $2 the two projects, $3 searchResultsBase, $4
// searchResultsItems): its project, the first for an odd n and the second for an even one; its
// author, bob for every fourth and alice for the rest; and when it was written.
const searchResultsItem = `
			from generate_series(1, $4::int) n
			cross join lateral (
			  select case when n % 2 = 1 then $1::text else $2::text end as key,
			         jsonb_build_object('kind', 'user', 'id', case when n % 4 = 0 then 'bob' else 'alice' end) as author,
			         $3::timestamptz + make_interval(mins => n) as at
			) item`

// searchResultsVectorSQL is the vector of item n of the kind at dimension, both SQL
// expressions: a cosine of 0.9 - 0.0002 n with the unit vector of dimension, which the fake
// embedder answers for the kind's word, and a cosine of 0 with every other kind's. The rest of its
// length stands at a position past the five kinds' own, one of its own for each item of a kind,
// so no two windows of a corpus share a vector.
func searchResultsVectorSQL(dimension, n string) string {
	return pairVectorSQL("("+dimension+") + 1",
		fmt.Sprintf("%d + (%d * (%s) + (%s)) %% %d", len(searchResultsKinds)+1, len(searchResultsKinds), n, dimension, embed.Dimension-len(searchResultsKinds)),
		"0.9 - 0.0002 * ("+n+")")
}

// newSearchResultsFixture writes R3's corpus as SQL. Each of the five kinds has
// searchResultsItems items holding the kind's word, item n written as searchResultsItem says.
// The issues carry the status the others are filtered by: issue n is done for every third n and
// todo for the rest. Item n of each of the other four kinds sits on issue n, except that a
// document n whose n%4 is 3 or 0 is its project's own, and the comment and the ask n whose n%4
// is 3 sit on that document. So the status filter leaves out every project document and every
// item on one, and each kind's first item sits on the first issue, in the first project, by
// alice.
//
// The first item of each kind holds its word three times or more and every other item holds it
// once, so the oldest item is the best match by ts_rank_cd. Every item has one window settled at
// the fake embedder's identity, whose vector (searchResultsVectorSQL) is the nearest to the
// kind's query for the first item and further for each later one, and orthogonal to every other
// kind's query: a search for one kind's word meets no other kind's window above
// searchMeaningFloor. The issues' windows are the rows the issues' own trigger enqueued, given
// their vectors; the other kinds' are written as crowdCommentWindows writes its own.
func newSearchResultsFixture(t *testing.T) searchResultsFixture {
	t.Helper()
	embedder := &fakeEmbedder{vectors: map[string][]float32{}}
	for dimension, kind := range searchResultsKinds {
		vector := make([]float32, embed.Dimension)
		vector[dimension] = 1
		embedder.vectors[kind.word] = vector
	}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	fixture := searchResultsFixture{
		database: database, embedder: embedder, projects: [2]string{"ANSA", "BETA"},
		since:  searchResultsBase.Add(searchResultsItems/2*time.Minute + 30*time.Second),
		oldest: map[string]string{},
	}
	createTestProject(t, handler, fixture.projects[:]...)
	ctx := context.Background()
	items := []any{fixture.projects[0], fixture.projects[1], searchResultsBase, searchResultsItems}
	for _, step := range []struct {
		what, statement string
		arguments       []any
		rows            int64
	}{
		{"the issues", `
			insert into issues (key, project_key, number, title, status, created_by, rank, created_at, updated_at)
			select item.key || '-' || n, item.key, n,
			       case when n = 1 then 'Wombat wombat wombat census' else 'Wombat census ' || n end,
			       case when n % 3 = 0 then 'done' else 'todo' end, item.author, lpad(n::text, 20, '0'), item.at, item.at` +
			searchResultsItem, items, searchResultsItems},
		{"the documents", `
			insert into artifacts (project_key, issue_key, slug, name, kind, created_by, created_at)
			select item.key, case when n % 4 in (1, 2) then item.key || '-' || n end,
			       'ibex-notes-' || n, 'Ibex notes ' || n || '.md', 'doc', item.author, item.at` +
			searchResultsItem, items, searchResultsItems},
		{"their versions", `
			insert into artifact_versions (artifact_id, number, markdown, authors, created_at)
			select a.id, 1, case when a.slug = 'ibex-notes-1' then E'# Ibex notes\n\nIbex ibex ibex.\n'
			                     else '# Notes ' || split_part(a.slug, '-', 3) || E'\n\nAn ibex sighting.\n' end,
			       '[]', a.created_at
			from artifacts a where a.slug like 'ibex-notes-%'
		`, nil, searchResultsItems},
		{"the comments", `
			insert into comments (issue_key, artifact_id, author, body, created_at)
			select case when n % 4 <> 3 then item.key || '-' || n end, a.id, item.author,
			       case when n = 1 then 'Okapi okapi okapi.' else 'An okapi sighting, ' || n || '.' end, item.at` +
			searchResultsItem + `
			left join artifacts a on n % 4 = 3 and a.slug = 'ibex-notes-' || n
		`, items, searchResultsItems},
		{"the asks", `
			insert into asks (issue_key, artifact_id, author, question, created_at)
			select case when n % 4 <> 3 then item.key || '-' || n end, a.id, item.author,
			       case when n = 1 then 'Quokka quokka quokka?' else 'A quokka count, ' || n || '?' end, item.at` +
			searchResultsItem + `
			left join artifacts a on n % 4 = 3 and a.slug = 'ibex-notes-' || n
		`, items, searchResultsItems},
		{"the messages", `
			insert into messages (issue_key, author, body, created_at)
			select item.key || '-' || n, item.author,
			       case when n = 1 then 'Tapir tapir tapir.' else 'A tapir sighting, ' || n || '.' end, item.at` +
			searchResultsItem, items, searchResultsItems},
		{"the issues' windows", `
			update embeddings e
			   set embedded_hash = e.content_hash, model = $1, embedded_at = now(),
			       embedding = ` + searchResultsVectorSQL("0", "i.number") + `
			  from issues i
			 where e.kind = 'issue' and e.id = i.key
		`, []any{embedder.Identity()}, searchResultsItems},
		{"the other kinds' windows", `
			insert into embeddings (kind, id, text_snapshot, content_hash, embedded_hash, model, embedding, embedded_at, covers)
			select w.kind, w.id, 'A window of ' || w.id, md5(w.id), md5(w.id), $1,
			       ` + searchResultsVectorSQL("w.dimension", "w.n") + `, now(), array['b1']
			from (
			  select item.kind, item.dimension, item.id, (extract(epoch from item.at - $2::timestamptz) / 60)::int as n
			  from (
			    select 'document_block' as kind, 1 as dimension, a.id::text || '#b1' as id, a.created_at as at
			      from artifacts a where a.slug like 'ibex-notes-%'
			    union all select 'comment_block', 2, 'comment/' || c.id || '#b1', c.created_at from comments c
			    union all select 'ask_part', 3, 'ask/' || k.id || '#b1', k.created_at from asks k
			    union all select 'message_block', 4, 'message/' || m.id || '#b1', m.created_at from messages m
			  ) item
			) w
		`, []any{embedder.Identity(), searchResultsBase}, 4 * searchResultsItems},
	} {
		written, err := database.Pool.Exec(ctx, step.statement, step.arguments...)
		if err != nil {
			t.Fatalf("write %s: %v", step.what, err)
		}
		if written.RowsAffected() != step.rows {
			t.Fatalf("writing %s wrote %d rows, want %d", step.what, written.RowsAffected(), step.rows)
		}
	}
	var issue, document, comment, ask, message string
	if err := database.Pool.QueryRow(ctx, `
		select (select key from issues where number = 1),
		       (select id::text from artifacts where slug = 'ibex-notes-1'),
		       (select id::text from comments where body = 'Okapi okapi okapi.'),
		       (select id::text from asks where question = 'Quokka quokka quokka?'),
		       (select id::text from messages where body = 'Tapir tapir tapir.')`).Scan(&issue, &document, &comment, &ask, &message); err != nil {
		t.Fatalf("read each kind's oldest item: %v", err)
	}
	fixture.oldest = map[string]string{"issue": issue, "document": document, "comment": comment, "ask": ask, "message": message}
	if _, err := database.Pool.Exec(ctx, "analyze"); err != nil {
		t.Fatalf("analyze the fixture: %v", err)
	}
	return fixture
}

// statementAnswer is one statement's answer as R3 reads it: every row as text, in the order the
// statement answers them, and the total, the reachable and the items of the page, each written
// "kind/id". It is not search_as_of_test.go's searchAnswer, which is a route's decoded answer.
type statementAnswer struct {
	rows             []string
	total, reachable int64
	items            []string
}

// readStatementAnswer runs statement bound to arguments under settings (runSearchStatement) and
// reads its answer. A page past the last row is one row of totals and no item.
func readStatementAnswer(t *testing.T, ctx context.Context, database *store.Store, statement string, arguments []any, settings []string) statementAnswer {
	t.Helper()
	answer := statementAnswer{rows: []string{}}
	runSearchStatement(t, ctx, database, statement, arguments, settings, func(row pgx.CollectableRow) error {
		values, err := row.Values()
		if err != nil {
			return err
		}
		answer.rows = append(answer.rows, fmt.Sprint(values...))
		// The columns scanSearchRows reads: the totals first, the kind third and the id fifteenth.
		// The kind and the id are null on a page past the last row.
		var kind, id *string
		columns := make([]any, len(values))
		columns[0], columns[1], columns[2], columns[14] = &answer.total, &answer.reachable, &kind, &id
		if err := row.Scan(columns...); err != nil {
			return err
		}
		if kind != nil {
			answer.items = append(answer.items, *kind+"/"+*id)
		}
		return nil
	})
	return answer
}

// searchFilterTotal is what a filter row of R3 requires of the total its answer counts.
type searchFilterTotal int

const (
	// totalOpen leaves the total to the comparison with main's answer: the filter narrows the
	// matches by an amount this test does not state.
	totalOpen searchFilterTotal = iota
	// totalEvery is every match of the kind's word, 1,500 in every project and 750 in one.
	totalEvery
	// totalNone is no match at all, and no item on the page.
	totalNone
)

// searchFilter is one narrowing of R3's search: how it sets the search's parameters, whether
// the first page of its answer must hold the oldest item of the word's kind, and what it
// requires of the total.
type searchFilter struct {
	name        string
	narrow      func(*searchParameters)
	keepsOldest bool
	total       searchFilterTotal
}

// searchKindFilters are the filter rows that name kinds, for the word of the kind at index of
// searchResultsKinds: each of the five kinds alone, a pair holding the word's kind and the kind
// after it, and a pair of two other kinds, which holds no item the word matches. A row names the
// kinds the search asks for; the set the parameters carry is the kinds it leaves out,
// searchNoKind &^ asked. A row that names the word's kind counts every match and holds the
// oldest item on its first page, and a row that names only other kinds answers no match.
func searchKindFilters(index int) []searchFilter {
	row := func(asked ...int) searchFilter {
		var named searchKinds
		var names []string
		holds := false
		for _, kind := range asked {
			named |= 1 << kind
			names = append(names, searchResultsKinds[kind].kind)
			holds = holds || kind == index
		}
		filter := searchFilter{
			name:   "kinds " + strings.Join(names, " and "),
			narrow: func(p *searchParameters) { p.kinds = searchNoKind &^ named },
			total:  totalNone,
		}
		if holds {
			filter.keepsOldest, filter.total = true, totalEvery
		}
		return filter
	}
	next := func(by int) int { return (index + by) % len(searchResultsKinds) }
	var filters []searchFilter
	for kind := range searchResultsKinds {
		filters = append(filters, row(kind))
	}
	return append(filters, row(index, next(1)), row(next(1), next(2)))
}

// searchGenericPlanSetting makes a statement run under its generic plan, planned without the
// parameter values, which Postgres takes for a prepared text after five custom plans when it
// costs no more than their average (plan_cache_mode = auto).
const searchGenericPlanSetting = `set local plan_cache_mode = force_generic_plan`

// TestSearchAnswersMainsRowsOrderTotalAndReachable holds R3: each keyword arm ranks and counts
// every match whatever its age, and a search answers the rows, the order, the total and the
// reachable main's statements at 497581c7 answer, which are main's statements at 588af9ce byte for
// byte, for every set of kinds it names. The fixture (newSearchResultsFixture) gives each of the
// five kinds 1,500 items holding the kind's word, the oldest of them the best match by ts_rank_cd
// and by vector, and each kind's word is a query.
//
// For each query, each of the four statements - searchQuery, and searchQueryMeaning,
// searchQueryMeaningScoped and searchQueryMeaningJoined over the vector the fake embedder answers
// for the word - runs for each project form its caller binds it with (every project and one),
// with no filter and with each of status, author, updated_since and kinds alone, at offsets 0
// and 50, this build's text and main's over the same arguments and under the settings the route
// runs it under (searchScanSettings over searchKeywordScan or searchMeaningScan), the 15-second
// bound among them (D3). The kinds are no kind named, each of the five kinds alone, and two
// pairs of kinds, one holding the word's kind and one not (searchKindFilters). Each search runs
// its set's statement (searchKinds.statements) against main's statement for the same set
// (mainAt4975Statements). Set 0 gives the unkinded texts on both sides. The two answers must be
// the same rows in the same order. A search narrowed by nothing, or by kinds that include the
// word's own, must count all 1,500 of the kind's matches, or the 750 of one project; a search
// naming only other kinds must count none and page none, and is read at offset 0 alone, since
// no later page holds anything. The first page of a search whose filters keep the oldest item
// must hold it.
func TestSearchAnswersMainsRowsOrderTotalAndReachable(t *testing.T) {
	t.Parallel()
	fixture := newSearchResultsFixture(t)
	ctx := context.Background()
	for bit, kind := range searchResultsKinds {
		if kind.kind != mainAt4975KindNames[bit] || kind.kind != searchIndexedKinds[bit] {
			t.Fatalf("kind %d of the fixture is %q, but main's bit %d names %q and this build's %q", bit, kind.kind, bit, mainAt4975KindNames[bit], searchIndexedKinds[bit])
		}
	}
	every, one := "", fixture.projects[0]
	statements := []struct {
		name     string
		meaning  bool
		projects []string
		// generic runs the form of the statement that names a project under the generic plan
		// (searchGenericPlanSetting). The joined shape's custom plan for one project, the plan a
		// text takes for its first five runs, runs for about 16 seconds on this corpus - main's
		// text and this build's alike, with no kind named too - past the 15-second bound, and
		// its generic plan for about a quarter of a second. An answer does not depend on the
		// plan, and both sides run under the same one.
		generic    bool
		selectText func(*searchStatements) string
	}{
		{"searchQuery", false, []string{every, one}, false,
			func(s *searchStatements) string { return s.keyword }},
		{"searchQueryMeaning", true, []string{every}, false,
			func(s *searchStatements) string { return s.meaningEvery }},
		{"searchQueryMeaningScoped", true, []string{one}, false,
			func(s *searchStatements) string { return s.meaningScoped }},
		{"searchQueryMeaningJoined", true, []string{every, one}, true,
			func(s *searchStatements) string { return s.meaningJoined }},
	}
	for index, kind := range searchResultsKinds {
		t.Run(kind.word, func(t *testing.T) {
			vectors, err := fixture.embedder.Embed(ctx, []string{kind.word}, embed.InputQuery)
			if err != nil {
				t.Fatalf("embed %q: %v", kind.word, err)
			}
			oldest := kind.kind + "/" + fixture.oldest[kind.kind]
			filters := append([]searchFilter{
				{"no filter", nil, true, totalEvery},
				{"status todo", func(p *searchParameters) { p.status = "todo" }, true, totalOpen},
				{"author alice", func(p *searchParameters) { p.author = "alice" }, true, totalOpen},
				{"updated since", func(p *searchParameters) { p.since = &fixture.since }, false, totalOpen},
			}, searchKindFilters(index)...)
			for _, statement := range statements {
				for _, project := range statement.projects {
					for _, filter := range filters {
						offsets := []int{0, 50}
						if filter.total == totalNone {
							offsets = offsets[:1]
						}
						for _, offset := range offsets {
							scope := "every project"
							if project != every {
								scope = project
							}
							t.Run(fmt.Sprintf("%s, %s, %s, offset %d", statement.name, scope, filter.name, offset), func(t *testing.T) {
								parameters := searchParameters{text: kind.word, project: project, limit: searchDefaultLimit, offset: offset}
								if filter.narrow != nil {
									filter.narrow(&parameters)
								}
								arguments, settings := searchQueryArguments(parameters), searchScanSettings(searchKeywordScan)
								if statement.meaning {
									arguments = searchMeaningQueryArguments(parameters, vectors[0], fixture.embedder.Identity())
									settings = searchScanSettings(searchMeaningScan)
								}
								if statement.generic && project != every {
									settings = append(settings, searchGenericPlanSetting)
								}
								live := statement.selectText(parameters.kinds.statements())
								main := statement.selectText(mainAt4975Statements(uint8(parameters.kinds)))
								want := readStatementAnswer(t, ctx, fixture.database, main, arguments, settings)
								got := readStatementAnswer(t, ctx, fixture.database, live, arguments, settings)
								if !slices.Equal(got.rows, want.rows) {
									t.Fatalf("this build's statement answered\n%s\nwant main's\n%s",
										strings.Join(got.rows, "\n"), strings.Join(want.rows, "\n"))
								}
								switch filter.total {
								case totalEvery:
									matches := int64(searchResultsItems)
									if project != every {
										matches /= 2
									}
									if got.total != matches {
										t.Fatalf("total = %d, want every one of the %d matches of %q", got.total, matches, kind.word)
									}
								case totalNone:
									if got.total != 0 || got.reachable != 0 || len(got.items) != 0 {
										t.Fatalf("a search naming no kind that holds %q answered total %d, reachable %d and items %v, want none:\n%s",
											kind.word, got.total, got.reachable, got.items, strings.Join(got.rows, "\n"))
									}
								}
								if offset == 0 && filter.keepsOldest && !slices.Contains(got.items, oldest) {
									t.Fatalf("the first page holds no %s, the oldest item and the best match:\n%s", oldest, strings.Join(got.rows, "\n"))
								}
							})
						}
					}
				}
			}
		})
	}
}

// TestASearchAtACutAnswersMainsRowsOrderTotalAndReachable holds R3 at a cut: each of search's two
// statements at a cut, searchAsOfQuery and searchAsOfMeaningQuery, built for a search that ranks
// each post kind first and for one that reads them whole, answers the rows, the order, the total
// and the reachable main's at 588af9ce answer (mainAt588aAsOfStatement), for each set of kinds a
// filter row names, over the same arguments, at the same cut and under the settings the route
// runs them under (searchScanSettings over searchKeywordScan or searchMeaningScan), the 15-second
// bound among them, each sent in an Exec of its own as beginSearchScan sends them at a cut.
//
// The corpus is written through the routes, so each item has the events the keyword table and
// the shadows read at a cut: an issue whose title and spec hold the word, two comments, a message
// and an ask that hold it, and a window of each comment and of the message embedded before the
// cut. After the cut one comment is edited to drop the word and another is written with it, and
// the keyword table is filled past the cut. A statement that ranks first serves only a search that
// narrows by no status, author or time, so it runs with no filter, with a project and with each
// kinds row; one that reads whole runs with every filter row. Each runs for two pages: the first
// of the default limit, and two rows from the second. The unfiltered first page of each must hold
// the comment edited after the cut, by the word it held then, and not the comment written after
// the cut, so each pair compared is of statements that read the cut.
func TestASearchAtACutAnswersMainsRowsOrderTotalAndReachable(t *testing.T) {
	t.Parallel()
	const word, project = "wombat", "ORACUT"
	w := newSearchAtCutWorld(t, project, "Wombat census", "The wombat count for the season.")
	w.embedder.vectors[word] = queryVector()
	kept := w.comment(t, "A wombat sighting at the creek.")
	edited := w.comment(t, "Two wombat burrows by the fence.")
	message := w.message(t, "The wombat survey starts at dawn.")
	createIssueAsk(t, w.handler, w.issue, "Count the wombat burrows again?")
	w.embedPost(t, "comment", kept, angledVector(0.9, 2))
	w.embedPost(t, "comment", edited, angledVector(0.85, 3))
	w.embedPost(t, "message", message, angledVector(0.8, 4))
	processAllPending(t, w.database, w.embedder)
	cut := w.cut(t)
	time.Sleep(5 * time.Millisecond)
	w.edit(t, "/api/v1/comments/"+edited, "Two burrows by the fence.")
	late := w.comment(t, "A late wombat sighting.")
	processAllPending(t, w.database, w.embedder)
	w.fill(t, cut)

	ctx := asof.WithCut(context.Background(), cut)
	versions := store.TextVersions{Cut: cut.Time()}
	vectors, err := w.embedder.Embed(context.Background(), []string{word}, embed.InputQuery)
	if err != nil {
		t.Fatalf("embed %q: %v", word, err)
	}
	statements := []struct {
		name                string
		meaning, ranksFirst bool
		main                string
	}{
		{"searchAsOfQuery ranking first", false, true, mainAt588aAsOfKeywordRanksFirst},
		{"searchAsOfQuery reading whole", false, false, mainAt588aAsOfKeywordReadWhole},
		{"searchAsOfMeaningQuery ranking first", true, true, mainAt588aAsOfMeaningRanksFirst},
		{"searchAsOfMeaningQuery reading whole", true, false, mainAt588aAsOfMeaningReadWhole},
	}
	since := cut.Time().Add(-time.Hour)
	type cutFilter struct {
		name     string
		narrow   func(*searchParameters)
		filtered bool
	}
	filters := []cutFilter{
		{"no filter", nil, false},
		{"project " + project, func(p *searchParameters) { p.project = project }, false},
		{"status todo", func(p *searchParameters) { p.status = "todo" }, true},
		{"author alice", func(p *searchParameters) { p.author = "alice" }, true},
		{"updated since an hour before the cut", func(p *searchParameters) { p.since = &since }, true},
		{"kinds comment and message", func(p *searchParameters) { p.kinds = searchNoKind &^ (1<<2 | 1<<4) }, false},
	}
	for index, kind := range searchIndexedKinds {
		filters = append(filters, cutFilter{"kind " + kind, func(p *searchParameters) { p.kinds = searchNoKind &^ (1 << index) }, false})
	}
	pages := []struct{ limit, offset int }{{searchDefaultLimit, 0}, {2, 1}}
	for _, statement := range statements {
		for _, filter := range filters {
			if statement.ranksFirst && filter.filtered {
				continue
			}
			for _, page := range pages {
				t.Run(fmt.Sprintf("%s, %s, limit %d offset %d", statement.name, filter.name, page.limit, page.offset), func(t *testing.T) {
					parameters := searchParameters{text: word, limit: page.limit, offset: page.offset}
					if filter.narrow != nil {
						filter.narrow(&parameters)
					}
					arguments, settings := searchQueryArguments(parameters), searchScanSettings(searchKeywordScan)
					live := searchAsOfQuery(versions, statement.ranksFirst, parameters.kinds.exclusion())
					if statement.meaning {
						arguments = searchMeaningQueryArguments(parameters, vectors[0], w.embedder.Identity())
						settings = searchScanSettings(searchMeaningScan)
						live = searchAsOfMeaningQuery(versions, statement.ranksFirst, parameters.kinds.exclusion())
					}
					main := mainAt588aAsOfStatement(statement.main, statement.meaning, cut.Time(), uint8(parameters.kinds))
					want := readStatementAnswer(t, ctx, w.database, main, arguments, settings)
					got := readStatementAnswer(t, ctx, w.database, live, arguments, settings)
					if !slices.Equal(got.rows, want.rows) {
						t.Fatalf("this build's statement at the cut answered\n%s\nwant main's\n%s",
							strings.Join(got.rows, "\n"), strings.Join(want.rows, "\n"))
					}
					if filter.narrow == nil && page.offset == 0 &&
						(!slices.Contains(got.items, "comment/"+edited) || slices.Contains(got.items, "comment/"+late)) {
						t.Fatalf("the unfiltered first page at the cut holds %v, want the comment %s edited after the cut and not "+
							"the comment %s written after it", got.items, edited, late)
					}
				})
			}
		}
	}
}
