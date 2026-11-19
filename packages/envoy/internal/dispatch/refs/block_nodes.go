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

// blockNodeAddress is a block's address in the store, which document_blocks.node_id and
// block_refs.from_node hold and which the graph addresses a block by: the document's ref_key,
// '#' and the block's id.
func blockNodeAddress(refKey, blockID string) string { return refKey + "#" + blockID }

// MaxBlockNodeBytes bounds a block's address in the store, which is one address of the store:
// graph_nodes keys a node by (kind, id) in a B-tree, and graph_tuples_once, the repeat key of a
// relation, keys both of its ends at once, so a block's address and the far end of every tuple
// out of it have to fit the roughly 2,704 bytes a B-tree entry holds. A block's address is one
// of two ends, and its far end is not a second block address but a reference target of up to
// text.MaxTargetBytes (a mention's far end) or another node's address: the store bounds each end
// at graphstore.MaxAddressBytes, so two ends, the two kinds and the source_event and asserted the
// key also carries stay well under the entry bound. A block's address holds its document's
// ref_key, which nothing short of that key's own unique index bounds (migration 0121 hash-indexes
// document_blocks.node_id for the same reason), so a long-named document's blocks can pass the
// bound on their own.
//
// It is the store's one address bound and is defined there (graphstore.MaxAddressBytes): the
// census of the store against its projections and the backfill of the rows written before the
// store existed (internal/dispatch/graphcensus) read the same bound, and a block past it carries
// no node here, so a census holding such a row to one would report a difference this writer made
// on purpose.
const MaxBlockNodeBytes = graphstore.MaxAddressBytes

// blockIDFits is whether the address a block's id would take is inside that bound, read off
// the two lengths that make the address rather than off the address itself: a pass over a
// document's blocks asks this of every block and writes the address of the few it states
// something about, so building a string per block here would be a string per block thrown
// away.
func blockIDFits(refKey, blockID string) bool {
	return len(refKey)+len("#")+len(blockID) <= MaxBlockNodeBytes
}

// blockFits is whether each block's address fits the store's key, and how many do not.
func blockFits(refKey string, nodes []model.ArtifactBlock) ([]bool, int) {
	fits := make([]bool, len(nodes))
	past := 0
	for index, block := range nodes {
		fits[index] = blockIDFits(refKey, block.ID)
		if !fits[index] {
			past++
		}
	}
	return fits, past
}

// blockNode is one block's node as a version's index writes it: project-visible, in state, in the
// project of the document that holds it, made by the version's writer at its time, and with no
// cause yet - graphstore.Caused gives it the version's event. A node already there keeps its
// made_at and made_by, so a block is made by the version that first indexed it, and a block an
// earlier build indexed with no project takes its document's here (graphstore.Write fills a
// project the row does not hold).
//
// The index owns a block's text and its place in its document, not its state or its visibility,
// so every node it writes restates the block (graphstore.Node's Restates): state is live for a
// block the version holds and gone for one it dropped, and nothing more. A block already there
// keeps its visibility, and a block the version holds keeps any state a later release's write
// marked it with - a requirement marked proposed whose words a version changes stays proposed -
// while one stored gone, a block a version restores, comes back live. A block the version
// dropped goes gone whatever it was.
func blockNode(address string, record IndexedVersion, at time.Time, state string) graphstore.Node {
	return graphstore.Node{
		Kind: BlockNodeKind, ID: address, Supertag: BlockSupertag,
		Visibility: graphstore.VisibilityProject, State: state,
		Project: record.Project, Parties: record.Parties,
		MadeAt: at, MadeBy: record.Writer, SourceEvent: graphstore.NoEvent,
		Restates: true,
	}
}

// ReadIndexedVersion is what the version's own row and its document's row say of the pass
// (IndexedVersion), read through q: the document's ref_key, the first actor the version's row
// records as its writer, and the project the document belongs to or the session whose
// conversation holds it.
func ReadIndexedVersion(ctx context.Context, q Queryer, artifactID string, version int) (IndexedVersion, error) {
	var record IndexedVersion
	var project, session *string
	var authors []byte
	if err := q.QueryRow(ctx, `
		select a.ref_key, a.project_key, a.session_id, v.authors
		from artifacts a
		left join artifact_versions v on v.artifact_id = a.id and v.number = $2
		where a.id = $1::uuid
	`, artifactID, version).Scan(&record.RefKey, &project, &session, &authors); err != nil {
		return IndexedVersion{}, fmt.Errorf("read version %d of %s for its block index: %w", version, artifactID, err)
	}
	var projectKey, conversation string
	if project != nil {
		projectKey = *project
	}
	if session != nil {
		conversation = *session
	}
	record.Project, record.Parties = graphstore.SessionScope(projectKey, conversation)
	writers, err := storedActors(authors)
	if err != nil {
		return IndexedVersion{}, fmt.Errorf("decode the authors of version %d of %s: %w", version, artifactID, err)
	}
	if len(writers) > 0 {
		record.Writer = writers[0]
	}
	return record, nil
}

