package refs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Pointer is one reference a typed block's `block[]` attribute holds (pmdoc.BlockPointer): the
// typed block's id and type, the link's kind (the attribute's name), the block reference it names,
// and the block types the attribute points at (none: any block).
type Pointer struct {
	Block, BlockType, Kind string
	Target                 text.Ref
	Targets                []string
}

// BlockNodeKind is what the node and tuple store calls one block of a document, and
// BlockSupertag the supertag every one of them carries. A block's own type - paragraph,
// decision, check - is the `type` column of its row and a supertag of the block schema
// (migration 20261109051232); what makes the row a block is `block`, the supertag of the closed
// list that names one.
const (
	BlockNodeKind = "block"
	BlockSupertag = "block"
)

// TupleInDocument carries a block's membership of its document: the fact `graph_edges` reads
// off a document_blocks row, dated that row's own created_at. Beside it graphstore.TuplePartOf
// carries the block of the same document that holds this one, which document_blocks's
// parent_block_id projects and which is the document's own artifact address for a block no
// block holds.
const TupleInDocument = "in_document"

// BlockTuple is one fact a block index pass states, with whether the row behind it takes the
// version's own event as its source_seq. A block_refs row the pass inserts outside a first
// indexing is written with none, and refs.Stamp or docs' stampVersionEvent fills it with that
// event in this same transaction, so the tuple's legacy_source_seq is that event's id; the pass
// cannot read the value back, since no tuple may be updated later. A fact no row of the
// projection dates that way - a block's parent and its membership of its document - leaves the
// mark false and its legacy_source_seq as the pass read it.
type BlockTuple struct {
	graphstore.Tuple
	FromVersionEvent bool
}

// BlockIndexWrite is what one pass of the block index states in the node and tuple store: the
// block nodes it wrote, and the tuples the projection rows beside them are the projection of.
//
// The nodes are written by the pass itself, because the foreign key on (node_kind,
// node_address) is checked inside each statement that writes a projection row and there is no
// node to name otherwise; the tuples are not, because on the request path no event exists while
// the pass runs. The caller appends them once it holds the event that caused the write
// (Caused), in the same transaction, where refs.Stamp already runs. A pass that lost the
// revision fence states nothing and leaves an empty write, which Caused writes nothing for.
type BlockIndexWrite struct {
	Nodes  []graphstore.Node
	Tuples []BlockTuple
}

// Caused appends what the pass stated under the event that caused it, and moves the nodes the
// pass wrote onto that event: a node row is the projection of its tuples and may be written
// again, while a tuple may not be updated, which is why the tuples wait for this call rather
// than being stamped after it. event is graphstore.NoEvent where no event recorded the write,
// and such a tuple carries the sentinel and no legacy_source_seq, as its row carries no
// source_seq either.
func (w BlockIndexWrite) Caused(ctx context.Context, tx pgx.Tx, event int64) error {
	if len(w.Nodes) == 0 && len(w.Tuples) == 0 {
		return nil
	}
	// The nodes are not written again: graphstore.Caused moves the event onto the rows the pass
	// already wrote, in one statement over their addresses. Writing them again would pay a
	// second node write per block for one column, and the upsert's own guard would refuse it,
	// since nothing else about the node moved.
	if err := graphstore.Caused(ctx, tx, w.Nodes, event); err != nil {
		return err
	}
	tuples := make([]graphstore.Tuple, len(w.Tuples))
	for index, stated := range w.Tuples {
		tuple := stated.Tuple
		tuple.SourceEvent = event
		if stated.FromVersionEvent && tuple.LegacySourceSeq == nil && event != graphstore.NoEvent {
			tuple.LegacySourceSeq = &event
		}
		tuples[index] = tuple
	}
	return graphstore.Write(ctx, tx, nil, tuples)
}

// blockNodeAddress is a block's address in the store, which document_blocks.node_id and
// block_refs.from_node hold and which the graph addresses a block by: the document's ref_key,
// '#' and the block's id.
func blockNodeAddress(refKey, blockID string) string { return refKey + "#" + blockID }

// MaxBlockNodeBytes bounds a block's address in the store. graph_nodes keys a node by
// (kind, id) in a B-tree, and graph_tuples_once, the repeat key of a relation, keys the
// addresses of both of its ends at once, so two block addresses and their kinds have to fit
// the roughly 2,704 bytes a B-tree entry holds. A block's address holds its document's
// ref_key, which nothing short of that key's own unique index bounds (migration 0121
// hash-indexes document_blocks.node_id for the same reason), so a long-named document's
// blocks can pass that on their own. Half the bound every reference target takes leaves both
// ends and their kinds well inside it.
//
// It is exported because the census of the store against its projections and the backfill of
// the rows written before the store existed (internal/dispatch/graphcensus) read the same
// bound: a block past it carries no node here, so a census holding such a row to one would
// report a difference this writer made on purpose.
const MaxBlockNodeBytes = text.MaxTargetBytes / 2

// blockNodeFits is whether a block's address is inside that bound. A block past it is no node
// of the store, as a block whose id is past text.MaxBlockIDBytes is no graph node: its row
// carries no node address, nothing is stated about it, and a block it holds reaches the
// nearest block above it that is a node, or the document.
func blockNodeFits(address string) bool { return len(address) <= MaxBlockNodeBytes }

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

// blockNode is one block's node as a version's index writes it: project-visible, in state, made
// by the version's writer at its time, and with no cause yet - Caused gives it the version's
// event. A node already there keeps its made_at and made_by, so a block is made by the version
// that first indexed it.
func blockNode(address string, writer model.Actor, at time.Time, state string) graphstore.Node {
	return graphstore.Node{
		Kind: BlockNodeKind, ID: address, Supertag: BlockSupertag,
		Visibility: graphstore.VisibilityProject, State: state,
		MadeAt: at, MadeBy: writer, SourceEvent: graphstore.NoEvent,
	}
}

