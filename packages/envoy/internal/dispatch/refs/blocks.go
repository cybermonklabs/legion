package refs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/sjawhar/envoy/internal/dispatch/store"
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

// BlockIndex is what a document's block index holds: each block's row - the sha256 of its text,
// when the row was written and whether it names the block's node - by block id; the links its
// blocks hold, each with the block types it points at, when its row was written, the source
// event it stands at and whether it names its block's node; and the version the document's
// blocks stand at (blockStand), which the revision fence is decided on. placed is, for the Held
// of an indexing the transaction has queued and not written, the far end that indexing gave each
// block's part_of, by block id, which the store holds only once the write phase has run; it is
// nil for an index read from the tables, whose placements the plan reads from the store.
type BlockIndex struct {
	blocks map[string]storedBlock
	links  []indexedLink
	stand  blockStand
	placed map[string]blockFarEnd
}

// indexedLink is one block_refs row the index holds: its pair, when the row was written, the
// source event it stands at, and whether it names its block's node.
type indexedLink struct {
	blockPair
	createdAt time.Time
	sourceSeq *int64
	named     bool
}

// ReadBlockIndex reads what q's block index holds for artifactID: its blocks, its links read with
// the predicate the reconciling delete visits them by, each with the block types it points at, so
// a plan can tell a link whose attribute now names other types, and the version its blocks stand
// at, all in the read phase of the write that plans the indexing (PlanBlocks).
func ReadBlockIndex(ctx context.Context, q Queryer, artifactID string) (BlockIndex, error) {
	blocks, err := storedBlocks(ctx, q, artifactID)
	if err != nil {
		return BlockIndex{}, err
	}
	rows, err := q.Query(ctx, `
		select block_id, kind, to_kind, to_id, to_types, created_at, source_seq,
		       node_kind is not null and node_address is not null
		from block_refs where artifact_id = $1
	`, artifactID)
	if err != nil {
		return BlockIndex{}, fmt.Errorf("read the block mentions of %s: %w", artifactID, err)
	}
	defer rows.Close()
	var links []indexedLink
	for rows.Next() {
		var link indexedLink
		if err := rows.Scan(&link.blockID, &link.kind, &link.target.Kind, &link.target.ID, &link.toTypes,
			&link.createdAt, &link.sourceSeq, &link.named); err != nil {
			return BlockIndex{}, fmt.Errorf("scan a block mention of %s: %w", artifactID, err)
		}
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return BlockIndex{}, fmt.Errorf("read the block mentions of %s: %w", artifactID, err)
	}
	stand, err := readBlockStand(ctx, q, artifactID)
	if err != nil {
		return BlockIndex{}, err
	}
	return BlockIndex{blocks: blocks, links: links, stand: stand}, nil
}

// IndexedVersion is what a version's own row and its document's row say of its block index: the
// document's ref_key, which every block's address starts with; the actor the row records as the
// version's writer, which every node and tuple the index writes carries; and the project the
// document belongs to, or for an agent conversation's file the session it is between, which every
// block node the index writes carries (graphstore.Node's Project and Parties). A conversation
// holds no markdown document (api's agentOwnsFilesOnly), so no block has parties today; the record
// keeps the one rule every document's node follows. A version no row records - a caller indexing
// blocks of its own, as the seed does - leaves the writer nobody, the zero actor, exactly as a
// node of the store written on a credential that names nobody carries. A writer whose version row
// is only queued hands it over from what that row will hold (docs' planBlockIndex), since no read
// of the row answers yet; one whose row is stored reads it (ReadIndexedVersion).
type IndexedVersion struct {
	RefKey  string
	Writer  model.Actor
	Project string
	Parties []string
}

// VersionBlocks is the version a block-index pass indexes (PlanBlocks), the inputs that travel
// together from the writer that wrote or read the version into the plan: the document
// (ArtifactID), what the version's own row says of its index (Record), the version's number and
// its time, its markdown, its rendered blocks (Blocks), the Mentions of its text (Located) and its
// `block[]` values (Pointers).
type VersionBlocks struct {
	ArtifactID string
	Record     IndexedVersion
	Number     int
	At         time.Time
	Markdown   string
	Blocks     []model.ArtifactBlock
	Located    []text.Located
	Pointers   []Pointer
}

