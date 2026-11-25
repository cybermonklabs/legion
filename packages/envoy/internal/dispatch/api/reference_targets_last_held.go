package api

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// lastHeldParentAtCut selects the standing placement by the same order as refs.TreeSubtrees:
// the newest assertion or retraction of each far end, then the newest asserted parent.
// The caller has joined the recorded query cut as cut.
func lastHeldParentAtCut(kind, id string) string {
	return `select p.to_kind, p.to_id from (
		select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
		from graph_tuples t
		where t.from_kind = ` + kind + ` and t.from_id = ` + id + ` and t.kind = 'part_of'
		  and t.to_kind <> 'value' and t.seq <= cut.read_seq
		order by t.to_kind, t.to_id, t.at desc, t.seq desc
	) p where p.asserted order by p.at desc, p.seq desc limit 1`
}

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
// answer its memo, but not a requirement or criterion of its own.
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
		join graph_nodes held on held.kind = 'block' and held.id = addressed.node and held.state = $4
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
		       coalesce((v.converted = 'born' or v.converted = 't19b' and v.view_marked)
		         and v.own_spec_at is null, false) as viewable,
		       chain.depth > 0 and exists (
		         select 1 from artifacts doc join document_blocks d on d.artifact_id = doc.id
		         where doc.issue_key = a.key and d.type = 'requirement'
		       ) as has_requirements,
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
	       opening.text as opening, (block.supertag <> 'callout' or block.node = memo.from_id) as taken
	from query_blocks block
	join version_renders cut on cut.artifact_id = block.query_id
	  and cut.read_seq is not null and cut.at is not null and not cut.tree_mismatch
	join artifact_versions version on version.artifact_id = cut.artifact_id and version.number = cut.number
	cross join lateral (
		select case when t.asserted then split_part(t.to_id, chr(10), 1) end as text
		from graph_tuples t where t.from_kind = 'block' and t.from_id = block.node
		  and t.kind = '` + graphstore.AttributeText + `' and t.to_kind = 'value' and t.seq <= cut.read_seq
		order by t.at desc, t.seq desc limit 1
	) opening
	left join lateral (
		select first_child.from_id
		from (
			select distinct on (t.from_kind, t.from_id) t.from_kind, t.from_id, t.asserted
			from graph_tuples t
			where t.to_kind = 'issue' and t.to_id = block.issue_key and t.kind = 'part_of' and t.seq <= cut.read_seq
			order by t.from_kind, t.from_id, t.at desc, t.seq desc
		) first_child
		cross join lateral (` + lastHeldParentAtCut("first_child.from_kind", "first_child.from_id") + `) parent
		cross join lateral (
			select case when t.asserted then t.to_id end as rank
			from graph_tuples t where t.from_kind = first_child.from_kind and t.from_id = first_child.from_id
			  and t.kind = '` + refs.AttributeRank + `' and t.to_kind = 'value' and t.seq <= cut.read_seq
			order by t.at desc, t.seq desc limit 1
		) rank
		where block.supertag = 'callout' and first_child.asserted and rank.rank is not null
		  and parent.to_kind = 'issue' and parent.to_id = block.issue_key
		  and first_child.from_kind not in ('comment', 'issue', 'step', 'plan', 'handoff')
		  and (first_child.from_kind not in ('artifact', 'version', 'block')
		    or split_part(first_child.from_id, '/', 1) = block.issue_key
		    or ` + graphstore.FindingIDSQL("first_child.from_id") + `)
		  and (select case when t.asserted then t.to_id end from graph_tuples t
		       where t.from_kind = first_child.from_kind and t.from_id = first_child.from_id
		         and t.kind = '` + graphstore.AttributeState + `' and t.to_kind = 'value' and t.seq <= cut.read_seq
		       order by t.at desc, t.seq desc limit 1) is distinct from $4
		order by rank.rank collate "C", (first_child.from_kind || ':' || first_child.from_id) collate "C"
		limit 1
	) memo on true
	where opening.text is not null and exists (
		with recursive placed(kind, id, child_kind, child_id) as (
			select 'block'::text, block.node, null::text, null::text
			union
			select parent.to_kind, parent.to_id, child.kind, child.id
			from placed child
			cross join lateral (` + lastHeldParentAtCut("child.kind", "child.id") + `) parent
			-- The part_of log alone does not prove that a comment or message's thread
			-- passed the conversation filter at this cut. Do not use it as a root proof.
			where child.kind not in ('issue', 'comment', 'comment_block', 'message', 'message_block')
			  and (child.kind not in ('artifact', 'version', 'block') or split_part(child.id, '/', 1) = block.issue_key
			    or ` + graphstore.FindingIDSQL("child.id") + `)
			  and (select case when t.asserted then t.to_id end from graph_tuples t
			       where t.from_kind = child.kind and t.from_id = child.id and t.kind = '` + graphstore.AttributeState + `'
			         and t.to_kind = 'value' and t.seq <= cut.read_seq
			       order by t.at desc, t.seq desc limit 1) is distinct from $4
		)
		select 1 from placed where kind = 'issue' and id = block.issue_key
		union all
		-- A block placed directly under an ancestor's block is also a root of the
		-- second half of WalkIssueTree (TreePlacedAbove). Prove its membership at the cut.
		select 1 from placed foreign_parent
		join spec_chain ancestor on ancestor.subject = block.issue_key and ancestor.depth > 0
		  and ancestor.key = split_part(foreign_parent.id, '/', 1)
		where foreign_parent.kind = 'block' and foreign_parent.child_kind = 'block'
		  and exists (
		      select 1 from artifacts d
		      cross join lateral (` + lastHeldParentAtCut("'artifact'", "d.ref_key") + `) top
		      where d.issue_key = block.issue_key and top.to_kind = 'issue' and top.to_id = block.issue_key
		        and (select t.asserted from graph_tuples t
		             where t.from_kind = 'block' and t.from_id = foreign_parent.child_id
		               and t.kind = 'in_document' and t.to_kind = 'artifact' and t.to_id = d.ref_key
		               and t.seq <= cut.read_seq order by t.at desc, t.seq desc limit 1)
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
	rows, err := s.deps.Store.Pool.Query(ctx, lastHeldBlocksStatement, candidates, refs.VisibilityArg(who),
		map[string]any{"sees": viewer.Sees, "all": viewer.All, "open": viewer.Open, "private": viewer.AnyPrivate,
			"person": viewer.Person, "session": viewer.Session}, graphstore.StateGone, lastHeldQueryTypes,
		refs.IssueDepthCap)
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