// IndexBlocks records version `version` (written at versionAt) of document artifactID's blocks as
// graph nodes, and each mention written inside a block as an edge out of the innermost block
// holding its start, in the caller's transaction, which holds the document's owner row. blocks
// are the version's rendered blocks in document order, each parent before its children; the first
// holder of a repeated id is the node. located must be Mentions of the document's text, the one
// reading of it a version's index takes, so a mention's offset places it in the block holding it
// however the text wrote the reference. A block whose id is longer than text.MaxBlockIDBytes,
// which no key of the index can hold, is no node: a mention inside it starts at the innermost
// node holding it, if any, and the indexing logs a warning naming the document, the longest
// such id's length and the bound.
//
// pointers are the version's `block[]` values (pmdoc.BlockPointers, which lists none of a typed
// block repeating the id of a typed block before it): each whose block is a node is an edge of its
// attribute's kind from that block, carrying the attribute's targets as to_types, which the
// readers list only while its target has an allowed type (block_ref_listed). A value is written in
// its typed block's opening line, so the extraction also finds it as a mention held by that block:
// the block's mention of a target is kept only while the block itself holds more references to it
// than its pointers do (a string attribute citing it too, say).
//
// A block whose text (markdown[From:To]) is unchanged keeps its changed version; a new, changed
// or restored block takes this version, and its text is recorded in document_block_texts; a
// block the version no longer holds is deleted, recorded there with no text, its node's state
// stated gone, and the edges at it stop resolving until a version holds its id again. A block's
// stored excerpt is its text cut to BlockExcerptRunes, the excerpt a document's mention has
// shown since migration 0092.
//
// A pair (block, kind, target) that survives keeps its created version, time and source event,
// and takes the to_types this version gives it. A new pair takes this version and versionAt, and
// the source event Stamp gives it. On the document's first indexing (no block rows yet), a new
// pair dates instead from the document's own refs row for that target, when one exists. The
// version is recorded as indexed, and as the version the document's blocks stand at
// (document_block_versions), a mark that only ever moves forward: the second result is false
// where this pass lost that race to one holding the same version or a higher one, and such a
// pass writes nothing at all - no projection row and no tuple - since what it would state is
// what the winning version already stated. It returns the targets of the pairs
// it inserted or deleted, or whose to_types it changed, whose counts moved: a node is counted
// once for each block citing it. It also returns what else a Referenced by panel or a link
// shows that the version moved: the target of each pair it kept in a block whose text it
// changed (the block's text is that link's excerpt), and each block it changed or deleted that
// some source cites (citedBlocks), whose text every link to it and its own panel show.
//
// Its third result is what the pass states in the node and tuple store, which the caller
// appends under the event that caused the write (BlockIndexWrite).
func IndexBlocks(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	version int,
	versionAt time.Time,
	markdown string,
	blocks []model.ArtifactBlock,
	located []text.Located,
	pointers []Pointer,
) ([]Target, bool, BlockIndexWrite, error) {
	// The fence is the pass's first write, so a pass that loses it has written nothing to undo
	// and waits at that one row rather than at a projection row further in.
	won, err := standsAtVersion(ctx, tx, artifactID, version)
	if err != nil || !won {
		return nil, won, BlockIndexWrite{}, err
	}
	refKey, writer, err := indexedVersionRecord(ctx, tx, artifactID, version)
	if err != nil {
		return nil, false, BlockIndexWrite{}, err
	}
	stored, err := storedBlockSums(ctx, tx, artifactID, len(blocks))
	if err != nil {
		return nil, false, BlockIndexWrite{}, err
	}
	firstIndexing := len(stored) == 0
	nodes, long, longest := uniqueBlocks(blocks)
	if long > 0 {
		slog.Warn("dispatch: block ids past the bound are not graph nodes; a mention inside one of their blocks starts at the block holding it",
			"artifact", artifactID, "version", version, "blocks", long, "longest_bytes", longest, "bound_bytes", text.MaxBlockIDBytes)
	}
	fits, unaddressable := blockFits(refKey, nodes)
	if unaddressable > 0 {
		slog.Warn("dispatch: block addresses past the bound are no nodes of the store; their rows carry no node address and the pass states no fact about them",
			"artifact", artifactID, "version", version, "blocks", unaddressable, "ref_key_bytes", len(refKey), "bound_bytes", MaxBlockNodeBytes)
	}

	// The blocks whose text this version changed (new, changed or restored), and those it deleted.
	var ids, types, sums, excerpts []string
	var textIDs []string
	var texts []*string
	// A block whose text no version moved needs no string of its sum: the comparison against
	// the sum the index holds reads the hex out of one buffer this loop keeps, and only a
	// block this version writes a row for takes a string of its own.
	var sum blockSumHex
	for _, block := range nodes {
		body := markdown[block.From:block.To]
		blockTextSum(&sum, body)
		previous, held := stored[block.ID]
		delete(stored, block.ID)
		if held && previous == string(sum[:]) {
			continue
		}
		ids, types, sums = append(ids, block.ID), append(types, block.Type), append(sums, string(sum[:]))
		excerpts = append(excerpts, text.HeadRunes(body, BlockExcerptRunes))
		textIDs, texts = append(textIDs, block.ID), append(texts, &body)
	}
	deleted := make([]string, 0, len(stored))
	for id := range stored {
		deleted = append(deleted, id)
	}
	// In id order, so the rows this pass writes and the facts it states come in an order a
	// reader of the log can predict, which a map's iteration would not give.
	slices.Sort(deleted)
	for _, id := range deleted {
		textIDs, texts = append(textIDs, id), append(texts, nil)
	}

	// The nodes this pass states are the blocks whose document_blocks row it writes - the new,
	// the changed and the restored - and the ids the version dropped, and no other. That is
	// exactly the set R16's foreign key needs a node for, and it is what makes the write cost
	// what the edit changed rather than what the document holds: a version that moves one word
	// of a two-thousand-block document states two node rows, not four thousand. A block whose
	// text this version left alone already has its node from the version that first indexed
	// it, and its placement, which may still have moved, is a tuple reconcileBlockParents
	// states against that node.
	changed := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		changed[id] = struct{}{}
	}
	write := BlockIndexWrite{Nodes: make([]graphstore.Node, 0, len(ids)+len(deleted))}
	for index, block := range nodes {
		if !fits[index] {
			continue
		}
		if _, moved := changed[block.ID]; !moved {
			continue
		}
		write.Nodes = append(write.Nodes, blockNode(blockNodeAddress(refKey, block.ID), writer, versionAt, graphstore.StateLive))
	}
	// A block the version no longer holds has its node stated gone. The tuple that says so is
	// not written here: graphstore.Write states a base attribute it moved, with this write's
	// own time, writer and cause, so a block restored by a later version is stated live by the
	// same rule and no writer of this store states a base attribute of its own.
	for _, id := range deleted {
		if !blockIDFits(refKey, id) {
			continue
		}
		write.Nodes = append(write.Nodes, blockNode(blockNodeAddress(refKey, id), writer, versionAt, graphstore.StateGone))
	}
	// The nodes go in before any projection row that names one: the foreign key is checked
	// inside the statement writing that row, so a node written afterwards would be too late.
	if err := graphstore.Write(ctx, tx, write.Nodes, nil); err != nil {
		return nil, false, BlockIndexWrite{}, fmt.Errorf("write the block nodes of version %d of %s: %w", version, artifactID, err)
	}

	if len(deleted) > 0 {
		if _, err := tx.Exec(ctx, `
			delete from document_blocks where artifact_id = $1 and block_id = any($2)
		`, artifactID, deleted); err != nil {
			return nil, false, BlockIndexWrite{}, fmt.Errorf("delete the blocks version %d of %s no longer holds: %w", version, artifactID, err)
		}
	}
	if len(ids) > 0 {
		// xmax = 0 tells a row this statement inserted from one it updated, which is what says
		// whether the block joined the document here: a block whose row was already there - a
		// block whose text alone changed - keeps that row and the membership it already states.
		fresh, err := scanFreshBlocks(tx.Query(ctx, `
			insert into document_blocks (artifact_id, block_id, node_id, node_kind, node_address, type, text_sha256, excerpt, changed_version, changed_at)
			select $1::uuid, b.id, a.ref_key || '#' || b.id,
			       case when octet_length(a.ref_key || '#' || b.id) <= $9 then $8 end,
			       case when octet_length(a.ref_key || '#' || b.id) <= $9 then a.ref_key || '#' || b.id end,
			       b.type, b.sum, b.excerpt, $2, $3
			from unnest($4::text[], $5::text[], $6::text[], $7::text[]) as b (id, type, sum, excerpt)
			cross join (select ref_key from artifacts where id = $1::uuid) a
			on conflict (artifact_id, block_id) do update set
				type = excluded.type,
				text_sha256 = excluded.text_sha256,
				excerpt = excluded.excerpt,
				changed_version = excluded.changed_version,
				changed_at = excluded.changed_at
			returning block_id, created_at, xmax = 0
		`, artifactID, version, versionAt, ids, types, sums, excerpts, BlockNodeKind, MaxBlockNodeBytes))
		if err != nil {
			return nil, false, BlockIndexWrite{}, fmt.Errorf("record the blocks version %d of %s changed: %w", version, artifactID, err)
		}
		for _, id := range ids {
			joined, inserted := fresh[id]
			if !inserted || !blockIDFits(refKey, id) {
				continue
			}
			write.Tuples = append(write.Tuples, BlockTuple{Tuple: graphstore.Tuple{
				FromKind: BlockNodeKind, FromID: blockNodeAddress(refKey, id), Kind: TupleInDocument,
				ToKind: "artifact", ToID: refKey,
				At: joined, Who: writer, Asserted: true,
			}})
		}
	}
	if len(textIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			insert into document_block_texts (artifact_id, block_id, version, text)
			select $1::uuid, t.id, $2, t.text from unnest($3::text[], $4::text[]) as t (id, text)
			on conflict (artifact_id, block_id, version) do update set text = excluded.text
		`, artifactID, version, textIDs, texts); err != nil {
			return nil, false, BlockIndexWrite{}, fmt.Errorf("record the block texts of version %d of %s: %w", version, artifactID, err)
		}
	}
	if err := reconcileBlockParents(ctx, tx, artifactID, refKey, nodes, fits, versionAt, writer, &write); err != nil {
		return nil, false, BlockIndexWrite{}, err
	}

	pairs := blockPairs(nodes, located, pointers)
	moved, links, err := replaceBlockRefs(ctx, tx, artifactID, refKey, version, versionAt, writer, firstIndexing, pairs)
	if err != nil {
		return nil, false, BlockIndexWrite{}, err
	}
	write.Tuples = append(write.Tuples, links...)
	// A pair held by a block whose text this version moved is a link whose excerpt moved with
	// it, which is the same set of blocks the nodes above were stated for.
	for _, pair := range pairs {
		if _, wrote := changed[pair.blockID]; wrote {
			moved = append(moved, pair.target)
		}
	}
	// textIDs is every block this version changed, restored or deleted.
	cited, err := citedBlocks(ctx, tx, artifactID, textIDs)
	if err != nil {
		return nil, false, BlockIndexWrite{}, err
	}
	moved = append(moved, cited...)
	if _, err := tx.Exec(ctx, `
		insert into document_indexed_versions (artifact_id, version) values ($1, $2)
		on conflict (artifact_id, version) do nothing
	`, artifactID, version); err != nil {
		return nil, false, BlockIndexWrite{}, fmt.Errorf("record version %d of %s indexed: %w", version, artifactID, err)
	}
	if err := writeDocumentWindows(ctx, tx, artifactID, markdown, nodes); err != nil {
		return nil, false, BlockIndexWrite{}, err
	}
	return moved, true, write, nil
}

// standsAtVersion settles the mark migration 0122's trigger left on the document when this
// version's row was inserted, and answers whether this pass is the one the document's blocks
// now stand at. The mark only ever moves forward: two passes can be indexing one document at
// once, and the slower one, reading version 9 while the other already wrote 10, would otherwise
// set the row back to 9 and claim 10's work as its own. The fence is the `where`, and the
// `returning` reads the row back in the same statement, so the pass learns in one conditional
// write whether it won: no row comes back when the fence held, which is this pass losing. A
// second pass at the same version loses too, which is what keeps a re-index from stating a
// parent twice, since no code may update or delete a tuple row.
//
// indexOwedDocument's own early return is a short cut, not this fence: it spares the work of a
// pass whose version is already settled, and it reads before the write rather than deciding it.
func standsAtVersion(ctx context.Context, tx pgx.Tx, artifactID string, version int) (bool, error) {
	var stands int
	err := tx.QueryRow(ctx, `
		insert into document_block_versions (artifact_id, version) values ($1, $2)
		on conflict (artifact_id) do update set version = excluded.version
		where document_block_versions.version is null or document_block_versions.version < excluded.version
		returning version
	`, artifactID, version).Scan(&stands)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record the blocks of %s indexed at version %d: %w", artifactID, version, err)
	}
	return stands == version, nil
}

// indexedVersionRecord is what the version's own row says of the pass: the document's ref_key,
// which every block's address starts with, and the actor that row records as the version's
// writer, which every node and tuple this pass writes carries. A version no row records - a
// caller indexing blocks of its own, as the seed does - leaves the writer nobody, the zero
// actor, exactly as a node of the store written on a credential that names nobody carries.
func indexedVersionRecord(ctx context.Context, tx pgx.Tx, artifactID string, version int) (string, model.Actor, error) {
	var refKey string
	var authors []byte
	if err := tx.QueryRow(ctx, `
		select a.ref_key, v.authors
		from artifacts a
		left join artifact_versions v on v.artifact_id = a.id and v.number = $2
		where a.id = $1::uuid
	`, artifactID, version).Scan(&refKey, &authors); err != nil {
		return "", model.Actor{}, fmt.Errorf("read version %d of %s for its block index: %w", version, artifactID, err)
	}
	writers, err := storedActors(authors)
	if err != nil {
		return "", model.Actor{}, fmt.Errorf("decode the authors of version %d of %s: %w", version, artifactID, err)
	}
	if len(writers) == 0 {
		return refKey, model.Actor{}, nil
	}
	return refKey, writers[0], nil
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
// document_blocks.
//
// It reads the parent each block holds in the store, over every block of the document and not
// the changed ones alone, so a block another block now holds, whose own text no version
// changed, gets its new parent here. It appends a tuple only where the render disagrees with
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
	write *BlockIndexWrite,
) error {
	if len(nodes) == 0 {
		return nil
	}
	// A block whose address is no key of the store is no node of it: the parent it reaches is
	// the nearest block above it that is one, or the document, and nothing is stated about the
	// block itself.
	held := blockParents(nodes)
	parents := make([]int, len(nodes))
	for index := range nodes {
		parents[index] = -1
		for above := held[index]; above >= 0; above = held[above] {
			if fits[above] {
				parents[index] = above
				break
			}
		}
	}
	blockIDs := make([]string, 0, len(nodes))
	parentIDs := make([]*string, 0, len(nodes))
	for index := range nodes {
		if !fits[index] {
			continue
		}
		blockIDs = append(blockIDs, nodes[index].ID)
		var parent *string
		if parents[index] >= 0 {
			parent = &nodes[parents[index]].ID
		}
		parentIDs = append(parentIDs, parent)
	}
	if len(blockIDs) == 0 {
		return nil
	}
	differences, err := heldBlockParentDifferences(ctx, tx, refKey, blockIDs, parentIDs)
	if err != nil {
		return fmt.Errorf("read the parents the blocks of %s hold: %w", artifactID, err)
	}
	for _, difference := range differences {
		address := blockNodeAddress(refKey, blockIDs[difference.block])
		for _, far := range difference.retract {
			write.Tuples = append(write.Tuples, parentTuple(address, far.kind, far.id, at, writer, false))
		}
		if difference.holds {
			continue
		}
		toKind, toID := "artifact", refKey
		if parent := parentIDs[difference.block]; parent != nil {
			toKind, toID = BlockNodeKind, blockNodeAddress(refKey, *parent)
		}
		write.Tuples = append(write.Tuples, parentTuple(address, toKind, toID, at, writer, true))
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
		return fmt.Errorf("project the parents of the blocks of %s: %w", artifactID, err)
	}
	return nil
}

// parentTuple is one statement about the block that holds another: an assertion of the parent
// the render gives, or a retraction of the one the store still asserts.
func parentTuple(address, toKind, toID string, at time.Time, writer model.Actor, asserted bool) BlockTuple {
	return BlockTuple{Tuple: graphstore.Tuple{
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
// far end, kept while it asserts (graphstore.CurrentOut) - but in one statement for every block
// of the document rather than one per block, which on a document of thousands of blocks would
// be thousands of round trips. The lateral reads through graph_tuples_current, whose column
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
				select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted
				from graph_tuples t
				where t.from_kind = $3 and t.from_id = $7 || '#' || block.id and t.kind = $4
				  and t.to_kind <> $5
				order by t.to_kind, t.to_id, t.seq desc
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

// DocumentWindowKind is the embeddings kind of a window of a document's blocks, as
// ItemKind.WindowKind is of a comment's, a message's or an ask's.
const DocumentWindowKind = "document_block"

// windowKinds is every kind an embeddings window row is written under. A statement naming them
// as literals is one the planner can prove embeddings_window_owner's partial predicate for,
// whatever a parameter holds (writeWindows).
const windowKinds = `('document_block', 'comment_block', 'message_block', 'ask_part')`

// BlockEmbeddingStates is where meaning search stands on each block of the owner whose windows
// are kind kind under ownerKey: model.BlockEmbedded for a block every window covering it holds
// a vector of its current text for, model.BlockEmbeddingPending while one of them waits for its
// vector. A block no window covers is in no entry of the map, which a reader of one block reads
// as model.BlockNotEmbedded - a block the indexer puts in no window, and every block of an owner
// whose windows are not written yet. The read names the window kinds as literals beside the
// parameter, as every read and write of a window does (writeWindows), so the planner can prove
// embeddings_window_owner's partial predicate and reach the owner's rows through that index.
//
// It lives here, with the writes of the rows it reads, so the list of window kinds is held in
// one place: this package's windowKinds, migration 20261102233630's index predicate and the
// column the two of them name.
func BlockEmbeddingStates(ctx context.Context, q Queryer, kind, ownerKey string) (map[string]string, error) {
	rows, err := q.Query(ctx, `
		select covered.block_id, bool_and(e.embedded_hash is not distinct from e.content_hash)
		from embeddings e cross join unnest(e.covers) as covered(block_id)
		where e.kind = $1 and e.kind in `+windowKinds+`
		  and split_part(e.id, '#', 1) = $2
		group by covered.block_id
	`, kind, ownerKey)
	if err != nil {
		return nil, fmt.Errorf("read the embedding state of the blocks of %s %s: %w", kind, ownerKey, err)
	}
	defer rows.Close()
	states := map[string]string{}
	for rows.Next() {
		var blockID string
		var current bool
		if err := rows.Scan(&blockID, &current); err != nil {
			return nil, fmt.Errorf("scan the embedding state of a block of %s %s: %w", kind, ownerKey, err)
		}
		states[blockID] = model.BlockEmbeddingPending
		if current {
			states[blockID] = model.BlockEmbedded
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the embedding state of the blocks of %s %s: %w", kind, ownerKey, err)
	}
	return states, nil
}

// WriteDocumentWindows writes the windows of document artifactID's blocks, as the version write
// writes them (IndexBlocks), for the block-index tick's window arm: blocks are the document's
// blocks in document order, each parent before its children, and markdown the text they index
// into.
func WriteDocumentWindows(ctx context.Context, tx pgx.Tx, artifactID, markdown string, blocks []model.ArtifactBlock) error {
	nodes, _, _ := uniqueBlocks(blocks)
	return writeDocumentWindows(ctx, tx, artifactID, markdown, nodes)
}

// writeDocumentWindows writes the windows of the document's blocks (nodes, as uniqueBlocks keeps
// them) in the caller's transaction, which holds the document's owner row, and stamps the state
// those blocks are in beside them, so the window arm can tell these windows from ones an older
// build left behind. The owner key of a document's windows is its artifact id, and the context
// of each is the document's own title and the heading above the window
// (itemblocks.DocumentWindows).
func writeDocumentWindows(ctx context.Context, tx pgx.Tx, artifactID, markdown string, nodes []model.ArtifactBlock) error {
	var title string
	if err := tx.QueryRow(ctx, `select name from artifacts where id = $1::uuid`, artifactID).Scan(&title); err != nil {
		return fmt.Errorf("read the title of %s for its windows: %w", artifactID, err)
	}
	return writeWindows(ctx, tx, DocumentWindowKind, artifactID,
		itemblocks.DocumentWindows(title, documentBlockTexts(markdown, nodes)), blockState{
			query: `select ` + documentBlockState + ` as block_state
			          from document_block_versions v where v.artifact_id = $5::uuid`,
			args: []any{artifactID},
		})
}

// documentBlockTexts is each block of nodes holding text of its own, in document order, with
// that text (markdown[From:To]): a block inside another is left out, since the window of its
// parent already holds its text and no window should embed one passage twice. nodes are in
// document order, each parent before its children, so a block starting before the last kept one
// ended is inside it.
func documentBlockTexts(markdown string, nodes []model.ArtifactBlock) []itemblocks.WindowBlock {
	blocks := make([]itemblocks.WindowBlock, 0, len(nodes))
	end := 0
	for _, node := range nodes {
		if node.To <= node.From || node.From < end {
			continue
		}
		end = node.To
		blocks = append(blocks, itemblocks.WindowBlock{ID: node.ID, Type: node.Type, Text: markdown[node.From:node.To]})
	}
	return blocks
}

// blockState is how one write of an owner's windows (writeWindows) tells that write what state
// the blocks those windows cover are in: one query returning a single block_state column, which
// the write runs as a common table expression and stamps beside the windows. An index that
// settles the owner's own state row writes that row here, returning the state it has just
// written (indexedItemState), so it pays no round trip for the stamp; a write that settles no
// such row reads the state the row already holds (storedItemState, and the document's own query
// above). The query numbers its parameters from $5 and passes their values in args, since
// writeWindows holds $1 to $4.
type blockState struct {
	query string
	args  []any
}

// writeWindows writes windows as the windows of owner ownerKey, of kind kind, in the caller's
// transaction: each takes the id "<owner key>#<its id suffix>" through
// embeddings_enqueue_window, which rewrites a row only where the hash of its text moved, and the
// owner's rows of that kind the new set does not name are deleted. Rewriting an owner's windows
// when none of them changed therefore leaves every row's vector, attempts and wait as they
// stand, and an owner left with no block loses every window it had.
//
// The whole write is one statement, so a document of fifty windows costs one round trip and an
// index whose state query settles its owner's state row (indexedItemState) pays nothing beyond
// the round trip it was already sending for that. The windows go through jsonb_to_recordset in
// the top-level select; the state query, the stamp of what it returns and the delete are common
// table expressions, and Postgres runs a data-modifying one exactly once and to completion
// whether or not the select reads it. The delete cannot race the enqueue: it names only ids the
// new set does not hold, and the enqueue writes only ids it does.
//
// The delete names the kind twice, as the parameter and in the literal list of every window
// kind, because a generic plan knows nothing of what the parameter holds: the literal list is
// what lets the planner prove embeddings_window_owner's partial predicate and read the owner's
// rows through that index.
func writeWindows(ctx context.Context, tx pgx.Tx, kind, ownerKey string, windows []itemblocks.Window, state blockState) error {
	type row struct {
		Suffix string   `json:"suffix"`
		Text   string   `json:"text"`
		Covers []string `json:"covers"`
	}
	rows := make([]row, len(windows))
	ids := make([]string, len(windows))
	for i, window := range windows {
		rows[i] = row{Suffix: window.IDSuffix, Text: window.Text, Covers: window.Covers}
		ids[i] = ownerKey + "#" + window.IDSuffix
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("encode the windows of %s %s: %w", kind, ownerKey, err)
	}
	if _, err := tx.Exec(ctx, `
		with state as (
			`+state.query+`
		),
		stamped as (
			insert into embedding_window_states (kind, owner_key, block_state, written_at)
			select $1::text, $2::text, state.block_state, now() from state
			on conflict (kind, owner_key) do update
			  set block_state = excluded.block_state, written_at = excluded.written_at
		),
		dropped as (
			delete from embeddings
			where kind = $1 and kind in `+windowKinds+`
			  and split_part(id, '#', 1) = $2 and id <> all($4::text[])
		)
		select embeddings_enqueue_window($1, $2 || '#' || w.suffix, w.text, w.covers)
		from jsonb_to_recordset($3::jsonb) as w (suffix text, text text, covers text[])
	`, append([]any{kind, ownerKey, payload, ids}, state.args...)...); err != nil {
		return fmt.Errorf("write the windows of %s %s: %w", kind, ownerKey, err)
	}
	return nil
}

// documentBlockState is the state of a document's blocks that its windows are stamped with
// (writeDocumentWindows) and read back against (DocumentsOwedWindows): the version they are
// indexed at, over document_block_versions v. A document owed its first block index holds a
// null version, which stamps and compares as the empty string, so windowing it once settles it
// until the index that gives it a version writes its windows again. The state is read from
// document_block_versions in the statement that writes the windows, so what is stamped is the
// version the blocks those windows cover are indexed at.
const documentBlockState = `coalesce(v.version::text, '')`

// DocumentsOwedWindows lists at most limit documents whose blocks the index holds and whose
// windows do not stand for those blocks, in artifact id order past after (empty: from the
// start), for the block-index tick's window arm. A document written before meaning search moved
// to windows has blocks and no window, and one whose version write failed after its blocks were
// written has the same. One a build without this change re-indexed during the deploy is the
// harder case: it keeps the windows of its old text, so holding a window says nothing. The read
// therefore asks both whether the state its windows were written from
// (embedding_window_states, migration 20261104010400) is the state its blocks are in now, and
// whether it holds a window at all, so a window row lost by any other route is written again
// too. The read walks document_block_versions, one row per document, and probes the other
// three tables through their keys.
func DocumentsOwedWindows(ctx context.Context, q Queryer, after string, limit int) ([]string, error) {
	rows, err := q.Query(ctx, `
		select v.artifact_id::text from document_block_versions v
		where v.artifact_id > $1::uuid
		  and exists (select 1 from document_blocks b where b.artifact_id = v.artifact_id)
		  and (not exists (
			select 1 from embedding_window_states w
			where w.kind = '`+DocumentWindowKind+`' and w.owner_key = v.artifact_id::text
			  and w.block_state = `+documentBlockState+`
		  ) or not exists (
			select 1 from embeddings e
			where e.kind = '`+DocumentWindowKind+`' and split_part(e.id, '#', 1) = v.artifact_id::text
		  ))
		order by v.artifact_id limit $2
	`, uuidAfter(after), limit)
	if err != nil {
		return nil, fmt.Errorf("list the documents whose windows do not stand for their blocks: %w", err)
	}
	documents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the documents whose windows do not stand for their blocks: %w", err)
	}
	return documents, nil
}

// uuidAfter is a keyset cursor over uuids: the id a batch starts after, or the nil uuid, which
// is below every uuid a row holds, when a batch starts from the beginning.
func uuidAfter(after string) string {
	if after == "" {
		return "00000000-0000-0000-0000-000000000000"
	}
	return after
}

// citedBlocks is, of the document's blocks named by ids, each that a source cites (a refs or a
// block_refs row points at its node), as a block target.
func citedBlocks(ctx context.Context, tx pgx.Tx, artifactID string, ids []string) ([]Target, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cited, err := scanTargets(tx.Query(ctx, `
		select 'block', b.node_id
		from (
			select a.ref_key || '#' || t.id as node_id
			from unnest($2::text[]) as t (id)
			cross join (select ref_key from artifacts where id = $1::uuid) a
		) b
		where exists (select 1 from refs r where r.to_kind = 'block' and r.to_id = b.node_id)
		   or exists (select 1 from block_refs r where r.to_kind = 'block' and r.to_id = b.node_id)
	`, artifactID, ids))
	if err != nil {
		return nil, fmt.Errorf("read the cited blocks of %s: %w", artifactID, err)
	}
	return cited, nil
}

// blockSumHex is a block text's sha256 as document_blocks stores it: the hex of the 32 bytes,
// held in an array so the pass over a document's blocks can hash each one into a buffer of its
// own and compare it against the stored sum with no string made (string(sum[:]) == stored
// allocates nothing).
type blockSumHex [2 * sha256.Size]byte

// blockTextSum writes the sha256 (hex) of a block's text (markdown[From:To]) into sum.
func blockTextSum(sum *blockSumHex, body string) {
	hashed := sha256.Sum256([]byte(body))
	hex.Encode(sum[:], hashed[:])
}

// BlockTextSum is the sha256 (hex) of a block's text (markdown[From:To]), as document_blocks
// stores it in text_sha256.
func BlockTextSum(body string) string {
	var sum blockSumHex
	blockTextSum(&sum, body)
	return string(sum[:])
}

// storedBlockSums is each block the document's index holds, by id, with its text's sha256.
// blocks is how many blocks the version being indexed holds, which is about how many rows the
// read answers: a map made at that size is not grown once per thousand rows read into it.
func storedBlockSums(ctx context.Context, tx pgx.Tx, artifactID string, blocks int) (map[string]string, error) {
	rows, err := tx.Query(ctx, `select block_id, text_sha256 from document_blocks where artifact_id = $1`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the blocks of %s: %w", artifactID, err)
	}
	defer rows.Close()
	stored := make(map[string]string, blocks)
	for rows.Next() {
		var id, sum string
		if err := rows.Scan(&id, &sum); err != nil {
			return nil, fmt.Errorf("scan a block of %s: %w", artifactID, err)
		}
		stored[id] = sum
	}
	return stored, rows.Err()
}

// blockPair is one link a block holds: the block's id, the edge's kind and its target, and the
// block types it points at (nil: any).
type blockPair struct {
	blockID string
	kind    string
	target  Target
	toTypes []string
}

// heldTarget is a target as one block holds it.
type heldTarget struct {
	blockID string
	target  Target
}

// blockPairs is each distinct link the pointers of nodes and the mentions in located write: a
// pointer from its typed block (pointerPairs), a mention from the innermost of nodes holding its
// start. A mention no block holds, and an external URL, write none; nor does a block's mention of
// a target its own pointers account for (IndexBlocks).
func blockPairs(nodes []model.ArtifactBlock, located []text.Located, pointers []Pointer) []blockPair {
	pairs, pointed := pointerPairs(nodes, pointers)
	pairs = slices.Grow(pairs, len(located))

	starts := make([]int, len(located))
	for i, mention := range located {
		starts[i] = mention.Offset
	}
	holders := innermostBlocks(nodes, starts)
	mentions := make([]heldTarget, 0, len(located))
	counts := make(map[heldTarget]int, len(located))
	for i, mention := range located {
		if holders[i] < 0 {
			continue
		}
		target, ok := TargetOf(mention.Ref)
		if !ok {
			continue
		}
		held := heldTarget{blockID: nodes[holders[i]].ID, target: target}
		if counts[held] == 0 {
			mentions = append(mentions, held)
		}
		counts[held]++
	}
	for _, held := range mentions {
		if counts[held] > pointed[held] {
			pairs = append(pairs, blockPair{blockID: held.blockID, kind: "mentions", target: held.target})
		}
	}
	return pairs
}

// pointerPairs is each distinct link the pointers of nodes write, from its typed block, and the
// number of each block's pointer values naming each target. A pinned and an unpinned value of one
// attribute name one target, and write one pair. No attribute is named mentions (pmdoc), so no
// pointer's pair is a mention's. A pointer writes a pair only from the node uniqueBlocks keeps for
// its block id, the first holder of the id with an offset, and only when that node is the typed
// block holding it (its type is the pointer's BlockType). pmdoc.BlockPointers already lists none
// of a typed block repeating the id of a typed block before it. The type check keeps a typed
// block's pointers off a first holder of its id that is not typed (a paragraph, a heading). It is
// not what makes the two paths agree: a typed block's id reads back from the markdown as the tree
// holds it (pmdoc.TypedBlockIDAttribute), so the version write, indexing the tree, and the
// reconciliation, reading the markdown back, list the same pointers under that id and resolve it
// to the same node of the same render, and without the check both would write the same wrong edge
// from that holder. A holder with no offset (a list item, a table cell) is never a node, so a typed
// block after it is the id's node and keeps its pointers. One whose block is no node (an id past
// the bound, say) or not its own writes nothing and is charged against no mention.
func pointerPairs(nodes []model.ArtifactBlock, pointers []Pointer) ([]blockPair, map[heldTarget]int) {
	if len(pointers) == 0 {
		return nil, nil
	}
	type pointerKey struct {
		held heldTarget
		kind string
	}
	written := make(map[pointerKey]struct{}, len(pointers))
	nodeTypes := make(map[string]string, len(nodes))
	for _, node := range nodes {
		nodeTypes[node.ID] = node.Type
	}
	pairs := make([]blockPair, 0, len(pointers))
	pointed := make(map[heldTarget]int, len(pointers))
	for _, pointer := range pointers {
		if typ, ok := nodeTypes[pointer.Block]; !ok || typ != pointer.BlockType {
			continue
		}
		target, _ := TargetOf(pointer.Target)
		held := heldTarget{blockID: pointer.Block, target: target}
		pointed[held]++
		key := pointerKey{held: held, kind: pointer.Kind}
		if _, dup := written[key]; dup {
			continue
		}
		written[key] = struct{}{}
		pairs = append(pairs, blockPair{blockID: pointer.Block, kind: pointer.Kind, target: target, toTypes: pointer.Targets})
	}
	return pairs, pointed
}

// replaceBlockRefs reconciles the document's block_refs with pairs, matching on the five columns
// of its key, and returns the targets of the pairs it deleted and inserted, and of those whose
// to_types it changed.
//
// Beside them it states what each row it truly wrote or dropped is the projection of: one tuple
// out of the block, under that pair's own kind - a pointer attribute's name, or `mentions` for
// a block's mention - asserted for each pair the insert inserted and retracted for each pair
// the delete dropped, so the tuple and the row arrive and leave together. A surviving pair
// earns none: it keeps its dates and its source event and takes this version's to_types, and a
// tuple appended for it would carry this version's time and win the current read over the one
// that matches the row. The insert returns a surviving pair whose to_types changed as well, so
// the two are told apart by xmax = 0 and never by the rows that come back.
//
// A row's node columns are written by the insert alone: a pair written before this build keeps
// the nulls the projection's migration left it, and takes its node's address when a version
// writes the pair again. Adding them to the conflict update would return a row whose to_types
// no version changed and count its target as moved.
func replaceBlockRefs(
	ctx context.Context,
	tx pgx.Tx,
	artifactID, refKey string,
	version int,
	versionAt time.Time,
	writer model.Actor,
	firstIndexing bool,
	pairs []blockPair,
) ([]Target, []BlockTuple, error) {
	blockIDs := make([]string, len(pairs))
	kinds := make([]string, len(pairs))
	toKinds := make([]string, len(pairs))
	toIDs := make([]string, len(pairs))
	// A pair's to_types travel as one space-joined text, null for no limit: a block type's name is
	// a directive name (pmdoc), which holds no space.
	toTypes := make([]*string, len(pairs))
	for i, pair := range pairs {
		blockIDs[i], kinds[i], toKinds[i], toIDs[i] = pair.blockID, pair.kind, pair.target.Kind, pair.target.ID
		if len(pair.toTypes) > 0 {
			joined := strings.Join(pair.toTypes, " ")
			toTypes[i] = &joined
		}
	}
	dropped, err := scanBlockRefRows(tx.Query(ctx, `
		delete from block_refs r
		where r.artifact_id = $1 and not exists (
			select 1 from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
			where p.block_id = r.block_id and p.kind = r.kind and p.to_kind = r.to_kind and p.to_id = r.to_id)
		returning r.block_id, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq, true
	`, artifactID, blockIDs, kinds, toKinds, toIDs))
	if err != nil {
		return nil, nil, fmt.Errorf("delete the block mentions version %d of %s dropped: %w", version, artifactID, err)
	}
	moved := make([]Target, 0, len(dropped))
	var tuples []BlockTuple
	for _, row := range dropped {
		moved = append(moved, Target{Kind: row.toKind, ID: row.toID})
		address := blockNodeAddress(refKey, row.blockID)
		if !blockNodeFits(address) {
			continue
		}
		// The row is gone, so no source_seq is ever written for it and the retraction carries
		// none: its time is this version's, the moment the pair stopped being held.
		tuples = append(tuples, BlockTuple{Tuple: graphstore.Tuple{
			FromKind: BlockNodeKind, FromID: address, Kind: row.kind,
			ToKind: row.toKind, ToID: row.toID,
			At: versionAt, Who: writer, Asserted: false,
		}})
	}
	if len(pairs) == 0 {
		return moved, tuples, nil
	}
	// A surviving pair keeps its dates and source event and takes this version's to_types, so a
	// change of an attribute's targets reaches it here; it is returned only when that changed.
	written, err := scanBlockRefRows(tx.Query(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, node_kind, node_address, to_kind, to_id, kind, to_types, created_version, created_at, source_seq)
		select $1::uuid, p.block_id, a.ref_key || '#' || p.block_id,
		       case when octet_length(a.ref_key || '#' || p.block_id) <= $11 then $10 end,
		       case when octet_length(a.ref_key || '#' || p.block_id) <= $11 then a.ref_key || '#' || p.block_id end,
		       p.to_kind, p.to_id, p.kind, string_to_array(p.to_types, ' '), $6,
		       coalesce(case when $8 then r.created_at end, $7), case when $8 then r.source_seq end
		from unnest($2::text[], $3::text[], $4::text[], $5::text[], $9::text[]) as p (block_id, kind, to_kind, to_id, to_types)
		cross join (select ref_key from artifacts where id = $1::uuid) a
		left join refs r on r.from_kind = 'artifact' and r.from_id = $1::uuid::text
		                and r.to_kind = p.to_kind and r.to_id = p.to_id
		on conflict (artifact_id, block_id, to_kind, to_id, kind) do update set to_types = excluded.to_types
		where block_refs.to_types is distinct from excluded.to_types
		returning block_id, kind, to_kind, to_id, created_at, source_seq, xmax = 0
	`, artifactID, blockIDs, kinds, toKinds, toIDs, version, versionAt, firstIndexing, toTypes, BlockNodeKind, MaxBlockNodeBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("record the block mentions of version %d of %s: %w", version, artifactID, err)
	}
	for _, row := range written {
		moved = append(moved, Target{Kind: row.toKind, ID: row.toID})
		address := blockNodeAddress(refKey, row.blockID)
		if !row.inserted || !blockNodeFits(address) {
			continue
		}
		// Outside a first indexing the insert leaves source_seq null and the version's event
		// fills it in this same transaction, which is what FromVersionEvent names.
		tuples = append(tuples, BlockTuple{
			Tuple: graphstore.Tuple{
				FromKind: BlockNodeKind, FromID: address, Kind: row.kind,
				ToKind: row.toKind, ToID: row.toID,
				At: row.createdAt, Who: writer, LegacySourceSeq: row.sourceSeq, Asserted: true,
			},
			FromVersionEvent: true,
		})
	}
	return moved, tuples, nil
}

