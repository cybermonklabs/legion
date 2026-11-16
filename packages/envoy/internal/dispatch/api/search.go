package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const (
	searchDefaultLimit = 20
	searchMaxLimit     = 50
	// searchFusionK is the fusion constant: a row's legs contribute their own weighted scores,
	// and their sum is divided by searchFusionK + 1 - the score reciprocal rank fusion gave the
	// first row of a list, 1/61 - so the keyword leg's rank and the meaning leg's cosine, each on
	// a scale of its own and weighted below, are softened onto that one scale, the scale every
	// result's rank has been published on since LEGION-386: a row one leg found at full strength
	// still scores about 1/61, as the best row of a list did. 60 is the value of the paper that
	// introduced the method (Cormack, Clarke and Buettcher, 2009) and Elasticsearch's and
	// OpenSearch's default; nothing tunes it.
	searchFusionK = 60
	// searchKeywordWeight is what a keyword hit's own rank is worth in the fused score, and the
	// unit searchMeaningWeight is written in: 1, so a keyword-only search ranks by the row's
	// length-normalised ts_rank_cd (searchKeywordLegsSQL) and nothing else.
	searchKeywordWeight = 1.0
	// searchMeaningWeight is what a meaning hit's cosine similarity is worth beside it. The two
	// legs score on different scales, and 0.4 is what lines them up on the corpus
	// searchMeaningFloor was derived from: ts_rank_cd with length normalisation scores a title
	// hit 0.5, an issue matched by its spec alone 0.167 and a one-word comment or message body
	// 0.091 (measured on this corpus's own lexeme weights, A 1.0, C 0.2, D 0.1), while a meaning
	// hit's cosine runs 0.45-0.50 for a true paraphrase and 0.9 for a near-copy. At 0.4 a
	// near-copy (0.36) still sits below a title hit, a true paraphrase (0.20) above an issue
	// found by its spec alone, and a hit at the floor (0.10) beside the weakest keyword hit
	// there is, so meaning fills what the words miss without displacing an exact match.
	searchMeaningWeight = 0.4
	// searchOwnKeyLead is what q.own_key is worth: an issue whose key is the whole query leads
	// the page, since ts_rank_cd scores its own key no higher than the title of another issue
	// citing it (TestSearchListsAnIssueFirstForItsOwnKey). 1 is above every score the legs can
	// reach together, (searchKeywordWeight + searchMeaningWeight) / (searchFusionK + 1), about
	// 0.023, so the lead is absolute rather than one more term to be outweighed.
	searchOwnKeyLead = 1.0
	// searchBroadenWords is how many of a query's own words the fallback searches for when the
	// query as typed answers nothing at all (searchRarestWords): its rarest words, and no more of
	// them. It is no longer a length threshold - a query of any length is answered by
	// websearch_to_tsquery's AND of its words for as long as that AND matches anything (runSearch)
	// - but the bound survives on the fallback, for the reason the number was chosen: a person
	// naming a thing writes two to five words ("astrolabe", "search ranking fusion"), so the five
	// rarest words of a sentence are the thing it is about, and the words past them are what a row
	// holding nothing of its subject still matches. ORing all of one ask's own thirty words
	// matched 3,424 asks, 36,059 comments and 3,710 issues of a corpus of 79,555 items where the
	// AND of them matched the one ask asked for, and no leg being read deeper than
	// contracts.SearchKindDepth, the right row never reached the page at all.
	searchBroadenWords = 5
	// searchAuthorMax bounds the author filter's value, in UTF-16 units as every other cap on
	// this route counts: 320 is the longest address RFC 5321 allows, and an author is a person's
	// login (an address) or a session's id, which is shorter.
	searchAuthorMax = 320
	// searchEmbedTimeout bounds the one model-service call search makes (the query's own embedding).
	// Past it, or any other embedder failure, search answers keyword-only and says so (LEGION-549)
	// rather than making every search wait indefinitely on a degraded embedder.
	searchEmbedTimeout = 3 * time.Second
	// searchMeaningFloor is the lowest cosine similarity a meaning-leg row may score to reach legs
	// at all: below it, a row is noise, not a weak match, and letting it through would put a row
	// the query means nothing to on the page, where every row is read as an answer, and give it
	// searchMeaningWeight's share of a score besides. Chosen from embed-v4 pairs on
	// this corpus: true paraphrase matches scored 0.45-0.50, same-domain-different-issue
	// near-misses 0.34-0.40, and unrelated content topped out at 0.18 (one query, eight unrelated
	// documents) - 0.25 sits with a wide margin on both sides of that gap. It is not re-derived
	// per request or per corpus; a corpus whose true matches cluster lower would need a new floor
	// chosen the same way.
	searchMeaningFloor = 0.25
	markStart          = "\uE000" // private-use sentinels: ts_headline writes them, markSnippet turns them into <mark>
	markEnd            = "\uE001"
	headlineOptions    = "StartSel=" + markStart + ", StopSel=" + markEnd + ", MaxWords=24, MinWords=12, MaxFragments=1"
	// searchWindowCandidates is how many nearest windows one window arm takes before its
	// `distinct on (id)` keeps the nearest window of each item. Every arm is held to
	// contracts.SearchKindDepth items afterwards (ranked), but a window is not an item: one
	// long document holds hundreds of them, and at a candidate depth of SearchKindDepth its
	// windows alone fill the arm, which then yields that one document and every other document
	// the query matches is lost. Ten times the depth leaves room for a thousand windows, which
	// is the one document's hundreds and the rest of the page's items beside them, and the arm
	// still reads them through embeddings_cosine, nearest first.
	searchWindowCandidates = 10 * contracts.SearchKindDepth
)

// searchKeywordLegs is the five-kind keyword-ranked union - issue, document, comment, ask,
// message, each matched against q.tsq and ranked by rank of its own search column - shared
// verbatim by searchQueryBody's "kinds" CTE and searchQueryMeaningBody's "kinds" CTE, where it is
// unioned with the five meaning_* CTEs below it - this, searchFusedSQL and searchPageAndTailSQL
// are the whole of the duplication between the two queries. rank is search's ts_rank_cd
// (searchKeywordLegsSQL) or the related-item check's coordination rank
// (relatedCoordinationRank). Every arm carries 'keyword' as its own list column, which the shared
// fused CTE reads to weigh this leg's score against the meaning leg's; in the keyword-only query
// it is the only value the column ever takes. Every arm carries q's three filters
// (searchRowFilters) in the arm itself, so a filtered search reads its own matches as deep as
// any other search reads, rather than a page of unfiltered ones with the rest struck out.
//
// The issue arm is two arms, not one: the issues q.tsq finds, and the issues the query names by
// key (q.own_keys, searchOwnIssueKeys). A key is indexed with its project ('infra' <-> '-1042'),
// so the number alone, or a space where the hyphen is, matches no issue's vector, and the issue
// a person asked for would be absent from the answer rather than first in it - but the two are
// different access paths, and one predicate ORing them together is never an index condition on
// issues_search: it is a join filter over a sequential scan of issues, or at best the recheck
// of a BitmapOr that re-tests the vector of every candidate row on the heap. Either way the arm
// pays for the whole row, which an issue's own spec in issues.search made expensive (migration
// 20261105145242_issues_search_spec): 8.5 kB a row against 190 bytes, 36 MB of out-of-line
// values the planner prices at nothing (INFRA-1042). Split, the vector match is
// `i.search @@ q.tsq` alone, the shape the comment, ask, message and document arms already take,
// and the planner reads issues_search for it. Measured on a fixture of production's size with
// the parameters bound as the route binds them (explain (analyze, buffers) of a generic plan):
// the OR takes a BitmapOr of issues_search and issues_pkey whose recheck is the OR itself, 2.4
// and 6.0 ms over 32 and 147 buffers for two queries; the two arms take one bitmap index scan
// of issues_search whose condition is the match alone, 0.2 and 1.1 ms over 10 and 40 buffers,
// with the key arm never executed for a query that names no issue.
//
// The two arms are disjoint, so the union is `union all` and no issue is listed twice: the
// vector arm leaves out every issue the key arm answers (`not (i.key = any(q.own_keys))`), and
// an issue matched both ways comes from the key arm alone, with the own flag and the ts_rank_cd
// rank it had when one arm carried both. own is therefore a constant per arm - false on the
// vector arm, true on the key arm - rather than a test. The key arm reads the keys themselves
// (unnest) and joins issues on its primary key, so a search naming no issue walks no row of
// issues at all and one naming an issue reads that issue by key. Both arms carry q's project
// narrowing and q's three filters, as the one arm did.
//
// Neither arm reads q.own_keys as anything but an array: it is never null (searchOwnIssueKeys
// answers '{}', relatedQOpener binds '{}'), which `not (... = any(...))` and unnest both need.
//
// Column shape after kind, list: issue_key, owner_artifact_id, artifact_id, id, block_id,
// window_id, covers, issue_title, issue_status, owner_project, owner_slug, owner_name,
// updated_at, own, r. block_id is an ask's typed block only while it is a node of the reference
// graph (askBlockNodeSQL). window_id and covers are the embeddings window a meaning arm matched
// and the blocks its vector covers; a keyword arm matched no window and carries null for both,
// as it carries 'keyword' as list, so the two unions have one column shape. The ask arm's anchor
// cast is store.CastableUUID's, which answers the document every spelling of an artifact_id
// Postgres casts names.
//
// The document arm is a named piece of its own (searchDocumentKeywordLeg), so that it can be
// ranked, read and measured apart from the four arms beside it.
func searchKeywordLegs(rank func(searchColumn string) string) string {
	return `
  select 'issue' as kind, 'keyword' as list, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
         null::text as window_id, null::text[] as covers,
         i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
         false as own, ` + rank("i.search") + ` as r
    from issues i, q
   where i.search @@ q.tsq and not (i.key = any(q.own_keys)) and ($2 = '' or i.project_key = $2)` +
		searchRowFilters("i.status", "i.created_by", "i.updated_at") + `
  union all
  select 'issue', 'keyword', i.key, null::uuid, null::uuid, i.key, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         true, ` + rank("i.search") + `
    from q, unnest(q.own_keys) as named(key), issues i
   where i.key = named.key and ($2 = '' or i.project_key = $2)` +
		searchRowFilters("i.status", "i.created_by", "i.updated_at") + `
  union all
` + searchDocumentKeywordLeg(rank) + `
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
   where c.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)` +
		searchRowFilters("i.status", "c.author", "c.created_at") + `
  union all
  select 'ask' as kind, 'keyword', k.issue_key, case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) else null::uuid end,
         coalesce(k.artifact_id, k.block_artifact_id, ` + store.CastableUUID(`k.anchor->>'artifact_id'`) + `), k.id::text, ` + askBlockNodeSQL + `,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name, coalesce(i.updated_at, k.created_at),
         false, ` + rank("k.search") + `
    from asks k
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where k.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)` +
		searchRowFilters("i.status", "k.author", "k.created_at") + `
  union all
  select 'message' as kind, 'keyword', m.issue_key, null::uuid, null::uuid, m.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         false, ` + rank("m.search") + `
    from messages m join issues i on i.key = m.issue_key, q
   where m.search @@ q.tsq and ($2 = '' or i.project_key = $2)` +
		searchRowFilters("i.status", "m.author", "m.created_at")
}

