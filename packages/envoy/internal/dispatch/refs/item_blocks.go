package refs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// ItemKind is the kind of item whose blocks a call of this file writes: a comment, a message or
// an ask, whose blocks are the parts of its question, options and answer (PlanAskParts). It
// names the tables a call writes and is never caller input.
type ItemKind string

const (
	ItemComment ItemKind = "comment"
	ItemMessage ItemKind = "message"
	ItemAsk     ItemKind = "ask"
)

// itemTables is the storage of one item kind and its blocks: the items table, the blocks table,
// the column naming the item in every block table, the state table, the block mentions table and
// the table of each revision's block texts, which a kind with no revisions has none of. makers is
// whether a block of the kind records its own maker (made_by): an ask's answer part was made by
// whoever answered, where every block of a comment or a message was made by its author.
type itemTables struct {
	items, blocks, item, state, refs, texts string
	makers                                  bool
}

func (kind ItemKind) tables() (itemTables, error) {
	switch kind {
	case ItemComment:
		return itemTables{items: "comments", blocks: "comment_blocks", item: "comment_id", state: "comment_block_state", refs: "comment_block_refs", texts: "comment_block_texts"}, nil
	case ItemMessage:
		return itemTables{items: "messages", blocks: "message_blocks", item: "message_id", state: "message_block_state", refs: "message_block_refs"}, nil
	case ItemAsk:
		return itemTables{items: "asks", blocks: "ask_blocks", item: "ask_id", state: "ask_block_state", refs: "ask_block_refs", texts: "ask_block_texts", makers: true}, nil
	}
	return itemTables{}, fmt.Errorf("index the blocks of a %q: not an item kind", string(kind))
}

// revisions is whether the kind counts its indexed revisions and records the text its blocks held
// at each: a comment's and an ask's, never a message's, which is never edited.
func (tables itemTables) revisions() bool { return tables.texts != "" }

// BlockKind is the storage kind of a block of an item of this kind, in graph_edges and refs:
// comment_block, message_block or ask_block. The API names every block node kind block; the
// storage kind keeps a build that predates item blocks from counting or resolving their rows
// (TargetOf).
func (kind ItemKind) BlockKind() string {
	return string(kind) + "_block"
}

// WindowKind is the embeddings kind of a window of an item's blocks: comment_block,
// message_block, and ask_part for an ask, whose blocks are the parts of its question, options
// and answer rather than blocks of a body.
func (kind ItemKind) WindowKind() string {
	if kind == ItemAsk {
		return "ask_part"
	}
	return kind.BlockKind()
}

// WindowOwnerKey is the owner key of the windows of item itemID: `<kind>/<uuid>`, the id
// grammar the item's block nodes already use (ParseItemBlockNode), so a window's id reads as
// `<kind>/<uuid>#<the first block it covers>`.
func (kind ItemKind) WindowOwnerKey(itemID string) string {
	return string(kind) + "/" + itemID
}

// ParseItemBlockNode reads an item block's node id: the kind of item it is a block of, that
// item's uuid and the block's id. That id is `<item kind>/<item uuid>#<block id>` -
// `comment/<uuid>#b2`, `message/<uuid>#b1`, `ask/<uuid>#b3` for a part of an ask - the one
// shape every block table's node_id column holds, which text.TargetID writes for a reference to
// one and the index writes into the rows. ok is false for any other text — a document block's
// node id, which is its document's ref_key and names no item kind (`KEY/slug#b2`), and a kind
// this build does not know among them.
func ParseItemBlockNode(nodeID string) (kind ItemKind, itemID, blockID string, ok bool) {
	item, blockID, found := strings.Cut(nodeID, "#")
	if !found {
		return "", "", "", false
	}
	name, itemID, found := strings.Cut(item, "/")
	if !found {
		return "", "", "", false
	}
	kind = ItemKind(name)
	if _, err := kind.tables(); err != nil {
		return "", "", "", false
	}
	return kind, itemID, blockID, true
}

// CarriedEdit is the block ids an edit of a comment carries (PATCH /api/v1/comments/{id}'s
// `blocks` and `base_revision`): the comment's revision the editor read its blocks at, and each
// carried block's id with its From in UTF-16 code units into the body the edit writes, in body
// order.
type CarriedEdit struct {
	BaseRevision int
	Blocks       []itemblocks.Carried
}

// IndexItem records body, written at `at` (the created_at of an insert, the edited_at of an
// edit), as the current revision of the blocks of comment or message itemID, in the caller's
// transaction. It is ReadItemIndex, planItemBlocks and Queue back to back, the queued statements
// run at once as a batch of their own (store.RunWrite); a transaction that decides everything
// before it writes calls the three apart and queues the writes in its write phase. An ask indexes
// its parts through the same core (PlanAskParts, ItemIndexing.Queue).
//
// The caller holds the item's owner - its advisory lock (store.LockOwner), which the
// reconciliation takes too (IndexOwedItemBlocks) - and the index's first queued statement takes
// the item's state row (inserting it when missing, then `for no key update`), so the index of one
// item is written by one transaction at a time. A direct message has no owner, and no site
// rewrites a message, so every writer of its index plans the same index from the same body.
//
// The body splits into blocks (itemblocks.Split) and each takes an id: when carried is given, its
// base revision is the comment's stored revision and the item is a comment, each block takes the
// id of the carried span starting in it (itemblocks.Carry, after itemblocks.ReadCarried refuses an
// id that is not a current block, an id named twice, or a From outside the body or out of order,
// with a *itemblocks.CarriedError naming the field); otherwise, and for a carried edit made
// against another revision, the ids follow the text (itemblocks.Align over every block the item
// ever held, removed ones included). A new block takes made_at = at, except at the first indexing
// of a comment already edited, whose blocks' making time was never recorded (null); a new, changed
// (identity hash) or restored block takes the new revision as changed_revision and `at` as
// changed_at; an unchanged one moves only its position, type and excerpt; a current block the body
// no longer holds is removed (position null, removed_at = at). A comment records the text of each
// new, changed or restored block, and null for each removed one, in comment_block_texts at the new
// revision.
//
// Revisions: a comment's revision counts its indexed revisions, 1 the first indexing. A call bumps
// it by one when it is the first indexing, when body differs byte for byte from the body last
// indexed (body_sha256), or when the blocks changed (a parser that reads the same body otherwise);
// a call that finds none of those writes no block and no text row and keeps the revision. A
// message has no revisions.
//
// Every call reconciles the item's block mentions from located (text.ExtractAt(body, …)): each
// mention to the block whose [From, To) holds its offset, one row per (block, target), so a
// reindex with a new grammar re-reads them even when the body is unchanged. A new row takes `at`,
// or at the item's first indexing its own refs row's created_at and source_seq for that target;
// Stamp gives the others their event. A direct message passes no located mentions and holds none.
// It stores the revision, the next block number and itemblocks.BodySHA256(body) and clears
// owed_at.
//
// It returns the targets of the block mentions it inserted or deleted, which the caller's event
// counts (ItemIndexing.Moved).
func IndexItem(
	ctx context.Context,
	tx pgx.Tx,
	kind ItemKind,
	itemID, body string,
	at time.Time,
	located []text.Located,
	carried *CarriedEdit,
) ([]Target, error) {
	index, err := ReadItemIndex(ctx, tx, kind, itemID)
	if err != nil {
		return nil, err
	}
	indexing, err := planItemBlocks(index, kind, itemID, at, itemBodySource(kind, body, located, carried))
	if err != nil {
		return nil, err
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		indexing.Queue(b)
		return nil
	}); err != nil {
		return nil, err
	}
	return indexing.Moved(), nil
}

