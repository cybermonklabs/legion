package refs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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

// BlockRefKindsSQL is a select of every tuple kind IndexBlocks writes into block_refs, one row
// each, so of every kind of tuple out of a block that a block_refs row is the projection of: a
// block's mention (blockPairs) and the name of every `block[]` attribute the store's type nodes
// declare, the kind of each pointer that attribute makes (pointerPairs: a pointer's kind is its
// attribute's name as a document writes it, pmdoc.BlockPointer's Attribute, and never its
// store_name). It reads the store's attribute nodes rather than the schema a process installed,
// so it answers the same for every build reading one store: a build older than the store installs
// no attribute whose `since` is above it (pmdoc.Install), and an attribute node stated gone is
// installed by none, while the block_refs rows of its kind stand until a version re-indexes their
// documents. So an attribute node is read at any state. A tuple of any other kind out of a block -
// its placement, in_document and part_of, or one another writer states of it - is no part of that
// projection, and the store census holds the block_refs rows to the tuples of these kinds alone
// (graphcensus).
const BlockRefKindsSQL = `select 'mentions'
		union select attribute.attributes->>'name' from graph_nodes attribute
		where attribute.kind = 'attribute' and attribute.attributes->>'kind' = '` + string(pmdoc.BlockAttributeBlockList) + `'`

// Version is one version of a document a block index pass covers: its number and the created_at
// of its row.
type Version struct {
	Number int
	At     time.Time
}

// Versions is the version of each document one block index pass covers, by the document's
// artifact id (IndexBlocks). A pass over a document that is its own room covers one version of
// that document (OneVersion). A pass over an issue room, which indexes the room's blocks under its
// tree artifact, covers a version of each document whose fragment of the room holds its blocks -
// the version the room's settlement wrote of that document, or the newest it holds - and one of
// the tree artifact itself for the blocks of its own fragment (T12a of dispatch://INFRA-1253's
// plan): a block of the room records a version of its own document, never a number of the tree
// artifact's history, so two documents of one room can each hold a block at the same number.
type Versions map[string]Version

// OneVersion is the Versions of a pass over version number of document artifactID alone, written
// at at.
func OneVersion(artifactID string, number int, at time.Time) Versions {
	return Versions{artifactID: {Number: number, At: at}}
}

// Newest is the document and the version of v written last: the version a pass over v states the
// facts that name no document of their own in the name of and at the time of (IndexBlocks), and
// whose first author is the pass's writer (indexedVersionRecord). Of versions written at one time,
// artifactID's own comes first, then the document of the lowest id, so the choice does not
// depend on the map's order. An empty v answers artifactID at no version.
func (v Versions) Newest(artifactID string) (string, Version) {
	before := func(document, lead string) bool {
		return document == artifactID || lead != artifactID && document < lead
	}
	lead, version, found := artifactID, Version{}, false
	for document, candidate := range v {
		if !found || candidate.At.After(version.At) || candidate.At.Equal(version.At) && before(document, lead) {
			lead, version, found = document, candidate, true
		}
	}
	return lead, version
}

