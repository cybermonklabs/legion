package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// deferBlockIndexBlocks is the number of blocks above which a document write leaves the block
// index to the block-index reconciliation instead of paying for it in the request. Not every
// document defers: one of this many blocks or fewer is indexed by its own write, as every
// document was.
//
// The in-request index costs about 46 µs a block on a two-core pod: refs.BlockIndexing.Queue
// writes two rows a block, one in document_blocks and one in document_block_texts. At the bound
// itself a write of a list of 1,999 blocks took 234, 239 and 252 ms against 144, 145 and 157 ms
// for the same list one block longer, which defers - 92 ms for 2,000 blocks - and the same
// difference at 5,000 blocks is 239 ms, 48 µs a block. The heavy write measured for
// sjawhar/legion#2519 agrees: its 8,000-item list body is 8,001 blocks and spends 347 ms in
// IndexDocumentReferences, 43 µs a block, of a request total of 939-1,130 ms against the 1 s that
// write is held to. A bullet list is one block for the list and one for each item's paragraph; the
// list item itself is no block.
//
// The budget this holds the in-request index to is a tenth of that 1 s, 100 ms, which is well
// inside the ±150 ms by which this pod moves the same byte-identical write, so the block index of
// a document that stays inline is never what carries its write past the bound. 100 ms ÷ 48 µs a
// block is 2,083 blocks, and the bound is 2,000 of them, the 92 ms measured above. A document of
// 1,999 blocks is indexed by its own write and pays that there; one of 2,001 pays nothing in the
// request, and its blocks reach the index out of it, through the block-index reconciliation,
// which indexes every document owed one and runs every BlockIndexReconcileInterval, 30 s
// (RunBlockIndexReconciliation, block_index.go). The settlement the write queues indexes them
// only where it writes a version of its own, which it does only where the document changed after
// that write (contentChangedSinceVersion, mutation.go), so an upload nothing follows is the
// reconciliation's. The 8,000-item list pays nothing instead of 347 ms.
const deferBlockIndexBlocks = 2000

// missingVersionNodesBound is the most version nodes a writer's index pass writes in its own
// request for versions of the documents it covers that the release before wrote with none
// (indexRenderedVersion); a pass that finds more leaves them all to the block-index
// reconciliation, which writes them under no request's clock. Before the backfill's version pass
// has run, a document the backfill marked can hold every one of its versions with no node.
//
// A version node costs about 300 µs to write on a two-core pod: the drain
// (graphcensus.MissingVersionNodes) over one document wrote 51 nodes in 15 and 27 ms, 101 in
// 31 and 37 ms, 201 in 38 and 57 ms, 401 in 90 and 121 ms and 801 in 210 and 211 ms, the read of
// the versions with no node included. At 100 versions a write that drains them pays at most about
// 40 ms more, under half of the 100 ms its own block index is held to (deferBlockIndexBlocks), and
// so well inside the ±150 ms by which this pod moves the same byte-identical write.
const missingVersionNodesBound = 100

// IndexDocumentReferences indexes version's markdown, which the calling transaction has just
// stored as the document's version (indexDocument): its references, each mention's excerpt and
// its blocks, all from the rendering of the tree that transaction wrote: an upload's replacement,
// or a seeded spec. A seed renders its tree once. An upload that changes the document renders it
// twice - ReplaceText's render of the replacement, which is the version's markdown, and
// applyLive's render of the fork, which the ledger records - so the excerpts and blocks are cut
// from applyLive's render only when the two agree. A document of more blocks than a write indexes
// in its own request leaves them to the block-index reconciliation (deferBlockIndexBlocks). It
// refuses a context that was not joined.
//
// The version's time, which dates its new references and blocks, is the transaction's now()
// (Ledger.Now): every writer of a version in a joined transaction dates it so, by the column's
// default or explicitly (NextVersion), and its row may not be written yet - its insert can be
// queued for the write phase like the index's own writes.
//
// Its second result is what the block index states in the node and tuple store, the removals a
// person's own write of this transaction made of answered asks' blocks of the document among it
// (removalsOf), which the caller appends under the event it goes on to queue for this version,
// right after that event (graphstore.QueueCaused), in this same write phase.
func (s *Service) IndexDocumentReferences(ctx context.Context, artifactID string, version int, markdown string) (model.ReferenceChanges, graphstore.Uncaused, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, errUnjoined
	}
	ledger := ledgerFrom(ctx)
	versionAt, err := ledger.Now(ctx)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	rendered := ledger.writtenRendering(artifactID)
	if rendered.tree != nil && rendered.markdown != markdown {
		slog.Warn("dispatch: a document version left its reference excerpts and blocks to the reconciliation: the tree its transaction wrote does not render as the version's markdown",
			"artifact", artifactID, "version", version)
		rendered = renderedTree{}
	}
	// A document of an issue room, and the room's tree artifact once a document has joined it, is
	// indexed by the room's one pass over every fragment (indexDocumentInRoom): no pass indexes one
	// fragment of a room alone. The pass defers as one document's does, measured on the blocks it
	// would index, every fragment's. A document this transaction creates takes its room from the
	// ledger (indexedRoom): its row is written in the write phase, after this read.
	room, layout, err := indexedRoom(ctx, tx, artifactID)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	if len(layout) > 0 {
		return s.indexDocumentInRoom(ctx, tx, room, layout, artifactID, rendered, nil, true)
	}
	// An empty version holds no block, whatever the transaction rendered, so one no write of this
	// transaction rendered - an issue's decisions query's version 1, which no fragment holds - is
	// indexed as the empty document: its block index stands at it in its own write, and no
	// block-index reconciliation reads it.
	if rendered.tree == nil && markdown == "" {
		rendered = renderedTree{tree: &pmdoc.Node{Type: "doc"}}
	}
	// Above deferBlockIndexBlocks the write indexes this version's references alone: migration
	// 0122's trigger marked the document owed a block index (document_block_versions.version
	// null) when the version's row was inserted, indexDeferredVersion adds this build's own mark
	// (queueDocumentIndexOwed), and nothing here settles either, so the block-index
	// reconciliation (RunBlockIndexReconciliation) indexes the blocks and the mentions they hold
	// out of the request. The typed passes still run here, so a decision is a node, with its ties
	// and their rows, and so is each requirement, check and plan task, with its text value, its
	// part_of, its row and its serves, in the transaction that accepts it, whatever the size of its
	// document, and a typed block the version removes is gone in it (indexDeferredVersion).
	if held := len(rendered.offsets); rendered.tree != nil && held > deferBlockIndexBlocks {
		logDeferredVersion(artifactID, version, held)
		return s.indexDeferredVersion(ctx, tx, artifactID, refs.OneVersion(artifactID, version, versionAt), markdown, rendered)
	}
	return s.indexDocument(ctx, tx, artifactID, refs.OneVersion(artifactID, version, versionAt), markdown, rendered, nil)
}

// logDeferredVersion logs a version of artifactID, a document that is its own room, whose write
// left its blocks, held of them, to the block-index reconciliation (deferBlockIndexBlocks).
func logDeferredVersion(artifactID string, version, held int) {
	slog.Info("dispatch: a document version left its blocks to the block-index reconciliation: it holds more blocks than a write indexes in its own request",
		"artifact", artifactID, "version", version, "blocks", held, "bound", deferBlockIndexBlocks)
}

