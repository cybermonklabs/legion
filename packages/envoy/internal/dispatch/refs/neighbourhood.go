package refs

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// IssueNeighbours is the graph neighbourhood of the issue issueKey (R24 of the issue-tree spec):
// the closest existing issues of its project by the graph targets they share with it, for who,
// at most limit of them, in rank order.
//
// The issue's graph targets are the far ends of its own child_of, needs to an issue, filed_from
// and affects tuples, and of the mentions tuples whose source is its spec, the spec's node or one
// of its current blocks (documentBlockIDs, which reads a spec edited in its issue's room in that
// room). A component counts only by the issue's own affects tuple: the graph holds no tuple of a
// component a child inherits from its parent, and an inherited one would make every child of a
// parent share it. A target that is the issue itself, one who may not see and one whose node is
// gone count for nothing.
//
// A candidate is an issue of the same project, not the issue itself, that reaches one of those
// targets by any of the five relations, the same one or another: its own child_of, needs,
// filed_from or affects tuple, or a mentions tuple out of its spec's node or a block of its spec.
// The read leaves out every issue the issue already links: its parent, each blocker, the issue it
// is a duplicate of, each issue its spec mentions, and the issue that holds its filing origin. It
// counts the targets each candidate shares and orders the candidates open before done, then by
// that count, highest first, then by the newest issues.updated_at, then by key. Each answers
// its key, status and title, the reference of each target it shares, in the order of the targets'
// kinds and ids, and its link.
//
// The read is one statement (issueNeighboursStatement), under the visibility predicate of each
// candidate issue and each target. Each target's inbound probe binds both keys of
// graph_tuples_as_of_in, the index R20 names for a read into a node.
func IssueNeighbours(ctx context.Context, q Queryer, who model.Actor, issueKey string, limit int) ([]model.IssueNeighbour, error) {
	neighbours := []model.IssueNeighbour{}
	if issueKey == "" || limit <= 0 {
		return neighbours, nil
	}
	rows, err := q.Query(ctx, issueNeighboursStatement(), issueKey, limit, VisibilityArg(who))
	if err != nil {
		return nil, fmt.Errorf("read the graph neighbourhood of %s: %w", issueKey, err)
	}
	defer rows.Close()
	for rows.Next() {
		var neighbour model.IssueNeighbour
		var targets []neighbourTarget
		if err := rows.Scan(&neighbour.Key, &neighbour.Status, &neighbour.Title, &targets); err != nil {
			return nil, fmt.Errorf("scan a graph neighbour of %s: %w", issueKey, err)
		}
		neighbour.Shares = make([]string, len(targets))
		for index, target := range targets {
			neighbour.Shares[index] = target.reference()
		}
		neighbour.Href = "/issues/" + neighbour.Key
		neighbours = append(neighbours, neighbour)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the graph neighbourhood of %s: %w", issueKey, err)
	}
	return neighbours, nil
}

// neighbourTarget carries the address and the owner facts needed to format it. The statement
// reads all of them on the same snapshot as the tuples, without loading the targets' text.
type neighbourTarget struct {
	Kind, ID, Item, ItemID, Project, Slug, DocKind, Block string
	Issue, Session, ItemIssue                             *string
	Primary                                               bool
}

func (target neighbourTarget) reference() string {
	switch {
	case target.Item != "":
		ref := itemRef(target.Item, target.ItemIssue, target.Project, target.Slug, target.ItemID)
		if ref != "" {
			if target.Block != "" {
				return BlockRef(ref, target.Block)
			}
			return ref
		}
	case target.Slug != "":
		ref := ArtifactRef(target.Issue, target.Project, target.Session, target.Slug, target.DocKind, target.Primary)
		if target.Block != "" {
			return BlockRef(ref, target.Block)
		}
		return ref
	case target.Kind == graphstore.KindIssue:
		return "dispatch://" + target.ID
	case target.Kind == graphstore.KindComponent:
		if project, id, ok := strings.Cut(target.ID, "/"); ok {
			return componentRef(project, id)
		}
	case target.Kind == graphstore.KindLensBlock || graphstore.AddressKind(target.Kind, target.ID) == graphstore.KindLensBlock:
		return StoreRef(graphstore.KindLensBlock, target.ID)
	case target.Kind == graphstore.KindLens || target.Kind == graphstore.KindRisk ||
		target.Kind == graphstore.KindConstraint || target.Kind == graphstore.KindInteraction ||
		target.Kind == graphstore.KindLensKind:
		return StoreRef(target.Kind, target.ID)
	}
	return target.Kind + " " + target.ID
}

