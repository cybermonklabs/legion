package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The constants below are main's text, at commit b459fa923705, of statements of the graph read.
// refs.Edges' in reads, as readEdgeRows writes each for the Query each caller named sends (a
// document with WholeDocuments false and true, a message, a comment, an ask and the ask panel,
// and the issue read, the first arm of every in read), are the statements this change alters; the
// four loaders resolveNodes runs for the comments, asks, messages and blocks at the other end of an
// edge are the ones the graph read's one-hop change alters. Each was captured from that commit's
// code by a throwaway test that called the function through a Queryer recording the text it sent
// and the arguments it bound, and each is held here letter for letter as b459fa923705 wrote it, as
// mainClosureQuery is: a text built from the package's own fragments would move with them, and main
// and the head would then agree however the head's statement changed. The ids a capture bound are
// not in any text: every statement binds them.

// mainEdgesInDocument is readEdgeRows' in read of a document by its ref_key, Query{Direction:
// "in", Kind: "artifact", WholeDocuments: false, ExcludeKinds: ["part_of"]}: the Query
// GET /api/v1/references?to=<document> sends with no ?kind=, which the issue page's Referenced
// by panel and `dispatch read` read. It binds $1 the kind "artifact", $2 the document's ref_key
// and $3 the excluded kinds.
const mainEdgesInDocument = `select kind, other_kind, other_id, created_at, source_seq, at_block from (select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.to_kind = $1 and g.to_id = $2 and not (g.kind = 'mentions' and ((g.from_kind = 'artifact' and exists (
		select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.artifact_id::text = g.from_id)) or (g.from_kind = 'comment' and exists (
		select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.comment_id::text = g.from_id)) or (g.from_kind = 'message' and exists (
		select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.message_id::text = g.from_id)) or (g.from_kind = 'ask' and exists (
		select 1 from ask_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.ask_id::text = g.from_id))))
			union all
			select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, g.to_id
			from graph_edges g where g.to_kind = 'block' and g.to_id in (select d.node_id from document_blocks d join artifacts a on a.id = d.artifact_id where a.ref_key = $2) and not (g.kind = 'mentions' and ((g.from_kind = 'artifact' and exists (
		select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.artifact_id::text = g.from_id)) or (g.from_kind = 'comment' and exists (
		select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.comment_id::text = g.from_id)) or (g.from_kind = 'message' and exists (
		select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.message_id::text = g.from_id)) or (g.from_kind = 'ask' and exists (
		select 1 from ask_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.ask_id::text = g.from_id)))))
		as e (kind, other_kind, other_id, created_at, source_seq, at_block) where true and not (kind = any($3)) order by created_at desc, kind, other_kind, other_id, at_block`

// mainEdgesInWholeDocument is readEdgeRows' in read of a document by its ref_key as
// refs.ReferencedBy sends it, Query{Direction: "in", Kind: "artifact", WholeDocuments: true,
// Kinds: ["mentions"]}, for GET /api/v1/artifacts/{id}/references. It binds $1 the kind
// "artifact", $2 the document's ref_key and $3 the kinds.
const mainEdgesInWholeDocument = `select kind, other_kind, other_id, created_at, source_seq, at_block from (select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.to_kind = $1 and g.to_id = $2 and g.from_kind not in ('block', 'comment_block', 'message_block', 'ask_block')
			union all
			select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, g.to_id
			from graph_edges g where g.to_kind = 'block' and g.to_id in (select d.node_id from document_blocks d join artifacts a on a.id = d.artifact_id where a.ref_key = $2) and g.from_kind not in ('block', 'comment_block', 'message_block', 'ask_block')
			union all
			select r.kind, 'ask', r.ask_id::text, max(r.created_at), max(r.source_seq), null::text
		from ask_block_refs r
		join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
		where b.position is not null
		and r.to_kind = 'artifact' and r.to_id = $2
		group by r.kind, r.ask_id
		union all
		select r.kind, 'ask', r.ask_id::text, max(r.created_at), max(r.source_seq), r.to_id
		from ask_block_refs r
		join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
		where b.position is not null
		and r.to_kind = 'block' and r.to_id in (select d.node_id from document_blocks d join artifacts a on a.id = d.artifact_id where a.ref_key = $2)
		group by r.kind, r.ask_id, r.to_id)
		as e (kind, other_kind, other_id, created_at, source_seq, at_block) where true and kind = any($3) order by created_at desc, kind, other_kind, other_id, at_block`

// mainEdgesInMessage is readEdgeRows' in read of a message by its uuid, Query{Direction: "in",
// Kind: "message", ExcludeKinds: ["part_of"]}: the Query GET /api/v1/references?to=<message>
// sends with no ?kind=, which the issue page's Referenced by panel and `dispatch read` read. It
// binds $1 the kind "message", $2 the message's id, $3 the same id, which the statement casts to
// the uuid the message's blocks are keyed by, and $4 the excluded kinds.
const mainEdgesInMessage = `select kind, other_kind, other_id, created_at, source_seq, at_block from (select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.to_kind = $1 and g.to_id = $2 and not (g.kind = 'mentions' and ((g.from_kind = 'artifact' and exists (
		select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.artifact_id::text = g.from_id)) or (g.from_kind = 'comment' and exists (
		select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.comment_id::text = g.from_id)) or (g.from_kind = 'message' and exists (
		select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.message_id::text = g.from_id)) or (g.from_kind = 'ask' and exists (
		select 1 from ask_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.ask_id::text = g.from_id)))) and not (g.kind = 'replies_to' and g.from_kind = 'message' and g.from_id in (
		select r.id::text from messages r where r.replies_to_block in (select b.node_id from message_blocks b where b.message_id = $3::uuid and b.position is not null)))
			union all
			select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, g.to_id
			from graph_edges g where g.to_kind = 'message_block' and g.to_id in (select b.node_id from message_blocks b where b.message_id = $3::uuid and b.position is not null) and not (g.kind = 'mentions' and ((g.from_kind = 'artifact' and exists (
		select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.artifact_id::text = g.from_id)) or (g.from_kind = 'comment' and exists (
		select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.comment_id::text = g.from_id)) or (g.from_kind = 'message' and exists (
		select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.message_id::text = g.from_id)) or (g.from_kind = 'ask' and exists (
		select 1 from ask_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.ask_id::text = g.from_id)))))
		as e (kind, other_kind, other_id, created_at, source_seq, at_block) where true and not (kind = any($4)) order by created_at desc, kind, other_kind, other_id, at_block`

// mainEdgesInComment is readEdgeRows' in read of a comment by its uuid, Query{Direction: "in",
// Kind: "comment", ExcludeKinds: ["part_of"]}: the Query GET /api/v1/references?to=<comment>
// sends with no ?kind=, which the issue page's Referenced by panel and `dispatch read` read; with
// replies_to neither asked for nor excluded, it folds a reply's answers edge into its replies_to
// edge (foldsAnswers). It binds $1 the kind "comment", $2 the comment's id, $3 the same id, which
// the statement casts to the uuid the comment's blocks are keyed by, and $4 the excluded kinds.
const mainEdgesInComment = `select kind, other_kind, other_id, created_at, source_seq, at_block from (select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.to_kind = $1 and g.to_id = $2 and not (g.kind = 'mentions' and ((g.from_kind = 'artifact' and exists (
		select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.artifact_id::text = g.from_id)) or (g.from_kind = 'comment' and exists (
		select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.comment_id::text = g.from_id)) or (g.from_kind = 'message' and exists (
		select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.message_id::text = g.from_id)) or (g.from_kind = 'ask' and exists (
		select 1 from ask_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.ask_id::text = g.from_id)))) and not (g.kind = 'replies_to' and g.from_kind = 'comment' and g.from_id in (
		select r.id::text from comments r where r.replies_to_block in (select b.node_id from comment_blocks b where b.comment_id = $3::uuid and b.position is not null)))
			union all
			select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, g.to_id
			from graph_edges g where g.to_kind = 'comment_block' and g.to_id in (select b.node_id from comment_blocks b where b.comment_id = $3::uuid and b.position is not null) and not (g.kind = 'mentions' and ((g.from_kind = 'artifact' and exists (
		select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.artifact_id::text = g.from_id)) or (g.from_kind = 'comment' and exists (
		select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.comment_id::text = g.from_id)) or (g.from_kind = 'message' and exists (
		select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.message_id::text = g.from_id)) or (g.from_kind = 'ask' and exists (
		select 1 from ask_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.ask_id::text = g.from_id)))))
		as e (kind, other_kind, other_id, created_at, source_seq, at_block) where true and not (kind = any($4)) and not (kind = 'answers' and exists (select 1 from comments c where c.id::text = other_id and (c.reply_to::text = $2 or c.replies_to_block in (select b.node_id from comment_blocks b where b.comment_id = $3::uuid and b.position is not null)))) order by created_at desc, kind, other_kind, other_id, at_block`

// mainEdgesInAsk is readEdgeRows' in read of an ask by its uuid with no Kinds and no
// ExcludeKinds, Query{Direction: "in", Kind: "ask"}: the read of a caller of refs.Edges that
// names no kind and excludes none. No route sends it for an ask, since GET /api/v1/references
// excludes AskBacklinkExclusions from an ask's in read whenever ?kind= is empty
// (mainEdgesInAskPanel). It binds $1 the kind "ask", $2 the ask's id and $3 the same id, which
// the statement casts to the uuid the ask's parts are keyed by.
const mainEdgesInAsk = `select kind, other_kind, other_id, created_at, source_seq, at_block from (select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.to_kind = $1 and g.to_id = $2 and not (g.kind = 'mentions' and ((g.from_kind = 'artifact' and exists (
		select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.artifact_id::text = g.from_id)) or (g.from_kind = 'comment' and exists (
		select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.comment_id::text = g.from_id)) or (g.from_kind = 'message' and exists (
		select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.message_id::text = g.from_id)) or (g.from_kind = 'ask' and exists (
		select 1 from ask_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.ask_id::text = g.from_id)))) and not (g.kind = 'replies_to' and g.from_kind = 'comment' and g.from_id in (
		select r.id::text from comments r where r.replies_to_block in (select b.node_id from ask_blocks b where b.ask_id = $3::uuid and b.position is not null)))
			union all
			select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, g.to_id
			from graph_edges g where g.to_kind = 'ask_block' and g.to_id in (select b.node_id from ask_blocks b where b.ask_id = $3::uuid and b.position is not null) and not (g.kind = 'mentions' and ((g.from_kind = 'artifact' and exists (
		select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.artifact_id::text = g.from_id)) or (g.from_kind = 'comment' and exists (
		select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.comment_id::text = g.from_id)) or (g.from_kind = 'message' and exists (
		select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.message_id::text = g.from_id)) or (g.from_kind = 'ask' and exists (
		select 1 from ask_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.ask_id::text = g.from_id)))))
		as e (kind, other_kind, other_id, created_at, source_seq, at_block) where true order by created_at desc, kind, other_kind, other_id, at_block`

// mainEdgesInAskPanel is readEdgeRows' in read of an ask by its uuid, Query{Direction: "in", Kind:
// "ask", ExcludeKinds: AskBacklinkExclusions}: the Query GET /api/v1/references?to=<ask> sends
// with no ?kind=, which the ask's Referenced by panel on the issue page and `dispatch read --ask`
// read. Its text is mainEdgesInAsk's with the exclusion added. It binds $1 the kind "ask", $2 the
// ask's id, $3 the same id, which the statement casts to the uuid the ask's parts are keyed by,
// and $4 the excluded kinds, replies_to and part_of.
const mainEdgesInAskPanel = `select kind, other_kind, other_id, created_at, source_seq, at_block from (select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.to_kind = $1 and g.to_id = $2 and not (g.kind = 'mentions' and ((g.from_kind = 'artifact' and exists (
		select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.artifact_id::text = g.from_id)) or (g.from_kind = 'comment' and exists (
		select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.comment_id::text = g.from_id)) or (g.from_kind = 'message' and exists (
		select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.message_id::text = g.from_id)) or (g.from_kind = 'ask' and exists (
		select 1 from ask_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.ask_id::text = g.from_id)))) and not (g.kind = 'replies_to' and g.from_kind = 'comment' and g.from_id in (
		select r.id::text from comments r where r.replies_to_block in (select b.node_id from ask_blocks b where b.ask_id = $3::uuid and b.position is not null)))
			union all
			select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, g.to_id
			from graph_edges g where g.to_kind = 'ask_block' and g.to_id in (select b.node_id from ask_blocks b where b.ask_id = $3::uuid and b.position is not null) and not (g.kind = 'mentions' and ((g.from_kind = 'artifact' and exists (
		select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.artifact_id::text = g.from_id)) or (g.from_kind = 'comment' and exists (
		select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.comment_id::text = g.from_id)) or (g.from_kind = 'message' and exists (
		select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.message_id::text = g.from_id)) or (g.from_kind = 'ask' and exists (
		select 1 from ask_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.ask_id::text = g.from_id)))))
		as e (kind, other_kind, other_id, created_at, source_seq, at_block) where true and not (kind = any($4)) order by created_at desc, kind, other_kind, other_id, at_block`

// mainEdgesInIssue is readEdgeRows' in read of an issue by its key with no Kinds and no
// ExcludeKinds, Query{Direction: "in", Kind: "issue"}: the Query GET /api/v1/references?to=<issue>
// sends with no ?kind=, which the issue page's Referenced by panel and `dispatch read` read. Its
// one arm is the first arm of every in read, with heldByABlock's probe and no arm over blocks, so
// it is also the text of the in read of a component, a session, a block, an ask's part and a
// comment's or a message's block, each with its own kind bound as $1. It binds $1 the kind
// "issue" and $2 the issue's key. It was captured as the constants above were, from graph.go as
// b459fa923705 holds it, by a throwaway test that compared readEdgeRows' text for each of those
// kinds with it, letter for letter. It is the oracle of c-graph-edges' probe case.
const mainEdgesInIssue = `select kind, other_kind, other_id, created_at, source_seq, at_block from (select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.to_kind = $1 and g.to_id = $2 and not (g.kind = 'mentions' and ((g.from_kind = 'artifact' and exists (
		select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.artifact_id::text = g.from_id)) or (g.from_kind = 'comment' and exists (
		select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.comment_id::text = g.from_id)) or (g.from_kind = 'message' and exists (
		select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.message_id::text = g.from_id)) or (g.from_kind = 'ask' and exists (
		select 1 from ask_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id
		and b.ask_id::text = g.from_id)))))
		as e (kind, other_kind, other_id, created_at, source_seq, at_block) where true order by created_at desc, kind, other_kind, other_id, at_block`

// mainLoadCommentNodes is loadCommentNodes' statement, which resolveNodes sends for every
// comment at the other end of an edge (refs.Edges, refs.Node, refs.TargetNames). It binds $1 the
// comments' ids, as text.
const mainLoadCommentNodes = `
		select c.id::text, c.issue_key, coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, ''), c.body,
		       c.author, c.created_at, c.edited_at, c.reply_to::text, c.replies_to_block, c.ask_id::text, doc.id::text, doc.issue_key, coalesce(doc.project_key, ''), doc.session_id, doc.slug, doc.kind, doc.is_primary,
		       case when c.anchor->>'artifact_id' is not null then case when c.anchor->>'version' ~ '^[1-9][0-9]{0,8}$' then (c.anchor->>'version')::int end else c.written_version end
		from comments c
		left join issues i on i.key = c.issue_key
		left join artifacts a on a.id = c.artifact_id
		left join artifacts doc on doc.id = coalesce((c.anchor->>'artifact_id')::uuid, c.artifact_id)
		where c.id::text = any($1)`

