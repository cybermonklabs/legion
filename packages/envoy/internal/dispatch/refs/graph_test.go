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

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The constants below are main's text, at commit b459fa923705, of each statement of the graph
// read that INFRA-4704 keys: refs.Edges' in read of a node, as readEdgeRows writes it for the
// Query each caller named sends, and the four loaders resolveNodes runs for the comments, asks,
// messages and blocks at the other end of an edge. Each was captured from that commit's code by
// a throwaway test that called the function through a Queryer recording the text it sent and the
// arguments it bound, and is held here letter for letter, as mainClosureQuery is: a text built
// from the package's own fragments would move with them, and main and the head would then agree
// however the head's statement changed. The ids a capture bound are not in any text: every
// statement binds them.

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

// TestEdgesIntoANodesBlocksAnswerWhatMainsStatementDid is c-graph-edges (INFRA-4855). refs.Edges'
// in read of a document (WholeDocuments false, and true with Kinds mentions), a message, a
// comment and an ask (with no exclusion, and with AskBacklinkExclusions as its panel reads it)
// answers, row for row and in order, what main's statement answered over the same data: the raw
// rows of the statement Edges sends first, run again, against main's text held above. The rows
// are compared before Edges resolves them, because a source block with no position resolves to
// no node, so a comparison of resolved edges would miss a statement that lists one.
//
// The plan of each of those statements, read under explain (analyze) with sequential scans off
// and under the default settings, reads every link table that it ran a scan of by target, with an
// Index Cond on to_id, and reads no blocks table whole. heldByABlock's probe of block_refs, in
// every plan the test reads, keys the source document: its Index Cond holds to_kind, to_id and
// (artifact_id)::text, and it removes no row by a filter. The probe case reads the in read of an
// issue that edgesProbeDocuments documents mention in their text and in two of their blocks each,
// the kinds case the in read of a node of every kind that carries the probe, and the index case
// block_refs' indexes. Every table the statements read holds at least edgesOutsideNodes rows,
// written by COPY and analyzed, so the default settings plan from real sizes.
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

	t.Run("outside graph", func(t *testing.T) {
		for _, subject := range fixture.subjects {
			inside, outside := blockLinkCounts(t, pool, subject)
			if inside == 0 || outside < 5*inside {
				t.Errorf("the fixture holds %d links into the blocks of %s and %d into other blocks of kind %s; want at least five times as many outside", inside, subject.node, outside, subject.blockKind)
			}
		}
	})

	t.Run("edges", func(t *testing.T) {
		for _, read := range reads {
			t.Run(read.name, func(t *testing.T) {
				requireMainsRows(t, pool, read)
				if read.query.Kind == "artifact" && !read.query.WholeDocuments {
					t.Logf("refs.Edges' in statement of a document with WholeDocuments false:\n%s", read.statement.sql)
				}
			})
		}
	})

	t.Run("plan", func(t *testing.T) {
		for _, read := range reads {
			for setting, settings := range edgesPlanSettings {
				t.Run(read.name+"/"+settings.name, func(t *testing.T) {
					requireKeyedReads(t, read.plans[setting])
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
					if member.main != "" {
						requireMainsRows(t, pool, member)
						return
					}
					got := edgeStatementRows(t, pool, member.statement)
					if !slices.Equal(sortedEdgeKeys(got), sortedEdgeKeys(member.rows)) {
						t.Errorf("the in read of %s %s answered %s; want the mentions of E and of D's block, and none of D's own (heldByABlock): %s", member.query.Kind, member.query.ID, got, member.rows)
					}
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
		relations := map[string]bool{}
		for _, read := range every {
			for _, plan := range read.plans {
				for _, scan := range explaintest.Scans(t, plan) {
					relations[scan.Relation] = true
				}
			}
		}
		for _, relation := range slices.Sorted(maps.Keys(relations)) {
			var count int
			if err := pool.QueryRow(context.Background(), "select count(*) from "+pgx.Identifier{relation}.Sanitize()).Scan(&count); err != nil {
				t.Fatalf("count the rows of %s: %v", relation, err)
			}
			if count < edgesOutsideNodes {
				t.Errorf("the statements read %s, which holds %d rows; want at least %d, so the default settings plan from a real size", relation, count, edgesOutsideNodes)
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

// edgesProbeCarryingReads is the list "The in reads that carry heldByABlock's probe" of
// INFRA-4704's plan: refs.Edges' in read of a node of every kind nodeLoaders resolves, whose
// first arm carries the probe for every kind, and the whole-document read, which carries none. The
// kinds case compares it with nodeKinds first, so a kind a later build resolves fails it until it
// is listed here, and the fixture writes a node of each.
var edgesProbeCarryingReads = []string{
	"artifact", "ask", "ask_block", "block", "comment", "comment_block", "component", "issue",
	"message", "message_block", "session", edgesWholeDocumentRead,
}

// edgesPlanSettings are the two settings each plan is read under: sequential scans off, which
// leaves the planner only the reads an index gives it, and the default settings, which plan from
// the fixture's analyzed sizes. JIT is off under both, as on production's database (D11 of
// INFRA-4704's plan): it changes how a plan runs, not which plan is chosen.
var edgesPlanSettings = [2]struct {
	name     string
	settings []string
}{
	{name: "enable_seqscan off", settings: []string{"set local jit = off", "set local enable_seqscan = off"}},
	{name: "default settings", settings: []string{"set local jit = off"}},
}

// edgesLinkTables are the tables that store a link, and edgesBlockTables the tables of the blocks
// a link starts or ends at.
var (
	edgesLinkTables  = []string{"refs", "block_refs", "ask_block_refs", "comment_block_refs", "message_block_refs"}
	edgesBlockTables = []string{"document_blocks", "ask_blocks", "comment_blocks", "message_blocks"}
)

// An Index Cond keys a column when one of its comparisons has that column on the left, which is
// where the plan writes an index's column, alone or under the scan's alias, each comparison in
// parentheses. namesSourceDocument finds a scan of block_refs that tests the source document at
// all, in its Index Cond or its Filter: heldByABlock's probe.
var (
	condKeysToKind         = regexp.MustCompile(`\((?:[a-z_][a-z0-9_]*\.)?to_kind = `)
	condKeysToID           = regexp.MustCompile(`\((?:[a-z_][a-z0-9_]*\.)?to_id = `)
	condKeysSourceDocument = regexp.MustCompile(`\(\((?:[a-z_][a-z0-9_]*\.)?artifact_id\)::text = `)
	namesSourceDocument    = regexp.MustCompile(`\bartifact_id\b`)
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
// that statement's plan under each of edgesPlanSettings, and the answer it is held to. That is
// main's statement with the arguments main bound, which must list every key of listed and none of
// hidden, so a fixture main did not answer as it was written to decides nothing; or, where main
// is empty, rows, the read's whole answer.
type edgesRead struct {
	name           string
	query          Query
	main           string
	mainArgs       []any
	listed, hidden []edgeKey
	rows           []edgeKey
	probes         edgesProbes
	statement      recordedStatement
	plans          [2]json.RawMessage
}

// edgesStatement is the statement refs.Edges sends first for query, its in statement.
func edgesStatement(t *testing.T, q Queryer, query Query) recordedStatement {
	t.Helper()
	recorder := &statementRecorder{Queryer: q}
	if _, err := Edges(context.Background(), recorder, query); err != nil {
		t.Fatalf("read the edges of %+v: %v", query, err)
	}
	if len(recorder.statements) == 0 {
		t.Fatalf("refs.Edges sent no statement for %+v", query)
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
// or a scan with no Index Cond, the two forms a read of the whole table takes, as the input of a
// hash join or anywhere else; a read of a source's block by its item and its block id is keyed.
func requireKeyedReads(t *testing.T, plan json.RawMessage) {
	t.Helper()
	failed := false
	fail := func(format string, args ...any) {
		t.Helper()
		failed = true
		t.Errorf(format, args...)
	}
	for _, node := range explaintest.Nodes(t, plan) {
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
	if failed {
		t.Logf("the plan's scans:%s", planScans(t, plan))
	}
}

// requireProbeRule holds plan to c-graph-edges' probe rule: every scan of block_refs that ran and
// tests the source document (heldByABlock's probe) reads block_refs through an Index Cond that
// holds to_kind, to_id and (artifact_id)::text, a Bitmap Heap Scan through its Bitmap Index Scans,
// and removes no row by a filter; no scan of block_refs that ran is a sequential scan; and as many
// probes ran as want says. Rows Removed by Filter is an average for each loop, rounded to a whole
// number, so no row removed is at most 0.5 times the loops in all (D12).
func requireProbeRule(t *testing.T, plan json.RawMessage, want edgesProbes) {
	t.Helper()
	failed := false
	ran := 0
	judge := func(scan string, loops float64, cond, filter string, removed float64) {
		t.Helper()
		if !namesSourceDocument.MatchString(cond) && !namesSourceDocument.MatchString(filter) {
			return
		}
		ran++
		if !condKeysToKind.MatchString(cond) || !condKeysToID.MatchString(cond) || !condKeysSourceDocument.MatchString(cond) {
			failed = true
			t.Errorf("heldByABlock's probe %s (loops %g) reads block_refs with Index Cond %q and Filter %q: its Index Cond does not hold to_kind, to_id and (artifact_id)::text", scan, loops, cond, filter)
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
			judge(node.String(), node.Loops, node.IndexCond, node.Filter, node.RowsRemovedByFilter)
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
		judge(scan.Scan.String(), scan.Scan.Loops, strings.Join(conds, " AND "), scan.Scan.Filter, scan.Scan.RowsRemovedByFilter)
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

// blockLinkCounts is how many links the fixture holds into the blocks of subject, current or
// not, and how many into other blocks of the same kind: rows of every link table, and replies
// that name a block.
func blockLinkCounts(t *testing.T, q Queryer, subject edgesSubject) (inside, outside int) {
	t.Helper()
	if err := q.QueryRow(context.Background(), `
		select count(*) filter (where to_id = any($2)), count(*) filter (where not to_id = any($2))
		from (
			select to_id from refs where to_kind = $1
			union all select to_id from block_refs where to_kind = $1
			union all select to_id from ask_block_refs where to_kind = $1
			union all select to_id from comment_block_refs where to_kind = $1
			union all select to_id from message_block_refs where to_kind = $1
			union all select replies_to_block from comments
			where replies_to_block is not null
			and $1 = case when replies_to_block like 'ask/%' then 'ask_block' else 'comment_block' end
			union all select replies_to_block from messages where replies_to_block is not null and $1 = 'message_block'
		) links (to_id)`, subject.blockKind, []string{subject.blocks[0], subject.blocks[1], subject.gone}).Scan(&inside, &outside); err != nil {
		t.Fatalf("count the links into blocks of kind %s: %v", subject.blockKind, err)
	}
	return inside, outside
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
// carry, in the order the copies run: each after the tables its foreign keys name.
var edgesFixtureTables = []struct {
	name    string
	columns []string
}{
	{name: "projects", columns: []string{"key", "name", "default_assignee"}},
	{name: "architecture_sources", columns: []string{"id", "project_key", "repo", "branch", "installation_id", "created_by"}},
	{name: "architecture_snapshots", columns: []string{"id", "project_key", "commit", "files", "imported_at"}},
	{name: "components", columns: []string{"project_key", "id", "title", "prose", "parent", "snapshot_id", "source_id"}},
	{name: "component_depends", columns: []string{"project_key", "from_id", "to_id"}},
	{name: "issues", columns: []string{"key", "project_key", "number", "title", "created_by", "rank", "created_at", "updated_at"}},
	{name: "issue_links", columns: []string{"issue_key", "kind", "target_key", "created_at"}},
	{name: "issue_components", columns: []string{"issue_key", "mode"}},
	{name: "issue_component_members", columns: []string{"issue_key", "project_key", "component_id"}},
	{name: "artifacts", columns: []string{"id", "issue_key", "project_key", "slug", "name", "kind", "ref_key", "created_by", "created_at"}},
	{name: "asks", columns: []string{"id", "issue_key", "artifact_id", "anchor", "author", "question", "created_at"}},
	{name: "ask_followers", columns: []string{"ask_id", "session_id", "since"}},
	{name: "comments", columns: []string{"id", "issue_key", "artifact_id", "anchor", "author", "body", "reply_to", "answers", "ask_id", "replies_to_block", "created_at"}},
	{name: "messages", columns: []string{"id", "issue_key", "author", "body", "in_reply_to", "replies_to_block", "created_at"}},
	{name: "document_blocks", columns: []string{"artifact_id", "block_id", "node_id", "type", "text_sha256", "excerpt", "changed_version", "changed_at", "created_at"}},
	{name: "ask_blocks", columns: []string{"ask_id", "block_id", "node_id", "position", "type", "text_sha256", "excerpt", "made_at", "changed_revision", "changed_at", "removed_at", "created_at"}},
	{name: "comment_blocks", columns: []string{"comment_id", "block_id", "node_id", "position", "type", "text_sha256", "excerpt", "made_at", "changed_revision", "changed_at", "removed_at", "created_at"}},
	{name: "message_blocks", columns: []string{"message_id", "block_id", "node_id", "position", "type", "text_sha256", "excerpt", "made_at", "changed_at", "removed_at", "created_at"}},
	{name: "refs", columns: []string{"from_kind", "from_id", "to_kind", "to_id", "kind", "created_at"}},
	{name: "block_refs", columns: []string{"artifact_id", "block_id", "from_node", "to_kind", "to_id", "kind", "to_types", "created_version", "created_at"}},
	{name: "ask_block_refs", columns: []string{"ask_id", "block_id", "from_node", "to_kind", "to_id", "kind", "created_at"}},
	{name: "comment_block_refs", columns: []string{"comment_id", "block_id", "from_node", "to_kind", "to_id", "kind", "created_at"}},
	{name: "message_block_refs", columns: []string{"message_id", "block_id", "from_node", "to_kind", "to_id", "kind", "created_at"}},
}

// newEdgesFixture writes the fixture in one transaction, a COPY for each table, commits it and
// analyzes every table it wrote.
func newEdgesFixture(t *testing.T) edgesFixture {
	t.Helper()
	w := &edgesFixtureWriter{rows: map[string][][]any{}}
	w.add("projects", "CORE", "CORE project", "alice")
	w.add("projects", "OUT", "OUT project", "alice")
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
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	// The tables' triggers are off for the copies and on again before the commit, in the one
	// transaction, so no other session sees them off, as testdata/coverage_corpus.sql of
	// embedqueue loads its corpus: they write the search vectors, the owed queues, the user
	// threads and the watermarks, which no read here reads, and they were most of the load's
	// time. artifacts_ref_key is one of them, so the copy writes each document's ref_key itself.
	tables := make([]string, 0, len(edgesFixtureTables))
	for _, table := range edgesFixtureTables {
		tables = append(tables, table.name)
	}
	for _, table := range tables {
		if _, err := tx.Exec(ctx, "alter table "+table+" disable trigger user"); err != nil {
			t.Fatalf("turn the triggers of %s off: %v", table, err)
		}
	}
	for _, table := range edgesFixtureTables {
		copyText(t, tx, table.name, table.columns, w.rows[table.name])
	}
	for _, table := range tables {
		if _, err := tx.Exec(ctx, "alter table "+table+" enable trigger user"); err != nil {
			t.Fatalf("turn the triggers of %s on: %v", table, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the fixture: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, "analyze "+strings.Join(tables, ", ")); err != nil {
		t.Fatalf("analyze the fixture: %v", err)
	}

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
	reads[1].probes = probesNone
	return edgesFixture{
		database: database, subjects: []edgesSubject{document, message, comment, ask},
		reads: reads, probe: probe, kinds: kinds,
	}
}

// edgesFixtureWriter gathers the fixture's rows, table by table, for one COPY of each. Every row
// takes a time of its own (at), so every read's rows are ordered by created_at alone and an order
// a statement got wrong shows, and every uuid and slug is its own too.
type edgesFixtureWriter struct {
	rows  map[string][][]any
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
	w.add("artifacts", document.id, issue, project, slug, slug, "doc", document.refKey, edgesFixtureActor, w.at())
	return document
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
// names the types its attribute allows, and answers its source node id and its time.
func (w *edgesFixtureWriter) blockRef(document edgesDocument, blockID, kind string, to Target, toTypes *string) (string, time.Time) {
	node, at := document.refKey+"#"+blockID, w.at()
	w.add("block_refs", document.id, blockID, node, to.Kind, to.ID, kind, toTypes, 1, at)
	return node, at
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
// reads hold as many rows.
func (w *edgesFixtureWriter) outsideGraph() {
	const n = edgesOutsideNodes
	documents := make([]edgesDocument, n)
	asks, comments, messages := make([]string, n), make([]string, n), make([]string, n)
	for index := range n {
		key := "OUT-" + strconv.Itoa(index+1)
		w.issue(key, "OUT", index+1)
		documents[index] = w.document(new(key), "OUT", "outside-md")
		w.documentBlock(documents[index], "b1", "paragraph")
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
	w.add("architecture_sources", 1, "OUT", "sjawhar/out", "main", 1, edgesFixtureActor)
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
		w.add("issue_components", key, "explicit")
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
		w.blockRef(documents[index], "b1", "mentions", target(index+1), nil)
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
// not list once query's Kinds and ExcludeKinds have dropped the edges of the kinds they drop.
func (s *edgesSubject) read(name string, query Query, main string, mainArgs ...any) edgesRead {
	read := edgesRead{name: name, query: query, main: main, mainArgs: mainArgs, hidden: slices.Clone(s.hidden)}
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
}

// subjectDocument writes the document of the two document reads, of CORE-1, with two current
// blocks and the links of cite into it. Beside them it writes a typed pointer into its first block
// whose target type the pointer's attribute does not allow, which block_ref_listed hides, and the
// pointer's document's own mention of that block, which heldByABlock hides behind the pointer;
// and the asks and comments anchored to it and owned by it. It answers the subject as the read
// with WholeDocuments false lists it, and as the read with WholeDocuments true does.
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
	w.add("architecture_sources", 2, "CORE", "sjawhar/core", "main", 1, edgesFixtureActor)
	w.add("architecture_snapshots", edgesOutsideNodes+1, "CORE", "abc123", "{}", w.at())
	w.add("components", "CORE", "kinds-component", "Kinds", "", nil, edgesOutsideNodes+1, 2)
	return Target{Kind: "component", ID: "CORE/kinds-component"}
}

// kindsMembers writes a node n for each read of edgesProbeCarryingReads, with a document D that
// mentions n in its own text and from its current block b1, and a document E that mentions n in
// its text alone; for a document, an ask, a comment and a message, D and E also mention a current
// block of n in the same two ways. Each read is the Query GET /api/v1/references?to=<n> sends with
// no ?kind= (api.parseReferencesQuery), a session's, which no reference names, with no filter, and
// the whole-document read as refs.ReferencedBy sends it. A member's rows are E's mentions and
// those of D's block: D's own are hidden behind its block's, which a probe of D's block links
// found. The whole-document read lists D and E by their own mentions, as main's statement does.
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
		case "session":
			node = Target{Kind: "session", ID: "session-kinds"}
		default:
			t.Fatalf("the kinds case writes no node for the read %q", name)
		}
		if strings.HasSuffix(name, "block") {
			// The member is the block itself, which has no blocks of its own.
			node, blocks = blocks[0], nil
		}
		member := edgesRead{name: name, query: Query{Direction: "in", Kind: node.Kind, ID: node.ID}, probes: probesAtLeastOne}
		switch name {
		case "artifact", "comment", "message":
			member.query.ExcludeKinds = []string{"part_of"}
		case "ask":
			member.query.ExcludeKinds = AskBacklinkExclusions
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
			if member.main != "" {
				member.listed = append(member.listed, own, alone)
				member.hidden = append(member.hidden, byBlock)
				continue
			}
			member.rows = append(member.rows, alone, byBlock)
		}
		members = append(members, member)
	}
	return members
}