// indexDeferredVersion indexes one document version whose blocks its write leaves to the
// block-index reconciliation (deferBlockIndexBlocks), or an issue room under its tree artifact at
// the version of each of its documents (versions), in a transaction holding the document owner's
// lock: its references, as indexUnrenderedVersion indexes them, and its typed blocks through the
// typed passes on their own (planTypedPasses), over rendered, the tree the write left - for a room,
// every fragment joined, each block carrying its fragment's document (rendered.documents), and
// markdown the fragments' own renderings laid end to end (fragmentsText). The targets the passes'
// rows moved are counted with the references'. What the passes state, the removals a person's own
// write of this transaction made of answered asks' blocks of the documents it covers among it
// (removalsOfVersions), is answered for the caller's event, as the block index's is, with what the
// references' reconcile stated of the edges it moved (replaceVersionRefs). The references take
// artifactID's own version, as indexRenderedVersion dates them, read from the one reading of
// markdown the passes read their authored nodes' sources from. Each document of versions takes
// this build's own index mark first, of the Service's marking rule (oweVersionsIndex), as
// indexDocument's unrendered one does, so a version the release before indexes first during a
// roll is drained by this build once the roll ends.
func (s *Service) indexDeferredVersion(ctx context.Context, tx pgx.Tx, artifactID string, versions refs.Versions, markdown string, rendered renderedTree) (model.ReferenceChanges, graphstore.Uncaused, error) {
	if err := oweVersionsIndex(ctx, tx, versions, s.markingRule); err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	own := ownVersion(artifactID, versions)
	passes, err := s.planTypedPasses(ctx, tx, artifactID, versions, markdown, rendered, removalsOfVersions(ctx, versions), "")
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	if err := settleRoomDocumentsOwed(ctx, tx, artifactID, versions); err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	changes, mentions, err := replaceVersionRefs(ctx, tx, artifactID, passes.record, own.Number, own.At, passes.located, passes.plan.Moved(), nil)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	stated := passes.plan.Stated()
	stated.Tuples = append(stated.Tuples, mentions...)
	return changes, stated, nil
}

// typedPasses is what planTypedPasses planned and queued: the passes' one plan, what the newest
// version's row says of them (refs.IndexedVersion), and the mentions of the markdown they read.
type typedPasses struct {
	plan    refs.DecisionIndexing
	record  refs.IndexedVersion
	located []text.Located
}

// planTypedPasses plans and queues, in the read phase of the write that leaves the blocks of
// versions of artifactID to the block-index reconciliation, the two passes that write its typed
// blocks in that write's own transaction whatever its size: the
// typed-node pass (refs.PlanDecisionNodes), which writes each decision and reading, with its ties
// and their rows, and marks gone, with its rows, each typed block the version removes; and, against
// the block index that pass leaves, the requirements, checks and plan tasks of the version, each
// with its text value, its part_of, its row and its serves (refs.DecisionIndexing.PlanAuthored).
// Both plan from rendered, the tree the write left, and markdown, its rendering, against what the
// block index holds (heldBlockIndex), and are written by one step on the write phase's batch, so
// no read of theirs runs in the window that holds the transaction id. beside is what the caller
// states under the same event (refs.PlanDecisionNodes); fragment, where it is not "", is the one
// document of the issue room artifactID whose fragment rendered is (indexRoomLeftOwed).
//
// Every value and tuple the passes state is dated at the pass's own time, the database's
// clock_timestamp() read once the pass holds the owner lock (refs.IndexedVersion's At): the
// record of a version whose row is stored carries it, and one whose row is only queued has it read
// here, once for both passes. A pass that states a requirement node of an issue marks the issue's
// spec its own where the issue is no view (markOwnSpecIndexed), as every pass of the block index
// does.
func (s *Service) planTypedPasses(ctx context.Context, tx pgx.Tx, artifactID string, versions refs.Versions, markdown string, rendered renderedTree, beside graphstore.Uncaused, fragment string) (typedPasses, error) {
	blocks := offsetBlocks(rendered.offsets, rendered.documents)
	record, err := indexedVersionOf(ctx, tx, artifactID, versions)
	if err != nil {
		return typedPasses{}, err
	}
	if record.At.IsZero() {
		if err := tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&record.At); err != nil {
			return typedPasses{}, fmt.Errorf("read the time of the typed blocks of %s: %w", artifactID, err)
		}
	}
	pointers := blockPointers(blocks, rendered.tree)
	facts, err := refs.DecisionFacts(ctx, tx, artifactID, record, versions, blocks, rendered.tree, pointers,
		typedSources(ctx, tx, artifactID, rendered.tree, nil))
	if err != nil {
		return typedPasses{}, err
	}
	index, err := heldBlockIndex(ctx, tx, artifactID)
	if err != nil {
		return typedPasses{}, err
	}
	decisions, err := refs.PlanDecisionNodes(ctx, tx, index, artifactID, facts, beside, fragment)
	if err != nil {
		return typedPasses{}, err
	}
	located := refs.NewReader(ctx, tx, record.Writer, "artifact", artifactID, s.serverURL).Mentions(markdown)
	// A row the passes insert takes the transaction's now() as its created_at, which dates the
	// membership it projects: the ledger's, read once a transaction, where one queues tx's writes.
	var now time.Time
	if ledger := ledgerFrom(ctx); ledger != nil && ledger.writer != nil {
		now, err = ledger.Now(ctx)
	} else {
		now, err = readNow(ctx, tx)
	}
	if err != nil {
		return typedPasses{}, err
	}
	plan, err := decisions.PlanAuthored(ctx, tx, record, versions, markdown, blocks, located, pointers, now)
	if err != nil {
		return typedPasses{}, err
	}
	recordBlockIndex(ctx, tx, artifactID, plan.Held())
	if err := write(ctx, tx, plan.Queue); err != nil {
		return typedPasses{}, err
	}
	if err := markOwnSpecIndexed(ctx, tx, record.Issue, blocks); err != nil {
		return typedPasses{}, err
	}
	return typedPasses{plan: plan, record: record, located: located}, nil
}

// indexDocument indexes one document version, or one issue room at the version of each of its
// documents (refs.Versions), in its writer's transaction, which holds the document owner's lock,
// from the render the version's markdown came from: indexRenderedVersion with rendered's blocks.
// The blocks are rendered.offsets; GET …/blocks lists one block per offset of the same renderer
// (renderedBlocks), so both list one set of blocks. A version with no rendering - an upload whose
// replacement changed nothing, which writes no tree, or one whose blocks the write deferred
// (deferBlockIndexBlocks) - is indexUnrenderedVersion, which indexes no blocks: migration 0122's
// trigger left the document owed when the version's row was inserted, for the block-index
// reconciliation, which reads the document itself. settled is the authors a settlement read of
// the blocks it settles (typedSources), nil for any other write. What it answers for the caller's
// event carries the removals a person's own write of this transaction made of answered asks'
// blocks of the document (removalsOf): planned into the index pass's own write where one runs,
// and queued on their own where none does (queueRemovals).
//
// That trigger's mark is the null version, which the release before this one reads and drains
// too, so during a roll a pod of that release can index this version first and write its rows
// naming no node. Such a version therefore takes this build's own mark as well, in this same
// transaction (oweVersionsIndex), which that release neither reads nor clears: this build's
// reconciliation then indexes the version after it, names every row's node and states what each
// projects (refs.PlanBlocks). A version whose blocks the write deferred takes it in
// indexDeferredVersion.
//
// versions is what its writer wrote in this transaction, whose nodes that writer writes after the
// index (WriteVersionNode), so a pass that drains this build's mark leaves them out of the version
// nodes it writes (indexRenderedVersion's written).
func (s *Service) indexDocument(ctx context.Context, tx pgx.Tx, artifactID string, versions refs.Versions, markdown string, rendered renderedTree, settled *askBlockSources) (model.ReferenceChanges, graphstore.Uncaused, error) {
	if rendered.tree == nil {
		if err := oweVersionsIndex(ctx, tx, versions, s.markingRule); err != nil {
			return model.ReferenceChanges{}, graphstore.Uncaused{}, err
		}
		_, newest := versions.Newest(artifactID)
		changes, mentions, err := s.indexUnrenderedVersion(ctx, tx, artifactID, versions, versions[artifactID].Number, newest.At, markdown)
		if err != nil {
			return model.ReferenceChanges{}, graphstore.Uncaused{}, err
		}
		removals, err := queueRemovals(ctx, tx, artifactID)
		if err != nil {
			return model.ReferenceChanges{}, graphstore.Uncaused{}, err
		}
		removals.Tuples = append(removals.Tuples, mentions...)
		return changes, removals, nil
	}
	return s.indexRenderedVersion(ctx, tx, artifactID, versions, versions, false, markdown, offsetBlocks(rendered.offsets, rendered.documents), rendered.tree, settled)
}