// searchRowFilters is the status, author and date filter every leg of every search narrows by,
// read from q (searchQOpener) rather than bound into each arm, so the related-item check's own q
// names them once as no filter at all. Each is a no-op while its own value is empty.
//
// status is the issue status the row belongs to: a row whose kind has no issue - a project
// document, and a comment or ask on one - matches no status at all, since the filter asks for
// the rows of the issues in that status and such a row belongs to none. author is the jsonb
// actor that wrote the item the row names, matched on its id, which is a person's login or a
// session's id; every kind has one, an issue and a document their creator. written is when the
// item itself was last written, which the date filter reads in place of the recency the row is
// ordered by: an issue's updated_at moves whenever anything on it moves, and a comment written
// last year on an issue touched today is not a comment from today.
func searchRowFilters(status, author, written string) string {
	return `
     and (q.status_filter = '' or ` + status + ` = q.status_filter)
     and (q.author_filter = '' or ` + author + `->>'id' = q.author_filter)
     and (q.updated_since is null or ` + written + ` >= q.updated_since)`
}

// searchDocumentKeywordLeg is searchKeywordLegs' document arm, written apart from the four arms
// beside it so that it can be ranked, read and measured on its own: it is a small part of the
// cost of every statement that unions it, and a cost that small is legible only when the arm is
// read apart from theirs.
//
// It reads document_search (migration 20261101182447), one row per document holding its latest
// version's vector under a GIN index, rather than a lateral over artifact_versions: that table's
// search column has no index and can have none (0010 left it out, and its 5.2 GiB is above what
// the pre-deploy census lets a migration touch), so an arm reading it tests every document's
// latest version in turn. The rows are the same ones - 20261101182454's trigger holds
// document_search at each document's latest version with markdown - and the join to artifacts
// decides the arm's project, kind and issue.
//
// The text carries no newline of its own at either end: searchKeywordLegs joins it to its
// neighbours with the union the arms are separated by, and a caller running the arm by itself
// wraps it in a select of its own.
func searchDocumentKeywordLeg(rank func(searchColumn string) string) string {
	return `  select 'document' as kind, 'keyword', a.issue_key, case when a.issue_key is null then a.id else null::uuid end, a.id, a.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, v.created_at),
         false, ` + rank("v.search") + `
    from document_search v
    join artifacts a on a.id = v.artifact_id
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or p.key = $2)` +
		searchRowFilters("i.status", "a.created_by", "v.created_at")
}

// searchKeywordLegsSQL is search's own keyword legs, each arm ranked by ts_rank_cd with length
// normalisation.
var searchKeywordLegsSQL = searchKeywordLegs(func(searchColumn string) string {
	// 32 is ts_rank_cd's "divide the rank by itself plus one" normalisation flag: the raw rank
	// grows with every further matching lexeme a text holds, and dividing it by itself plus one
	// maps it onto [0, 1), so a long document no longer scores above a short issue merely by
	// holding more of the query's lexemes, and the one score every kind is now ranked on
	// (searchFusedSQL) can weigh a rank against a cosine similarity. The related-item check's own
	// rank already passes it (relatedCoordinationRank).
	return "ts_rank_cd(" + searchColumn + ", q.tsq, 32)"
})

// askBlockNodeSQL is the block id of ask k's typed block while that block is a node of the
// reference graph (a document_blocks row of the document holding it), and null otherwise: an id
// past text.MaxBlockIDBytes is stored but never a node, and a later edit can remove the block.
// searchHref links an ask's block only when this is set, since a reader of the `#b-` link reads
// the block's node, and links the ask itself (`?ask=`) when it is not. Search's two ask arms -
// the keyword one above and meaning_ask_candidates - and the related-item check's cross-project
// leg (relatedOtherProjectsSQL) all select it.
const askBlockNodeSQL = `case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end`

// searchFusedSQL is the one score every match of GET /api/v1/search is ranked by, which
// searchQuery and searchQueryMeaning each pass their body as its "fused" CTE: one scored list
// across every kind, rather than five lists of their own merged by the position a row holds in
// its kind. Each leg contributes its own score - the keyword leg's length-normalised ts_rank_cd
// (searchKeywordLegsSQL), the meaning leg's cosine similarity - weighted onto the one scale the
// two can be added on (searchKeywordWeight, searchMeaningWeight), the sum divided by
// searchFusionK + 1. Grouping by
// (kind, id) is what makes a row both legs found one row scoring the sum of both, which is
// strictly above what either leg alone gives it, since every leg's own term is positive (a
// matched tsquery ranks above zero, and a meaning row below searchMeaningFloor never reaches
// legs). No kind is ranked ahead of another here or in the page, which orders by this score
// alone, then recency, then id (searchPageAndTailSQL); what position still decides is depth, how
// many of a leg's rows reach legs at all ($7, contracts.SearchKindDepth), and nothing else. An
// issue the query names by key leads the page by searchOwnKeyLead, read from the own column the
// issue arm carries (q.own_keys), and own_lead carries that fact out of the score so the page can
// order such issues among themselves by their key, which for one number is their project's order
// (searchPageAndTailSQL).
//
// The keyword-only body has one list, so its 'meaning' arm never fires there; the one text is
// passed to both bodies rather than kept as two near-identical copies, as the keyword legs
// themselves are. The related-item check passes a fused CTE of its own (relatedFusedSQL) in
// place of this one; its comment says why this weighted sum cannot rank that check's two legs.
var searchFusedSQL = fmt.Sprintf(`fused as (
  select kind, id, bool_or(own) as own_lead,
         sum(case list when 'keyword' then %[1]g * r else %[2]g * r end) / ($8 + 1)
           + case when bool_or(own) then %[3]g else 0 end as score
    from legs group by kind, id
),
`, searchKeywordWeight, searchMeaningWeight, searchOwnKeyLead)

// windowOwnerUUID and windowItemUUID read the item a window belongs to out of the window's own
// id, "<owner key>#<the first block it covers>": the owner key is the id up to the first #, and
// it is the artifact id for a document's window and "<kind>/<uuid>" for a comment's, a message's
// or an ask's (refs.ItemKind.WindowOwnerKey). Each meaning arm joins its item on that item's
// primary key through one of these, so the planner reaches the row through the key rather than
// reading the whole table to compare a text it computes per row. The cast is guarded
// (store.CanonicalUUID) so a window id in any other shape resolves to no item instead of
// answering SQLSTATE 22P02 for the whole search.
var (
	windowOwnerUUID = store.CanonicalUUID(`split_part(e.id, '#', 1)`)
	windowItemUUID  = store.CanonicalUUID(`split_part(split_part(e.id, '#', 1), '/', 2)`)
)

// searchPageAndTailSQL is the "page the fused ranking, then stitch back the snippet" tail shared
// by searchQuery and searchQueryMeaning - the other half of the LEGION-549 round-5 dedup above:
// identical but for legsSource, the name of the CTE each query's own ranked/fused machinery hands
// it - searchQuery's single-list legs, or searchQueryMeaning's legs_unique (one representative
// row per (kind, id) after scoring the keyword and the meaning list together, searchFusedSQL).
// legsSource is always one of those two Go identifiers below, never caller input, so there is no
// SQL injection surface in formatting it directly into the query text.
//
// The order is the fused score, then the key of an issue the query named (own_lead), then
// recency, then id, and no kind is named in it: a kind leads only by scoring highest. id breaks
// what is left, so the order is total and consecutive offsets walk the reachable rows once while
// the corpus holds still. The own_lead term orders nothing but those issues: for every other row
// it is null, and every issue it does order already stands above them by searchOwnKeyLead. A
// number alone names one issue of each project (searchOwnIssueKeys), and their ids are their
// keys, so ordering them by id is ordering them by project key.
//
// The page also names the block a meaning hit matched (hit.block_id, INFRA-370). A meaning leg
// matched one window of an owner's blocks, and that window covers a run of them (r.covers); the
// block named is the covered block whose own text ranks highest for the query by ts_rank_cd over
// the configuration the keyword legs are built with ('english' over search_text), ties broken by
// the block's place in the window, and so the window's first block when no covered block shares a
// term with the query. A search for the text of a one-line ask option therefore names the option,
// not the question that gives the option its context and stands in the same window. The rank runs
// in the page's own lateral join, over the rows of one page and the few blocks each window covers,
// never over every candidate; a keyword row and an issue carry no covers, so the join answers
// nothing for them.
func searchPageAndTailSQL(legsSource string) string {
	return fmt.Sprintf(`page as (
  select l.*, f.score, f.own_lead
    from %s l join fused f using (kind, id)
   order by f.score desc, case when f.own_lead then l.id end, l.updated_at desc, l.id
   limit $3 offset $6
)
select t.total, t.reachable, r.kind, case when r.owner_artifact_id is null then 'issue' else 'document' end,
       r.issue_key, r.issue_title, r.issue_status,
       r.owner_project, r.owner_slug, r.owner_artifact_id::text, r.owner_name,
       ar.slug, ar.name, coalesce(ar.is_primary, false), r.id, r.block_id, hit.block_id, r.score::float8,
       ts_headline('english',
         search_text(case when q.term <> '' and strpos(lower(txt.text), lower(q.term)) > 0
              then substr(txt.text, greatest(1, strpos(lower(txt.text), lower(q.term)) - 1500), 4000)
              else left(txt.text, 4000) end),
         q.tsq, $5) as headline
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
  -- The block of r.covers that matched, if the row carries a window. Each covered block's own
  -- text comes from the table its indexer writes it to: a document's, a comment's and an ask's
  -- block texts are kept per version or revision, the latest being the current text. A message
  -- is never edited and keeps no text table (refs/item_blocks.go's table map), so its block is
  -- ranked on the excerpt its row holds, the block's first refs.BlockExcerptRunes runes.
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
     order by ts_rank_cd(to_tsvector('english', search_text(coalesce(block.text, ''))), q.tsq) desc, covered.ord
     limit 1
  ) hit on true
 order by r.score desc, case when r.own_lead then r.id end, r.updated_at desc, r.id
`, legsSource)
}

