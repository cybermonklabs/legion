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
// '#' and the block's id as the block index keys it (storedBlockID). An id read from the store,
// or from the rendering the index is written from, is already in that form.
func blockNodeAddress(refKey, blockID string) string { return refKey + "#" + blockID }

// BlockAddress is the store's address of block blockID of the document whose ref_key is refKey,
// for an id that may not be in the form the block index keys it - one a request posted, or one a
// caller read off a typed block - read as the index keys it (storedBlockID). It is the one
// address a reader outside this package names a block's node by, so the Keep's lookup of a
// decision and the restore's moved_to tuples name the node the index wrote.
func BlockAddress(refKey, blockID string) string {
	return blockNodeAddress(refKey, storedBlockID(blockID))
}

// storedBlockID is a block's id as the block index keys it: a U+0000 reads as U+FFFD, as the
// rendering writes it (pmdoc.BlockPointers).
func storedBlockID(blockID string) string { return strings.ReplaceAll(blockID, "\x00", "\uFFFD") }

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
// project the row does not hold). supertag is BlockSupertag for every block but a decision and a
// reading, which take their type's (TypedFacts.supertag), so the typed-node pass and this
// index write one node and neither moves what the other wrote.
//
// The index owns a block's text and its place in its document, not its state or its visibility,
// so every node it writes restates the block (graphstore.Node's Restates): state is live for a
// block the version holds and gone for one it dropped, and nothing more. A block already there
// keeps its visibility, and a block the version holds keeps any state a later release's write
// marked it with - a requirement marked proposed whose words a version changes stays proposed -
// while one stored gone, a block a version restores, comes back live. A block the version
// dropped goes gone whatever it was.
func blockNode(address string, record IndexedVersion, at time.Time, state, supertag string) graphstore.Node {
	return graphstore.Node{
		Kind: BlockNodeKind, ID: address, Supertag: supertag,
		Visibility: graphstore.VisibilityProject, State: state,
		Project: record.Project, Parties: record.Parties,
		MadeAt: at, MadeBy: record.Writer, SourceEvent: graphstore.NoEvent,
		Restates: true,
	}
}

// membership is the in_document tuple a block's document_blocks row projects: the block a member
// of the document refKey names, dated joined, the row's created_at, and stated by the version's
// writer.
func membership(address, refKey string, joined time.Time, writer model.Actor) graphstore.UncausedTuple {
	return graphstore.UncausedTuple{Tuple: graphstore.Tuple{
		FromKind: BlockNodeKind, FromID: address, Kind: TupleInDocument,
		ToKind: "artifact", ToID: refKey,
		At: joined, Who: writer, Asserted: true,
	}}
}

// unwritten is nodes without each one written already holds, by kind and id, in their order: one
// write of the store takes each node once, and a caller's node beside a pass (VersionBlocks.Beside
// and PlanDecisionNodes' beside) can be one the pass writes itself - a person who both introduced
// a decision and removed an ask.
func unwritten(nodes, written []graphstore.Node) []graphstore.Node {
	if len(nodes) == 0 {
		return nil
	}
	held := make(map[[2]string]struct{}, len(written)+len(nodes))
	for _, node := range written {
		held[[2]string{node.Kind, node.ID}] = struct{}{}
	}
	kept := make([]graphstore.Node, 0, len(nodes))
	for _, node := range nodes {
		key := [2]string{node.Kind, node.ID}
		if _, already := held[key]; already {
			continue
		}
		held[key] = struct{}{}
		kept = append(kept, node)
	}
	return kept
}

// BlockInDocument is block blockID's node, live, and its in_document tuple, its membership of the
// document record names, both as the index pass states them (blockNode, membership): in the
// document's project or between its parties, made, dated and stated by record's writer at at, and
// caused by event. fits is false, and the two are zero, for a block whose address is past
// MaxBlockNodeBytes, which is no node of the store. It is the one statement of a block's node and
// its membership for a writer that holds its cause already: the backfill of the block rows
// written before the store (graphcensus), under the event that recorded each row's version.
func BlockInDocument(record IndexedVersion, blockID string, at time.Time, event int64) (node graphstore.Node, member graphstore.Tuple, fits bool) {
	if !blockIDFits(record.RefKey, blockID) {
		return graphstore.Node{}, graphstore.Tuple{}, false
	}
	address := blockNodeAddress(record.RefKey, blockID)
	node = blockNode(address, record, at, graphstore.StateLive, BlockSupertag)
	node.SourceEvent = event
	member = membership(address, record.RefKey, at, record.Writer).Tuple
	member.SourceEvent = event
	return node, member, true
}

