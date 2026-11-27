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
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// blockParentDifference is one block whose part_of in the store is not what the render gives:
// where the block sits in the arrays the read was handed, the far ends the store still asserts
// and the render does not give, whether the render's own parent is among the far ends it
// asserts, and the placement the store holds of the block, "" where it holds none, which says
// whether a move of the node route placed it (PlacementStored). A block the store already places
// where the render does answers no difference at all.
type blockParentDifference struct {
	block     int
	retract   []blockFarEnd
	holds     bool
	placement string
}

// heldBlockParentDifferences is each block of a document whose parent in the store is not the
// one the render gives, read the way the store reads a current relation - the newest tuple per
// far end, kept while it asserts (graphstore.CurrentTupleRead) - but in one statement for every
// block of the document rather than one per block, which on a document of thousands of blocks
// would be thousands of round trips. The lateral is keyed on the block's address and part_of: it
// reads through graph_tuples_current under a custom plan, where the bound kind proves that
// index's predicate, and through graph_tuples_as_of_out under a generic plan, which sees no kind.
//
// The probe names the blocks that are nodes of the store, in document order (ids), and beside
// each the block it sits in (parents), nil for a block at the document's top level, whose parent
// is the node topKind and topID name (IndexedVersion.top), and the node it hangs under in place
// of either (endKinds and endIDs, nil or as long as ids: an answered ask's for a reading in it,
// and an authored node's own, placeAuthored), nil for every other block. The statement builds
// both block addresses from refKey, as the insert of document_blocks does, so the caller hands
// over one ref_key and not one address per block.
//
// The comparison is the statement's: a block answers a row only where the store asserts a far
// end the render does not give, or asserts none the render does, so an edit that moved nothing
// reads back no row and a document of thousands of blocks costs its probe and nothing more. The
// having clause is that rule, and the two array_aggs carry the far ends the pass retracts. The
// placement the store holds is read for those rows alone, as heldBlockValues reads a value, so a
// pass over a document nothing moved reads none. A difference names its block by its place in the
// probe. It reads through q, in the plan's read phase.
func heldBlockParentDifferences(ctx context.Context, q Queryer, refKey, topKind, topID string, probe parentProbe) ([]blockParentDifference, error) {
	if len(probe.ids) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select block.ordinality,
		       array_agg(current.to_kind) filter (where not current.wanted),
		       array_agg(current.to_id) filter (where not current.wanted),
		       coalesce(bool_or(current.wanted), false),
		       coalesce((
		           select case when t.asserted then t.to_id end from graph_tuples t
		           where t.from_kind = $3 and t.from_id = $7 || '#' || block.id and t.kind = $11
		             and t.to_kind = '`+graphstore.KindValue+`'
		           order by t.seq desc limit 1), '')
		from unnest($1::text[], $2::text[], $8::text[], $9::text[]) with ordinality as block (id, parent, end_kind, end_id)
		left join lateral (
			select held.to_kind, held.to_id,
			       held.to_kind = case when block.end_kind is not null then block.end_kind
			                           when block.parent is null then $6 else $3 end
			       and held.to_id = case when block.end_kind is not null then block.end_id
			                             when block.parent is null then $10 else $7 || '#' || block.parent end
			         as wanted
			from (
				`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = $3 and t.from_id = $7 || '#' || block.id and t.kind = $4
				  and t.to_kind <> $5`)+`
			) held
			where held.asserted
		) current on true
		group by block.ordinality, block.id
		having not coalesce(bool_or(current.wanted), false)
		    or count(*) filter (where not current.wanted) > 0
		order by block.ordinality
	`, probe.ids, probe.parents, BlockNodeKind, graphstore.TuplePartOf, graphstore.KindValue, topKind, refKey, probe.endKinds, probe.endIDs, topID,
		AttributePlacement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var differences []blockParentDifference
	for rows.Next() {
		var ordinality int64
		var kinds, ids []string
		var holds bool
		var placement string
		if err := rows.Scan(&ordinality, &kinds, &ids, &holds, &placement); err != nil {
			return nil, err
		}
		difference := blockParentDifference{block: int(ordinality) - 1, holds: holds, placement: placement}
		for index, kind := range kinds {
			difference.retract = append(difference.retract, blockFarEnd{kind: kind, id: ids[index]})
		}
		differences = append(differences, difference)
	}
	return differences, rows.Err()
}

// heldBlockPlacements reads the current AttributePlacement value of each block in ids from the
// store, in the same order as ids ("" where the store holds none). It uses the same index probe as
// heldBlockParentDifferences and runs only for blocks whose parent an earlier pass of the same
// transaction cached in BlockIndex.placed without populating BlockIndex.values.
func heldBlockPlacements(ctx context.Context, q Queryer, refKey string, ids []string) ([]string, error) {
	rows, err := q.Query(ctx, `
		select coalesce((
			select case when t.asserted then t.to_id end from graph_tuples t
			where t.from_kind = $2 and t.from_id = $3 || '#' || block.id and t.kind = $4
			  and t.to_kind = '`+graphstore.KindValue+`'
			order by t.seq desc limit 1), '')
		from unnest($1::text[]) with ordinality as block (id)
		order by block.ordinality
	`, ids, BlockNodeKind, refKey, AttributePlacement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	placements := make([]string, 0, len(ids))
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		placements = append(placements, value)
	}
	return placements, rows.Err()
}

// heldLink is one link a block node states beside its membership of its document and its
// placement, of a kind a pair of a version writes into block_refs (heldLinkKinds): a mention's or
// a pointer's, under the kind its pair names, to its far end, or a decision's tie. A tuple of any
// other kind out of a block is no link of the version's: a requirement's constrained_by
// (graphstore.TupleConstrainedBy, the pointer route's), a changed_by_run a write made for a run
// appended (graphstore.TupleChangedByRun), a decision's kept_by (the Keep's), a reading's
// authored_by (PlanDecisionNodes, TypedFacts.indexedFacts) and an ask block copy's copied_from
// (docs' claimCopies). No block_refs row projects any of them, so no pair of a version writes them
// and the reconcile of the links retracts none of them. A copy's copied_from is retracted where
// its block goes instead, by the pass that states the block gone (heldCopiedFrom), or, where the
// version's write deferred that pass, by a settlement's claim if one reads the block's row first.
type heldLink struct{ kind, toKind, toID string }

// heldLinkKinds is true of a tuple t of a kind a pair of a version writes into block_refs, the
// kinds whose links the release before this one drops by deleting their rows: a mention and a
// pointer of each `block[]` attribute the store's type nodes declare, which blockRefKinds, a
// text[] parameter bound to what BlockRefKinds read by a statement of its own in the caller's
// transaction, holds, and a decision's ties, its decided_by, from_ask and shaped (typedKinds,
// TypedFacts.pairs). It is the census's own reading of the tuples a block_refs row states
// (graphcensus's blockRefKinds and tieKinds).
func heldLinkKinds(blockRefKinds string) string {
	return `(t.kind = any(` + blockRefKinds + `) or t.kind in (` + store.QuoteLiterals(typedKinds) + `))`
}

// heldCopiedFrom is, for each of ids, blocks of the document refKey names that a version no longer
// holds, the asks its node states it was copied from now (graphstore.TupleCopiedFrom, read the way
// the store reads a current relation, graphstore.CurrentTupleRead), by block id, in far-end order.
// A block whose node states none, or that is no node of the store, is absent. The index pass reads
// it for the blocks it states gone (PlanBlocks), so a version indexed in its own write - an
// agent's edit, an upload, a settlement's version - ends the relation of a copy it drops in that
// write and under the same event: no later write reads that block's row, which the drop deletes,
// to end it then. A version whose write defers its block index (docs' indexDeferredVersion, which
// runs PlanDecisionNodes and no index pass) leaves the row and the relation standing, and the
// first settlement to read the row, whose claim retracts the relation (docs' claimCopies), or the
// block-index reconciliation's index pass ends it. One probe a block, through
// graph_tuples_current, whose predicate the kind written into the text proves.
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
// into the document's artifact address) and that the store holds present (at any state but gone,
// R12 of the node-visibility spec), with every link it states now of a kind a version's pairs write
// (heldLinkKinds) - a mention or a pointer (BlockRefKindsSQL), or a decision's or an ask block's
// tie (typedKinds) - read the way the store reads a current relation (graphstore.CurrentTupleRead),
// in kind and far-end order. A member that states no such link is in the map with none, and one
// whose address does not start with the document's ref_key is no block the pass addresses and is
// left out. A tuple of any other kind out of a block is no link the pass's pairs are compared
// with, since no block_refs row projects it: its membership and its placement, its values, and
// what another writer states of it - a requirement's constrained_by, a changed_by_run, a
// decision's or a reading's authored_by, a copy's copied_from. A gone block's pointers are
// retractGonePointers' to retract.
//
// The state read is every state but gone, not live alone: a write marks a block proposed,
// settled or overtaken, and the index pass keeps such a state unless the pass itself makes the
// block gone (blockNode). A block in one of those states that another release's version dropped
// is as gone as a live one, and a read of the live blocks alone would leave it standing in that
// state for ever, with nothing projecting it.
//
// It is the store's own side of the document, and IndexBlocks reads it only where that side is
// stale (blockStand.stale): another release indexed the document after the store last stated its
// blocks, and that release deleted the rows of the blocks and the links its version dropped and
// retracted nothing, so no row is left to say what the store still holds. The pass's input stays
// the version's render: this read is what the store would otherwise go on asserting against it,
// as heldBlockParentDifferences is for the parents. The members come through
// graph_tuples_as_of_in in one probe and each member's links through graph_tuples_as_of_out in
// one probe a block, in one statement: the links are the kinds a version's pairs write
// (heldLinkKinds), a list that proves nothing of graph_tuples_current's predicate, and that index
// leaves delivered_to out. The typed kinds and the value kind are written into the text, and the
// block-reference kinds, read first by a statement of their own in the caller's transaction
// (BlockRefKinds), are bound as one parameter, so the planner can prove each index's partial
// predicate whatever plan it caches, and the statement reads no attribute node of graph_nodes.
//
// Each member probes its own node through graph_nodes' primary key, in a subquery behind an
// offset 0 the planner cannot flatten. Joined plainly, the planner took `kind = 'block'` alone
// for the index's condition wherever the table's statistics counted few blocks, and read every
// block node again for each member, its id left to a join filter: on a document of 1,000 blocks
// that removed 499,500 rows, and a document of 150,000 blocks is the read this pass makes
// (TestTheHeldDocumentReadProbesEachMembersNodeOnce).
func heldDocumentBlocks(ctx context.Context, q Queryer, refKey string) (map[string][]heldLink, error) {
	blockRefKinds, err := BlockRefKinds(ctx, q)
	if err != nil {
		return nil, err
	}
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
				  and t.to_kind <> '`+graphstore.KindValue+`' and `+heldLinkKinds(`$2::text[]`))+`
			) current
			where current.asserted
		) held on true
		where node.state <> '`+graphstore.StateGone+`'
		order by member.id, held.kind, held.to_kind, held.to_id
	`, refKey, blockRefKinds)
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

// goneSinceStated is, in id order, each block the store holds present in the document (held) that
// the version holds no node of (nodes, its unique blocks) and that the rows did not hold either
// (deleted, the blocks of the rows the version dropped, which IndexBlocks states gone itself): a
// block another release's version dropped after the store last stated the document. That release
// deleted the block's row and stated nothing, so its node stands, and it is gone exactly as a
// block the rows dropped is, by the version that does not hold it.
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