// blockFarEnd is one far end a block's part_of points at now.
type blockFarEnd struct{ kind, id string }

// planBlockParents plans the parent of every block of the version and its projection into
// document_blocks (queueBlockParents). It answers the tuples it states, which PlanBlocks appends
// to the pass's write as it appends the reference pass's, and where each block sits once the
// indexing is written, for a later indexing of the document in the same transaction (Held).
//
// It reads the parent each block holds in the store, over every block of the document and not
// the changed ones alone, so a block another block now holds, whose own text no version changed,
// gets its new parent here. It states a tuple only where the render disagrees with what the store
// asserts: one retraction for the parent that changed, one assertion for the new one. A re-index
// of a document nothing moved states nothing, which is what keeps a second pass from asserting a
// parent twice, since no code may update or delete a tuple row. A block an earlier indexing of
// this transaction placed (BlockIndex.placed) is compared with where that indexing put it, which
// the store holds only once the write phase has run.
//
// The comparison is the read's own (heldBlockParentDifferences): the parent the render gives
// travels with the probe, so a document nothing moved reads back no row at all rather than one
// row per block for Go to compare. Both statements name a block by its id and build its node
// address from the document's ref_key, as the insert of document_blocks does, so a pass over
// thousands of blocks holds no address for the thousands it states nothing about.
func (b *BlockIndexing) planBlockParents(
	ctx context.Context,
	q Queryer,
	index BlockIndex,
	nodes []model.ArtifactBlock,
	fits []bool,
	writer model.Actor,
) ([]graphstore.UncausedTuple, map[string]blockFarEnd, error) {
	placement := maps.Clone(index.placed)
	if len(nodes) == 0 {
		return nil, placement, nil
	}
	placed, parents := placeBlocks(nodes, fits)
	if len(placed) == 0 {
		return nil, placement, nil
	}
	if placement == nil {
		placement = make(map[string]blockFarEnd, len(placed))
	}
	refKey := b.refKey
	b.placedIDs = make([]string, len(placed))
	b.placedParents = make([]*string, len(placed))
	wanted := make([]blockFarEnd, len(placed))
	for position, block := range placed {
		b.placedIDs[position] = nodes[block].ID
		wanted[position] = blockFarEnd{kind: "artifact", id: refKey}
		if parents[position] >= 0 {
			b.placedParents[position] = &nodes[parents[position]].ID
			wanted[position] = blockFarEnd{kind: BlockNodeKind, id: blockNodeAddress(refKey, nodes[parents[position]].ID)}
		}
	}
	// Each placed block's difference, by its place among them: the ones an earlier indexing of
	// this transaction placed, compared here, and the rest read from the store in one probe.
	differences := make([]*blockParentDifference, len(placed))
	var readIDs []string
	var readParents []*string
	var readAt []int
	for position, id := range b.placedIDs {
		far, known := index.placed[id]
		if !known {
			readIDs, readParents, readAt = append(readIDs, id), append(readParents, b.placedParents[position]), append(readAt, position)
			continue
		}
		if far != wanted[position] {
			differences[position] = &blockParentDifference{block: position, retract: []blockFarEnd{far}}
		}
	}
	if len(readIDs) > 0 {
		read, err := heldBlockParentDifferences(ctx, q, refKey, readIDs, readParents)
		if err != nil {
			return nil, nil, fmt.Errorf("read the parents the blocks of %s hold: %w", b.artifactID, err)
		}
		for _, difference := range read {
			difference.block = readAt[difference.block]
			differences[difference.block] = &difference
		}
	}
	var tuples []graphstore.UncausedTuple
	for position, difference := range differences {
		placement[b.placedIDs[position]] = wanted[position]
		if difference == nil {
			continue
		}
		address := blockNodeAddress(refKey, b.placedIDs[position])
		for _, far := range difference.retract {
			tuples = append(tuples, parentTuple(address, far.kind, far.id, b.versionAt, writer, false))
		}
		if difference.holds {
			continue
		}
		tuples = append(tuples, parentTuple(address, wanted[position].kind, wanted[position].id, b.versionAt, writer, true))
	}
	return tuples, placement, nil
}

