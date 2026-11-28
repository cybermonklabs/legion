package api

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// lastHeldDocumentVisible applies both the project and the graph predicates. A known address,
// including the address of an alias, says nothing about what the caller can read.
func lastHeldDocumentVisible(alias string) string {
	return alias + `.kind in ('doc', 'tree') and ` + access.DocumentClause(alias, "$3") + `
		and ` + refs.VisibleKind(graphstore.KindArtifact, alias+".ref_key", "$2") + `
		and (` + alias + `.issue_key is null or (` + access.IssueProjectClause(alias+".issue_key", "$3") + `
		and ` + refs.VisibleKind(graphstore.KindIssue, alias+".issue_key", "$2") + `))`
}

// lastHeldCut bounds a read at the recorded query cut, which the statement joins as cut: a
// record's cut, which holds each tuple whose seq is at or below its read_seq (refs.TreeCut).
const lastHeldCut = `t.seq <= cut.read_seq`

// lastHeldBlocksStatement finds the real gone node and its last document before reading versions.
// ReadableFarEnds resolves visible live aliases. NamedBlockSQL also follows the address after the
// block row goes, so the history read checks the resolved node's current visibility and gone state
// separately. Check the source, the storage key and the document that last held the block, too.
//
// A query version uses the cut that issueTreeCut reads. Query-only revisions can have a cut but
// no document_indexed_versions entry (queueRevisionStand). Find the block's state, placement and
// type at the recorded cut, independently of the text index. The join to query artifacts is by
// issue and query type, not by the document whose fragment contains the block.
//
// For other blocks, use BlockTextAt's sparse text rule. An indexed version holds the last text
// row at or before it, including removal's null row. MAX of non-null text rows would miss
// unchanged versions. If a block changed fragments, the room's text rows do not say which
// document's version number they carry (MovedRoomBlockSQL); do not guess a document pin.
//
// spec_chain and spec_scope implement docs.ViewOf's current-issue rule in the same batch:
// follow parent_key up, then carry the nearest non-view spec down. A view's Spec query can
// answer its memo, but not a requirement or criterion of its own. The tests of each issue of the
// chain are docs' own (docs.ViewableSQL, docs.RequirementHeldSQL); the judgement from the root
// down, which docs makes in Go (viewOfChain), is the recursion of spec_scope.
var lastHeldBlocksStatement = `
	with recursive addressed as materialized (
		select request.address, ` + refs.NamedBlockSQL("request.address") + ` as node
		from (select distinct unnest($1::text[]) as address) request
		join artifacts source on source.ref_key = split_part(request.address, '#', 1)
		where ` + lastHeldDocumentVisible("source") + `
	), gone as materialized (
		select addressed.address, held.id as node, held.supertag, keyed.id as indexed_under, keyed.kind as keyed_kind,
		       substr(held.id, length(keyed.ref_key) + 2) as block_id
		from addressed
		join graph_nodes held on held.kind = '` + refs.BlockNodeKind + `' and held.id = addressed.node and held.state = $4
		join artifacts keyed on keyed.ref_key = split_part(held.id, '#', 1)
		where ` + refs.VisibleKindRow(refs.BlockNodeKind, "held", "$2") + ` and ` + lastHeldDocumentVisible("keyed") + `
	), visible as materialized (
		select gone.*, doc.id as document, doc.slug, doc.issue_key, doc.project_key
		from gone
		join artifacts doc on doc.id = case when gone.keyed_kind = 'tree'
			then ` + refs.RoomBlockDocumentSQL("gone.indexed_under", "gone.block_id") + ` else gone.indexed_under end
		where ` + lastHeldDocumentVisible("doc") + `
	), spec_chain (subject, key, parent, depth) as (
		select distinct a.key, a.key, a.parent_key, 0
		from visible block join issues a on a.key = block.issue_key
		where exists (select 1 from jsonb_each($5::jsonb) rule where rule.value ? block.supertag)
		union all
		select chain.subject, a.key, a.parent_key, chain.depth + 1
		from spec_chain chain join issues a on a.key = chain.parent
		where chain.depth < $6
	), spec_ancestors as materialized (
		select chain.*, a.status = 'done' as closed,
		       ` + docs.ViewableSQL("v") + ` as viewable,
		       chain.depth > 0 and ` + docs.RequirementHeldSQL("a.key") + ` as has_requirements,
		       max(chain.depth) over (partition by chain.subject) as deepest
		from spec_chain chain
		join issues a on a.key = chain.key
		left join issue_views v on v.issue_key = chain.key
	), spec_scope (subject, depth, nearest_closed, is_view) as (
		select a.subject, a.depth,
		       case when a.has_requirements then a.closed end, false
		from spec_ancestors a where a.depth = a.deepest
		union all
		select a.subject, a.depth,
		       case when a.has_requirements and not (a.viewable and parent.nearest_closed is false)
		         then a.closed else parent.nearest_closed end,
		       a.viewable and parent.nearest_closed is false
		from spec_scope parent join spec_ancestors a on a.subject = parent.subject and a.depth = parent.depth - 1
	), query_blocks as materialized (
		select block.address, block.node, block.issue_key, block.block_id, block.supertag, q.id as query_id, q.slug,
		       claim.attributes->>'` + graphstore.AttributeQuery + `' as query
		from visible block
		join artifacts q on q.issue_key = block.issue_key
		join graph_nodes claim on claim.kind = 'artifact' and claim.id = q.ref_key
		  and claim.supertag = '` + graphstore.SupertagQuery + `'
		  and ($5::jsonb -> (claim.attributes->>'` + graphstore.AttributeQuery + `')) ? block.supertag
		where ` + lastHeldDocumentVisible("q") + `
		  and (claim.attributes->>'` + graphstore.AttributeQuery + `' <> 'spec' or block.supertag = 'callout'
		    or not exists (select 1 from spec_scope scope where scope.subject = block.issue_key and scope.depth = 0 and scope.is_view))
	), query_presence as materialized (
	select block.address, block.issue_key, block.slug, block.block_id, version.number, block.query,
	       opening.held as opening, (block.supertag <> 'callout' or block.node = memo.from_id) as taken
	from query_blocks block
	join version_renders cut on cut.artifact_id = block.query_id
	  and cut.read_seq is not null and cut.at is not null and not cut.tree_mismatch
	join artifact_versions version on version.artifact_id = cut.artifact_id and version.number = cut.number
	cross join lateral (` + refs.AsOfValue(`'`+refs.BlockNodeKind+`'`, "block.node", graphstore.AttributeText, lastHeldCut, true) + `) opening
	left join lateral (
		select first_child.from_id
		from (
			select distinct on (t.from_kind, t.from_id) t.from_kind, t.from_id, t.asserted
			from graph_tuples t
			where t.to_kind = 'issue' and t.to_id = block.issue_key and t.kind = '` + graphstore.TuplePartOf + `' and ` + lastHeldCut + `
			order by t.from_kind, t.from_id, t.at desc, t.seq desc
		) first_child
		cross join lateral (` + refs.AsOfPlacement("first_child.from_kind", "first_child.from_id", lastHeldCut) + `) parent
		cross join lateral (` + refs.AsOfValue("first_child.from_kind", "first_child.from_id", refs.AttributeRank, lastHeldCut, false) + `) rank
		where block.supertag = 'callout' and first_child.asserted and rank.held is not null
		  and parent.to_kind = 'issue' and parent.to_id = block.issue_key
		  and first_child.from_kind not in ('comment', 'issue', 'step', 'plan', 'handoff')
		  and ` + refs.TreeOwnsSQL("block.issue_key", "first_child.from_kind", "first_child.from_id") + `
		  and (` + refs.AsOfValue("first_child.from_kind", "first_child.from_id", graphstore.AttributeState, lastHeldCut, false) + `) is distinct from $4
		order by rank.held collate "C", (first_child.from_kind || ':' || first_child.from_id) collate "C"
		limit 1
	) memo on true
	where opening.held is not null and exists (
		with recursive placed(kind, id, child_kind, child_id) as (
			select '` + refs.BlockNodeKind + `'::text, block.node, null::text, null::text
			union
			select parent.to_kind, parent.to_id, child.kind, child.id
			from placed child
			cross join lateral (` + refs.AsOfPlacement("child.kind", "child.id", lastHeldCut) + `) parent
			-- The climb ends at a comment or a message, or a block of one. The part_of log alone does
			-- not prove that the walk's conversation held it at this cut; the item's row does (below).
			where child.kind not in ('issue', 'comment', 'comment_block', 'message', 'message_block')
			  and ` + refs.TreeOwnsSQL("block.issue_key", "child.kind", "child.id") + `
			  and (` + refs.AsOfValue("child.kind", "child.id", graphstore.AttributeState, lastHeldCut, false) + `) is distinct from $4
		)
		select 1 from placed where kind = 'issue' and id = block.issue_key
		union all
		-- A comment or a message is a root of the walk where the walk's conversation reads it
		-- at the cut (WalkIssueTree, refs.ConversationNodes): an item of the block's issue
		-- written at or before the cut's time. The conversation also leaves out an item no
		-- person wrote or wrote in the thread of, which this statement does not test: a write
		-- places a block under no such item (pmdoc's itemReach), so only a block placed before
		-- that rule could stand under one.
		select 1 from placed item_root
		join comments item on item.id = ` + store.CastableUUID("item_root.id") + `
		where item_root.kind = '` + graphstore.KindComment + `' and item.issue_key = block.issue_key and item.created_at <= cut.at
		union all
		select 1 from placed item_root
		join messages item on item.id = ` + store.CastableUUID("item_root.id") + `
		where item_root.kind = '` + graphstore.KindMessage + `' and item.issue_key = block.issue_key and item.created_at <= cut.at
		union all
		-- A block of such a comment or message is a root of the walk as well: readConversation
		-- puts each current block of an item of the conversation in the frontier. The item's table
		-- of blocks names the block's item by the block's address; read the item's row through it.
		select 1 from placed item_root
		join comment_blocks own on own.node_id = item_root.id
		join comments item on item.id = own.comment_id
		where item_root.kind = '` + refs.ItemComment.BlockKind() + `' and item.issue_key = block.issue_key and item.created_at <= cut.at
		union all
		select 1 from placed item_root
		join message_blocks own on own.node_id = item_root.id
		join messages item on item.id = own.message_id
		where item_root.kind = '` + refs.ItemMessage.BlockKind() + `' and item.issue_key = block.issue_key and item.created_at <= cut.at
		union all
		-- A block placed directly under an ancestor's block, or under an ancestor's ask, comment
		-- or message or a block of one (refs.ItemOfIssuesSQL), is also a root of the second half
		-- of WalkIssueTree (TreePlacedAbove). Prove its membership at the cut.
		select 1 from placed foreign_parent
		join spec_chain ancestor on ancestor.subject = block.issue_key and ancestor.depth > 0
		where foreign_parent.child_kind = '` + refs.BlockNodeKind + `'
		  and (foreign_parent.kind = '` + refs.BlockNodeKind + `' and ancestor.key = split_part(foreign_parent.id, '/', 1)
		    or ` + refs.ItemOfIssuesSQL(refs.ItemAsk, "foreign_parent.kind", "foreign_parent.id", "array[ancestor.key]") + `
		    or ` + refs.ItemOfIssuesSQL(refs.ItemComment, "foreign_parent.kind", "foreign_parent.id", "array[ancestor.key]") + `
		    or ` + refs.ItemOfIssuesSQL(refs.ItemMessage, "foreign_parent.kind", "foreign_parent.id", "array[ancestor.key]") + `)
		  and exists (
		      select 1 from artifacts d
		      cross join lateral (` + refs.AsOfPlacement(`'`+graphstore.KindArtifact+`'`, "d.ref_key", lastHeldCut) + `) top
		      where d.issue_key = block.issue_key and top.to_kind = 'issue' and top.to_id = block.issue_key
		        and (select t.asserted from graph_tuples t
		             where t.from_kind = '` + refs.BlockNodeKind + `' and t.from_id = foreign_parent.child_id
		               and t.kind = '` + refs.TupleInDocument + `' and t.to_kind = 'artifact' and t.to_id = d.ref_key
		               and ` + lastHeldCut + ` order by t.at desc, t.seq desc limit 1)
		  )
	))
	select address, issue_key, '' as project_key, slug, block_id, max(number), query, opening
	from query_presence where taken
	group by address, issue_key, slug, block_id, query, opening
	union all
	select block.address, coalesce(block.issue_key, ''), coalesce(block.project_key, ''), block.slug, block.block_id,
	       held.number, '', ''
	from visible block
	cross join lateral (
		select version.number
		from document_indexed_versions indexed
		join artifact_versions version on version.artifact_id = indexed.artifact_id and version.number = indexed.version
		join lateral (
			select stored.text from document_block_texts stored
			where stored.artifact_id in (block.document, block.indexed_under) and stored.block_id = block.block_id
			  and stored.version <= version.number
			order by stored.version desc, (stored.artifact_id = block.indexed_under) desc limit 1
		) history on history.text is not null
		where indexed.artifact_id = block.document
		order by version.number desc limit 1
	) held
	where (not exists (select 1 from query_blocks q where q.address = block.address)
	  or (block.supertag = 'callout'
	    and exists (select 1 from query_presence q where q.address = block.address and q.taken is false)
	    and not exists (select 1 from query_presence q where q.address = block.address and q.taken)))
	  and not ` + refs.MovedRoomBlockSQL("block.indexed_under", "block.block_id") + `
	order by 1, 6 desc`