// BlockIndexing is one version's block index, planned against what the index holds
// (PlanBlocks), so the targets it moves and the facts it states in the node and tuple store are
// known before anything is written, and written by Queue, which puts its statements on a write
// phase's batch and does nothing else. An indexing that lost the revision fence (Won) writes
// nothing at all - no projection row and no tuple - since what it would state is what the winning
// version already stated.
type BlockIndexing struct {
	artifactID    string
	refKey        string
	version       int
	versionAt     time.Time
	firstIndexing bool
	won           bool
	// rule is the marking rule of the build planning the pass (BlockIndexMarkingRule), which the
	// fence decides the pass's win at the version the row stands at on, and whether its stand
	// clears the document's mark (queueStand).
	rule int
	// namesSurvivingRows says the pass names the node of every surviving block_refs row of the
	// document that names none, stating the link that row projects (queueBlockRefs), because
	// such a row may be another release's: on the drain of this build's mark (blockStand.owed)
	// and where the pass reads the store's statement of the document as stale (blockStand.stale).
	namesSurvivingRows bool
	// deleted are the blocks the version no longer holds; ids, types, sums and excerpts the
	// blocks whose text it changed (new, changed or restored); textIDs and texts the block texts
	// it records, nil for a deleted block.
	deleted, ids, types, sums, excerpts, textIDs []string
	texts                                        []*string
	pairs                                        []blockPair
	// placedIDs and placedParents are the blocks the pass places, in document order, and the
	// block each sits in, nil for one the document holds: what the parents' projection writes.
	placedIDs     []string
	placedParents []*string
	moved         []Target
	held          BlockIndex
	// stated is what the pass states in the node and tuple store, held for the event that causes
	// the write (graphstore.Uncaused).
	stated graphstore.Uncaused
}

// PlanBlocks plans the block index of version (VersionBlocks) against index, what the document's
// block index holds: one ReadBlockIndex read, or the Held of the caller's own earlier indexing of
// the document that is not written yet. rule is the marking rule of the build planning it
// (BlockIndexMarkingRule, or the rule a docs.Service carries), which the fence reads (queueStand).
// It does the indexing's whole work - each block's text sum, its excerpt, the links its mentions
// and its pointers write, and what the node and tuple store is told of them - and reads, through
// q, only what the store itself holds of the document where the plan compares the render against
// it: each block's parent, the document's own references on its first indexing, and the store's
// statement of the document where that is stale. A version writer plans the indexing in its read
// phase and queues it in its write phase (docs.indexDocument). now is the transaction's now(),
// which a row the indexing inserts takes from its column's default.
//
// The plan makes the version's blocks graph nodes, and each mention written inside a block an edge
// out of the innermost block holding its start. version.Blocks are its rendered blocks in
// document order, each parent before its children, and the first holder of a repeated id is the
// node; version.Located must be the Mentions of the document's text (NewReader), the one reading
// of it a version's index takes, so a mention's offset places it in the block holding it however
// the text wrote the reference. A block whose id is longer than text.MaxBlockIDBytes, which no
// key of the index can hold, is no node: a mention inside it starts at the innermost node holding
// it, if any, and the plan logs a warning naming the document, the longest such id's length and
// the bound.
//
// A block whose text (markdown[From:To]) is unchanged keeps the version its text last changed in;
// a new, changed or restored block takes this version, dated version.At, and its excerpt is its
// text cut to BlockExcerptRunes, the excerpt a document's mention has shown since migration 0092.
// A block the version no longer holds is deleted, its node's state stated gone, and the edges at
// it stop resolving until a version holds its id again. A block the version holds keeps the state
// and the visibility its node has, unless it was gone, when it comes back live (blockNode
// restates every node it writes). The windows the version's blocks are embedded in are not
// planned here and not written by this index: the row it records marks the document owed them,
// and the block-index tick's window arm writes them (Queue, DocumentsOwedWindows).
//
// version.Pointers are its `block[]` values (pmdoc.BlockPointers, which lists none of a typed
// block repeating the id of a typed block before it): each whose block is a node is an edge of its
// attribute's kind from that block, carrying the attribute's targets as to_types, which the
// readers list only while its target has an allowed type (block_ref_listed). A value is written in
// its typed block's opening line, so the extraction also finds it as a mention held by that block:
// the block's mention of a target is kept only while the block itself holds more references to it
// than its pointers do (a string attribute citing it too, say).
//
// What the pass states in the node and tuple store (Stated) is held for the event that caused the
// write (graphstore.Uncaused): the block nodes it writes, and the tuples the projection rows
// beside them are the projection of. The nodes are written by the pass itself, first among its
// statements after the fence, because the foreign key on (node_kind, node_address) is checked
// inside each statement that writes a projection row and there is no node to name otherwise; the
// tuples are not, because on the request path no event exists while the pass runs. The caller
// appends them once it has queued that event, in the same transaction, right after it, where
// refs.QueueStamp runs. The base attributes of the nodes the pass wrote ride with them, so a block
// stated gone by a version carries the event that deleted it, and a block_refs row the pass
// inserts outside a first indexing takes that event as its tuple's legacy_source_seq
// (graphstore.UncausedTuple). A pass that lost the revision fence states nothing and answers the
// zero write, for which graphstore.QueueCaused writes nothing.
//
// A block of the document whose row names no node - a row a build before this release wrote,
// which the backfill has not reached - takes its node and its membership of the document here,
// whether this version touched its text or not, because the row learns the address of that node
// in this same pass (the parents' projection) and the foreign key on it is checked inside the
// statement that writes it.
//
// A pass over a document that holds this build's mark (store_owed_at, blockStand.owed) is the
// drain of that mark, and it names the node of every block_refs row of the document that names
// none as well, stating the link that row projects (queueBlockRefs). The mark is set where this
// build leaves the document's blocks to a later pass - the backfill, a deferred version, a
// re-index owed - and the release before this one can index the document before that pass runs,
// which writes its rows with no node. A row of a pair that survives is what that release leaves,
// and a pass that named a row's node only when it inserted the row would leave it naming none
// until someone ran the backfill again. The drain is not the only pass that meets such a row: a
// pass that reads the store's statement as stale names it too (below).
//
// Where another release indexed the document after the store last stated its blocks
// (blockStand.stale), the rows the pass compares the render with are that release's, and they no
// longer name everything the store holds: the release before this one deletes the rows of the
// blocks and the links its version drops, inserts the rows of those it adds naming no node, and
// states no node gone, retracts no tuple and asserts none. The pass then also reads the store's
// own statement of the document (heldDocumentBlocks) and states, from the render, what a version
// dropping those rows would have stated: each block the store holds in any state but gone that
// the version does not hold, gone (goneSinceStated), and each link the store asserts out of a
// block of the document that no pair of the version writes, retracted (unheldLinks). That is no
// repair: what decides each statement is the version's render, and the store is read only for
// what it would go on asserting against it, as planBlockParents reads it for the parents. Such a
// pass also names the node of every block_refs row that survives naming none, stating the link
// that row projects, as the drain does, whether or not the document holds the mark: a version
// this build writes before any tick of this build reads the event of that release's version is
// such a pass, and it moves store_version to its own version, after which no tick marks the
// document (docs' markVersionedDocuments) and no drain reaches the row. A pass over a document
// only this release indexed reads none of it.
func PlanBlocks(ctx context.Context, q Queryer, index BlockIndex, rule int, now time.Time, version VersionBlocks) (BlockIndexing, error) {
	indexing := BlockIndexing{artifactID: version.ArtifactID, refKey: version.Record.RefKey, version: version.Number, versionAt: version.At, rule: rule, held: index}
	// The fence decides first, so a pass that loses it plans nothing and writes nothing.
	if !index.stand.admits(version.Number, rule) {
		return indexing, nil
	}
	stale := index.stand.stale()
	indexing.won, indexing.namesSurvivingRows = true, index.stand.owed || stale
	writer := version.Record.Writer
	nodes, fits := indexing.indexedNodes(version.Blocks)
	texts := indexing.planBlockTexts(index, version.Markdown, nodes, fits, now)
	statement, err := indexing.planBlockNodes(ctx, q, stale, nodes, texts, version.Record, now)
	if err != nil {
		return BlockIndexing{}, err
	}
	parents, placed, err := indexing.planBlockParents(ctx, q, index, nodes, fits, writer)
	if err != nil {
		return BlockIndexing{}, err
	}
	indexing.stated.Tuples = append(indexing.stated.Tuples, parents...)
	indexing.pairs = blockPairs(nodes, version.Located, version.Pointers)
	heldLinks, err := indexing.planLinks(ctx, q, index, statement, texts.changed, writer)
	if err != nil {
		return BlockIndexing{}, err
	}
	indexing.held = BlockIndex{blocks: texts.held, links: heldLinks, stand: index.stand.indexedAt(version.Number, rule), placed: placed}
	return indexing, nil
}

