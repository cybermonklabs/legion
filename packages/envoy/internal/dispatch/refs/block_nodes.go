package refs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
func blockNode(address string, document indexedDocument, at time.Time, state string) graphstore.Node {
	return graphstore.Node{
		Kind: BlockNodeKind, ID: address, Supertag: BlockSupertag,
		Visibility: graphstore.VisibilityProject, State: state,
		Project: document.project, Parties: document.parties,
		MadeAt: at, MadeBy: document.writer, SourceEvent: graphstore.NoEvent,
	}
}

// indexedDocument is what the version's own row and its document's row say of the pass: the
// document's ref_key, which every block's address starts with; the actor the version's row
// records as its writer, which every node and tuple this pass writes carries; and the project the
// document belongs to, or for an agent conversation's file the session it is between, which every
// block node the pass writes carries (graphstore.Node's Project and Parties). A conversation holds
// no markdown document (api's agentOwnsFilesOnly), so no block has parties today; the read keeps
// the one rule every document's node follows.
type indexedDocument struct {
	refKey  string
	writer  model.Actor
	project string
	parties []string
}

// indexedVersionRecord reads indexedDocument in one statement, under the owner row the pass's
// caller already holds. A version no row records - a caller indexing blocks of its own, as the
// seed does - leaves the writer nobody, the zero actor, exactly as a node of the store written on
// a credential that names nobody carries.
func indexedVersionRecord(ctx context.Context, tx pgx.Tx, artifactID string, version int) (indexedDocument, error) {
	var document indexedDocument
	var project, session *string
	var authors []byte
	if err := tx.QueryRow(ctx, `
		select a.ref_key, a.project_key, a.session_id, v.authors
		from artifacts a
		left join artifact_versions v on v.artifact_id = a.id and v.number = $2
		where a.id = $1::uuid
	`, artifactID, version).Scan(&document.refKey, &project, &session, &authors); err != nil {
		return indexedDocument{}, fmt.Errorf("read version %d of %s for its block index: %w", version, artifactID, err)
	}
	if project != nil {
		document.project = *project
	}
	if session != nil {
		document.parties = []string{graphstore.PartySession(*session)}
	}
	writers, err := storedActors(authors)
	if err != nil {
		return indexedDocument{}, fmt.Errorf("decode the authors of version %d of %s: %w", version, artifactID, err)
	}
	if len(writers) > 0 {
		document.writer = writers[0]
	}
	return document, nil
}

// scanFreshBlocks reads back the rows the document_blocks statement wrote: the created_at of
// each row it truly inserted, by block id. A row it updated is left out, since the block was
// already a part of the document and states no second membership.
func scanFreshBlocks(rows pgx.Rows, err error) (map[string]time.Time, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fresh := map[string]time.Time{}
	for rows.Next() {
		var id string
		var created time.Time
		var inserted bool
		if err := rows.Scan(&id, &created, &inserted); err != nil {
			return nil, err
		}
		if inserted {
			fresh[id] = created
		}
	}
	return fresh, rows.Err()
}

// blockFarEnd is one far end a block's part_of points at now.
type blockFarEnd struct{ kind, id string }

// reconcileBlockParents states the parent of every block of the version and projects it into
// document_blocks. It returns the tuples it states, which its caller appends to the pass's
// write as it appends the reference pass's.
//
// It reads the parent each block holds in the store, over every block of the document and not
// the changed ones alone, so a block another block now holds, whose own text no version
// changed, gets its new parent here. It states a tuple only where the render disagrees with
// what the store asserts: one retraction for the parent that changed, one assertion for the new
// one. A re-index of a document nothing moved states nothing, which is what keeps a second pass
// from asserting a parent twice, since no code may update or delete a tuple row.
//
// The comparison is the read's own (heldBlockParentDifferences): the parent the render gives
// travels with the probe, so a document nothing moved reads back no row at all rather than one
// row per block for Go to compare. Both statements name a block by its id and build its node
// address from the document's ref_key, as the insert above does, so a pass over thousands of
// blocks holds no address for the thousands it states nothing about.
//
// The projection is a statement of its own rather than a column of the insert above, so that
// insert keeps firing only for a block whose text changed: a block that only moved keeps its
// changed_version and changed_at. The statement carries the node columns beside the parent, so
// a row the insert did not touch learns the address of its node here.
func reconcileBlockParents(
	ctx context.Context,
	tx pgx.Tx,
	artifactID, refKey string,
	nodes []model.ArtifactBlock,
	fits []bool,
	at time.Time,
	writer model.Actor,
) ([]graphstore.UncausedTuple, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	placed, parents := placeBlocks(nodes, fits)
	if len(placed) == 0 {
		return nil, nil
	}
	blockIDs := make([]string, len(placed))
	parentIDs := make([]*string, len(placed))
	for index, block := range placed {
		blockIDs[index] = nodes[block].ID
		if parents[index] >= 0 {
			parentIDs[index] = &nodes[parents[index]].ID
		}
	}
	differences, err := heldBlockParentDifferences(ctx, tx, refKey, blockIDs, parentIDs)
	if err != nil {
		return nil, fmt.Errorf("read the parents the blocks of %s hold: %w", artifactID, err)
	}
	var tuples []graphstore.UncausedTuple
	for _, difference := range differences {
		address := blockNodeAddress(refKey, blockIDs[difference.block])
		for _, far := range difference.retract {
			tuples = append(tuples, parentTuple(address, far.kind, far.id, at, writer, false))
		}
		if difference.holds {
			continue
		}
		toKind, toID := "artifact", refKey
		if parent := parentIDs[difference.block]; parent != nil {
			toKind, toID = BlockNodeKind, blockNodeAddress(refKey, *parent)
		}
		tuples = append(tuples, parentTuple(address, toKind, toID, at, writer, true))
	}
	if _, err := tx.Exec(ctx, `
		update document_blocks d
		set node_kind = $3, node_address = $4 || '#' || p.block_id, parent_block_id = p.parent
		from unnest($2::text[], $5::text[]) as p (block_id, parent)
		where d.artifact_id = $1::uuid and d.block_id = p.block_id
		  and (d.parent_block_id is distinct from p.parent
		       or d.node_kind is distinct from $3
		       or d.node_address is distinct from $4 || '#' || p.block_id)
	`, artifactID, blockIDs, BlockNodeKind, refKey, parentIDs); err != nil {
		return nil, fmt.Errorf("project the parents of the blocks of %s: %w", artifactID, err)
	}
	return tuples, nil
}

// PlaceBlocks is where the block index places each block of a version of the document refKey
// names. It answers the version's unique blocks (the first block of each id), the index among
// them of each block it states a part_of for, in document order, and beside each of those the
// index among them of the block that holds it, -1 for a block the document holds. blocks are
// the version's rendered blocks, as IndexBlocks takes them.
//
// It is the one walk the index states each part_of from (reconcileBlockParents) and the sizing
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
// would be thousands of round trips. The lateral reads through graph_tuples_current, whose column
// order is the one the distinct on walks.
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
	tx pgx.Tx,
	refKey string,
	blockIDs []string,
	parentIDs []*string,
) ([]blockParentDifference, error) {
	rows, err := tx.Query(ctx, `
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