// queueBlockParents queues the projection of every placed block's parent into document_blocks.
// The projection is a statement of its own rather than a column of the insert of the blocks a
// version changed, so that insert keeps firing only for a block whose text changed: a block that
// only moved keeps its changed_version and changed_at. The statement carries the node columns
// beside the parent, so a row the insert did not touch learns the address of its node here.
func (b BlockIndexing) queueBlockParents(batch *store.WriteBatch) {
	if len(b.placedIDs) == 0 {
		return
	}
	batch.Queue(fmt.Sprintf("project the parents of the blocks of %s", b.artifactID), `
		update document_blocks d
		set node_kind = $3, node_address = $4 || '#' || p.block_id, parent_block_id = p.parent
		from unnest($2::text[], $5::text[]) as p (block_id, parent)
		where d.artifact_id = $1::uuid and d.block_id = p.block_id
		  and (d.parent_block_id is distinct from p.parent
		       or d.node_kind is distinct from $3
		       or d.node_address is distinct from $4 || '#' || p.block_id)
	`, b.artifactID, b.placedIDs, BlockNodeKind, b.refKey, b.placedParents)
}

// PlaceBlocks is where the block index places each block of a version of the document refKey
// names. It answers the version's unique blocks (the first block of each id), the index among
// them of each block it states a part_of for, in document order, and beside each of those the
// index among them of the block that holds it, -1 for a block the document holds. blocks are
// the version's rendered blocks, as IndexBlocks takes them.
//
// It is the one walk the index states each part_of from (planBlockParents) and the sizing
// counts the parents a version moved with (graphcensus), so what the sizing counts and what the
// writer appends cannot drift.
func PlaceBlocks(refKey string, blocks []model.ArtifactBlock) (nodes []model.ArtifactBlock, placed, parents []int) {
	nodes, _, _ = uniqueBlocks(blocks)
	fits, _ := blockFits(refKey, nodes)
	placed, parents = placeBlocks(nodes, fits)
	return nodes, placed, parents
}

// placeBlocks is PlaceBlocks over a version's unique blocks and whether each one's address fits
// the store's key. A block whose address is no key of the store is no node of it: nothing is
// stated about the block itself, and a block it holds is placed in the nearest block above it
// that is a node, or in the document.
func placeBlocks(nodes []model.ArtifactBlock, fits []bool) (placed, parents []int) {
	held := blockParents(nodes)
	placed = make([]int, 0, len(nodes))
	parents = make([]int, 0, len(nodes))
	for index := range nodes {
		if !fits[index] {
			continue
		}
		parent := -1
		for above := held[index]; above >= 0; above = held[above] {
			if fits[above] {
				parent = above
				break
			}
		}
		placed = append(placed, index)
		parents = append(parents, parent)
	}
	return placed, parents
}

// parentTuple is one statement about the block that holds another: an assertion of the parent
// the render gives, or a retraction of the one the store still asserts.
func parentTuple(address, toKind, toID string, at time.Time, writer model.Actor, asserted bool) graphstore.UncausedTuple {
	return graphstore.UncausedTuple{Tuple: graphstore.Tuple{
		FromKind: BlockNodeKind, FromID: address, Kind: graphstore.TuplePartOf,
		ToKind: toKind, ToID: toID,
		At: at, Who: writer, Asserted: asserted,
	}}
}

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
// one ref_key and not one address per block.
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
) ([]blockParentDifference, error) {
	rows, err := q.Query(ctx, `
		select block.ordinality,
		       array_agg(current.to_kind) filter (where not current.wanted),
		       array_agg(current.to_id) filter (where not current.wanted),
		       coalesce(bool_or(current.wanted), false)
		from unnest($1::text[], $2::text[]) with ordinality as block (id, parent)
		left join lateral (
			select held.to_kind, held.to_id,
			       held.to_kind = case when block.parent is null then $6 else $3 end
			       and held.to_id = case when block.parent is null then $7 else $7 || '#' || block.parent end
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
	`, blockIDs, parentIDs, BlockNodeKind, graphstore.TuplePartOf, graphstore.KindValue, "artifact", refKey)
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
type heldLink struct{ kind, toKind, toID string }

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
				  and t.kind not in ('`+TupleInDocument+`', '`+graphstore.TuplePartOf+`')`)+`
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

// blockParents names, for each block, the index of the tightest block of the same document
// that holds it, or -1 for a block at the document's top level. blocks are in document order,
// each parent before its children, which is the order innermostBlocks walks too; over a
// block's own From that helper answers the block itself, because it pushes every block
// starting at or before the offset before it reads the top of its stack, so the parent is the
// entry below the block on the stack and not the top of it.
func blockParents(blocks []model.ArtifactBlock) []int {
	parents := make([]int, len(blocks))
	stack := make([]int, 0, 8)
	for index, block := range blocks {
		for len(stack) > 0 && blocks[stack[len(stack)-1]].To <= block.From {
			stack = stack[:len(stack)-1]
		}
		parents[index] = -1
		if len(stack) > 0 {
			parents[index] = stack[len(stack)-1]
		}
		stack = append(stack, index)
	}
	return parents
}