// IndexBlocks records the versions of versions of document artifactID's blocks as graph nodes,
// and each mention written inside a block as an edge out of the innermost block holding its
// start, in the caller's transaction, which holds the document's owner row. versions is the one
// version a pass over a document indexes (OneVersion), or, for an issue room indexed under its
// tree artifact artifactID, the version of each document whose fragment holds its blocks
// (Versions): each block records the version of its own document, the document
// model.ArtifactBlock's Document names, or artifactID's own for a block of no other. blocks
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
// or restored block takes the version its text stands since (textSince) - this version, or where
// the pass covers versions no pass indexed, the earliest of them from which its text stood - and
// its text is recorded in document_block_texts at that version; a block the version no longer
// holds is deleted, recorded there with no text, its node's state stated gone, and the edges at
// it stop resolving until a version holds its id again. Such a block also takes its pointer tuples
// with it: each prevented_by of a risk it covered as a check, and each constrained_by it held as a
// requirement, is retracted beside the facts the pass states (retractGonePointers), and a version
// that restores the block restores none of them. A block the version holds keeps the state and
// the visibility its node has, unless it was gone, when it comes back live (blockNode restates
// every node it writes). A block's stored excerpt is its text cut to BlockExcerptRunes, the
// excerpt a document's mention has shown since migration 0092.
//
// A pair (block, kind, target) that survives keeps its created version, time and source event,
// and takes the to_types this version gives it. A new pair takes its block's version and that
// version's time, and the source event Stamp gives it. On the document's first indexing (no block
// rows yet), a new pair dates instead from the document's own refs row for that target, when one
// exists. Each version is recorded as indexed under its own document (document_indexed_versions),
// and as the version that document's blocks stand at (document_block_versions), a mark that only
// ever moves forward: the second result is false where this pass lost that race to one holding
// the same versions or higher ones (standsAtVersions), and such a pass writes nothing at all - no
// projection row and no tuple - since what it would state is what the winning pass already
// stated. It returns the targets of the pairs
// it inserted or deleted, or whose to_types it changed, whose counts moved: a node is counted
// once for each block citing it. It also returns what else a Referenced by panel or a link
// shows that the version moved: the target of each pair it kept in a block whose text it
// changed (the block's text is that link's excerpt), and each block it changed or deleted that
// some source cites (citedBlocks), whose text every link to it and its own panel show.
//
// Its third result is what the pass states in the node and tuple store, held for the event that
// caused the write (graphstore.Uncaused): the block nodes it wrote, and the tuples the projection
// rows beside them are the projection of. The nodes are written by the pass itself, because the
// foreign key on (node_kind, node_address) is checked inside each statement that writes a
// projection row and there is no node to name otherwise; the tuples are not, because on the
// request path no event exists while the pass runs. The caller appends them once it holds that
// event, in the same transaction, where refs.Stamp already runs. The base attributes of the
// nodes the pass wrote ride with them, so a block stated gone by a version carries the event
// that deleted it, and a block_refs row the pass inserts outside a first indexing takes that
// event as its tuple's legacy_source_seq (graphstore.UncausedTuple). A pass that lost the
// revision fence states nothing and answers the zero write, for which graphstore.Caused writes
// nothing. Each block whose words the version added, changed or restored, and each it dropped,
// is marked changed (graphstore.Node's Changed), so where the caller appends the write on a
// context holding a run each of them, and no other block, carries that run's changed_by_run.
//
// A block of the document whose row names no node - a row a build before this release wrote,
// which the backfill has not reached - takes its node and its membership of the document here,
// whether this version touched its text or not, because the row learns the address of that
// node in this same pass (reconcileBlockParents) and the foreign key on it is checked inside
// the statement that writes it.
//
// An authored node - a criterion, a requirement, a check or a plan task - is the one block the
// render does not place: it takes its first parent from what its author wrote, its parent
// attribute, its serves pointer and for a requirement its From: line (located), and keeps it until
// its parent attribute names another target (placeAuthored, D4 of dispatch://INFRA-1253's plan).
// Its serves stays a pointer like any other, so a retarget appends a serves tuple and its
// retraction and no part_of. A plan task's needs naming an issue is a pointer too, a tuple from the
// task to the issue's node. A block whose row the pass writes first is made by the first author of
// the version that first holds it, its text's since (textSince), at that version's time. Every
// block of the version whose node records its authors - an authored node anywhere, and every
// block of an issue's tree artifact but a decision and a reading (recordsAuthors) - and states
// none yet takes one authored_by per author of the version that first holds it, in that version's
// own order, and the authors' nodes with them (blockAuthors): a block this pass writes first, and
// one a build before this one wrote, which takes them at the pass that next indexes its document.
//
// typed is what the typed-node pass read of the version (DecisionFacts; the zero TypedFacts for a
// caller that reads none, which writes every decision and reading as its type's node and no tie
// of theirs): a decision's and a reading's node takes its type's supertag, a reading in an
// answered ask takes that ask's node for its part_of, and the pairs handed to replaceBlockRefs
// carry each decision's from_ask and decided_by and each ask block's shaped, marked with the ask's
// node, and none of their ties to another issue (TypedFacts.pairs). So every block_refs row of
// the document has this one writer, the typed-node pass's above the deferral bound included
// (IndexDecisionNodes), and the two write one set of rows for one version.
//
// Where another release indexed the document after the store last stated its blocks
// (standsAtVersion's stale, read of artifactID's own row), the rows the pass compares the render
// with are that release's, and
// they no longer name everything the store holds: the release before this one deletes the rows of
// the blocks and the links its version drops and states no node gone and retracts no tuple. The
// pass then also reads the store's own statement of the document (heldDocumentBlocks) and states,
// from the render, what a version dropping those rows would have stated: each block the store
// holds present that the version does not hold, gone with its pointers (goneSinceStated,
// retractGonePointers), and each link the store asserts out of a block of the document, of a kind
// a pair of a version writes (heldLinkKinds), that no pair of the version writes, retracted
// (unheldLinks). A tuple another writer states out of a block - a requirement's constrained_by, a
// changed_by_run - is no such link, and the pass leaves it where it stands. And since that
// release rewrote the row of each block whose text its version moved and stated no text value,
// the pass states the text of every block it places (reconcileBlockValues). That is no repair:
// what decides each statement is the version's render, and the store is read only for what it
// would go on asserting against it, as reconcileBlockParents reads it for the parents. A pass
// over a document only this release indexed reads none of it.
func IndexBlocks(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	versions Versions,
	markdown string,
	blocks []model.ArtifactBlock,
	located []text.Located,
	pointers []Pointer,
	typed TypedFacts,
) ([]Target, bool, graphstore.Uncaused, error) {
	// The fence is the pass's first write, so a pass that loses it has written nothing to undo
	// and waits at that one row rather than at a projection row further in.
	won, stale, indexedBefore, err := standsAtVersions(ctx, tx, artifactID, versions)
	if err != nil || !won {
		return nil, won, graphstore.Uncaused{}, err
	}
	// The version the facts that name no block of their own are stated in the name of: the one
	// version of a document, or the newest of an issue room's.
	lead, newest := versions.Newest(artifactID)
	version, versionAt := newest.Number, newest.At
	document, err := indexedVersionRecord(ctx, tx, artifactID, lead, version)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	refKey, writer := document.refKey, document.writer
	stored, err := storedBlocks(ctx, tx, artifactID, len(blocks))
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
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
	// The documents whose fragments of an issue room hold this version's blocks, where the
	// document indexed is an issue's tree artifact: each block's in_document names its fragment's
	// document, and its rank stands in that document's band (dispatch://INFRA-1253 R5).
	fragments, err := fragmentDocuments(ctx, tx, artifactID, nodes)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	var holders map[string]model.ArtifactBlock
	if len(fragments) > 0 {
		holders = make(map[string]model.ArtifactBlock, len(nodes))
		for _, block := range nodes {
			holders[block.ID] = block
		}
	}
	// The document each block is a block of - the one whose fragment of an issue room holds it,
	// which the pass read (fragmentDocuments), or the one its row held for a block the version
	// dropped, and artifactID itself for a block of no other - and the version it records, that
	// document's.
	documentOf := func(id string) string {
		if held, ok := stored[id]; ok && held.document != "" {
			return held.document
		}
		if block, ok := holders[id]; ok {
			if _, known := fragments[block.Document]; known {
				return block.Document
			}
		}
		return artifactID
	}
	versionIn := func(document string) Version {
		if held, ok := versions[document]; ok {
			return held
		}
		if held, ok := versions[artifactID]; ok {
			return held
		}
		return newest
	}
	versionOf := func(id string) Version { return versionIn(documentOf(id)) }

	// The blocks whose text this version changed (new, changed or restored), and those it deleted.
	var ids, types, sums, excerpts []string
	var textIDs []string
	var texts []*string
	// Which of ids holds no row yet, so this pass writes its row first: a block the version adds,
	// or one a version restores, whose row went with the version that dropped it.
	var first []bool
	// The blocks of this version whose row names no node, each with the time that row was
	// written: a pre-store row, which the pass below gives its node and its membership.
	var nodeless []nodelessBlock
	// A block whose text no version moved needs no string of its sum: the comparison against
	// the sum the index holds reads the hex out of one buffer this loop keeps, and only a
	// block this version writes a row for takes a string of its own.
	var sum blockSumHex
	for _, block := range nodes {
		body := markdown[block.From:block.To]
		blockTextSum(&sum, body)
		previous, held := stored[block.ID]
		delete(stored, block.ID)
		if held && !previous.named {
			nodeless = append(nodeless, nodelessBlock{id: block.ID, typ: block.Type, joined: previous.joined})
		}
		// A block of an issue room cut from one fragment and pasted with the same text into another
		// moved into another document, whose version it records from here on (documentOf): its
		// row and its text are written again at that document's version, as a changed block's are.
		if held && previous.sum == string(sum[:]) && previous.document == documentOf(block.ID) {
			continue
		}
		ids, types, sums = append(ids, block.ID), append(types, block.Type), append(sums, string(sum[:]))
		first = append(first, !held)
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
	// the changed and the restored - the ids the version dropped, and the blocks whose row
	// names no node, and no other. The first two are exactly the set R16's foreign key needs a
	// node for where every row of the document already names one, and that is what makes the
	// write cost what the edit changed rather than what the document holds: a version that
	// moves one word of a two-thousand-block document states two node rows, not four thousand.
	// A block whose text this version left alone already has its node from the version that
	// first indexed it, and its placement, which may still have moved, is a tuple
	// reconcileBlockParents states against that node.
	changed := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		changed[id] = struct{}{}
	}
	// Where the store's statement of the document is stale, a block another release's version
	// dropped has no row left for deleted to name, and its node stands: the store's own
	// statement of the document names it (goneSinceStated), and it is stated gone by the rule
	// below, its pointers with it, as a block whose row the version dropped is. It takes no
	// document_block_texts row and moves no panel (textIDs), since that release deleted its row
	// and moved what the row showed. Every address the store holds is inside the bound, since a
	// block past it is no node.
	var storeLinks map[string][]heldLink
	dropped := deleted
	if stale {
		if storeLinks, err = heldDocumentBlocks(ctx, tx, refKey); err != nil {
			return nil, false, graphstore.Uncaused{}, err
		}
		dropped = slices.Concat(deleted, goneSinceStated(storeLinks, nodes, deleted))
	}
	// The version each block whose row this pass writes stands since (textSince): this version,
	// or where the pass covers versions no pass indexed, the earliest of them from which its text
	// stood, which is its row's changed_version and, for a block whose row the pass writes first,
	// the version that first holds it. Its authors are who made such a block: the first of them
	// its node's made_by, at that version's time, and each of them, in the version's own order, an
	// authored_by of a node that records its authors (blockAuthors). A block of an issue room
	// stands since a version of its own document (documentOf).
	var since []int32
	var sinceAt []time.Time
	authors := map[documentVersion][]model.Actor{{document: lead, number: version}: document.authors}
	if len(ids) > 0 {
		if since, sinceAt, err = textSinceIn(ctx, tx, versions, indexedBefore, ids, texts[:len(ids)], documentOf, versionIn); err != nil {
			return nil, false, graphstore.Uncaused{}, err
		}
		wanted := make([]documentVersion, len(ids))
		for index, id := range ids {
			wanted[index] = documentVersion{document: documentOf(id), number: int(since[index])}
		}
		if authors, err = versionAuthors(ctx, tx, authors, wanted); err != nil {
			return nil, false, graphstore.Uncaused{}, err
		}
	}
	authorsOf := func(index int) []model.Actor {
		return authors[documentVersion{document: documentOf(ids[index]), number: int(since[index])}]
	}
	// Every node the pass writes restates its block (blockNode): a block this version holds is
	// written live and keeps a state a write marked it with, so a version that changes a
	// proposed requirement's words leaves it proposed, and comes back live only from gone. A
	// block whose words this version wrote and a block it dropped are what the version changed,
	// so each is marked changed, and a version written for a run names it as changed by that run
	// (graphstore.TupleChangedByRun) where its cause is appended (graphstore.Caused).
	blockNodes := make([]graphstore.Node, 0, len(ids)+len(dropped)+len(nodeless))
	// The authors of the version each block whose row this pass writes first stands since, by id:
	// that version first holds it, and no document_block_texts row says so before this pass
	// writes one (blockAuthors).
	var firstWritten map[string][]model.Actor
	for index, id := range ids {
		if !blockIDFits(refKey, id) {
			continue
		}
		made := authorsOf(index)
		var maker model.Actor
		if len(made) > 0 {
			maker = made[0]
		}
		blockNodes = append(blockNodes, blockNode(blockNodeAddress(refKey, id), document, maker, sinceAt[index], graphstore.StateLive, typed.supertag(id, types[index]), true))
		if first[index] {
			if firstWritten == nil {
				firstWritten = map[string][]model.Actor{}
			}
			firstWritten[id] = made
		}
	}
	// Every block of the version whose node records its authors, whether this version wrote it
	// or not, so a node an earlier build wrote takes its authors at the pass that next indexes
	// its document.
	var recording []string
	for index, block := range nodes {
		if fits[index] && recordsAuthors(block.Type, document.band) {
			recording = append(recording, block.ID)
		}
	}
	authorNodes, authored, err := blockAuthors(ctx, tx, artifactID, document, recording, func(id string) []model.Actor { return firstWritten[id] })
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	blockNodes = append(blockNodes, authorNodes...)
	// A block the version no longer holds has its node stated gone, whatever state it held: a
	// restating write that says gone is written gone, and keeps only the visibility. The tuple
	// that says so is not built here: the node write states a base attribute it moved, with this
	// write's own time and writer, so a block restored by a later version is stated live by the
	// same rule and no writer of this store states a base attribute of its own. Its node keeps the
	// supertag it holds, which no version holding the block is left to say. A block this version
	// dropped is one it changed (blockNode's changed); one another release's version dropped is
	// not.
	held, err := heldSupertags(ctx, tx, refKey, dropped)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	gone := make([]string, 0, len(dropped))
	for index, id := range dropped {
		if !blockIDFits(refKey, id) {
			continue
		}
		supertag, stored := held[id]
		if !stored {
			supertag = BlockSupertag
		}
		gone = append(gone, blockNodeAddress(refKey, id))
		blockNodes = append(blockNodes, blockNode(blockNodeAddress(refKey, id), document, document.writer, versionOf(id).At, graphstore.StateGone, supertag, index < len(deleted)))
	}
	// A row written before this release names no node: migration 20261109022248 added the
	// columns and filled none of them, and the backfill has not reached the document or has
	// not been run at all. reconcileBlockParents names this block's node on every row of the
	// document whose columns differ, and document_blocks_node is checked inside that
	// statement, so each of those rows takes its node here or that update is refused. The
	// membership goes with it: the insert above states one only for a row it truly inserted,
	// so a pre-store block whose text this version changed would otherwise keep its row, state
	// no in_document, and be skipped by the backfill for naming a node by then.
	//
	// Each membership is dated from its row's own created_at, as a row the insert writes
	// afresh is, so the tuple arm of graph_edges answers what the document_blocks arm answered
	// for it. A block past the address bound is no node of the store: it takes neither, and
	// its row keeps its null columns, which is what keeps the old arm answering it.
	var memberships []graphstore.UncausedTuple
	for _, block := range nodeless {
		if !blockIDFits(refKey, block.id) {
			continue
		}
		address := blockNodeAddress(refKey, block.id)
		// A block whose row only learns its node here is no block this version changed: it is
		// not marked (blockNode).
		if _, stated := changed[block.id]; !stated {
			blockNodes = append(blockNodes, blockNode(address, document, document.writer, versionOf(block.id).At, graphstore.StateLive, typed.supertag(block.id, block.typ), false))
		}
		memberships = append(memberships, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
			FromKind: BlockNodeKind, FromID: address, Kind: TupleInDocument,
			ToKind: "artifact", ToID: document.inDocument(holders[block.id], fragments),
			At: block.joined, Who: writer, Asserted: true,
		}})
	}
	// The nodes go in before any projection row that names one: the foreign key is checked
	// inside the statement writing that row, so a node written afterwards would be too late.
	// The base attributes that write states come back here rather than being appended with it,
	// because this pass runs before its caller's event exists, and they are appended with the
	// pass's own tuples under that event (graphstore.Uncaused). They carry no legacy_source_seq:
	// no legacy row states a node's visibility or its state.
	write, err := graphstore.WriteUncaused(ctx, tx, blockNodes, nil)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, fmt.Errorf("write the block nodes of version %d of %s: %w", version, artifactID, err)
	}
	write.Tuples = append(write.Tuples, memberships...)
	write.Tuples = append(write.Tuples, authored...)
	retracted, err := retractGonePointers(ctx, tx, gone, versionAt, writer)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, fmt.Errorf("retract the pointers at the blocks version %d of %s no longer holds: %w", version, artifactID, err)
	}
	write.Tuples = append(write.Tuples, retracted...)

	if len(deleted) > 0 {
		if _, err := tx.Exec(ctx, `
			delete from document_blocks where artifact_id = $1 and block_id = any($2)
		`, artifactID, deleted); err != nil {
			return nil, false, graphstore.Uncaused{}, fmt.Errorf("delete the blocks version %d of %s no longer holds: %w", version, artifactID, err)
		}
	}
	var textVersions []int32
	if len(ids) > 0 {
		// xmax = 0 tells a row this statement inserted from one it updated, which is what says
		// whether the block joined the document here: a block whose row was already there - a
		// block whose text alone changed - keeps that row and the membership it already states.
		fresh, err := scanFreshBlocks(tx.Query(ctx, `
			insert into document_blocks (artifact_id, block_id, node_id, node_kind, node_address, type, text_sha256, excerpt, changed_version, changed_at)
			select $1::uuid, b.id, a.ref_key || '#' || b.id,
			       case when octet_length(a.ref_key || '#' || b.id) <= $9 then $8 end,
			       case when octet_length(a.ref_key || '#' || b.id) <= $9 then a.ref_key || '#' || b.id end,
			       b.type, b.sum, b.excerpt, b.since, b.since_at
			from unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::int[], $7::timestamptz[])
			     as b (id, type, sum, excerpt, since, since_at)
			cross join (select ref_key from artifacts where id = $1::uuid) a
			on conflict (artifact_id, block_id) do update set
				type = excluded.type,
				text_sha256 = excluded.text_sha256,
				excerpt = excluded.excerpt,
				changed_version = excluded.changed_version,
				changed_at = excluded.changed_at
			returning block_id, created_at, xmax = 0
		`, artifactID, ids, types, sums, excerpts, since, sinceAt, BlockNodeKind, MaxBlockNodeBytes))
		if err != nil {
			return nil, false, graphstore.Uncaused{}, fmt.Errorf("record the blocks version %d of %s changed: %w", version, artifactID, err)
		}
		textVersions = since
		for _, id := range ids {
			joined, inserted := fresh[id]
			if !inserted || !blockIDFits(refKey, id) {
				continue
			}
			write.Tuples = append(write.Tuples, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
				FromKind: BlockNodeKind, FromID: blockNodeAddress(refKey, id), Kind: TupleInDocument,
				ToKind: "artifact", ToID: document.inDocument(holders[id], fragments),
				At: joined, Who: writer, Asserted: true,
			}})
		}
	}
	if len(textIDs) > 0 {
		// A deleted block's row is recorded at the version of its document this pass covers, and
		// a changed one's at the version its text stands since (textSince), so the text a reader
		// reads at an indexed version is the latest row at or before it, as ever, and the row a
		// block's changed_version names.
		for index := len(textVersions); index < len(textIDs); index++ {
			textVersions = append(textVersions, int32(versionOf(textIDs[index]).Number))
		}
		if _, err := tx.Exec(ctx, `
			insert into document_block_texts (artifact_id, block_id, version, text)
			select $1::uuid, t.id, t.version, t.text from unnest($2::text[], $3::text[], $4::int[]) as t (id, text, version)
			on conflict (artifact_id, block_id, version) do update set text = excluded.text
		`, artifactID, textIDs, texts, textVersions); err != nil {
			return nil, false, graphstore.Uncaused{}, fmt.Errorf("record the block texts of version %d of %s: %w", version, artifactID, err)
		}
	}
	// An issue room's blocks name their documents in issue_room_blocks, beside their
	// document_blocks rows under the tree artifact, in this transaction (D16).
	moves, err := reconcileRoomBlocks(ctx, tx, artifactID, document, nodes, fits, fragments, deleted)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	write.Tuples = append(write.Tuples, moves...)
	sources := authoredSourcesOf(markdown, nodes, located, pointers)
	placement, parents, err := reconcileBlockParents(ctx, tx, artifactID, document, nodes, fits, typed.askParents, nil, sources)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	write.Tuples = append(write.Tuples, parents...)
	values, err := reconcileBlockValues(ctx, tx, document, markdown, nodes, placement, changed, stale, fragments)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	write.Tuples = append(write.Tuples, values...)

	pairs := typed.pairs(blockPairs(nodes, located, typed.withTies(pointers)))
	moved, links, err := replaceBlockRefs(ctx, tx, artifactID, refKey, versionOf, writer, firstIndexing, pairs, nil)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	write.Tuples = append(write.Tuples, links...)
	// Where the store's statement is stale, the links another release's version dropped have
	// no row left for replaceBlockRefs to drop, and the store still asserts them (unheldLinks).
	// They move no count and no panel, and neither do the blocks goneSinceStated names: what a
	// reader is shown of a block and its links is read through their rows (graph_edges answers
	// a block's tuple only while its document_blocks or block_refs row stands), and that release
	// deleted those rows, and moved what they showed, when it wrote its version.
	write.Tuples = append(write.Tuples, unheldLinks(storeLinks, refKey, pairs, links, versionAt, writer)...)
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
		return nil, false, graphstore.Uncaused{}, err
	}
	moved = append(moved, cited...)
	// Each version is recorded indexed under its own document, which that table keys by.
	documents := slices.Sorted(maps.Keys(versions))
	numbers := make([]int32, len(documents))
	for index, indexed := range documents {
		numbers[index] = int32(versions[indexed].Number)
	}
	if _, err := tx.Exec(ctx, `
		insert into document_indexed_versions (artifact_id, version)
		select i.artifact_id, i.version from unnest($1::uuid[], $2::int[]) as i (artifact_id, version)
		on conflict (artifact_id, version) do nothing
	`, documents, numbers); err != nil {
		return nil, false, graphstore.Uncaused{}, fmt.Errorf("record the versions of %s indexed: %w", artifactID, err)
	}
	return moved, true, write, nil
}