// oweVersionsIndex owes each document of versions this build's block index of the marking rule
// rule (queueDocumentIndexOwed), in ascending id, the order every writer of document_block_versions
// rows of several documents takes: one document for a version of a document that is its own room,
// every document of the room whose version the pass covers for an issue room, whose
// reconciliation then indexes the room (indexOwedRoom).
func oweVersionsIndex(ctx context.Context, tx pgx.Tx, versions refs.Versions, rule int) error {
	for _, document := range slices.Sorted(maps.Keys(versions)) {
		if err := queueDocumentIndexOwed(ctx, tx, document, rule); err != nil {
			return err
		}
	}
	return nil
}

// ownVersion is artifactID's own version of versions, the one its references are dated at: for an
// issue room whose tree holds no version yet, no number and the newest time the pass covers.
func ownVersion(artifactID string, versions refs.Versions) refs.Version {
	own, held := versions[artifactID]
	if !held {
		_, newest := versions.Newest(artifactID)
		own = refs.Version{At: newest.At}
	}
	return own
}

// removalsOfVersions is removalsOf for every document a pass covers (versions), in id order,
// joined: the one document's for a pass over one, each fragment's document's for an issue room,
// whose removals are held under the document holding each ask's block (answerDroppedAnsweredAsks).
func removalsOfVersions(ctx context.Context, versions refs.Versions) graphstore.Uncaused {
	var held []graphstore.Uncaused
	for _, document := range slices.Sorted(maps.Keys(versions)) {
		if removals := removalsOf(ctx, document); len(removals.Nodes) > 0 || len(removals.Tuples) > 0 {
			held = append(held, removals)
		}
	}
	switch len(held) {
	case 0:
		return graphstore.Uncaused{}
	case 1:
		return held[0]
	}
	return graphstore.Joined(held...)
}

// deferredBlockIndexKey marks the context of a write whose version defers its block index above
// deferBlockIndexBlocks (WithDeferredBlockIndex).
type deferredBlockIndexKey struct{}

// WithDeferredBlockIndex marks ctx, a document write's context, as a write whose version leaves its
// blocks to the block-index reconciliation where the document, or its issue room, holds more of
// them than a write indexes in its own request, as an upload's and an issue create's do
// (IndexDocumentReferences): SnapshotVersion, NamedVersion and RebuildDocument read the mark into
// the version's write (versionWrite.deferLarge). A route marks it where nothing in its transaction
// reads the version's block index or its record after the version write: the edit route, an
// anchored comment's or ask's pin, the accept of a suggestion, the named-version route and the
// rebuild. A settlement's version and the decisions step's are written unmarked.
func WithDeferredBlockIndex(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferredBlockIndexKey{}, true)
}

// deferredBlockIndex reports whether the route marked ctx (WithDeferredBlockIndex).
func deferredBlockIndex(ctx context.Context) bool {
	marked, _ := ctx.Value(deferredBlockIndexKey{}).(bool)
	return marked
}

// indexVersion is indexDocument for a version of artifactID its writer wrote outside an issue
// room's settlement (writeVersionTx): the document's own pass where it is a room of its own with
// no other fragment, and otherwise its room's one pass over every fragment (indexDocumentInRoom).
// Where deferLarge is set (versionWrite.deferLarge), a version of a document that is its own room
// holding more blocks than a write indexes in its own request leaves them to the block-index
// reconciliation, its references and typed nodes indexed here (indexDeferredVersion), as
// IndexDocumentReferences leaves an upload's, and the room's pass defers as it does for an upload.
func (s *Service) indexVersion(ctx context.Context, tx pgx.Tx, artifactID string, versions refs.Versions, markdown string, rendered renderedTree, settled *askBlockSources, deferLarge bool) (model.ReferenceChanges, graphstore.Uncaused, error) {
	room, layout, err := indexedRoom(ctx, tx, artifactID)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	if len(layout) > 0 {
		return s.indexDocumentInRoom(ctx, tx, room, layout, artifactID, rendered, settled, deferLarge)
	}
	if held := len(rendered.offsets); deferLarge && rendered.tree != nil && held > deferBlockIndexBlocks {
		logDeferredVersion(artifactID, versions[artifactID].Number, held)
		return s.indexDeferredVersion(ctx, tx, artifactID, versions, markdown, rendered)
	}
	return s.indexDocument(ctx, tx, artifactID, versions, markdown, rendered, settled)
}