// indexedNodes is the step of a pass that reads the version's render (blocks): it answers the
// version's unique blocks (uniqueBlocks) and, beside each, whether its address fits the store's
// key (blockFits), which every later step reads rather than asking again, and it logs a warning
// naming the document and the version for the blocks past either bound.
func (b *BlockIndexing) indexedNodes(blocks []model.ArtifactBlock) ([]model.ArtifactBlock, []bool) {
	artifactID, version, refKey := b.artifactID, b.version, b.refKey
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
	return nodes, fits
}

// planBlockTexts is the step of a pass that compares each of the version's blocks (nodes, with
// fits whether each one's address fits the store's key) with the row the index holds of it,
// reading its text out of markdown: it records on the indexing the blocks whose text this
// version changed, with their types, sums, excerpts and texts, and the blocks it deleted, and
// answers what the later steps read of them (blockTexts). now is the transaction's now(), the
// time a row the pass inserts is dated by.
func (b *BlockIndexing) planBlockTexts(index BlockIndex, markdown string, nodes []model.ArtifactBlock, fits []bool, now time.Time) blockTexts {
	stored := maps.Clone(index.blocks)
	b.firstIndexing = len(stored) == 0
	// The blocks whose text this version changed (new, changed or restored), and those it
	// deleted. held is each block's row as the indexing leaves it, which a later indexing of the
	// document in this transaction plans against (Held): a row the pass writes names its node
	// wherever its address fits, since the parents' projection names it on every placed row.
	held := make(map[string]storedBlock, len(nodes))
	// The blocks of this version whose row names no node, each with the time that row was
	// written: a pre-store row, which the node step (planBlockNodes) gives its node and its
	// membership.
	var nodeless []nodelessBlock
	// Beside each block whose text this version changed, whether its address fits and whether
	// the pass inserts its row: such a block joins the document here and states its membership
	// of it, dated as the insert dates the row, by its column's default, now.
	var written []writtenBlock
	// A block whose text no version moved needs no string of its sum: the comparison against
	// the sum the index holds reads the hex out of one buffer this loop keeps, and only a
	// block this version writes a row for takes a string of its own.
	var sum blockSumHex
	for position, block := range nodes {
		body := markdown[block.From:block.To]
		blockTextSum(&sum, body)
		previous, kept := stored[block.ID]
		delete(stored, block.ID)
		if kept && !previous.named {
			nodeless = append(nodeless, nodelessBlock{id: block.ID, joined: previous.joined, fits: fits[position]})
		}
		if kept && previous.sum == string(sum[:]) {
			held[block.ID] = storedBlock{sum: previous.sum, joined: previous.joined, named: fits[position]}
			continue
		}
		hexSum := string(sum[:])
		joined := now
		if kept {
			joined = previous.joined
		}
		held[block.ID] = storedBlock{sum: hexSum, joined: joined, named: fits[position]}
		b.ids, b.types, b.sums = append(b.ids, block.ID), append(b.types, block.Type), append(b.sums, hexSum)
		b.excerpts = append(b.excerpts, text.HeadRunes(body, BlockExcerptRunes))
		b.textIDs, b.texts = append(b.textIDs, block.ID), append(b.texts, &body)
		written = append(written, writtenBlock{fits: fits[position], inserted: !kept})
	}
	// In id order, so the rows this pass writes and the facts it states come in an order a
	// reader of the log can predict, which a map's iteration would not give.
	b.deleted = slices.Sorted(maps.Keys(stored))
	for _, id := range b.deleted {
		b.textIDs, b.texts = append(b.textIDs, id), append(b.texts, nil)
	}
	// The same changed blocks by id, which the node step and the link step look a block up in.
	changed := make(map[string]struct{}, len(b.ids))
	for _, id := range b.ids {
		changed[id] = struct{}{}
	}
	return blockTexts{held: held, nodeless: nodeless, written: written, changed: changed}
}