// itemBlockSource is one reading of an item's blocks, which the index writes: the blocks in
// order, the sum of the text they were read from (the state row's body_sha256), how each takes
// its id against the blocks the item holds, the maker each records (made_by, for a kind whose
// blocks have one: nil, or an entry per block, null where the item's own author made it), and the
// links the blocks hold once their ids are known. The context the blocks are embedded with is
// not read here: the index records the blocks alone, and the window arm reads that context for
// itself when it writes the windows (itemWindowText).
type itemBlockSource struct {
	blocks  []itemblocks.Block
	bodySum string
	ids     func(stored []itemblocks.Stored, next []itemblocks.Block, revision, counter int) ([]string, int, error)
	madeBy  []*string
	pairs   func(next []itemblocks.Block, ids []string) []blockPair
}

// itemBodySource is the reading of an item's blocks that a body makes, which IndexItem writes and
// PlanItemIndex plans: the blocks body splits into, the sum its state row records, the ids the
// carried spans or the text give them, and the links the mentions of located write. A comment's
// and a message's blocks record no maker of their own, so the source names none.
func itemBodySource(kind ItemKind, body string, located []text.Located, carried *CarriedEdit) itemBlockSource {
	return itemBlockSource{
		blocks:  itemblocks.Split(body),
		bodySum: itemblocks.BodySHA256(body),
		ids: func(stored []itemblocks.Stored, next []itemblocks.Block, revision, counter int) ([]string, int, error) {
			if carried != nil && kind == ItemComment && carried.BaseRevision == revision {
				spans, err := itemblocks.ReadCarried(stored, body, carried.Blocks)
				if err != nil {
					return nil, 0, err
				}
				ids, counter := itemblocks.Carry(stored, next, spans, counter)
				return ids, counter, nil
			}
			ids, counter := itemblocks.Align(stored, next, counter, nil)
			return ids, counter, nil
		},
		pairs: func(next []itemblocks.Block, ids []string) []blockPair {
			return itemBlockPairs(next, ids, located)
		},
	}
}

// ItemIndex is what a comment's, a message's or an ask's block index holds: its state row, every
// block the item ever held with the stored excerpt of each current one, and the block mentions
// those blocks hold.
type ItemIndex struct {
	state    itemState
	stored   []itemblocks.Stored
	excerpts map[string]string
	held     []itemBlockLink
}

// itemBlockLink is one stored block mention of an item: the block holding it, the edge kind and
// the target, the columns the reconciliation matches a link on.
type itemBlockLink struct {
	blockID string
	kind    string
	target  Target
}

// ReadItemIndex reads what q holds of comment, message or ask itemID's block index: its state
// row, the blocks it holds and their mentions, the mentions read with the predicate the
// reconciling delete visits them by. An item no block-aware build indexed holds no block and no
// block mention - the index writes them in the transaction that marks the state indexed, and a
// state row goes only with its item - and so does one whose row q does not hold yet, the item
// whose insert the calling transaction has queued.
func ReadItemIndex(ctx context.Context, q Queryer, kind ItemKind, itemID string) (ItemIndex, error) {
	tables, err := kind.tables()
	if err != nil {
		return ItemIndex{}, err
	}
	state, err := readItemState(ctx, q, kind, tables, itemID)
	if err != nil {
		return ItemIndex{}, err
	}
	index := ItemIndex{state: state, stored: []itemblocks.Stored{}}
	if !state.indexed {
		return index, nil
	}
	if index.stored, index.excerpts, err = storedItemBlocks(ctx, q, kind, tables, itemID); err != nil {
		return ItemIndex{}, err
	}
	if index.held, err = heldItemBlockLinks(ctx, q, kind, tables, itemID); err != nil {
		return ItemIndex{}, err
	}
	return index, nil
}

// Edited is the index with the item marked edited, which an edit's own read of the item does not
// show when the update that sets edited_at is queued and has not run. It matters at a first
// indexing alone: the blocks of a comment or an ask edited before this write were made at a time
// the item never recorded, so they record none (PlanItemIndex, madeAt).
func (i ItemIndex) Edited() ItemIndex {
	i.state.edited = true
	return i
}

// ItemIndexing is one body's index of an item's blocks and their mentions (IndexItem), planned
// against what the index holds (PlanItemIndex), so the targets it moves are known before anything
// is written, and written by Queue, which puts its statements on a write phase's batch and does
// nothing else.
type ItemIndexing struct {
	kind   ItemKind
	tables itemTables
	itemID string
	at     time.Time
	// madeAt is what a new block records as its making time: `at`, or null at the first indexing
	// of a comment or an ask already edited, whose blocks' making time was never recorded.
	madeAt              *time.Time
	revision, nextBlock int
	bodySHA256          string
	// changed reports that this body's blocks differ from the indexed ones, so the index takes a
	// new revision and writes them; an index that finds them unchanged writes no block row.
	changed       bool
	written, kept []itemBlockRow
	removed       []string
	pairs         []blockPair
	firstIndexing bool
	moved         []Target
	// current is the excerpt of each block the body holds, by block id: what a reply naming one
	// of them reads of the index before it is written (Block).
	current map[string]string
	// index is what the item's block index held when this one was planned against it, which Held
	// carries forward into what it leaves.
	index ItemIndex
}

// PlanItemIndex plans the index of body, written at `at`, of comment or message itemID against
// index, what its block index holds (ReadItemIndex): the blocks, their ids, the links their
// mentions write and the targets they move. It reads nothing. A carried id it refuses returns a
// *itemblocks.CarriedError. An ask's parts are planned from their own reading of its blocks
// through the same core (PlanAskParts, planItemBlocks).
func PlanItemIndex(
	index ItemIndex,
	kind ItemKind,
	itemID, body string,
	at time.Time,
	located []text.Located,
	carried *CarriedEdit,
) (ItemIndexing, error) {
	return planItemBlocks(index, kind, itemID, at, itemBodySource(kind, body, located, carried))
}