// retractGonePointers is the retraction of every pointer tuple a block the version no longer holds
// leaves behind (dispatch://INFRA-1950/spec#b-r-constraint-risk): each asserted prevented_by whose
// far end is one of the blocks at addresses, a check that covered a risk, and each asserted
// constrained_by whose near end is one of them, a requirement held to a constraint. A constrained_by
// points at a constraint, never at a block, so the one a gone block takes with it starts there.
// Each retraction is dated at, the version's time, and signed by writer, as a dropped pair of
// block_refs is, and rides with the facts the pass states for the version's event, so it is
// appended in the pass's own transaction (graphstore.Caused).
//
// It logs how many it retracts at each block, with the block's address, and answers the number to
// no reader: the risk a check covered may be one only its author may see, and a count served
// beside the block would tell another reader it is there.
func retractGonePointers(ctx context.Context, tx pgx.Tx, addresses []string, at time.Time, writer model.Actor) ([]graphstore.UncausedTuple, error) {
	if len(addresses) == 0 {
		return nil, nil
	}
	const columns = `t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.asserted`
	rows, err := tx.Query(ctx, `
		select checked.from_kind, checked.from_id, checked.kind, checked.to_kind, checked.to_id, checked.to_id
		from (
			`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id, t.kind, t.from_kind, t.from_id`, columns,
		`t.to_kind = '`+BlockNodeKind+`' and t.to_id = any($1) and t.kind = '`+graphstore.TuplePreventedBy+`'`)+`
		) checked
		where checked.asserted
		union all
		select held.from_kind, held.from_id, held.kind, held.to_kind, held.to_id, held.from_id
		from (
			`+graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id`, columns,
		`t.from_kind = '`+BlockNodeKind+`' and t.from_id = any($1) and t.kind = '`+graphstore.TupleConstrainedBy+`'
				and t.to_kind <> '`+graphstore.KindValue+`'`)+`
		) held
		where held.asserted
	`, addresses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var retracted []graphstore.UncausedTuple
	perBlock := map[string]int{}
	for rows.Next() {
		tuple := graphstore.Tuple{At: at, Who: writer, Asserted: false}
		var block string
		if err := rows.Scan(&tuple.FromKind, &tuple.FromID, &tuple.Kind, &tuple.ToKind, &tuple.ToID, &block); err != nil {
			return nil, err
		}
		retracted = append(retracted, graphstore.UncausedTuple{Tuple: tuple})
		perBlock[block]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, block := range slices.Sorted(maps.Keys(perBlock)) {
		slog.Info("dispatch: a block the version no longer holds takes its pointers with it",
			"block", block, "retracted", perBlock[block])
	}
	return retracted, nil
}

// standsAtVersion settles the mark migration 0122's trigger left on the document when this
// version's row was inserted, and answers whether this pass is the one the document's blocks
// now stand at. The mark only ever moves forward: two passes can be indexing one document at
// once, and the slower one, reading version 9 while the other already wrote 10, would otherwise
// set the row back to 9 and claim 10's work as its own. The fence is the `where`, and the
// `returning` reads the row back in the same statement, so the pass learns in one conditional
// write whether it won: no row comes back when the fence held, which is this pass losing. A
// second pass of this release at the same version loses too, which is what keeps a re-index
// from stating a parent twice, since no code may update or delete a tuple row.
//
// The row also carries two columns no other release names (migration 20261116221539), which the
// winning pass writes beside version: store_version, the version whose blocks the store now
// states, which it sets to this version; and store_owed_at, the backfill's mark, which it clears,
// since this pass states every block's node, part_of and parent_block_id (IndexBlocks,
// reconcileBlockParents). A pass at the version the row already stands at wins only where the
// document holds that mark: the backfill named the nodes of its rows, and the release before this
// one may have indexed that version since and stated nothing in the store, so this pass is the one
// that states its blocks there. That is the re-index the reconciliation owes a marked document
// (docs' owedDocuments), and the first pass of this release at that version clears the mark, so a
// second one loses as before.
//
// Its second result says whether the store's statement of the document's blocks is stale: the
// store last stated them at a version other than the latest one any release indexed
// (document_indexed_versions, which every block-aware release writes), so the rows this pass
// reads (storedBlocks, and replaceBlockRefs' delete) are another release's and no longer say
// what the store holds. Both values are read before the statement writes the row, in its own
// snapshot, and every writer of a document's blocks holds the document's owner row by then, so
// nothing moves them between that read and the fence. A document no release has indexed reads
// null on both sides and is not stale.
//
// indexOwedDocument's own early return is a short cut, not this fence: it spares the work of a
// pass whose version is already settled, and it reads before the write rather than deciding it.
//
// The row this writes is also what owes the document its windows: windows_owed_at marks it
// owed them here, in the transaction that writes the blocks those windows will cover, as the
// item path marks its item (recordItemIndexed). The windows of these blocks are not written
// here. The window arm reads this marker through its partial index (DocumentsOwedWindows,
// migration 20261108070000), and only the statement that writes the windows clears it
// (WriteDocumentWindows). The marker keeps the time it already carries, so a document owed
// since an earlier write stays owed from then, and a crash after this statement leaves the
// document owed its windows rather than silently windowless. A pass that loses the fence
// marks nothing, because the pass that won it marked the document already.
//
// A version holding no block is marked like any other, and the arm lists it: an issue created
// with no spec carries a spec.md whose markdown is empty, and a document whose last block an
// edit removed still holds the windows of the text it had. Writing a document's windows is
// what deletes the window rows its blocks no longer stand for (writeWindows' `dropped`), so
// leaving such a version unmarked here would leave those rows standing for text the document
// no longer holds; the arm finds it by the version recorded here rather than by a row of
// document_blocks it has none of (documentsOwedWindowsQuery, INFRA-988).
//
// Its third result is the latest version a pass indexed before this one, 0 for none, read in
// the same statement off document_indexed_versions: the versions between it and this one are
// the ones this pass covers that no pass indexed (textSince).
//
// What a reader meets for it: a document's windows, and so its hits in meaning search, appear
// one reconciliation tick after the write rather than within it, as a comment's do
// (indexItemBlocks). The write holds the issue row for its own rows alone: this window write
// was 9.7% of an hour's database time over 31,241 calls at a 114.3 ms mean on the load-test
// deployment, and the writers queued behind it were waiting on that row (INFRA-988).
func standsAtVersion(ctx context.Context, tx pgx.Tx, artifactID string, version int) (won, stale bool, indexedBefore int, err error) {
	err = tx.QueryRow(ctx, `
		with before as (
			select (select store_version from document_block_versions where artifact_id = $1)
			       is distinct from
			       (select max(version) from document_indexed_versions where artifact_id = $1) as stale
		)
		insert into document_block_versions (artifact_id, version, store_version, windows_owed_at) values ($1, $2, $2, now())
		on conflict (artifact_id) do update set version = excluded.version, store_version = excluded.store_version,
			store_owed_at = null,
			windows_owed_at = coalesce(document_block_versions.windows_owed_at, excluded.windows_owed_at)
		where document_block_versions.version is null or document_block_versions.version < excluded.version
		   or document_block_versions.version = excluded.version and document_block_versions.store_owed_at is not null
		returning (select stale from before),
		          coalesce((select max(i.version) from document_indexed_versions i
		                    where i.artifact_id = $1 and i.version < $2), 0)
	`, artifactID, version).Scan(&stale, &indexedBefore)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, 0, nil
	}
	if err != nil {
		return false, false, 0, fmt.Errorf("record the blocks of %s indexed at version %d: %w", artifactID, version, err)
	}
	return true, stale, indexedBefore, nil
}