var lastHeldQueryTypes = map[string][]string{
	graphstore.QuerySpec:         graphstore.QueryTakes(graphstore.QuerySpec),
	graphstore.QueryVerification: graphstore.QueryTakes(graphstore.QueryVerification),
	graphstore.QueryPlan:         graphstore.QueryTakes(graphstore.QueryPlan),
}

func (s *server) referenceTargetsLastHeld(ctx context.Context, who model.Actor, answers []model.ReferenceTarget, named []refs.Target, parsed []text.Ref) error {
	var candidates []string
	var positions []int
	for i, target := range named {
		if target.Kind == refs.BlockNodeKind && answers[i].Gone && !answers[i].Ambiguous {
			// Check the cited document even if live resolution rewrote the target.
			candidates = append(candidates, text.TargetID(parsed[i]))
			positions = append(positions, i)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	viewer := who.Viewer
	if viewer == nil {
		resolved, err := access.Resolve(ctx, s.deps.Store.Pool, who)
		if err != nil {
			return err
		}
		viewer = &resolved
	}
	nullable := func(value string) any {
		if value == "" {
			return nil
		}
		return value
	}
	rows, err := s.deps.Store.Pool.Query(ctx, lastHeldBlocksStatement, candidates, refs.VisibilityArg(who),
		map[string]any{"sees": viewer.Sees, "all": viewer.All, "open": viewer.Open, "private": viewer.AnyPrivate,
			"person": nullable(viewer.Person), "session": nullable(viewer.Session)}, graphstore.StateGone,
		lastHeldQueryTypes, refs.IssueDepthCap)
	if err != nil {
		return fmt.Errorf("read the versions that last held reference targets: %w", err)
	}
	defer rows.Close()
	found := make(map[string]string, len(candidates))
	for rows.Next() {
		var address, issue, project, slug, block, query, opening string
		var version int
		if err := rows.Scan(&address, &issue, &project, &slug, &block, &version, &query, &opening); err != nil {
			return fmt.Errorf("scan the version that last held a reference target: %w", err)
		}
		if _, already := found[address]; already {
			continue
		}
		if query != "" && !slices.Contains(lastHeldQueryTypes[query], refs.TreeNodeType(refs.BlockNodeKind, "", opening)) {
			continue
		}
		ref := text.Ref{Kind: "block", IssueKey: issue, ID: slug, Block: block, Pin: version}
		if issue == "" {
			ref.Project = project
		}
		if held, ok := text.FormatBlockRef(ref); ok {
			if issue != "" && slug == "spec" && query == "" {
				// A non-query block of spec.md opens the artifact, not the Spec filter.
				held = "dispatch://" + issue + "/artifact/spec#b-" + text.EncodeBlockID(block) + "@v" + strconv.Itoa(version)
			}
			found[address] = held
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the versions that last held reference targets: %w", err)
	}
	for i, address := range candidates {
		answers[positions[i]].LastHeld = found[address]
	}
	return nil
}