// planItemBlocks plans the index of source, read at `at`, of item itemID against index, what its
// block index holds: the blocks, their ids, the maker each records, the links their mentions
// write and the targets they move. It reads nothing and writes nothing; what it plans is written
// by Queue.
func planItemBlocks(
	index ItemIndex,
	kind ItemKind,
	itemID string,
	at time.Time,
	source itemBlockSource,
) (ItemIndexing, error) {
	tables, err := kind.tables()
	if err != nil {
		return ItemIndexing{}, err
	}
	state, stored := index.state, index.stored
	indexing := ItemIndexing{
		kind: kind, tables: tables, itemID: itemID, at: at,
		bodySHA256: source.bodySum, firstIndexing: !state.indexed, index: index,
	}
	counter := state.nextBlock
	for _, block := range stored {
		if number, ok := blockNumber(block.ID); ok && number >= counter {
			counter = number + 1
		}
	}

	next := source.blocks
	ids, counter, err := source.ids(stored, next, state.revision, counter)
	if err != nil {
		return ItemIndexing{}, err
	}

	byID := make(map[string]itemblocks.Stored, len(stored))
	for _, block := range stored {
		byID[block.ID] = block
	}
	indexing.changed = !state.indexed || state.bodySHA256 != indexing.bodySHA256
	indexing.current = make(map[string]string, len(next))
	for position, block := range next {
		row := itemBlockRow{
			id: ids[position], position: position, nodeType: block.Type, sum: block.Hash,
			excerpt: text.HeadRunes(block.Text, BlockExcerptRunes), text: block.Text,
		}
		if position < len(source.madeBy) {
			row.madeBy = source.madeBy[position]
		}
		indexing.current[row.id] = row.excerpt
		previous, held := byID[row.id]
		delete(byID, row.id)
		if held && previous.Position >= 0 && previous.Hash == row.sum {
			indexing.kept = append(indexing.kept, row)
			if previous.Position != position || previous.Type != block.Type {
				indexing.changed = true
			}
			continue
		}
		indexing.written = append(indexing.written, row)
		indexing.changed = true
	}
	for _, block := range stored {
		if _, untaken := byID[block.ID]; untaken && block.Position >= 0 {
			indexing.removed = append(indexing.removed, block.ID)
			indexing.changed = true
		}
	}

	indexing.revision, indexing.nextBlock = state.revision, counter
	if indexing.changed {
		indexing.revision++
		indexing.madeAt = &at
		if !state.indexed && tables.revisions() && state.edited {
			indexing.madeAt = nil
		}
	}

	// The links the body drops, in the order the index listed them, then those it adds, in the
	// order the insert writes them: the targets the delete and the insert move.
	indexing.pairs = source.pairs(next, ids)
	// An item's links carry no block types, so a pair is its key (blockPairKey) and no link is
	// kept while its types change, as a document's typed block attribute can be.
	written := make(map[blockPairKey]struct{}, len(indexing.pairs))
	for _, pair := range indexing.pairs {
		written[pair.key()] = struct{}{}
	}
	stillHeld := make(map[blockPairKey]struct{}, len(index.held))
	for _, link := range index.held {
		pair := blockPairKey{blockID: link.blockID, kind: link.kind, target: link.target}
		stillHeld[pair] = struct{}{}
		if _, still := written[pair]; !still {
			indexing.moved = append(indexing.moved, pair.target)
		}
	}
	for _, pair := range indexing.pairs {
		if _, already := stillHeld[pair.key()]; !already {
			indexing.moved = append(indexing.moved, pair.target)
		}
	}
	// A link a block keeps shows that block's excerpt in its target's referenced-by panel, so a
	// block that keeps its id while its excerpt changes names the targets of the links it holds,
	// as a document's reworded block does; a change past the excerpt's runes names none.
	newExcerpts := make(map[string]string, len(indexing.written))
	for _, row := range indexing.written {
		newExcerpts[row.id] = row.excerpt
	}
	for _, pair := range indexing.pairs {
		if previous, held := index.excerpts[pair.blockID]; held {
			if excerpt, rewritten := newExcerpts[pair.blockID]; rewritten && excerpt != previous {
				indexing.moved = append(indexing.moved, pair.target)
			}
		}
	}
	return indexing, nil
}

// Moved is the targets of the block mentions the index inserts or deletes, and of those it keeps
// in a block whose excerpt it changes.
func (i ItemIndexing) Moved() []Target {
	return i.moved
}

// Held is what the item's block index holds once this index is written, which a second index of
// the same item in one transaction plans against, so it sees the rows the first one queued
// (PlanAskParts). It reads the plan alone: the state this index records, the blocks the source
// holds in its order followed by the removed ones, the ones this index removes first, as a read
// after the write orders them (storedItemBlocks), the excerpt of each block the source holds,
// and the links its blocks hold once the reconciliation has run.
func (i ItemIndexing) Held() ItemIndex {
	held := ItemIndex{
		state: itemState{
			indexed: true, revision: i.revision, nextBlock: i.nextBlock,
			bodySHA256: i.bodySHA256, edited: i.index.state.edited,
		},
		stored:   make([]itemblocks.Stored, 0, len(i.index.stored)+len(i.written)),
		excerpts: i.current,
		held:     make([]itemBlockLink, 0, len(i.pairs)),
	}
	for _, rows := range [][]itemBlockRow{i.written, i.kept} {
		for _, row := range rows {
			held.stored = append(held.stored, itemblocks.Stored{
				ID: row.id, Type: row.nodeType, Hash: row.sum, Position: row.position,
			})
		}
	}
	slices.SortFunc(held.stored, func(first, second itemblocks.Stored) int {
		return first.Position - second.Position
	})
	// A block this index removes loses its position at a removed_at later than every earlier
	// removal's, so a read after the write puts it before them, and among themselves by id, the
	// order storedItemBlocks takes. Each keeps the type and the text it was written with.
	var earlier []itemblocks.Stored
	removing := len(held.stored)
	for _, block := range i.index.stored {
		if _, holds := i.current[block.ID]; holds {
			continue
		}
		removed := block.Position < 0
		block.Position = -1
		if removed {
			earlier = append(earlier, block)
			continue
		}
		held.stored = append(held.stored, block)
	}
	slices.SortFunc(held.stored[removing:], func(first, second itemblocks.Stored) int {
		return cmp.Or(cmp.Compare(len(first.ID), len(second.ID)), cmp.Compare(first.ID, second.ID))
	})
	held.stored = append(held.stored, earlier...)
	for _, pair := range i.pairs {
		held.held = append(held.held, itemBlockLink{blockID: pair.blockID, kind: pair.kind, target: pair.target})
	}
	return held
}

// Block is the node id and excerpt of block blockID as the index leaves it, the row its insert
// writes (queueBlocks): what a reply naming a block of an item this transaction is indexing
// reads in place of the stored block (CurrentItemBlock). found is false when the body holds no
// such block.
func (i ItemIndexing) Block(blockID string) (nodeID, excerpt string, found bool) {
	excerpt, found = i.current[blockID]
	if !found {
		return "", "", false
	}
	return string(i.kind) + "/" + i.itemID + "#" + blockID, excerpt, true
}

// Queue queues the index's writes on a write phase's batch: the item's state row taken first,
// the blocks the body changed and removed, the block texts of a kind that records them, the
// links its blocks hold, and last the statement that records the item indexed
// (queueRecordIndexed).
func (i ItemIndexing) Queue(b *store.WriteBatch) {
	i.queueStateLock(b)
	if i.changed {
		i.queueBlocks(b)
	}
	i.queueBlockRefs(b)
	i.queueRecordIndexed(b)
}