// indexDocumentInRoom indexes, in tx, the issue room room, of layout, after a version of document -
// one of its documents, or its tree artifact - that its writer wrote outside the room's
// settlement: the room's one pass under its tree artifact (indexRoom), over every fragment, each at
// its own document's latest version as this transaction leaves it (latestRoomVersions), the one
// just written for document, its text each fragment's own rendering laid end to end in the room's
// order (fragmentsText); the deferred path hands indexDeferredVersion that same text. document's
// fragment is the tree the version was rendered from (rendered), and every other fragment the
// room's own tree of it (roomParts, roomTree: the
// transaction's fork where it writes the room, else the room this server holds or the stored
// room; what the transaction wrote, for a room it creates), which holds the very block ids its
// blocks were indexed under; a stored rendering read back would mint a new id for each block whose
// id the rendering does not carry, and the pass would state that block anew under the writer's
// event. A fragment whose document holds no version yet is empty. Where a fragment does not read,
// or another fragment reads otherwise than its document's latest version - a browser's edit the
// room's settlement has not versioned yet - or document's own does where its writer rendered no
// tree of it (an upload whose replacement changed nothing, or one whose written tree did not
// render as the version's markdown: IndexDocumentReferences), the room is left owed and its blocks
// are not indexed here, as the block-index reconciliation leaves a room live ahead (indexOwedRoom):
// the version trigger marked document owed when its row was inserted, and the reconciliation, or
// the settlement that versions that edit, indexes the room. Only document's typed blocks are
// written then, where its writer's tree renders as its version (indexRoomLeftOwed). What the pass
// stated of every document is answered together, for the event the writer appends. Where
// deferLarge is set - an upload's or a create's write (IndexDocumentReferences), or a version write
// its route marked (WithDeferredBlockIndex, indexVersion) - and the room holds
// more blocks than a write indexes in its own request (deferBlockIndexBlocks), the blocks are left
// to the block-index reconciliation, which indexes the room whole (indexOwedRoom), and the room's
// references and typed nodes are indexed here, as one document's deferred version's are
// (indexDeferredVersion). document's latest version is the one its writer just wrote, whose node
// that writer writes after the index, so it is the version a pass that drains this build's mark
// leaves out (indexRenderedVersion's written).
func (s *Service) indexDocumentInRoom(ctx context.Context, tx pgx.Tx, room string, layout roomLayout, document string, rendered renderedTree, settled *askBlockSources, deferLarge bool) (model.ReferenceChanges, graphstore.Uncaused, error) {
	documents := layout.documents(room)
	latest, err := latestRoomVersions(ctx, tx, documents)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	parts, err := s.roomParts(ctx, room, layout)
	switch {
	case errors.Is(err, ErrDocumentUnloadable), errors.Is(err, ErrDocOutsideSchema):
		slog.Warn("dispatch: a version of a document of an issue room left the room to the block-index reconciliation: a fragment of the room does not read",
			"room", room, "document", document, "error", err)
		return s.indexRoomLeftOwed(ctx, tx, room, document, documents, latest, rendered)
	case err != nil:
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	versions := refs.Versions{}
	// Each fragment's own rendering and its blocks' ranges in it, the text the pass indexes laid end
	// to end (fragmentsText): document's the writer's, every other one rendered here to compare it.
	markdowns := make([]string, len(documents))
	offsets := make([][]pmdoc.BlockOffset, len(documents))
	for index, id := range documents {
		version := latest[index]
		switch {
		case version.number == 0:
			parts[index] = &pmdoc.Node{Type: "doc"}
			continue
		case id == document && rendered.tree != nil:
			parts[index] = rendered.tree
			markdowns[index], offsets[index] = rendered.markdown, rendered.offsets
		default:
			// A fragment no writer of this transaction rendered, document's own among them where
			// it wrote no tree, is indexed only where it reads as its document's latest version.
			markdown, held, err := renderFragment(parts[index])
			if err != nil || markdown != version.markdown {
				slog.Info("dispatch: a version of a document of an issue room left the room to the block-index reconciliation: a fragment of the room reads otherwise than its latest version",
					"room", room, "document", document, "fragment", id, "version", version.number, "error", err)
				return s.indexRoomLeftOwed(ctx, tx, room, document, documents, latest, rendered)
			}
			markdowns[index], offsets[index] = markdown, held
		}
		versions[id] = refs.Version{Number: version.number, At: version.at}
	}
	joined, counts := joinFragments(parts)
	// Each fragment reads as its document's latest version here, so a version that holds no text
	// is a fragment that renders as none, which holds no block (fragmentsText).
	markdown, laid := fragmentsText(markdowns, offsets)
	documentsOf := layout.blockDocuments(room, joined, counts)
	blocks := offsetBlocks(laid, documentsOf)
	if deferLarge && len(blocks) > deferBlockIndexBlocks {
		slog.Info("dispatch: a version of a document of an issue room left the room's blocks to the block-index reconciliation: the room holds more blocks than a write indexes in its own request",
			"room", room, "document", document, "blocks", len(blocks), "bound", deferBlockIndexBlocks)
		return s.indexDeferredVersion(ctx, tx, room, versions, markdown,
			renderedTree{tree: joined, markdown: markdown, offsets: laid, documents: documentsOf})
	}
	index, err := s.indexRoom(ctx, tx, room, versions, refs.Versions{document: versions[document]}, false, markdown, blocks, joined, settled)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	facts := make([]graphstore.Uncaused, 0, len(index.facts)+1)
	for _, id := range slices.Sorted(maps.Keys(index.facts)) {
		facts = append(facts, index.facts[id])
	}
	facts = append(facts, index.references)
	return index.changes, graphstore.Joined(facts...), nil
}

// indexRoomLeftOwed writes, in tx, the typed blocks of the version of document, a document of the
// issue room room or its tree artifact, whose write left the room owed its block index because a
// fragment of the room does not read or reads otherwise than its document's latest version
// (indexDocumentInRoom): the decisions, readings, requirements, checks and plan tasks of
// document's own fragment, and the gone mark of each typed block of that fragment the version
// removes, through the typed passes on their own (planTypedPasses), as a version above the bound
// writes them. It writes them only where document's writer rendered the
// tree of its version (rendered): where it rendered none - an upload whose replacement changed
// nothing, or one whose written tree did not render as the version's markdown - the tree the room
// holds and the version may disagree, and the typed blocks wait for the reconciliation with the
// rest, which the coverage and tree reads name as owed (api's owed_documents). No other fragment
// is read, and no reference of the room is reconciled: the reconciliation indexes the room whole.
// What the passes state is answered for the writer's event, with the targets their rows moved.
func (s *Service) indexRoomLeftOwed(ctx context.Context, tx pgx.Tx, room, document string, documents []string, latest []roomVersion, rendered renderedTree) (model.ReferenceChanges, graphstore.Uncaused, error) {
	at := slices.Index(documents, document)
	if rendered.tree == nil || at < 0 || latest[at].number == 0 {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, nil
	}
	var documentsOf map[string]string
	if document != room {
		documentsOf = make(map[string]string, len(rendered.offsets))
		for _, offset := range rendered.offsets {
			documentsOf[offset.ID] = document
		}
	}
	versions := refs.Versions{document: {Number: latest[at].number, At: latest[at].at}}
	passes, err := s.planTypedPasses(ctx, tx, room, versions, rendered.markdown,
		renderedTree{tree: rendered.tree, markdown: rendered.markdown, offsets: rendered.offsets, documents: documentsOf}, graphstore.Uncaused{}, document)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	changes, err := refs.CountedChanges(ctx, tx, passes.plan.Moved())
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	return changes, passes.plan.Stated(), nil
}

// roomParts is each fragment of room, of layout, in the order of layout.documents, as
// indexDocumentInRoom reads them: the room's own tree (roomTree) parted by its fragments; and, for
// a room this transaction creates (an issue born converted's, Ledger.RegisterPendingArtifact),
// what the transaction seeded or wrote into each fragment (Ledger.writtenRendering), a fragment it
// wrote nothing into being empty. Such a room holds nothing but what its transaction wrote, and
// its tree artifact has no row a read before the write phase sees, which the room's own read
// locks the owner by (joinRead).
func (s *Service) roomParts(ctx context.Context, room string, layout roomLayout) ([]*pmdoc.Node, error) {
	documents := layout.documents(room)
	if _, created := pendingArtifact(ctx, room); created {
		ledger := ledgerFrom(ctx)
		parts := make([]*pmdoc.Node, len(documents))
		for index, id := range documents {
			if parts[index] = ledger.writtenRendering(id).tree; parts[index] == nil {
				parts[index] = &pmdoc.Node{Type: "doc"}
			}
		}
		return parts, nil
	}
	held, counts, err := s.roomTree(ctx, room, layout)
	if err != nil {
		return nil, err
	}
	parts := layout.parts(held, counts)
	if parts == nil {
		return nil, fmt.Errorf("the tree of room %s does not part into its %d fragments", room, len(documents))
	}
	return parts, nil
}

// offsetBlocks is the blocks a render's offsets name, as the block index takes them, each with
// the attributes its opening line wrote, which the index states a plan node's size from
// (refs.AttributePlanSize), and carrying the document documents names for it where the render is
// an issue room's (renderedTree's documents).
func offsetBlocks(offsets []pmdoc.BlockOffset, documents map[string]string) []model.ArtifactBlock {
	blocks := make([]model.ArtifactBlock, len(offsets))
	for index, offset := range offsets {
		blocks[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To, Attributes: offset.Attributes, Document: documents[offset.ID]}
	}
	return blocks
}

// indexRenderedVersion indexes one document version, or one issue room under its tree artifact at
// the version of each of its documents (versions), in a transaction holding the document owner's
// lock: its references, the excerpt of each mention, and its blocks with the links each holds
// (refs.PlanBlocks), all from one reading of markdown over that transaction
// (refs.Reader.Mentions), so the document's own edges, its blocks' and their excerpts read the
// bare keys, `#id`s and short ids it writes alike. The reading resolves the short ids for the
// version's writer, the one its block index states every node and tuple as written by
// (indexedVersionOf). blocks index into markdown, the render they came from; tree, when the
// caller holds it, is the tree markdown was rendered from, and nil otherwise, for which markdown
// is read back once (versionTree). The event the caller appends names the counted targets the
// document's references and their excerpts, its blocks and its blocks' links moved
// (replaceVersionRefs), since a node is counted once for each block citing it. The reading, the
// index's plans and the counted targets are worked out where it is called; its writes go through
// write, in the order the index makes them.
//
// The typed-node pass runs beside the block index, over the facts both take (refs.DecisionFacts),
// in the one plan (refs.PlanBlocks): the index writes every block's node, part_of and block_refs
// row, those of the decisions, readings and ask blocks among them, and the typed nodes' values and
// their authors. A plan that lost the revision fence to a newer version writes neither, since what
// it would state is what that version already stated. settled is the authors a settlement read of
// the blocks it settles, nil for any other caller (typedSources).
//
// Its second result is what the pass states in the node and tuple store, the removals a person's
// own write of this transaction made of answered asks' blocks of the documents it covers among it
// (removalsOfVersions, planned into the pass's own write whether or not it won the fence), which
// the caller appends under that same event (graphstore.QueueCaused): the pass writes the block
// nodes itself, because the foreign key on the projection rows needs them, and holds its tuples
// back because on the request path the event does not exist yet. The tuples of the edges the
// document's own references moved go with them (replaceVersionRefs, Replacement.ArtifactMentions).
//
// The typed nodes' facts take the version of each block's own document, as the blocks' do. The
// document's own references - artifactID's, an issue room's tree artifact's, which belong to no
// one document of the room - take artifactID's own version and its time (ownVersion). Their write
// settles artifactID's re-index queue row, and for an issue room each document's the pass read is
// settled beside it (settleRoomDocumentsOwed).
//
// A pass that drains a mark, or that reads the store's statement of its document as stale, also
// writes, in this transaction, the node of each version of the documents it covers that holds none
// (graphcensus.MissingVersionNodes), read here, in the read phase, and queued after the pass's own
// write: the release before this one writes no version node, and once the marks are cleared, or
// the document stands stated at its version, no later pass reaches such a version. tick is set by
// the block-index reconciliation's passes (indexOwedDocument, indexOwedRoom), which run only on a
// document or a room they listed as owed by either mark - this build's (store_owed_at), or the
// null version migration 0122's trigger
// sets, which is all the release before leaves on a version whose blocks it deferred or which it
// wrote with no rendering - so such a pass writes them wherever it wins, every one, and leaves
// none out, since it writes no version. Any other pass is a writer's, and writes them where it
// drains this build's mark or reads the store's statement of its document stale
// (refs.BlockIndexing.Corrects): a version the release before wrote and indexed after this build
// stated the document leaves no mark, and a version of this build's written before any tick reads
// that release's event leaves the document stated at its version, which the tick's marking then
// passes over. written is the versions of versions its transaction wrote, which are left out, each
// one's node being its writer's, written after the index with the version's own writer, time and
// event (WriteVersionNode). A writer's pass writes at most missingVersionNodesBound of them: where
// more are missing it writes none, and owes each document the pass covers its block index again in
// this transaction (oweVersionsIndex), so the reconciliation's next tick indexes them again and
// writes those nodes.
//
// A pass that wins the fence and states a requirement node of the issue the documents belong to
// marks that issue's spec its own, in this transaction, where ViewOf answers here that the issue
// is no view (markOwnSpecIndexed): the writer's pass, a settlement's room pass and the
// reconciliation's alike.
func (s *Service) indexRenderedVersion(ctx context.Context, tx pgx.Tx, artifactID string, versions, written refs.Versions, tick bool, markdown string, blocks []model.ArtifactBlock, tree *pmdoc.Node, settled *askBlockSources) (model.ReferenceChanges, graphstore.Uncaused, error) {
	own := ownVersion(artifactID, versions)
	record, err := indexedVersionOf(ctx, tx, artifactID, versions)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	located := refs.NewReader(ctx, tx, record.Writer, "artifact", artifactID, s.serverURL).Mentions(markdown)
	tree = versionTree(artifactID, markdown, blocks, tree)
	pointers := blockPointers(blocks, tree)
	facts, err := refs.DecisionFacts(ctx, tx, artifactID, record, versions, blocks, tree, pointers, typedSources(ctx, tx, artifactID, tree, settled))
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	// The blocks are indexed before the document's own references are reconciled: a first
	// indexing dates a block's mention from the document's refs row for its target, which a
	// surviving target already holds and a new one gets with this same time.
	moved, indexing, err := planBlockIndex(ctx, tx, s.markingRule, refs.VersionBlocks{
		ArtifactID: artifactID, Record: record, Versions: versions,
		Markdown: markdown, Blocks: blocks, Located: located, Pointers: pointers,
		Typed: facts, Beside: removalsOfVersions(ctx, versions),
	})
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	if err := write(ctx, tx, func(b *store.WriteBatch) error {
		indexing.Queue(b)
		return nil
	}); err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	stated := indexing.Stated()
	// A pass that states a requirement node of the issue marks the issue's spec its own where the
	// issue is no view (markOwnSpecIndexed), whatever wrote the requirement. A plan that lost the
	// revision fence states nothing, so it marks nothing. A pass that wins it records each version
	// it covers (recordStatedVersions), whose version_renders row its transaction writes once its
	// write phase has run where that version is a query version this build's version write made
	// (readCovered): the request's pass, a room's pass for every fragment's newest version, and the
	// reconciliation's.
	if indexing.Won() {
		if err := markOwnSpecIndexed(ctx, tx, record.Issue, blocks); err != nil {
			return model.ReferenceChanges{}, graphstore.Uncaused{}, err
		}
		recordStatedVersions(ctx, tx, versions)
	}
	if tick && indexing.Won() || indexing.Corrects() {
		bound := missingVersionNodesBound
		if tick {
			bound = 0
		}
		drain, planned, err := graphcensus.PlanMissingVersionNodes(ctx, tx, slices.Sorted(maps.Keys(versions)), written, bound)
		if err != nil {
			return model.ReferenceChanges{}, graphstore.Uncaused{}, err
		}
		if planned {
			if err := write(ctx, tx, func(b *store.WriteBatch) error {
				drain.Queue(b)
				return nil
			}); err != nil {
				return model.ReferenceChanges{}, graphstore.Uncaused{}, err
			}
		} else {
			slog.Info("dispatch: a version write left the version nodes the release before did not write to the block-index reconciliation: more are missing than a write writes in its own request",
				"artifact", artifactID, "bound", missingVersionNodesBound)
			if err := oweVersionsIndex(ctx, tx, versions, s.markingRule); err != nil {
				return model.ReferenceChanges{}, graphstore.Uncaused{}, err
			}
		}
	}
	if err := settleRoomDocumentsOwed(ctx, tx, artifactID, versions); err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	changes, mentions, err := replaceVersionRefs(ctx, tx, artifactID, record, own.Number, own.At, located, moved, refs.MentionExcerpts(markdown, blocks, located))
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	stated.Tuples = append(stated.Tuples, mentions...)
	return changes, stated, nil
}

// versionTree is the tree a version's markdown was rendered from: tree, when its caller holds it,
// and otherwise markdown read back, which only a version holding blocks whose pointers or typed
// nodes the index writes needs (refs.HoldsTypedBlocks). A version whose markdown does not read
// back is indexed with no pointers and no typed facts, as one the server cannot read is with no
// blocks.
func versionTree(artifactID, markdown string, blocks []model.ArtifactBlock, tree *pmdoc.Node) *pmdoc.Node {
	if tree != nil || len(blocks) == 0 || (!pmdoc.HasLinkKinds() && !refs.HoldsTypedBlocks(blocks)) {
		return tree
	}
	read, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		slog.Warn("dispatch: a document version's block pointers and typed nodes are not indexed: its markdown does not read back",
			"artifact", artifactID, "error", err)
		return nil
	}
	return read
}