// standsAtVersions is standsAtVersion over every document versions names, the fence of one pass
// over an issue room (IndexBlocks): the pass wins where any of those documents' rows stands behind
// the version it covers of that document - a row owed (its version null), one at an earlier
// version, one at that version holding the backfill's mark, or no row at all - and then sets every
// row to the version it covers, clears each one's backfill mark and owes it its windows, as the
// fence of one document does, never moving a row back. So the trigger's mark on each version a
// room's settlement writes, and the mark of any document of the room the reconciliation found
// owed, are cleared by the one pass that indexes them all, in its own transaction. stale is read
// of artifactID's own row, the key every row of the room is written under, and indexedBefore is
// each document's latest version indexed before the one the pass covers. A pass over one document
// is standsAtVersion's.
func standsAtVersions(ctx context.Context, tx pgx.Tx, artifactID string, versions Versions) (won, stale bool, indexedBefore map[string]int, err error) {
	if own, ok := versions[artifactID]; ok && len(versions) == 1 {
		won, stale, before, err := standsAtVersion(ctx, tx, artifactID, own.Number)
		return won, stale, map[string]int{artifactID: before}, err
	}
	documents := slices.Sorted(maps.Keys(versions))
	numbers := make([]int32, len(documents))
	for index, document := range documents {
		numbers[index] = int32(versions[document].Number)
	}
	rows, err := tx.Query(ctx, `
		with wanted as (
			select w.artifact_id, w.version from unnest($2::uuid[], $3::int[]) as w (artifact_id, version)
		), before as (
			select (select store_version from document_block_versions where artifact_id = $1)
			       is distinct from
			       (select max(version) from document_indexed_versions where artifact_id = $1) as stale,
			       exists (
			           select 1 from wanted w
			           left join document_block_versions b on b.artifact_id = w.artifact_id
			           where b.artifact_id is null or b.version is null or b.version < w.version
			              or b.version = w.version and b.store_owed_at is not null) as behind
		), stood as (
			insert into document_block_versions (artifact_id, version, store_version, windows_owed_at)
			select w.artifact_id, w.version, w.version, now() from wanted w where (select behind from before)
			on conflict (artifact_id) do update set version = excluded.version, store_version = excluded.store_version,
				store_owed_at = null,
				windows_owed_at = coalesce(document_block_versions.windows_owed_at, excluded.windows_owed_at)
			where document_block_versions.version is null or document_block_versions.version <= excluded.version
		)
		select (select behind from before), (select stale from before), w.artifact_id::text,
		       coalesce((select max(i.version) from document_indexed_versions i
		                 where i.artifact_id = w.artifact_id and i.version < w.version), 0)
		from wanted w
	`, artifactID, documents, numbers)
	if err != nil {
		return false, false, nil, fmt.Errorf("record the blocks of %s indexed at the versions of its documents: %w", artifactID, err)
	}
	defer rows.Close()
	indexedBefore = make(map[string]int, len(documents))
	for rows.Next() {
		var document string
		var before int
		if err := rows.Scan(&won, &stale, &document, &before); err != nil {
			return false, false, nil, fmt.Errorf("record the blocks of %s indexed at the versions of its documents: %w", artifactID, err)
		}
		indexedBefore[document] = before
	}
	if err := rows.Err(); err != nil {
		return false, false, nil, fmt.Errorf("record the blocks of %s indexed at the versions of its documents: %w", artifactID, err)
	}
	return won, stale, indexedBefore, nil
}