// queueRecordIndexed queues the statement that records the item indexed from the text whose sum
// is bodySHA256, at the revision with nextBlock as the next block number for a kind that counts
// revisions, clears the block index's marker and leaves the window arm's standing.
//
// The windows of these blocks are not written by this index. The block-index tick's window arm
// writes them in a transaction of its own (docs.windowItem, WriteItemWindows), so a write holds
// the owner it took - the lock every writer of the item takes - for its own rows alone.
//
// What a reader meets for it: a comment's windows, and so its hits in meaning search, appear one
// reconciliation tick after the write rather than within it, and a window of text an edit removed
// stands until that tick rewrites the owner's set.
//
// The two markers part here. owed_at is this write's to settle: the blocks it names are the
// blocks this transaction wrote. windows_owed_at is not, since the windows of those blocks are
// the window arm's, and only the statement that writes them clears it (clearItemWindowsOwed,
// ItemsOwedWindows). It is left at the time it already carries - the trigger of the write that
// owes these blocks set it in that write's own transaction - and set to now() for an index no
// such write marked, a rebuild or an item an older build left behind, so that an index and the
// marker for the windows it did not write are one statement of one transaction: a crash after it
// leaves the item owed its windows rather than silently windowless.
//
// An item indexed to no block is marked like any other, and the arm lists it: a comment whose
// body is empty holds none (api's createCommentFor takes such a comment when it carries a
// suggestion), and so does one whose last block an edit removed, which still holds the windows
// of the text it had. The sum written here is what the arm finds it by, since it has no current
// block row to be found by (ItemsOwedWindows, INFRA-988).
//
// A document's version write does the same, in the same shape: it records the blocks and marks
// the document owed its windows in the one transaction, for the tick's arm to write
// (BlockIndexing.Queue, DocumentsOwedWindows). That write is the statement INFRA-988 measured as
// the write path's blocker - 9.7% of an hour's database time over 31,241 calls at a 114.3 ms
// mean, holding the issue row while a comment's `select status, closed_at is null from issues
// where key = $1 for no key update` waited on it - and the two window writes are told apart in
// pg_stat_statements by their state query: an item's reads `with state as ( update
// comment_block_state s set …)`, a document's `with state as ( update document_block_versions v
// set windows_owed_at = null …)`.
func (i ItemIndexing) queueRecordIndexed(b *store.WriteBatch) {
	const owed = `owed_at = null, windows_owed_at = coalesce(windows_owed_at, now())`
	what := fmt.Sprintf("record the blocks of %s %s indexed", i.kind, i.itemID)
	if i.tables.revisions() {
		b.Queue(what, `
			update `+i.tables.state+` set revision = $2, next_block = $3, body_sha256 = $4, `+owed+`
			where `+i.tables.item+` = $1::uuid
		`, i.itemID, i.revision, i.nextBlock, i.bodySHA256)
		return
	}
	b.Queue(what, `
		update `+i.tables.state+` set body_sha256 = $2, `+owed+`
		where `+i.tables.item+` = $1::uuid
	`, i.itemID, i.bodySHA256)
}

// itemWindowBlocks is the item's blocks as the windows that embed them hold them: each block's
// text and node type under the id this index gives it.
func itemWindowBlocks(blocks []itemblocks.Block, ids []string) []itemblocks.WindowBlock {
	windowed := make([]itemblocks.WindowBlock, len(blocks))
	for i, block := range blocks {
		windowed[i] = itemblocks.WindowBlock{ID: ids[i], Type: block.Type, Text: block.Text}
	}
	return windowed
}

// itemBlockState is the state of an item's blocks that its windows are stamped with
// (clearItemWindowsOwed): the sum of the body they were split from, over the kind's state table
// as s. An item with no sum recorded - one no block-aware build has indexed - stamps as the
// empty string.
//
// Nothing in the server reads the stamp back: windows_owed_at says which items are owed their
// windows (ItemsOwedWindows), and that marker replaced the comparison this stamp served. Two
// readers of it are left, which is why the write still makes it. Migration 20261107203841 reads
// it once, to mark every item whose windows were already stale when the column arrived, and the
// release before this one decides owedness from it, so a rollback finds the stamp it has always
// read (20261107194001.up.sql, "Rolling deploy").
const itemBlockState = `coalesce(s.body_sha256, '')`

// clearItemWindowsOwed is the query the one write of an item's windows runs for its stamp: it
// keeps the state the item's state row already holds, which is the state the blocks those
// windows cover are indexed at, and clears windows_owed_at. It is an update rather than a read
// so that clearing that marker and writing the windows it stands for are one statement, which
// is the whole of the marker's rule (ItemsOwedWindows).
func clearItemWindowsOwed(tables itemTables, itemID string) blockState {
	return blockState{
		query: `update ` + tables.state + ` s set windows_owed_at = null
		        where s.` + tables.item + ` = $5::uuid returning ` + itemBlockState + ` as block_state`,
		args: []any{itemID},
	}
}

// itemState is an item's state row as the index found it: whether a block-aware build indexed it
// before (a revision above 0 where the kind counts revisions, a message's body_sha256 set), its
// revision and next block number (a message's are 0 and 1), the sum of the text it was last
// indexed from, and whether the item was edited (edited_at set, which a message has not).
type itemState struct {
	indexed    bool
	revision   int
	nextBlock  int
	bodySHA256 string
	edited     bool
}

// readItemState is the item's state row as the index finds it, read under no lock: the index's
// write phase takes the row (queueStateLock) and the owner's lock the caller holds, the
// reconciliation included, excludes every other writer of the item until then (IndexItem). An
// item with no state row, one no block-aware build indexed, and one whose insert the calling
// transaction has queued, read as never indexed.
func readItemState(ctx context.Context, q Queryer, kind ItemKind, tables itemTables, itemID string) (itemState, error) {
	state := itemState{nextBlock: 1}
	var sum *string
	var err error
	if tables.revisions() {
		err = q.QueryRow(ctx, `
			select coalesce(s.revision, 0), coalesce(s.next_block, 1), s.body_sha256,
			       i.edited_at is not null
			from `+tables.items+` i left join `+tables.state+` s on s.`+tables.item+` = i.id
			where i.id = $1::uuid
		`, itemID).Scan(&state.revision, &state.nextBlock, &sum, &state.edited)
		state.indexed = state.revision > 0
	} else {
		err = q.QueryRow(ctx, `
			select s.body_sha256
			from `+tables.items+` i left join `+tables.state+` s on s.`+tables.item+` = i.id
			where i.id = $1::uuid
		`, itemID).Scan(&sum)
		state.indexed = sum != nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return itemState{nextBlock: 1}, nil
	}
	if err != nil {
		return itemState{}, fmt.Errorf("read the block state of %s %s: %w", kind, itemID, err)
	}
	if sum != nil {
		state.bodySHA256 = *sum
	}
	return state, nil
}