// blockTexts is what planBlockTexts finds of the version's blocks against the rows the index
// holds, for the steps that state the blocks' nodes (planBlockNodes) and the links they hold
// (planLinks): held, each block's row as the indexing leaves it; nodeless, the blocks whose row
// names no node; written, beside each of BlockIndexing.ids in order, what the pass writes of that
// block; and changed, the same blocks by id.
type blockTexts struct {
	held     map[string]storedBlock
	nodeless []nodelessBlock
	written  []writtenBlock
	changed  map[string]struct{}
}

// writtenBlock is one block whose document_blocks row the pass writes: whether its address fits
// the store's key, as blockFits answered it once for the version's blocks, and whether the pass
// inserts its row.
type writtenBlock struct {
	fits, inserted bool
}

// planLinks is the step of a pass that plans the links the version's blocks hold (the
// indexing's pairs) against those index holds (planBlockRefs), adds what it states of them to
// the pass's write, and answers the links the index holds once the indexing is written.
// statement is the store's own statement of the document where that is stale (planBlockNodes),
// and changed the blocks whose text this version changed (blockTexts).
func (b *BlockIndexing) planLinks(
	ctx context.Context,
	q Queryer,
	index BlockIndex,
	statement map[string][]heldLink,
	changed map[string]struct{},
	writer model.Actor,
) ([]indexedLink, error) {
	// The links the version drops, in the order the index listed them, then those it adds and
	// those whose block types it changes, in the order the insert writes them: the targets the
	// two statements stand for. Then the target of every link the version writes in a block whose
	// text it changed, whose excerpt moved.
	links, heldLinks, err := b.planBlockRefs(ctx, q, index, writer)
	if err != nil {
		return nil, err
	}
	b.stated.Tuples = append(b.stated.Tuples, links...)
	// Where the store's statement is stale, the links another release's version dropped have
	// no row left for the delete to drop, and the store still asserts them (unheldLinks). They
	// move no count and no panel, and neither do the blocks goneSinceStated names: what a reader
	// is shown of a block and its links is read through their rows (graph_edges answers a
	// block's tuple only while its document_blocks or block_refs row stands), and that release
	// deleted those rows, and moved what they showed, when it wrote its version.
	b.stated.Tuples = append(b.stated.Tuples, unheldLinks(statement, b.refKey, b.pairs, links, b.versionAt, writer)...)
	// A pair held by a block whose text this version moved is a link whose excerpt moved with
	// it, which is the same set of blocks the node step (planBlockNodes) stated nodes for.
	for _, pair := range b.pairs {
		if _, wrote := changed[pair.blockID]; wrote {
			b.moved = append(b.moved, pair.target)
		}
	}
	return heldLinks, nil
}

// Moved is the targets of the links the indexing inserts or deletes, and of those whose block
// types it changes, whose counts move - a node is counted once for each block citing it - and of
// the links it writes in a block whose text it changes, whose excerpts move, a link's excerpt
// being the text of the block holding it.
func (b BlockIndexing) Moved() []Target {
	return b.moved
}

// Won says the indexing won the revision fence: a pass that lost it - one holding the same
// version as the document's blocks stand at, or a lower one - writes nothing.
func (b BlockIndexing) Won() bool {
	return b.won
}