// ReadIndexedVersion is what the version's own row and its document's row say of the pass
// (IndexedVersion), read through q: the document's ref_key, the first actor the version's row
// records as its writer, the project the document belongs to or the session whose conversation
// holds it, and the issue that holds it.
func ReadIndexedVersion(ctx context.Context, q Queryer, artifactID string, version int) (IndexedVersion, error) {
	var record IndexedVersion
	var project, session, issue *string
	var authors []byte
	if err := q.QueryRow(ctx, `
		select a.ref_key, a.project_key, a.session_id, a.issue_key, v.authors
		from artifacts a
		left join artifact_versions v on v.artifact_id = a.id and v.number = $2
		where a.id = $1::uuid
	`, artifactID, version).Scan(&record.RefKey, &project, &session, &issue, &authors); err != nil {
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
	if issue != nil {
		record.Issue = *issue
	}
	writers, err := storedActors(authors)
	if err != nil {
		return IndexedVersion{}, fmt.Errorf("decode the authors of version %d of %s: %w", version, artifactID, err)
	}
	if len(writers) > 0 {
		record.Writer = writers[0]
	}
	return record, nil
}

// nodelessBlock is one block of the version whose document_blocks row names no node: its id, its
// type, which the node it takes is tagged by (TypedFacts.supertag), the time that row was written,
// which the membership PlanBlocks states for it is dated from, and whether its address fits the
// store's key, as blockFits answered it for the version's blocks.
type nodelessBlock struct {
	id, typ string
	joined  time.Time
	fits    bool
}

// planBlockNodes is the step of a pass that plans the nodes it states and the in_document tuples
// of the blocks that join the document (membership), as the start of what the pass states
// (BlockIndexing.stated), from what planBlockTexts found of the version's blocks (texts): a node
// for each block whose row the pass writes, each block the version deleted and each block whose
// row names no node, each tagged as version.Typed says (TypedFacts.supertag), with the
// typed-node pass's own nodes of the version (TypedFacts.indexedFacts) and the nodes the caller
// writes beside the pass (version.Beside) after them. Where the store's statement of the
// document is stale it reads that statement (heldDocumentBlocks) and answers it, for the link
// step to compare the version's pairs with (unheldLinks), and it answers nil elsewhere. Beside the
// nodes it plans the retraction of the copied_from each block the version no longer holds states
// (heldCopiedFrom). It answers the typed-node pass's own tuples of the version too, which
// PlanBlocks appends after the links. now is the transaction's now(), the time a row the pass
// inserts is dated by.
func (b *BlockIndexing) planBlockNodes(
	ctx context.Context,
	q Queryer,
	stale bool,
	nodes []model.ArtifactBlock,
	texts blockTexts,
	version VersionBlocks,
	now time.Time,
) (map[string][]heldLink, []graphstore.Tuple, error) {
	refKey, versionAt := b.refKey, b.versionAt
	record, typed, writer := version.Record, version.Typed, version.Record.Writer
	// The nodes this pass states are the blocks whose document_blocks row it writes - the new,
	// the changed and the restored - the ids the version dropped, and the blocks whose row
	// names no node, and no other. The first two are exactly the set R16's foreign key needs a
	// node for where every row of the document already names one, and that is what makes the
	// write cost what the edit changed rather than what the document holds: a version that
	// moves one word of a two-thousand-block document states two node rows, not four thousand.
	// A block whose text this version left alone already has its node from the version that
	// first indexed it, and its placement, which may still have moved, is a tuple
	// planBlockParents states against that node.
	//
	// Every node the pass writes restates its block (blockNode): a block this version holds is
	// written live and keeps a state another write marked it with, so a version that changes a
	// proposed requirement's words leaves it proposed, and comes back live only from gone.
	blockNodes := make([]graphstore.Node, 0, len(b.ids)+len(b.deleted)+len(texts.nodeless))
	for position, id := range b.ids {
		if !texts.written[position].fits {
			continue
		}
		blockNodes = append(blockNodes, blockNode(blockNodeAddress(refKey, id), record, versionAt, graphstore.StateLive, typed.supertag(id, b.types[position])))
	}
	// Where the store's statement of the document is stale, a block another release's version
	// dropped has no row left for deleted to name, and its node stands: the store's own
	// statement of the document names it (goneSinceStated), and it is stated gone by the rule
	// below. Every address the store holds is inside the bound, since a block past it is no node.
	var statement map[string][]heldLink
	var sinceStated []string
	if stale {
		var err error
		if statement, err = heldDocumentBlocks(ctx, q, refKey); err != nil {
			return nil, nil, err
		}
		sinceStated = goneSinceStated(statement, nodes, b.deleted)
	}
	// A block the version no longer holds has its node stated gone, whatever state it held: a
	// restating write that says gone is written gone, and keeps only the visibility. The tuple
	// that says so is not built here: the node write states a base attribute it moved, with this
	// write's own time and writer, so a block restored by a later version is stated live by the
	// same rule and no writer of this store states a base attribute of its own. Its node keeps
	// the supertag it holds, which no version holding the block is left to say. A deleted block
	// is none of the version's blocks, so blockFits answered nothing of it, and its address is
	// measured here, once.
	gone := b.deleted
	if len(sinceStated) > 0 {
		gone = slices.Concat(b.deleted, sinceStated)
	}
	supertags, err := heldSupertags(ctx, q, refKey, gone)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range gone {
		if !blockIDFits(refKey, id) {
			continue
		}
		supertag, known := supertags[id]
		if !known {
			supertag = BlockSupertag
		}
		blockNodes = append(blockNodes, blockNode(blockNodeAddress(refKey, id), record, versionAt, graphstore.StateGone, supertag))
	}
	// A block the version no longer holds is no copy of an ask either, so the copied_from its node
	// states (docs' claimCopies) is retracted at this version's time and by its writer, as the
	// tuple of a link the version drops is (planBlockRefs). The settlement that claims copies reads
	// which blocks hold one through the document's rows, and this write deletes the dropped block's
	// row, so no later write would end the relation.
	copies, err := heldCopiedFrom(ctx, q, refKey, gone)
	if err != nil {
		return nil, nil, err
	}
	// A row written before this release names no node: migration 20261109022248 added the
	// columns and filled none of them, and the backfill has not reached the document or has
	// not been run at all. The parents' projection names this block's node on every row of the
	// document whose columns differ, and document_blocks_node is checked inside that statement,
	// so each of those rows takes its node here or that update is refused. The membership goes
	// with it: the insert states one only for a row it truly inserts, so a pre-store block whose
	// text this version changed would otherwise keep its row, state no in_document, and be
	// skipped by the backfill for naming a node by then.
	//
	// Each membership is dated from its row's own created_at, as a row the insert writes afresh
	// is, so the tuple arm of graph_edges answers what the document_blocks arm answered for it. A
	// block past the address bound is no node of the store: it takes neither, and its row keeps
	// its null columns, which is what keeps the old arm answering it.
	var memberships []graphstore.UncausedTuple
	for _, block := range texts.nodeless {
		if !block.fits {
			continue
		}
		address := blockNodeAddress(refKey, block.id)
		if _, stated := texts.changed[block.id]; !stated {
			blockNodes = append(blockNodes, blockNode(address, record, versionAt, graphstore.StateLive, typed.supertag(block.id, block.typ)))
		}
		memberships = append(memberships, membership(address, refKey, block.joined, writer))
	}
	// The typed-node pass's own nodes of the version, and its own facts of them, which go in one
	// write with the block index's (TypedFacts.indexedFacts): a node the index writes already is
	// not written twice.
	typedNodes, typedTuples := typed.indexedFacts(versionAt, blockNodes)
	indexed := slices.Concat(blockNodes, typedNodes)
	// The nodes go in before any projection row that names one: the foreign key is checked
	// inside the statement writing that row, so a node written afterwards would be too late.
	// The base attributes that write states are appended with the pass's own tuples under the
	// caller's event (graphstore.Uncaused), as the first of them. They carry no
	// legacy_source_seq: no legacy row states a node's visibility or its state.
	b.stated = graphstore.PlanUncaused(append(indexed, unwritten(version.Beside.Nodes, indexed)...), nil)
	b.stated.Tuples = append(b.stated.Tuples, memberships...)
	// A block whose row the insert writes afresh joins the document there; one whose row was
	// already there - a block whose text alone changed - keeps that row and the membership it
	// already states.
	for position, id := range b.ids {
		if written := texts.written[position]; !written.inserted || !written.fits {
			continue
		}
		b.stated.Tuples = append(b.stated.Tuples, membership(blockNodeAddress(refKey, id), refKey, now, writer))
	}
	for _, id := range gone {
		for _, ask := range copies[id] {
			b.stated.Tuples = append(b.stated.Tuples, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
				FromKind: BlockNodeKind, FromID: blockNodeAddress(refKey, id), Kind: graphstore.TupleCopiedFrom,
				ToKind: graphstore.KindAsk, ToID: ask, At: versionAt, Who: writer, Asserted: false,
			}})
		}
	}
	return statement, typedTuples, nil
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
//
// askParents is each reading of the version that stands in an answered ask, by its id, with that
// ask's id (DecisionFacts): its part_of is the ask's node, since the decision a reading there
// reads is the person's answer the node holds, while document_blocks.parent_block_id keeps the
// id of the ask block holding it, which is that tuple's projection. only, where it is not nil,
// is the blocks the pass states a parent for, the typed-node pass's own above the deferral bound
// (PlanDecisionNodes); every block is placed in the version all the same, so the parent each of
// them takes is the one a pass over the whole version gives it.
func (b *BlockIndexing) planBlockParents(
	ctx context.Context,
	q Queryer,
	index BlockIndex,
	nodes []model.ArtifactBlock,
	fits []bool,
	askParents map[string]string,
	only map[string]struct{},
	writer model.Actor,
) ([]graphstore.UncausedTuple, map[string]blockFarEnd, error) {
	placement := maps.Clone(index.placed)
	if len(nodes) == 0 {
		return nil, placement, nil
	}
	placed, parents := placeBlocks(nodes, fits)
	if only != nil {
		kept := 0
		for position, block := range placed {
			if _, own := only[nodes[block].ID]; own {
				placed[kept], parents[kept] = block, parents[position]
				kept++
			}
		}
		placed, parents = placed[:kept], parents[:kept]
	}
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
	// The ask whose node a reading in an answered ask takes for its parent, by place, nil for
	// every other block, which the probe compares in place of the block holding it.
	var asks []*string
	if len(askParents) > 0 {
		asks = make([]*string, len(placed))
	}
	for position, block := range placed {
		b.placedIDs[position] = nodes[block].ID
		wanted[position] = blockFarEnd{kind: "artifact", id: refKey}
		if parents[position] >= 0 {
			b.placedParents[position] = &nodes[parents[position]].ID
			wanted[position] = blockFarEnd{kind: BlockNodeKind, id: blockNodeAddress(refKey, nodes[parents[position]].ID)}
		}
		if ask, held := askParents[nodes[block].ID]; held {
			asks[position] = &ask
			wanted[position] = blockFarEnd{kind: graphstore.KindAsk, id: ask}
		}
	}
	// Each placed block's difference, by its place among them: the ones an earlier indexing of
	// this transaction placed, compared here, and the rest read from the store in one probe.
	differences := make([]*blockParentDifference, len(placed))
	var readIDs []string
	var readParents, readAsks []*string
	var readAt []int
	for position, id := range b.placedIDs {
		far, known := index.placed[id]
		if !known {
			readIDs, readParents, readAt = append(readIDs, id), append(readParents, b.placedParents[position]), append(readAt, position)
			if asks != nil {
				readAsks = append(readAsks, asks[position])
			}
			continue
		}
		if far != wanted[position] {
			differences[position] = &blockParentDifference{block: position, retract: []blockFarEnd{far}}
		}
	}
	if len(readIDs) > 0 {
		read, err := heldBlockParentDifferences(ctx, q, refKey, readIDs, readParents, readAsks)
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

// planBlockRefs plans the reconciliation of the document's block_refs with the indexing's pairs,
// against the links index holds, and answers what it states in the store, with the links the
// index holds once it is written. It adds to the indexing's moved targets the targets of the
// links it drops and inserts, of those whose block types it changes and of those whose node it
// moves.
//
// Beside them it states what each row it inserts or drops is the projection of: one tuple out of
// the node the row hangs on (blockPair.from) - its block's, or for an ask block's shaped the ask's
// - under that pair's own kind - a pointer attribute's name, or `mentions` for a block's mention -
// asserted for each pair the insert inserts and retracted for each pair the delete drops, so the
// tuple and the row arrive and leave together. A surviving pair earns none while its row names the
// node it hangs on: it keeps its dates and its source event and takes this version's to_types, and
// a tuple appended for it would carry this version's time and win the current read over the one
// that matches the row.
//
// The insert writes a row's node columns where it inserts the row: a pair that names a node
// of its own (blockPair.nodeKind) writes that node into the row, which marks it. A surviving row
// that names no node - a pair a build before the store wrote, or one the release before this one
// inserted after this build's mark was set or after the store last stated the document - takes
// its node, its pair's own or else its block's, in a statement of its own wherever such a row may
// be another release's (BlockIndexing.namesSurvivingRows): on the drain of this build's mark, and
// where the pass reads the store's statement of the document as stale. It takes the one tuple that
// row projects, dated, written and caused as the tuple of a pair the insert inserts. That
// statement names only rows that name no node, so a surviving pair whose row names its node still
// earns no tuple. Any other pass leaves such a row its nulls, which the old block_refs arm
// answers, until the backfill names it and marks the document.
//
// A surviving pair whose row names a node other than the one the pair hangs on now moves to that
// node (rehome): a block retyped between an ask and a decision keeps its id and its tie, and an
// ask block's shaped hangs on the ask's node where a decision's hangs on the block's own. Its
// tuple leaves the node the row names and is stated out of the pair's, and the row is renamed to
// it (queueBlockRefs), so the row and the tuple move together. A row that names no node because
// its block's address is past the store's bound, whose pair now hangs on a node of its own inside
// it, is named with that node in any pass, and its tuple stated, as the insert would (rehome).
//
// A scope (BlockIndexing.scope) limits the rows the reconcile drops to those it reaches: every
// other link the index holds is held as it stands, whatever the pairs hold, so a pass that read
// only its own ties drops nothing it did not read.
func (b *BlockIndexing) planBlockRefs(ctx context.Context, q Queryer, index BlockIndex, writer model.Actor) ([]graphstore.UncausedTuple, []indexedLink, error) {
	written := make(map[blockPairKey]struct{}, len(b.pairs))
	for _, pair := range b.pairs {
		written[pair.key()] = struct{}{}
	}
	indexed := make(map[blockPairKey]indexedLink, len(index.links))
	var tuples []graphstore.UncausedTuple
	var kept []indexedLink
	for _, link := range index.links {
		indexed[link.key()] = link
		if _, still := written[link.key()]; still {
			continue
		}
		if !b.scope.reaches(link.blockPair) {
			kept = append(kept, link)
			continue
		}
		b.moved = append(b.moved, link.target)
		fromKind, address := link.from(b.refKey)
		// A mention's or a pointer's far end is a reference target, which the block's row holds
		// up to text.MaxTargetBytes but the store keys whole beside the block's own address: a
		// tuple whose far end is past graphstore.MaxAddressBytes was never written, so its
		// retraction is not written either, and the row kept its null node columns (the insert
		// below), which the old block_refs arm answers.
		if !graphstore.AddressFits(address) || !graphstore.AddressFits(link.target.ID) {
			continue
		}
		// The row is gone, so no source_seq is ever written for it and the retraction carries
		// none: its time is this version's, the moment the pair stopped being held.
		tuples = append(tuples, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
			FromKind: fromKind, FromID: address, Kind: link.kind,
			ToKind: link.target.Kind, ToID: link.target.ID,
			At: b.versionAt, Who: writer, Asserted: false,
		}})
	}
	// On the document's first indexing a new pair dates from the document's own refs row for its
	// target, when one exists, as the insert dates its row: the read is of the rows that insert
	// joins, which no statement of this write changes before it.
	var cited map[Target]citedRef
	if b.firstIndexing && len(b.pairs) > 0 {
		var err error
		if cited, err = documentRefs(ctx, q, b.artifactID); err != nil {
			return nil, nil, err
		}
	}
	links := make([]indexedLink, 0, len(kept)+len(b.pairs))
	links = append(links, kept...)
	for _, pair := range b.pairs {
		link, already := indexed[pair.key()]
		if already {
			var moved []graphstore.UncausedTuple
			link, moved = b.rehome(link, pair, writer)
			tuples = append(tuples, moved...)
		}
		if already && slices.Equal(link.toTypes, pair.toTypes) {
			links = append(links, link)
			continue
		}
		b.moved = append(b.moved, pair.target)
		if already {
			link.toTypes = pair.toTypes
			links = append(links, link)
			continue
		}
		_, address := pair.from(b.refKey)
		fits := graphstore.AddressFits(address) && graphstore.AddressFits(pair.target.ID)
		link = indexedLink{blockPair: pair, createdAt: b.versionAt, named: fits}
		if ref, found := cited[pair.target]; found {
			link.createdAt, link.sourceSeq = ref.createdAt, ref.sourceSeq
		}
		links = append(links, link)
		if fits {
			tuples = append(tuples, assertedLink(b.refKey, link, writer))
		}
	}
	if !b.namesSurvivingRows {
		return tuples, links, nil
	}
	// The pairs whose row survived naming no node. Every block a pair is held by is a block of
	// the version whose node this pass wrote or found named on its document_blocks row, so the
	// foreign key on the columns finds that node, and a pair that names a node of its own names
	// that node, as the insert would; a block or a far end past the address bound is no node of
	// the store, and its row keeps its nulls, as the insert leaves them. Naming the row moves no
	// count: the old arm answered the row, and the tuple arm answers the same link once the row
	// names its node. The links after kept are the pairs', one for each, in their order.
	for position, pair := range b.pairs {
		link := &links[len(kept)+position]
		if _, survives := indexed[link.key()]; !survives || link.named {
			continue
		}
		if _, address := pair.from(b.refKey); !graphstore.AddressFits(address) || !graphstore.AddressFits(link.target.ID) {
			continue
		}
		link.nodeKind, link.nodeAddress, link.named = pair.nodeKind, pair.nodeAddress, true
		tuples = append(tuples, assertedLink(b.refKey, *link, writer))
	}
	return tuples, links, nil
}