// blockPointers is each `block[]` value of tree (pmdoc.BlockPointers) as the block index writes
// it: none when the schema declares no `block[]` attribute, the version has no blocks, or no tree
// was read.
func blockPointers(blocks []model.ArtifactBlock, tree *pmdoc.Node) []refs.Pointer {
	if len(blocks) == 0 || tree == nil || !pmdoc.HasLinkKinds() {
		return nil
	}
	found := pmdoc.BlockPointers(tree)
	pointers := make([]refs.Pointer, len(found))
	for index, pointer := range found {
		pointers[index] = refs.Pointer{Block: pointer.Block, BlockType: pointer.BlockType, Kind: pointer.Attribute, Target: pointer.Target, Targets: pointer.Targets}
	}
	return pointers
}

// indexUnrenderedVersion indexes one document version with no rendering, written at versionAt, in
// a transaction holding the document owner's lock: its references alone, their short ids resolved
// for the version's writer (indexedVersionOf). It indexes no blocks and marks the document's
// mentions owed an excerpt (refs.ClearExcerpts) for the excerpt reconciliation, which reads the
// document itself. Its second result is what the references' reconcile stated of the edges it
// moved, for the caller's event (indexRenderedVersion's).
func (s *Service) indexUnrenderedVersion(ctx context.Context, tx pgx.Tx, artifactID string, versions refs.Versions, version int, versionAt time.Time, markdown string) (model.ReferenceChanges, []graphstore.UncausedTuple, error) {
	record, err := indexedVersionOf(ctx, tx, artifactID, versions)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	reader := refs.NewReader(ctx, tx, record.Writer, "artifact", artifactID, s.serverURL)
	return replaceVersionRefs(ctx, tx, artifactID, record, version, versionAt, reader.Mentions(markdown), nil, nil)
}

