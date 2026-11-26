package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
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
// Its second result is what the block index states in the node and tuple store, which the
// caller appends under the event it goes on to queue for this version, right after that event
// (graphstore.QueueCaused), in this same write phase.
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
	// Above deferBlockIndexBlocks the write indexes this version's references alone: migration
	// 0122's trigger marked the document owed a block index (document_block_versions.version
	// null) when the version's row was inserted, indexDocument adds this build's own mark
	// (oweDocumentIndex), and nothing here settles either, so the block-index reconciliation
	// (RunBlockIndexReconciliation) indexes the blocks and the mentions they hold out of the
	// request.
	if blocks := len(rendered.offsets); rendered.tree != nil && blocks > deferBlockIndexBlocks {
		slog.Info("dispatch: a document version left its blocks to the block-index reconciliation: it holds more blocks than a write indexes in its own request",
			"artifact", artifactID, "version", version, "blocks", blocks, "bound", deferBlockIndexBlocks)
		rendered = renderedTree{}
	}
	return s.indexDocument(ctx, tx, artifactID, version, versionAt, markdown, rendered)
}

// indexDocument indexes one document version, written at versionAt, in its writer's transaction,
// which holds the document owner's lock, from the render the version's markdown came from:
// indexRenderedVersion with rendered's blocks. The blocks are rendered.offsets; GET …/blocks lists
// one block per offset of the same renderer (renderedBlocks), so both list one set of blocks. A
// version with no rendering - an upload whose replacement changed nothing, which writes no tree,
// or one whose blocks the write deferred (deferBlockIndexBlocks) - is indexUnrenderedVersion,
// which indexes no blocks: migration 0122's trigger left the document owed when the version's row
// was inserted, for the block-index reconciliation, which reads the document itself.
//
// That trigger's mark is the null version, which the release before this one reads and drains
// too, so during a roll a pod of that release can index this version first and write its rows
// naming no node. Such a version therefore takes this build's own mark as well, in this same
// transaction (oweDocumentIndex), which that release neither reads nor clears: this build's
// reconciliation then indexes the version after it, names every row's node and states what each
// projects (refs.PlanBlocks).
func (s *Service) indexDocument(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, markdown string, rendered renderedTree) (model.ReferenceChanges, graphstore.Uncaused, error) {
	if rendered.tree == nil {
		if err := queueDocumentIndexOwed(ctx, tx, artifactID, s.markingRule); err != nil {
			return model.ReferenceChanges{}, graphstore.Uncaused{}, err
		}
		changes, err := s.indexUnrenderedVersion(ctx, tx, artifactID, version, versionAt, markdown)
		return changes, graphstore.Uncaused{}, err
	}
	return s.indexRenderedVersion(ctx, tx, artifactID, version, versionAt, markdown, offsetBlocks(rendered.offsets), rendered.tree)
}

// offsetBlocks is the blocks a render's offsets name, as the block index takes them, each with the
// attributes its render found: a typed block that reached the index with none would read as one
// that declares none, where GET …/blocks (renderedBlocks) answers the same block with them.
func offsetBlocks(offsets []pmdoc.BlockOffset) []model.ArtifactBlock {
	blocks := make([]model.ArtifactBlock, len(offsets))
	for index, offset := range offsets {
		blocks[index] = model.ArtifactBlock{
			ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To,
			Attributes: offset.Attributes,
		}
	}
	return blocks
}