// mainLoadAskNodes is loadAskNodes' statement, which resolveNodes sends for every ask at the
// other end of an edge (refs.Edges, refs.Node, refs.TargetNames). It binds $1 the asks' ids, as
// text.
const mainLoadAskNodes = `
		select k.id::text, k.issue_key, coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, ''), k.question,
		       k.author, k.created_at, k.edited_at, edit.edited_by, doc.id::text, doc.issue_key, coalesce(doc.project_key, ''), doc.session_id, doc.slug, doc.kind, doc.is_primary,
		       case
		         when (case when pg_input_is_valid(k.anchor->>'artifact_id', 'uuid') then (k.anchor->>'artifact_id')::uuid end) is not null then case when k.anchor->>'version' ~ '^[1-9][0-9]{0,8}$' then (k.anchor->>'version')::int end
		         when k.block_artifact_id is not null then case when first.known then first.version end
		         else k.written_version
		       end
		from asks k
		left join issues i on i.key = k.issue_key
		left join artifacts a on a.id = k.artifact_id
		left join artifacts doc on doc.id = case
		  when (case when pg_input_is_valid(k.anchor->>'artifact_id', 'uuid') then (k.anchor->>'artifact_id')::uuid end) is not null then (case when pg_input_is_valid(k.anchor->>'artifact_id', 'uuid') then (k.anchor->>'artifact_id')::uuid end)
		  when k.block_artifact_id is not null then k.block_artifact_id
		  when k.kind = 'approval' then (k.approval->>'artifact_id')::uuid
		  else k.artifact_id
		end
		left join lateral (
			select e.payload->'edited_by' as edited_by from events e
			where e.type in ('ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited') and e.type = 'ask.edited'
			  and e.payload->>'id' = k.id::text
			order by e.id desc limit 1
		) edit on k.edited_at is not null
		left join lateral (
		select t.version,
		       t.version = 1 or exists (
		         select 1 from document_indexed_versions i where i.artifact_id = t.artifact_id and i.version = t.version - 1
		       ) as known,
		       v.authors, v.created_at
		from document_block_texts t
		left join artifact_versions v on v.artifact_id = t.artifact_id and v.number = t.version
		where t.artifact_id = k.block_artifact_id and t.block_id = k.block_id and t.text is not null
		order by t.version limit 1
	) first on true
		where k.id::text = any($1)`

// mainLoadMessageNodes is loadMessageNodes' statement, which resolveNodes sends for every message
// at the other end of an edge (refs.Edges, refs.Node, refs.TargetNames). It binds $1 the messages'
// ids, as text.
const mainLoadMessageNodes = `
		select m.id::text, m.issue_key, coalesce(i.project_key, ''), m.body, m.author, m.created_at, m.in_reply_to::text, m.replies_to_block
		from messages m
		left join issues i on i.key = m.issue_key
		where m.id::text = any($1)`

// mainLoadBlockNodes is loadBlockNodes' statement, which resolveNodes sends for every block of a
// document at the other end of an edge, and for the blocks a document's read lists edges at
// (refs.Edges, refs.Node, refs.TargetNames). It binds $1 the blocks' node ids,
// '<document ref_key>#<block id>'.
const mainLoadBlockNodes = `
		select d.node_id, d.block_id, d.excerpt, d.changed_version, d.changed_at, changed.authors,
		       a.id::text, a.issue_key, coalesce(a.project_key, ''), a.session_id, a.slug, a.kind, a.is_primary, first.version, first.known, first.authors, first.created_at
		from document_blocks d join artifacts a on a.id = d.artifact_id
		left join artifact_versions changed on changed.artifact_id = d.artifact_id and changed.number = d.changed_version
		left join lateral (
		select t.version,
		       t.version = 1 or exists (
		         select 1 from document_indexed_versions i where i.artifact_id = t.artifact_id and i.version = t.version - 1
		       ) as known,
		       v.authors, v.created_at
		from document_block_texts t
		left join artifact_versions v on v.artifact_id = t.artifact_id and v.number = t.version
		where t.artifact_id = d.artifact_id and t.block_id = d.block_id and t.text is not null
		order by t.version limit 1
	) first on true
		where d.node_id = any($1)`