// replaceVersionRefs reconciles the document's references, dated versionAt, to the mentions
// located in its version, stores excerpts on them (nil: marks them owed, refs.ClearExcerpts), and
// counts the targets moved, which its blocks and their mentions moved, as well. A document mention
// shows the block holding it as its excerpt, so a target whose kept edge now stores another block
// is counted beside the ones whose edges the version inserted or deleted (refs.ExcerptsMoved,
// worked out here, before the reconciliation and the excerpts are written, in that order, by one
// write). It answers beside the counts the tuple of each edge the reconcile inserts and the
// retraction of each it deletes (refs.Replacement.ArtifactMentions, signed by record's writer),
// which its caller appends with the rest of the version's index under the version's event.
func replaceVersionRefs(ctx context.Context, tx pgx.Tx, artifactID string, record refs.IndexedVersion, version int, versionAt time.Time, located []text.Located, moved []refs.Target, excerpts map[refs.Target]model.GraphExcerpt) (model.ReferenceChanges, []graphstore.UncausedTuple, error) {
	replacement, err := planReplacement(ctx, tx, "artifact", artifactID, text.Refs(located), &versionAt)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	mentions, err := replacement.ArtifactMentions(ctx, tx, record)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	counted := slices.Concat(replacement.Changed(), moved)
	writeExcerpts := func(b *store.WriteBatch) { refs.ClearExcerpts(b, artifactID) }
	if excerpts != nil {
		recorded, err := refs.ExcerptsMoved(ctx, tx, artifactID, version, replacement.Held(), excerpts)
		if err != nil {
			return model.ReferenceChanges{}, nil, err
		}
		counted = append(counted, recorded...)
		writeExcerpts = func(b *store.WriteBatch) { refs.QueueRecordExcerpts(b, artifactID, version, excerpts) }
	}
	changes, err := refs.CountedChanges(ctx, tx, counted)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	return changes, mentions, write(ctx, tx, func(b *store.WriteBatch) error {
		replacement.Queue(b)
		writeExcerpts(b)
		return nil
	})
}

// planBlockIndex plans the block index of version, the versions of a document with its block
// pointers and its typed nodes (refs.PlanBlocks, over version.Typed, the typed-node pass's facts
// read from version.Record), at the marking rule rule of the build planning it, with
// version.Beside, a write planned for the same event that no index pass of its own carries - the
// removals of answered asks' blocks a person's own write made (removalsOfVersions) - planned into
// it. It returns the targets it moves, the cited blocks it changes included
// (refs.BlockIndexing.Cited), and the plan: its Queue writes it, its Stated is what it states in
// the node and tuple store, held for the event the caller appends for the version, and its Won
// and Marked say whether it won the revision fence and drains this build's mark. It plans against
// what the index holds (heldBlockIndex), and records what this one leaves for a later indexing of
// the same document (recordBlockIndex).
func planBlockIndex(ctx context.Context, tx pgx.Tx, rule int, version refs.VersionBlocks) ([]refs.Target, refs.BlockIndexing, error) {
	index, err := heldBlockIndex(ctx, tx, version.ArtifactID)
	if err != nil {
		return nil, refs.BlockIndexing{}, err
	}
	now, err := transactionNow(ctx, tx)
	if err != nil {
		return nil, refs.BlockIndexing{}, err
	}
	indexing, err := refs.PlanBlocks(ctx, tx, index, rule, now, version)
	if err != nil {
		return nil, refs.BlockIndexing{}, err
	}
	cited, err := indexing.Cited(ctx, tx)
	if err != nil {
		return nil, refs.BlockIndexing{}, err
	}
	recordBlockIndex(ctx, tx, version.ArtifactID, indexing.Held())
	return slices.Concat(indexing.Moved(), cited), indexing, nil
}

// heldBlockIndex is what artifactID's block index holds as a plan of it reads it: as the
// transaction's own queued indexing of the document leaves it, when there is one
// (recordBlockIndex), else as read in tx.
func heldBlockIndex(ctx context.Context, tx pgx.Tx, artifactID string) (refs.BlockIndex, error) {
	if ledger := queueFor(ctx, tx); ledger != nil {
		if index, held := ledger.blockIndexes[artifactID]; held {
			return index, nil
		}
	}
	return refs.ReadBlockIndex(ctx, tx, artifactID)
}

// recordBlockIndex records index as what artifactID's block index holds once the plan this
// transaction has just queued is written, which a later plan of the same document in the
// transaction reads (heldBlockIndex). Nothing is recorded where no ledger queues tx's writes: the
// plan's writes then ran at once, and a later read finds them in the table.
func recordBlockIndex(ctx context.Context, tx pgx.Tx, artifactID string, index refs.BlockIndex) {
	ledger := queueFor(ctx, tx)
	if ledger == nil {
		return
	}
	if ledger.blockIndexes == nil {
		ledger.blockIndexes = make(map[string]refs.BlockIndex)
	}
	ledger.blockIndexes[artifactID] = index
}

// indexedVersionOf is what the version a pass over artifactID at versions states its facts in the
// name of says of its block index (refs.IndexedVersion): the version of the document versions
// wrote last (Versions.Newest: artifactID's own for one document, or for an issue room the newest
// of its documents'), whose authors and first author are the pass's, read with artifactID's own
// scope, band and issue; and the document's ref_key, from its address as this transaction leaves
// it (addressOfDocument). Where that version is the one this transaction queued and has not
// written, which no read of the table answers yet, its authors are the queued row's, and the
// scope and band of a document this transaction creates are its registered address's
// (Ledger.RegisterPendingArtifact, refs.DocumentAddress.Band); else everything is read from the
// stored rows (refs.ReadIndexedVersion).
func indexedVersionOf(ctx context.Context, tx pgx.Tx, artifactID string, versions refs.Versions) (refs.IndexedVersion, error) {
	address, err := addressOfDocument(ctx, tx, artifactID)
	if err != nil {
		return refs.IndexedVersion{}, err
	}
	lead, newest := versions.Newest(artifactID)
	version := newest.Number
	if ledger := queueFor(ctx, tx); ledger != nil {
		if pending, queued := ledger.latest[lead]; queued && pending.version.Number == version {
			record := refs.IndexedVersion{RefKey: address.RefKey, Issue: address.IssueKey, Authors: pending.version.Authors}
			if len(pending.version.Authors) > 0 {
				record.Writer = pending.version.Authors[0]
			}
			if _, created := pendingArtifact(ctx, artifactID); created {
				record.Project, record.Parties = graphstore.SessionScope(address.Project, address.Session)
				record.Band = address.Band
				return record, nil
			}
			stored, err := refs.ReadIndexedVersion(ctx, tx, artifactID, lead, version)
			if err != nil {
				return refs.IndexedVersion{}, err
			}
			record.Project, record.Parties, record.Band = stored.Project, stored.Parties, stored.Band
			return record, nil
		}
	}
	record, err := refs.ReadIndexedVersion(ctx, tx, artifactID, lead, version)
	if err != nil {
		return refs.IndexedVersion{}, err
	}
	record.RefKey = address.RefKey
	return record, nil
}

// planReplacement plans the reconciliation of fromKind/fromID's references with found, a new edge
// dated insertedAt (nil: the transaction's time), against the source's edges as the transaction's
// own queued reconciliation of it leaves them, when there is one, else as read in tx, and records
// what this one leaves.
func planReplacement(ctx context.Context, tx pgx.Tx, fromKind, fromID string, found []text.Ref, insertedAt *time.Time) (refs.Replacement, error) {
	ledger := queueFor(ctx, tx)
	source := refs.Target{Kind: fromKind, ID: fromID}
	var replacement refs.Replacement
	held, known := []refs.Target(nil), false
	if ledger != nil {
		held, known = ledger.referenced[source]
	}
	if known {
		replacement = refs.PlanReplaceFrom(held, fromKind, fromID, found, insertedAt)
	} else {
		var err error
		if replacement, err = refs.PlanReplace(ctx, tx, fromKind, fromID, found, insertedAt); err != nil {
			return refs.Replacement{}, err
		}
	}
	if ledger != nil {
		if ledger.referenced == nil {
			ledger.referenced = make(map[refs.Target][]refs.Target)
		}
		ledger.referenced[source] = replacement.Held()
	}
	return replacement, nil
}