// heldItemBlockLinks is the block mentions the item's blocks hold, read with the predicate the
// reconciling delete visits them by, so the index knows what it moves before it writes.
func heldItemBlockLinks(ctx context.Context, q Queryer, kind ItemKind, tables itemTables, itemID string) ([]itemBlockLink, error) {
	rows, err := q.Query(ctx, `
		select block_id, kind, to_kind, to_id from `+tables.refs+` where `+tables.item+` = $1::uuid
	`, itemID)
	if err != nil {
		return nil, fmt.Errorf("read the block mentions of %s %s: %w", kind, itemID, err)
	}
	defer rows.Close()
	held := []itemBlockLink{}
	for rows.Next() {
		var link itemBlockLink
		if err := rows.Scan(&link.blockID, &link.kind, &link.target.Kind, &link.target.ID); err != nil {
			return nil, fmt.Errorf("scan a block mention of %s %s: %w", kind, itemID, err)
		}
		held = append(held, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the block mentions of %s %s: %w", kind, itemID, err)
	}
	return held, nil
}

// queueStateLock queues the item's state row, the first statements of the index's write phase:
// its insert when it has none, then the row `for no key update`, which the reconciliation takes
// as well (IndexOwedItemBlocks), so the index of one item is written by one transaction at a
// time. Its row is not read back: the index was planned from the state the read phase found.
func (i ItemIndexing) queueStateLock(b *store.WriteBatch) {
	b.Queue(fmt.Sprintf("create the block state of %s %s", i.kind, i.itemID), `
		insert into `+i.tables.state+` (`+i.tables.item+`) values ($1::uuid) on conflict do nothing
	`, i.itemID)
	b.Queue(fmt.Sprintf("lock the block state of %s %s", i.kind, i.itemID), `
		select 1 from `+i.tables.state+` where `+i.tables.item+` = $1::uuid for no key update
	`, i.itemID)
}

// storedItemBlocks is every block the item ever held, current ones first in body order, then
// removed ones, the most recently removed first, so Align's first untaken match prefers the block
// in place and, among removed blocks of equal text, the last removed.
func storedItemBlocks(ctx context.Context, q Queryer, kind ItemKind, tables itemTables, itemID string) ([]itemblocks.Stored, map[string]string, error) {
	rows, err := q.Query(ctx, `
		select block_id, type, text_sha256, coalesce(position, -1), excerpt from `+tables.blocks+`
		where `+tables.item+` = $1::uuid
		order by position nulls last, removed_at desc, length(block_id), block_id
	`, itemID)
	if err != nil {
		return nil, nil, fmt.Errorf("read the blocks of %s %s: %w", kind, itemID, err)
	}
	defer rows.Close()
	stored := []itemblocks.Stored{}
	excerpts := map[string]string{}
	for rows.Next() {
		var block itemblocks.Stored
		var excerpt string
		if err := rows.Scan(&block.ID, &block.Type, &block.Hash, &block.Position, &excerpt); err != nil {
			return nil, nil, fmt.Errorf("scan a block of %s %s: %w", kind, itemID, err)
		}
		stored = append(stored, block)
		if block.Position >= 0 {
			excerpts[block.ID] = excerpt
		}
	}
	return stored, excerpts, rows.Err()
}

// blockNumber is the number of block id b<N>.
func blockNumber(id string) (int, bool) {
	digits, ok := strings.CutPrefix(id, "b")
	if !ok {
		return 0, false
	}
	number, err := strconv.Atoi(digits)
	return number, err == nil
}

// itemBlockRow is one current block the index writes: its id, position, type, identity hash,
// excerpt, text and, for a kind whose blocks record one, its maker as the json its made_by column
// holds (nil: the item's own author made it).
type itemBlockRow struct {
	id       string
	position int
	nodeType string
	sum      string
	excerpt  string
	text     string
	madeBy   *string
}

// queueBlocks queues one new revision of the item's blocks: written (new, changed or restored
// blocks) take the revision and `at` as their changed facts, and a new one madeAt; kept blocks
// move their position, type and excerpt; removed ones lose their position at `at`. A kind with
// revisions records each written block's text, and null for each removed one, at the revision. A
// kind whose blocks record a maker writes each written block's; a kept block keeps the maker it
// was written with, as it keeps its text.
func (i ItemIndexing) queueBlocks(b *store.WriteBatch) {
	kind, tables, itemID, revision := i.kind, i.tables, i.itemID, i.revision
	if len(i.removed) > 0 {
		b.Queue(fmt.Sprintf("remove the blocks %s %s no longer holds", kind, itemID), `
			update `+tables.blocks+` set position = null, removed_at = $2
			where `+tables.item+` = $1::uuid and block_id = any($3)
		`, itemID, i.at, i.removed)
	}
	if len(i.written) > 0 {
		ids, positions, types, sums, excerpts, makers := itemBlockColumns(i.written)
		what := fmt.Sprintf("record the blocks of %s %s", kind, itemID)
		if tables.revisions() {
			// Only a kind whose blocks record a maker has the column, so the three fragments are
			// empty for the others; kind is this package's own constant, never caller input.
			maker, makerValue, makerUpdate := "", "", ""
			if tables.makers {
				maker, makerValue, makerUpdate = ", made_by", ", b.made_by::jsonb", ", made_by = excluded.made_by"
			}
			b.Queue(what, `
				insert into `+tables.blocks+` (`+tables.item+`, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_revision, changed_at`+maker+`)
				select $1::uuid, b.id, '`+string(kind)+`/' || $1::uuid::text || '#' || b.id, b.position, b.type, b.sum, b.excerpt, $2, $3, $4`+makerValue+`
				from unnest($5::text[], $6::int[], $7::text[], $8::text[], $9::text[], $10::text[]) as b (id, position, type, sum, excerpt, made_by)
				on conflict (`+tables.item+`, block_id) do update set
					position = excluded.position,
					type = excluded.type,
					text_sha256 = excluded.text_sha256,
					excerpt = excluded.excerpt,
					changed_revision = excluded.changed_revision,
					changed_at = excluded.changed_at,
					removed_at = null`+makerUpdate+`
			`, itemID, i.madeAt, revision, i.at, ids, positions, types, sums, excerpts, makers)
		} else {
			// A message is never edited, so its blocks are made and last changed when it was
			// written; a reindex under another parser moves only those of new text.
			b.Queue(what, `
				insert into message_blocks (message_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_at)
				select $1::uuid, b.id, 'message/' || $1::uuid::text || '#' || b.id, b.position, b.type, b.sum, b.excerpt, $2, $2
				from unnest($3::text[], $4::int[], $5::text[], $6::text[], $7::text[]) as b (id, position, type, sum, excerpt)
				on conflict (message_id, block_id) do update set
					position = excluded.position,
					type = excluded.type,
					text_sha256 = excluded.text_sha256,
					excerpt = excluded.excerpt,
					changed_at = excluded.changed_at,
					removed_at = null
			`, itemID, i.at, ids, positions, types, sums, excerpts)
		}
	}
	if len(i.kept) > 0 {
		ids, positions, types, _, excerpts, _ := itemBlockColumns(i.kept)
		b.Queue(fmt.Sprintf("move the unchanged blocks of %s %s", kind, itemID), `
			update `+tables.blocks+` b set position = k.position, type = k.type, excerpt = k.excerpt
			from unnest($2::text[], $3::int[], $4::text[], $5::text[]) as k (id, position, type, excerpt)
			where b.`+tables.item+` = $1::uuid and b.block_id = k.id
		`, itemID, ids, positions, types, excerpts)
	}
	if !tables.revisions() || len(i.written)+len(i.removed) == 0 {
		return
	}
	textIDs := make([]string, 0, len(i.written)+len(i.removed))
	texts := make([]*string, 0, len(i.written)+len(i.removed))
	for index := range i.written {
		textIDs, texts = append(textIDs, i.written[index].id), append(texts, &i.written[index].text)
	}
	for _, id := range i.removed {
		textIDs, texts = append(textIDs, id), append(texts, nil)
	}
	b.Queue(fmt.Sprintf("record the block texts of revision %d of %s %s", revision, kind, itemID), `
		insert into `+tables.texts+` (`+tables.item+`, block_id, revision, text)
		select $1::uuid, t.id, $2, t.text from unnest($3::text[], $4::text[]) as t (id, text)
		on conflict (`+tables.item+`, block_id, revision) do update set text = excluded.text
	`, itemID, revision, textIDs, texts)
}

// itemBlockColumns is rows as the parallel arrays an unnest reads.
func itemBlockColumns(rows []itemBlockRow) (ids []string, positions []int32, types, sums, excerpts []string, makers []*string) {
	for _, row := range rows {
		ids = append(ids, row.id)
		positions = append(positions, int32(row.position))
		types = append(types, row.nodeType)
		sums = append(sums, row.sum)
		excerpts = append(excerpts, row.excerpt)
		makers = append(makers, row.madeBy)
	}
	return ids, positions, types, sums, excerpts, makers
}

// itemBlockPairs is each distinct link the mentions in located write from the block of next
// (whose ids are ids) whose [From, To) holds the mention's start. A mention no block holds, and
// an external URL, write none. next's blocks are top-level, apart and in body order, and located
// is in offset order, so blockPairs' innermost holder is the one block holding each start.
func itemBlockPairs(next []itemblocks.Block, ids []string, located []text.Located) []blockPair {
	nodes := make([]model.ArtifactBlock, len(next))
	for index, block := range next {
		nodes[index] = model.ArtifactBlock{ID: ids[index], From: block.From, To: block.To}
	}
	return blockPairs(nodes, located, nil)
}

// queueBlockRefs queues the reconciliation of the item's block mentions with the links its blocks
// hold, matching on the columns of their key: the delete of the links the body dropped and the
// insert of the ones it holds. A link that survives keeps its created_at and source_seq; a new
// one takes `at`, or at the item's first indexing the item's own refs row for that target's
// created_at and source_seq, when one exists. At the first indexing the item holds no block
// mention, so nothing is deleted. The targets the two statements move are already known
// (PlanItemIndex), so neither returns its rows.
func (i ItemIndexing) queueBlockRefs(b *store.WriteBatch) {
	kind, tables, itemID, pairs := i.kind, i.tables, i.itemID, i.pairs
	blockIDs := make([]string, len(pairs))
	kinds := make([]string, len(pairs))
	toKinds := make([]string, len(pairs))
	toIDs := make([]string, len(pairs))
	for index, pair := range pairs {
		blockIDs[index], kinds[index], toKinds[index], toIDs[index] = pair.blockID, pair.kind, pair.target.Kind, pair.target.ID
	}
	if !i.firstIndexing {
		b.Queue(fmt.Sprintf("delete the block mentions %s %s dropped", kind, itemID), `
			delete from `+tables.refs+` r
			where r.`+tables.item+` = $1::uuid and not exists (
				select 1 from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
				where p.block_id = r.block_id and p.kind = r.kind and p.to_kind = r.to_kind and p.to_id = r.to_id)
		`, itemID, blockIDs, kinds, toKinds, toIDs)
	}
	if len(pairs) == 0 {
		return
	}
	b.Queue(fmt.Sprintf("record the block mentions of %s %s", kind, itemID), `
		insert into `+tables.refs+` (`+tables.item+`, block_id, from_node, to_kind, to_id, kind, created_at, source_seq)
		select $1::uuid, p.block_id, $6::text || '/' || $1::uuid::text || '#' || p.block_id, p.to_kind, p.to_id, p.kind,
		       coalesce(case when $8::boolean then r.created_at end, $7::timestamptz), case when $8::boolean then r.source_seq end
		from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
		left join refs r on r.from_kind = $6 and r.from_id = $1::uuid::text
		                and r.to_kind = p.to_kind and r.to_id = p.to_id
		on conflict (`+tables.item+`, block_id, to_kind, to_id, kind) do nothing
	`, itemID, blockIDs, kinds, toKinds, toIDs, string(kind), i.at, i.firstIndexing)
}

// OwedItemBlock is one comment, message or ask owed a block index: its state row's owed_at is
// set, by the trigger on a write no block-aware build indexed (a build without the index wrote
// it, during the roll or after a rollback), or by rebuild-refs.
type OwedItemBlock struct {
	Kind ItemKind
	ID   string
}

// OwedItemBlocks lists at most limit comments, messages and asks owed a block index, oldest
// owed_at first, and locks nothing. Each table's read goes through its partial index on owed_at.
func OwedItemBlocks(ctx context.Context, q Queryer, limit int) ([]OwedItemBlock, error) {
	rows, err := q.Query(ctx, `
		select kind, id from (
			(select 'comment' as kind, comment_id::text as id, owed_at from comment_block_state
			 where owed_at is not null order by owed_at limit $1)
			union all
			(select 'message', message_id::text, owed_at from message_block_state
			 where owed_at is not null order by owed_at limit $1)
			union all
			(select 'ask', ask_id::text, owed_at from ask_block_state
			 where owed_at is not null order by owed_at limit $1)
		) owed
		order by owed_at, kind, id limit $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list the items owed a block index of their blocks: %w", err)
	}
	owed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OwedItemBlock, error) {
		var item OwedItemBlock
		err := row.Scan(&item.Kind, &item.ID)
		return item, err
	})
	if err != nil {
		return nil, fmt.Errorf("list the items owed a block index of their blocks: %w", err)
	}
	return owed, nil
}

// IndexOwedItemBlocks indexes one owed comment, message or ask in a transaction of its own and
// reports whether it indexed it. It takes the item's owner lock (readSourceUnderOwner), the lock
// every writer of the item takes, and reads the item in the statement after it, so a write
// committed while it waited is seen; a direct message, whose conversation is no owner, has no
// such lock and needs none, since nothing ever rewrites a message's body. It takes no item row: a
// row lock would assign the transaction a transaction id to hold through everything it reads and
// plans, and what it would exclude beyond the owner lock is a writer of the item row that takes
// no owner lock, which the census test refuses (lockItemOwner). Reading and planning come first
// and its writes follow back to back (IndexItemIfOwed). A new block and a new mention take the
// time the item's text was written: a comment's and an ask's
// coalesce(edited_at, created_at), a message's created_at. An item a write site indexed since it
// was listed is no longer owed and is left alone; one deleted since, before the lock or while it
// waited, took its state row with it (on delete cascade), so nothing is owed.
//
// It runs from the block-index reconciliation (docs.(*Service).ReconcileBlockIndex) and indexes
// through IndexItemIfOwed, which indexes nothing unless the item's index is owed, so every
// mention it records is of text an earlier write already appended an event for. A block mention
// takes the source_seq of the item's own refs row for its target at the item's first indexing
// (PlanItemIndex) and none otherwise, and no ?since= read lists a row with none until a later
// write of the item stamps it (Stamp).
func IndexOwedItemBlocks(ctx context.Context, pool Pool, item OwedItemBlock, serverURL string) (bool, error) {
	tables, err := item.Kind.tables()
	if err != nil {
		return false, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	read := `select true from ` + tables.items + ` where id = $1::uuid`
	var present bool
	found, err := readSourceUnderOwner(ctx, tx, string(item.Kind), item.ID, read, &present)
	if err != nil || !found {
		return false, err
	}
	indexed, err := IndexItemIfOwed(ctx, tx, item.Kind, item.ID, serverURL)
	if err != nil {
		return false, err
	}
	return indexed, tx.Commit(ctx)
}

// lockItemOwner takes the lock of the owner of comment, message or ask itemID - its issue, or
// the document a comment or an ask on a document hangs on - which every writer of the item
// takes, and reports whether the item is still there: one deleted since it was listed took its
// state row with it (on delete cascade), so nothing is owed of it. A direct message, whose
// conversation is no owner, has none and needs none, since nothing rewrites a message's body.
//
// It takes no row lock. main took the item's row `for no key update` where this lock now
// stands, which gives the transaction a transaction id to hold through everything it then reads
// and plans; an advisory lock gives it none (store.LockOwner, IndexOwedItemBlocks). The reads
// this lock guards come in the statements after it, so each sees what the lock's previous
// holder committed.
//
// What main's row lock covered and this lock does not: the row lock excluded every writer of
// that row, whether or not that writer held the owner's lock, where an advisory lock excludes
// exactly the writers that take it. So this lock rests on an invariant of the whole server -
// every writer of a comment, a message, an ask or one of their block-state rows takes that
// item's owner lock first - and that invariant is held by a census test rather than by this
// comment: api's TestEveryWriterOfAnItemRowIsOnTheOwnerLockCensus finds every write of those
// rows in the module and reds unless the function making it is on a list naming the lock it
// takes, so a `delete from comments` added tomorrow (dispatch://INFRA-1523) reds there before
// it ships.
//
// Two writers on that list hold no owner lock and cost this one nothing.
// docs.RestoreDecidedWordings runs once at boot under the copy locks alone, and refuses its
// restoration when the rows moved under it; refs.RebuildAll raises the owed markers of every
// comment, message and ask so the index re-derives them, and writes no item row and no other
// column of a state row. A third, docs.backfillAnchorBlocks, is the one writer of an item row
// that holds no owner lock while the server serves: it is an operator command pinning the block
// of each legacy quote anchor, one key of one column per row, over rows whose block_id is still
// unset, which is why the route that writes that same key writes only its own key too (api's
// commentAction).
//
// A new writer of one of those rows takes this lock before it reads what it writes:
// store.LockOwner on store.OwnerOf(the row's issue key, its artifact id), as lockItemOwner,
// api's lockedComment and lockCommentOwner and docs' lockArtifactOwner each take it, and then
// reads in the statement after it. Then it goes on the census with the lock it took.
func lockItemOwner(ctx context.Context, tx pgx.Tx, kind ItemKind, itemID string) (bool, error) {
	owner, found, err := sourceOwner(ctx, tx, string(kind), itemID)
	if err != nil || !found {
		return false, err
	}
	if owner == nil {
		return true, nil
	}
	return true, store.LockOwner(ctx, tx, *owner)
}

// WriteItemWindows writes the windows of comment, message or ask itemID's blocks, and nothing
// else, in the caller's transaction, for the block-index tick's window arm: it is to an item
// what WriteDocumentWindows is to a document. It takes the item's owner lock, the lock every
// writer of the item takes (lockItemOwner), reads under it the text the item's blocks were
// split from - its stored body and its issue's title in one statement, or an ask's parts
// (AskParts, itemWindowText) - pairs it with the item's current block rows, and sends the one
// statement that writes the windows under those ids, stamping the state the blocks are in
// beside them and clearing the marker that listed it in that same statement
// (clearItemWindowsOwed, writeWindows). It writes no block row and no reference: the index of
// this item's blocks stands, and only its windows are missing, so everything it reads is read
// before it holds a transaction id.
//
// It is the one writer of an item's windows. An index of the item's blocks records the blocks
// and leaves the windows that stand for them to this arm (ItemIndexing.Queue), so every set of
// windows an item holds was written here, one reconciliation tick behind the write of its text.
//
// It reports whether it wrote them. An item gone since it was listed is no longer anyone's to
// window. An item whose stored text no longer splits into the blocks the index holds - a parser
// that reads the same body otherwise, a body written since by a build that did not index it -
// is windowed by no shortcut: windows under those ids would cover blocks the item does not
// hold, so it is marked owed a block index instead (markItemBlocksOwed) and the next tick's
// owed arm indexes it whole, windows included.
func WriteItemWindows(ctx context.Context, tx pgx.Tx, kind ItemKind, itemID string) (bool, error) {
	tables, err := kind.tables()
	if err != nil {
		return false, err
	}
	found, err := lockItemOwner(ctx, tx, kind, itemID)
	if err != nil || !found {
		return false, err
	}
	blocks, windowContext, found, err := itemWindowText(ctx, tx, kind, tables, itemID)
	if err != nil || !found {
		return false, err
	}
	ids, aligned, err := currentItemBlockIDs(ctx, tx, kind, tables, itemID, blocks)
	if err != nil {
		return false, err
	}
	if !aligned {
		return false, markItemBlocksOwed(ctx, tx, kind, tables, itemID)
	}
	return true, writeWindows(ctx, tx, kind.WindowKind(), kind.WindowOwnerKey(itemID),
		itemblocks.Windows(windowContext, itemWindowBlocks(blocks, ids)), clearItemWindowsOwed(tables, itemID))
}

// itemWindowText is the blocks the item's stored text splits into, in order, and the context
// they are embedded under, read as the item's own index reads the text (IndexItem,
// PlanAskParts): a comment's or a message's body under its issue's title, so a reader
// searching for what the issue is about reaches the reply written under it, and an ask's parts
// under its question, which is what gives an option and an answer their meaning - "eu, for
// latency" is found by a reader asking which region was chosen only through the question it
// answers. A comment on a document and an agent's direct message stand on no issue and have no
// context: their blocks are embedded with their own text alone. found is false when the item is
// gone.
//
// A comment's and a message's two texts are one statement rather than two reads of the same row
// by the same key: this path runs once per comment, message and ask write now that no write of
// the text writes its own windows, and the seed pays it per item. Neither read takes a row
// lock, which would give the transaction a transaction id to hold through everything it then
// reads and plans: the caller holds the item's owner lock, which every writer of the item takes
// (WriteItemWindows, lockItemOwner), and the reads come in the statements after it, so each
// sees what the lock's previous holder committed.
//
// main read the comment's or message's row `for no key update` here, which held it against
// every writer of that row, one holding no owner lock included. This read holds it against the
// writers that take the owner's lock, and the census test is what makes those every writer
// there is (lockItemOwner, api's TestEveryWriterOfAnItemRowIsOnTheOwnerLockCensus). A new
// writer of the row takes that lock, or the text read here can move under the windows written
// from it.
func itemWindowText(ctx context.Context, tx pgx.Tx, kind ItemKind, tables itemTables, itemID string) ([]itemblocks.Block, string, bool, error) {
	if kind == ItemAsk {
		ask, _, found, err := askForParts(ctx, tx, itemID)
		if err != nil || !found {
			return nil, "", false, err
		}
		return AskParts(ask), ask.Question, true, nil
	}
	var body, title string
	switch err := tx.QueryRow(ctx, `
		select t.body, coalesce(i.title, '') from `+tables.items+` t
		left join issues i on i.key = t.issue_key
		where t.id = $1::uuid
	`, itemID).Scan(&body, &title); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, "", false, nil
	case err != nil:
		return nil, "", false, fmt.Errorf("read the text of %s %s for its windows: %w", kind, itemID, err)
	}
	return itemblocks.Split(body), title, true, nil
}

// currentItemBlockIDs is the id the index holds for each of blocks, the item's stored text as it
// splits now, paired as api's readItemBlockRows pairs them: the item's current block rows in
// body order, one for each block, each standing at that block's identity hash. aligned is false
// where they do not pair - another count of blocks, or a block whose hash moved - which says the
// rows were written from another reading of the text than this one.
func currentItemBlockIDs(ctx context.Context, tx pgx.Tx, kind ItemKind, tables itemTables, itemID string, blocks []itemblocks.Block) ([]string, bool, error) {
	stored, _, err := storedItemBlocks(ctx, tx, kind, tables, itemID)
	if err != nil {
		return nil, false, err
	}
	ids := make([]string, 0, len(blocks))
	for index, block := range blocks {
		if index >= len(stored) || stored[index].Position != index || stored[index].Hash != block.Hash {
			return nil, false, nil
		}
		ids = append(ids, stored[index].ID)
	}
	// A block the text no longer holds, which storedItemBlocks lists after the current ones.
	if len(stored) > len(blocks) && stored[len(blocks)].Position >= 0 {
		return nil, false, nil
	}
	return ids, true, nil
}

// markItemBlocksOwed marks the item owed a block index, keeping the time it has been owed since
// when it already is, so the owed arm takes it oldest first as it takes every other.
func markItemBlocksOwed(ctx context.Context, tx pgx.Tx, kind ItemKind, tables itemTables, itemID string) error {
	if _, err := tx.Exec(ctx, `
		update `+tables.state+` set owed_at = coalesce(owed_at, now()) where `+tables.item+` = $1::uuid
	`, itemID); err != nil {
		return fmt.Errorf("mark %s %s owed a block index: %w", kind, itemID, err)
	}
	return nil
}

// ItemsOwedWindows lists the id of at most limit comments, messages or asks of kind whose
// blocks the index holds - an item can hold none, and such a one is listed too - and whose
// windows do not stand for those blocks, in id order past after (empty: from the start), for
// the block-index tick's window arm. An item written before meaning search moved to windows
// has blocks and no window, and so has one whose write failed between the two. One a build
// without this change re-indexed during the deploy is the harder case: it keeps the windows of
// its old text, so holding a window says nothing.
//
// The read is of the kind's own marker, windows_owed_at (migration 20261107194001), which is
// the question written down rather than asked of state:
//
//   - the trigger on every write of the text an item's blocks are split from sets it, beside
//     the block index's owed_at (item_blocks_owe). It is set in the trigger so that the
//     release before this one marks the item too: that release writes the text, indexes the
//     blocks and clears owed_at without naming this column, which is the rolling-deploy case,
//     and it cannot clear a marker it does not know.
//   - rebuild-refs sets it with owed_at (RebuildAll), and migration 20261107203841 set it
//     once for every item whose windows were already stale when the column arrived, which no
//     later write would have marked.
//   - the one statement that writes an item's windows clears it, beside the stamp of the
//     state those windows were written from (writeWindows, clearItemWindowsOwed), so the clear
//     and the windows cannot come apart. An index of the item's blocks writes no window and
//     clears nothing of this arm's (queueRecordIndexed), so every item an index records is
//     left here for this arm, which windows it one tick behind. That statement's own
//     `dropped` is the only delete of a window row in the server; a route that removes one
//     otherwise marks its owner here, or this read will not see it.
//
// owed_at is no answer to this question: it marks the items owed a block index, which is
// nearly the complement of the items whose windows do not stand for the blocks the index
// already holds, so reading it would hand this arm the block arm's backlog and leave a drifted
// item unwindowed for ever.
//
// Which marked rows are this arm's is the question the lateral join answers, as the documents'
// does (documentsOwedWindowsQuery): an item the block index has reached - its state row
// carries the sum of the text it was indexed from - and an item holding a current block, which
// an index older than that sum left behind. An item indexed to no block at all is the first
// kind and not the second: a comment whose body is empty, which api's createCommentFor takes
// when the comment carries a suggestion, and an item whose last block an edit removed, whose
// block rows all read `position is null`. The arm windows such an item to no window, which
// deletes the windows its old text left and clears the marker in the one statement
// (WriteItemWindows, writeWindows); the semi-join this replaced dropped it, and since that
// statement is the only one that clears the marker, it stayed marked for ever - an entry in
// the partial index every later pass paid a probe for and no pass could ever remove
// (INFRA-988).
//
// An item that is neither - one an older build wrote, marked for both arms at once, indexed by
// no block-aware build and holding no block yet - is not this arm's: windows under no block
// are not its to write, the block arm indexes it whole, windows included, and that index marks
// it here again.
func ItemsOwedWindows(ctx context.Context, q Queryer, kind ItemKind, after string, limit int) ([]string, error) {
	tables, err := kind.tables()
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, itemsOwedWindowsQuery(tables), uuidAfter(after), limit)
	if err != nil {
		return nil, fmt.Errorf("list the %ss whose windows do not stand for their blocks: %w", kind, err)
	}
	items, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the %ss whose windows do not stand for their blocks: %w", kind, err)
	}
	return items, nil
}

// itemsOwedWindowsQuery is the statement ItemsOwedWindows sends for the kind whose tables are
// tables: $1 is the id its batch starts after and $2 the most ids it answers. The tests read
// the statement from here rather than from a copy of it (item_windows_owed_test.go).
//
// The driving rows are the marked rows of the kind's state table, read through its partial
// index on windows_owed_at, whose key is the item's id: the index holds the owed rows alone,
// in the order the batch pages by, so the keyset cursor and the limit are both the index's own
// and the read costs what the arm has to do rather than what the deployment has stored.
//
// The blocks are read through a lateral join rather than as `s.body_sha256 is not null or
// exists (…)`: an EXISTS under an OR is a correlated subplan the planner runs per marked row,
// which the plan test refuses, while the join is one probe for each marked row read, as the
// semi-join before it was, and the inner limit stops it at the first current block row.
//
// The two readings this replaced both walked the whole state table to decide owedness from
// state: the two correlated `not exists` probes under an `or`, 13.7 s a pass over the
// load-test deployment's 511,595 comment state rows for an answer of no rows, and the same
// predicate as anti-joins, 2.92 s a pass with 472 MB of temporary files written (INFRA-988).
func itemsOwedWindowsQuery(tables itemTables) string {
	return `
		select s.` + tables.item + `::text
		from ` + tables.state + ` s
		left join lateral (
			select 1 from ` + tables.blocks + ` b
			where b.` + tables.item + ` = s.` + tables.item + ` and b.position is not null
			limit 1
		) held (block) on true
		where s.windows_owed_at is not null
		  and s.` + tables.item + ` > $1::uuid
		  and (s.body_sha256 is not null or held.block is not null)
		order by s.` + tables.item + ` limit $2
	`
}