// Stated is what the indexing states in the node and tuple store, held for the event that causes
// its write: the caller hands it to graphstore.QueueCaused right after that event, in the same
// write phase. It is the zero write for a pass that lost the fence.
func (b BlockIndexing) Stated() graphstore.Uncaused {
	return b.stated
}

// Cited is, of the blocks the indexing changes, restores or deletes (textIDs), each that a source
// cites (citedBlocks), as q holds them before the indexing is written: such a block's text is what
// every link to it and its own Referenced by panel show, so the version moved what they show. A
// block whose citing link the indexing itself inserts or deletes is among Moved already, so the
// read names the same targets the written index would.
func (b BlockIndexing) Cited(ctx context.Context, q Queryer) ([]Target, error) {
	return citedBlocks(ctx, q, b.artifactID, b.textIDs)
}

// Held is what the document's block index holds once the indexing is written.
func (b BlockIndexing) Held() BlockIndex {
	return b.held
}

// Queue queues the indexing's writes on a write phase's batch, which its caller runs in the
// transaction holding the document's owner lock: the revision fence first (queueStand), then the
// nodes of the blocks it states (graphstore.QueueNodes), then the blocks the version deleted,
// which document_blocks loses, and the blocks it changed, with their excerpts and their node
// columns; the text each of those blocks has at this version, recorded in document_block_texts, a
// deleted block's as no text, after which the edges at it stop resolving until a version holds
// its id again; the parent of each block it places, with its node columns; the links its blocks
// hold; and the version recorded as indexed (document_indexed_versions). It queues no window of
// its own. A pass that lost the fence queues nothing, and one whose row the fence's statement
// refuses fails the batch there (queueStand). Whether the stand clears the document's mark is
// the stand's own statement's to decide, and none of the statements after it reads the mark, so
// a pass whose stand leaves a mark of a higher rule writes what it writes otherwise and no
// statement of it assumes the mark gone.
//
// A pair (block, target) that survives keeps its created version, time and source event. A new
// pair takes this version and versionAt, and the source event QueueStamp gives it. On the
// document's first indexing (no block rows yet), a new pair dates instead from the document's own
// refs row for that target, when one exists.
func (b BlockIndexing) Queue(batch *store.WriteBatch) {
	if !b.won {
		return
	}
	artifactID, version := b.artifactID, b.version
	b.queueStand(batch)
	graphstore.QueueNodes(batch, b.stated)
	if len(b.deleted) > 0 {
		batch.Queue(fmt.Sprintf("delete the blocks version %d of %s no longer holds", version, artifactID), `
			delete from document_blocks where artifact_id = $1 and block_id = any($2)
		`, artifactID, b.deleted)
	}
	if len(b.ids) > 0 {
		batch.Queue(fmt.Sprintf("record the blocks version %d of %s changed", version, artifactID), `
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
		`, artifactID, version, b.versionAt, b.ids, b.types, b.sums, b.excerpts, BlockNodeKind, MaxBlockNodeBytes)
	}
	if len(b.textIDs) > 0 {
		batch.Queue(fmt.Sprintf("record the block texts of version %d of %s", version, artifactID), `
			insert into document_block_texts (artifact_id, block_id, version, text)
			select $1::uuid, t.id, $2, t.text from unnest($3::text[], $4::text[]) as t (id, text)
			on conflict (artifact_id, block_id, version) do update set text = excluded.text
		`, artifactID, version, b.textIDs, b.texts)
	}
	b.queueBlockParents(batch)
	b.queueBlockRefs(batch)
	batch.Queue(fmt.Sprintf("record version %d of %s indexed", version, artifactID), `
		insert into document_indexed_versions (artifact_id, version) values ($1, $2)
		on conflict (artifact_id, version) do nothing
	`, artifactID, version)
}

// IndexBlocks indexes version's blocks of artifactID in tx at once: the document's index read
// (ReadBlockIndex) with what the version's own row says of it (ReadIndexedVersion), the plan
// (PlanBlocks) and the blocks it cites (Cited), and its statements run as a batch of their own
// (Queue, store.RunWrite). It is the whole index for a writer with no write phase to queue it on:
// the seed, and a test that indexes a version itself, each at this build's marking rule
// (BlockIndexMarkingRule). It answers the targets the indexing moves, the cited blocks among them,
// whether it won the revision fence, and what it states in the node and tuple store, for the
// caller to append under its cause (graphstore.Caused).
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
) ([]Target, bool, graphstore.Uncaused, error) {
	index, err := ReadBlockIndex(ctx, tx, artifactID)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	record, err := ReadIndexedVersion(ctx, tx, artifactID, version)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&now); err != nil {
		return nil, false, graphstore.Uncaused{}, fmt.Errorf("read the transaction's time: %w", err)
	}
	indexing, err := PlanBlocks(ctx, tx, index, BlockIndexMarkingRule, now, VersionBlocks{
		ArtifactID: artifactID, Record: record, Number: version, At: versionAt,
		Markdown: markdown, Blocks: blocks, Located: located, Pointers: pointers,
	})
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	cited, err := indexing.Cited(ctx, tx)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		indexing.Queue(b)
		return nil
	}); err != nil {
		return nil, false, graphstore.Uncaused{}, err
	}
	return slices.Concat(indexing.Moved(), cited), indexing.Won(), indexing.Stated(), nil
}

