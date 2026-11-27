package refs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// blockParentDifference is one block whose part_of in the store is not what the render gives:
// where the block sits in the arrays the read was handed, the far ends the store still asserts
// and the render does not give, and whether the render's own parent is among the far ends it
// asserts. A block the store already places where the render does answers no difference at all.
type blockParentDifference struct {
	block   int
	retract []blockFarEnd
	holds   bool
}

// heldBlockParentDifferences is each block of a document whose parent in the store is not the
// one the render gives, read the way the store reads a current relation - the newest tuple per
// far end, kept while it asserts (graphstore.CurrentTupleRead) - but in one statement for every
// block of the document rather than one per block, which on a document of thousands of blocks
// would be thousands of round trips. The lateral is keyed on the block's address and part_of: it
// reads through graph_tuples_current under a custom plan, where the bound kind proves that
// index's predicate, and through graph_tuples_as_of_out under a generic plan, which sees no kind.
//
// blockIDs are the blocks that are nodes of the store, in document order, and parentIDs the
// block each of them sits in, nil for a block the document itself holds. The statement builds
// both addresses from refKey, as the insert of document_blocks does, so the caller hands over
// one ref_key and not one address per block. askIDs, nil or as long as blockIDs, names the ask
// whose node a reading in an answered ask takes for its parent in place of the block holding it
// (planBlockParents' askParents), nil for every other block.
//
// The comparison is the statement's: a block answers a row only where the store asserts a far
// end the render does not give, or asserts none the render does, so an edit that moved nothing
// reads back no row and a document of thousands of blocks costs its probe and nothing more. The
// having clause is that rule, and the two array_aggs carry the far ends the pass retracts.
func heldBlockParentDifferences(
	ctx context.Context,
	q Queryer,
	refKey string,
	blockIDs []string,
	parentIDs []*string,
	askIDs []*string,
) ([]blockParentDifference, error) {
	rows, err := q.Query(ctx, `
		select block.ordinality,
		       array_agg(current.to_kind) filter (where not current.wanted),
		       array_agg(current.to_id) filter (where not current.wanted),
		       coalesce(bool_or(current.wanted), false)
		from unnest($1::text[], $2::text[], $8::text[]) with ordinality as block (id, parent, ask)
		left join lateral (
			select held.to_kind, held.to_id,
			       held.to_kind = case when block.ask is not null then $9 when block.parent is null then $6 else $3 end
			       and held.to_id = case when block.ask is not null then block.ask
			                             when block.parent is null then $7 else $7 || '#' || block.parent end
			         as wanted
			from (
				`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = $3 and t.from_id = $7 || '#' || block.id and t.kind = $4
				  and t.to_kind <> $5`)+`
			) held
			where held.asserted
		) current on true
		group by block.ordinality
		having not coalesce(bool_or(current.wanted), false)
		    or count(*) filter (where not current.wanted) > 0
		order by block.ordinality
	`, blockIDs, parentIDs, BlockNodeKind, graphstore.TuplePartOf, graphstore.KindValue, "artifact", refKey, askIDs, graphstore.KindAsk)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var differences []blockParentDifference
	for rows.Next() {
		var ordinality int64
		var kinds, ids []string
		var holds bool
		if err := rows.Scan(&ordinality, &kinds, &ids, &holds); err != nil {
			return nil, err
		}
		difference := blockParentDifference{block: int(ordinality) - 1, holds: holds}
		for index, kind := range kinds {
			difference.retract = append(difference.retract, blockFarEnd{kind: kind, id: ids[index]})
		}
		differences = append(differences, difference)
	}
	return differences, rows.Err()
}

// heldLink is one link a block node states beside its membership of its document and its
// placement: a mention's or a pointer's, under the kind its pair names, to its far end.
// A reading's authored_by (PlanDecisionNodes), an ask block copy's copied_from (docs'
// claimCopies) and a block's mention of a person, an agent's session or a role (IdentityKinds,
// blockIdentityMentions) are no link: no block_refs row projects any of them, so no pair of a
// version writes them and the reconcile of the links retracts none of them. A copy's copied_from
// is retracted where its block goes instead, by the pass that states the block gone
// (heldCopiedFrom). The block index retracts an identity mention only where the version's text of
// its block no longer names it (unnamedMentions), and the next index whose text names it again
// states it again (identityMentions). A person's goes through graphstore.ToPerson alone, which
// refuses it in any other write.
type heldLink struct{ kind, toKind, toID string }

// heldCopiedFrom is, for each of ids, blocks of the document refKey names that a version no longer
// holds, the asks its node states it was copied from now (graphstore.TupleCopiedFrom, read the way
// the store reads a current relation, graphstore.CurrentTupleRead), by block id, in far-end order.
// A block whose node states none, or that is no node of the store, is absent. The index pass reads
// it for the blocks it states gone (planBlockNodes), so the write that drops a copy - an agent's
// edit, an upload, a settlement's version - ends the relation in the same write and under the same
// event: no later write reads that block's row, which the drop deletes, to end it then. One probe a
// block, through graph_tuples_current, whose predicate the kind written into the text proves.
func heldCopiedFrom(ctx context.Context, q Queryer, refKey string, ids []string) (map[string][]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select gone.id, copied.to_id
		from unnest($2::text[]) as gone (id)
		cross join lateral (`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
		`t.from_kind = '`+BlockNodeKind+`' and t.from_id = $1 || '#' || gone.id
			  and t.kind = '`+graphstore.TupleCopiedFrom+`' and t.to_kind = '`+graphstore.KindAsk+`'`)+`
		) copied
		where copied.asserted
		order by gone.id, copied.to_id
	`, refKey, ids)
	if err != nil {
		return nil, fmt.Errorf("read the copies the blocks of %s a version drops hold: %w", refKey, err)
	}
	defer rows.Close()
	held := map[string][]string{}
	for rows.Next() {
		var id, ask string
		if err := rows.Scan(&id, &ask); err != nil {
			return nil, fmt.Errorf("scan a copy a block of %s a version drops holds: %w", refKey, err)
		}
		held[id] = append(held[id], ask)
	}
	return held, rows.Err()
}

