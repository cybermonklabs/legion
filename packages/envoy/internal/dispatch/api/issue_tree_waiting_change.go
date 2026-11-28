package api

import (
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A decision node of the tree read answers the Change it waits on (model.IssueTreeWaitingChange,
// INFRA-1196's decision 578), by the rule the issue page reads a Change of a decision node by
// (part T's decision-review.ts, nodeChangeComments and decisionNodeChanges): a comment that stands
// under the decision node in the tree, whose first author is a person, made after the decision's
// words last changed and after its last Keep, on a decision that is not replaced; of those the
// newest its author has not deleted. The read answers it off the store for every decision of the
// page in one statement (waitingChangesStatement), whatever page of the read's comments holds the
// comment, so the page learns it without reading the older comment pages. A comment counts only
// where the read's comment pages answer it: the walk reads the issue's conversation as the server
// sees the store, the empty actor its reader (docs' treeWalk.readConversation), so a comment that
// actor may not see is on no reader's page, and none waits.
//
// A comment stands under the node its part_of names at the read's cut, or under the nearest node
// above that one the read answers (keep): its part_of names the decision's block, or a block below
// it that no read answers as a node, which a Change written on the decision's words names - a block
// of the decision's own text (typedOwnText), or, in the spec, the Verification document and the
// plan, a block that is no typed block, their prose (excluded) - or a block below it the reader may
// not see. A block the read answers - a typed block under the decision, a reading - holds the
// comments under it, and none of them is the decision's.

// waitingValue is a lateral's body reading, at the read's cut (anchorContainerCut), the value of
// attribute out of the node of kind whose id the SQL id names, as the walk of the tree reads a
// node's value at a cut (anchorContainerValue): the newest tuple of that attribute by its time and
// then its seq, held where it asserts and null where it retracts, its first line alone where
// opening says so.
func waitingValue(kind, id, attribute string, opening bool) string {
	held := `t.to_id`
	if opening {
		held = `split_part(t.to_id, chr(10), 1)`
	}
	return `select case when t.asserted then ` + held + ` end as held
		from graph_tuples t
		where t.from_kind = '` + kind + `' and t.from_id = ` + id + ` and t.kind = '` + attribute + `'
		  and t.to_kind = '` + graphstore.KindValue + `' and ` + anchorContainerCut + `
		order by t.at desc, t.seq desc
		limit 1`
}

// waitingChildren is a lateral's body reading, at the read's cut, the nodes of kind whose part_of
// names the block climb.id: of each node's part_of tuples into it, read into the block through
// graph_tuples_as_of_in, the newest by its time and then its seq, answered as from_id and asserted;
// a caller keeps those that assert and whose newest standing part_of is that one
// (waitingPlacement), as the walk's children read does (refs' treeChildJoins). The kind is tested
// past an `offset 0`, on the tuples into the block of every kind, so no index keyed on the near
// end's kind reads them: given `t.from_kind = 'comment'` among its conditions, the planner read
// the comments under a decision of an issue of the largest plan through graph_tuples_current,
// whose key begins with the near end's kind and id, and so read the key of every relation tuple
// out of a comment of the store, 153 pages for each of the 4 blocks it climbed: 1.8 of the
// statement's 2.0 ms over the bound's corpus (LargestPlanFloor's diagnostic), where the tuples
// into each block are a few.
func waitingChildren(kind string) string {
	return `select inbound.from_id, inbound.asserted from (
			select distinct on (t.from_kind, t.from_id) t.from_kind, t.from_id, t.asserted
			from graph_tuples t
			where t.to_kind = '` + refs.BlockNodeKind + `' and t.to_id = climb.id and t.kind = '` + graphstore.TuplePartOf + `'
			  and ` + anchorContainerCut + `
			order by t.from_kind, t.from_id, t.at desc, t.seq desc
			offset 0) inbound
		where inbound.from_kind = '` + kind + `'`
}

// waitingPlacement is a lateral's body reading, at the read's cut, the parent of the node of kind
// whose id the SQL id names: of its part_of tuples, the newest to each far end kept while it
// asserts, and of those the newest, so a node stands under one parent at a time (refs'
// asOfPlacement).
func waitingPlacement(kind, id string) string {
	return `select held.to_kind, held.to_id from (
			select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
			from graph_tuples t
			where t.from_kind = '` + kind + `' and t.from_id = ` + id + ` and t.kind = '` + graphstore.TuplePartOf + `'
			  and t.to_kind <> '` + graphstore.KindValue + `' and ` + anchorContainerCut + `
			order by t.to_kind, t.to_id, t.at desc, t.seq desc
		) held
		where held.asserted
		order by held.at desc, held.seq desc
		limit 1`
}

// waitingChangesStatement reads, at the read's cut ($3 and $4, anchorContainerCut), for each
// decision block $1 names, beside the moment $2 names for it - the later of when its words last
// changed and of its last Keep, null where it holds neither - what places a Change under it: each
// block that stands below it, down its part_of children whose newest standing part_of names their
// parent, at most $6 levels (pmdoc.MaxTreeDepth), with its parent, its depth, its row's supertag
// and the first line of its text value, which say its type (refs.TreeNodeType), its placement
// value, the band of its document (refs.DocumentBandSQL), whether its state value is gone, and
// whether the reader $5 may see it, true where the store holds no row of it, as the registry reads
// such a node; and each comment whose newest standing part_of names the decision or one of those
// blocks, whose state is not gone, whose author is a person, that its author has not deleted, made
// after the decision's moment, that the credential $7 may see (the comment kind's predicate, bound
// as the walk of the conversation binds it: the empty actor), with when it was made. The rows of a
// decision come in depth order, its blocks before its comments.
var waitingChangesStatement = `
	with recursive climb (decision, since, id, parent, depth) as (
		select decided.id, decided.since, decided.id, ''::text, 0
		from unnest($1::text[], $2::timestamptz[]) as decided (id, since)
		union all
		select climb.decision, climb.since, child.from_id, climb.id, climb.depth + 1
		from climb
		cross join lateral (` + waitingChildren(refs.BlockNodeKind) + `) child
		cross join lateral (` + waitingPlacement(refs.BlockNodeKind, `child.from_id`) + `) placed
		where child.asserted and placed.to_kind = '` + refs.BlockNodeKind + `' and placed.to_id = climb.id and climb.depth < $6
	)
	select climb.decision, climb.id, climb.parent, climb.depth, false, coalesce(node.supertag, ''), coalesce(opening.held, ''),
	       coalesce(placement.held, ''), coalesce(document.band, ''), state.held is not distinct from '` + graphstore.StateGone + `',
	       coalesce(` + refs.VisibleKindRow(refs.BlockNodeKind, "node", "$5") + `, true), null::timestamptz
	from climb
	left join lateral (` + waitingValue(refs.BlockNodeKind, `climb.id`, graphstore.AttributeText, true) + `) opening on true
	left join lateral (` + waitingValue(refs.BlockNodeKind, `climb.id`, refs.AttributePlacement, false) + `) placement on true
	left join lateral (` + waitingValue(refs.BlockNodeKind, `climb.id`, graphstore.AttributeState, false) + `) state on true
	left join lateral (
		select ` + refs.DocumentBandSQL("a") + ` as band
		from document_blocks d join artifacts a on a.id = ` + refs.BlockDocumentSQL("d") + `
		where d.node_id = climb.id
		limit 1
	) document on true
	left join graph_nodes node on node.kind = '` + refs.BlockNodeKind + `' and node.id = climb.id
	where climb.depth > 0
	union all
	select climb.decision, held.from_id, climb.id, climb.depth, true, '', '', '', '', false, true, c.created_at
	from climb
	cross join lateral (` + waitingChildren(graphstore.KindComment) + `) held
	cross join lateral (` + waitingPlacement(graphstore.KindComment, `held.from_id`) + `) placed
	left join lateral (` + waitingValue(graphstore.KindComment, `held.from_id`, graphstore.AttributeState, false) + `) state on true
	join comments c on c.id = ` + store.CastableUUID(`held.from_id`) + `
	where held.asserted and placed.to_kind = '` + refs.BlockNodeKind + `' and placed.to_id = climb.id
	  and state.held is distinct from '` + graphstore.StateGone + `'
	  and coalesce(c.author->>'kind' = 'user', false) and c.deleted_at is null
	  and (climb.since is null or c.created_at > climb.since)
	  and ` + refs.VisibleKind(graphstore.KindComment, `held.from_id`, `$7`) + `
	order by 1, 5, 4`

// changeSince is the moment a Change of decision is made after: the later of when its words last
// changed (ChangedAt) and of its last Keep (Kept.KeptAt), nil where it holds neither, as the page's
// rule skips a moment the decision does not hold (decision-review.ts' after).
func changeSince(decision model.Decision) *time.Time {
	since := decision.ChangedAt
	if decision.Kept != nil && (since == nil || decision.Kept.KeptAt.After(*since)) {
		kept := decision.Kept.KeptAt
		since = &kept
	}
	return since
}

// waitingBlock is what the read holds of a block below a decision (waitingChangesStatement): whether
// it is a typed block, whether it is the decision's own text (typedOwnText: no typed block, placed
// by the text, under a typed block or under such text), and whether a comment under it stands under
// the decision, since neither it nor a block between it and the decision is a node the read
// answers.
type waitingBlock struct {
	typed, own, under bool
}

// readWaitingChanges reads, for each of decided that is a decision block and not replaced, the
// Change it waits on (waitingChangesStatement), into waiting by its node; a decision none waits on
// holds none there.
func (b *issueTreeBuilder) readWaitingChanges(decided []model.Decision) error {
	b.waiting = map[refs.Target]*model.IssueTreeWaitingChange{}
	var ids []string
	var since []*time.Time
	for _, decision := range decided {
		if decision.NodeKind == refs.BlockNodeKind && !decision.Replaced {
			ids = append(ids, decision.NodeID)
			since = append(since, changeSince(decision))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	seq, at := treeCutBounds(b.cut)
	rows, err := b.q.Query(b.ctx, waitingChangesStatement, ids, since, seq, at, refs.VisibilityArg(b.who), pmdoc.MaxTreeDepth,
		refs.VisibilityArg(model.Actor{}))
	if err != nil {
		return fmt.Errorf("read the Changes the decisions of the tree of %s wait on: %w", b.key, err)
	}
	defer rows.Close()
	blocks := map[[2]string]waitingBlock{}
	for rows.Next() {
		var decision, id, parent, supertag, opening, placement, band string
		var depth int
		var comment, gone, seen bool
		var madeAt *time.Time
		if err := rows.Scan(&decision, &id, &parent, &depth, &comment, &supertag, &opening, &placement, &band, &gone, &seen, &madeAt); err != nil {
			return fmt.Errorf("read a Change a decision of the tree of %s waits on: %w", b.key, err)
		}
		above := waitingBlock{typed: true, under: true}
		if parent != decision {
			above = blocks[[2]string{decision, parent}]
		}
		if !comment {
			typed := pmdoc.IsTypedBlock(refs.TreeNodeType(refs.BlockNodeKind, id, supertag, opening))
			own := !typed && placement == refs.PlacementText && (above.typed || above.own)
			prose := !typed && band != "" && (band[0] == refs.RankBandSpec || band[0] == refs.RankBandVerification || band[0] == refs.RankBandPlan)
			answered := seen && !own && !prose
			blocks[[2]string{decision, id}] = waitingBlock{typed: typed, own: own, under: above.under && !gone && !answered}
			continue
		}
		if !above.under || madeAt == nil {
			continue
		}
		target := refs.Target{Kind: refs.BlockNodeKind, ID: decision}
		// The newest Change, and of two made at once the one the tree's order takes first, the
		// lower id byte for byte, as the page keeps the first it walks and replaces it only with a
		// newer one.
		if held := b.waiting[target]; held == nil || madeAt.After(held.CreatedAt) || madeAt.Equal(held.CreatedAt) && id < held.CommentID {
			b.waiting[target] = &model.IssueTreeWaitingChange{CommentID: id, CreatedAt: *madeAt}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the Changes the decisions of the tree of %s wait on: %w", b.key, err)
	}
	return nil
}