// rehome moves link, the row a surviving pair holds, to the node pair hangs on, where that is not
// the node the row names, and answers the link as the index holds it after, with what the move
// states: a retraction of the row's tuple out of the node it named, at this version's time, and
// the same tuple out of pair's node, dated, written and caused as the tuple of a row the insert
// inserts (assertedLink). The row takes pair's node, or none where that node's address is past the
// store's bound, as the insert leaves such a row (queueBlockRefs), and its target is moved, since
// the node that cites it is another.
//
// A row that names no node has no tuple to move. It names none because its block's address or its
// far end was past the store's bound when it was inserted, or because another release or a build
// before the store wrote it. Where its block's address is past the bound and pair now hangs on a
// node of its own whose address and far end fit - a decision of a long-named document retyped to
// an ask block that keeps its id and its shaped, which hangs on the ask's node - the row is named
// with that node and its tuple is stated, as the insert of the same pair names the row and states
// it, and its target is moved: otherwise only a pass that names surviving rows
// (namesSurvivingRows), which few passes are, would name it, and the ask's node would state no tie
// until then. Every other row that names no node is left to that pass, or to the old block_refs
// arm.
func (b *BlockIndexing) rehome(link indexedLink, pair blockPair, writer model.Actor) (indexedLink, []graphstore.UncausedTuple) {
	heldKind, held := link.from(b.refKey)
	wantedKind, wanted := pair.from(b.refKey)
	if !link.named {
		if graphstore.AddressFits(held) || !graphstore.AddressFits(wanted) || !graphstore.AddressFits(link.target.ID) {
			return link, nil
		}
		link.nodeKind, link.nodeAddress, link.named = pair.nodeKind, pair.nodeAddress, true
		b.rehomed = append(b.rehomed, link)
		b.moved = append(b.moved, link.target)
		return link, []graphstore.UncausedTuple{assertedLink(b.refKey, link, writer)}
	}
	if heldKind == wantedKind && held == wanted {
		return link, nil
	}
	stated := []graphstore.UncausedTuple{{Tuple: graphstore.Tuple{
		FromKind: heldKind, FromID: held, Kind: link.kind,
		ToKind: link.target.Kind, ToID: link.target.ID,
		At: b.versionAt, Who: writer, Asserted: false,
	}}}
	link.nodeKind, link.nodeAddress = pair.nodeKind, pair.nodeAddress
	link.named = graphstore.AddressFits(wanted)
	if link.named {
		stated = append(stated, assertedLink(b.refKey, link, writer))
	}
	b.rehomed = append(b.rehomed, link)
	b.moved = append(b.moved, link.target)
	return link, stated
}