// DocumentWindowKind is the embeddings kind of a window of a document's blocks, as
// ItemKind.WindowKind is of a comment's, a message's or an ask's.
const DocumentWindowKind = "document_block"

// windowKinds is every kind an embeddings window row is written under. A statement naming them
// as literals is one the planner can prove embeddings_window_owner's partial predicate for,
// whatever a parameter holds (writeWindows).
const windowKinds = `('document_block', 'comment_block', 'message_block', 'ask_part')`

// BlockEmbeddingStates is where meaning search stands on each block of the owner whose windows
// are kind kind under ownerKey, as who reads it: model.BlockEmbedded for a block every window
// covering it holds a vector of its current text for, model.BlockEmbeddingPending while one of
// them waits for its vector. A block no window covers is in no entry of the map, which a reader of
// one block reads as model.BlockNotEmbedded - a block the indexer puts in no window, and every
// block of an owner whose windows are not written yet. The read names the window kinds as
// literals beside the parameter, as every read and write of a window does (writeWindows), so the
// planner can prove embeddings_window_owner's partial predicate and reach the owner's rows through
// that index.
//
// The windows are read only while who may see their owner, the registry's predicate of that node
// (VisibleKind): the document the key is the uuid of, or the comment, message or ask `<kind>/<uuid>`
// names. An owner who may not see answers as one no window covers.
//
// It lives here, with the writes of the rows it reads, so the list of window kinds is held in
// one place: this package's windowKinds, migration 20261102233630's index predicate and the
// column the two of them name.
func BlockEmbeddingStates(ctx context.Context, q Queryer, who model.Actor, kind, ownerKey string) (map[string]string, error) {
	var args queryArgs
	kindParam, ownerParam := args.param(kind), args.param(ownerKey)
	var owner string
	if itemKind, itemID, item := strings.Cut(ownerKey, "/"); item {
		owner = args.visible(&who, itemKind, args.param(itemID))
	} else {
		owner = args.visible(&who, "artifact", artifactRefKeyOf(args.param(ownerKey)+`::uuid`))
	}
	rows, err := q.Query(ctx, `
		select covered.block_id, bool_and(e.embedded_hash is not distinct from e.content_hash)
		from embeddings e cross join unnest(e.covers) as covered(block_id)
		where e.kind = `+kindParam+` and e.kind in `+windowKinds+`
		  and split_part(e.id, '#', 1) = `+ownerParam+` and `+owner+`
		group by covered.block_id
	`, args.values...)
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
// transaction, which holds the document's owner, and stamps the state those blocks are in
// beside them, so the window arm can tell these windows from ones an older build left behind:
// blocks are the document's blocks in document order, each parent before its children, and
// markdown the text they index into. The owner key of a document's windows is its artifact id,
// and the context of each is the document's own title and the heading above the window
// (itemblocks.DocumentWindows).
//
// It is the one writer of a document's windows. A version write records the blocks and marks
// the document owed the windows that stand for them, which the block-index tick's window arm
// writes (BlockIndexing.Queue, docs.windowDocument), windowing the stored latest version's
// markdown - the text the version write indexed - a tick behind that write.
//
// The title the windows embed is read first, and the one statement that writes them is the only
// one after it, so the transaction takes its transaction id at that statement and holds it only
// to the commit.
func WriteDocumentWindows(ctx context.Context, tx pgx.Tx, artifactID, markdown string, blocks []model.ArtifactBlock) error {
	title, err := documentTitle(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	nodes, _, _ := uniqueBlocks(blocks)
	// The state is read from document_block_versions inside the statement, which also clears the
	// marker that listed the document, so the clear and the windows it stands for cannot come
	// apart (DocumentsOwedWindows).
	return writeWindows(ctx, tx, DocumentWindowKind, artifactID,
		itemblocks.DocumentWindows(title, documentBlockTexts(markdown, nodes)), blockState{
			query: `update document_block_versions v set windows_owed_at = null
			          where v.artifact_id = $5::uuid returning ` + documentBlockState + ` as block_state`,
			args: []any{artifactID},
		})
}

// documentTitle is the name document artifactID's artifacts row holds, which the windows of its
// blocks embed: the one reader of it is the window arm's write above, whose document is stored.
func documentTitle(ctx context.Context, q Queryer, artifactID string) (string, error) {
	var title string
	if err := q.QueryRow(ctx, `select name from artifacts where id = $1::uuid`, artifactID).Scan(&title); err != nil {
		return "", fmt.Errorf("read the title of %s for its windows: %w", artifactID, err)
	}
	return title, nil
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
// the write runs as a common table expression and stamps beside the windows. Both owners'
// queries are updates: an item's clears the marker that listed it on the kind's state row and
// returns the state that row holds (clearItemWindowsOwed), and a document's clears the same
// marker on document_block_versions and returns the version its blocks are indexed at
// (WriteDocumentWindows). The query numbers its parameters from $5 and passes their values in
// args, since writeWindows holds $1 to $4.
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
// alike (clearItemWindowsOwed, WriteDocumentWindows). It is the last statement of the
// transaction that sends it, and everything it writes was decided before it, so the transaction
// takes its transaction id here and holds it only to the commit (WriteDocumentWindows,
// WriteItemWindows). The windows go through jsonb_to_recordset in the top-level select; the
// state query, the stamp of what it returns and the delete are common table expressions, and
// Postgres runs a data-modifying one exactly once and to completion whether or not the select
// reads it. The delete cannot race the enqueue: it names only ids the new set does not hold, and
// the enqueue writes only ids it does.
//
// The delete names the kind twice, as the parameter and in the literal list of every window
// kind, because a generic plan knows nothing of what the parameter holds: the literal list is
// what lets the planner prove embeddings_window_owner's partial predicate and read the owner's
// rows through that index.
func writeWindows(ctx context.Context, tx pgx.Tx, kind, ownerKey string, windows []itemblocks.Window, state blockState) error {
	statement, args, err := windowsStatement(kind, ownerKey, windows, state)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, statement, args...); err != nil {
		return fmt.Errorf("write the windows of %s %s: %w", kind, ownerKey, err)
	}
	return nil
}

// queueWindows is writeWindows' one statement queued on a write phase's batch, for a writer that
// decides the windows before it writes anything and sends its writes as one batch: the delete of
// a post, whose item is left with no block and so with no window (TombstoneItem).
func queueWindows(b *store.WriteBatch, kind, ownerKey string, windows []itemblocks.Window, state blockState) error {
	statement, args, err := windowsStatement(kind, ownerKey, windows, state)
	if err != nil {
		return err
	}
	b.Queue(fmt.Sprintf("write the windows of %s %s", kind, ownerKey), statement, args...)
	return nil
}

// windowsStatement is the one statement writeWindows sends and queueWindows queues, with its
// arguments.
func windowsStatement(kind, ownerKey string, windows []itemblocks.Window, state blockState) (string, []any, error) {
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
		return "", nil, fmt.Errorf("encode the windows of %s %s: %w", kind, ownerKey, err)
	}
	return `
		with state as (
			` + state.query + `
		),
		stamped as (
			insert into embedding_window_states (kind, owner_key, block_state, written_at)
			select $1::text, $2::text, state.block_state, now() from state
			on conflict (kind, owner_key) do update
			  set block_state = excluded.block_state, written_at = excluded.written_at
		),
		dropped as (
			delete from embeddings
			where kind = $1 and kind in ` + windowKinds + `
			  and split_part(id, '#', 1) = $2 and id <> all($4::text[])
		)
		select embeddings_enqueue_window($1, $2 || '#' || w.suffix, w.text, w.covers)
		from jsonb_to_recordset($3::jsonb) as w (suffix text, text text, covers text[])
	`, append([]any{kind, ownerKey, payload, ids}, state.args...), nil
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
// the blocks and leaves the windows here (BlockIndexing.Queue, INFRA-988).
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
func citedBlocks(ctx context.Context, q Queryer, artifactID string, ids []string) ([]Target, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cited, err := scanTargets(q.Query(ctx, `
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
// dated from (TupleInDocument), and whether that row names the block's node.
type storedBlock struct {
	sum    string
	joined time.Time
	named  bool
}

// storedBlocks is each block the document's index holds, by id.
//
// It reads the node columns and the row's created_at beside the digest, in the one read of the
// document's rows the plan takes, because the pass writes a node and a membership for every row
// that names none (PlanBlocks) and a second read of the same rows would cost a scan of the
// document for every version.
func storedBlocks(ctx context.Context, q Queryer, artifactID string) (map[string]storedBlock, error) {
	rows, err := q.Query(ctx, `
		select block_id, text_sha256, created_at, node_kind is not null and node_address is not null
		from document_blocks where artifact_id = $1
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the blocks of %s: %w", artifactID, err)
	}
	defer rows.Close()
	stored := make(map[string]storedBlock)
	for rows.Next() {
		var id string
		var held storedBlock
		if err := rows.Scan(&id, &held.sum, &held.joined, &held.named); err != nil {
			return nil, fmt.Errorf("scan a block of %s: %w", artifactID, err)
		}
		stored[id] = held
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

// blockPairKey is how block_refs keys a pair: all of it but the block types it points at, which
// a surviving pair takes from the version being indexed.
type blockPairKey struct {
	blockID string
	kind    string
	target  Target
}

// key is the pair as block_refs keys it.
func (p blockPair) key() blockPairKey {
	return blockPairKey{blockID: p.blockID, kind: p.kind, target: p.target}
}

// heldTarget is a target as one block holds it.
type heldTarget struct {
	blockID string
	target  Target
}

// blockPairs is each distinct link the pointers of nodes and the mentions in located write: a
// pointer from its typed block (pointerPairs), a mention from the innermost of nodes holding its
// start. A mention no block holds, and an external URL, write none; nor does a block's mention of
// a target its own pointers account for (PlanBlocks).
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

// BlockLinks lists each link from a block of document fromArtifact to a block of document
// toArtifact (artifact uuids; the two may be one document), newest first, then by the source
// block's node id, then by the target's. A link counts only while both of its blocks are current
// blocks of their documents: a deleted block's links drop out, and its stored mentions bring them
// back when a version holds its id again. A pointer's link counts only while its target has a
// type the pointer allows, tested as the arm of graph_edges that answers the row tests it
// (blockRefListed). The links are read only while who may see both documents (the artifact
// predicate of each), and a link either of whose blocks who may not see drops out the same way,
// since neither end resolves to a node.
func BlockLinks(ctx context.Context, q Queryer, who model.Actor, fromArtifact, toArtifact string) ([]model.BlockLink, error) {
	rows, err := q.Query(ctx, `
		select r.kind, r.from_node, r.to_id, r.created_at, r.created_version
		from block_refs r
		join document_blocks f on f.artifact_id = r.artifact_id and f.block_id = r.block_id
		join document_blocks t on t.node_id = r.to_id and t.artifact_id = $2::uuid
		where r.artifact_id = $1::uuid and r.to_kind = 'block' and `+blockRefListed("r")+`
		  and `+VisibleKind("artifact", artifactRefKeyOf(`$1::uuid`), "$3")+`
		  and `+VisibleKind("artifact", artifactRefKeyOf(`$2::uuid`), "$3")+`
		order by r.created_at desc, r.from_node, r.to_id`, fromArtifact, toArtifact, VisibilityArg(who))
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
// and that version's time, as document_blocks records them (BlockIndexing.Queue).
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

// DocumentAddress is where a document sits: the reference key its blocks' node ids are built
// from, the issue holding it, the parent of an issue this transaction creates too, and the
// dispatch:// address a citation of the document reports. A write hands the address over rather
// than a read of it taking those from the artifacts row, because a transaction creating the
// document has only queued that row: its own reads cannot see it, while the caller knows what
// the queued insert will write. It is what a read of the document's address answers before and
// after the write phase has run (docs.addressOfDocument), and what a citation of the document
// resolves to meanwhile (Pending.PendingArtifact).
type DocumentAddress struct {
	// RefKey is the document's reference key, its owner's key and its slug (migration 0071).
	RefKey string
	// IssueKey is the issue the document belongs to, "" for a project's or a session's document.
	IssueKey string
	// Ref is the document's own dispatch:// address, as ArtifactRef spells it from the row the
	// insert writes: the reference a Referenced by panel is opened on, which an event naming
	// the document as a changed target carries (model.ChangedReference.Ref).
	Ref string
	// PendingParent is the parent of IssueKey when this transaction creates that issue's row
	// too, "" when it does not or when the issue is a root. The ancestors walk starts there
	// while the issues row is only queued: no read of this transaction can see that row, so the
	// walk from the issue itself (IssueAncestors) would answer nothing, and the ancestors are
	// that parent followed by its own. A pending issue with no parent is a root, and both paths
	// answer the empty list.
	PendingParent string
	// Project is the project the document belongs to, an issue's or its own, and Session the
	// session whose conversation holds it, "" where it is a project's: the scope its block
	// nodes carry (IndexedVersion's Project and Parties). Only an address this transaction
	// recorded for a row it is creating carries them; a stored row's version is read with its
	// scope (ReadIndexedVersion).
	Project, Session string
}

// BlockTargets is where a write to document artifactID, which sits at address, lands and the
// block each of targets names, by node id (text.TargetID), as the block index would hold it once
// that write, leaving the blocks written (rendered, in document order, each parent before its
// children), is indexed, read through q: a block of that document as written's node for its id
// (uniqueBlocks: the first holder of a repeated id, and no id the index cannot key), any other as
// its document's current blocks (document_blocks) hold it, with one query for every such target.
// A target no block has is absent. The issue's ancestors are read only once a target turns out to
// sit in another document (IssueAncestors, walked from DocumentAddress.PendingParent when the
// address names one), since a pointer inside the writing document - a plan task's `needs`, the
// common case - is held to nothing above it (pmdoc.CheckBlockPointers), and this runs inside the
// write's transaction.
func BlockTargets(
	ctx context.Context,
	q Queryer,
	artifactID string,
	address DocumentAddress,
	written []model.ArtifactBlock,
	targets []text.Ref,
) (pmdoc.BlockWriting, map[string]pmdoc.BlockTarget, error) {
	writing := pmdoc.BlockWriting{Document: address.RefKey, Issue: address.IssueKey}
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
	// for only here. A pending parent stands at the head of the answer, the walk starting there
	// (DocumentAddress.PendingParent).
	above := writing.Issue
	if address.PendingParent != "" {
		above = address.PendingParent
		writing.Ancestors = append(writing.Ancestors, address.PendingParent)
	}
	ancestors, err := IssueAncestors(ctx, q, above)
	if err != nil {
		return pmdoc.BlockWriting{}, nil, err
	}
	writing.Ancestors = append(writing.Ancestors, ancestors...)
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