// planReplaceCounted is the read half of a reconciliation this package writes: it plans the
// reconciliation of fromKind/fromID's references with found (planReplacement), resolves the
// counted targets it and moved move, and returns those with the step that writes it.
func planReplaceCounted(
	ctx context.Context,
	tx pgx.Tx,
	fromKind, fromID string,
	found []text.Ref,
	insertedAt *time.Time,
	moved []refs.Target,
) (model.ReferenceChanges, WriteStep, error) {
	replacement, err := planReplacement(ctx, tx, fromKind, fromID, found, insertedAt)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	changes, err := refs.CountedChanges(ctx, tx, slices.Concat(replacement.Changed(), moved))
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	return changes, func(b *store.WriteBatch) error {
		replacement.Queue(b)
		return nil
	}, nil
}

// planAskPartIndex plans the parts the ask's row holds at `at` - its question, its options and
// the answer it carries - read through r, the Reader of the write that holds the ask's text,
// and returns the plan, which writes nothing (refs.PlanAskParts). It plans against what the
// index holds: as the transaction's own queued writes of the same ask leave it, when there is
// one, else as read in tx; and it records what this one leaves for a later plan of the same ask.
// A version write that moves an open approval request and then answers it plans the one ask's
// parts twice, and the answer takes a revision of its own because it runs against the move's
// rows rather than the stored ones.
func planAskPartIndex(ctx context.Context, tx pgx.Tx, r *refs.Reader, ask model.Ask, at time.Time) (refs.ItemIndexing, error) {
	ledger := queueFor(ctx, tx)
	index, held := refs.ItemIndex{}, false
	if ledger != nil {
		index, held = ledger.askParts[ask.ID]
	}
	if !held {
		var err error
		if index, err = refs.ReadItemIndex(ctx, tx, refs.ItemAsk, ask.ID); err != nil {
			return refs.ItemIndexing{}, fmt.Errorf("index the parts of ask %s: %w", ask.ID, err)
		}
	}
	indexing, err := refs.PlanAskParts(index, r, ask, at)
	if err != nil {
		return refs.ItemIndexing{}, err
	}
	if ledger != nil {
		if ledger.askParts == nil {
			ledger.askParts = make(map[string]refs.ItemIndex)
		}
		noteEntryUndo(ledger, &ledger.askParts, ask.ID)
		ledger.askParts[ask.ID] = indexing.Held()
	}
	return indexing, nil
}

// planAskPartsCounted is the counted index of an ask's parts as this package makes its writes: it
// plans them (planAskPartIndex), resolves the counted targets they move and merges them into
// beside, what the same write moved elsewhere, and returns those with the step that writes the
// parts. The step is queued after the statements that leave the row as the plan reads it, since a
// part row names its ask; the plan itself runs in the read phase, where the counts the event
// carries are worked out. The parts are read through r, the Reader of the write that holds the
// ask's text, so a write that reconciles the ask's question as well passes the one reader to
// both and the database answers it once per kind.
func planAskPartsCounted(
	ctx context.Context,
	tx pgx.Tx,
	r *refs.Reader,
	ask model.Ask,
	at time.Time,
	beside model.ReferenceChanges,
) (model.ReferenceChanges, WriteStep, error) {
	indexing, err := planAskPartIndex(ctx, tx, r, ask, at)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	counted, err := refs.CountedChanges(ctx, tx, indexing.Moved())
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	return refs.MergeChanges(beside, counted), func(b *store.WriteBatch) error {
		indexing.Queue(b)
		return nil
	}, nil
}

// planAskReferences is planReplaceCounted for ask askID, whose references are the ones its
// question cites, read with the whole grammar through r (refs.Reader.Mentions).
func planAskReferences(ctx context.Context, tx pgx.Tx, r *refs.Reader, askID, question string) (model.ReferenceChanges, WriteStep, error) {
	return planReplaceCounted(ctx, tx, "ask", askID, text.Refs(r.Mentions(question)), nil, nil)
}

// planEditedAskReferences is planAskReferences for an edit of the ask's question from previous:
// it also counts every target the question still cites when the edit moves the head of its text,
// which each of the ask's edges shows (refs.EditedTargets).
func planEditedAskReferences(ctx context.Context, tx pgx.Tx, r *refs.Reader, askID, previous, question string) (model.ReferenceChanges, WriteStep, error) {
	found := text.Refs(r.Mentions(question))
	return planReplaceCounted(ctx, tx, "ask", askID, found, nil, refs.EditedTargets(previous, question, found))
}

// settleRoomDocumentsOwed settles the re-index queue row of each document of versions other than
// artifactID (refs.SettleOwedDocuments), queued through write: the documents of the issue room
// whose tree artifact is artifactID, whose text the pass over the room read, while the room's
// references, written under artifactID (replaceVersionRefs), settle the tree's own row. For one
// document's pass versions names artifactID alone, and nothing is settled here.
func settleRoomDocumentsOwed(ctx context.Context, tx pgx.Tx, artifactID string, versions refs.Versions) error {
	documents := make([]string, 0, len(versions))
	for _, document := range slices.Sorted(maps.Keys(versions)) {
		if document != artifactID {
			documents = append(documents, document)
		}
	}
	if len(documents) == 0 {
		return nil
	}
	return write(ctx, tx, func(b *store.WriteBatch) error {
		refs.SettleOwedDocuments(b, documents)
		return nil
	})
}

// ExcerptsOwedChannel is the Postgres notification channel every artifact mention left owed an
// excerpt announces itself on at commit (refs_excerpts_owed_notify, migration 0153): the channel
// whose pglisten.Hub wake the server passes to RunReferenceExcerptReconciliation.
const ExcerptsOwedChannel = "dispatch_excerpts_owed"

// referenceExcerptBatch is how many documents one reconciliation pass reads, and
// referenceExcerptRetryCeiling the longest a document that keeps failing waits between attempts
// (refs.DeferExcerpts).
const (
	referenceExcerptBatch        = 20
	referenceExcerptRetryCeiling = 6 * time.Hour
)

// excerptSchedule is the reconciliation's pace. minGap is the least time from one pass's start to
// the next one's, whatever woke it (duty.WokenLoop.MinGap), and also how long a pass waits when
// another process holds the lease; retryBase is how long a document whose read or record failed
// waits before its first retry, doubling with each failure after (refs.DeferExcerpts), and how
// long a pass whose own read failed waits before the next. A test runs the loop faster.
type excerptSchedule struct {
	minGap    time.Duration
	retryBase time.Duration
}

// referenceExcerptSchedule is the server's pace: two seconds between passes, the step the timer
// this loop replaced took while a batch was full, so under load the loop runs no more often than
// that timer did; one minute before a failed document's first retry.
var referenceExcerptSchedule = excerptSchedule{minGap: 2 * time.Second, retryBase: time.Minute}

// referenceExcerptLease names the lease one reconciliation pass runs under. The excerpts it
// stores are the same whichever process cuts them, and a mention is owed one until some process
// does, so two processes reading the same batch of documents and writing the same rows is work
// done twice for one result - and, because each pass claims nothing, the same documents twice.
const referenceExcerptLease = "reference-excerpts"

// RunReferenceExcerptReconciliation stores the excerpts of every document some of whose mentions
// are owed one (refs.DriftedExcerpts): every mention indexed before migration 0092, each one
// rebuild-refs inserts, and each a version write could not cut. It is a woken loop
// (duty.RunWoken) under the reference-excerpts lease: one pass at start, then a pass when wake is
// signalled - in the server, the pglisten.Hub wake on ExcerptsOwedChannel, which every mention
// left owed announces through a trigger on refs, and which the hub also signals after each
// successful LISTEN, so what was written while the listener had no connection is caught up then -
// and when a pass asks for one: at once while a full batch leaves documents it can still record,
// and when the earliest document it deferred falls due (refs.NextExcerptRetry). Passes start at
// least two seconds apart. With nothing owed and nothing deferred it holds no timer and makes no
// statement. A nil wake leaves the loop with its start and its own due times.
//
// A document the pass cannot read or record is deferred, one minute after its first failure and
// doubling to six hours (refs.DeferExcerpts), so it neither holds the loop at two seconds nor
// fills every batch ahead of the documents that sort after it; a document holding an element
// this build does not know (ErrDocNewerContent), which a newer build wrote while two builds serve
// one database, is deferred the same way, its mentions left owed for a build that reads it. The
// first pass each process runs also marks owed the excerpts a version written by a binary
// predating migration 0092 left behind (refs.MarkStaleExcerpts); every build since records its
// excerpts in each version write, so one marking per process is enough. A mention owed an excerpt
// is answered with the document's name until this stores one.
func (s *Service) RunReferenceExcerptReconciliation(ctx context.Context, leases duty.Leases, wake <-chan struct{}) {
	s.runReferenceExcerptReconciliation(ctx, leases, wake, referenceExcerptSchedule)
}