// indexRenderedVersion indexes one document version, written at versionAt, in a transaction
// holding the document owner's lock: its references, the excerpt of each mention, and its blocks
// with the links each holds (refs.PlanBlocks), all from one reading of markdown over that
// transaction (refs.Reader.Mentions), so the document's own edges, its blocks' and their
// excerpts read the bare keys, `#id`s and short ids it writes alike. The reading resolves the
// short ids for the version's writer, the one its block index states every node and tuple as
// written by (indexedVersionOf). blocks index into markdown, the render they came from; tree,
// when the caller holds it, is the tree markdown was rendered from, and nil otherwise. The event
// the caller appends names the counted targets the document's references and their excerpts, its
// blocks and its blocks' links moved (replaceVersionRefs), since a node is counted once for each
// block citing it. The reading, the index's plans and the counted targets are worked out where it
// is called; its writes go through write, in the order the index makes them.
//
// Its second result is what the pass states in the node and tuple store, which the caller
// appends under that same event (graphstore.QueueCaused): the pass writes the block nodes
// itself, because the foreign key on the projection rows needs them, and holds its tuples back
// because on the request path the event does not exist yet.
//
// Whether the block pass won the revision fence (refs.BlockIndexing.Won) decides what its
// reconciliation settles: only a pass that won writes the version's blocks, so only its
// reconciliation settles the document's row of the forms queue (replaceVersionRefs'
// blocksIndexed). A pass that lost - a newer version's
// blocks already stand - writes no block, and it leaves that row owed for the next pass, as it
// leaves the block index's own mark (refs.BlockIndexing.Queue writes nothing for it).
func (s *Service) indexRenderedVersion(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, markdown string, blocks []model.ArtifactBlock, tree *pmdoc.Node) (model.ReferenceChanges, graphstore.Uncaused, error) {
	record, err := indexedVersionOf(ctx, tx, artifactID, version)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	located := refs.NewReader(ctx, tx, record.Writer, "artifact", artifactID, s.origins).Mentions(markdown)
	// The blocks are indexed before the document's own references are reconciled: a first
	// indexing dates a block's mention from the document's refs row for its target, which a
	// surviving target already holds and a new one gets with this same time.
	moved, indexBlocks, stated, won, err := planBlockIndex(ctx, tx, s.markingRule, refs.VersionBlocks{
		ArtifactID: artifactID, Record: record, Number: version, At: versionAt,
		Markdown: markdown, Blocks: blocks, Located: located, Pointers: blockPointers(artifactID, markdown, blocks, tree),
		ServerURL: s.serverURL,
	})
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	if err := write(ctx, tx, indexBlocks); err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	changes, err := replaceVersionRefs(ctx, tx, artifactID, version, versionAt, located, moved, refs.MentionExcerpts(markdown, blocks, located, s.serverURL), won)
	return changes, stated, err
}