// neighbourRelations are the five relations of R24 a candidate reaches a target by, and
// neighbourOwnRelations the four of them an issue states out of its own node; an issue's
// duplicate_of names an issue it links, which is left out, and no target.
var (
	neighbourRelations = store.QuoteLiterals([]string{
		graphstore.TupleChildOf, graphstore.TupleNeeds, graphstore.TupleFiledFrom, graphstore.TupleAffects, graphstore.TupleMentions,
	})
	neighbourOwnRelations = store.QuoteLiterals([]string{
		graphstore.TupleChildOf, graphstore.TupleNeeds, graphstore.TupleDuplicateOf, graphstore.TupleFiledFrom, graphstore.TupleAffects,
	})
)

// issueNeighboursStatement is IssueNeighbours' one statement, $1 the issue's key, $2 the limit
// and $3 the acting credential (VisibilityArg), each row a neighbour's key, status and title and
// the addresses of the targets it shares, in rank order. It is built once, at its first
// read, because the predicates it binds are rendered from the registry, which init fills.
//
// Each relation is read as it stands now (graphstore.CurrentTupleRead): out of the issue's node
// and out of each source of its spec's mentions through graph_tuples_as_of_out, and into each
// target through graph_tuples_as_of_in, each probe binding the index's predicate, to_kind <>
// 'value', as a literal. A candidate is found from the near end of a tuple into a target: an issue
// by its key, a spec's node by its artifacts row, a block of a spec by its document_blocks row and
// the document that row's block is of (BlockDocumentSQL), so a block of an issue room names the
// spec whose fragment holds it.
var issueNeighboursStatement = sync.OnceValue(func() string {
	const (
		issue    = `'` + graphstore.KindIssue + `'`
		value    = `'` + graphstore.KindValue + `'`
		mentions = `'` + graphstore.TupleMentions + `'`
	)
	return `
	with subject as (
		select i.key, i.project_key from issues i where i.key = $1
	), held as (
		select own.kind, own.to_kind, own.to_id from (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = `+issue+` and t.from_id = $1 and t.to_kind <> `+value+`
			  and t.kind in (`+neighbourOwnRelations+`)`) + `
		) own where own.asserted
	), spoken (kind, id) as (
		select '` + graphstore.KindArtifact + `'::text, spec.ref_key from artifacts spec where spec.issue_key = $1 and spec.is_primary
		union all
		select '` + BlockNodeKind + `'::text, spec_block.node_id
		from artifacts spec cross join lateral ` + documentBlockIDs(`spec.ref_key`) + ` spec_block (node_id)
		where spec.issue_key = $1 and spec.is_primary
	), mentioned as (
		select said.to_kind, said.to_id from spoken origin_node cross join lateral (
			` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = origin_node.kind and t.from_id = origin_node.id and t.kind = `+mentions+` and t.to_kind <> `+value) + `
		) said where said.asserted
	), targets as (
		select held.to_kind, held.to_id from held
		where held.kind in (` + store.QuoteLiterals([]string{graphstore.TupleChildOf, graphstore.TupleFiledFrom, graphstore.TupleAffects}) + `)
		   or (held.kind = '` + graphstore.TupleNeeds + `' and held.to_kind = ` + issue + `)
		union
		select mentioned.to_kind, mentioned.to_id from mentioned
	), seen_targets as (
		select target.to_kind, target.to_id from targets target
		left join graph_nodes target_node on target_node.kind = ` + StoredKindSQL("target.to_kind") + ` and target_node.id = target.to_id
		where not (target.to_kind = ` + issue + ` and target.to_id = $1)
		  and coalesce(target_node.state, '` + graphstore.StateLive + `') <> '` + graphstore.StateGone + `'
		  and ` + VisibleNodeOfRow(`target.to_kind`, `target.to_id`, `target_node`, `$3`) + `
	), reached as (
		select target.to_kind, target.to_id, held_in.kind, held_in.from_kind, held_in.from_id
		from seen_targets target cross join lateral (
			` + graphstore.CurrentTupleRead(`t.kind, t.from_kind, t.from_id`, `t.kind, t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = target.to_kind and t.to_id = target.to_id and t.to_kind <> `+value+`
			  and t.kind in (`+neighbourRelations+`)`) + `
		) held_in where held_in.asserted
	), shared as (
		select distinct reach.to_kind, reach.to_id, owner.key
		from reached reach cross join lateral (
			select reach.from_id as key
			where reach.from_kind = ` + issue + ` and reach.kind <> ` + mentions + `
			union all
			select doc.issue_key from artifacts doc
			where reach.kind = ` + mentions + ` and reach.from_kind = '` + graphstore.KindArtifact + `'
			  and doc.ref_key = reach.from_id and doc.is_primary and doc.issue_key is not null
			union all
			select doc.issue_key from document_blocks held_block join artifacts doc on doc.id = ` + BlockDocumentSQL(`held_block`) + `
			where reach.kind = ` + mentions + ` and reach.from_kind = '` + BlockNodeKind + `'
			  and held_block.node_id = reach.from_id and doc.is_primary and doc.issue_key is not null
		) owner
	), linked (key) as (
		select held.to_id from held
		where held.to_kind = ` + issue + ` and held.kind in (` + store.QuoteLiterals([]string{graphstore.TupleChildOf, graphstore.TupleNeeds, graphstore.TupleDuplicateOf}) + `)
		union
		select mentioned.to_id from mentioned where mentioned.to_kind = ` + issue + `
		union
		select origin.key from held cross join lateral (
			select coalesce(item.issue_key, doc.issue_key) as key
			from asks item left join artifacts doc on doc.id = item.artifact_id
			where held.to_kind = '` + graphstore.KindAsk + `' and item.id = ` + store.CastableUUID(`held.to_id`) + `
			union all
			select coalesce(item.issue_key, doc.issue_key)
			from comments item left join artifacts doc on doc.id = item.artifact_id
			where held.to_kind = '` + string(ItemComment) + `' and item.id = ` + store.CastableUUID(`held.to_id`) + `
			union all
			select item.issue_key from messages item
			where held.to_kind = '` + graphstore.KindMessage + `' and item.id = ` + store.CastableUUID(`held.to_id`) + `
		) origin
		where held.kind = '` + graphstore.TupleFiledFrom + `' and origin.key is not null
	), ranked as (
		select i.key, i.status, i.title, i.updated_at, count(*) as score
		from shared
		join issues i on i.key = shared.key
		join subject on subject.project_key = i.project_key
		where i.key <> $1
		  and not exists (select 1 from linked where linked.key = i.key)
		  and ` + VisibleKind(graphstore.KindIssue, `i.key`, `$3`) + `
		group by i.key, i.status, i.title, i.updated_at
		order by i.status = 'done', score desc, i.updated_at desc, i.key
		limit $2
	)
	select ranked.key, ranked.status, ranked.title, refs.targets
	from ranked cross join lateral (
		select jsonb_agg(` + neighbourTargetJSON + ` order by shared.to_kind, shared.to_id) as targets
		from shared ` + neighbourTargetJoins + `
		where shared.key = ranked.key
	) refs
	order by ranked.status = 'done', ranked.score desc, ranked.updated_at desc, ranked.key`
})

// The target's document (for an artifact, its block, or a conversation item on a project
// document) and issue owner (for a conversation item) determine its canonical address.
var neighbourTargetJoins = `
		left join lateral (
			select case when shared.to_kind in ('ask', 'comment', 'message') then shared.to_kind
			            when shared.to_kind in ('ask_block', 'comment_block', 'message_block')
			            then split_part(shared.to_id, '/', 1) end as kind,
			       case when shared.to_kind in ('ask', 'comment', 'message') then shared.to_id
			            when shared.to_kind in ('ask_block', 'comment_block', 'message_block')
			            then ` + itemOfBlockNodeSQL(`shared.to_id`) + ` end as id
		) item on true
		left join asks k on item.kind = 'ask' and k.id = ` + store.CastableUUID(`item.id`) + `
		left join comments c on item.kind = 'comment' and c.id = ` + store.CastableUUID(`item.id`) + `
		left join messages m on item.kind = 'message' and m.id = ` + store.CastableUUID(`item.id`) + `
		left join document_blocks b on shared.to_kind = 'block' and b.node_id = ` + ResolvedBlockSQL(`shared.to_id`) + `
		left join artifacts doc on doc.id = coalesce(k.artifact_id, c.artifact_id, ` + BlockDocumentSQL(`b`) + `,
			(select artifact.id from artifacts artifact where shared.to_kind = 'artifact' and artifact.ref_key = shared.to_id))`

const neighbourTargetJSON = `jsonb_build_object(
		'kind', shared.to_kind, 'id', shared.to_id, 'item', item.kind, 'itemID', item.id,
		'itemIssue', coalesce(k.issue_key, c.issue_key, m.issue_key, doc.issue_key),
		'block', coalesce(b.block_id, case when item.kind is not null then nullif(substr(shared.to_id, strpos(shared.to_id, '#') + 1), shared.to_id) end),
		'issue', doc.issue_key, 'project', doc.project_key, 'session', doc.session_id,
		'slug', doc.slug, 'docKind', doc.kind, 'primary', doc.is_primary)`