// textSinceIn is textSince over blocks of several documents: ids are the blocks whose text the
// pass records, beside bodies, each read against the versions of its own document (documentOf)
// that the pass covers and no pass indexed before (indexedBefore). A block of a document versions
// covers no version of takes the version versionIn answers for it, whose history it does not read.
func textSinceIn(
	ctx context.Context,
	tx pgx.Tx,
	versions Versions,
	indexedBefore map[string]int,
	ids []string,
	bodies []*string,
	documentOf func(string) string,
	versionIn func(string) Version,
) ([]int32, []time.Time, error) {
	since := make([]int32, len(ids))
	at := make([]time.Time, len(ids))
	grouped := map[string][]int{}
	for index, id := range ids {
		document := documentOf(id)
		grouped[document] = append(grouped[document], index)
	}
	for _, document := range slices.Sorted(maps.Keys(grouped)) {
		indexes := grouped[document]
		version, covered := versions[document]
		if !covered {
			version = versionIn(document)
			for _, index := range indexes {
				since[index], at[index] = int32(version.Number), version.At
			}
			continue
		}
		held := make([]*string, len(indexes))
		for position, index := range indexes {
			held[position] = bodies[index]
		}
		documentSince, documentAt, err := textSince(ctx, tx, document, version.Number, version.At, indexedBefore[document], held)
		if err != nil {
			return nil, nil, err
		}
		for position, index := range indexes {
			since[index], at[index] = documentSince[position], documentAt[position]
		}
	}
	return since, at, nil
}

// textSince is, for each block whose text this pass records (bodies, its text in this version),
// the version that text has stood since and that version's time, which the block's row takes as
// its changed_version and changed_at. It is this version where the pass covers no other: on the
// request path every version is indexed by its own write, and indexedBefore is the one before it.
// A pass can cover more than one - a document past the deferral bound, or versions a build
// before wrote during the roll, each of which the block-index reconciliation indexes at the latest
// owed version alone (docs' indexOwedDocument) - and then the text stands since the earliest of
// the owed versions from which every version up to this one holds a block of that same text, as
// the renderer cuts the version's stored markdown (versionBlockTexts). So a version that rewrote
// a block and a later one that left it alone, indexed in one pass, date the change to the first,
// and the text-change time a reader takes from the row (dispatch://INFRA-1150 c-spec-tie) is that
// version's. The owed versions are read newest first and only while some block still matches, and
// not at all on the request path.
func textSince(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, indexedBefore int, bodies []*string) ([]int32, []time.Time, error) {
	since := make([]int32, len(bodies))
	at := make([]time.Time, len(bodies))
	pending := make([]int, len(bodies))
	for index := range bodies {
		since[index], at[index], pending[index] = int32(version), versionAt, index
	}
	if indexedBefore >= version-1 {
		return since, at, nil
	}
	rows, err := tx.Query(ctx, `
		select number, markdown, created_at from artifact_versions
		where artifact_id = $1::uuid and number > $2 and number < $3 and markdown is not null
		order by number desc
	`, artifactID, indexedBefore, version)
	if err != nil {
		return nil, nil, fmt.Errorf("read the versions of %s no pass indexed before %d: %w", artifactID, version, err)
	}
	defer rows.Close()
	for len(pending) > 0 && rows.Next() {
		var number int32
		var markdown string
		var created time.Time
		if err := rows.Scan(&number, &markdown, &created); err != nil {
			return nil, nil, fmt.Errorf("read a version of %s no pass indexed: %w", artifactID, err)
		}
		held := versionBlockTexts(markdown)
		standing := pending[:0]
		for _, index := range pending {
			if _, holds := held[*bodies[index]]; holds {
				since[index], at[index] = number, created
				standing = append(standing, index)
			}
		}
		pending = standing
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the versions of %s no pass indexed before %d: %w", artifactID, version, err)
	}
	return since, at, nil
}

// versionBlockTexts is the text of every block of a version's stored markdown, as the renderer
// cuts it for the block index: the markdown read back (pmdoc.ParseRendering) and rendered with its
// block offsets. A version that does not read back holds no block textSince can match.
func versionBlockTexts(markdown string) map[string]struct{} {
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return nil
	}
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		return nil
	}
	texts := make(map[string]struct{}, len(offsets))
	for _, offset := range offsets {
		texts[rendered[offset.From:offset.To]] = struct{}{}
	}
	return texts
}

// OwedBlockIndexDebts is one row per document owed a block index - its version null, or the
// backfill's mark on it (store_owed_at, standsAtVersion) - each dated by the write that owed it:
// the created_at of the latest markdown version the document holds, which is the version a tick
// would index, or the document's own created_at where it holds no markdown version at all.
//
// Nothing dates a null version - document_block_versions records no time it was set back to null
// (migration 0121) - so the date is derived, and the backfill's mark is dated the same way, which
// reads older than the mark, the safe direction for a backlog. The two readers of the backlog
// derive it from this one text: the tick's own logged figure (docs.ReconcileBlockIndex) and the
// one `envoy-dispatch census --store --sizing` prints for somebody holding the database and not
// the log (graphcensus's blockIndexBacklogQuery). Each wraps it in its own aggregate.
const OwedBlockIndexDebts = owedBlockIndexDebtsFrom + `
		where b.version is null or b.store_owed_at is not null`

// OwedBlockIndexDebtsBeforeTheMark is OwedBlockIndexDebts over a database migration
// 20261116221539 has not reached yet, which holds no store_owed_at column and so no mark of the
// backfill's: only a null version is owed there. `census --store --sizing` takes its readings on
// such a database, before this branch's migrations run (R4), and reads this one there; the tick
// only ever runs on a migrated database and reads the full text.
const OwedBlockIndexDebtsBeforeTheMark = owedBlockIndexDebtsFrom + `
		where b.version is null`

// owedBlockIndexDebtsFrom is the select both owed-debt texts share, so the two date a debt alike.
const owedBlockIndexDebtsFrom = `
		select coalesce(
		         (select v.created_at from artifact_versions v
		          where v.artifact_id = b.artifact_id and v.markdown is not null
		          order by v.number desc limit 1),
		         a.created_at) as owed_at
		from document_block_versions b
		left join artifacts a on a.id = b.artifact_id`