// blockPointers is each `block[]` value of the version (pmdoc.BlockPointers) as the block index
// writes it: none when the schema declares no `block[]` attribute or the version has no blocks;
// otherwise those of tree, or, with no tree, of markdown read back. A version whose markdown does
// not read back is indexed with no pointers, as one the server cannot read is with no blocks.
func blockPointers(artifactID, markdown string, blocks []model.ArtifactBlock, tree *pmdoc.Node) []refs.Pointer {
	if len(blocks) == 0 || !pmdoc.HasLinkKinds() {
		return nil
	}
	if tree == nil {
		read, err := pmdoc.ParseRendering(markdown)
		if err != nil {
			slog.Warn("dispatch: a document version's block pointers are not indexed: its markdown does not read back",
				"artifact", artifactID, "error", err)
			return nil
		}
		tree = read
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
// for the version's writer (versionWriterOf). It indexes no blocks, so it leaves the document owed
// to the forms queue (blocksIndexed false), and marks the document's mentions owed an excerpt
// (refs.ClearExcerpts) for the excerpt reconciliation, which reads the document itself.
func (s *Service) indexUnrenderedVersion(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, markdown string) (model.ReferenceChanges, error) {
	writer, err := versionWriterOf(ctx, tx, artifactID, version)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	reader := refs.NewReader(ctx, tx, writer, "artifact", artifactID, s.origins)
	return replaceVersionRefs(ctx, tx, artifactID, version, versionAt, reader.Mentions(markdown), nil, nil, false)
}

// replaceVersionRefs reconciles the document's references, dated versionAt, to the mentions
// located in its version, stores excerpts on them (nil: marks them owed, refs.ClearExcerpts), and
// counts the targets moved, which its blocks and their mentions moved, as well. A document mention
// shows the block holding it as its excerpt, so a target whose kept edge now stores another block
// is counted beside the ones whose edges the version inserted or deleted (refs.ExcerptsMoved,
// worked out here, before the reconciliation and the excerpts are written, in that order, by one
// write). blocksIndexed says the caller queued the version's block index in tx first and that
// index won the revision fence, which is what lets the reconciliation, queued after it, settle
// the document's row of the forms queue in the statement that settles the whole grammar's
// (refs.Replacement.SettlingForms); whether excerpts is nil decides only whether the excerpts are
// stored or marked owed.
func replaceVersionRefs(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, located []text.Located, moved []refs.Target, excerpts map[refs.Target]model.GraphExcerpt, blocksIndexed bool) (model.ReferenceChanges, error) {
	replacement, err := planReplacement(ctx, tx, "artifact", artifactID, text.Refs(located), &versionAt)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	if blocksIndexed {
		replacement = replacement.SettlingForms()
	}
	counted := slices.Concat(replacement.Changed(), moved)
	writeExcerpts := func(b *store.WriteBatch) { refs.ClearExcerpts(b, artifactID) }
	if excerpts != nil {
		recorded, err := refs.ExcerptsMoved(ctx, tx, artifactID, version, replacement.Held(), excerpts)
		if err != nil {
			return model.ReferenceChanges{}, err
		}
		counted = append(counted, recorded...)
		writeExcerpts = func(b *store.WriteBatch) { refs.QueueRecordExcerpts(b, artifactID, version, excerpts) }
	}
	changes, err := refs.CountedChanges(ctx, tx, counted)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return changes, write(ctx, tx, func(b *store.WriteBatch) error {
		replacement.Queue(b)
		writeExcerpts(b)
		return nil
	})
}

// planBlockIndex plans the block index of version, one version of a document with its block
// pointers (refs.PlanBlocks), at the marking rule rule of the build planning it, and returns the
// targets it moves, the cited blocks it changes included (refs.BlockIndexing.Cited), the step
// that writes it, what it states in the node and tuple store, held for the event the caller
// appends for the version (refs.BlockIndexing.Stated), and whether it won the revision fence
// (refs.BlockIndexing.Won): the step of a pass that lost writes nothing. version.Record is what
// the version's own row says of it (indexedVersionOf). It plans against what the index holds: as
// the transaction's own queued indexing of the document leaves it, when there is one, else as
// read in tx; and it records what this one leaves for a later indexing of the same document. Each
// block's excerpt is cut in the plan, around the dashboard URLs of the deployment at
// version.ServerURL.
func planBlockIndex(ctx context.Context, tx pgx.Tx, rule int, version refs.VersionBlocks) ([]refs.Target, WriteStep, graphstore.Uncaused, bool, error) {
	ledger := queueFor(ctx, tx)
	index, held := refs.BlockIndex{}, false
	if ledger != nil {
		index, held = ledger.blockIndexes[version.ArtifactID]
	}
	if !held {
		var err error
		if index, err = refs.ReadBlockIndex(ctx, tx, version.ArtifactID); err != nil {
			return nil, nil, graphstore.Uncaused{}, false, err
		}
	}
	now, err := transactionNow(ctx, tx)
	if err != nil {
		return nil, nil, graphstore.Uncaused{}, false, err
	}
	indexing, err := refs.PlanBlocks(ctx, tx, index, rule, now, version)
	if err != nil {
		return nil, nil, graphstore.Uncaused{}, false, err
	}
	cited, err := indexing.Cited(ctx, tx)
	if err != nil {
		return nil, nil, graphstore.Uncaused{}, false, err
	}
	if ledger != nil {
		if ledger.blockIndexes == nil {
			ledger.blockIndexes = make(map[string]refs.BlockIndex)
		}
		ledger.blockIndexes[version.ArtifactID] = indexing.Held()
	}
	indexBlocks := func(b *store.WriteBatch) error {
		indexing.Queue(b)
		return nil
	}
	return slices.Concat(indexing.Moved(), cited), indexBlocks, indexing.Stated(), indexing.Won(), nil
}

// indexedVersionOf is what version's own row says of its block index (refs.IndexedVersion): the
// document's ref_key and scope, from its address as this transaction leaves it
// (addressOfDocument), and the version's writer (versionWriterOf).
func indexedVersionOf(ctx context.Context, tx pgx.Tx, artifactID string, version int) (refs.IndexedVersion, error) {
	address, err := addressOfDocument(ctx, tx, artifactID)
	if err != nil {
		return refs.IndexedVersion{}, err
	}
	if ledger := queueFor(ctx, tx); ledger != nil {
		if pending, queued := ledger.latest[artifactID]; queued && pending.version.Number == version {
			record := refs.IndexedVersion{RefKey: address.RefKey}
			if len(pending.version.Authors) > 0 {
				record.Writer = pending.version.Authors[0]
			}
			if _, created := pendingArtifact(ctx, artifactID); created {
				record.Project, record.Parties = graphstore.SessionScope(address.Project, address.Session)
				return record, nil
			}
			stored, err := refs.ReadIndexedVersion(ctx, tx, artifactID, version)
			if err != nil {
				return refs.IndexedVersion{}, err
			}
			record.Project, record.Parties = stored.Project, stored.Parties
			return record, nil
		}
	}
	record, err := refs.ReadIndexedVersion(ctx, tx, artifactID, version)
	if err != nil {
		return refs.IndexedVersion{}, err
	}
	record := refs.IndexedVersion{RefKey: address.RefKey, Writer: writer}
	record.Project, record.Parties = graphstore.SessionScope(address.Project, address.Session)
	return record, nil
}

// versionWriterOf is the writer version's own row records, its first author: from the version
// this transaction queued and has not written where that is the version, which no read of the
// table answers yet, and else from the stored row (refs.ReadIndexedVersion). A version that
// records no author has the zero actor, nobody. It is the part of indexedVersionOf a version
// with no block index reads (indexUnrenderedVersion).
func versionWriterOf(ctx context.Context, tx pgx.Tx, artifactID string, version int) (model.Actor, error) {
	if ledger := queueFor(ctx, tx); ledger != nil {
		if pending, queued := ledger.latest[artifactID]; queued && pending.version.Number == version {
			if len(pending.version.Authors) > 0 {
				return pending.version.Authors[0], nil
			}
			return model.Actor{}, nil
		}
	}
	record, err := refs.ReadIndexedVersion(ctx, tx, artifactID, version)
	if err != nil {
		return model.Actor{}, err
	}
	return record.Writer, nil
}

// planReplacement plans the reconciliation of fromKind/fromID's references with found, a new edge
// dated insertedAt (nil: the transaction's time), against the source's edges as the transaction's
// own queued reconciliation of it leaves them, when there is one, else as read in tx, and records
// what this one leaves.
func planReplacement(ctx context.Context, tx pgx.Tx, fromKind, fromID string, found []text.Ref, insertedAt *time.Time) (refs.Replacement, error) {
	return planReplacementOver(ctx, tx, fromKind, fromID, found, insertedAt, nil, false)
}

// planReplacementOver is planReplacement given what an earlier read of this transaction found
// the source's edges to be (stored, with read true): the transaction's own queued reconciliation
// of the source still comes first, then that read, and only with neither a read of its own in tx.
func planReplacementOver(ctx context.Context, tx pgx.Tx, fromKind, fromID string, found []text.Ref, insertedAt *time.Time, stored []refs.Target, read bool) (refs.Replacement, error) {
	ledger := queueFor(ctx, tx)
	source := refs.Target{Kind: fromKind, ID: fromID}
	var replacement refs.Replacement
	held, known := stored, read
	if ledger != nil {
		if queued, ok := ledger.referenced[source]; ok {
			held, known = queued, true
		}
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
// and returns the plan, which writes nothing (refs.PlanAskParts), with the index it planned
// against. It plans against what the index holds: as the transaction's own queued writes of the
// same ask leave it, when there is one, else as read in tx; and it records what this one leaves
// for a later plan of the same ask. A version write that moves an open approval request and then
// answers it plans the one ask's parts twice, and the answer takes a revision of its own because
// it runs against the move's rows rather than the stored ones.
func planAskPartIndex(ctx context.Context, tx pgx.Tx, r *refs.Reader, ask model.Ask, at time.Time) (refs.ItemIndexing, refs.ItemIndex, error) {
	ledger := queueFor(ctx, tx)
	index, held := refs.ItemIndex{}, false
	if ledger != nil {
		index, held = ledger.askParts[ask.ID]
	}
	if !held {
		var err error
		if index, err = refs.ReadItemIndex(ctx, tx, refs.ItemAsk, ask.ID); err != nil {
			return refs.ItemIndexing{}, refs.ItemIndex{}, fmt.Errorf("index the parts of ask %s: %w", ask.ID, err)
		}
	}
	indexing, err := refs.PlanAskParts(index, r, ask, at)
	if err != nil {
		return refs.ItemIndexing{}, refs.ItemIndex{}, err
	}
	if ledger != nil {
		if ledger.askParts == nil {
			ledger.askParts = make(map[string]refs.ItemIndex)
		}
		ledger.askParts[ask.ID] = indexing.Held()
	}
	return indexing, index, nil
}

// planAskQuestionAfterParts plans the reconciliation of the ask's question's references that a
// write of its parts alone queues after them, a new edge dated at, settling the forms queue in
// the statement that settles the whole grammar's (refs.Replacement.SettlingForms): the question
// is the rest of the ask's text, so with it the write has indexed the whole of it, after every
// delete that index made. It plans against the question's edges as the transaction's own queued
// reconciliation of them leaves them, when there is one, else as the read of the ask's part index
// found them (refs.ItemIndex.HeldRefs), which costs the write no read of its own, else as read in
// tx (planReplacementOver).
func planAskQuestionAfterParts(ctx context.Context, tx pgx.Tx, r *refs.Reader, ask model.Ask, at time.Time, index refs.ItemIndex) (refs.Replacement, error) {
	held, read := index.HeldRefs()
	replacement, err := planReplacementOver(ctx, tx, "ask", ask.ID, text.Refs(r.Mentions(ask.Question)), &at, held, read)
	if err != nil {
		return refs.Replacement{}, fmt.Errorf("index ask %s: %w", ask.ID, err)
	}
	return replacement.SettlingForms(), nil
}

// planAskPartsCounted is the counted index of an ask's parts as this package makes its writes: it
// plans them (planAskPartIndex), resolves the counted targets they move and merges them into
// beside, what the same write moved elsewhere, and returns those with the step that writes the
// parts. The step is queued after the statements that leave the row as the plan reads it, since a
// part row names its ask; the plan itself runs in the read phase, where the counts the event
// carries are worked out. The parts are read through r, the Reader of the write that holds the
// ask's text, so a write that reconciles the ask's question as well passes the one reader to
// both and the database answers it once per kind.
//
// The parts are the rest of the ask's text beside its question, so the step is where the write
// settles what the forms queue holds for the ask (refs.RefsFormsOwed). questionReconciled says
// the write queued the question's own reconciliation before this step (planAskReferences,
// planEditedAskReferences), counted in beside: the step then settles that queue in the
// statement that ends the parts' index (refs.ItemIndexing.SettlingForms). A write that holds the
// parts alone - an answer, a restoration - passes false, and the step reconciles the question's
// references after the parts as well, counted with them (planAskQuestionAfterParts), which
// settles both queues after both deletes.
func planAskPartsCounted(
	ctx context.Context,
	tx pgx.Tx,
	r *refs.Reader,
	ask model.Ask,
	at time.Time,
	questionReconciled bool,
	beside model.ReferenceChanges,
) (model.ReferenceChanges, WriteStep, error) {
	indexing, index, err := planAskPartIndex(ctx, tx, r, ask, at)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	if questionReconciled {
		indexing = indexing.SettlingForms()
		counted, err := refs.CountedChanges(ctx, tx, indexing.Moved())
		if err != nil {
			return model.ReferenceChanges{}, nil, err
		}
		return refs.MergeChanges(beside, counted), func(b *store.WriteBatch) error {
			indexing.Queue(b)
			return nil
		}, nil
	}
	question, err := planAskQuestionAfterParts(ctx, tx, r, ask, at, index)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	counted, err := refs.CountedChanges(ctx, tx, slices.Concat(indexing.Moved(), question.Changed()))
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	return refs.MergeChanges(beside, counted), func(b *store.WriteBatch) error {
		indexing.Queue(b)
		question.Queue(b)
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
// which each of the ask's edges shows, cut against r's deployment (refs.EditedTargets).
func planEditedAskReferences(ctx context.Context, tx pgx.Tx, r *refs.Reader, askID, previous, question string) (model.ReferenceChanges, WriteStep, error) {
	found := text.Refs(r.Mentions(question))
	return planReplaceCounted(ctx, tx, "ask", askID, found, nil, refs.EditedTargets(previous, question, r.ServerURL(), found))
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
		markdown, blocks, err := s.TextWithBlocks(ctx, model.ServerReader, document.FromID)
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
			record, err := refs.ReadIndexedVersion(ctx, s.store.Pool, document.FromID, document.Version)
			if err != nil {
				if ctx.Err() != nil {
					return documents, false, ctx.Err()
				}
				slog.Error("dispatch: reference excerpt reconciliation deferred a document whose version's writer it could not read",
					"artifact", document.FromID, "version", document.Version, "error", err)
				deferDocument(document)
				continue
			}
			reader := refs.NewReader(ctx, s.store.Pool, record.Writer, "artifact", document.FromID, s.origins)
			excerpts = refs.MentionExcerpts(markdown, blocks, reader.Mentions(markdown), s.origins)
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