func (s *Service) runReferenceExcerptReconciliation(ctx context.Context, leases duty.Leases, wake <-chan struct{}, schedule excerptSchedule) {
	ctx = store.WithTransactionTracking(ctx)
	// marked is whether a pass of this process has marked the stale excerpts; a marking that
	// failed is tried again by the next pass that runs.
	marked := false
	duty.RunWoken(ctx, duty.WokenLoop{
		Lease:  referenceExcerptLease,
		Leases: leases,
		Wake:   wake,
		MinGap: schedule.minGap,
		Pass: func(ctx context.Context) time.Time {
			if !marked {
				marked = s.markStaleExcerpts(ctx)
			}
			return s.reconcileExcerptPass(ctx, schedule)
		},
	})
}

// markStaleExcerpts marks owed the excerpts older than their document's latest version
// (refs.MarkStaleExcerpts) and reports whether it did so without error.
func (s *Service) markStaleExcerpts(ctx context.Context) bool {
	marked, err := refs.MarkStaleExcerpts(ctx, s.store.Pool)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch: mark stale reference excerpts", "error", err)
		}
		return false
	}
	if marked > 0 {
		slog.Info("dispatch: marked stale reference excerpts owed", "mentions", marked)
	}
	return true
}

// reconcileExcerptPass is one pass of the reconciliation's batch, and returns when the loop next
// needs to run with no wake: now while a full batch leaves documents it can still record, else the
// earliest due time of a document it deferred, else never. A pass whose own read failed asks again
// after schedule.retryBase.
func (s *Service) reconcileExcerptPass(ctx context.Context, schedule excerptSchedule) time.Time {
	documents, full, err := s.reconcileReferenceExcerpts(ctx, schedule.retryBase)
	if documents > 0 {
		slog.Info("dispatch: reconciled reference excerpts", "documents", documents)
	}
	switch {
	case ctx.Err() != nil:
		return time.Time{}
	case err != nil:
		slog.Error("dispatch: reconcile reference excerpts", "error", err)
		return time.Now().Add(schedule.retryBase)
	case full:
		return time.Now()
	}
	next, err := refs.NextExcerptRetry(ctx, s.store.Pool)
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}
		}
		slog.Error("dispatch: reconcile reference excerpts", "error", err)
		return time.Now().Add(schedule.retryBase)
	}
	return next
}

// ReconcileReferenceExcerpts runs one batch of the reconciliation at the server's pace
// (reconcileReferenceExcerpts).
func (s *Service) ReconcileReferenceExcerpts(ctx context.Context) (documents int, full bool, err error) {
	return s.reconcileReferenceExcerpts(ctx, referenceExcerptSchedule.retryBase)
}

// reconcileReferenceExcerpts reads each document of one batch of drift as the caller sees it
// (TextWithBlocks), with no transaction or connection held, and stores its mentions' excerpts at
// the version the drift named. A document whose stored history does not load or whose tree is
// outside the schema has no block to show, so its mentions are stored with none and keep the
// document's name until its next version. A document holding an element this build does not know
// (ErrDocNewerContent) is one a newer build wrote while two builds serve one database: its
// mentions stay owed, for a build that reads it, and it is deferred so the batches after it are
// read. A document it cannot read or record for any other reason is logged and deferred too
// (refs.DeferExcerpts, retryBase after its first failure), its mentions left owed, so one document
// never holds back the rest of its batch nor the batches after it; a recorded document's backoff
// row is deleted. documents counts the ones recorded, and full reports a full batch at least one
// of whose documents left the drift a batch reads - recorded or deferred - so the next batch reads
// documents this one did not.
func (s *Service) reconcileReferenceExcerpts(ctx context.Context, retryBase time.Duration) (documents int, full bool, err error) {
	drifted, err := refs.DriftedExcerpts(ctx, s.store.Pool, referenceExcerptBatch)
	if err != nil {
		return 0, false, err
	}
	moved := 0
	deferDocument := func(document refs.ExcerptDrift) {
		if err := refs.DeferExcerpts(ctx, s.store.Pool, document.FromID, document.Version, retryBase, referenceExcerptRetryCeiling); err != nil {
			if ctx.Err() == nil {
				slog.Error("dispatch: reference excerpt reconciliation could not defer a document; the next batch reads it again",
					"artifact", document.FromID, "error", err)
			}
			return
		}
		moved++
	}
	for _, document := range drifted {
		var excerpts map[refs.Target]model.GraphExcerpt
		markdown, blocks, err := s.TextWithBlocks(ctx, document.FromID)
		switch {
		case ctx.Err() != nil:
			return documents, false, ctx.Err()
		case errors.Is(err, ErrDocNewerContent):
			slog.Warn("dispatch: reference excerpt reconciliation deferred a document holding an element this build cannot read; its mentions stay owed for a build that reads it",
				"artifact", document.FromID, "version", document.Version, "error", err)
			deferDocument(document)
			continue
		case errors.Is(err, ErrDocumentUnloadable), errors.Is(err, ErrDocOutsideSchema):
			slog.Warn("dispatch: reference excerpt reconciliation cannot read the document; its mentions keep the document's name",
				"artifact", document.FromID, "error", err)
		case err != nil:
			slog.Error("dispatch: reference excerpt reconciliation deferred a document it could not read",
				"artifact", document.FromID, "error", err)
			deferDocument(document)
			continue
		default:
			// The mentions are read as the version's own write read them, their short ids resolved
			// for its writer (refs.NewReader), so each excerpt is the one of the edge that write
			// stored.
			record, err := refs.ReadIndexedVersion(ctx, s.store.Pool, document.FromID, document.FromID, document.Version)
			if err != nil {
				if ctx.Err() != nil {
					return documents, false, ctx.Err()
				}
				slog.Error("dispatch: reference excerpt reconciliation deferred a document whose version's writer it could not read",
					"artifact", document.FromID, "version", document.Version, "error", err)
				deferDocument(document)
				continue
			}
			reader := refs.NewReader(ctx, s.store.Pool, record.Writer, "artifact", document.FromID, s.serverURL)
			excerpts = refs.MentionExcerpts(markdown, blocks, reader.Mentions(markdown))
		}
		// The reconciliation appends no event, so what it moved is named nowhere: until it stores
		// an excerpt, the route answers a mention owed one with the document's name.
		if _, err := refs.RecordExcerpts(ctx, s.store.Pool, document.FromID, document.Version, excerpts); err != nil {
			if ctx.Err() != nil {
				return documents, false, ctx.Err()
			}
			slog.Error("dispatch: reference excerpt reconciliation deferred a document it could not record",
				"artifact", document.FromID, "error", err)
			deferDocument(document)
			continue
		}
		documents++
		moved++
		if document.BackedOff {
			// A row left behind only costs the next batch its exclusion check: the version
			// recorded here owes nothing, and NextExcerptRetry drops rows of documents that owe
			// nothing.
			if err := refs.ClearExcerptBackoff(ctx, s.store.Pool, document.FromID); err != nil && ctx.Err() == nil {
				slog.Error("dispatch: reference excerpt reconciliation could not clear a document's backoff",
					"artifact", document.FromID, "error", err)
			}
		}
	}
	return documents, len(drifted) == referenceExcerptBatch && moved > 0, nil
}