// citedRef is when the document's own refs row for a target was written and the source event
// it stands at, which a new pair of the document's first indexing takes.
type citedRef struct {
	createdAt time.Time
	sourceSeq *int64
}

// documentRefs reads the document's own refs rows, by target, as the insert of a first
// indexing's pairs joins them.
func documentRefs(ctx context.Context, q Queryer, artifactID string) (map[Target]citedRef, error) {
	rows, err := q.Query(ctx, `
		select to_kind, to_id, created_at, source_seq from refs
		where from_kind = 'artifact' and from_id = $1::uuid::text
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the references of %s: %w", artifactID, err)
	}
	defer rows.Close()
	cited := map[Target]citedRef{}
	for rows.Next() {
		var target Target
		var ref citedRef
		if err := rows.Scan(&target.Kind, &target.ID, &ref.createdAt, &ref.sourceSeq); err != nil {
			return nil, fmt.Errorf("scan a reference of %s: %w", artifactID, err)
		}
		cited[target] = ref
	}
	return cited, rows.Err()
}

// assertedLink is the tuple a block_refs row the pass writes or names is the projection of: the
// link out of the node the row names (blockPair.from) under the pair's own kind, dated from the
// row's created_at and stated by the version's writer. Its legacy_source_seq is the row's
// source_seq, and a row that holds none takes the cause's (LegacyFromCause): outside a first
// indexing the insert leaves source_seq null and the version's event fills it in this same
// transaction.
func assertedLink(refKey string, link indexedLink, writer model.Actor) graphstore.UncausedTuple {
	fromKind, address := link.from(refKey)
	return graphstore.UncausedTuple{
		Tuple: graphstore.Tuple{
			FromKind: fromKind, FromID: address, Kind: link.kind,
			ToKind: link.target.Kind, ToID: link.target.ID,
			At: link.createdAt, Who: writer, LegacySourceSeq: link.sourceSeq, Asserted: true,
		},
		LegacyFromCause: true,
	}
}

// queueBlockRefs queues the reconciliation of the document's block_refs with the indexing's pairs,
// matching on the five columns of its key (planBlockRefs planned what it states): the delete of
// the pairs the version drops, within the indexing's scope where it has one, the insert of those
// it adds, which names a row's node - its pair's own, or else its block's - only where it inserts
// the row, the move of each surviving row whose pair hangs on another node now (queueRehomed),
// and, where the rows may be another release's (namesSurvivingRows: on the drain of this build's
// mark and where the store's statement is stale), the naming of each surviving row that names
// none.
func (b BlockIndexing) queueBlockRefs(batch *store.WriteBatch) {
	pairs := b.pairs
	blockIDs := make([]string, len(pairs))
	kinds := make([]string, len(pairs))
	toKinds := make([]string, len(pairs))
	toIDs := make([]string, len(pairs))
	// A pair's to_types travel as one space-joined text, null for no limit: a block type's name is
	// a directive name (pmdoc), which holds no space.
	toTypes := make([]*string, len(pairs))
	// The node a pair names of its own, null for the block's.
	nodeKinds := make([]*string, len(pairs))
	nodeAddresses := make([]*string, len(pairs))
	for i, pair := range pairs {
		blockIDs[i], kinds[i], toKinds[i], toIDs[i] = pair.blockID, pair.kind, pair.target.Kind, pair.target.ID
		if len(pair.toTypes) > 0 {
			joined := strings.Join(pair.toTypes, " ")
			toTypes[i] = &joined
		}
		if pair.nodeKind != "" {
			nodeKinds[i], nodeAddresses[i] = &pair.nodeKind, &pair.nodeAddress
		}
	}
	var scopeKinds, scopeBlocks []string
	if b.scope != nil {
		scopeKinds, scopeBlocks = b.scope.kinds, b.scope.blocks
	}
	batch.Queue(fmt.Sprintf("delete the block mentions version %d of %s dropped", b.version, b.artifactID), `
		delete from block_refs r
		where r.artifact_id = $1 and not exists (
			select 1 from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
			where p.block_id = r.block_id and p.kind = r.kind and p.to_kind = r.to_kind and p.to_id = r.to_id)
		  and ($6::text[] is null or (r.kind = any($6::text[]) and r.block_id = any($7::text[])))
	`, b.artifactID, blockIDs, kinds, toKinds, toIDs, scopeKinds, scopeBlocks)
	if len(pairs) == 0 {
		return
	}
	// A surviving pair takes this version's to_types, so a change of an attribute's targets
	// reaches it here; PlanBlocks named the targets it moves. Adding the node columns to the
	// conflict update would rewrite a surviving row no version changed, so a surviving row that
	// names no node learns its node in the statement after this one, and only where the pass names
	// surviving rows (namesSurvivingRows).
	batch.Queue(fmt.Sprintf("record the block mentions of version %d of %s", b.version, b.artifactID), `
		insert into block_refs (artifact_id, block_id, from_node, node_kind, node_address, to_kind, to_id, kind, to_types, created_version, created_at, source_seq)
		select $1::uuid, p.block_id, a.ref_key || '#' || p.block_id,
		       case when octet_length(p.to_id) > $11 then null
		            when p.node_kind is not null then p.node_kind
		            when octet_length(a.ref_key || '#' || p.block_id) <= $11 then $10 end,
		       case when octet_length(p.to_id) > $11 then null
		            when p.node_kind is not null then p.node_address
		            when octet_length(a.ref_key || '#' || p.block_id) <= $11 then a.ref_key || '#' || p.block_id end,
		       p.to_kind, p.to_id, p.kind, string_to_array(p.to_types, ' '), $6,
		       coalesce(case when $8 then r.created_at end, $7), case when $8 then r.source_seq end
		from unnest($2::text[], $3::text[], $4::text[], $5::text[], $9::text[], $12::text[], $13::text[])
		     as p (block_id, kind, to_kind, to_id, to_types, node_kind, node_address)
		cross join (select ref_key from artifacts where id = $1::uuid) a
		left join refs r on r.from_kind = 'artifact' and r.from_id = $1::uuid::text
		                and r.to_kind = p.to_kind and r.to_id = p.to_id
		on conflict (artifact_id, block_id, to_kind, to_id, kind) do update set to_types = excluded.to_types
		where block_refs.to_types is distinct from excluded.to_types
	`, b.artifactID, blockIDs, kinds, toKinds, toIDs, b.version, b.versionAt, b.firstIndexing, toTypes, BlockNodeKind, MaxBlockNodeBytes,
		nodeKinds, nodeAddresses)
	b.queueRehomed(batch)
	if !b.namesSurvivingRows {
		return
	}
	batch.Queue(fmt.Sprintf("name the nodes of the block mentions of %s that name none at version %d", b.artifactID, b.version), `
		update block_refs r
		set node_kind = coalesce(p.node_kind, $6), node_address = coalesce(p.node_address, $7 || '#' || r.block_id)
		from unnest($2::text[], $3::text[], $4::text[], $5::text[], $9::text[], $10::text[])
		     as p (block_id, kind, to_kind, to_id, node_kind, node_address)
		where r.artifact_id = $1 and r.block_id = p.block_id and r.kind = p.kind
		  and r.to_kind = p.to_kind and r.to_id = p.to_id and r.node_kind is null
		  and (p.node_kind is not null or octet_length($7 || '#' || r.block_id) <= $8)
		  and octet_length(r.to_id) <= $8
	`, b.artifactID, blockIDs, kinds, toKinds, toIDs, BlockNodeKind, b.refKey, MaxBlockNodeBytes, nodeKinds, nodeAddresses)
}

// queueRehomed queues the move of each surviving row whose pair hangs on another node now
// (rehome) to that node, or to none where that node's address is past the store's bound, matching
// on the five columns of the row's key.
func (b BlockIndexing) queueRehomed(batch *store.WriteBatch) {
	if len(b.rehomed) == 0 {
		return
	}
	blockIDs := make([]string, len(b.rehomed))
	kinds := make([]string, len(b.rehomed))
	toKinds := make([]string, len(b.rehomed))
	toIDs := make([]string, len(b.rehomed))
	nodeKinds := make([]*string, len(b.rehomed))
	nodeAddresses := make([]*string, len(b.rehomed))
	for i, link := range b.rehomed {
		blockIDs[i], kinds[i], toKinds[i], toIDs[i] = link.blockID, link.kind, link.target.Kind, link.target.ID
		if link.named {
			kind, address := link.from(b.refKey)
			nodeKinds[i], nodeAddresses[i] = &kind, &address
		}
	}
	batch.Queue(fmt.Sprintf("move the block mentions of version %d of %s to the node each hangs on now", b.version, b.artifactID), `
		update block_refs r
		set node_kind = p.node_kind, node_address = p.node_address
		from unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[])
		     as p (block_id, kind, to_kind, to_id, node_kind, node_address)
		where r.artifact_id = $1 and r.block_id = p.block_id and r.kind = p.kind
		  and r.to_kind = p.to_kind and r.to_id = p.to_id
	`, b.artifactID, blockIDs, kinds, toKinds, toIDs, nodeKinds, nodeAddresses)
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