// searchQuery scores every kind of content on one list, so a page holds the query's best matches
// whatever kind they are: kinds unions the five kinds' matches (searchKeywordLegsSQL) with no
// per-kind limit or order of its own; one window over it, partitioned by kind, orders each
// kind's matches by own, then ts_rank_cd, then recency, then id, and numbers them (pos), and the
// same partition's count(*), taken before any cut, is that kind's every match (matches).
// Filtering to pos <= $7 (contracts.SearchKindDepth) then keeps each kind's best $7 as legs,
// which is how deep a kind is read and no longer where it is ranked: fused (searchFusedSQL)
// scores what survives by each row's own rank, and the page orders by that score, then the key of
// an issue the query named (own_lead, which orders nothing else), then recency, then id, which
// makes the order total, so consecutive offsets cover the reachable rows once
// while the corpus holds still. A kind's one weak match therefore no longer outranks the second
// of fifty strong ones in another kind, and no kind leads a tie by being named first. An issue
// the query names by key leads the page (searchOwnKeyLead), since ts_rank_cd scores its own key
// no higher than the title of another issue citing it. legs and fused carry no row's text, so
// the union, the window and the fused score cost nothing per kind's full body; only the page's
// own rows pay for one lateral fetch of their text and, after that, a snippet. totals is joined
// outside the page, so a page past the end still answers how many rows the query matches. This is
// the keyword-only fallback (LEGION-549 degraded mode); the related-item check runs its body,
// with its keyword arms ranked by relatedCoordinationRank, over its own q and with the owners it
// leaves out filtered out of kinds (searchKindsCTE).
//
// Parameters: $1 q, $2 project (empty for every project), $3 limit, $4 firstTerm,
// $5 headlineOptions, $6 offset, $7 contracts.SearchKindDepth, $8 searchFusionK, then
// searchQOpener's own six from $9: $9 the fallback's rarest words, $10 the words it excludes,
// $11 the issues the query names by key, $12 status, $13 author, $14 updated_since
// (searchQueryArguments).
var searchQuery = searchQOpener(9) + searchQueryBody(searchKeywordLegsSQL, searchFusedSQL, "")

// searchQOpener opens searchQuery and searchQueryMeaning: their q, the query text read as
// websearch_to_tsquery, its first term ($4) for the snippet window, the issues the query names by
// key (own_keys, searchOwnIssueKeys) and the three filters every leg narrows by
// (searchRowFilters). base is the first parameter index of the six values the opener itself
// binds, which differs between the two statements because the keyword body's own parameters end
// at $8 and the meaning body's at $11. The related-item check prefixes the same two bodies with
// its own q (relatedQOpener), which names the same columns with no key and no filter, their
// keyword legs ranked by its idf-weighted coordination rank (relatedCoordinationRank); the rest
// of each body, the fused score and the page, is search's own.
//
// tsq is websearch_to_tsquery's own reading of the text - the phrase, OR and -word forms a person
// already has - and that is what every search runs, whatever its length. The fallback runs this
// same statement a second time, and only for a query whose first run matched nothing at all
// (runSearch), with the query's rarest words bound at $base (searchRarestWords): tsq is then the
// OR of those words alone, never OR-ed onto the AND that has just matched nothing, since the
// AND's own lexemes would otherwise be counted a second time by ts_rank_cd and rank the
// fallback's rows by the words it dropped. The words the query excludes are ANDed onto that OR
// ($base+1), since ORing the words back in without them would return the rows a -word left out;
// a query that excludes nothing binds the empty string and takes the shorter form.
func searchQOpener(base int) string {
	return fmt.Sprintf(`
with q as (select
         case when $%[1]d = '' then websearch_to_tsquery('english', $1)
              when $%[2]d = '' then websearch_to_tsquery('english', $%[1]d)
              else websearch_to_tsquery('english', $%[1]d) && websearch_to_tsquery('english', $%[2]d)
         end as tsq,
         $4::text as term, $%[3]d::text[] as own_keys,
         $%[4]d::text as status_filter, $%[5]d::text as author_filter, $%[6]d::timestamptz as updated_since),
`, base, base+1, base+2, base+3, base+4, base+5)
}

// searchKindsCTE is the kinds CTE both bodies open with: the arms whose rows the query ranks,
// with every row exclude names dropped before anything ranks, counts or pages them. exclude is
// an SQL predicate over the arms' own columns, true of a row the caller leaves out, and empty
// for a query that leaves nothing out, whose text is then the arms as they stand. Search leaves
// nothing out; the related-item check passes relatedExcludedOwners, because the page it takes is
// ten rows deep (suggestionSearchDepth) and rows dropped after that page took places in it and
// hid the matches the check exists to find (INFRA-1160).
//
// The arms are a union whose first select names every column, so the derived table the filter
// reads carries those names on to ranked below unchanged.
func searchKindsCTE(arms, exclude string) string {
	if exclude == "" {
		return `kinds as (` + arms + `
),
`
	}
	return `kinds as (select * from (` + arms + `
) arms where not (` + exclude + `)
),
`
}

// searchQueryBody is searchQuery after its q, over keywordLegs (searchKeywordLegs), fused (the
// "fused" CTE scoring what the legs matched, searchFusedSQL or relatedFusedSQL) and without or
// with an exclusion (searchKindsCTE): every CTE it reads q from and the page.
func searchQueryBody(keywordLegs, fused, exclude string) string {
	return searchKindsCTE(keywordLegs, exclude) + `ranked as (
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
` + searchPageAndTailSQL("legs")
}

// searchWindowLimit is the LIMIT a window arm's candidate CTE takes, searchWindowCandidates as
// SQL. It is written into the statement rather than bound, so that the two queries built from
// searchQueryMeaningBody keep the parameters they have: the number is this build's own, never a
// caller's.
var searchWindowLimit = "limit " + strconv.Itoa(searchWindowCandidates)

// searchMeaningScan is what a meaning query's transaction sets before it runs, every statement
// local to that transaction and no other:
//
//   - statement_timeout: the only bound the server itself holds on a meaning query, 15 seconds.
//     The request's own context does reach the query - runMeaningSearch and relatedMeaningSearch
//     each pass it to tx.Query - but pgx cancels out of band: a CancelRequest is a new connection
//     of its own, and under saturation, which is exactly when meaning searches pile up, there is
//     none to open, so the cancellation never arrives and a search whose caller has gone keeps
//     running. One measured 60-minute step of INFRA-988's load test left 219 backends on
//     searchQueryMeaning, 5,049 seconds old on average, still running an hour after the load had
//     stopped, holding the connection pool full and 15 GiB of temporary files; cancelling them
//     by hand took that deployment from 221 active backends to 2 and released the 15 GiB. The
//     bound sits between the two numbers it has to separate: a healthy meaning search, under a
//     second on production's data, and the 30-second deadline the generator and Dispatch's own
//     clients give a request. So the server gives up before the client does, rather than an hour
//     after, and the backend goes with the statement. A statement ended that way is SQLSTATE
//     57014 (meaningStatementEnded), which each caller reports as its own timeout.
//   - hnsw.ef_search: how many nearest vectors the HNSW index walks before the kind, project
//     and model filters run. pgvector's default, 40, is spent on whichever kind holds the most
//     vectors - the document windows, tens of thousands of them against one row per issue - so
//     the issue and message arms are left with few candidates or none, and the project filter
//     can take the rest. 400 keeps them, at a wider walk of the index paid once per query.
//   - hnsw.iterative_scan: with it off, an arm whose kind filter rejects every one of those 400
//     answers nothing at all; relaxed_order lets the scan carry on past them instead of stopping
//     there. It does not promise the arm's LIMIT: measured on the load-test corpus, an arm forced
//     through embeddings_cosine over 1,104 qualifying rows took 245 of them, and that number did
//     not move at ef_search 40, 400 or 1,000, with iterative_scan relaxed_order or off, at
//     work_mem 256MB or hnsw.scan_mem_multiplier 16 - so neither the LIMIT nor
//     hnsw.max_scan_tuples (20,000) was what ended the walk, and how deep an arm reaches is the
//     index's own business (INFRA-988). That corpus's vectors are near-duplicates of each other,
//     the worst case for an HNSW walk's reach, so how much of the shortfall a corpus of real text
//     would see is an inference from it rather than a measurement of it. Relaxed rather than
//     strict: every arm re-ranks its rows by r (distinct on, then ranked), so nothing downstream
//     reads the index's order.
//
// Both meaning queries run in a transaction beginMeaningScan opens, and set them there.
var searchMeaningScan = []string{
	searchMeaningStatementBound,
	`set local hnsw.ef_search = 400`,
	`set local hnsw.iterative_scan = relaxed_order`,
}