// TestEdgesIntoANodesBlocksAnswerWhatMainsStatementDid is c-graph-edges. refs.Edges' in read of a
// document (WholeDocuments false, and true with Kinds mentions), a message, a comment and an ask
// (with no exclusion, and with AskBacklinkExclusions as its panel reads it) answers, row for row
// and in order, what main's statement answered over the same data: the raw rows of the statement
// Edges sends first, run again, against main's text held above. The rows are compared before Edges
// resolves them, because a source block with no position resolves to no node, so a comparison of
// resolved edges would miss a statement that lists one.
//
// The arms case holds graph_edges' arms that can end at a block to edgesBlockArms, so an arm a
// later migration adds fails it until it is listed; the fixture writes a link of each arm into
// each read node's blocks. The plan of each of the six statements, read under explain (analyze)
// with sequential scans off and under the default settings, reads every link table that it ran a
// scan of by target, with an Index Cond on to_id; reads no blocks table whole, and none into a
// hash join but by an index probe; and reads comments and messages by replies_to_block wherever
// it tests it. The count case holds the plan's reads of each link table to the rows of that table
// the fixture holds whose target is the node read or one of its current blocks. heldByABlock's
// probe of block_refs, in every plan the test reads, keys the source document: its Index Cond
// holds to_kind, to_id and (artifact_id)::text, and it removes no row by a filter. The probe case
// reads the in read of an issue that edgesProbeDocuments documents mention in their text and in
// two of their blocks each, the kinds case the in read of a node of every kind that carries the
// probe, and the index case block_refs' indexes. Every table the plans read holds at least
// edgesOutsideNodes rows, written by COPY and analyzed, so the default settings plan from real
// sizes. With -v the test logs the head's in statement of a document, on one line that starts
// "head in statement, document:", for c-production-load.
func TestEdgesIntoANodesBlocksAnswerWhatMainsStatementDid(t *testing.T) {
	fixture := newEdgesFixture(t)
	pool := fixture.database.Pool
	every := append(append(slices.Clone(fixture.reads), fixture.probe), fixture.kinds...)
	for index := range every {
		read := &every[index]
		read.statement = edgesStatement(t, pool, read.query)
		for setting, settings := range edgesPlanSettings {
			read.plans[setting] = explainAnalyzed(t, pool, read.statement, settings.settings...)
		}
	}
	reads, probe, kinds := every[:len(fixture.reads)], every[len(fixture.reads)], every[len(fixture.reads)+1:]
	keys := explaintest.NewIndexKeys(pool)

	t.Run("arms", func(t *testing.T) {
		got, want := graphViewBlockArms(t, pool), make([]string, 0, len(edgesBlockArms))
		for _, arm := range edgesBlockArms {
			want = append(want, arm.String())
		}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("graph_edges' arms that can end at a block are not edgesBlockArms.\nthe view:\n  %s\nthe list:\n  %s\n"+
				"List each new arm in edgesBlockArms, read it by the node's block ids in refs.Edges' in read, and write a link of it into each read node's blocks in the fixture.",
				strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	})

	t.Run("outside graph", func(t *testing.T) {
		for _, subject := range fixture.subjects {
			counts := blockLinkCounts(t, pool, subject)
			tables := slices.Clone(edgesLinkTables)
			if replies, ok := edgesReplyTables[subject.blockKind]; ok {
				tables = append(tables, replies)
			}
			for _, table := range tables {
				inside, outside := counts[table][0], counts[table][1]
				if inside == 0 || outside < 5*inside {
					t.Errorf("the fixture holds %d links of %s into the blocks of %s and %d into other blocks of kind %s; want at least one, and at least five times as many outside", inside, table, subject.node, outside, subject.blockKind)
				}
			}
		}
	})

	t.Run("edges", func(t *testing.T) {
		for _, read := range reads {
			t.Run(read.name, func(t *testing.T) {
				if read.query.Kind == "artifact" && !read.query.WholeDocuments {
					// One line, which c-production-load looks for: strings.Fields cuts the
					// statement at every run of spaces, tabs and newlines, and the words are joined
					// again by one space each.
					t.Logf("head in statement, document: %s", strings.Join(strings.Fields(read.statement.sql), " "))
				}
				requireMainsRows(t, pool, read)
			})
		}
	})

	t.Run("plan", func(t *testing.T) {
		for _, read := range reads {
			for setting, settings := range edgesPlanSettings {
				t.Run(read.name+"/"+settings.name, func(t *testing.T) {
					requireKeyedReads(t, keys, read.plans[setting])
				})
			}
		}
	})

	t.Run("count", func(t *testing.T) {
		for _, read := range reads {
			counts := edgesTargetCounts(t, pool, read.query)
			for setting, settings := range edgesPlanSettings {
				t.Run(read.name+"/"+settings.name, func(t *testing.T) {
					requireCountedReads(t, read.plans[setting], counts)
				})
			}
		}
	})

	t.Run("probe rule", func(t *testing.T) {
		for _, read := range reads {
			for setting, settings := range edgesPlanSettings {
				t.Run(read.name+"/"+settings.name, func(t *testing.T) {
					requireProbeRule(t, read.plans[setting], read.probes)
				})
			}
		}
	})

	t.Run("probe case", func(t *testing.T) {
		t.Run("rows", func(t *testing.T) {
			requireMainsRows(t, pool, probe)
		})
		for setting, settings := range edgesPlanSettings {
			t.Run(settings.name, func(t *testing.T) {
				requireProbeRule(t, probe.plans[setting], probe.probes)
			})
		}
	})

	t.Run("kinds", func(t *testing.T) {
		t.Run("list", func(t *testing.T) {
			want := append(slices.Clone(nodeKinds), edgesWholeDocumentRead)
			slices.Sort(want)
			if !slices.Equal(edgesProbeCarryingReads, want) {
				t.Fatalf("the reads that carry heldByABlock's probe are %v; nodeKinds and the whole-document read are %v", edgesProbeCarryingReads, want)
			}
		})
		for _, member := range kinds {
			t.Run(member.name, func(t *testing.T) {
				t.Run("rows", func(t *testing.T) {
					requireMainsRows(t, pool, member)
				})
				for setting, settings := range edgesPlanSettings {
					t.Run(settings.name, func(t *testing.T) {
						requireProbeRule(t, member.plans[setting], member.probes)
					})
				}
			})
		}
	})

	t.Run("index", func(t *testing.T) {
		requireBlockRefsTargetIndex(t, pool)
	})

	t.Run("tables", func(t *testing.T) {
		// Every relation a plan of the test scanned, in a scan that ran, holds at least
		// edgesOutsideNodes rows as the planner counts them: pg_class.reltuples, which the
		// fixture's analyze wrote.
		relations := map[string]bool{}
		for _, read := range every {
			for _, plan := range read.plans {
				for _, scan := range explaintest.Scans(t, plan) {
					if scan.Loops > 0 {
						relations[scan.Relation] = true
					}
				}
			}
		}
		for _, relation := range slices.Sorted(maps.Keys(relations)) {
			var tuples float64
			if err := pool.QueryRow(context.Background(), "select reltuples::float8 from pg_class where oid = $1::regclass", relation).Scan(&tuples); err != nil {
				t.Fatalf("read the size of %s: %v", relation, err)
			}
			if tuples < edgesOutsideNodes {
				t.Errorf("the statements read %s, which the planner counts %g rows of; want at least %d, so the default settings plan from a real size", relation, tuples, edgesOutsideNodes)
			}
		}
	})
}

// edgesOutsideNodes is how many issues the outside graph holds, and under them as many
// documents, asks, comments and messages, each with one block and one link from it: the least
// number of rows every table the statements read holds.
const edgesOutsideNodes = 10000

// edgesProbeDocuments is how many documents mention the issue of the probe case, each in its own
// text and in two of its blocks.
const edgesProbeDocuments = 100

// edgesWholeDocumentRead names, in the kinds case, the in read of a document with WholeDocuments
// true, which runs no probe.
const edgesWholeDocumentRead = "whole document"

// edgesProbeCarryingReads is the list "The in reads that carry heldByABlock's probe" of the plan of
// the graph read's one-hop change: refs.Edges' in read of a node of every kind nodeRegistry
// resolves, whose first arm carries the probe for every kind, and the whole-document read, which
// carries none. The kinds case compares it with nodeKinds first, so a kind a later build resolves
// fails it until it is listed here, and the fixture writes a node of each. The store's own kinds
// (graphstore.KindPerson and its three siblings) are read as an issue is, by the node's address,
// and so are the kinds of a lens and what stands under it (graphstore.KindInteraction and the
// kinds beside it), which no read of blocks covers.
var edgesProbeCarryingReads = []string{
	"artifact", "ask", "ask_block", "block", "comment", "comment_block", "component",
	graphstore.KindConstraint, graphstore.KindInteraction, "issue", graphstore.KindLens, graphstore.KindLensBlock,
	graphstore.KindLensKind, "message", "message_block", "page", "person", graphstore.KindRisk, "role", "session",
	edgesWholeDocumentRead,
}

// edgesPlanSettings are the two settings each plan is read under: sequential scans off, which
// leaves the planner only the reads an index gives it, and the default settings, which plan from
// the fixture's analyzed sizes. JIT is off under both, as on production's database (D11 of the plan
// of the graph read's one-hop change): it changes how a plan runs, not which plan is chosen.
var edgesPlanSettings = [2]struct {
	name     string
	settings []string
}{
	{name: "enable_seqscan off", settings: []string{"set local jit = off", "set local enable_seqscan = off"}},
	{name: "default settings", settings: []string{"set local jit = off"}},
}

// edgesLinkTables are the tables that store a link, and edgesBlockTables the tables of the blocks
// a link starts or ends at: the list "The link tables the plan rule and the count judge" of the
// plan of this change, with edgesReplyTables, the table of the replies that can name a block of
// each kind: a comment answers a comment's block and an ask's part, a message a message's block,
// and nothing answers a document's block.
var (
	edgesLinkTables  = []string{"refs", "block_refs", "ask_block_refs", "comment_block_refs", "message_block_refs"}
	edgesBlockTables = []string{"document_blocks", "ask_blocks", "comment_blocks", "message_blocks"}
	edgesReplyTables = map[string]string{"ask_block": "comments", "comment_block": "comments", "message_block": "messages"}
)

// edgesSourceColumns names, for each link table, the column that keys the item its links start
// in, which heldByABlock's probe of that table tests (blockHolds): an Index Cond that names it is
// a probe's. refs has none.
var edgesSourceColumns = map[string]*regexp.Regexp{
	"refs":               nil,
	"block_refs":         namesSourceDocument,
	"ask_block_refs":     regexp.MustCompile(`\bask_id\b`),
	"comment_block_refs": regexp.MustCompile(`\bcomment_id\b`),
	"message_block_refs": regexp.MustCompile(`\bmessage_id\b`),
}

// edgesReplyIndexes are the indexes that key comments and messages by the block a reply names
// (migration 0142). Each is partial on replies_to_block is not null, so a scan through one tests
// replies_to_block though it may print no condition: the index's predicate holds it.
var edgesReplyIndexes = []string{"comments_replies_to_block", "messages_replies_to_block"}

// edgesBlockArm is one arm of graph_edges that can end at a block: the arm as parseGraphViewArms
// reads it, and its FROM clause as pg_get_viewdef prints it, every run of white space one space,
// which holds the rule the arm keeps for a current block at its source end.
type edgesBlockArm struct {
	arm  graphViewArm
	from string
}

func (a edgesBlockArm) String() string {
	return a.arm.String() + ": " + a.from
}

// edgesBlockArms is the list "The arms that can end at a block" of the plan of this change: the
// arms of graph_edges whose to_kind is a column or one of blockNodeKinds. They are refs, over the
// rows the store states no tuple of (store.ReferenceNotStated); block_refs, whose typed pointer is
// listed only where block_ref_listed allows its target, over the rows that name no node;
// comment_block_refs, message_block_refs and ask_block_refs, each joined to its source block,
// which must hold a position; a comment's reply to a comment's block and to an ask's part, told
// apart by the ask/ that starts an ask part's node id; a message's reply to a message's block; the
// store's own arm over graph_tuples (migrations 20261121034242 and
// 20261121090000_issue_tree_graph_edges_after_tuple_arm_filter, restated by
// 20261125150001_issue_tree_graph_edges_block_node_listed to test a pointer's target through
// block_node_listed, R26 of the issue-tree spec), which answers the links of the block_refs rows
// that name their node, a block's link and an answered ask's shaped among them (keyedTupleArm), a
// document's mention where the document's row names its node (keyedMentionArm), and the relations
// it answers from the tuples alone (keyedStoreArm); and its arm over the tuples of a lens's
// findings (keyedFindingArm). refs.Edges' in read reads each of them by the node's block ids, and
// the fixture writes a link of each into each read node's blocks. The arms case holds the view to
// this list, so an arm a later migration adds fails it until it is listed here, read by block ids
// and written in the fixture.
var edgesBlockArms = []edgesBlockArm{
	{
		arm: graphViewArm{table: "refs"},
		from: "FROM refs r WHERE r.from_kind <> 'artifact'::text OR octet_length(r.to_id) > 1024 OR NOT (EXISTS ( SELECT 1 FROM artifacts mentioning " +
			"WHERE mentioning.id::text = r.from_id AND mentioning.node_kind IS NOT NULL OFFSET 0))",
	},
	{
		arm:  graphViewArm{table: "block_refs", fromKind: "block"},
		from: "FROM block_refs WHERE (block_refs.to_types IS NULL OR block_ref_listed(block_refs.to_types, block_refs.to_id)) AND block_refs.node_kind IS NULL",
	},
	{
		arm: graphViewArm{table: "graph_tuples"},
		from: `FROM ( SELECT DISTINCT ON (tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id) tuple.from_kind, tuple.from_id, tuple.node_id, CASE` +
			` WHEN tuple.kind = 'in_document'::text THEN 'part_of'::text WHEN tuple.from_kind = 'issue'::text AND tuple.kind = 'needs'::text AND tuple.to_kind = 'i` +
			`ssue'::text THEN 'blocked_by'::text ELSE tuple.kind END AS kind, tuple.to_kind, tuple.to_id, tuple.at AS created_at, tuple.legacy_source_seq AS source` +
			`_seq, tuple.kind AS stated, tuple.asserted, CASE WHEN tuple.from_kind = 'block'::text AND tuple.kind = 'in_document'::text THEN 'block'::text WHEN NOT` +
			` (tuple.from_kind = 'block'::text AND tuple.from_id ~~ 'lens/%'::text) AND ((tuple.kind = ANY (ARRAY['changed_by_run'::text, 'has_kind'::text, 'trigge` +
			`red_by'::text, 'constrained_by'::text, 'prevented_by'::text])) OR (tuple.kind = ANY (ARRAY['part_of'::text, 'authored_by'::text])) AND (tuple.from_kin` +
			`d = ANY (ARRAY['interaction'::text, 'lens'::text, 'risk'::text, 'constraint'::text]))) THEN 'store'::text WHEN tuple.from_kind = 'issue'::text AND (tu` +
			`ple.kind = ANY (ARRAY['duplicate_of'::text, 'filed_from'::text])) THEN 'store'::text WHEN tuple.from_kind = 'issue'::text AND tuple.kind = 'child_of':` +
			`:text AND tuple.to_kind = 'issue'::text THEN 'issue_parent'::text WHEN tuple.from_kind = 'issue'::text AND tuple.kind = 'needs'::text AND tuple.to_kin` +
			`d = 'issue'::text THEN 'issue_link'::text WHEN tuple.from_kind = 'component'::text AND tuple.kind = 'part_of'::text AND tuple.to_kind = 'component'::t` +
			`ext THEN 'component_parent'::text WHEN tuple.from_kind = 'component'::text AND tuple.kind = 'depends_on'::text AND tuple.to_kind = 'component'::text T` +
			`HEN 'component_depends'::text WHEN tuple.from_kind = 'issue'::text AND tuple.kind = 'affects'::text AND tuple.to_kind = 'component'::text THEN 'compon` +
			`ent_member'::text WHEN tuple.from_kind = 'artifact'::text AND tuple.kind = 'mentions'::text THEN 'artifact_ref'::text WHEN tuple.from_kind = 'block'::` +
			`text AND tuple.kind <> 'part_of'::text THEN 'block_ref'::text WHEN tuple.from_kind = 'comment'::text AND (tuple.kind = ANY (ARRAY['anchored_to'::text,` +
			` 'owned_by'::text])) THEN 'comment'::text WHEN tuple.from_kind = 'ask'::text AND (tuple.kind = ANY (ARRAY['anchored_to'::text, 'owned_by'::text])) THE` +
			`N 'ask'::text WHEN tuple.from_kind = 'ask'::text AND tuple.kind = 'followed_by'::text THEN 'ask_follower'::text WHEN tuple.from_kind = 'ask'::text AND` +
			` tuple.kind = 'shaped'::text THEN 'block_ref'::text ELSE NULL::text END AS projects FROM ( SELECT tuple_1.from_kind, tuple_1.from_id, tuple_1.from_id ` +
			`AS node_id, tuple_1.kind, tuple_1.to_kind, tuple_1.to_id, tuple_1.at, tuple_1.legacy_source_seq, tuple_1.asserted, tuple_1.seq FROM graph_tuples tuple` +
			`_1 WHERE tuple_1.from_kind <> 'artifact'::text OR tuple_1.kind <> 'mentions'::text UNION ALL SELECT 'artifact'::text, mentioning.id::text AS id, tuple` +
			`_1.from_id, tuple_1.kind, tuple_1.to_kind, tuple_1.to_id, projected.created_at, projected.source_seq, tuple_1.asserted, tuple_1.seq FROM graph_tuples ` +
			`tuple_1 JOIN artifacts mentioning ON mentioning.ref_key = tuple_1.from_id AND mentioning.node_kind IS NOT NULL CROSS JOIN LATERAL ( SELECT stated.crea` +
			`ted_at, stated.source_seq FROM refs stated WHERE stated.from_kind = tuple_1.from_kind AND stated.from_id = mentioning.id::text AND stated.to_kind = tu` +
			`ple_1.to_kind AND stated.to_id = tuple_1.to_id OFFSET 0) projected WHERE tuple_1.from_kind = 'artifact'::text AND tuple_1.kind = 'mentions'::text) tup` +
			`le WHERE tuple.to_kind <> 'value'::text AND (tuple.from_kind = 'block'::text AND tuple.kind = 'in_document'::text OR NOT (tuple.from_kind = 'block'::t` +
			`ext AND tuple.from_id ~~ 'lens/%'::text) AND ((tuple.kind = ANY (ARRAY['changed_by_run'::text, 'has_kind'::text, 'triggered_by'::text, 'constrained_by` +
			`'::text, 'prevented_by'::text])) OR (tuple.kind = ANY (ARRAY['part_of'::text, 'authored_by'::text])) AND (tuple.from_kind = ANY (ARRAY['interaction'::` +
			`text, 'lens'::text, 'risk'::text, 'constraint'::text]))) OR tuple.from_kind = 'issue'::text AND (tuple.kind = ANY (ARRAY['duplicate_of'::text, 'filed_` +
			`from'::text])) OR tuple.from_kind = 'issue'::text AND tuple.kind = 'child_of'::text AND tuple.to_kind = 'issue'::text OR tuple.from_kind = 'issue'::te` +
			`xt AND tuple.kind = 'needs'::text AND tuple.to_kind = 'issue'::text OR tuple.from_kind = 'component'::text AND tuple.kind = 'part_of'::text AND tuple.` +
			`to_kind = 'component'::text OR tuple.from_kind = 'component'::text AND tuple.kind = 'depends_on'::text AND tuple.to_kind = 'component'::text OR tuple.` +
			`from_kind = 'issue'::text AND tuple.kind = 'affects'::text AND tuple.to_kind = 'component'::text OR tuple.from_kind = 'artifact'::text AND tuple.kind ` +
			`= 'mentions'::text OR tuple.from_kind = 'block'::text AND tuple.kind <> 'part_of'::text OR tuple.from_kind = 'comment'::text AND (tuple.kind = ANY (AR` +
			`RAY['anchored_to'::text, 'owned_by'::text])) OR tuple.from_kind = 'ask'::text AND (tuple.kind = ANY (ARRAY['anchored_to'::text, 'owned_by'::text])) OR` +
			` tuple.from_kind = 'ask'::text AND tuple.kind = 'followed_by'::text OR tuple.from_kind = 'ask'::text AND tuple.kind = 'shaped'::text) ORDER BY tuple.t` +
			`o_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id, tuple.seq DESC) edge JOIN graph_nodes node ON node.kind = edge.from_kind AND node.id ` +
			`= edge.node_id WHERE edge.asserted AND node.state <> 'gone'::text AND (edge.projects <> 'block_ref'::text OR edge.stated = 'mentions'::text OR block_n` +
			`ode_listed(block_pointer_targets(edge.from_id, edge.stated), edge.to_id)) AND CASE edge.projects WHEN 'store'::text THEN true WHEN 'block'::text THEN (` +
			`EXISTS ( SELECT 1 FROM document_blocks projected WHERE projected.node_kind = edge.from_kind AND projected.node_address = edge.from_id OFFSET 0)) WHEN ` +
			`'issue_parent'::text THEN (EXISTS ( SELECT 1 FROM issues projected WHERE projected.key = edge.from_id AND projected.node_kind = edge.from_kind AND pro` +
			`jected.node_address = edge.from_id AND projected.parent_key = edge.to_id OFFSET 0)) WHEN 'issue_link'::text THEN (EXISTS ( SELECT 1 FROM issue_links p` +
			`rojected WHERE projected.issue_key = edge.from_id AND projected.kind = 'blocked_by'::text AND projected.target_key = edge.to_id AND projected.node_kin` +
			`d = edge.from_kind AND projected.node_address = edge.from_id OFFSET 0)) WHEN 'component_parent'::text THEN (EXISTS ( SELECT 1 FROM components projecte` +
			`d WHERE ((projected.project_key || '/'::text) || projected.id) = edge.from_id AND projected.node_kind = edge.from_kind AND projected.node_address = ed` +
			`ge.from_id AND ((projected.project_key || '/'::text) || projected.parent) = edge.to_id OFFSET 0)) WHEN 'component_depends'::text THEN (EXISTS ( SELECT` +
			` 1 FROM component_depends projected WHERE ((projected.project_key || '/'::text) || projected.from_id) = edge.from_id AND ((projected.project_key || '/` +
			`'::text) || projected.to_id) = edge.to_id AND projected.node_kind = edge.from_kind AND projected.node_address = edge.from_id OFFSET 0)) WHEN 'componen` +
			`t_member'::text THEN (EXISTS ( SELECT 1 FROM issue_component_members projected WHERE projected.issue_key = edge.from_id AND ((projected.project_key ||` +
			` '/'::text) || projected.component_id) = edge.to_id AND projected.node_kind = edge.from_kind AND projected.node_address = edge.from_id OFFSET 0)) WHEN` +
			` 'artifact_ref'::text THEN true WHEN 'block_ref'::text THEN (EXISTS ( SELECT 1 FROM block_refs projected WHERE projected.node_kind = edge.from_kind AN` +
			`D projected.node_address = edge.from_id AND projected.kind = edge.stated AND projected.to_kind = edge.to_kind AND projected.to_id = edge.to_id OFFSET ` +
			`0)) WHEN 'comment'::text THEN (EXISTS ( SELECT 1 FROM comments projected WHERE projected.node_kind = edge.from_kind AND projected.node_address = edge.` +
			`from_id OFFSET 0)) WHEN 'ask'::text THEN (EXISTS ( SELECT 1 FROM asks projected WHERE projected.node_kind = edge.from_kind AND projected.node_address ` +
			`= edge.from_id OFFSET 0)) WHEN 'ask_follower'::text THEN (EXISTS ( SELECT 1 FROM ask_followers projected WHERE projected.node_kind = edge.from_kind AN` +
			`D projected.node_address = edge.from_id AND projected.session_id = edge.to_id OFFSET 0)) ELSE NULL::boolean END`,
	},
	{
		arm: graphViewArm{table: "graph_tuples", fromKind: graphstore.KindLensBlock},
		from: `FROM ( SELECT DISTINCT ON (tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id) tuple.from_kind, tuple.from_id, tuple.kind, tuple.t` +
			`o_kind, tuple.to_id, tuple.at AS created_at, tuple.legacy_source_seq AS source_seq, tuple.asserted FROM graph_tuples tuple WHERE tuple.from_kind = 'bl` +
			`ock'::text AND tuple.from_id ~~ 'lens/%'::text AND (tuple.kind = ANY (ARRAY['part_of'::text, 'authored_by'::text, 'changed_by_run'::text, 'has_kind'::` +
			`text, 'triggered_by'::text, 'constrained_by'::text, 'prevented_by'::text])) AND tuple.to_kind <> 'value'::text ORDER BY tuple.to_kind, tuple.to_id, tu` +
			`ple.kind, tuple.from_kind, tuple.from_id, tuple.seq DESC) finding JOIN graph_nodes node ON node.kind = finding.from_kind AND node.id = finding.from_id` +
			` WHERE finding.asserted AND node.state <> 'gone'::text`,
	},
	{
		arm:  graphViewArm{table: "comment_block_refs", fromKind: "comment_block"},
		from: `FROM comment_block_refs r JOIN comment_blocks b ON b.comment_id = r.comment_id AND b.block_id = r.block_id WHERE b."position" IS NOT NULL`,
	},
	{
		arm:  graphViewArm{table: "message_block_refs", fromKind: "message_block"},
		from: `FROM message_block_refs r JOIN message_blocks b ON b.message_id = r.message_id AND b.block_id = r.block_id WHERE b."position" IS NOT NULL`,
	},
	{
		arm:  graphViewArm{table: "ask_block_refs", fromKind: "ask_block"},
		from: `FROM ask_block_refs r JOIN ask_blocks b ON b.ask_id = r.ask_id AND b.block_id = r.block_id WHERE b."position" IS NOT NULL`,
	},
	{
		arm:  graphViewArm{table: "comments", fromKind: "comment", kind: "replies_to", toKind: "comment_block"},
		from: `FROM comments c WHERE c.replies_to_block IS NOT NULL AND c.replies_to_block !~~ 'ask/%'::text`,
	},
	{
		arm:  graphViewArm{table: "comments", fromKind: "comment", kind: "replies_to", toKind: "ask_block"},
		from: `FROM comments c WHERE c.replies_to_block IS NOT NULL AND c.replies_to_block ~~ 'ask/%'::text`,
	},
	{
		arm:  graphViewArm{table: "messages", fromKind: "message", kind: "replies_to", toKind: "message_block"},
		from: "FROM messages m WHERE m.replies_to_block IS NOT NULL",
	},
}

// An Index Cond keys a column when one of its comparisons has that column on the left, which is
// where the plan writes an index's column, alone or under the scan's alias, each comparison in
// parentheses. namesSourceDocument finds a scan of block_refs that tests the source document at
// all, in its Index Cond or its Filter: heldByABlock's probe. namesRepliesToBlock finds a scan
// that tests the block a reply names at all, and condKeysOwnID an Index Cond that keys a row's own
// id, as text or not.
var (
	condKeysToKind         = regexp.MustCompile(`\((?:[a-z_][a-z0-9_]*\.)?to_kind = `)
	condKeysToID           = regexp.MustCompile(`\((?:[a-z_][a-z0-9_]*\.)?to_id = `)
	condKeysSourceDocument = regexp.MustCompile(`\(\((?:[a-z_][a-z0-9_]*\.)?artifact_id\)::text = `)
	namesSourceDocument    = regexp.MustCompile(`\bartifact_id\b`)
	condKeysRepliesToBlock = regexp.MustCompile(`\((?:[a-z_][a-z0-9_]*\.)?replies_to_block = `)
	namesRepliesToBlock    = regexp.MustCompile(`\breplies_to_block\b`)
	condKeysOwnID          = regexp.MustCompile(`\(\(?(?:[a-z_][a-z0-9_]*\.)?id(?:\)::text)? = `)
)

// edgeKey is one row of an in statement, comparable: its edge kind, the node at its other end,
// the block of the node read that it is at (empty: the node itself), its created_at in
// microseconds and its source_seq (-1: null).
type edgeKey struct {
	kind, otherKind, otherID, atBlock string
	createdAt, sourceSeq              int64
}

func (key edgeKey) String() string {
	at := ""
	if key.atBlock != "" {
		at = " at " + key.atBlock
	}
	return fmt.Sprintf("%s from %s %s%s (%s)", key.kind, key.otherKind, key.otherID, at, time.UnixMicro(key.createdAt).UTC().Format(time.RFC3339Nano))
}

// edgeAt is the key of the edge kind from otherKind otherID, at block atBlock, of a fixture row
// written at `at`, which carries no source_seq.
func edgeAt(kind, otherKind, otherID, atBlock string, at time.Time) edgeKey {
	return edgeKey{kind: kind, otherKind: otherKind, otherID: otherID, atBlock: atBlock, createdAt: at.UnixMicro(), sourceSeq: -1}
}

func sortedEdgeKeys(keys []edgeKey) []edgeKey {
	sorted := slices.Clone(keys)
	slices.SortFunc(sorted, func(left, right edgeKey) int {
		return strings.Compare(left.String(), right.String())
	})
	return sorted
}

// edgesProbes is what the probe rule asks of how many of heldByABlock's probes of block_refs a
// plan ran.
type edgesProbes int

const (
	// probesAnyNumber: the rule holds of each probe that ran.
	probesAnyNumber edgesProbes = iota
	// probesAtLeastOne: the read's rows show a probe hid a mention, so one ran.
	probesAtLeastOne
	// probesNone: the read writes no probe, as the whole-document read does not.
	probesNone
)

// edgesRead is one in read of the test: the Query refs.Edges is given, the statement it sent and
// that statement's plan under each of edgesPlanSettings, and the answer it is held to: main's
// statement with the arguments main bound, which must list every key of listed and none of
// hidden, so a fixture main did not answer as it was written to decides nothing.
type edgesRead struct {
	name           string
	query          Query
	main           string
	mainArgs       []any
	listed, hidden []edgeKey
	probes         edgesProbes
	statement      recordedStatement
	plans          [len(edgesPlanSettings)]json.RawMessage
}

// edgesStatement is query's in statement: the statement readEdgeRows sends, the first refs.Edges
// sends.
func edgesStatement(t *testing.T, q Queryer, query Query) recordedStatement {
	t.Helper()
	recorder := &statementRecorder{Queryer: q}
	if _, err := readEdgeRows(context.Background(), recorder, model.Actor{}, query); err != nil {
		t.Fatalf("read the edge rows of %+v: %v", query, err)
	}
	if len(recorder.statements) != 1 {
		t.Fatalf("readEdgeRows sent %d statements for %+v; want its one", len(recorder.statements), query)
	}
	return recorder.statements[0]
}

// edgeStatementRows runs an in statement and answers its rows, in the order it gave them.
func edgeStatementRows(t *testing.T, q Queryer, statement recordedStatement) []edgeKey {
	t.Helper()
	rows, err := q.Query(context.Background(), statement.sql, statement.args...)
	if err != nil {
		t.Fatalf("run the in statement: %v\n%s", err, statement.sql)
	}
	defer rows.Close()
	var keys []edgeKey
	for rows.Next() {
		var row edgeRow
		if err := rows.Scan(&row.kind, &row.otherKind, &row.otherID, &row.createdAt, &row.sourceSeq, &row.atBlock); err != nil {
			t.Fatalf("scan a row of the in statement: %v", err)
		}
		key := edgeKey{kind: row.kind, otherKind: row.otherKind, otherID: row.otherID, createdAt: row.createdAt.UnixMicro(), sourceSeq: -1}
		if row.atBlock != nil {
			key.atBlock = *row.atBlock
		}
		if row.sourceSeq != nil {
			key.sourceSeq = *row.sourceSeq
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the rows of the in statement: %v", err)
	}
	return keys
}

// requireMainsRows holds read's statement to main's: main's rows list every key of read.listed
// and none of read.hidden, and the head's rows are main's, in order.
func requireMainsRows(t *testing.T, q Queryer, read edgesRead) {
	t.Helper()
	want := edgeStatementRows(t, q, recordedStatement{sql: read.main, args: read.mainArgs})
	answered := map[edgeKey]bool{}
	for _, key := range want {
		answered[key] = true
	}
	for _, key := range read.listed {
		if !answered[key] {
			t.Errorf("main's statement does not list %s, which the fixture wrote for it to list, so the fixture decides nothing of that row", key)
		}
	}
	for _, key := range read.hidden {
		if answered[key] {
			t.Errorf("main's statement lists %s, which the fixture wrote as a row no read lists", key)
		}
	}
	if got := edgeStatementRows(t, q, read.statement); !slices.Equal(got, want) {
		t.Errorf("the in statement answered %d rows that are not main's %d, in order: %s", len(got), len(want), edgeRowsDifference(got, want))
	}
}

// edgeRowsDifference names how got differs from want: the rows only one of them answered, or,
// when both answered the same rows, the first place their orders part.
func edgeRowsDifference(got, want []edgeKey) string {
	count := map[edgeKey]int{}
	for _, key := range got {
		count[key]++
	}
	for _, key := range want {
		count[key]--
	}
	var only []string
	for _, key := range sortedEdgeKeys(slices.Concat(got, want)) {
		switch n := count[key]; {
		case n > 0:
			only = append(only, fmt.Sprintf("\n\tonly the head's (%d): %s", n, key))
		case n < 0:
			only = append(only, fmt.Sprintf("\n\tonly main's (%d): %s", -n, key))
		}
		count[key] = 0
	}
	if len(only) > 0 {
		return strings.Join(only, "")
	}
	for index := range got {
		if got[index] != want[index] {
			return fmt.Sprintf("the same rows, whose orders part at row %d: the head's %s, main's %s", index, got[index], want[index])
		}
	}
	return "no difference"
}

// requireKeyedReads holds plan to c-graph-edges' plan rule, over every scan that ran: a scan the
// executor never ran read no row and is not judged. Every scan of a link table reads the table by
// target, with an Index Cond on to_id, its own or its bitmap's: a scan keyed by to_kind alone
// reads every link of that kind in the database. No read of a blocks table is a sequential scan
// or a scan with no Index Cond, the two forms a read of the whole table takes; a read of a
// source's block by its item and its block id is keyed. No read of a blocks table is an input of
// a hash join unless it is an index probe (isIndexProbe): a hash join reads each of its inputs
// whole, once. And every scan of comments or messages that tests replies_to_block, in a condition
// or through an index whose predicate holds it, reads by replies_to_block in its Index Cond (D11 of
// the plan of this change): the reply arms read the replies to the node's blocks through
// comments_replies_to_block and messages_replies_to_block.
func requireKeyedReads(t *testing.T, keys *explaintest.IndexKeys, plan json.RawMessage) {
	t.Helper()
	failed := false
	fail := func(format string, args ...any) {
		t.Helper()
		failed = true
		t.Errorf(format, args...)
	}
	nodes := explaintest.Nodes(t, plan)
	for _, node := range nodes {
		link, blocks := slices.Contains(edgesLinkTables, node.Relation), slices.Contains(edgesBlockTables, node.Relation)
		switch {
		case !link && !blocks, node.Type == "Bitmap Heap Scan":
			// Not a table of the rule, or a bitmap scan, which is judged by its bitmap below.
		case node.Loops == 0:
			// A scan the executor never ran.
		case node.Type != "Index Scan" && node.Type != "Index Only Scan":
			fail("%s (loops %g) reads %s whole, through no index", node, node.Loops, node.Relation)
		case link && !condKeysToID.MatchString(node.IndexCond):
			fail("%s (loops %g, rows %g) reads %s with Index Cond %q, not by to_id", node, node.Loops, node.Rows, node.Relation, node.IndexCond)
		case blocks && node.IndexCond == "":
			fail("%s (loops %g) reads %s whole, with no Index Cond", node, node.Loops, node.Relation)
		}
	}
	for _, relation := range slices.Concat(edgesLinkTables, edgesBlockTables) {
		link := slices.Contains(edgesLinkTables, relation)
		for _, scan := range explaintest.BitmapHeapScans(t, plan, relation) {
			if scan.Scan.Loops == 0 {
				continue
			}
			if len(scan.BitmapIndexScans) == 0 {
				fail("%s (loops %g) reads %s through no Bitmap Index Scan", scan.Scan, scan.Scan.Loops, relation)
			}
			for _, index := range scan.BitmapIndexScans {
				switch {
				case link && !condKeysToID.MatchString(index.IndexCond):
					fail("%s (loops %g) reads %s through %s with Index Cond %q, not by to_id", scan.Scan, scan.Scan.Loops, relation, index, index.IndexCond)
				case !link && index.IndexCond == "":
					fail("%s (loops %g) reads %s whole, through %s with no Index Cond", scan.Scan, scan.Scan.Loops, relation, index)
				}
			}
		}
	}

	var tree edgesPlanTree
	if err := json.Unmarshal(plan, &tree); err != nil {
		t.Fatalf("decode the plan: %v", err)
	}
	for _, input := range hashJoinInputs(tree, nil) {
		if slices.Contains(edgesBlockTables, input.Relation) && input.Loops > 0 && !isIndexProbe(t, keys, input) {
			fail("%s (loops %g, Index Cond %q) is an input of a hash join, which reads %s whole: it is no probe of its index's first key column", input, input.Loops, input.IndexCond, input.Relation)
		}
	}

	repliesKeyed := func(scan string, loops, rows float64, indexes []string, cond, filter, recheck string) {
		t.Helper()
		tests := namesRepliesToBlock.MatchString(cond) || namesRepliesToBlock.MatchString(filter) || namesRepliesToBlock.MatchString(recheck) ||
			slices.ContainsFunc(indexes, func(index string) bool { return slices.Contains(edgesReplyIndexes, index) })
		switch {
		case !tests, condKeysRepliesToBlock.MatchString(cond):
		case condKeysOwnID.MatchString(cond):
			// The one exemption: a scan whose Index Cond keys the row's own id reads that one
			// row each time it runs, and tests replies_to_block on that row alone. That is the
			// exists foldsAnswers writes into a comment's read, which asks whether one answering
			// comment replies to the comment or to one of its blocks; it reads no reply arm, so
			// it has no replies_to_block to key.
		default:
			fail("%s (loops %g, rows %g) tests replies_to_block with Index Cond %q, Filter %q and Recheck Cond %q: it does not read by replies_to_block", scan, loops, rows, cond, filter, recheck)
		}
	}
	for _, node := range nodes {
		if (node.Relation != "comments" && node.Relation != "messages") || node.Type == "Bitmap Heap Scan" || node.Loops == 0 {
			continue
		}
		repliesKeyed(node.String(), node.Loops, node.Rows, []string{node.Index}, node.IndexCond, node.Filter, node.RecheckCond)
	}
	for _, relation := range []string{"comments", "messages"} {
		for _, scan := range explaintest.BitmapHeapScans(t, plan, relation) {
			if scan.Scan.Loops == 0 {
				continue
			}
			indexes, conds := make([]string, 0, len(scan.BitmapIndexScans)), make([]string, 0, len(scan.BitmapIndexScans))
			for _, index := range scan.BitmapIndexScans {
				indexes, conds = append(indexes, index.Index), append(conds, index.IndexCond)
			}
			repliesKeyed(scan.Scan.String(), scan.Scan.Loops, scan.Scan.Rows, indexes, strings.Join(conds, " AND "), scan.Scan.Filter, scan.Scan.RecheckCond)
		}
	}
	if failed {
		t.Logf("the plan's scans:%s", planScans(t, plan))
	}
}

// edgesPlanTree is one node of a plan's own JSON with the nodes under it. The plan case walks it
// for the inputs of each hash join, since explaintest answers a plan's nodes in a flat list and
// exports no node's parent.
type edgesPlanTree struct {
	Type               string          `json:"Node Type"`
	Relation           string          `json:"Relation Name"`
	Index              string          `json:"Index Name"`
	IndexCond          string          `json:"Index Cond"`
	ParentRelationship string          `json:"Parent Relationship"`
	Loops              float64         `json:"Actual Loops"`
	Plans              []edgesPlanTree `json:"Plans"`
}

func (n edgesPlanTree) String() string {
	return explaintest.Node{Type: n.Type, Relation: n.Relation, Index: n.Index}.String()
}

// hashJoinInputs appends to inputs every scan whose rows a hash join at or under node reads: on
// each side of the join (the inner side under its Hash), the scans reached through the nodes that
// pass rows on, as a Hash, a Sort, an Append or an Aggregate does (joinInputScans).
func hashJoinInputs(node edgesPlanTree, inputs []edgesPlanTree) []edgesPlanTree {
	if node.Type == "Hash Join" {
		for _, side := range node.Plans {
			inputs = joinInputScans(side, inputs)
		}
	}
	for _, child := range node.Plans {
		inputs = hashJoinInputs(child, inputs)
	}
	return inputs
}

// joinInputScans appends to scans the scans whose rows node passes on to the join above it. The
// walk stops at a scan, and at another join, whose inputs are that join's own; it leaves out a
// subplan and an initplan, which a node runs for a condition rather than for rows.
func joinInputScans(node edgesPlanTree, scans []edgesPlanTree) []edgesPlanTree {
	switch {
	case node.ParentRelationship == "SubPlan", node.ParentRelationship == "InitPlan":
		return scans
	case node.Relation != "":
		return append(scans, node)
	case node.Type == "Nested Loop", node.Type == "Hash Join", node.Type == "Merge Join":
		return scans
	}
	for _, child := range node.Plans {
		scans = joinInputScans(child, scans)
	}
	return scans
}

// isIndexProbe reports whether scan reads its relation through an index whose first key column
// its Index Cond names (explaintest.CondNamesFirstKey): its own Index Cond, or for a Bitmap Heap
// Scan the Index Cond of every Bitmap Index Scan of its bitmap. A scan with no Index Cond, or with
// one that names no column its index leads with, reads the whole of the index.
func isIndexProbe(t *testing.T, keys *explaintest.IndexKeys, scan edgesPlanTree) bool {
	t.Helper()
	probes := func(index edgesPlanTree) bool {
		return index.IndexCond != "" && explaintest.CondNamesFirstKey(index.IndexCond, keys.FirstKey(t, index.Index))
	}
	switch scan.Type {
	case "Index Scan", "Index Only Scan":
		return probes(scan)
	case "Bitmap Heap Scan":
		indexes := bitmapIndexScans(scan, nil)
		return len(indexes) > 0 && !slices.ContainsFunc(indexes, func(index edgesPlanTree) bool { return !probes(index) })
	}
	return false
}

// bitmapIndexScans appends to indexes the Bitmap Index Scans that the bitmap of node, a Bitmap Heap
// Scan, is built from: its child of parent relationship Outer, and through a BitmapOr or a
// BitmapAnd their children of parent relationship Member, as explaintest.BitmapHeapScans reads
// them.
func bitmapIndexScans(node edgesPlanTree, indexes []edgesPlanTree) []edgesPlanTree {
	for _, child := range node.Plans {
		switch {
		case child.ParentRelationship != "Outer" && child.ParentRelationship != "Member":
		case child.Type == "Bitmap Index Scan":
			indexes = append(indexes, child)
		default:
			indexes = bitmapIndexScans(child, indexes)
		}
	}
	return indexes
}

// requireCountedReads holds plan to the count case (D11 of the plan of this change): the read reads
// no block link but those that end at the node read or at one of its current blocks. For each link
// table, the scans of it that ran and whose Index Cond does not name its source column
// (edgesSourceColumns) read at most counts[table] rows in all: the rows of that table the fixture
// holds that end at those targets. A scan whose Index Cond names the source column is
// heldByABlock's probe of one item's links to one target: it reads at most one row a loop and
// removes no row by a filter. A Bitmap Heap Scan's Index Cond is that of the Bitmap Index Scans of
// its bitmap, and its rows are its own. Actual Rows is an average for each loop, rounded to two
// decimals, and Rows Removed by Filter one rounded to a whole number, so each bound allows 0.005
// or 0.5 times the loops beyond it: the precision of the plan's own numbers, the one margin the
// Verification document's "Running the checks" allows.
func requireCountedReads(t *testing.T, plan json.RawMessage, counts map[string]int) {
	t.Helper()
	failed := false
	fail := func(format string, args ...any) {
		t.Helper()
		failed = true
		t.Errorf(format, args...)
	}
	nodes := explaintest.Nodes(t, plan)
	for _, table := range edgesLinkTables {
		source := edgesSourceColumns[table]
		var rows, loops float64
		var read strings.Builder
		judge := func(scan string, scanLoops, scanRows float64, cond string, removed float64) {
			t.Helper()
			switch {
			case scanLoops == 0:
				// A scan the executor never ran.
			case source != nil && source.MatchString(cond):
				if scanRows > 1.005 {
					fail("%s (loops %g, Index Cond %q) is heldByABlock's probe of %s and read %g rows a loop; want at most one", scan, scanLoops, cond, table, scanRows)
				}
				if removed*scanLoops > 0.5*scanLoops {
					fail("%s (loops %g, Index Cond %q) is heldByABlock's probe of %s and removed %g rows a loop by its filter; want none", scan, scanLoops, cond, table, removed)
				}
			default:
				rows += scanRows * scanLoops
				loops += scanLoops
				fmt.Fprintf(&read, "\n\t%s: loops %g, rows %g, Index Cond %q", scan, scanLoops, scanRows, cond)
			}
		}
		for _, node := range nodes {
			if node.Relation == table && node.Type != "Bitmap Heap Scan" {
				judge(node.String(), node.Loops, node.Rows, node.IndexCond, node.RowsRemovedByFilter)
			}
		}
		for _, scan := range explaintest.BitmapHeapScans(t, plan, table) {
			conds := make([]string, 0, len(scan.BitmapIndexScans))
			for _, index := range scan.BitmapIndexScans {
				conds = append(conds, index.IndexCond)
			}
			judge(scan.Scan.String(), scan.Scan.Loops, scan.Scan.Rows, strings.Join(conds, " AND "), scan.Scan.RowsRemovedByFilter)
		}
		if rows > float64(counts[table])+0.005*loops {
			fail("the plan read %g rows of %s beyond heldByABlock's probes, in %g loops; the fixture holds %d rows of %s that end at the node read or at one of its current blocks, and the read reads no other:%s",
				rows, table, loops, counts[table], table, read.String())
		}
	}
	if failed {
		t.Logf("the plan's scans:%s", planScans(t, plan))
	}
}

// edgesTargetCounts is, for each link table, how many of its rows the fixture holds that end at
// the node query reads or at one of its current blocks, by one count(*) of each table: the rows
// the in read must read of it. A document's current blocks are its document_blocks rows; an
// item's are its blocks that hold a position. Each count must be above 0, since the fixture
// writes a link of each table into each read node's blocks, and a count of 0 would judge nothing.
func edgesTargetCounts(t *testing.T, q Queryer, query Query) map[string]int {
	t.Helper()
	blockKind, blocks := "block", `select d.node_id from document_blocks d join artifacts a on a.id = d.artifact_id where a.ref_key = $4`
	if query.Kind != "artifact" {
		item := ItemKind(query.Kind)
		tables, err := item.tables()
		if err != nil {
			t.Fatalf("the count case reads no blocks of kind %q: %v", query.Kind, err)
		}
		blockKind = item.BlockKind()
		blocks = `select b.node_id from ` + tables.blocks + ` b where b.` + tables.item + ` = $4::uuid and b.position is not null`
	}
	counts := make(map[string]int, len(edgesLinkTables))
	for _, table := range edgesLinkTables {
		var count int
		if err := q.QueryRow(context.Background(), `select count(*) from `+table+`
			where (to_kind = $1 and to_id = $2) or (to_kind = $3 and to_id in (`+blocks+`))`,
			query.Kind, query.ID, blockKind, query.ID).Scan(&count); err != nil {
			t.Fatalf("count the rows of %s that end at %s %s or at one of its current blocks: %v", table, query.Kind, query.ID, err)
		}
		if count == 0 {
			t.Errorf("the fixture holds no row of %s that ends at %s %s or at one of its current blocks, so the count case judges nothing of %s", table, query.Kind, query.ID, table)
		}
		counts[table] = count
	}
	return counts
}

// graphViewBlockArms is every arm of graph_edges that can end at a block, as the view's own text
// declares it (edgesBlockArm.String), sorted: each arm parseGraphViewArms reads whose to_kind is a
// column or one of blockNodeKinds, with its FROM clause. parseGraphViewArms splits the view's text
// at each top-level UNION ALL; the same split gives each arm's text, whose FROM clause runs from
// its FROM line to its end.
func graphViewBlockArms(t *testing.T, q Queryer) []string {
	t.Helper()
	var definition string
	if err := q.QueryRow(context.Background(), `select pg_get_viewdef('graph_edges'::regclass, true)`).Scan(&definition); err != nil {
		t.Fatalf("read the graph_edges definition: %v", err)
	}
	arms := parseGraphViewArms(t, definition)
	texts := strings.Split(strings.TrimSuffix(strings.TrimRight(definition, "\n"), ";"), "\nUNION ALL\n")
	if len(texts) != len(arms) {
		t.Fatalf("graph_edges splits into %d arms' texts and %d arms", len(texts), len(arms))
	}
	var blockKinds []string
	for _, match := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(blockNodeKinds, -1) {
		blockKinds = append(blockKinds, match[1])
	}
	if len(blockKinds) == 0 {
		t.Fatalf("blockNodeKinds names no kind: %s", blockNodeKinds)
	}
	var found []string
	for index, arm := range arms {
		if arm.toKind != "" && !slices.Contains(blockKinds, arm.toKind) {
			continue
		}
		_, from, ok := strings.Cut(texts[index], "\n   FROM ")
		if !ok {
			t.Fatalf("arm %d of graph_edges has no FROM line:\n%s", index+1, texts[index])
		}
		found = append(found, edgesBlockArm{arm: arm, from: "FROM " + strings.Join(strings.Fields(from), " ")}.String())
	}
	slices.Sort(found)
	return found
}

// requireProbeRule holds plan to c-graph-edges' probe rule: every scan of block_refs that ran and
// tests the source document in its Index Cond, its Recheck Cond or its Filter (heldByABlock's
// probe) reads block_refs through an Index Cond that holds to_kind, to_id and
// (artifact_id)::text, a Bitmap Heap Scan through a Bitmap Index Scan of its bitmap that carries
// that Index Cond, and removes no row by a filter; no scan of block_refs that ran is a sequential
// scan; and as many probes ran as want says. Rows Removed by Filter is an average for each loop,
// rounded to a whole number, so no row removed is at most 0.5 times the loops in all, the one
// margin the Verification document's "Running the checks" allows.
func requireProbeRule(t *testing.T, plan json.RawMessage, want edgesProbes) {
	t.Helper()
	failed := false
	ran := 0
	keysProbe := func(cond string) bool {
		return condKeysToKind.MatchString(cond) && condKeysToID.MatchString(cond) && condKeysSourceDocument.MatchString(cond)
	}
	judge := func(scan string, loops float64, conds []string, filter, recheck string, removed float64) {
		t.Helper()
		if !slices.ContainsFunc(append([]string{filter, recheck}, conds...), namesSourceDocument.MatchString) {
			return
		}
		ran++
		if !slices.ContainsFunc(conds, keysProbe) {
			failed = true
			t.Errorf("heldByABlock's probe %s (loops %g) reads block_refs with Index Cond %q and Filter %q: no Index Cond of it holds to_kind, to_id and (artifact_id)::text", scan, loops, conds, filter)
		}
		if removed*loops > 0.5*loops {
			failed = true
			t.Errorf("heldByABlock's probe %s removed %g rows by its filter in each of %g loops, %g in all; want none", scan, removed, loops, removed*loops)
		}
	}
	for _, node := range explaintest.Nodes(t, plan) {
		if node.Relation != "block_refs" || node.Loops == 0 {
			continue
		}
		switch node.Type {
		case "Seq Scan":
			failed = true
			t.Errorf("%s (loops %g) reads block_refs whole", node, node.Loops)
		case "Bitmap Heap Scan":
			// Judged with its bitmap below.
		default:
			judge(node.String(), node.Loops, []string{node.IndexCond}, node.Filter, node.RecheckCond, node.RowsRemovedByFilter)
		}
	}
	for _, scan := range explaintest.BitmapHeapScans(t, plan, "block_refs") {
		if scan.Scan.Loops == 0 {
			continue
		}
		conds := make([]string, 0, len(scan.BitmapIndexScans))
		for _, index := range scan.BitmapIndexScans {
			conds = append(conds, index.IndexCond)
		}
		judge(scan.Scan.String(), scan.Scan.Loops, conds, scan.Scan.Filter, scan.Scan.RecheckCond, scan.Scan.RowsRemovedByFilter)
	}
	switch {
	case want == probesAtLeastOne && ran == 0:
		failed = true
		t.Errorf("no probe of block_refs by the source document ran, though the read's rows hide a document's mention behind its block")
	case want == probesNone && ran > 0:
		failed = true
		t.Errorf("%d scans of block_refs test the source document, in a read that writes no probe", ran)
	}
	if failed {
		t.Logf("the plan's scans:%s", planScans(t, plan))
	}
}

// planScans is every node of plan that reads a relation or an index, one to a line, as a failure
// prints the plan it judged.
func planScans(t *testing.T, plan json.RawMessage) string {
	t.Helper()
	var lines strings.Builder
	for _, node := range explaintest.Nodes(t, plan) {
		if node.Relation == "" && node.Index == "" {
			continue
		}
		fmt.Fprintf(&lines, "\n\t%s: loops %g, rows %g", node, node.Loops, node.Rows)
		if node.Subplan != "" {
			fmt.Fprintf(&lines, ", in %s", node.Subplan)
		}
		if node.IndexCond != "" {
			fmt.Fprintf(&lines, ", Index Cond %s", node.IndexCond)
		}
		if node.Filter != "" {
			fmt.Fprintf(&lines, ", Filter %s, removed %g", node.Filter, node.RowsRemovedByFilter)
		}
	}
	return lines.String()
}

// requireBlockRefsTargetIndex holds block_refs' indexes to the index case: block_refs_to_source is
// the B-tree on (to_kind, to_id, ((artifact_id)::text)), and no other index of block_refs starts
// with to_kind, so block_refs keeps one index of targets.
func requireBlockRefsTargetIndex(t *testing.T, q Queryer) {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select indexname, indexdef from pg_indexes
		where schemaname = current_schema() and tablename = 'block_refs' order by indexname`)
	if err != nil {
		t.Fatalf("read block_refs' indexes: %v", err)
	}
	definitions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var index [2]string
		return index, row.Scan(&index[0], &index[1])
	})
	if err != nil {
		t.Fatalf("read block_refs' indexes: %v", err)
	}
	found := false
	for _, index := range definitions {
		name, definition := index[0], index[1]
		if name == "block_refs_to_source" {
			found = true
			if !strings.HasSuffix(definition, ".block_refs USING btree (to_kind, to_id, ((artifact_id)::text))") {
				t.Errorf("block_refs_to_source is %q; want the B-tree on (to_kind, to_id, ((artifact_id)::text))", definition)
			}
			continue
		}
		if _, keys, ok := strings.Cut(definition, " USING "); ok {
			if _, columns, ok := strings.Cut(keys, " ("); ok && strings.HasPrefix(columns, "to_kind") {
				t.Errorf("%s is an index of block_refs that starts with to_kind beside block_refs_to_source: %s", name, definition)
			}
		}
	}
	if !found {
		t.Errorf("block_refs has no index block_refs_to_source; its indexes are %v", definitions)
	}
}

// blockLinkCounts is, for each table that stores a link that can end at a block (each link table,
// and comments and messages, whose replies_to_block names a block), how many links of it the
// fixture holds into the blocks of subject, current or not, and how many into other blocks of the
// same kind.
func blockLinkCounts(t *testing.T, q Queryer, subject edgesSubject) map[string][2]int {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select link, count(*) filter (where to_id = any($2)), count(*) filter (where not to_id = any($2))
		from (
			select 'refs', to_id from refs where to_kind = $1
			union all select 'block_refs', to_id from block_refs where to_kind = $1
			union all select 'ask_block_refs', to_id from ask_block_refs where to_kind = $1
			union all select 'comment_block_refs', to_id from comment_block_refs where to_kind = $1
			union all select 'message_block_refs', to_id from message_block_refs where to_kind = $1
			union all select 'comments', replies_to_block from comments
			where replies_to_block is not null
			and $1 = case when replies_to_block like 'ask/%' then 'ask_block' else 'comment_block' end
			union all select 'messages', replies_to_block from messages where replies_to_block is not null and $1 = 'message_block'
		) links (link, to_id)
		group by link`, subject.blockKind, []string{subject.blocks[0], subject.blocks[1], subject.gone})
	if err != nil {
		t.Fatalf("count the links into blocks of kind %s: %v", subject.blockKind, err)
	}
	defer rows.Close()
	counts := map[string][2]int{}
	for rows.Next() {
		var link string
		var count [2]int
		if err := rows.Scan(&link, &count[0], &count[1]); err != nil {
			t.Fatalf("scan the links into blocks of kind %s: %v", subject.blockKind, err)
		}
		counts[link] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count the links into blocks of kind %s: %v", subject.blockKind, err)
	}
	return counts
}

// edgesFixture is the data the test reads: the outside graph, the four nodes of the five reads
// with the links written around each, the issue of the probe case and the nodes of the kinds
// case, committed and analyzed, with each read the test makes of them.
type edgesFixture struct {
	database *store.Store
	subjects []edgesSubject
	reads    []edgesRead
	probe    edgesRead
	kinds    []edgesRead
}

// edgesFixtureActor is the actor every row of the fixture names.
const edgesFixtureActor = `{"kind":"user","id":"alice"}`

// edgesFixtureEpoch is the time of the fixture's first row; each later row is a millisecond
// after the one before (edgesFixtureWriter.at).
var edgesFixtureEpoch = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// edgesFixtureTables is every table the fixture copies rows into, with the columns its rows
// carry, in the order the copies run: each after the tables its foreign keys name, graph_nodes
// before block_refs, whose node_kind and node_address name a node of it.
var edgesFixtureTables = []copyTable{
	{name: "projects", columns: []string{"key", "name", "default_assignee", "created_at"}},
	{name: "architecture_sources", columns: []string{"id", "project_key", "repo", "branch", "installation_id", "created_by", "created_at"}},
	{name: "architecture_snapshots", columns: []string{"id", "project_key", "commit", "files", "imported_at"}},
	{name: "components", columns: []string{"project_key", "id", "title", "prose", "parent", "snapshot_id", "source_id"}},
	{name: "component_depends", columns: []string{"project_key", "from_id", "to_id"}},
	{name: "issues", columns: []string{"key", "project_key", "number", "title", "created_by", "rank", "created_at", "updated_at"}},
	{name: "issue_links", columns: []string{"issue_key", "kind", "target_key", "created_at"}},
	{name: "issue_components", columns: []string{"issue_key", "mode", "created_at", "updated_at"}},
	{name: "issue_component_members", columns: []string{"issue_key", "project_key", "component_id"}},
	{name: "artifacts", columns: []string{"id", "issue_key", "project_key", "slug", "name", "kind", "ref_key", "created_by", "created_at", "node_kind", "node_address"}},
	{name: "issue_trees", columns: []string{"issue_key", "artifact_id"}},
	{name: "document_rooms", columns: []string{"artifact_id", "room"}},
	{name: "issue_room_blocks", columns: []string{"tree_artifact_id", "block_id", "document_id"}},
	{name: "asks", columns: []string{"id", "issue_key", "artifact_id", "anchor", "author", "question", "created_at"}},
	{name: "ask_followers", columns: []string{"ask_id", "session_id", "since"}},
	{name: "comments", columns: []string{"id", "issue_key", "artifact_id", "anchor", "author", "body", "reply_to", "answers", "ask_id", "replies_to_block", "created_at"}},
	{name: "messages", columns: []string{"id", "issue_key", "author", "body", "in_reply_to", "replies_to_block", "created_at"}},
	{name: "document_blocks", columns: []string{"artifact_id", "block_id", "node_id", "type", "text_sha256", "excerpt", "changed_version", "changed_at", "created_at"}},
	{name: "ask_blocks", columns: []string{"ask_id", "block_id", "node_id", "position", "type", "text_sha256", "excerpt", "made_at", "changed_revision", "changed_at", "removed_at", "created_at"}},
	{name: "comment_blocks", columns: []string{"comment_id", "block_id", "node_id", "position", "type", "text_sha256", "excerpt", "made_at", "changed_revision", "changed_at", "removed_at", "created_at"}},
	{name: "message_blocks", columns: []string{"message_id", "block_id", "node_id", "position", "type", "text_sha256", "excerpt", "made_at", "changed_at", "removed_at", "created_at"}},
	{name: "refs", columns: []string{"from_kind", "from_id", "to_kind", "to_id", "kind", "created_at"}},
	{name: "graph_nodes", columns: []string{"kind", "id", "supertag", "visibility", "state", "made_at", "made_by", "source_event"}},
	{name: "block_refs", columns: []string{"artifact_id", "block_id", "from_node", "to_kind", "to_id", "kind", "to_types", "created_version", "created_at", "node_kind", "node_address"}},
	{name: "graph_tuples", columns: []string{"from_kind", "from_id", "kind", "to_kind", "to_id", "at", "who", "source_event", "legacy_source_seq", "asserted"}},
	{name: "ask_block_refs", columns: []string{"ask_id", "block_id", "from_node", "to_kind", "to_id", "kind", "created_at"}},
	{name: "comment_block_refs", columns: []string{"comment_id", "block_id", "from_node", "to_kind", "to_id", "kind", "created_at"}},
	{name: "message_block_refs", columns: []string{"message_id", "block_id", "from_node", "to_kind", "to_id", "kind", "created_at"}},
}

// newEdgesFixture writes the fixture in bulk (loadFixture), a COPY for each table.
func newEdgesFixture(t *testing.T) edgesFixture {
	t.Helper()
	w := &edgesFixtureWriter{rows: map[string][][]any{}, nodes: map[[2]string]bool{}}
	w.add("projects", "CORE", "CORE project", "alice", w.at())
	w.add("projects", "OUT", "OUT project", "alice", w.at())
	// CORE-1 holds the nodes of the five reads, CORE-2 the items that cite them, CORE-3 is the
	// issue of the probe case, CORE-4 the issue of the kinds case, and CORE-5 holds its items.
	for number := 1; number <= 5; number++ {
		w.issue("CORE-"+strconv.Itoa(number), "CORE", number)
	}
	w.outsideGraph()
	document, whole := w.subjectDocument()
	message, comment, ask := w.subjectItem(ItemMessage), w.subjectItem(ItemComment), w.subjectItem(ItemAsk)
	probe := w.probeCase()
	kinds := w.kindsMembers(t)

	database := storetest.Open(t)
	loadFixture(t, database.Pool, edgesFixtureTables, w.rows)

	partOf, mentions := []string{"part_of"}, []string{"mentions"}
	reads := []edgesRead{
		document.read("document", Query{Direction: "in", Kind: "artifact", ID: document.node.ID, ExcludeKinds: partOf},
			mainEdgesInDocument, "artifact", document.node.ID, partOf),
		whole.read("whole document", Query{Direction: "in", Kind: "artifact", ID: document.node.ID, Kinds: mentions, WholeDocuments: true},
			mainEdgesInWholeDocument, "artifact", document.node.ID, mentions),
		message.read("message", Query{Direction: "in", Kind: "message", ID: message.node.ID, ExcludeKinds: partOf},
			mainEdgesInMessage, "message", message.node.ID, message.node.ID, partOf),
		comment.read("comment", Query{Direction: "in", Kind: "comment", ID: comment.node.ID, ExcludeKinds: partOf},
			mainEdgesInComment, "comment", comment.node.ID, comment.node.ID, partOf),
		ask.read("ask", Query{Direction: "in", Kind: "ask", ID: ask.node.ID},
			mainEdgesInAsk, "ask", ask.node.ID, ask.node.ID),
		ask.read("ask panel", Query{Direction: "in", Kind: "ask", ID: ask.node.ID, ExcludeKinds: AskBacklinkExclusions},
			mainEdgesInAskPanel, "ask", ask.node.ID, ask.node.ID, AskBacklinkExclusions),
	}
	return edgesFixture{
		database: database, subjects: []edgesSubject{document, message, comment, ask},
		reads: reads, probe: probe, kinds: kinds,
	}
}

// edgesFixtureWriter gathers the fixture's rows, table by table, for one COPY of each. Every row
// takes a time of its own (at), so every read's rows are ordered by created_at alone and an order
// a statement got wrong shows, and every uuid and slug is its own too. nodes is every node of the
// store it has written, each once.
type edgesFixtureWriter struct {
	rows  map[string][][]any
	nodes map[[2]string]bool
	ticks int
	ids   int
}

func (w *edgesFixtureWriter) add(table string, values ...any) {
	w.rows[table] = append(w.rows[table], values)
}

func (w *edgesFixtureWriter) at() time.Time {
	w.ticks++
	return edgesFixtureEpoch.Add(time.Duration(w.ticks) * time.Millisecond)
}

func (w *edgesFixtureWriter) uuid() string {
	w.ids++
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", w.ids)
}

func (w *edgesFixtureWriter) slug(prefix string) string {
	w.ids++
	return prefix + "-" + strconv.Itoa(w.ids)
}

func (w *edgesFixtureWriter) issue(key, project string, number int) {
	at := w.at()
	w.add("issues", key, project, number, key, edgesFixtureActor, fmt.Sprintf("U%07d", number), at, at)
}

// edgesDocument is a document of the fixture: its uuid and its ref_key, which the copy writes as
// the artifacts_ref_key trigger does, from its issue, else its project, and its slug.
type edgesDocument struct{ id, refKey string }

func (w *edgesFixtureWriter) document(issue *string, project, slug string) edgesDocument {
	owner := project
	if issue != nil {
		owner = *issue
	}
	document := edgesDocument{id: w.uuid(), refKey: owner + "/" + slug}
	w.add("artifacts", document.id, issue, project, slug, slug, "doc", document.refKey, edgesFixtureActor, w.at(), nil, nil)
	return document
}

// markedDocument writes a document whose row names its node, as this build writes one, and the
// node: the store answers the document's mentions from the mentions tuples out of that node
// while the refs row of each stands (store.GraphEdgesArtifactRefs), and graph_edges' arm over
// refs leaves its rows out (store.ReferenceNotStated).
func (w *edgesFixtureWriter) markedDocument(issue *string, project, slug string) edgesDocument {
	owner := project
	if issue != nil {
		owner = *issue
	}
	document := edgesDocument{id: w.uuid(), refKey: owner + "/" + slug}
	w.add("artifacts", document.id, issue, project, slug, slug, "doc", document.refKey, edgesFixtureActor, w.at(),
		graphstore.KindArtifact, document.refKey)
	w.storeNode(graphstore.KindArtifact, document.refKey, graphstore.SupertagDocument, graphstore.StateLive)
	return document
}

// roomOf writes the issue room of issue key and puts document in it, as a conversion records one:
// the room's tree artifact, its issue_trees row, the document's document_rooms row, and the
// issue_room_blocks row of the document's block blockID. It writes no block under the tree, so
// the room adds no edge any read lists; it gives the tables a read of a room reads their size.
func (w *edgesFixtureWriter) roomOf(key, project string, document edgesDocument, blockID string) {
	tree := w.uuid()
	w.add("artifacts", tree, key, project, "tree", "tree", "tree", key+"/tree", edgesFixtureActor, w.at(), nil, nil)
	w.add("issue_trees", key, tree)
	w.add("document_rooms", document.id, tree)
	w.add("issue_room_blocks", tree, blockID, document.id)
}

// documentBlock writes a block of document that its current version holds, and answers its node
// id and the time of its part_of edge.
func (w *edgesFixtureWriter) documentBlock(document edgesDocument, blockID, blockType string) (string, time.Time) {
	node, at := document.refKey+"#"+blockID, w.at()
	w.add("document_blocks", document.id, blockID, node, blockType, "sha", "excerpt", 1, at, at)
	return node, at
}

// edgesItem is what the fixture writes of an ask, a comment or a message beside its id and its
// time: its owner, an issue or (an ask's, a comment's) a document; the document it is anchored
// to; and, for a reply, what it answers.
type edgesItem struct {
	issue, artifact, anchor                          *string
	replyTo, answers, ask, inReplyTo, repliesToBlock *string
}

func (w *edgesFixtureWriter) item(kind ItemKind, item edgesItem) (string, time.Time) {
	id, at := w.uuid(), w.at()
	switch kind {
	case ItemAsk:
		w.add("asks", id, item.issue, item.artifact, item.anchor, edgesFixtureActor, "Which?", at)
	case ItemComment:
		w.add("comments", id, item.issue, item.artifact, item.anchor, edgesFixtureActor, "A comment.", item.replyTo, item.answers, item.ask, item.repliesToBlock, at)
	case ItemMessage:
		w.add("messages", id, item.issue, edgesFixtureActor, "A message.", item.inReplyTo, item.repliesToBlock, at)
	}
	return id, at
}

// itemBlock writes a block of an item, current at position, or removed by a later revision when
// position is nil, and answers its node id and the time of its part_of edge.
func (w *edgesFixtureWriter) itemBlock(kind ItemKind, itemID, blockID string, position *int) (string, time.Time) {
	node, at := string(kind)+"/"+itemID+"#"+blockID, w.at()
	var removedAt any
	if position == nil {
		removedAt = at
	}
	tables, _ := kind.tables()
	if kind == ItemMessage {
		w.add(tables.blocks, itemID, blockID, node, position, "paragraph", "sha", "excerpt", at, at, removedAt, at)
	} else {
		w.add(tables.blocks, itemID, blockID, node, position, "paragraph", "sha", "excerpt", at, 1, at, removedAt, at)
	}
	return node, at
}

// ref writes a mention in the text of the item fromKind fromID, and answers its time.
func (w *edgesFixtureWriter) ref(fromKind, fromID string, to Target) time.Time {
	at := w.at()
	w.add("refs", fromKind, fromID, to.Kind, to.ID, "mentions", at)
	return at
}

// blockRef writes a link of kind from block blockID of document, a typed pointer when toTypes
// names the types its attribute allows, in a row that names no node, which graph_edges answers
// from its arm over block_refs, and answers its source node id and its time.
func (w *edgesFixtureWriter) blockRef(document edgesDocument, blockID, kind string, to Target, toTypes *string) (string, time.Time) {
	node, at := document.refKey+"#"+blockID, w.at()
	w.add("block_refs", document.id, blockID, node, to.Kind, to.ID, kind, toTypes, 1, at, nil, nil)
	return node, at
}

// storeNode writes the store's node of kind and id, in state, once. It records no event, as a
// write by no event does (graphstore.NoEvent).
func (w *edgesFixtureWriter) storeNode(kind, id, supertag, state string) {
	if w.nodes[[2]string{kind, id}] {
		return
	}
	w.nodes[[2]string{kind, id}] = true
	w.add("graph_nodes", kind, id, supertag, graphstore.VisibilityProject, state, w.at(), edgesFixtureActor, int(graphstore.NoEvent))
}

// storeTuple writes a tuple of the store, which asserts, or retracts where asserted is false, and
// records no event.
func (w *edgesFixtureWriter) storeTuple(fromKind, fromID, kind string, to Target, at time.Time, asserted bool) {
	w.add("graph_tuples", fromKind, fromID, kind, to.Kind, to.ID, at, edgesFixtureActor, int(graphstore.NoEvent), nil, strconv.FormatBool(asserted))
}

// tupleLink writes a mention from block blockID of document as this build's block index writes
// one, which graph_edges answers from its tuple arm (migration 20261121034242): the block's node,
// in state; the block_refs row of the link, naming that node; and the link's tuple, followed by a
// retraction of it where retracted. It answers the block's node id and the time of the tuple the
// view answers the edge at.
func (w *edgesFixtureWriter) tupleLink(document edgesDocument, blockID string, to Target, state string, retracted bool) (string, time.Time) {
	node := document.refKey + "#" + blockID
	w.storeNode(BlockNodeKind, node, BlockSupertag, state)
	at := w.at()
	w.add("block_refs", document.id, blockID, node, to.Kind, to.ID, "mentions", nil, 1, at, BlockNodeKind, node)
	w.storeTuple(BlockNodeKind, node, "mentions", to, at, true)
	if retracted {
		w.storeTuple(BlockNodeKind, node, "mentions", to, w.at(), false)
	}
	return node, at
}

// shapedTie writes an answered ask's shaped tie to block to, as the store states it (migration
// 20261116094101): the ask's node, its tuple, and the block_refs row of the ask's block blockID of
// document that names the ask's node. It answers the ask's id and the tie's time.
func (w *edgesFixtureWriter) shapedTie(document edgesDocument, blockID string, to Target) (string, time.Time) {
	ask := w.uuid()
	w.storeNode(graphstore.KindAsk, ask, graphstore.KindAsk, graphstore.StateLive)
	at := w.at()
	w.add("block_refs", document.id, blockID, document.refKey+"#"+blockID, to.Kind, to.ID, TupleShaped, nil, 1, at, graphstore.KindAsk, ask)
	w.storeTuple(graphstore.KindAsk, ask, TupleShaped, to, at, true)
	return ask, at
}

// itemBlockRef writes a mention from block blockID of an item, and answers its source node id
// and its time.
func (w *edgesFixtureWriter) itemBlockRef(kind ItemKind, itemID, blockID string, to Target) (string, time.Time) {
	tables, _ := kind.tables()
	node, at := string(kind)+"/"+itemID+"#"+blockID, w.at()
	w.add(tables.refs, itemID, blockID, node, to.Kind, to.ID, "mentions", at)
	return node, at
}

// outsideGraph writes edgesOutsideNodes issues of OUT, and under them as many documents, asks,
// comments and messages, each with one current block, b1, whose links no read of the test
// lists: a mention in the text of one item for each issue, and one from each block, each to a
// block of another of them or to an issue. The targets turn through the block kinds, so every
// link table holds thousands of links into blocks of each kind, as Dispatch's database does, and
// a read of a link table by to_kind alone reads all of them. Replies name blocks too: a third of
// the comments reply to a block of the comment before them, a third answer a part of an ask in its
// thread, and every other message replies to a block of the message before it. Each issue is also
// blocked by the next, has a component that is part of the one before and depends on the next,
// and a session follows each ask, so the arms an issue's, a component's and a session's in read
// reads hold as many rows. A document's block's link is one this build's block index writes, its
// row naming the block's node beside the node and its tuple (tupleLink), so graph_nodes and
// graph_tuples, which the reads' predicates and graph_edges' tuple arm read, hold as many rows.
// Each document is in its issue's room (roomOf), so issue_trees, document_rooms and
// issue_room_blocks, which a document's read reads to find its room, hold as many rows too.
func (w *edgesFixtureWriter) outsideGraph() {
	const n = edgesOutsideNodes
	documents := make([]edgesDocument, n)
	asks, comments, messages := make([]string, n), make([]string, n), make([]string, n)
	for index := range n {
		key := "OUT-" + strconv.Itoa(index+1)
		w.issue(key, "OUT", index+1)
		documents[index] = w.document(new(key), "OUT", "outside-md")
		w.documentBlock(documents[index], "b1", "paragraph")
		w.roomOf(key, "OUT", documents[index], "b1")
		asks[index], _ = w.item(ItemAsk, edgesItem{issue: new(key)})
		w.itemBlock(ItemAsk, asks[index], "b1", new(0))
		comment := edgesItem{issue: new(key)}
		switch index % 3 {
		case 1:
			comment.replyTo, comment.repliesToBlock = new(comments[index-1]), new("comment/"+comments[index-1]+"#b1")
		case 2:
			comment.ask, comment.repliesToBlock = new(asks[index]), new("ask/"+asks[index]+"#b1")
		}
		comments[index], _ = w.item(ItemComment, comment)
		w.itemBlock(ItemComment, comments[index], "b1", new(0))
		message := edgesItem{issue: new(key)}
		if index%2 == 1 {
			message.inReplyTo, message.repliesToBlock = new(messages[index-1]), new("message/"+messages[index-1]+"#b1")
		}
		messages[index], _ = w.item(ItemMessage, message)
		w.itemBlock(ItemMessage, messages[index], "b1", new(0))
	}
	w.add("architecture_sources", 1, "OUT", "sjawhar/out", "main", 1, edgesFixtureActor, w.at())
	for index := range n {
		key, component := "OUT-"+strconv.Itoa(index+1), "component-"+strconv.Itoa(index)
		var parent *string
		if index > 0 {
			parent = new("component-" + strconv.Itoa(index-1))
		}
		w.add("architecture_snapshots", index+1, "OUT", "commit-"+strconv.Itoa(index), "{}", w.at())
		w.add("components", "OUT", component, component, "", parent, index+1, 1)
		w.add("component_depends", "OUT", component, "component-"+strconv.Itoa((index+1)%n))
		w.add("issue_links", key, "blocked_by", "OUT-"+strconv.Itoa((index+1)%n+1), w.at())
		attached := w.at()
		w.add("issue_components", key, "explicit", attached, attached)
		w.add("issue_component_members", key, "OUT", component)
		w.add("ask_followers", asks[index], "session-"+strconv.Itoa(index), w.at())
	}
	target := func(index int) Target {
		other := (index*7 + 3) % n
		switch index % 5 {
		case 0:
			return Target{Kind: "block", ID: documents[other].refKey + "#b1"}
		case 1:
			return Target{Kind: ItemComment.BlockKind(), ID: "comment/" + comments[other] + "#b1"}
		case 2:
			return Target{Kind: ItemMessage.BlockKind(), ID: "message/" + messages[other] + "#b1"}
		case 3:
			return Target{Kind: ItemAsk.BlockKind(), ID: "ask/" + asks[other] + "#b1"}
		}
		return Target{Kind: "issue", ID: "OUT-" + strconv.Itoa(other+1)}
	}
	for index := range n {
		switch index % 4 {
		case 0:
			w.ref("artifact", documents[index].id, target(index))
		case 1:
			w.ref("comment", comments[index], target(index))
		case 2:
			w.ref("message", messages[index], target(index))
		case 3:
			w.ref("ask", asks[index], target(index))
		}
		w.tupleLink(documents[index], "b1", target(index+1), graphstore.StateLive, false)
		w.itemBlockRef(ItemAsk, asks[index], "b1", target(index+2))
		w.itemBlockRef(ItemComment, comments[index], "b1", target(index+3))
		w.itemBlockRef(ItemMessage, messages[index], "b1", target(index+4))
	}
}

// edgesSubject is a node of the five reads as the fixture wrote it: the target it is read as, the
// kind its blocks are stored under, its two current blocks, a block no read lists (one its item
// removed, or one no current version of its document holds), and the edges main's statement
// lists and must not list of the rows written around it, before a read's kind filters.
type edgesSubject struct {
	node           Target
	blockKind      string
	blocks         [2]string
	gone           string
	listed, hidden []edgeKey
}

func (s *edgesSubject) block(index int) Target {
	return Target{Kind: s.blockKind, ID: s.blocks[index]}
}

// read is the subject read by query: main's statement with mainArgs, and what it lists and must
// not list once query's Kinds and ExcludeKinds have dropped the edges of the kinds they drop. A
// read of whole documents writes no probe (probesNone).
func (s *edgesSubject) read(name string, query Query, main string, mainArgs ...any) edgesRead {
	read := edgesRead{name: name, query: query, main: main, mainArgs: mainArgs, hidden: slices.Clone(s.hidden)}
	if query.WholeDocuments {
		read.probes = probesNone
	}
	for _, key := range s.listed {
		if (len(query.Kinds) == 0 || slices.Contains(query.Kinds, key.kind)) && !slices.Contains(query.ExcludeKinds, key.kind) {
			read.listed = append(read.listed, key)
		} else {
			read.hidden = append(read.hidden, key)
		}
	}
	return read
}

// edgesCiter is a document, an ask, a comment or a message of CORE-2 that cites a subject, with
// two current blocks, b1 and b2, and, for an item, a block b3 a later revision removed, which has
// no position.
type edgesCiter struct {
	kind, id, blockKind string
	document            edgesDocument
}

func (w *edgesFixtureWriter) citer(kind string) edgesCiter {
	if kind == "artifact" {
		document := w.document(new("CORE-2"), "CORE", w.slug("citer"))
		w.documentBlock(document, "b1", "paragraph")
		w.documentBlock(document, "b2", "paragraph")
		return edgesCiter{kind: kind, id: document.id, blockKind: "block", document: document}
	}
	item := ItemKind(kind)
	id, _ := w.item(item, edgesItem{issue: new("CORE-2")})
	w.itemBlock(item, id, "b1", new(0))
	w.itemBlock(item, id, "b2", new(1))
	w.itemBlock(item, id, "b3", nil)
	return edgesCiter{kind: kind, id: id, blockKind: item.BlockKind()}
}

// blockLink writes a mention from block blockID of citer, in its kind's block link table, and
// answers its source node id and its time.
func (w *edgesFixtureWriter) blockLink(citer edgesCiter, blockID string, to Target) (string, time.Time) {
	if citer.kind == "artifact" {
		return w.blockRef(citer.document, blockID, "mentions", to, nil)
	}
	return w.itemBlockRef(ItemKind(citer.kind), citer.id, blockID, to)
}

// cite writes the links of two citers of each kind into subject, one row for each arm of the in
// statement that can end at a block, and records in whole what a whole-document read lists of
// them, which for an item is thrown away. The first citer mentions the node in its text alone;
// the first current block in its text and from its block b1, so its own mention is hidden behind
// the block's (heldByABlock); the second current block in its text alone; and the gone block in
// its text and from its block b2, which no read lists. An item's removed block b3 mentions the
// first current block too, which no read lists from b3: the item no longer holds it. The second
// citer mentions the node in its text and from its block b1, so its own mention is hidden behind
// the block's. A whole-document read lists each citer by its own mentions, held or not, and an
// ask by the mentions of its current parts (asksCitingTheDocument), and no block.
func (w *edgesFixtureWriter) cite(subject, whole *edgesSubject) {
	gone := Target{Kind: subject.blockKind, ID: subject.gone}
	for _, kind := range []string{"artifact", "comment", "message", "ask"} {
		citer := w.citer(kind)
		alone := edgeAt("mentions", citer.kind, citer.id, "", w.ref(citer.kind, citer.id, subject.node))
		subject.listed = append(subject.listed, alone)
		whole.listed = append(whole.listed, alone)

		held := edgeAt("mentions", citer.kind, citer.id, subject.blocks[0], w.ref(citer.kind, citer.id, subject.block(0)))
		subject.hidden = append(subject.hidden, held)
		whole.listed = append(whole.listed, held)
		node, at := w.blockLink(citer, "b1", subject.block(0))
		subject.listed = append(subject.listed, edgeAt("mentions", citer.blockKind, node, subject.blocks[0], at))
		whole.hidden = append(whole.hidden, edgeAt("mentions", citer.blockKind, node, subject.blocks[0], at))
		if kind == "ask" {
			whole.listed = append(whole.listed, edgeAt("mentions", "ask", citer.id, subject.blocks[0], at))
		}

		second := edgeAt("mentions", citer.kind, citer.id, subject.blocks[1], w.ref(citer.kind, citer.id, subject.block(1)))
		subject.listed = append(subject.listed, second)
		whole.listed = append(whole.listed, second)

		goneAlone := edgeAt("mentions", citer.kind, citer.id, subject.gone, w.ref(citer.kind, citer.id, gone))
		subject.hidden = append(subject.hidden, goneAlone)
		whole.hidden = append(whole.hidden, goneAlone)
		node, at = w.blockLink(citer, "b2", gone)
		subject.hidden = append(subject.hidden, edgeAt("mentions", citer.blockKind, node, subject.gone, at))
		whole.hidden = append(whole.hidden, edgeAt("mentions", citer.blockKind, node, subject.gone, at))
		if kind == "ask" {
			whole.hidden = append(whole.hidden, edgeAt("mentions", "ask", citer.id, subject.gone, at))
		}

		if kind != "artifact" {
			node, at = w.blockLink(citer, "b3", subject.block(0))
			subject.hidden = append(subject.hidden, edgeAt("mentions", citer.blockKind, node, subject.blocks[0], at))
			whole.hidden = append(whole.hidden, edgeAt("mentions", citer.blockKind, node, subject.blocks[0], at))
			if kind == "ask" {
				whole.hidden = append(whole.hidden, edgeAt("mentions", "ask", citer.id, subject.blocks[0], at))
			}
		}

		other := w.citer(kind)
		ownHeld := edgeAt("mentions", other.kind, other.id, "", w.ref(other.kind, other.id, subject.node))
		subject.hidden = append(subject.hidden, ownHeld)
		whole.listed = append(whole.listed, ownHeld)
		node, at = w.blockLink(other, "b1", subject.node)
		subject.listed = append(subject.listed, edgeAt("mentions", other.blockKind, node, "", at))
		whole.hidden = append(whole.hidden, edgeAt("mentions", other.blockKind, node, "", at))
		if kind == "ask" {
			whole.listed = append(whole.listed, edgeAt("mentions", "ask", other.id, "", at))
		}
	}

	// A document of CORE-2 whose blocks' links this build's block index wrote, which graph_edges
	// answers from its tuple arm (tupleLink): b1's to the first current block, listed from b1; b2's
	// to it too, from a block whose node is gone, and b3's to the second, whose tuple a later one
	// retracts, neither of which any read lists. A whole-document read lists no block.
	stored := w.document(new("CORE-2"), "CORE", w.slug("stored"))
	for _, block := range []string{"b1", "b2", "b3"} {
		w.documentBlock(stored, block, "paragraph")
	}
	node, at := w.tupleLink(stored, "b1", subject.block(0), graphstore.StateLive, false)
	subject.listed = append(subject.listed, edgeAt("mentions", "block", node, subject.blocks[0], at))
	whole.hidden = append(whole.hidden, edgeAt("mentions", "block", node, subject.blocks[0], at))
	node, at = w.tupleLink(stored, "b2", subject.block(0), graphstore.StateGone, false)
	subject.hidden = append(subject.hidden, edgeAt("mentions", "block", node, subject.blocks[0], at))
	whole.hidden = append(whole.hidden, edgeAt("mentions", "block", node, subject.blocks[0], at))
	node, at = w.tupleLink(stored, "b3", subject.block(1), graphstore.StateLive, true)
	subject.hidden = append(subject.hidden, edgeAt("mentions", "block", node, subject.blocks[1], at))
	whole.hidden = append(whole.hidden, edgeAt("mentions", "block", node, subject.blocks[1], at))

	// The arms of graph_edges over the store that end at a block (edgesBlockArms). A document whose
	// row names its node mentions the first current block, which the view answers from the mentions
	// tuple out of that node, dated as the refs row of the pair, while that row stands
	// (keyedMentionArm); a whole-document read lists the document by its own mention. And the
	// relations the store holds with no projection row behind them, which the view answers from the
	// tuples alone (keyedStoreArm): a risk whose check the first current block is, by its
	// prevented_by, and a lens invoked on that block, by its part_of; and a finding of a lens tied to
	// it, which the arm over a lens's findings answers (keyedFindingArm). None of these is listed: a
	// gone risk's prevented_by at the second block, a risk's prevented_by a later tuple retracts, and
	// a lens invoked on the gone block. A whole-document read lists none of the store's relations,
	// since it reads mentions alone.
	marked := w.markedDocument(nil, "CORE", w.slug("marked"))
	at = w.ref("artifact", marked.id, subject.block(0))
	w.storeTuple(graphstore.KindArtifact, marked.refKey, "mentions", subject.block(0), at, true)
	mention := edgeAt("mentions", "artifact", marked.id, subject.blocks[0], at)
	subject.listed = append(subject.listed, mention)
	whole.listed = append(whole.listed, mention)
	stand := func(kind, id, state, relation string, to Target, retracted bool) edgeKey {
		stored := StoredKindOf(kind)
		w.storeNode(stored, id, stored, state)
		at := w.at()
		w.storeTuple(stored, id, relation, to, at, true)
		if retracted {
			w.storeTuple(stored, id, relation, to, w.at(), false)
		}
		key := edgeAt(relation, kind, id, to.ID, at)
		whole.hidden = append(whole.hidden, key)
		return key
	}
	subject.listed = append(subject.listed,
		stand(graphstore.KindRisk, w.uuid(), graphstore.StateLive, graphstore.TuplePreventedBy, subject.block(0), false),
		stand(graphstore.KindLensBlock, graphstore.KindLens+"/"+w.uuid()+"#f1", graphstore.StateLive, graphstore.TupleConstrainedBy, subject.block(0), false),
		stand(graphstore.KindLens, w.uuid(), graphstore.StateLive, graphstore.TuplePartOf, subject.block(0), false))
	subject.hidden = append(subject.hidden,
		stand(graphstore.KindRisk, w.uuid(), graphstore.StateGone, graphstore.TuplePreventedBy, subject.block(1), false),
		stand(graphstore.KindRisk, w.uuid(), graphstore.StateLive, graphstore.TuplePreventedBy, subject.block(1), true),
		stand(graphstore.KindLens, w.uuid(), graphstore.StateLive, graphstore.TuplePartOf, gone, false))
}

// subjectDocument writes the document of the two document reads, of CORE-1, with two current
// blocks and the links of cite into it. Beside them it writes a typed pointer into its first block
// whose target type the pointer's attribute does not allow, which block_ref_listed hides, and the
// pointer's document's own mention of that block, which heldByABlock hides behind the pointer;
// an answered ask's shaped tie to its first block, which the store states on the ask's node and
// graph_edges answers from its tuple arm (shapedTie); and the asks and comments anchored to it and
// owned by it. It answers the subject as the read with WholeDocuments false lists it, and as the
// read with WholeDocuments true does.
func (w *edgesFixtureWriter) subjectDocument() (edgesSubject, edgesSubject) {
	document := w.document(new("CORE-1"), "CORE", "subject-md")
	subject := edgesSubject{node: Target{Kind: "artifact", ID: document.refKey}, blockKind: "block", gone: document.refKey + "#b9"}
	whole := edgesSubject{node: subject.node}
	for index := range subject.blocks {
		node, at := w.documentBlock(document, "b"+strconv.Itoa(index+1), "paragraph")
		subject.blocks[index] = node
		subject.listed = append(subject.listed, edgeAt("part_of", "block", node, "", at))
		whole.listed = append(whole.listed, edgeAt("part_of", "block", node, "", at))
	}
	w.cite(&subject, &whole)

	pointer := w.document(new("CORE-2"), "CORE", w.slug("pointer"))
	w.documentBlock(pointer, "b1", "check")
	mention := edgeAt("mentions", "artifact", pointer.id, subject.blocks[0], w.ref("artifact", pointer.id, subject.block(0)))
	subject.hidden = append(subject.hidden, mention)
	whole.listed = append(whole.listed, mention)
	node, at := w.blockRef(pointer, "b1", "serves", subject.block(0), new("{requirement}"))
	subject.hidden = append(subject.hidden, edgeAt("serves", "block", node, subject.blocks[0], at))
	whole.hidden = append(whole.hidden, edgeAt("serves", "block", node, subject.blocks[0], at))

	decisions := w.document(new("CORE-2"), "CORE", w.slug("decisions"))
	ask, at := w.shapedTie(decisions, "q1", subject.block(0))
	subject.listed = append(subject.listed, edgeAt(TupleShaped, "ask", ask, subject.blocks[0], at))
	whole.listed = append(whole.listed, edgeAt(TupleShaped, "ask", ask, subject.blocks[0], at))

	anchor := new(`{"artifact_id":"` + document.id + `","version":1}`)
	for _, item := range []struct {
		kind ItemKind
		edge string
		row  edgesItem
	}{
		{kind: ItemAsk, edge: "anchored_to", row: edgesItem{issue: new("CORE-2"), anchor: anchor}},
		{kind: ItemComment, edge: "anchored_to", row: edgesItem{issue: new("CORE-2"), anchor: anchor}},
		{kind: ItemAsk, edge: "owned_by", row: edgesItem{artifact: new(document.id)}},
		{kind: ItemComment, edge: "owned_by", row: edgesItem{artifact: new(document.id)}},
	} {
		id, at := w.item(item.kind, item.row)
		subject.listed = append(subject.listed, edgeAt(item.edge, string(item.kind), id, "", at))
		whole.listed = append(whole.listed, edgeAt(item.edge, string(item.kind), id, "", at))
	}
	return subject, whole
}

// subjectItem writes the ask, comment or message of a read, of CORE-1, with two current blocks, a
// block a later revision removed and the links of cite into them. Beside them it writes three
// replies: one to its first block, which is listed under that block and not by its replies_to
// edge to the item; one to the item alone; and one to the removed block, which is listed by its
// edge to the item alone. A comment's replies also name it as the comment they answer, which the
// read folds into their replies_to edges (foldsAnswers). It writes a typed pointer into its second
// block too, which is listed: block_ref_listed reads document_blocks alone, so it hides no pointer
// into an item's block.
func (w *edgesFixtureWriter) subjectItem(kind ItemKind) edgesSubject {
	id, _ := w.item(kind, edgesItem{issue: new("CORE-1")})
	subject := edgesSubject{node: Target{Kind: string(kind), ID: id}, blockKind: kind.BlockKind()}
	for index := range subject.blocks {
		node, at := w.itemBlock(kind, id, "b"+strconv.Itoa(index+1), new(index))
		subject.blocks[index] = node
		subject.listed = append(subject.listed, edgeAt("part_of", subject.blockKind, node, "", at))
	}
	gone, at := w.itemBlock(kind, id, "b3", nil)
	subject.gone = gone
	subject.hidden = append(subject.hidden, edgeAt("part_of", subject.blockKind, gone, "", at))
	w.cite(&subject, &edgesSubject{})

	replies := repliesFrom(kind)
	for _, block := range []string{subject.blocks[0], "", subject.gone} {
		row := edgesItem{issue: new("CORE-1")}
		var named *string
		if block != "" {
			named = new(block)
		}
		switch kind {
		case ItemComment:
			row.replyTo, row.answers, row.repliesToBlock = new(id), new(id), named
		case ItemMessage:
			row.inReplyTo, row.repliesToBlock = new(id), named
		case ItemAsk:
			row.ask, row.repliesToBlock = new(id), named
		}
		reply, at := w.item(replies, row)
		toItem := edgeAt("replies_to", string(replies), reply, "", at)
		switch block {
		case subject.blocks[0]:
			subject.hidden = append(subject.hidden, toItem)
			subject.listed = append(subject.listed, edgeAt("replies_to", string(replies), reply, block, at))
		case "":
			subject.listed = append(subject.listed, toItem)
		default:
			subject.listed = append(subject.listed, toItem)
			subject.hidden = append(subject.hidden, edgeAt("replies_to", string(replies), reply, block, at))
		}
		if kind == ItemComment {
			subject.hidden = append(subject.hidden, edgeAt("answers", string(replies), reply, "", at))
		}
	}

	pointer := w.document(new("CORE-2"), "CORE", w.slug("pointer"))
	w.documentBlock(pointer, "b1", "check")
	subject.hidden = append(subject.hidden, edgeAt("mentions", "artifact", pointer.id, subject.blocks[1], w.ref("artifact", pointer.id, subject.block(1))))
	node, at := w.blockRef(pointer, "b1", "serves", subject.block(1), new("{requirement}"))
	subject.listed = append(subject.listed, edgeAt("serves", "block", node, subject.blocks[1], at))
	return subject
}

// probeCase writes the issue of the probe case, CORE-3, and edgesProbeDocuments documents that
// mention it in their own text and in two of their blocks each: the in read lists each block's
// mention, and hides each document's own behind them (heldByABlock).
func (w *edgesFixtureWriter) probeCase() edgesRead {
	issue := Target{Kind: "issue", ID: "CORE-3"}
	read := edgesRead{
		name: "probe case", query: Query{Direction: "in", Kind: issue.Kind, ID: issue.ID},
		main: mainEdgesInIssue, mainArgs: []any{issue.Kind, issue.ID}, probes: probesAtLeastOne,
	}
	for range edgesProbeDocuments {
		document := w.document(nil, "CORE", w.slug("probe"))
		read.hidden = append(read.hidden, edgeAt("mentions", "artifact", document.id, "", w.ref("artifact", document.id, issue)))
		for _, blockID := range []string{"b1", "b2"} {
			w.documentBlock(document, blockID, "paragraph")
			node, at := w.blockRef(document, blockID, "mentions", issue, nil)
			read.listed = append(read.listed, edgeAt("mentions", "block", node, "", at))
		}
	}
	return read
}

// kindsComponent writes the component of the kinds case, which no package helper writes, as
// backlink_counts_test.go inserts one: under an architecture source and a snapshot of CORE's.
func (w *edgesFixtureWriter) kindsComponent() Target {
	w.add("architecture_sources", 2, "CORE", "sjawhar/core", "main", 1, edgesFixtureActor, w.at())
	w.add("architecture_snapshots", edgesOutsideNodes+1, "CORE", "abc123", "{}", w.at())
	w.add("components", "CORE", "kinds-component", "Kinds", "", nil, edgesOutsideNodes+1, 2)
	return Target{Kind: "component", ID: "CORE/kinds-component"}
}

// kindsMembers writes a node n for each read of edgesProbeCarryingReads, with a document D that
// mentions n in its own text and from its current block b1, and a document E that mentions n in
// its text alone; for a document, an ask, a comment and a message, D and E also mention a current
// block of n in the same two ways. Each read is the Query GET /api/v1/references?to=<n> sends with
// no ?kind= (api.parseReferencesQuery), a session's, which no reference names, with no filter, and
// the whole-document read as refs.ReferencedBy sends it. Each is held to main's statement for that
// Query: mainEdgesInIssue with n's kind bound as $1 for each kind with no arm over blocks,
// and the held text of its read for a document, an ask, a comment, a message and the whole
// document. Main lists E's mentions and those of D's block, and hides D's own behind its block's,
// which a probe of D's block links found; the whole-document read lists D and E by their own
// mentions and no block.
func (w *edgesFixtureWriter) kindsMembers(t *testing.T) []edgesRead {
	t.Helper()
	members := make([]edgesRead, 0, len(edgesProbeCarryingReads))
	for _, name := range edgesProbeCarryingReads {
		var node Target
		var blocks []Target
		switch name {
		case "issue":
			node = Target{Kind: "issue", ID: "CORE-4"}
		case "artifact", "block", edgesWholeDocumentRead:
			document := w.document(nil, "CORE", w.slug("kinds"))
			block, _ := w.documentBlock(document, "b1", "paragraph")
			node, blocks = Target{Kind: "artifact", ID: document.refKey}, []Target{{Kind: "block", ID: block}}
		case "ask", "comment", "message", "ask_block", "comment_block", "message_block":
			kind := ItemKind(strings.TrimSuffix(name, "_block"))
			id, _ := w.item(kind, edgesItem{issue: new("CORE-5")})
			block, _ := w.itemBlock(kind, id, "b1", new(0))
			node, blocks = Target{Kind: string(kind), ID: id}, []Target{{Kind: kind.BlockKind(), ID: block}}
		case "component":
			node = w.kindsComponent()
		case graphstore.KindSession, graphstore.KindPerson, graphstore.KindRole, graphstore.KindPage,
			graphstore.KindConstraint, graphstore.KindInteraction, graphstore.KindLens, graphstore.KindLensKind, graphstore.KindRisk:
			node = Target{Kind: name, ID: name + "-kinds"}
		case graphstore.KindLensBlock:
			// A finding is addressed by its lens and its block id (graphstore.ParseFindingID).
			node = Target{Kind: name, ID: graphstore.KindLens + "/" + w.uuid() + "#b-kinds"}
		default:
			t.Fatalf("the kinds case writes no node for the read %q", name)
		}
		if len(blocks) > 0 && strings.HasSuffix(name, "block") {
			// The member is the block itself, which has no blocks of its own.
			node, blocks = blocks[0], nil
		}
		member := edgesRead{
			name: name, query: Query{Direction: "in", Kind: node.Kind, ID: node.ID},
			main: mainEdgesInIssue, mainArgs: []any{node.Kind, node.ID}, probes: probesAtLeastOne,
		}
		partOf := []string{"part_of"}
		switch name {
		case "artifact":
			member.query.ExcludeKinds = partOf
			member.main, member.mainArgs = mainEdgesInDocument, []any{"artifact", node.ID, partOf}
		case "comment":
			member.query.ExcludeKinds = partOf
			member.main, member.mainArgs = mainEdgesInComment, []any{"comment", node.ID, node.ID, partOf}
		case "message":
			member.query.ExcludeKinds = partOf
			member.main, member.mainArgs = mainEdgesInMessage, []any{"message", node.ID, node.ID, partOf}
		case "ask":
			member.query.ExcludeKinds = AskBacklinkExclusions
			member.main, member.mainArgs = mainEdgesInAskPanel, []any{"ask", node.ID, node.ID, AskBacklinkExclusions}
		case edgesWholeDocumentRead:
			member.query.Kinds, member.query.WholeDocuments = []string{"mentions"}, true
			member.main, member.mainArgs, member.probes = mainEdgesInWholeDocument, []any{"artifact", node.ID, []string{"mentions"}}, probesNone
		}
		d := w.document(nil, "CORE", w.slug("kinds-d"))
		w.documentBlock(d, "b1", "paragraph")
		e := w.document(nil, "CORE", w.slug("kinds-e"))
		for _, target := range append([]Target{node}, blocks...) {
			atBlock := ""
			if target != node {
				atBlock = target.ID
			}
			own := edgeAt("mentions", "artifact", d.id, atBlock, w.ref("artifact", d.id, target))
			source, at := w.blockRef(d, "b1", "mentions", target, nil)
			byBlock := edgeAt("mentions", "block", source, atBlock, at)
			alone := edgeAt("mentions", "artifact", e.id, atBlock, w.ref("artifact", e.id, target))
			if member.query.WholeDocuments {
				member.listed = append(member.listed, own, alone)
				member.hidden = append(member.hidden, byBlock)
				continue
			}
			member.listed = append(member.listed, alone, byBlock)
			member.hidden = append(member.hidden, own)
		}
		members = append(members, member)
	}
	return members
}