// blockRefRow is one block_refs row as the reconcile's two statements return it, with whether
// the insert inserted it rather than updating a row that was already there (xmax = 0).
type blockRefRow struct {
	blockID, kind, toKind, toID string
	createdAt                   time.Time
	sourceSeq                   *int64
	inserted                    bool
}

func scanBlockRefRows(rows pgx.Rows, err error) ([]blockRefRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var written []blockRefRow
	for rows.Next() {
		var row blockRefRow
		if err := rows.Scan(&row.blockID, &row.kind, &row.toKind, &row.toID, &row.createdAt, &row.sourceSeq, &row.inserted); err != nil {
			return nil, err
		}
		written = append(written, row)
	}
	return written, rows.Err()
}

// BlockLinks lists each link from a block of document fromArtifact to a block of document
// toArtifact (artifact uuids; the two may be one document), newest first, then by the source
// block's node id, then by the target's. A link counts only while both of its blocks are current
// blocks of their documents: a deleted block's links drop out, and its stored mentions bring them
// back when a version holds its id again. A pointer's link counts only while its target has a
// type the pointer allows (block_ref_listed). A link either of whose blocks who may not see
// drops out the same way, since neither end resolves to a node.
func BlockLinks(ctx context.Context, q Queryer, who model.Actor, fromArtifact, toArtifact string) ([]model.BlockLink, error) {
	rows, err := q.Query(ctx, `
		select r.kind, r.from_node, r.to_id, r.created_at, r.created_version
		from block_refs r
		join document_blocks f on f.artifact_id = r.artifact_id and f.block_id = r.block_id
		join document_blocks t on t.node_id = r.to_id and t.artifact_id = $2::uuid
		where r.artifact_id = $1::uuid and r.to_kind = 'block' and (r.to_types is null or block_ref_listed(r.to_types, r.to_id))
		order by r.created_at desc, r.from_node, r.to_id`, fromArtifact, toArtifact)
	if err != nil {
		return nil, fmt.Errorf("list the block links from %s to %s: %w", fromArtifact, toArtifact, err)
	}
	type linkRow struct {
		kind, from, to string
		createdAt      time.Time
		createdVersion int
	}
	var links []linkRow
	var ends []edgeRow
	for rows.Next() {
		var link linkRow
		if err := rows.Scan(&link.kind, &link.from, &link.to, &link.createdAt, &link.createdVersion); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan a block link: %w", err)
		}
		links = append(links, link)
		ends = append(ends, edgeRow{otherKind: "block", otherID: link.from}, edgeRow{otherKind: "block", otherID: link.to})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the block links from %s to %s: %w", fromArtifact, toArtifact, err)
	}
	nodes, err := resolveNodes(ctx, q, who, true, ends)
	if err != nil {
		return nil, err
	}
	result := make([]model.BlockLink, 0, len(links))
	for _, link := range links {
		from, fromFound := nodes[[2]string{"block", link.from}]
		to, toFound := nodes[[2]string{"block", link.to}]
		if !fromFound || !toFound {
			// A block deleted between the two reads: its links drop out as they would have.
			continue
		}
		result = append(result, model.BlockLink{
			Kind:           link.kind,
			From:           from.GraphNode,
			To:             to.GraphNode,
			CreatedAt:      link.createdAt,
			CreatedVersion: link.createdVersion,
		})
	}
	return result, nil
}