// searchMeaningStatementBound is the first of those settings. It is named because two readers
// outside the list need the very same string: the measurement of these statements, which has to
// read what a statement costs and so cannot run under a bound that would end it
// (TestStatementTimings), and the test that shortens the bound to prove what each caller reports
// when the server ends a meaning statement (endMeaningStatementsAt).
const searchMeaningStatementBound = `set local statement_timeout = '15s'`

// meaningStatementEnded reports whether err is Postgres ending a meaning statement itself rather
// than answering it: SQLSTATE 57014, query_canceled, which is what searchMeaningStatementBound
// raises when a query outlasts it, and what an operator's pg_cancel_backend raises too. A
// caller's own context ending is not this error - pgx answers that with the context's error - so
// a caller that tells a timeout from a failure has to read the error as well as its context
// (relatedMeaningStage).
func meaningStatementEnded(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}

// beginMeaningScan opens the read transaction a meaning query runs in, searchMeaningScan's
// settings applied to it: `set local` holds every one of them to this transaction, so a pooled
// connection carries none of them - the server's bound on the query included - back to the next
// request. The caller ends the transaction, a rollback deferred, since neither query writes; a
// transaction a setting failed on is rolled back here and never handed back. Search's own query
// (runMeaningSearch) and the related-item check's (relatedMeaningSearch) both start here, so
// neither can read the index on other settings than the other, and neither can outlive the
// bound.
//
// The settings go in one Exec, joined with "; ", rather than one Exec each: they carry no
// argument, so pgx sends them over the simple protocol, which takes a statement list and drains
// every result (pgx v5 conn.go's execSimpleProtocol), and every meaning search and every
// related-item check pays one round trip of setup rather than one per setting. The join is made
// here rather than at package init because a test reassigns searchMeaningScan to shorten the
// bound (endMeaningStatementsAt), and a string built at init would not see it.
//
// A setting Postgres refuses fails the whole Exec, and the Go-side error no longer names which
// of them failed; Postgres's own message still does, since it quotes the parameter it refused.
func (s *server) beginMeaningScan(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, strings.Join(searchMeaningScan, "; ")); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return tx, nil
}

// searchQueryMeaning extends searchQuery with a second list per kind, ranked by cosine distance
// to the query's own embedding ($9, a pgvector literal) against embeddings.embedding
// (0072_embeddings_core.up.sql), rather than ts_rank_cd. Each kind's meaning candidates are their
// own CTE (meaning_issue_candidates and so on) doing plainly `order by embedding <=> $9 limit n`
// - pgvector's HNSW index serves that shape directly (confirmed by
// TestSearchMeaningLegsUseTheHNSWIndexNotASequentialScan) - rather than joining meaning rows into
// one shared window spanning every kind, which would force a sequential scan of embeddings once
// per leg. The vector is the bound parameter itself ($9::vector), never a column of q: the index
// orders by distance only to a value fixed for the whole scan, and a column joined in from q is
// not one, so ordering by it sorts every candidate in memory instead. Each candidate CTE reads
// only vectors whose embeddings.model is $11, the query embedder's own identity
// (embed.Embedder.Identity, INFRA-393): a vector another model or place made lives in a different
// space, and its cosine distance to this query means nothing. The coverage pass (embedqueue.Cover)
// drops such vectors once the configured embedder has answered, so the filter has to catch what a
// differently configured process writes during a roll, and every vector of the identity before
// it while a newly configured embedder has not embedded anything yet. Each candidate set is then
// floor-filtered (searchMeaningFloor) before kinds ever sees it: a candidate below the floor is
// noise, not a weak match, and reaching legs at all would put it on the page as an answer and
// score it as one.
//
// Four of the five arms read windows of neighbouring blocks (INFRA-370: kinds document_block,
// comment_block, message_block and ask_part), where an issue's arm still reads the issue's one
// whole-text row. A window's id is "<owner key>#<suffix>", so the item it belongs to is
// split_part(id, '#', 1) - the artifact id for a document, "<kind>/<uuid>" for a comment, a
// message or an ask - and each arm projects the item's own kind and id, so fusion still ranks
// one row per item however many of its windows the index hands back. A window arm takes
// searchWindowCandidates of them, ten times the items any arm keeps, because one long
// document's windows would otherwise fill an arm of contracts.SearchKindDepth and leave every
// other document of the page out. Each arm then keeps the item's
// nearest window alone (distinct on (id), the highest cosine first) and carries that window's
// id and covers, the blocks its vector holds, on to legs_unique and the page, which names the
// one of them that matched (searchPageAndTailSQL).
//
// The comment arm is a named piece of its own (searchMeaningCommentArm), and it is two arms:
// scoped on the candidate row's own column or unscoped, it takes its windows through the index
// under the kind, model and project filters alone and joins the comment above that limit, where
// the three window arms beside it join inside the ordered select; scoped on the owner it joins
// inside that select as they do, which is the arm the release before this change ran and the
// arm a request narrowing by status, author or updated_since takes (meaningStatementFor).
//
// Every arm's project filter is one predicate on the candidate row itself,
// `e.project_key = $2` (meaningProjectScope), and the statement is composed three times: once
// scoped on that column, with the predicate in each candidate CTE; once scoped on the owner
// table each arm already joins, as every arm found a project before that column existed; and
// once unscoped, with no project predicate in any of them. runMeaningSearch picks between them
// over three things: whether the request narrows by any of q's three filters, whether the caller
// named a project, and whether the fill that labels the stored rows has finished
// (meaningStatementFor). A filtered request takes the joined one whatever the other two say,
// because its comment arm is the only one of the three that applies those filters inside its
// candidate limit. Statements rather than one predicate reading "$2 is empty, or it is the
// row's project", because one generic plan of that OR has to serve both cases and
// can lose the good plan of either: measured on the load-test deployment, the scoped form
// takes a small project's windows through embeddings_kind_project (800 of 800 rows, 13.7 ms)
// and the unscoped form takes every project's through embeddings_cosine (INFRA-988, migration
// 20261109120000's header holds the four measured plans). All three take the same seventeen
// parameters with the same meanings; in the unscoped one $2 is read by the keyword
// legs alone, which keep the project predicate they share with searchQuery.
//
// kinds unions searchKeywordLegs - the same five-arm keyword text searchQuery's own kinds CTE
// uses verbatim, each arm already carrying 'keyword' as list - with the five meaning_* CTEs
// above. Every leg now carries which list it belongs to (list), and ranked's window partitions by
// (kind, list): each list keeps its own top $7, so a row that matches both lists for the same
// (kind, id) occupies two rows of legs - one per list - each carrying its own score, a
// ts_rank_cd rank on one list and a cosine similarity on the other. fused weighs those two scales
// against each other and sums them grouped by (kind, id) (searchFusedSQL): an item only the
// keyword list reached, only the meaning list reached, or both, is ranked once, and an item both
// lists found scores above what either list alone would have given it. legs_unique picks one
// representative row's owner/text-location columns per (kind, id) - both lists' rows carry the
// same ones, read from the same underlying issue/document/comment/ask/message - preferring the
// row that names a window, since only that row knows which blocks the hit covers, before the
// page is cut and joined back to fused's score. total is every distinct (kind, id) either list
// matched, not a sum of list sizes, so an id both lists reach is counted once.
//
// Parameters: $1 q, $2 project, $3 limit, $4 firstTerm, $5 headlineOptions, $6 offset,
// $7 contracts.SearchKindDepth, $8 searchFusionK, $9 the query's embedding (a vector literal),
// $10 searchMeaningFloor, $11 the query embedder's identity, then searchQOpener's own six from
// $12: $12 the fallback's rarest words, $13 the words it excludes, $14 the issues the query names
// by key, $15 status, $16 author, $17 updated_since (searchMeaningQueryArguments). All three
// shapes take all seventeen; searchQueryMeaning reads $2 in its keyword legs alone.
var (
	searchQueryMeaning       = searchQOpener(12) + searchQueryMeaningBody(searchKeywordLegsSQL, searchFusedSQL, "", meaningEveryProject)
	searchQueryMeaningScoped = searchQOpener(12) + searchQueryMeaningBody(searchKeywordLegsSQL, searchFusedSQL, "", meaningOneProject)
	searchQueryMeaningJoined = searchQOpener(12) + searchQueryMeaningBody(searchKeywordLegsSQL, searchFusedSQL, "", meaningOneProjectJoined)
)

// meaningProjectScope is which of the three shapes of the meaning statement is being composed,
// and candidateProject is the whole of the difference: the project predicate each meaning
// candidate CTE carries, on the candidate row's own column (migration 20261109120000), on the
// owner table the arm already joins, or nothing at all. searchQueryMeaningBody's own comment
// argues the shapes; every statement built from it is built three times, search's and the
// related-item check's alike (related_statements.go).
//
// Why the joined shape outlived the column. project_key is nullable, and every row stored
// before 20261109120000 carries null until store.FillEmbeddingProjectKeys labels it - 31 m 41 s
// for 1,109,536 rows on a clone of the load-test database, and longer in production, where the
// HNSW index is there to maintain. `null = $2` is unknown in SQL, so for as long as that pass
// runs the column's own predicate hides every row it has not reached, and a project-scoped
// meaning search answers from the labelled part of the corpus alone. The joined predicate reads
// the project off the owner, which every row has whether it is labelled or not, so it answers
// for the whole corpus, at the recall and the plan this change exists to improve: the read
// path serves it until the fill's own row reads done, and the pass retires it for the
// unfiltered searches (meaningStatementFor), which is every search but the one narrowing by
// status, author or updated_since - that one keeps it for its filters' sake, not for the
// column's.
//
// column is the owner column the joined shape reads, one per arm, each of them a predicate on
// a table the arm joins: the issue arm an "$2 is empty, or it matches" over issues.project_key,
// the document arm the same over `p.key`, the ask arm over `coalesce(i.project_key, p.key)`,
// the message arm over its issue's `i.project_key`, and the comment arm the same coalesce,
// inside its candidate limit as the others carry theirs - which for that arm means its joins go
// below the limit too, since the column is theirs (searchMeaningCommentArmJoined). What
// meaningOneProject replaces is the plan that costs, not the rows it finds.
// meaningOneProject ignores column: the candidate row carries the project itself. What moves
// between the shapes beside the project predicate is where the comment arm carries q's three
// filters (searchRowFilters): inside the candidate limit under meaningOneProjectJoined, as every
// other arm carries them in every shape, and above it in the arm the other two scopes take,
// which is why a request narrowing by one of them runs the joined statement whatever project it
// names (meaningStatementFor).
type meaningProjectScope int