// heldDocumentBlocks is what the store states now of the blocks of the document refKey names, by
// block id: each block node that states itself a member of the document (an in_document tuple
// into the document's artifact address) and that the store holds in any state but gone, with
// every link it states now beside that membership and its placement, read the way the store reads
// a current relation (graphstore.CurrentTupleRead), in kind and far-end order. A member that
// states no link is in the map with none, and one whose address does not start with the
// document's ref_key is no block the pass addresses and is left out.
//
// The state read is every state but gone, not live alone: a later release marks a block
// proposed, settled or overtaken, and the index pass keeps such a state unless the pass itself
// makes the block gone (blockNode). A block in one of those states that another release's
// version dropped is as gone as a live one, and a read of the live blocks alone would leave it
// standing in that state for ever, with nothing projecting it.
//
// It is the store's own side of the document, and IndexBlocks reads it only where that side is
// stale (blockStand.stale): another release indexed the document after the store last stated its
// blocks, and that release deleted the rows of the blocks and the links its version dropped and
// retracted nothing, so no row is left to say what the store still holds. The pass's input stays
// the version's render: this read is what the store would otherwise go on asserting against it,
// as heldBlockParentDifferences is for the parents. The members come through
// graph_tuples_as_of_in in one probe and each member's links through graph_tuples_as_of_out in
// one probe a block, in one statement: the links are every kind but two, which proves nothing of
// graph_tuples_current's predicate, and that index leaves delivered_to out. The kinds and the
// value kind are written into the text so the planner can prove each index's partial predicate
// whatever plan it caches.
//
// Each member probes its own node through graph_nodes' primary key, in a subquery behind an
// offset 0 the planner cannot flatten. Joined plainly, the planner took `kind = 'block'` alone
// for the index's condition wherever the table's statistics counted few blocks, and read every
// block node again for each member, its id left to a join filter: on a document of 1,000 blocks
// that removed 499,500 rows, and a document of 150,000 blocks is the read this pass makes
// (TestTheHeldDocumentReadProbesEachMembersNodeOnce).
func heldDocumentBlocks(ctx context.Context, q Queryer, refKey string) (map[string][]heldLink, error) {
	rows, err := q.Query(ctx, `
		select member.id, held.kind, held.to_kind, held.to_id
		from (
			select distinct t.from_id as id from graph_tuples t
			where t.to_kind = 'artifact' and t.to_id = $1
			  and t.kind = '`+TupleInDocument+`' and t.from_kind = '`+BlockNodeKind+`'
		) member
		cross join lateral (
			select n.state from graph_nodes n
			where n.kind = '`+BlockNodeKind+`' and n.id = member.id
			offset 0
		) node
		left join lateral (
			select current.kind, current.to_kind, current.to_id
			from (
				`+graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = '`+BlockNodeKind+`' and t.from_id = member.id
				  and t.to_kind <> '`+graphstore.KindValue+`'
				  and t.kind not in ('`+TupleInDocument+`', '`+graphstore.TuplePartOf+`',
				                     '`+graphstore.TupleAuthoredBy+`', '`+graphstore.TupleCopiedFrom+`')
				  and not (t.kind = '`+graphstore.TupleMentions+`'
				           and t.to_kind in ('`+strings.Join(IdentityKinds, `', '`)+`'))`)+`
			) current
			where current.asserted
		) held on true
		where node.state <> '`+graphstore.StateGone+`'
		order by member.id, held.kind, held.to_kind, held.to_id
	`, refKey)
	if err != nil {
		return nil, fmt.Errorf("read what the store states of the blocks of %s: %w", refKey, err)
	}
	defer rows.Close()
	prefix := refKey + "#"
	held := map[string][]heldLink{}
	for rows.Next() {
		var address string
		var kind, toKind, toID *string
		if err := rows.Scan(&address, &kind, &toKind, &toID); err != nil {
			return nil, fmt.Errorf("scan what the store states of a block of %s: %w", refKey, err)
		}
		id, ours := strings.CutPrefix(address, prefix)
		if !ours {
			continue
		}
		links := held[id]
		if kind != nil {
			links = append(links, heldLink{kind: *kind, toKind: *toKind, toID: *toID})
		}
		held[id] = links
	}
	return held, rows.Err()
}

// goneSinceStated is, in id order, each block the store holds in the document in any state but
// gone (held) that the version holds no node of (nodes, its unique blocks) and that the rows did
// not hold either (deleted, the blocks of the rows the version dropped, which IndexBlocks states
// gone itself): a block another release's version dropped after the store last stated the
// document. That release deleted the block's row and stated nothing, so its node stands, and it is
// gone exactly as a block the rows dropped is, by the version that does not hold it.
func goneSinceStated(held map[string][]heldLink, nodes []model.ArtifactBlock, deleted []string) []string {
	if len(held) == 0 {
		return nil
	}
	holds := make(map[string]struct{}, len(nodes)+len(deleted))
	for _, block := range nodes {
		holds[block.ID] = struct{}{}
	}
	for _, id := range deleted {
		holds[id] = struct{}{}
	}
	var gone []string
	for id := range held {
		if _, kept := holds[id]; !kept {
			gone = append(gone, id)
		}
	}
	slices.Sort(gone)
	return gone
}

// unheldLinks retracts each link the store states now out of a block of the document (held) that
// no pair of the version writes, at the version's time and by its writer, as planBlockRefs
// retracts the pair of a row the pass drops: a link another release's version dropped after the
// store last stated the document, deleting its block_refs row and retracting nothing. The links
// of a block goneSinceStated names are among them, as a dropped block's rows are among the rows
// queueBlockRefs drops. stated is the tuples planBlockRefs answered, and a link it already
// retracts, the pair of a row the pass dropped, is not retracted twice. The retractions come in
// block id order and in the read's order within a block, so a reader of the log can predict them.
func unheldLinks(held map[string][]heldLink, refKey string, pairs []blockPair, stated []graphstore.UncausedTuple, at time.Time, writer model.Actor) []graphstore.UncausedTuple {
	if len(held) == 0 {
		return nil
	}
	type blockLink struct {
		blockID string
		link    heldLink
	}
	kept := make(map[blockLink]struct{}, len(pairs)+len(stated))
	for _, pair := range pairs {
		kept[blockLink{pair.blockID, heldLink{pair.kind, pair.target.Kind, pair.target.ID}}] = struct{}{}
	}
	prefix := refKey + "#"
	for _, tuple := range stated {
		if id, ours := strings.CutPrefix(tuple.FromID, prefix); ours && !tuple.Asserted {
			kept[blockLink{id, heldLink{tuple.Kind, tuple.ToKind, tuple.ToID}}] = struct{}{}
		}
	}
	var retracted []graphstore.UncausedTuple
	for _, id := range slices.Sorted(maps.Keys(held)) {
		for _, link := range held[id] {
			if _, ok := kept[blockLink{id, link}]; ok {
				continue
			}
			retracted = append(retracted, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
				FromKind: BlockNodeKind, FromID: blockNodeAddress(refKey, id), Kind: link.kind,
				ToKind: link.toKind, ToID: link.toID,
				At: at, Who: writer, Asserted: false,
			}})
		}
	}
	return retracted
}