// BlockChange is when a block of a document last changed: the version its text last changed in
// and that version's time, as document_blocks records them (IndexBlocks).
type BlockChange struct {
	Version int
	At      time.Time
}

// BlockChanges is each current block of document artifactID, by block id, with when its text last
// changed. It is the document's own half of the out-of-date rule a decision record is read by: a
// decision's changed version and time are the greatest of its blocks', and BlockLinks gives the
// same facts for the blocks it links to. A document the block index has not reached has no rows
// and answers an empty map.
func BlockChanges(ctx context.Context, q Queryer, artifactID string) (map[string]BlockChange, error) {
	rows, err := q.Query(ctx, `
		select block_id, changed_version, changed_at from document_blocks where artifact_id = $1::uuid
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list the block changes of %s: %w", artifactID, err)
	}
	defer rows.Close()
	changes := map[string]BlockChange{}
	for rows.Next() {
		var blockID string
		var change BlockChange
		if err := rows.Scan(&blockID, &change.Version, &change.At); err != nil {
			return nil, fmt.Errorf("scan a block change of %s: %w", artifactID, err)
		}
		changes[blockID] = change
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the block changes of %s: %w", artifactID, err)
	}
	return changes, nil
}

// BlockTargets is where a write to document artifactID lands and the block each of targets names,
// by node id (text.TargetID), as the block index would hold it once that write, leaving the blocks
// written (rendered, in document order, each parent before its children), is indexed, read through
// q: a block of that document as written's node for its id (uniqueBlocks: the first holder of a
// repeated id, and no id the index cannot key), any other as its document's current blocks
// (document_blocks) hold it, with one query for every such target. A target no block has is
// absent. The write lands in the document's own issue, or in none when a project holds it; that
// issue's ancestors are read only once a target turns out to sit in another document
// (IssueAncestors), since a pointer inside the writing document - a plan task's `needs`, the
// common case - is held to nothing above it (pmdoc.CheckBlockPointers), and this runs inside
// the write's transaction.
func BlockTargets(ctx context.Context, q Queryer, artifactID string, written []model.ArtifactBlock, targets []text.Ref) (pmdoc.BlockWriting, map[string]pmdoc.BlockTarget, error) {
	var writing pmdoc.BlockWriting
	if err := q.QueryRow(ctx, `
		select coalesce(ref_key, ''), coalesce(issue_key, '') from artifacts where id = $1
	`, artifactID).Scan(&writing.Document, &writing.Issue); err != nil {
		return pmdoc.BlockWriting{}, nil, fmt.Errorf("read the reference of document %s: %w", artifactID, err)
	}
	nodes, _, _ := uniqueBlocks(written)
	own := make(map[string]string, len(nodes))
	for _, node := range nodes {
		own[node.ID] = node.Type
	}
	found := make(map[string]pmdoc.BlockTarget, len(targets))
	var elsewhere []string
	for _, target := range targets {
		nodeID := text.TargetID(target)
		// A ref_key holds no '#', so the node id's document part ends at its first one.
		if document, _, _ := strings.Cut(nodeID, "#"); document != writing.Document {
			elsewhere = append(elsewhere, nodeID)
		} else if typ, ok := own[target.Block]; ok {
			found[nodeID] = pmdoc.BlockTarget{Types: []string{typ}, Document: writing.Document, Issue: writing.Issue}
		}
	}
	if len(elsewhere) == 0 {
		return writing, found, nil
	}
	rows, err := q.Query(ctx, `
		select d.node_id, d.type, a.ref_key, coalesce(a.issue_key, '')
		from document_blocks d join artifacts a on a.id = d.artifact_id
		where d.node_id = any($1)
	`, elsewhere)
	if err != nil {
		return pmdoc.BlockWriting{}, nil, fmt.Errorf("read the blocks %s points at: %w", artifactID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID, typ, document, issue string
		if err := rows.Scan(&nodeID, &typ, &document, &issue); err != nil {
			return pmdoc.BlockWriting{}, nil, fmt.Errorf("scan a block %s points at: %w", artifactID, err)
		}
		target := found[nodeID]
		target.Types = append(target.Types, typ)
		target.Document, target.Issue = document, issue
		found[nodeID] = target
	}
	if err := rows.Err(); err != nil {
		return pmdoc.BlockWriting{}, nil, err
	}
	// Only a target in another document can sit in another issue, so the ancestor walk is paid
	// for only here.
	if writing.Ancestors, err = IssueAncestors(ctx, q, writing.Issue); err != nil {
		return pmdoc.BlockWriting{}, nil, err
	}
	return writing, found, nil
}

// blockFirstVersion is a left join lateral, aliased alias, over the first version that held block
// block of document artifact (SQL expressions of the outer row): F, the lowest version
// document_block_texts records a text of the block in, as alias.version, and F's authors and
// time as alias.authors and alias.created_at. alias.known is whether Dispatch knows the block was
// made in F: F is 1, or a block-aware build indexed version F-1, which did not hold it. A block
// first seen when its document was first indexed, or after a version no block-aware build
// indexed, may be older than F. A block with no recorded text leaves every column null.
func blockFirstVersion(alias, artifact, block string) string {
	return `left join lateral (
		select t.version,
		       t.version = 1 or exists (
		         select 1 from document_indexed_versions i where i.artifact_id = t.artifact_id and i.version = t.version - 1
		       ) as known,
		       v.authors, v.created_at
		from document_block_texts t
		left join artifact_versions v on v.artifact_id = t.artifact_id and v.number = t.version
		where t.artifact_id = ` + artifact + ` and t.block_id = ` + block + ` and t.text is not null
		order by t.version limit 1
	) ` + alias + ` on true`
}

// blockFirstColumns selects blockFirstVersion's columns, aliased alias, in blockFirst.targets'
// order.
func blockFirstColumns(alias string) string {
	return alias + `.version, ` + alias + `.known, ` + alias + `.authors, ` + alias + `.created_at`
}

// blockFirst is the first version that held a block, as blockFirstColumns selects it.
type blockFirst struct {
	version   *int
	known     *bool
	authors   []byte
	createdAt *time.Time
}

func (f *blockFirst) targets() []any {
	return []any{&f.version, &f.known, &f.authors, &f.createdAt}
}

// maker is who made the block and when: the authors and time of the version that first held it
// when Dispatch knows the block was made there, else nobody recorded at no time. Authors that do
// not decode (storedActors) fail.
func (f blockFirst) maker() ([]model.Actor, *time.Time, error) {
	if f.known == nil || !*f.known {
		return []model.Actor{}, nil, nil
	}
	authors, err := storedActors(f.authors)
	if err != nil {
		return nil, nil, err
	}
	return authors, f.createdAt, nil
}

// RemovedBlockNode is the node nodeID of block blockID of document (its resolved artifact node),
// which no current version holds since version removedVersion deleted it, built as loadBlockNodes
// builds a current block's but with no text or changed version: made as a current block is
// (blockFirstVersion), last changed by that deletion, with its version's authors and time, and
// part of the document at the latest version a block-aware build indexed below removedVersion, the
// last one known to hold it (null when none is). The caller adds its pin. Authors that do not
// decode (storedActors) fail the read.
func RemovedBlockNode(ctx context.Context, q Queryer, document model.GraphNode, nodeID, blockID string, removedVersion int) (model.GraphNode, error) {
	var first blockFirst
	var removedBy []byte
	var removedAt *time.Time
	var held *int
	documentID := document.ID
	if err := q.QueryRow(ctx, `
		select `+blockFirstColumns("first")+`, removed.authors, removed.created_at,
		       (select max(i.version) from document_indexed_versions i where i.artifact_id = b.artifact_id and i.version < $3)
		from (select $1::uuid as artifact_id, $2::text as block_id) b
		`+blockFirstVersion("first", "b.artifact_id", "b.block_id")+`
		left join artifact_versions removed on removed.artifact_id = b.artifact_id and removed.number = $3
	`, documentID, blockID, removedVersion).Scan(append(first.targets(), &removedBy, &removedAt, &held)...); err != nil {
		return model.GraphNode{}, fmt.Errorf("read the facts of removed block %q of %s: %w", blockID, documentID, err)
	}
	madeBy, madeAt, err := first.maker()
	if err != nil {
		return model.GraphNode{}, fmt.Errorf("decode the maker of removed block %q of %s: %w", blockID, documentID, err)
	}
	changedBy, err := storedActors(removedBy)
	if err != nil {
		return model.GraphNode{}, fmt.Errorf("decode the last change of removed block %q of %s: %w", blockID, documentID, err)
	}
	return model.GraphNode{
		Kind:          "block",
		ID:            nodeID,
		BlockID:       blockID,
		IssueKey:      document.IssueKey,
		Project:       document.Project,
		Ref:           BlockRef(document.Ref, blockID),
		Document:      document.Ref,
		MadeBy:        madeBy,
		MadeAt:        madeAt,
		ChangedBy:     changedBy,
		ChangedAt:     removedAt,
		Parent:        &model.NodeParent{Kind: "artifact", ID: documentID, Ref: document.Ref},
		ParentVersion: held,
	}, nil
}

// uniqueBlocks keeps the first block of each id, in order, as the node GET …/blocks addresses by
// that id: a rendered version can repeat an id (pmdoc/block_ids.go), and document_blocks holds one
// row per id. It skips an empty id, and one that is not valid UTF-8 or holds a NUL, which a
// Postgres text[] cannot hold. It skips an id longer than text.MaxBlockIDBytes too, which a
// B-tree key of the index cannot hold, and reports how many blocks it skipped for that and the
// longest such id's bytes.
func uniqueBlocks(blocks []model.ArtifactBlock) (nodes []model.ArtifactBlock, long, longest int) {
	nodes = make([]model.ArtifactBlock, 0, len(blocks))
	seen := make(map[string]struct{}, len(blocks))
	for _, block := range blocks {
		if block.ID == "" || !text.Storable(block.ID) {
			continue
		}
		if len(block.ID) > text.MaxBlockIDBytes {
			long, longest = long+1, max(longest, len(block.ID))
			continue
		}
		if _, dup := seen[block.ID]; dup {
			continue
		}
		seen[block.ID] = struct{}{}
		nodes = append(nodes, block)
	}
	return nodes, long, longest
}

// innermostBlocks names, for each start (ascending), the index of the tightest block whose
// [From, To) holds it, or -1. blocks are in document order, each parent before its children.
func innermostBlocks(blocks []model.ArtifactBlock, starts []int) []int {
	holders := make([]int, len(starts))
	stack := make([]int, 0, 8)
	next := 0
	for i, start := range starts {
		for next < len(blocks) && blocks[next].From <= start {
			for len(stack) > 0 && blocks[stack[len(stack)-1]].To <= blocks[next].From {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, next)
			next++
		}
		for len(stack) > 0 && blocks[stack[len(stack)-1]].To <= start {
			stack = stack[:len(stack)-1]
		}
		holders[i] = -1
		if len(stack) > 0 {
			holders[i] = stack[len(stack)-1]
		}
	}
	return holders
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