const (
	meaningEveryProject meaningProjectScope = iota
	meaningOneProject
	meaningOneProjectJoined
)

func (scope meaningProjectScope) candidateProject(column string) string {
	switch scope {
	case meaningOneProject:
		return " and e.project_key = $2"
	case meaningOneProjectJoined:
		return " and ($2 = '' or " + column + " = $2)"
	default:
		return ""
	}
}

// searchQueryMeaningBody is searchQueryMeaning after its q, over keywordLegs (searchKeywordLegs),
// fused (searchFusedSQL or relatedFusedSQL, as searchQueryBody takes it), without or with an
// exclusion (searchKindsCTE), and scoped to one project - on the row's own column or on the
// owner each arm joins - or to every one.
func searchQueryMeaningBody(keywordLegs, fused, exclude string, scope meaningProjectScope) string {
	return `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id, q
   where e.kind = 'issue' and e.embedding is not null and e.model = $11` + scope.candidateProject("i.project_key") +
		searchRowFilters("i.status", "i.created_by", "i.updated_at") + `
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
    join artifacts a on a.id = ` + windowOwnerUUID + `
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where e.kind = 'document_block' and a.kind = 'doc' and e.embedding is not null and e.model = $11` + scope.candidateProject("p.key") +
		searchRowFilters("i.status", "a.created_by", "v.created_at") + `
   order by e.embedding <=> $9::vector
   ` + searchWindowLimit + `
),
meaning_document as (
  select distinct on (meaning_document_candidates.id) 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
   order by meaning_document_candidates.id, r desc, window_id
),
` + searchMeaningCommentArm(scope) + `,
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, ` + askBlockNodeSQL + ` as block_id, e.id as window_id, e.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id = ` + windowItemUUID + `
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where e.kind = 'ask_part' and e.embedding is not null and e.model = $11` + scope.candidateProject("coalesce(i.project_key, p.key)") +
		searchRowFilters("i.status", "k.author", "k.created_at") + `
   order by e.embedding <=> $9::vector
   ` + searchWindowLimit + `
),
meaning_ask as (
  select distinct on (meaning_ask_candidates.id) 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, ` + store.CastableUUID(`anchor->>'artifact_id'`) + `), id::text, block_id,
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
    join messages m on m.id = ` + windowItemUUID + `
    join issues i on i.key = m.issue_key, q
   where e.kind = 'message_block' and e.embedding is not null and e.model = $11` + scope.candidateProject("i.project_key") +
		searchRowFilters("i.status", "m.author", "m.created_at") + `
   order by e.embedding <=> $9::vector
   ` + searchWindowLimit + `
),
meaning_message as (
  select distinct on (meaning_message_candidates.id) 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
   order by meaning_message_candidates.id, r desc, window_id
),
` + searchKindsCTE(keywordLegs+`
  union all select * from meaning_issue
  union all select * from meaning_document
  union all select * from meaning_comment
  union all select * from meaning_ask
  union all select * from meaning_message`, exclude) + `ranked as (
  select *, row_number() over (partition by kind, list order by own desc, r desc, updated_at desc, id) as pos
    from kinds
),
legs as (
  select * from ranked where pos <= $7
),
` + fused + `legs_unique as (
  select distinct on (kind, id) kind, issue_key, owner_artifact_id, artifact_id, id, block_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at
    from legs
   order by kind, id, (window_id is null), r desc
),
totals as (
  select (select count(*) from (select distinct kind, id from kinds) u) as total,
         (select count(*) from fused) as reachable
),
` + searchPageAndTailSQL("legs_unique")
}

// searchMeaningCommentArm is searchQueryMeaningBody's comment arm. It is two arms, not one,
// and which of them is scope's: the arm this change buys, below, under meaningOneProject and
// meaningEveryProject, and the arm the release before it ran under meaningOneProjectJoined
// (searchMeaningCommentArmJoined). The two differ in where the comment's own tables are joined
// and therefore in nearly every line, so the second is written out rather than switched inside
// this one: a body that parameterised the join's place would read as neither arm.
//
// This arm is written apart from the three window arms beside it because its order differs
// from theirs and that order is the whole of it: it takes the nearest comment windows through
// the index first - under `embedding is not null`, which is embeddings_cosine's own partial
// predicate, and the kind, model and project filters, which the scan applies as it walks, so
// the ordering stays the index's to serve - and joins comments, issues, artifacts and projects
// above that limit, for the row's own fields alone.
//
// That join is a filter as well as a read: `join comments` drops a window whose comment row is
// gone, and above the limit it drops it after the window has taken a place in the candidate set,
// where main's arm drops it inside the ordered select. So 1,001 windows of comments that no longer
// exist, nearer than a project's three comments, would answer that project with none of them,
// where main answers all three (measured on such a corpus, INFRA-988). It loses no row today
// because no path deletes a comment row: no route deletes a comment, and nothing outside the
// tests runs `delete from comments`. A change that adds comment deletion (dispatch://INFRA-1523)
// must delete the comment's windows in the same transaction, or move this join back inside the
// limit.
//
// The three window arms beside it filter and join inside the ordered, limited select, and for
// comments that order costs the index. The item join is on an expression of the window's id
// (windowItemUUID), which carries no statistics, so the planner misreads it: measured on the
// load-test deployment (INFRA-988, EXPLAIN (ANALYZE, BUFFERS) of this route's own statement with
// the eleven arguments it took then), the join planned for 475 rows and got 120,519, its hash side
// planned 1,030 and got 158,727 per loop, and one hash batch became 32 spilled ones, 330 MB of
// temp I/O. With that join below the ordering, `order by embedding <=> $9` could not be served
// by embeddings_cosine at all: the arm read embeddings whole (1.27 M rows for the 117 K
// comment_block vectors it wanted), read comments whole, and top-N-sorted 120 K rows - 5,166 ms
// of the statement's 5,351 ms. The document arm, which does the same work the index can serve,
// read 1,060 rows in 146.8 ms.
//
// Searching first and filtering after is paid in candidate depth, and that is why this arm
// could not simply keep its project filter where the join is: with the filter above the limit,
// the windows of every project consume the candidate set a project-scoped search then filters,
// and a project holding 800 windows was answered with 2 of them while another project's were
// nearer, however deep the candidate set was made. The filter is a predicate on the candidate
// row itself here (scope.candidateProject, migration 20261109120000), inside the ordered,
// limited select where the other arms carry theirs: the limit is spent on the project that
// asked, and the planner chooses between embeddings_kind_project for a small project and the
// HNSW scan with the predicate inside it for a large one.
//
// The depth asked for is still ten times the items the arm keeps (searchWindowCandidates), for
// the reason that constant gives: one long thread's windows would otherwise fill the arm. What
// the index delivers against it is its own: searchMeaningScan sets hnsw.iterative_scan to
// relaxed_order, which lets an HNSW scan carry on past the first hnsw.ef_search = 400 vectors
// rather than stopping there, and that is the whole of what it promises. It does not fill the
// LIMIT - measured on the load-test corpus, this arm forced through embeddings_cosine over
// 1,104 qualifying rows took 245, unmoved by every scan setting tried (the list beside
// searchMeaningScan holds the numbers) - so the depth is a request, not a guarantee, and a
// project the index does not reach is as absent as one a predicate excluded.
//
// Everything else is as the other window arms have it, bar where q's filters run. The floor
// (searchMeaningFloor, $10) and the per-item window dedup - `distinct on` over the comment, its
// nearest window first, carrying that window's id and covers - run above the join rather than
// beside it, which is where the joined arm keeps them too, so neither of them moved. q's own
// three filters (searchRowFilters) did move: they run above the limit here, since status, author
// and written are columns of the comment and its issue, which only that select has joined, where
// the joined arm carries them inside it. A filter above the limit narrows the wrong rows - one
// author's searchWindowCandidates+1 nearer windows fill the candidate set, and a search for
// another author's comment is answered with none of them - so a request that names any of the
// three does not run this arm at all: it runs the joined statement, whose comment arm is main's
// (meaningStatementFor). The project predicate is the one filter that had to move the other
// way, down into the candidate CTE: it is the one a search names on nearly every request, so it
// is the one whose absence from the index scan spent the whole candidate set on other projects.
func searchMeaningCommentArm(scope meaningProjectScope) string {
	if scope == meaningOneProjectJoined {
		return searchMeaningCommentArmJoined
	}
	return `meaning_comment_candidates as (
  select e.id as window_id, e.covers, ` + windowItemUUID + ` as comment_id,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
   where e.kind = 'comment_block' and e.embedding is not null and e.model = $11` +
		scope.candidateProject(searchMeaningCommentProject) + `
   order by e.embedding <=> $9::vector
   ` + searchWindowLimit + `
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
   where meaning_comment_candidates.r >= $10` + searchRowFilters("i.status", "c.author", "c.created_at") + `
   order by c.id, meaning_comment_candidates.r desc, meaning_comment_candidates.window_id
)`
}