// OwedTimeText writes a time the block-index backlog is dated by as both of its readers print
// it - the tick's log line (docs.ReconcileBlockIndex) and the sizing's backlog lines
// (graphcensus) - so the two say one time in one spelling: UTC to the nanosecond, so two runs over
// one unchanged database print one byte sequence, and `none` where there is no such time, which a
// zero value would otherwise print as a date in the year 1.
func OwedTimeText(at time.Time) string {
	if at.IsZero() {
		return "none"
	}
	return at.UTC().Format(time.RFC3339Nano)
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

// WriteDocumentWindows writes the windows of document artifactID's blocks in the caller's
// transaction, which holds the document's owner row, and stamps the state those blocks are in
// beside them, so the window arm can tell these windows from ones an older build left behind:
// blocks are the document's blocks in document order, each parent before its children, and
// markdown the text they index into. The owner key of a document's windows is its artifact id,
// and the context of each is the document's own title and the heading above the window
// (itemblocks.DocumentWindows).
//
// It is the one writer of a document's windows. A version write records the blocks and leaves
// the windows that stand for them to the block-index tick's window arm (IndexBlocks,
// docs.windowDocument), which windows the stored latest version's markdown - the text the
// version write indexed - a tick behind that write.
func WriteDocumentWindows(ctx context.Context, tx pgx.Tx, artifactID, markdown string, blocks []model.ArtifactBlock) error {
	nodes, _, _ := uniqueBlocks(blocks)
	var title string
	if err := tx.QueryRow(ctx, `select name from artifacts where id = $1::uuid`, artifactID).Scan(&title); err != nil {
		return fmt.Errorf("read the title of %s for its windows: %w", artifactID, err)
	}
	return writeWindows(ctx, tx, DocumentWindowKind, artifactID,
		itemblocks.DocumentWindows(title, documentBlockTexts(markdown, nodes)), blockState{
			query: `update document_block_versions v set windows_owed_at = null
			          where v.artifact_id = $5::uuid returning ` + documentBlockState + ` as block_state`,
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
// the write runs as a common table expression and stamps beside the windows. Both owners' queries
// are updates: an item's clears the marker that listed it on the kind's state row and returns the
// state that row holds (clearItemWindowsOwed), and a document's clears the same marker on
// document_block_versions and returns the version its blocks are indexed at (the query above).
// The query numbers its parameters from $5 and passes their values in args, since writeWindows
// holds $1 to $4.
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
// The whole write is one statement, so a document of fifty windows costs one round trip, and the
// marker that listed the owner is settled in that same round trip, an item's and a document's
// alike (clearItemWindowsOwed, WriteDocumentWindows). The windows go through
// jsonb_to_recordset in the top-level select; the state query, the stamp of what it returns and
// the delete are common table expressions, and Postgres runs a data-modifying one exactly once
// and to completion whether or not the select reads it. The delete cannot race the enqueue: it
// names only ids the new set does not hold, and the enqueue writes only ids it does.
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
// (WriteDocumentWindows): the version they are indexed at, over document_block_versions v. The
// state is read from document_block_versions in the statement that writes the windows, so what
// is stamped is the version the blocks those windows cover are indexed at, and a document owed
// its first block index holds a null version, which stamps as the empty string.
//
// Nothing in the server reads the stamp back: windows_owed_at says which documents are owed
// their windows (DocumentsOwedWindows), and that marker replaced the comparison this stamp
// served. Two readers of it are left, which is why the write still makes it. Migration
// 20261108070100 reads it once, to mark every document whose windows were already stale when
// the column arrived, and the release before this one decides owedness from it, so a rollback
// finds the stamp it has always read.
const documentBlockState = `coalesce(v.version::text, '')`

// DocumentsOwedWindows lists at most limit documents whose blocks the index holds - a document
// can hold none, and such a one is listed too - and whose windows do not stand for those
// blocks, in artifact id order past after (empty: from the start), for the block-index tick's
// window arm. A document written before meaning search moved to windows has blocks and no
// window; one whose version write failed after its blocks were written has the same; and every
// document written since this arm last windowed it is one too, because a version write records
// the blocks and leaves the windows here (IndexBlocks, INFRA-988).
//
// The read is of the marker, windows_owed_at (migration 20261108070000), which is the
// question written down rather than asked of state, as the items' is (ItemsOwedWindows): the
// version write and 0122's trigger set it, rebuild-refs reaches it through the re-index, and
// migration 20261108070100 set it once for every document whose windows were already stale
// when the column arrived. The one statement that writes a document's windows clears it,
// beside the stamp of the version those windows were written from (WriteDocumentWindows,
// writeWindows), so the clear and the windows cannot come apart; that statement's own
// `dropped` is the only delete of a window row in the server, and a route that removes one
// otherwise marks its owner here, or this read will not see it.
//
// The reading this replaced decided owedness from state - whether a stamp stood at the
// version the blocks are indexed at, and whether a window row existed at all - which is two
// index probes for every row of document_block_versions: 112-122 ms a pass with 67,496 buffer
// hits over the load-test deployment's 9,881 documents, for an answer of no rows, and a tick
// runs it every 2 s while the arm has a full batch (INFRA-988).
func DocumentsOwedWindows(ctx context.Context, q Queryer, after string, limit int) ([]string, error) {
	rows, err := q.Query(ctx, documentsOwedWindowsQuery, uuidAfter(after), limit)
	if err != nil {
		return nil, fmt.Errorf("list the documents whose windows do not stand for their blocks: %w", err)
	}
	documents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the documents whose windows do not stand for their blocks: %w", err)
	}
	return documents, nil
}

// documentsOwedWindowsQuery is the statement DocumentsOwedWindows sends: $1 is the id its
// batch starts after and $2 the most ids it answers. The tests read the statement from here
// rather than from a copy of it (document_windows_owed_test.go).
//
// The driving rows are the marked rows of document_block_versions, read through its partial
// index on windows_owed_at, whose key is the artifact id: the index holds the owed rows
// alone, in the order the batch pages by, so the keyset cursor and the limit are both the
// index's own and the read costs what the arm has to do rather than what the deployment has
// stored.
//
// Two kinds of marked row are this arm's, and the lateral join is what tells them from the
// rows that are not:
//
//   - a document the block index has reached, whose row carries the version its blocks are
//     indexed at. A version holding no block is one of them: an issue created with no spec
//     carries a spec.md whose markdown is empty, so the index holds no row of
//     document_blocks for it to be found by. The arm windows such a document to no window,
//     which deletes whatever windows a version that did hold blocks left behind and clears
//     the marker in the one statement (WriteDocumentWindows, writeWindows). The semi-join
//     this replaced dropped it, and since that statement is the only one that clears the
//     marker, the row stayed marked for ever: an entry in the partial index that every later
//     pass paid a probe for and no pass could ever remove (INFRA-988).
//   - a document owed a block index (version null) that holds the blocks of an earlier
//     version. Its stored version's windows are this arm's to write while the owed arm
//     cannot index it, which is what a document whose live text stays ahead of its latest
//     version needs (docs.windowDocument).
//
// A marked row with neither - a document an older build wrote, owed a block index and holding
// no block yet - is not this arm's, as the items' is not: windows under no block are not its
// to write, the owed-document arm indexes it, and that index marks it here again.
//
// The blocks are read through a lateral join rather than as `v.version is not null or exists
// (…)`: an EXISTS under an OR is a correlated subplan the planner runs per marked row, which
// the plan test refuses, while the join is one probe for each marked row read, as the
// semi-join before it was, and the inner limit stops it at the first block row.
const documentsOwedWindowsQuery = `
	select v.artifact_id::text from document_block_versions v
	left join lateral (
		select 1 from document_blocks b where b.artifact_id = v.artifact_id limit 1
	) held (block) on true
	where v.windows_owed_at is not null
	  and v.artifact_id > $1::uuid
	  and (v.version is not null or held.block is not null)
	order by v.artifact_id limit $2`

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

// storedBlock is what the document's index already holds of one block: the sha256 of its text,
// when its document_blocks row was written, which is the time a membership of the document is
// dated from (TupleInDocument), whether that row names the block's node, and the document the
// block is a block of (BlockDocumentSQL), whose version a block the version drops records.
type storedBlock struct {
	sum      string
	joined   time.Time
	named    bool
	document string
}

// nodelessBlock is one block of the version whose document_blocks row names no node: its id, its
// type, which the node it takes is tagged by (TypedFacts.supertag), and the time that row was
// written, which the membership IndexBlocks states for it is dated from.
type nodelessBlock struct {
	id, typ string
	joined  time.Time
}

// heldSupertags is the supertag the store holds for each of ids, blocks of the document refKey
// names, by id: a block a version deletes keeps the supertag its node carries, since no block of
// that version is left to say its type. An id the store holds no node of is absent.
func heldSupertags(ctx context.Context, tx pgx.Tx, refKey string, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		select substr(n.id, length($2) + 2), n.supertag
		from graph_nodes n
		where n.kind = $1 and n.id = any(select $2 || '#' || id from unnest($3::text[]) as deleted (id))
	`, BlockNodeKind, refKey, ids)
	if err != nil {
		return nil, fmt.Errorf("read the supertags of the blocks of %s a version deletes: %w", refKey, err)
	}
	defer rows.Close()
	held := make(map[string]string, len(ids))
	for rows.Next() {
		var id, supertag string
		if err := rows.Scan(&id, &supertag); err != nil {
			return nil, fmt.Errorf("scan the supertag of a block of %s: %w", refKey, err)
		}
		held[id] = supertag
	}
	return held, rows.Err()
}

// storedBlocks is each block the document's index holds, by id. blocks is how many blocks the
// version being indexed holds, which is about how many rows the read answers: a map made at
// that size is not grown once per thousand rows read into it.
//
// It reads the node columns and the row's created_at beside the digest, in the one read of the
// document's rows this pass takes, because the pass writes a node and a membership for every
// row that names none (IndexBlocks) and a second read of the same rows would cost a scan of
// the document for every version. The document of each block is BlockDocumentSQL's, read as a
// join on the room's rows (none for a document that is its own room) rather than a probe a row.
func storedBlocks(ctx context.Context, tx pgx.Tx, artifactID string, blocks int) (map[string]storedBlock, error) {
	rows, err := tx.Query(ctx, `
		select d.block_id, d.text_sha256, d.created_at, d.node_kind is not null and d.node_address is not null,
		       coalesce(r.document_id, d.artifact_id)::text
		from document_blocks d
		left join (select block_id, document_id from issue_room_blocks where tree_artifact_id = $1) r
		       on r.block_id = d.block_id
		where d.artifact_id = $1
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the blocks of %s: %w", artifactID, err)
	}
	defer rows.Close()
	stored := make(map[string]storedBlock, blocks)
	for rows.Next() {
		var id string
		var held storedBlock
		if err := rows.Scan(&id, &held.sum, &held.joined, &held.named, &held.document); err != nil {
			return nil, fmt.Errorf("scan a block of %s: %w", artifactID, err)
		}
		stored[id] = held
	}
	return stored, rows.Err()
}

// blockPair is one link a block holds: the block's id, the edge's kind and its target, and the
// block types it points at (nil: any). nodeKind and nodeAddress name the node the link hangs on
// where it is not the block's own: an ask block's shaped hangs on the ask's node, whose row is
// marked ('ask', <ask id>) (TypedFacts.pairs).
type blockPair struct {
	blockID     string
	kind        string
	target      Target
	toTypes     []string
	nodeKind    string
	nodeAddress string
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
// no version changed and count its target as moved. A pair that names a node of its own
// (blockPair.nodeKind: an ask block's shaped, on the ask's node) writes that node into the row,
// which marks it, and its tuple is stated out of that node, as its retraction is once the row
// goes.
//
// scope, where it is not nil, is the rows the reconcile may delete: those of its kinds held by
// its blocks, which is the typed-node pass's reconcile of its own ties above the deferral bound
// (IndexDecisionNodes). Every other row of the document is left as it stands, whatever pairs
// holds, so a pass that read only those ties deletes nothing it did not read.
//
// versionOf is the version each pair's block records, by the block's id: a pair inserted takes
// it as its created_version and its time as its created_at, and a pair dropped is retracted at
// that time. It is the one version a pass over a document indexes, and for an issue room the
// version of the block's own document (IndexBlocks), so a reader keying a pair's version by the
// block's document (BlockDocumentSQL) finds that document's.
func replaceBlockRefs(
	ctx context.Context,
	tx pgx.Tx,
	artifactID, refKey string,
	versionOf func(blockID string) Version,
	writer model.Actor,
	firstIndexing bool,
	pairs []blockPair,
	scope *pairScope,
) ([]Target, []graphstore.UncausedTuple, error) {
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
	createdVersions := make([]int32, len(pairs))
	createdAts := make([]time.Time, len(pairs))
	for i, pair := range pairs {
		blockIDs[i], kinds[i], toKinds[i], toIDs[i] = pair.blockID, pair.kind, pair.target.Kind, pair.target.ID
		if len(pair.toTypes) > 0 {
			joined := strings.Join(pair.toTypes, " ")
			toTypes[i] = &joined
		}
		if pair.nodeKind != "" {
			nodeKinds[i], nodeAddresses[i] = &pairs[i].nodeKind, &pairs[i].nodeAddress
		}
		version := versionOf(pair.blockID)
		createdVersions[i], createdAts[i] = int32(version.Number), version.At
	}
	var scopeKinds, scopeBlocks []string
	if scope != nil {
		scopeKinds, scopeBlocks = scope.kinds, scope.blocks
	}
	dropped, err := scanBlockRefRows(tx.Query(ctx, `
		delete from block_refs r
		where r.artifact_id = $1 and not exists (
			select 1 from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
			where p.block_id = r.block_id and p.kind = r.kind and p.to_kind = r.to_kind and p.to_id = r.to_id)
		  and ($6::text[] is null or (r.kind = any($6::text[]) and r.block_id = any($7::text[])))
		returning r.block_id, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq, true, r.node_kind, r.node_address
	`, artifactID, blockIDs, kinds, toKinds, toIDs, scopeKinds, scopeBlocks))
	if err != nil {
		return nil, nil, fmt.Errorf("delete the block mentions the version of %s dropped: %w", artifactID, err)
	}
	moved := make([]Target, 0, len(dropped))
	var tuples []graphstore.UncausedTuple
	for _, row := range dropped {
		moved = append(moved, Target{Kind: row.toKind, ID: row.toID})
		fromKind, address := row.from(refKey)
		// A mention's or a pointer's far end is a reference target, which the block's row holds
		// up to text.MaxTargetBytes but the store keys whole beside the block's own address: a
		// tuple whose far end is past graphstore.MaxAddressBytes was never written, so its
		// retraction is not written either, and the row kept its null node columns (the insert
		// below), which the old block_refs arm answers.
		if !graphstore.AddressFits(address) || !graphstore.AddressFits(row.toID) {
			continue
		}
		// The row is gone, so no source_seq is ever written for it and the retraction carries
		// none: its time is the version's of the block, the moment the pair stopped being held.
		tuples = append(tuples, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
			FromKind: fromKind, FromID: address, Kind: row.kind,
			ToKind: row.toKind, ToID: row.toID,
			At: versionOf(row.blockID).At, Who: writer, Asserted: false,
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
		       case when octet_length(p.to_id) > $11 then null
		            when p.node_kind is not null then p.node_kind
		            when octet_length(a.ref_key || '#' || p.block_id) <= $11 then $10 end,
		       case when octet_length(p.to_id) > $11 then null
		            when p.node_kind is not null then p.node_address
		            when octet_length(a.ref_key || '#' || p.block_id) <= $11 then a.ref_key || '#' || p.block_id end,
		       p.to_kind, p.to_id, p.kind, string_to_array(p.to_types, ' '), p.created_version,
		       coalesce(case when $8 then r.created_at end, p.created_at), case when $8 then r.source_seq end
		from unnest($2::text[], $3::text[], $4::text[], $5::text[], $9::text[], $12::text[], $13::text[], $6::int[], $7::timestamptz[])
		     as p (block_id, kind, to_kind, to_id, to_types, node_kind, node_address, created_version, created_at)
		cross join (select ref_key from artifacts where id = $1::uuid) a
		left join refs r on r.from_kind = 'artifact' and r.from_id = $1::uuid::text
		                and r.to_kind = p.to_kind and r.to_id = p.to_id
		on conflict (artifact_id, block_id, to_kind, to_id, kind) do update set to_types = excluded.to_types
		where block_refs.to_types is distinct from excluded.to_types
		returning block_id, kind, to_kind, to_id, created_at, source_seq, xmax = 0, node_kind, node_address
	`, artifactID, blockIDs, kinds, toKinds, toIDs, createdVersions, createdAts, firstIndexing, toTypes, BlockNodeKind, MaxBlockNodeBytes,
		nodeKinds, nodeAddresses))
	if err != nil {
		return nil, nil, fmt.Errorf("record the block mentions of the version of %s: %w", artifactID, err)
	}
	for _, row := range written {
		moved = append(moved, Target{Kind: row.toKind, ID: row.toID})
		fromKind, address := row.from(refKey)
		if !row.inserted || !graphstore.AddressFits(address) || !graphstore.AddressFits(row.toID) {
			continue
		}
		// Outside a first indexing the insert leaves source_seq null and the version's event
		// fills it in this same transaction, which is what LegacyFromCause names.
		tuples = append(tuples, graphstore.UncausedTuple{
			Tuple: graphstore.Tuple{
				FromKind: fromKind, FromID: address, Kind: row.kind,
				ToKind: row.toKind, ToID: row.toID,
				At: row.createdAt, Who: writer, LegacySourceSeq: row.sourceSeq, Asserted: true,
			},
			LegacyFromCause: true,
		})
	}
	return moved, tuples, nil
}

// pairScope is the rows of a document's block_refs one reconcile may delete (replaceBlockRefs):
// those of the tuple kinds kinds held by the blocks blocks.
type pairScope struct {
	kinds, blocks []string
}

// blockRefRow is one block_refs row as the reconcile's two statements return it, with whether
// the insert inserted it rather than updating a row that was already there (xmax = 0), and the
// node it names, null where it names none.
type blockRefRow struct {
	blockID, kind, toKind, toID string
	createdAt                   time.Time
	sourceSeq                   *int64
	inserted                    bool
	nodeKind, nodeAddress       *string
}

// from is the node the row's tuple is stated out of: the node a marked row names (an ask's, for
// an ask block's shaped), and the block's own otherwise, whose address is its document's ref_key
// and its id whether or not the row names it.
func (row blockRefRow) from(refKey string) (kind, address string) {
	if row.nodeKind != nil && *row.nodeKind != BlockNodeKind && row.nodeAddress != nil {
		return *row.nodeKind, *row.nodeAddress
	}
	return BlockNodeKind, blockNodeAddress(refKey, row.blockID)
}

func scanBlockRefRows(rows pgx.Rows, err error) ([]blockRefRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var written []blockRefRow
	for rows.Next() {
		var row blockRefRow
		if err := rows.Scan(&row.blockID, &row.kind, &row.toKind, &row.toID, &row.createdAt, &row.sourceSeq, &row.inserted, &row.nodeKind, &row.nodeAddress); err != nil {
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
// back when a version holds its id again. A link whose target no current version holds but whose
// moved_to tuples reach a block of toArtifact (graphstore.MovedTo) counts as a link to that block,
// which it names, so a citation written before its target moved still reaches it
// (dispatch://INFRA-1150 R8); only a target no row holds is followed through block_moved_to, so a
// link to a block that stands costs the probe alone. A pointer's link counts only while the block
// it reaches has a type the pointer allows, tested as the arm of graph_edges that answers the row
// tests it (blockRefListed). A link either of whose blocks who may not see drops out the same
// way, since neither end resolves to a node.
//
// It reads no document's address and follows no document's move: it takes the two documents by
// uuid, and a document the pre-mortem conversion retired, whose address moved to a lens
// (api.ConvertPremortem), keeps its row, its uuid, its versions and its blocks, so the links to its
// blocks from a third document read as they did before it was retired.
func BlockLinks(ctx context.Context, q Queryer, who model.Actor, fromArtifact, toArtifact string) ([]model.BlockLink, error) {
	rows, err := q.Query(ctx, `
		select r.kind, r.from_node, t.node_id, r.created_at, r.created_version
		from block_refs r
		join document_blocks f on f.artifact_id = r.artifact_id and f.block_id = r.block_id
		cross join lateral (
			select coalesce(
				(select held.node_id from document_blocks held where held.node_id = r.to_id limit 1),
				block_moved_to(r.to_id)) as node_id
		) target
		join document_blocks t on t.node_id = target.node_id and t.artifact_id = $2::uuid
		where r.artifact_id = $1::uuid and r.to_kind = 'block' and `+blockRefListed("r", "t.node_id")+`
		order by r.created_at desc, r.from_node, t.node_id`, fromArtifact, toArtifact)
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

// BlockTargets is where a write to document artifactID lands and the target each of targets
// names, by node id (text.TargetID), as the block index would hold it once that write, leaving the
// blocks written (rendered, in document order, each parent before its children), is indexed, read
// through q: a block of that document as written's node for its id (uniqueBlocks: the first
// holder of a repeated id, and no id the index cannot key), any other block of a document as its
// document's current blocks (document_blocks) hold it, with one query for every such target; an
// ask, a comment or a message, or a current block of one, as conversationTargets reads it, with
// its item's own issue and R2's test at the write's time; and an issue by its key. A target no row
// has is absent. The write lands in the document's own issue, or in none when a project holds
// it; that issue's ancestors are read only once a target turns out to sit in another document or
// in another issue's item (IssueAncestors), since a pointer inside the writing document - a plan
// task's `needs`, the common case - is held to nothing above it (pmdoc.CheckBlockPointers), and
// its descendants only once a target is an issue (IssueDescendants, read as a reader who proves
// no author reads it, since a rule of a write names no reader). This runs inside the write's
// transaction.
func BlockTargets(ctx context.Context, q Queryer, artifactID string, written []model.ArtifactBlock, targets []text.Ref) (pmdoc.BlockWriting, map[string]pmdoc.BlockTarget, error) {
	var writing pmdoc.BlockWriting
	var at time.Time
	if err := q.QueryRow(ctx, `
		select coalesce(ref_key, ''), coalesce(issue_key, ''), clock_timestamp() from artifacts where id = $1
	`, artifactID).Scan(&writing.Document, &writing.Issue, &at); err != nil {
		return pmdoc.BlockWriting{}, nil, fmt.Errorf("read the reference of document %s: %w", artifactID, err)
	}
	nodes, _, _ := uniqueBlocks(written)
	own := make(map[string]string, len(nodes))
	for _, node := range nodes {
		own[node.ID] = node.Type
	}
	found := make(map[string]pmdoc.BlockTarget, len(targets))
	var elsewhere, issues []string
	var items []text.Ref
	for _, target := range targets {
		nodeID := text.TargetID(target)
		switch {
		case target.Kind == "issue":
			issues = append(issues, nodeID)
			continue
		case pmdoc.ItemKindOf(target) != "":
			items = append(items, target)
			continue
		}
		// A ref_key holds no '#', so the node id's document part ends at its first one.
		if document, _, _ := strings.Cut(nodeID, "#"); document != writing.Document {
			elsewhere = append(elsewhere, nodeID)
		} else if typ, ok := own[target.Block]; ok {
			found[nodeID] = pmdoc.BlockTarget{Types: []string{typ}, Document: writing.Document, Issue: writing.Issue}
		}
	}
	if len(elsewhere) > 0 {
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
	}
	held, err := conversationTargets(ctx, q, items, at)
	if err != nil {
		return pmdoc.BlockWriting{}, nil, err
	}
	aside := len(elsewhere) > 0
	for nodeID, target := range held {
		found[nodeID] = target
		aside = aside || target.Issue != writing.Issue
	}
	if len(issues) > 0 {
		var keys []string
		if err := q.QueryRow(ctx, `select coalesce(array_agg(key), '{}') from issues where key = any($1)`, issues).Scan(&keys); err != nil {
			return pmdoc.BlockWriting{}, nil, fmt.Errorf("read the issues %s points at: %w", artifactID, err)
		}
		for _, key := range keys {
			found[key] = pmdoc.BlockTarget{Issue: key}
		}
		if writing.Descendants, err = IssueDescendants(ctx, q, model.Actor{}, writing.Issue); err != nil {
			return pmdoc.BlockWriting{}, nil, err
		}
	}
	// Only a target in another document or in another issue's item can sit in another issue, so
	// the ancestor walk is paid for only there.
	if aside {
		if writing.Ancestors, err = IssueAncestors(ctx, q, writing.Issue); err != nil {
			return pmdoc.BlockWriting{}, nil, err
		}
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
//
// F is a version of the block's own document (BlockDocumentSQL): for a block of an issue room,
// whose rows are keyed by the room's tree artifact and record that document's own version
// numbers, the document its fragment is, whose versions and indexed versions F is read against
// (T12a of dispatch://INFRA-1253's plan); for any other block, artifact itself. A block of an
// issue room that has stood in more than one fragment (MovedRoomBlockSQL) has rows recording
// versions of more than one document, and no row says which, so the lowest of them need not be a
// version of the document that first held it, nor F a version of the document it stands in now:
// Dispatch knows no first version of it, and every column is null, as for a block with no
// recorded text.
func blockFirstVersion(alias, artifact, block string) string {
	return `left join lateral (
		select t.version,
		       t.version = 1 or exists (
		         select 1 from document_indexed_versions i where i.artifact_id = owner.id and i.version = t.version - 1
		       ) as known,
		       v.authors, v.created_at
		from document_block_texts t
		cross join lateral (select ` + BlockDocumentSQL("t") + ` as id) owner
		left join artifact_versions v on v.artifact_id = owner.id and v.number = t.version
		where t.artifact_id = ` + artifact + ` and t.block_id = ` + block + ` and t.text is not null
		  and not ` + MovedRoomBlockSQL(artifact, block) + `
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
// (blockFirstVersion), last changed by that deletion, with its version's authors and time, part of
// the document at the latest version a block-aware build indexed below removedVersion, the last
// one known to hold it (null when none is), and at the state and visibility the store holds for
// it (storedNode), which the index pass that deleted it stated gone. The caller adds its pin.
// Authors that do not decode (storedActors) fail the read.
func RemovedBlockNode(ctx context.Context, q Queryer, document model.GraphNode, nodeID, blockID string, removedVersion int) (model.GraphNode, error) {
	var first blockFirst
	var removedBy []byte
	var removedAt *time.Time
	var held *int
	var stored []string
	documentID := document.ID
	if err := q.QueryRow(ctx, `
		select `+blockFirstColumns("first")+`, removed.authors, removed.created_at,
		       (select max(i.version) from document_indexed_versions i where i.artifact_id = b.artifact_id and i.version < $3),
		       `+storedNode(BlockNodeKind, "$4")+`
		from (select $1::uuid as artifact_id, $2::text as block_id) b
		`+blockFirstVersion("first", "b.artifact_id", "b.block_id")+`
		left join artifact_versions removed on removed.artifact_id = b.artifact_id and removed.number = $3
	`, documentID, blockID, removedVersion, nodeID).Scan(append(first.targets(), &removedBy, &removedAt, &held, &stored)...); err != nil {
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
	node := model.GraphNode{
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
	}
	if err := setStored(&node, stored); err != nil {
		return model.GraphNode{}, err
	}
	return node, nil
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