// searchMeaningCommentProject is the project a comment belongs to read off the tables the arm
// joins: its issue's, or, for a comment anchored on a project document, that document's. It is
// the column embeddings_owner_project_key labels a comment window with (migration
// 20261109120000), so the joined arm and the labelled column answer the same project for the
// same row - which is what makes the switch between the two shapes a change of plan and not of
// answer. meaningOneProject and meaningEveryProject ignore it (candidateProject).
const searchMeaningCommentProject = `coalesce(i.project_key, p.key)`

// searchMeaningCommentArmJoined is the comment arm under meaningOneProjectJoined, and it is the
// arm the release before this change ran, line for line: comments, issues, artifacts and
// projects joined and the project predicate applied inside the ordered, limited candidate
// select, with the `distinct on` and the floor above it. The predicate's own text is rendered
// rather than written out (meaningOneProjectJoined.candidateProject), so it is the same string
// every other arm takes under that scope; every other line is this arm as the default branch
// held it at 5f86d6c949f9.
//
// It is that arm deliberately, and deliberately slower, for exactly as long as the fill runs.
// Slower because its joins stand below the ordering, which is the 5,166 ms the measurement
// above holds: `order by embedding <=> $9` is not embeddings_cosine's to serve once a join on
// an expression of the window's id stands under it. What it buys is the rows: project_key is
// nullable and every row stored before 20261109120000 carries null until
// store.FillEmbeddingProjectKeys labels it, so `e.project_key = $2` hides every row the pass
// has not reached, where the project read off the owner is there for every row whether the
// pass has reached it or not.
//
// Keeping the predicate inside the limit is the whole of why this is written out rather than
// made by moving one predicate: this arm's project predicate reads tables the candidate select
// is the only place to have joined, so the predicate can only stay inside the limit if the
// joins do. Hoisting both, which is what this arm did when the candidate predicate first
// arrived here, spends the candidate limit on every project's windows again - the defect the
// whole change exists to end. Measured through GET /api/v1/search with 1,001 comment windows
// of another project at cosine 1.000 against three comments of the named project: the hoisted
// shape answered four kinds with the comment gone, where this shape and a build of the default
// branch both answered five, and the threshold is the candidate limit itself - 1,001 windows
// lose it, 999 do not (INFRA-988,
// TestProjectScopedSearchFindsItsCommentsPastThatCrowdWhileTheFillIsStillLabelling).
//
// The fill's row reading done is what retires it for the unfiltered searches
// (meaningStatementFor): the plan the arm above has is this change's whole point, and this is
// the price of serving every row until the pass has labelled them. A search narrowing by status,
// author or updated_since keeps it for good, since those three filters are inside this arm's
// limit and above the other's.
var searchMeaningCommentArmJoined = `meaning_comment_candidates as (
  select c.issue_key, c.artifact_id, c.anchor, c.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, c.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join comments c on c.id = ` + windowItemUUID + `
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where e.kind = 'comment_block' and e.embedding is not null and e.model = $11` +
	meaningOneProjectJoined.candidateProject(searchMeaningCommentProject) +
	searchRowFilters("i.status", "c.author", "c.created_at") + `
   order by e.embedding <=> $9::vector
   ` + searchWindowLimit + `
),
meaning_comment as (
  select distinct on (meaning_comment_candidates.id) 'comment' as kind, 'meaning' as list, issue_key, artifact_id, coalesce(artifact_id, (anchor->>'artifact_id')::uuid), id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_comment_candidates where r >= $10
   order by meaning_comment_candidates.id, r desc, window_id
)`

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}

	query := r.URL.Query()
	searchText := strings.TrimSpace(query.Get("q"))
	if utf8.RuneCountInString(searchText) < 2 {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "q must be at least 2 characters")
		return
	}
	// q is capped as `dispatch search` caps it, counted after trimming so a query the command
	// sends is never refused here, and a project must be a key (an empty one searches every
	// project). Both ride in the URL; packages/contracts/AGENTS.md "Search limits" says what keeps
	// it short.
	if length := itemblocks.Units16(searchText); length > contracts.SearchQueryMax {
		tooLong := capExceededError("q", length, contracts.SearchQueryMax)
		writeError(w, tooLong.code, tooLong.status, tooLong.message+"; "+contracts.SearchQueryHint)
		return
	}
	project := strings.TrimSpace(query.Get("project"))
	if project != "" && !projectKeyPattern.MatchString(project) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("project", project))
		return
	}
	// number_project is the project of the page the search was made from, which a bare issue
	// number is read in and which nothing else reads: it is no filter, and a search carrying it
	// answers every project's rows as a search carrying nothing does. It is a key or empty, as
	// project is, and is refused under a code of its own.
	numberProject := strings.TrimSpace(query.Get("number_project"))
	if numberProject != "" && !projectKeyPattern.MatchString(numberProject) {
		writeError(w, "INVALID_NUMBER_PROJECT", http.StatusBadRequest,
			projectKeyProblem("number_project", numberProject))
		return
	}
	// keywords_only is the bar's keystroke request: the search runs the keyword statement alone
	// and makes no embedding call, so a deployment whose embedder is live pays nothing for a
	// query a person is still typing. The route spells it as GET /api/v1/projects/{key}/related
	// does, true or false and nothing else (parseQueryBool), and refuses anything else - a blank
	// value, or the parameter repeated - under a code of its own, as every other parameter here
	// is refused. A request that leaves it out searches exactly as it always did.
	keywordsOnly := false
	if parsed, present, valid := parseQueryBool(query, "keywords_only", true, false); present {
		if !valid {
			writeError(w, "INVALID_KEYWORDS_ONLY", http.StatusBadRequest, "keywords_only must be true or false")
			return
		}
		keywordsOnly = parsed
	}
	status, author, since, ok := parseSearchFilters(w, query)
	if !ok {
		return
	}

	limit := searchDefaultLimit
	if parsed, present, valid := parseQueryInt(query, "limit", 1, searchMaxLimit); present {
		if !valid {
			writeError(w, "INVALID_LIMIT", http.StatusBadRequest, "limit must be an integer from 1 to 50")
			return
		}
		limit = parsed
	}
	offset := 0
	if parsed, present, valid := parseQueryInt(query, "offset", 0, math.MaxInt); present {
		if !valid {
			writeError(w, "INVALID_OFFSET", http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = parsed
	}

	// The query is read once for the whole request - the words a person wrote, the words they
	// excluded and the operators around them - and every later reader of it takes that one
	// reading (searchParameters.queryReading) rather than walking the text again.
	reading := readSearchQuery(searchText)
	parameters := searchParameters{text: searchText, project: project, numberProject: numberProject,
		status: status, author: author, since: since, limit: limit, offset: offset,
		keywordsOnly: keywordsOnly, trees: wantsDisplayTrees(r), reading: &reading}
	response, err := s.runSearch(r.Context(), parameters)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// What search made of the words, and which filters it narrowed every leg by, for a caller
	// that shows a person what it did with them; answered on every path, the empty answer of a
	// query with no lexemes included. Whether search broadened the query is no reading of the
	// text: runSearch stamps that on the response itself, true only where the fallback ran.
	response.Query.Operators = reading.operators()
	response.Filters = searchAppliedFilters(parameters)
	// Where each ask hit stands is read for this route alone: the write-time suggestions share
	// the ranking (runFusedSearch) but never show it. TookMS counts the read.
	started := time.Now()
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	if err := s.attachAskStates(r.Context(), s.deps.Store.Pool, response.Results, reader); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader.resolve()
	response.TookMS += time.Since(started).Milliseconds()
	WriteJSON(w, http.StatusOK, response)
}

// runSearch is the search handler's own entry point. The keys a query names are resolved here
// and the numnode check runs behind them, both before anything else - including before the one
// model-service call this function ever makes - so a stopword-only or operator-only query never
// pays for an embedding it would throw away. A query that names an issue by key answers that
// issue even when it holds no lexeme any item matches, so numnode cannot refuse it and its round
// trip is not made: the key forms need no statement at all (searchOwnIssueKeys), and typing a
// key is what the bar does on every keystroke. It tries meaning search first when an embedder is
// configured, falling back to keyword-only (runFusedSearchQuery)
// only when the query's own embedding could not be had - never when the meaning query itself then
// fails, which is a real error the caller should see as a 500, not something to paper over as a
// degraded answer.
//
// A request that asked for keywords only (?keywords_only=true) never reaches the embedder at
// all: it runs the keyword statement whatever the deployment configured, and its answer is not
// stamped degraded, because nothing about this deployment's meaning search is unavailable - the
// caller asked for the words alone. That is the bar's keystroke request, and an embedding per
// keystroke is the cost it exists to avoid.
//
// The query as typed is what it runs, however many words that query holds: one statement, and
// for every query matching anything the only statement a search runs, so the common path costs
// what it cost before there was a fallback at all. A query that matches nothing - no row of any
// kind, which is what the answer's own total says - is run a second time over its rarest words
// (searchRarestWords, searchBroadenWords), and that second answer alone is stamped broadened.
// The query's embedding is taken once and both runs share it, so a broadened search still costs
// one model-service call.
func (s *server) runSearch(ctx context.Context, parameters searchParameters) (model.SearchResponse, error) {
	keys, err := s.searchOwnIssueKeys(ctx, parameters.text, parameters.numberScope())
	if err != nil {
		return model.SearchResponse{}, err
	}
	parameters.ownKeys = keys
	if len(keys) == 0 {
		var nodes int
		if err := s.deps.Store.Pool.QueryRow(ctx,
			"select numnode(websearch_to_tsquery('english', $1))", parameters.text).Scan(&nodes); err != nil {
			return model.SearchResponse{}, err
		}
		if nodes == 0 {
			return model.SearchResponse{Results: []model.SearchResult{}, Limit: parameters.limit, Offset: parameters.offset}, nil
		}
	}
	run := s.runKeywordOnlyDegraded
	if parameters.keywordsOnly {
		run = s.runFusedSearchQuery
	} else if s.deps.Embedder != nil {
		vector, err := s.embedQuery(ctx, parameters.text)
		if err != nil {
			slog.Warn("dispatch search: query embedding unavailable, answering keyword-only", "error", err)
		} else {
			run = func(ctx context.Context, parameters searchParameters) (model.SearchResponse, error) {
				return s.runMeaningSearch(ctx, parameters, vector)
			}
		}
	}
	started := time.Now()
	response, err := run(ctx, parameters)
	if err != nil || response.Total > 0 {
		return response, err
	}
	words, err := s.searchRarestWords(ctx, parameters)
	if err != nil {
		return model.SearchResponse{}, err
	}
	if len(words) == 0 {
		return response, nil
	}
	parameters.broaden = words
	parameters.excluded = parameters.queryReading().excludedWords()
	broadened, err := run(ctx, parameters)
	if err != nil {
		return model.SearchResponse{}, err
	}
	broadened.Query.Broadened = true
	// What the caller waited for is all three reads: the query as typed, its words' df, and the
	// fallback itself.
	broadened.TookMS = time.Since(started).Milliseconds()
	return broadened, nil
}

// runKeywordOnlyDegraded runs the keyword-only fallback and stamps the response Degraded on
// success: the one thing both of runSearch's fallback branches (no embedder configured, or the
// query's own embedding failing) do identically. runSearch has already run the numnode check
// before ever reaching here.
func (s *server) runKeywordOnlyDegraded(ctx context.Context, parameters searchParameters) (model.SearchResponse, error) {
	response, err := s.runFusedSearchQuery(ctx, parameters)
	if err == nil {
		response.Degraded = contracts.SearchDegradedEmbedderUnavailable
	}
	return response, err
}

// runFusedSearchQuery runs searchQuery, assuming nodes > 0 or an issue named by key already,
// which its callers (runKeywordOnlyDegraded, and runSearch itself for a request that asked for
// keywords only, both after runSearch's checks) must have established first. The text is assumed
// already validated (length, non-empty after trimming); an empty project searches every project,
// and an empty filter narrows nothing.
func (s *server) runFusedSearchQuery(ctx context.Context, parameters searchParameters) (model.SearchResponse, error) {
	started := time.Now()
	rows, err := s.deps.Store.Pool.Query(ctx, searchQuery, searchQueryArguments(parameters)...)
	if err != nil {
		return model.SearchResponse{}, err
	}
	defer rows.Close()

	response, err := scanSearchRows(rows, parameters.text, parameters.limit, parameters.offset, parameters.trees)
	if err != nil {
		return model.SearchResponse{}, err
	}
	response.TookMS = time.Since(started).Milliseconds()
	return response, nil
}

// runMeaningSearch is runFusedSearchQuery's counterpart once a query embedding exists: same row
// shape, one of the three meaning statements in place of searchQuery. Its only caller, runSearch,
// has already run the numnode check before ever reaching here (so it never pays for an
// embedding it would throw away).
//
// Which of the three is meaningStatementFor's, over the project the caller named and the one
// row that says whether the fill has labelled the corpus (embeddingProjectKeysFilled);
// meaningProjectScope says why the choice is made here, in Go, rather than written as one
// "$2 is empty, or it matches" predicate the planner would have to serve all three ways. The
// arguments are the same seventeen whichever it is, $2 among them: the keyword legs read it in
// all three.
//
// It runs in a read transaction of its own, the one place searchMeaningScan's settings apply
// (beginMeaningScan), the bound on the statement among them. A statement the server ends at that
// bound comes back here as the error it is, and this route reports it as it reports every other
// failure of the meaning query: the handler answers 500 INTERNAL (writeHandlerError), never the
// keyword-only answer runSearch keeps for a query whose embedding could not be had. A search
// cut off at 15 s is a real failure of this request, not a degraded answer to it.
func (s *server) runMeaningSearch(ctx context.Context, parameters searchParameters, vector []float32) (model.SearchResponse, error) {
	started := time.Now()
	tx, err := s.beginMeaningScan(ctx)
	if err != nil {
		return model.SearchResponse{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Each flag is read only where it can change the statement: a filtered search runs the
	// joined one whatever the fill has reached, and a search naming no project runs the unscoped
	// one, so neither pays for the progress row's savepoint.
	filtered := parameters.filtered()
	filled := !filtered && parameters.project != "" && embeddingProjectKeysFilled(ctx, tx)
	statement := meaningStatementFor(parameters.project, filled, filtered)
	rows, err := tx.Query(ctx, statement,
		searchMeaningQueryArguments(parameters, vector, s.deps.Embedder.Identity())...)
	if err != nil {
		return model.SearchResponse{}, err
	}
	defer rows.Close()

	response, err := scanSearchRows(rows, parameters.text, parameters.limit, parameters.offset, parameters.trees)
	if err != nil {
		return model.SearchResponse{}, err
	}
	response.TookMS = time.Since(started).Milliseconds()
	return response, nil
}

// meaningStatementFor is the meaning statement a search of project runs. filled is what
// embeddingProjectKeysFilled answered in that search's own transaction, and filtered whether the
// request narrows by any of q's three filters (searchParameters.filtered).
//
// A filtered search takes the joined statement, whatever project it names and whatever the fill
// has reached, because that is the only one of the three whose comment arm applies those filters
// inside its ordered, limited candidate select. The arm the other two take reads the nearest
// searchWindowCandidates comment windows first and filters above that limit, so one author's
// thousand nearer windows fill the candidate set and a search for another author's comment is
// answered with none of them, where the route says each of the three narrows every leg
// (routes_table.go, GET /api/v1/search) and the web client shows them as doing so
// (packages/dispatch/web/src/api/client.ts). What it costs that search is main's plan, the joins
// below the ordering that searchMeaningCommentArm measures, and that is the price of answering
// the contract the route states.
//
// One statement serves the filtered searches that name a project and those that do not: every
// joined arm's project predicate reads "$2 is empty, or the owner matches it"
// (meaningProjectScope.candidateProject), which a search naming no project answers by binding $2
// empty, exactly as the keyword legs it unions already do.
//
// Unfiltered, the choice is the project's and the fill's: a search naming no project takes the
// unscoped statement, a project on a database whose fill has finished takes the one reading
// `e.project_key = $2`, and a project on a database whose fill is still running takes the joined
// one, the only one of the three that can answer for a row the fill has not reached
// (meaningProjectScope). The related-item check chooses among its own three the same way and has
// no filtered case to choose for: its q binds all three filters empty (relatedQOpener), so no
// caller of it can narrow by them (relatedMeaningStatementFor).
func meaningStatementFor(project string, filled, filtered bool) string {
	switch {
	case filtered:
		return searchQueryMeaningJoined
	case project == "":
		return searchQueryMeaning
	case filled:
		return searchQueryMeaningScoped
	default:
		return searchQueryMeaningJoined
	}
}

// embeddingProjectKeysFilled answers whether store.FillEmbeddingProjectKeys has labelled every
// embeddings row stored before migration 20261109120000 added project_key: the done of the one
// row 20261109120100 creates for that pass to record its place in, read on the caller's own
// transaction, which is the connection the meaning statement is about to run on.
//
// Read per search, with no cache and no flag taken at boot. During a rolling deploy one process
// makes the pass and the other skips it (api.RunEmbeddingProjectKeyFill,
// cmd/dispatch/main.go), and the process that skipped it has no way of its own to learn that
// the other has finished: an in-memory flag would leave half the deployment searching the
// older way for as long as the release lived. This row is the one answer both of them read,
// and it costs one read of a one-row table on a connection the search already holds.
//
// A read that fails answers false, which serves the joined statement: that one finds a row
// whether it is labelled or not, so of the two answers it is the one that cannot drop rows
// from the page, where answering true over a half-labelled table would silently lose every
// row the pass has not reached. A worse plan is the right price for a complete answer, and the
// failure is logged rather than swallowed: a progress row this process cannot read is a fact
// about the deployment, not about this request.
//
// The read runs inside a savepoint of its own (pgx opens one per nested Begin) because a
// failure the server raises aborts the transaction it is raised in, and that is the
// transaction the meaning statement is about to run on: without the savepoint the promise
// above is not kept, and the search answers 500 rather than the joined statement's page. Both
// failures were measured that way at this change's head (INFRA-988) - the progress table
// missing, and a value this process cannot scan, whose first search on each pooled connection
// met `cached plan must not change result type` (SQLSTATE 0A000), which pgx does not retry
// inside a transaction. Rolling back to the savepoint leaves the transaction exactly as it was.
//
// A missing table is not a binary newer than its database: the boot migrates before it serves
// (cmd/dispatch/main.go), so a process that serves a search has applied every migration it
// carries. What does reach that state is a database somebody repaired by hand, one restored
// from a dump taken before the migration, or one somebody is in the middle of moving - and each
// of them is a fact about the deployment, which is why the read says so and answers the
// complete page rather than failing the request.
//
// The savepoint also carries the bound on how long that read may wait. The row is one row of a
// one-row table, so the only wait it can take is a lock somebody else holds on the table, and
// behind an ACCESS EXCLUSIVE lock the read waited out searchMeaningStatementBound's whole 15
// seconds before it degraded - 15,002 ms measured at this change's head, against 202 ms with
// the setting below (INFRA-988). A search that is going to answer from the joined statement
// anyway must not spend a quarter of a minute learning that, so the savepoint sets
// embeddingProjectKeysFilledLockWait before the read and the deferred rollback takes it back
// off the transaction the meaning statement then runs on.
//
// What it costs is three round trips on a connection the caller already holds - the savepoint,
// the lock bound and the rollback - for every search that reads it: a project-scoped meaning
// search narrowing by none of q's filters, and the related-item check of one project, which
// narrows by none of them ever (relatedMeaningSearch). The other way out was to read the row
// before beginMeaningScan opens the transaction at all, which costs no savepoint
// but a second acquisition from the pool per search, and the pool is the resource a deployment
// under meaning-search load runs out of first (searchMeaningScan's own measurement of 219
// backends holding it full). This way also keeps the read where both callers already make it,
// on the transaction they pass in (runMeaningSearch, relatedMeaningSearch), rather than in two
// places that could drift apart.
func embeddingProjectKeysFilled(ctx context.Context, tx pgx.Tx) bool {
	degraded := func(err error) bool {
		slog.Warn("dispatch: a project-scoped meaning search could not read the embeddings project_key fill's progress, so it searched by each arm's own owner",
			"error", err)
		return false
	}
	read, err := tx.Begin(ctx)
	if err != nil {
		return degraded(err)
	}
	// Rolled back whether the read answered or failed: it wrote nothing either way, and the
	// rollback is what hands the transaction back usable when it failed.
	defer func() { _ = read.Rollback(ctx) }()
	// Inside the savepoint, so the rollback above restores whatever the transaction had: a
	// `set local` is undone by a rollback to the savepoint it was made in, exactly as the read's
	// own failure is.
	if _, err := read.Exec(ctx, embeddingProjectKeysFilledLockWait); err != nil {
		return degraded(err)
	}
	var done bool
	if err := read.QueryRow(ctx, embeddingProjectKeysFilledQuery).Scan(&done); err != nil {
		return degraded(err)
	}
	return done
}

// embeddingProjectKeysFilledLockWait bounds the progress read's wait for the one lock it can
// queue behind, set inside the savepoint that read runs in (embeddingProjectKeysFilled). 200 ms
// is far above what taking an uncontended lock on a one-row table costs and far below the 15
// seconds a search may not spend on a flag: a read that waits that long is behind a lock
// somebody holds on embeddings_project_key_backfill - a hand repair, a migration of a later
// release - and the answer it would give is the one a failed read already gives.
const embeddingProjectKeysFilledLockWait = `set local lock_timeout = '200ms'`

// embeddingProjectKeysFilledQuery reads that row's done and nothing else of it: the kind and id
// beside it are the pass's keyset cursor, which the pass alone reads
// (store.FillEmbeddingProjectKeys).
const embeddingProjectKeysFilledQuery = `select done from embeddings_project_key_backfill`

// scanSearchRows reads searchQuery's and searchQueryMeaning's shared column shape - every column
// through the headline - into a SearchResponse. q is the original query text, for each result's
// href (searchHref's own query-string echo).
func scanSearchRows(rows pgx.Rows, q string, limit, offset int, trees bool) (model.SearchResponse, error) {
	response := model.SearchResponse{Results: []model.SearchResult{}, Limit: limit, Offset: offset}
	for rows.Next() {
		var result model.SearchResult
		var kind, ownerKind, id, headline *string
		var ownerKey, ownerTitle, ownerStatus, ownerProject, ownerSlug, ownerArtifactID, ownerName, slug, name *string
		var primary bool
		var blockID *string
		var matchBlockID *string
		var rank *float64
		if err := rows.Scan(
			&response.Total,
			&response.Reachable,
			&kind,
			&ownerKind,
			&ownerKey,
			&ownerTitle,
			&ownerStatus,
			&ownerProject,
			&ownerSlug,
			&ownerArtifactID,
			&ownerName,
			&slug,
			&name,
			&primary,
			&id,
			&blockID,
			&matchBlockID,
			&rank,
			&headline,
		); err != nil {
			return model.SearchResponse{}, err
		}
		// A page past the last reachable row is one row of totals and no hit.
		if kind == nil {
			continue
		}
		result.Kind, result.ID, result.Rank, result.BlockID = *kind, *id, *rank, matchBlockID
		if *ownerKind == "issue" {
			result.Owner = model.SearchOwner{Kind: *ownerKind, Key: *ownerKey, Title: *ownerTitle, Status: *ownerStatus}
			result.Issue = &model.SearchIssue{Key: *ownerKey, Title: *ownerTitle, Status: *ownerStatus}
		} else {
			result.Owner = model.SearchOwner{
				Kind:       *ownerKind,
				Project:    deref(ownerProject),
				Slug:       *ownerSlug,
				ArtifactID: *ownerArtifactID,
				Name:       *ownerName,
			}
		}
		if slug != nil {
			result.Artifact = &model.SearchArtifact{Slug: *slug, Name: *name}
		}
		// A null headline is an empty snippet, not a panic. ts_headline answers null when the
		// text it is given is null, and txt.text is null for a document hit whose latest
		// artifact_versions row carries no markdown while an earlier one holds the markdown the
		// search matched (searchPageAndTailSQL's document arm reads the latest version alone).
		// The write path refuses the shape that leaves such a row - a blob version under a
		// document artifact answers ARTIFACT_KIND_MISMATCH - so this is a bound on the scan and
		// not a path a caller drives: deref costs nothing per row, and without it one row of a
		// database somebody wrote by hand takes the process down for every search.
		headlineText := deref(headline)
		result.Snippet = markSnippet(headlineText)
		// A snippet's tree is no stored tree and is never primed from the table or stored in it:
		// it is one query's own headline, cut and marked for this search alone
		// (pmdoc.DisplaySnippetJSON), so there is nothing two readers would share. A null
		// headline is the empty snippet here too, for the reason above: the tree of no text.
		if trees {
			result.SnippetTree = model.DisplayTree(pmdoc.DisplaySnippetJSON(headlineText, markStart, markEnd))
		}
		result.Href = searchHref(result.Kind, result.Owner, result.Artifact, primary, result.ID, blockID, matchBlockID, q)
		response.Results = append(response.Results, result)
	}
	if err := rows.Err(); err != nil {
		return model.SearchResponse{}, err
	}
	return response, nil
}

// embedQuery embeds q for meaning search (embed.InputQuery - Cohere's embed-v4 asymmetric mode
// embeds a query differently from a stored document), bounded by searchEmbedTimeout so one
// degraded request never holds the whole search handler open on a slow or wedged model service.
func (s *server) embedQuery(ctx context.Context, q string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, searchEmbedTimeout)
	defer cancel()
	vectors, err := s.deps.Embedder.Embed(ctx, []string{q}, embed.InputQuery)
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		// Not a timeout: the Embedder interface promises one vector per input text, and this is
		// a contract violation by whatever implementation answered, never something an actual
		// deadline produces. runSearch logs this exactly like a real timeout and falls back to
		// keyword-only, which is still the right behavior for callers - the distinct message
		// matters only to whoever reads the log.
		return nil, fmt.Errorf("embed: embedder returned %d vectors for 1 query text", len(vectors))
	}
	return vectors[0], nil
}

// firstTerm is the first plain word of a query text, the operators a person wrote around it
// skipped. A search binds the first term of the reading it has already taken
// (searchReading.firstTerm, searchQueryBase); this reads a text no reading was taken of, which
// is what the package's own tests of the snippet window hold.
func firstTerm(query string) string {
	return readSearchQuery(query).firstTerm()
}

func markSnippet(headline string) string {
	return strings.NewReplacer(markStart, "<mark>", markEnd, "</mark>").Replace(html.EscapeString(headline))
}

// issueDocumentHref is the SPA route for one of an issue's documents: the primary document is
// the issue's Spec tab, every other artifact its own Artifacts route.
func issueDocumentHref(issueHref string, artifact *model.SearchArtifact, primary bool) string {
	if primary {
		return issueHref + "/spec"
	}
	return issueHref + "/artifacts/" + url.PathEscape(artifact.Slug)
}

// searchHref is where a hit's reader should land. A comment or ask that carries a document —
// the document its anchor quotes, or the one holding its typed block — belongs beside that
// document, so its href opens the document itself and names the item in the query, exactly as
// the SPA's own thread links do. An ask whose typed block is a node of the reference graph
// (blockID, askBlockNodeSQL) names that block in the fragment instead, which a reader of the
// href reads as the block's node; one whose block is no node names the ask. An item with no
// document is a Conversation turn, named by its own route so the turn is focused.
//
// matchBlock is the block a meaning hit matched (INFRA-370), which a document hit names in its
// fragment so the reader lands on the passage rather than at the top of the document. A comment,
// a message and an ask keep the href they had: no item page reads a fragment, and the ask's own
// fragment names the document block it is typed on, which is another block than the part of the
// ask that matched.
func searchHref(kind string, owner model.SearchOwner, artifact *model.SearchArtifact, primary bool, id string, blockID, matchBlock *string, query string) string {
	issueHref := "/issues/" + owner.Key
	// The document a hit belongs beside: a project document's own route, or the issue document
	// its anchor names. An issue item with no document has none, and keeps its own route.
	documentHref := ""
	if owner.Kind == "document" {
		documentHref = "/projects/" + url.PathEscape(owner.Project) + "/documents/" + url.PathEscape(owner.Slug)
	} else if artifact != nil {
		documentHref = issueDocumentHref(issueHref, artifact, primary)
	}

	switch kind {
	case "issue":
		return issueHref
	case "document":
		// A document hit always has its artifact, so documentHref above is its document's.
		href := documentHref
		href += "?q=" + url.QueryEscape(query)
		if matchBlock != nil {
			href += "#b-" + text.EncodeBlockID(*matchBlock)
		}
		return href
	case "comment":
		if documentHref == "" {
			return issueHref + "/comments/" + id
		}
		return documentHref + "?comment=" + url.QueryEscape(id)
	case "ask":
		if documentHref == "" {
			return issueHref + "/asks/" + id
		}
		if blockID != nil {
			return documentHref + "#b-" + text.EncodeBlockID(*blockID)
		}
		return documentHref + "?ask=" + url.QueryEscape(id)
	case "message":
		return issueHref + "/messages/" + id
	default:
		return ""
	}
}
