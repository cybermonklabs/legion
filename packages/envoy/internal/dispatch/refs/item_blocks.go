package refs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
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

// ItemScope is what the store holds of the item whose blocks and citations a pass writes: whether
// it holds the item's node, and that node, whose visibility, project, parties and maker every
// block node of the item carries in its turn, so a block is read under its item's rules and its
// part_of and each citation point out of and at a node the store holds. An item the store holds
// no node of - one an older build wrote, which no backfill reached - has no block node and no
// citation either: its rows name no node and hold no tuples_stored, and its old arms answer them.
type ItemScope struct {
	held bool
	node graphstore.Node
}

// WrittenItem is the scope of an item whose node the calling write states itself, before the row
// that names it: a create that drew the item's id in its own transaction and queued the node's
// write there (an ask's, a comment's, a message's create). Every pass that indexes the item then
// takes node as the item's scope rather than read it back, since no read of the queued row can
// answer yet and no other transaction can name an id this one drew until it commits.
func WrittenItem(node graphstore.Node) ItemScope {
	return ItemScope{held: true, node: node}
}

// ReadItemScope reads ItemScope for item itemID of kind: the node the store holds of it
// (graphstore.ReadNode). An id that is no uuid names no item (a test's synthetic source), and has
// no node.
func ReadItemScope(ctx context.Context, q Queryer, kind ItemKind, itemID string) (ItemScope, error) {
	if _, err := kind.tables(); err != nil {
		return ItemScope{}, err
	}
	if _, ok := canonicalUUID(itemID); !ok {
		return ItemScope{}, nil
	}
	node, held, err := graphstore.ReadNode(ctx, q, string(kind), itemID)
	if err != nil {
		return ItemScope{}, fmt.Errorf("read the node of %s %s: %w", kind, itemID, err)
	}
	return ItemScope{held: held, node: node}, nil
}

// ItemBlockAddress is the SQL test that the block node address held by column is an item's block,
// `<item kind>/<item uuid>#<block id>`, and not a document's, whose address starts with its
// document's ref_key, a project key in capitals. The prefix is the one thing that tells the two
// apart in the store, where both are `block` nodes; a reader of document blocks alone (the store
// census's block kinds) leaves an item's out by it.
func ItemBlockAddress(column string) string {
	tests := make([]string, len(itemKinds))
	for index, item := range itemKinds {
		tests[index] = column + ` like '` + string(item) + `/%'`
	}
	return `(` + strings.Join(tests, " or ") + `)`
}

// itemBlockNodeAddress is the address of block blockID of item itemID in the store, which its
// row's node_id holds and a reply to it names: `<item kind>/<item uuid>#<block id>`
// (ParseItemBlockNode).
func itemBlockNodeAddress(kind ItemKind, itemID, blockID string) string {
	return string(kind) + "/" + itemID + "#" + blockID
}

// blockNodeOf is whether the block blockID of the item is a node of the store: its item is one,
// and its address fits the store's bound (graphstore.AddressFits). A block that is not has no
// node, its row names none, and nothing is stated of it, so its old arms answer it.
func (scope ItemScope) blockNodeOf(kind ItemKind, itemID, blockID string) bool {
	return scope.held && graphstore.AddressFits(itemBlockNodeAddress(kind, itemID, blockID))
}

// blockNode is the node of one block of the item, as a pass that writes the block states it: a
// block, of the closed supertag list's `block` (graphstore.KindBlock), in state, read under its
// item's visibility, project and parties, so it answers who may see it exactly as the item it is a
// part of does, made by maker - the item's maker, or whoever made the block where that is someone
// else (makerOf) - at `at` - a node already there keeps the made_at and made_by its first write
// gave it - and with no cause yet, which graphstore.QueueCaused gives it.
func (scope ItemScope) blockNode(address string, at time.Time, state string, maker model.Actor) graphstore.Node {
	return graphstore.Node{
		Kind: BlockNodeKind, ID: address, Supertag: BlockSupertag,
		Visibility: scope.node.Visibility, State: state,
		Project: scope.node.Project, Parties: scope.node.Parties,
		MadeAt: at, MadeBy: maker, SourceEvent: graphstore.NoEvent,
	}
}

// makerOf is who made block blockID of the item, which its node and every tuple it states name:
// the maker makers holds for it - an ask's answer part, made by whoever answered (askPartMakers)
// - and otherwise the item's own maker, who made every block of a comment or a message and every
// question and option part of an ask.
func (scope ItemScope) makerOf(makers map[string]model.Actor, blockID string) model.Actor {
	if maker, own := makers[blockID]; own {
		return maker
	}
	return scope.node.MadeBy
}

// revisions is whether the kind counts its indexed revisions and records the text its blocks held
// at each: a comment's and an ask's, never a message's. A message is edited since INFRA-1523,
// and its blocks are re-indexed at the edit, but it keeps no per-revision block text: the whole
// body an edit replaced is kept in post_body_history, which serves it to the history read.
func (tables itemTables) revisions() bool { return tables.texts != "" }

// postMarkColumns is the three columns of alias, a row of the kind's items table, that a read
// carries beside a comment's or a message's text: edited_at, deleted_at and deleted_by, in that
// order, which postMarks reads. An ask is no post a correction marks - no author deletes one, and
// its own edit (asks.edited_at) is its question's, which its own reads carry - so for an ask
// they are three nulls.
func (kind ItemKind) postMarkColumns(alias string) string {
	if kind == ItemAsk {
		return `null::timestamptz, null::timestamptz, null::jsonb`
	}
	return alias + `.edited_at, ` + alias + `.deleted_at, ` + alias + `.deleted_by`
}

// postMarks is the three columns postMarkColumns selects as a read carries them: the time of the
// last edit as it stands, and the delete mark as model.DeletedPostMark decodes it, the one
// decoder of that mark.
type postMarks struct {
	editedAt, deletedAt *time.Time
	deletedBy           []byte
}

// targets are the scan targets of postMarkColumns' three columns, in its order.
func (marks *postMarks) targets() []any {
	return []any{&marks.editedAt, &marks.deletedAt, &marks.deletedBy}
}

// deleted is the delete mark the columns hold, nil and nil for a post that stands.
func (marks postMarks) deleted() (*time.Time, *model.Actor, error) {
	return model.DeletedPostMark(marks.deletedAt, marks.deletedBy)
}

// ItemTable is where the items of this kind are stored: the table holding the items themselves
// ("comments"), and the column naming one of them, which every table keyed by an item carries
// ("comment_id"). It is the one map of a kind to its storage, which a caller outside this
// package reads rather than restating (api's postHistoryOwner, whose post_body_history is keyed
// by this column).
func (kind ItemKind) ItemTable() (items, item string, err error) {
	tables, err := kind.tables()
	if err != nil {
		return "", "", err
	}
	return tables.items, tables.item, nil
}

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

// refsHoldBlockLinks is whether an item's own refs rows hold every mention its blocks hold: a
// comment's and a message's do, since their rows and their blocks are read from the one body,
// and an ask's do not, since its row holds its question's links alone (itemBlockLinks), so a
// link written in an option or in the answer is in ask_block_refs and in no refs row of the
// ask. The out statement of the references read reads the links of an item's blocks from its
// block mentions table where this is false, and from its refs rows alone where it is true
// (linksOutQuery).
func (kind ItemKind) refsHoldBlockLinks() bool {
	return kind == ItemComment || kind == ItemMessage
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
// counts (ItemIndexing.Moved), and what it states in the node and tuple store, held for that
// event (ItemIndexing.Stated): a caller whose write appends one queues it with
// graphstore.QueueCaused after that event, and a pass no event records - the owed index, the
// re-index of old text, the seed - hands it over with graphstore.NoEvent, in the same
// transaction, before anything else writes the item's nodes again. serverURL is the deployment
// whose dashboard URLs each block's excerpt is cut around (BlockExcerpt).
func IndexItem(
	ctx context.Context,
	tx pgx.Tx,
	kind ItemKind,
	itemID, body, serverURL string,
	at time.Time,
	located []text.Located,
	carried *CarriedEdit,
) ([]Target, graphstore.Uncaused, error) {
	index, err := ReadItemIndex(ctx, tx, kind, itemID)
	if err != nil {
		return nil, graphstore.Uncaused{}, err
	}
	indexing, err := planItemBlocks(index, kind, itemID, at, itemBodySource(kind, body, serverURL, located, carried))
	if err != nil {
		return nil, graphstore.Uncaused{}, err
	}
	if err := store.RunWrite(ctx, tx, indexing.Queue); err != nil {
		return nil, graphstore.Uncaused{}, err
	}
	return indexing.Moved(), indexing.Stated(), nil
}

// itemBlockSource is one reading of an item's blocks, which the index writes: the blocks in
// order, the sum of the text they were read from (the state row's body_sha256), the deployment
// whose dashboard URLs each block's excerpt is cut around, how each takes its id against the
// blocks the item holds, the maker each records (made_by, for a kind whose blocks have one: nil,
// or an entry per block, nil where the item's own author made it), and the links the blocks hold
// once their ids are known. The context the blocks are embedded with is not read here: the index
// records the blocks alone, and the window arm reads that context for itself when it writes the
// windows (itemWindowText).
type itemBlockSource struct {
	blocks    []itemblocks.Block
	bodySum   string
	serverURL string
	ids       func(stored []itemblocks.Stored, next []itemblocks.Block, revision, counter int) ([]string, int, error)
	madeBy    []*model.Actor
	pairs     func(next []itemblocks.Block, ids []string) []blockPair
	// identities is each identity the text names, a person, a session or a role, whose mention
	// the index appends out of the item's node (identityTargets): a comment's and an issue
	// message's; an ask's parts and a direct message name none here.
	identities []Target
}

// itemBodySource is the reading of an item's blocks that a body makes, which IndexItem writes and
// PlanItemIndex plans: the blocks body splits into, the sum its state row records, the ids the
// carried spans or the text give them, the links the mentions of located write, and the
// identities those mentions name. A comment's and a message's blocks record no maker of their
// own, so the source names none.
func itemBodySource(kind ItemKind, body, serverURL string, located []text.Located, carried *CarriedEdit) itemBlockSource {
	return itemBlockSource{
		identities: identityTargets(located),
		blocks:     itemblocks.Split(body),
		bodySum:    itemblocks.BodySHA256(body),
		serverURL:  serverURL,
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

// TombstoneItem queues, on a write phase's batch, what the delete of comment or message itemID
// writes to the tables of its blocks besides the index of its cleared body. The caller holds the
// item's owner lock, as every writer of the item does (LockItemOwner), and has queued before this
// the update that clears the item's own body column and the index of that empty body.
//
// The body has four copies beside that column, and the delete takes it out of each in its one
// transaction:
//
//   - the block index, which the caller indexes again from the empty body, planned in its read
//     phase (api's clearDeletedPostBody, planItemWrite): the empty body splits into no block, so
//     each block's position is nulled, removed_at is stamped, a null text is written at the new
//     revision for a kind that records one, and each block mention goes (PlanItemIndex,
//     ItemIndexing.Queue).
//   - each block's stored excerpt, which a removal keeps and which is what a reply read serves
//     as the text of the block it answers (ReplyBlocks), so without this clear a reply still
//     prints the deleted text.
//   - the text each revision recorded of a block, for a kind that records them, which a pinned
//     block reference serves the latest of at or before its revision (ItemBlockAt) and which
//     the null text the re-index wrote at this revision does not reach.
//   - the item's embedding windows. The windows of an item left with no block are none, so the
//     statement that writes an item's windows (writeWindows) deletes every window the item held,
//     enqueues none, stamps the empty state the index leaves its blocks in and clears the marker
//     that listed it (clearItemWindowsOwed), so no meaning search finds the deleted item once
//     the delete commits. It is the write the window arm makes (WriteItemWindows) with nothing
//     of the item to read - the cleared body and its blocks are rows this transaction has only
//     queued - and it is queued after the index, whose record of the item indexed leaves that
//     marker set (queueRecordIndexed). The keyword arm drops the item with its body, and for a
//     hit of a deleted item all the same, which no path of this build produces, search stamps
//     the item's delete mark as the backstop (api's stampSearchPostMarks).
//
// The excerpts and the texts belong to the delete alone: an ordinary edit keeps the excerpt of a
// block it removed, and the texts it recorded are what a pin to an earlier revision of an item
// that stands reads. It reads nothing.
func TombstoneItem(b *store.WriteBatch, kind ItemKind, itemID string) error {
	tables, err := kind.tables()
	if err != nil {
		return err
	}
	b.Queue(fmt.Sprintf("clear the stored excerpts of %s %s", kind, itemID), `
		update `+tables.blocks+` set excerpt = '' where `+tables.item+` = $1::uuid and excerpt <> ''
	`, itemID)
	if tables.revisions() {
		b.Queue(fmt.Sprintf("clear the recorded block texts of %s %s", kind, itemID), `
			update `+tables.texts+` set text = null where `+tables.item+` = $1::uuid and text is not null
		`, itemID)
	}
	return queueWindows(b, kind.WindowKind(), kind.WindowOwnerKey(itemID), nil, clearItemWindowsOwed(tables, itemID))
}

// ItemIndex is what a comment's, a message's or an ask's block index holds: its state row, every
// block the item ever held with what the index reads of each row (storedItemBlock), the block
// mentions those blocks hold, the item's scope (ItemScope), and, at the first indexing of an item
// whose row is stored, its own refs rows, which date the links that first indexing writes; for an
// indexed ask, also its own refs rows, read beside its parts' links (PlanOwnRefs).
type ItemIndex struct {
	kind   ItemKind
	itemID string
	state  itemState
	stored []itemblocks.Stored
	rows   map[string]storedItemBlock
	held   []itemBlockLink
	scope  ItemScope
	// firstRefs is the item's own refs rows by target, read at the first indexing of an item whose
	// row is stored (an older build's, the owed index, a rebuild): the tuple that first indexing
	// states of a link it inserts is dated by the row's created_at and carries its source_seq for
	// its target (planItemBlocks), from the same refs row queueBlockRefs's insert copies them from.
	firstRefs map[Target]itemRefRow
	// own is an indexed ask's own refs rows, each with its kind and tuples_stored, and ownRead
	// whether the read of the index made them (PlanOwnRefs).
	own     []heldCitation
	ownRead bool
	// mentioned is each identity the store holds a mention of out of the item's node now, read
	// with the block mentions of a comment or message whose node the store holds, indexed or not
	// (heldItemBlockLinks, ReadItemIndex): what the index appends no mention of again
	// (identityMentions), and retracts each mention of that the text no longer names
	// (unnamedMentions). routes is each identity a comment's delivery routes name, read beside them
	// (its comment_mentions rows, api's commentMentionFacts): mentions the comment states apart from
	// its text, which no change of its text retracts.
	mentioned, routes map[Target]struct{}
}

// itemBlockLink is one stored block mention of an item: the block holding it, the edge kind and
// the target, the columns the reconciliation matches a link on, and whether the row names its
// block's node, which is the row a tuple of the store stands for.
type itemBlockLink struct {
	blockID string
	kind    string
	target  Target
	named   bool
}

// itemRefRow is the date of one of an item's own refs rows: its created_at and source_seq.
type itemRefRow struct {
	createdAt time.Time
	sourceSeq *int64
}

// ReadItemIndex reads what q holds of comment, message or ask itemID's block index: its state
// row, the blocks it holds and their mentions, the mentions read with the predicate the
// reconciling delete visits them by, and the item's scope, the node the store holds of it, read
// beside its state row (readItemState), with where the store places each of its block nodes now,
// read beside its block rows (storedItemBlocks). An item no block-aware build indexed holds no
// block and no block mention - the index writes them in the transaction that marks the state
// indexed, and a state row goes only with its item - and so does one whose row q does not hold
// yet, the item whose insert the calling transaction has queued, whose scope its caller hands over
// instead (Scoped, WrittenItem). The node of a comment or a message no block-aware build indexed
// can hold identity mentions all the same: the backfill states those an older build's text names
// without indexing its blocks (graphcensus' commentPointersPass, messagesPass and its pass over
// the queue of that build's events), and a comment states its delivery routes as mentions too. So
// they are read for such an item as for an indexed one (heldItemBlockLinks, mentioned and routes),
// and its first index retracts each its text no longer names. None of these reads takes a lock:
// the caller holds the item's owner lock, and the index's write phase takes the state row
// (queueStateLock).
func ReadItemIndex(ctx context.Context, q Queryer, kind ItemKind, itemID string) (ItemIndex, error) {
	tables, err := kind.tables()
	if err != nil {
		return ItemIndex{}, err
	}
	state, scope, exists, err := readItemState(ctx, q, kind, tables, itemID)
	if err != nil {
		return ItemIndex{}, err
	}
	index := ItemIndex{kind: kind, itemID: itemID, state: state, stored: []itemblocks.Stored{}}
	if !exists {
		return index, nil
	}
	index.scope = scope
	if !state.indexed {
		if index.scope.held {
			if index.firstRefs, err = itemRefRows(ctx, q, kind, itemID); err != nil {
				return ItemIndex{}, err
			}
			if kind != ItemAsk {
				if _, _, _, index.mentioned, index.routes, err = heldItemBlockLinks(ctx, q, kind, tables, itemID); err != nil {
					return ItemIndex{}, err
				}
			}
		}
		return index, nil
	}
	if index.stored, index.rows, err = storedItemBlocks(ctx, q, kind, tables, itemID, index.scope.held); err != nil {
		return ItemIndex{}, err
	}
	if index.held, index.own, index.ownRead, index.mentioned, index.routes, err = heldItemBlockLinks(ctx, q, kind, tables, itemID); err != nil {
		return ItemIndex{}, err
	}
	return index, nil
}

// Scoped is the index with the item's scope set to scope: the node the calling write states of an
// item whose row it has queued, which no read of the index could find (WrittenItem).
func (i ItemIndex) Scoped(scope ItemScope) ItemIndex {
	i.scope = scope
	return i
}

// PlanOwnRefs plans the reconciliation of the item's own references with found, written at `at`,
// as PlanItemReplace plans it under the index's scope - each row with the citation it stands for -
// against the item's own refs rows as the read of its index found them, and reports whether that
// read made them: the read of an indexed ask's index makes them, with each row's kind and
// tuples_stored, in the statement that reads its parts' links (heldItemBlockLinks), so a write of
// the ask's parts alone plans its question's reconciliation after them with no read of its own.
// An index that did not read them - another kind's, an ask's no block-aware build indexed, one
// planned rather than read (ItemIndexing.Held) - answers false, and its caller plans with
// PlanItemReplace, which reads them.
func (i ItemIndex) PlanOwnRefs(found []text.Ref, at time.Time) (Replacement, bool) {
	if !i.ownRead {
		return Replacement{}, false
	}
	return planItemReplaceOver(i.own, i.scope, i.kind, i.itemID, found, at), true
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
// against what the index holds (PlanItemIndex), so the targets it moves and what it states in the
// store are known before anything is written, and written by Queue, which puts its statements on
// a write phase's batch and does nothing else.
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
	// linkNamed is, beside each pair, whether the row its insert writes names its block's node:
	// the block is a node once this index is written and the far end fits the store's bound.
	linkNamed     []bool
	firstIndexing bool
	moved         []Target
	// makers is each block whose maker is not the item's own, as its row holds it once this index
	// is written.
	makers map[string]model.Actor
	// stated is what the index states in the node and tuple store, held for the event of the
	// write that queues it (Stated): the block nodes it writes before their rows (QueueNodes, in
	// Queue), the part_of of each block it places and the links it states or retracts.
	stated graphstore.Uncaused
	// current is the excerpt of each block the body holds, by block id: what a reply naming one
	// of them reads of the index before it is written (Block).
	current map[string]string
	// index is what the item's block index held when this one was planned against it, which Held
	// carries forward into what it leaves.
	index ItemIndex
	// settleForms says the statement that records the item indexed settles what the forms queue
	// holds for the item too (SettlingForms).
	settleForms bool
}

// PlanItemIndex plans the index of body, written at `at`, of comment or message itemID against
// index, what its block index holds (ReadItemIndex): the blocks, their ids, the links their
// mentions write, the excerpt each block stores, cut around the dashboard URLs of the deployment
// at serverURL (BlockExcerpt), the targets they move and what they state in the store. It reads
// nothing, and it is where the excerpts are cut: a writer plans in its read phase, before its
// write phase holds a transaction id. A carried id it refuses returns a *itemblocks.CarriedError.
// An ask's parts are planned from their own reading of its blocks through the same core
// (PlanAskParts, planItemBlocks).
func PlanItemIndex(
	index ItemIndex,
	kind ItemKind,
	itemID, body, serverURL string,
	at time.Time,
	located []text.Located,
	carried *CarriedEdit,
) (ItemIndexing, error) {
	return planItemBlocks(index, kind, itemID, at, itemBodySource(kind, body, serverURL, located, carried))
}

// planItemBlocks plans the index of source, read at `at`, of item itemID against index, what its
// block index holds: the blocks, their ids, the maker each records, the links their mentions
// write, the targets they move and what the index states in the store. It reads nothing and
// writes nothing; what it plans is written by Queue.
//
// What the blocks are in the node and tuple store is planned beside their rows, under the item's
// scope (ItemIndex.scope), as a document's block index plans its own: each block a revision
// writes (new, changed or restored) is a `block` node, live, and so is a block it keeps whose row
// named no node yet, a row an older build wrote, which this index takes over; a block it removes
// whose row named its node is stated gone, and keeps its row's node columns. The nodes are queued
// before the rows that name them, since each row's foreign key is checked in the statement that
// writes it, and a node already there keeps its made_at and made_by. Each block that is a node
// once the revision is written is placed in the item by a part_of, which is stated only where the
// store does not place it there now (storedItemBlocks), so a revision that only moved its
// blocks states nothing; a removed block keeps its part_of, which its node's state and its row's
// lost position each take out of graph_edges. Each link is a tuple out of its block's node,
// asserted with the row the reconciliation inserts and retracted with a row it deletes that named
// its node. An index that changed no block writes no node and states no part_of, so a re-index
// of unchanged text appends nothing but a link a new grammar moved.
//
// Every node is made at `at`, the moment of the write whose text it indexes, and so is every block
// row it inserts, whose created_at the insert writes as `at` rather than leave to the column's
// default. Each part_of and each link is dated by the row it is the projection of: a part_of by
// its block row's created_at, `at` for a row this index inserts and the row's own for one already
// stored, and a link by its row's created_at, `at` or, at a first indexing, the item's own refs
// row's; a retraction by `at`, the moment the link stopped being held. Each is made by the block's
// maker (makerOf).
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
	state, stored, scope := index.state, index.stored, index.scope
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
			excerpt: BlockExcerpt(block.Text, source.serverURL), text: block.Text,
			named: scope.blockNodeOf(kind, itemID, ids[position]),
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

	// makers is each block whose maker is not the item's own, as its row holds it once this index
	// is written: a block it writes records the source's maker, and every other keeps the one its
	// row was written with. A kind whose blocks record no maker holds none, and makes no map.
	own := func(id string, maker *model.Actor) {
		if maker == nil {
			delete(indexing.makers, id)
			return
		}
		if indexing.makers == nil {
			indexing.makers = map[string]model.Actor{}
		}
		indexing.makers[id] = *maker
	}
	for id, row := range index.rows {
		own(id, row.madeBy)
	}
	for _, row := range indexing.written {
		own(row.id, row.madeBy)
	}

	// named is each current block whose row names its node once this index is written, which is
	// what a link it holds is stated under: every block a changed revision writes or keeps that is
	// a node of the store, and otherwise the rows as they were.
	named := make(map[string]bool, len(indexing.written)+len(indexing.kept))
	if indexing.changed {
		nodes := make([]graphstore.Node, 0, len(indexing.written)+len(indexing.kept)+len(indexing.removed))
		for _, row := range indexing.written {
			if row.named {
				nodes = append(nodes, scope.blockNode(itemBlockNodeAddress(kind, itemID, row.id), at, graphstore.StateLive, scope.makerOf(indexing.makers, row.id)))
			}
		}
		for _, row := range indexing.kept {
			if row.named && !index.rows[row.id].named {
				nodes = append(nodes, scope.blockNode(itemBlockNodeAddress(kind, itemID, row.id), at, graphstore.StateLive, scope.makerOf(indexing.makers, row.id)))
			}
		}
		for _, id := range indexing.removed {
			if index.rows[id].named && scope.blockNodeOf(kind, itemID, id) {
				nodes = append(nodes, scope.blockNode(itemBlockNodeAddress(kind, itemID, id), at, graphstore.StateGone, scope.makerOf(indexing.makers, id)))
			}
		}
		if len(nodes) > 0 {
			indexing.stated = graphstore.PlanUncaused(nodes, nil)
		}
		// Every current block is one the revision wrote or kept, so the blocks it places are the
		// current ones that are nodes, in body order, each the store does not place there now.
		for _, id := range ids {
			if !scope.blockNodeOf(kind, itemID, id) {
				continue
			}
			named[id] = true
			row, stored := index.rows[id]
			if stored && row.placed {
				continue
			}
			placedAt := at
			if stored {
				placedAt = row.createdAt
			}
			indexing.stated.Tuples = append(indexing.stated.Tuples,
				parentTuple(itemBlockNodeAddress(kind, itemID, id), string(kind), itemID, placedAt, scope.makerOf(indexing.makers, id), true))
		}
	} else {
		for _, row := range indexing.kept {
			named[row.id] = index.rows[row.id].named
		}
	}

	// The links the body drops, in the order the index listed them, then those it adds, in the
	// order the insert writes them: the targets the delete and the insert move, and the tuples the
	// rows each stand for.
	indexing.pairs = source.pairs(next, ids)
	indexing.linkNamed = make([]bool, len(indexing.pairs))
	// An item's links carry no block types, so a pair is its key (blockPairKey) and no link is
	// kept while its types change, as a document's typed block attribute can be.
	written := make(map[blockPairKey]struct{}, len(indexing.pairs))
	for position, pair := range indexing.pairs {
		written[pair.key()] = struct{}{}
		indexing.linkNamed[position] = named[pair.blockID] && graphstore.AddressFits(pair.target.ID)
	}
	stillHeld := make(map[blockPairKey]struct{}, len(index.held))
	for _, link := range index.held {
		pair := blockPairKey{blockID: link.blockID, kind: link.kind, target: link.target}
		stillHeld[pair] = struct{}{}
		if _, still := written[pair]; !still {
			indexing.moved = append(indexing.moved, pair.target)
			if link.named && !indexing.firstIndexing {
				indexing.stated.Tuples = append(indexing.stated.Tuples, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
					FromKind: BlockNodeKind, FromID: itemBlockNodeAddress(kind, itemID, link.blockID), Kind: link.kind,
					ToKind: link.target.Kind, ToID: link.target.ID,
					At: at, Who: scope.makerOf(indexing.makers, link.blockID), Asserted: false,
				}})
			}
		}
	}
	for position, pair := range indexing.pairs {
		if _, already := stillHeld[pair.key()]; already {
			continue
		}
		indexing.moved = append(indexing.moved, pair.target)
		if !indexing.linkNamed[position] {
			continue
		}
		createdAt, sourceSeq := at, (*int64)(nil)
		if indexing.firstIndexing {
			if row, cited := index.firstRefs[pair.target]; cited {
				createdAt, sourceSeq = row.createdAt, row.sourceSeq
			}
		}
		indexing.stated.Tuples = append(indexing.stated.Tuples, graphstore.UncausedTuple{
			Tuple: graphstore.Tuple{
				FromKind: BlockNodeKind, FromID: itemBlockNodeAddress(kind, itemID, pair.blockID), Kind: pair.kind,
				ToKind: pair.target.Kind, ToID: pair.target.ID,
				At: createdAt, Who: scope.makerOf(indexing.makers, pair.blockID), LegacySourceSeq: sourceSeq, Asserted: true,
			},
			LegacyFromCause: true,
		})
	}
	// A link a block keeps shows that block's excerpt in its target's referenced-by panel, so a
	// block that keeps its id while its excerpt changes names the targets of the links it holds,
	// as a document's reworded block does; a change past the excerpt's runes names none.
	newExcerpts := make(map[string]string, len(indexing.written))
	for _, row := range indexing.written {
		newExcerpts[row.id] = row.excerpt
	}
	for _, pair := range indexing.pairs {
		if previous, held := index.rows[pair.blockID]; held && previous.current {
			if excerpt, rewritten := newExcerpts[pair.blockID]; rewritten && excerpt != previous.excerpt {
				indexing.moved = append(indexing.moved, pair.target)
			}
		}
	}
	// The identities the text names, a mention of each out of the item's node where the store
	// holds none from it already (identityMentions), and the retraction of each mention the store
	// holds from it that the text no longer names and no delivery route of a comment names
	// (unnamedMentions): a session's and a role's with the node's other facts, a person's through
	// the store's one writer of a fact that names a person. An item no block-aware build indexed is
	// read with the mentions the backfill stated of its text and, a comment, its delivery routes
	// (ReadItemIndex), so its first index retracts as a later one does.
	if scope.held {
		tuples, people := identityMentions(string(kind), itemID, scope.node.Visibility, scope.node.MadeBy, at, source.identities, index.mentioned)
		unnamed, unnamedPeople := unnamedMentions(string(kind), itemID, scope.node.Visibility, scope.node.MadeBy, at, source.identities, index.mentioned, index.routes)
		indexing.stated.Tuples = append(indexing.stated.Tuples, slices.Concat(tuples, unnamed)...)
		indexing.stated.People = append(indexing.stated.People, slices.Concat(people, unnamedPeople)...)
	}
	return indexing, nil
}

// Moved is the targets of the block mentions the index inserts or deletes, and of those it keeps
// in a block whose excerpt it changes.
func (i ItemIndexing) Moved() []Target {
	return i.moved
}

// Stated is what the index states in the node and tuple store, held for the event of the write
// that queues it: the block nodes Queue writes before their rows, and the base attributes that
// write states of them, the part_of of each block it places and the links it states or retracts,
// the mention of each identity its text newly names (identityMentions), and the retraction of each
// identity mention its text no longer names (unnamedMentions).
// A write that appends an event queues it with graphstore.QueueCaused right after that event, in
// the same write phase; a pass no event records queues it at graphstore.NoEvent.
func (i ItemIndexing) Stated() graphstore.Uncaused {
	return i.stated
}

// Held is what the item's block index holds once this index is written, which a second index of
// the same item in one transaction plans against, so it sees the rows the first one queued
// (PlanAskParts). It reads the plan alone: the state this index records, the blocks the source
// holds in its order followed by the removed ones, the ones this index removes first, as a read
// after the write orders them (storedItemBlocks), what the index reads of each row, the links its
// blocks hold once the reconciliation has run, the item's scope, the identities the store holds a
// mention of from the item once this index's mentions and retractions are appended, and those its
// delivery routes name. It holds none of the item's own refs rows, which the plan does not read
// (PlanOwnRefs answers false).
func (i ItemIndexing) Held() ItemIndex {
	held := ItemIndex{
		kind: i.kind, itemID: i.itemID,
		state: itemState{
			indexed: true, revision: i.revision, nextBlock: i.nextBlock,
			bodySHA256: i.bodySHA256, edited: i.index.state.edited,
		},
		stored:    make([]itemblocks.Stored, 0, len(i.index.stored)+len(i.written)),
		rows:      make(map[string]storedItemBlock, len(i.index.rows)+len(i.written)),
		held:      make([]itemBlockLink, 0, len(i.pairs)),
		scope:     i.index.scope,
		mentioned: make(map[Target]struct{}, len(i.index.mentioned)+len(i.stated.People)),
		routes:    i.index.routes,
	}
	maps.Copy(held.mentioned, i.index.mentioned)
	held.mentioned = mentionsAfter(held.mentioned, slices.Concat(i.stated.People, statedMentions(i.stated.Tuples)))
	maps.Copy(held.rows, i.index.rows)
	for _, rows := range [][]itemBlockRow{i.written, i.kept} {
		for _, row := range rows {
			held.stored = append(held.stored, itemblocks.Stored{
				ID: row.id, Type: row.nodeType, Hash: row.sum, Position: row.position,
			})
			previous, stored := i.index.rows[row.id]
			kept := storedItemBlock{current: true, excerpt: row.excerpt, createdAt: i.at, named: previous.named, placed: previous.placed}
			if stored {
				kept.createdAt = previous.createdAt
			}
			if i.changed {
				kept.named = row.named
				kept.placed = row.named
			}
			if maker, own := i.makers[row.id]; own {
				kept.madeBy = &maker
			}
			held.rows[row.id] = kept
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
		row := held.rows[block.ID]
		row.current = false
		held.rows[block.ID] = row
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
	before := make(map[blockPairKey]bool, len(i.index.held))
	for _, link := range i.index.held {
		before[blockPairKey{blockID: link.blockID, kind: link.kind, target: link.target}] = link.named
	}
	for position, pair := range i.pairs {
		named, kept := before[pair.key()]
		if !kept {
			named = i.linkNamed[position]
		}
		held.held = append(held.held, itemBlockLink{blockID: pair.blockID, kind: pair.kind, target: pair.target, named: named})
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
	return itemBlockNodeAddress(i.kind, i.itemID, blockID), excerpt, true
}

// SettlingForms is the index settling what the forms queues hold for the item (RefsFormsOwed,
// RefsDecisionsFormsOwed) in the statement that records it indexed (queueRecordIndexed) and the
// one after it, so the settle costs the write one statement beyond its own. A writer asks for it
// where this index is the last of the writes that index the item's whole text, queued after the
// reconciliation of the item's own references: an ask's parts after its question's
// (Replacement.SettlingForms is the other order). That statement is the index's last, so the
// settle follows every delete of its block edges and the references' delete before it, each of
// which owes the first queue the item (migrations 20261117025906-20261117025910).
func (i ItemIndexing) SettlingForms() ItemIndexing {
	i.settleForms = true
	return i
}

// Queue queues the index's writes on a write phase's batch: the item's state row taken first,
// the block nodes the index states (graphstore.QueueNodes), before the blocks the body changed
// and removed whose rows name them, the block texts of a kind that records them, the links its
// blocks hold, and last the statement that records the item indexed (queueRecordIndexed). What
// the index states of those nodes, their part_of and the links waits for the write's event
// (Stated).
func (i ItemIndexing) Queue(b *store.WriteBatch) error {
	i.queueStateLock(b)
	if i.changed {
		graphstore.QueueNodes(b, i.stated)
		if err := i.queueBlocks(b); err != nil {
			return err
		}
	}
	i.queueBlockRefs(b)
	i.queueRecordIndexed(b)
	return nil
}

// queueRecordIndexed queues the statement that records the item indexed from the text whose sum
// is bodySHA256, at the revision with nextBlock as the next block number for a kind that counts
// revisions, clears the block index's marker and leaves the window arm's standing. With
// settleForms it also deletes the item's row of refs_forms_owed, in the same statement
// (SettlingForms): the statement is the index's last, so the delete follows every delete of its
// block edges. The item's row of refs_decisions_forms_owed, which that delete relays
// (migration 20261126040100) at the end of its statement, and its row of the bootstrap queue
// beside it are deleted by a statement after it (settleDecisionsFormsStatement), never by the one
// that relays it.
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
	statement := `
		update ` + i.tables.state + ` set body_sha256 = $2, ` + owed + `
		where ` + i.tables.item + ` = $1::uuid
	`
	args := []any{i.itemID, i.bodySHA256}
	if i.tables.revisions() {
		statement = `
			update ` + i.tables.state + ` set revision = $2, next_block = $3, body_sha256 = $4, ` + owed + `
			where ` + i.tables.item + ` = $1::uuid
		`
		args = []any{i.itemID, i.revision, i.nextBlock, i.bodySHA256}
	}
	if i.settleForms {
		args = append(args, string(i.kind))
		statement = fmt.Sprintf(`with settled as (delete from refs_forms_owed where kind = $%d and id = $1::uuid) `, len(args)) + statement
	}
	b.Queue(what, statement, args...)
	if i.settleForms {
		b.Queue(fmt.Sprintf("settle the decisions index owed by %s %s", i.kind, i.itemID),
			settleDecisionsFormsStatement, string(i.kind), i.itemID)
	}
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
// indexed from, and whether the item was edited (edited_at set, which only the state read of a
// kind that counts revisions reads: a message carries edited_at too, and this read leaves it).
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
// transaction has queued, read as never indexed; exists is false for the last, whose row q does
// not hold.
func readItemState(ctx context.Context, q Queryer, kind ItemKind, tables itemTables, itemID string) (itemState, ItemScope, bool, error) {
	state := itemState{nextBlock: 1}
	var sum *string
	var node graphstore.HeldNode
	// The item's node (ReadItemScope), read in the same statement off the key it is read by.
	held := `
		left join graph_nodes n on n.kind = $2 and n.id = $3`
	var err error
	if tables.revisions() {
		err = q.QueryRow(ctx, `
			select coalesce(s.revision, 0), coalesce(s.next_block, 1), s.body_sha256,
			       i.edited_at is not null, `+graphstore.HeldNodeColumns("n")+`
			from `+tables.items+` i left join `+tables.state+` s on s.`+tables.item+` = i.id`+held+`
			where i.id = $1::uuid
		`, itemID, string(kind), itemID).Scan(append([]any{&state.revision, &state.nextBlock, &sum, &state.edited}, node.Targets()...)...)
		state.indexed = state.revision > 0
	} else {
		err = q.QueryRow(ctx, `
			select s.body_sha256, `+graphstore.HeldNodeColumns("n")+`
			from `+tables.items+` i left join `+tables.state+` s on s.`+tables.item+` = i.id`+held+`
			where i.id = $1::uuid
		`, itemID, string(kind), itemID).Scan(append([]any{&sum}, node.Targets()...)...)
		state.indexed = sum != nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return itemState{nextBlock: 1}, ItemScope{}, false, nil
	}
	if err != nil {
		return itemState{}, ItemScope{}, false, fmt.Errorf("read the block state of %s %s: %w", kind, itemID, err)
	}
	if sum != nil {
		state.bodySHA256 = *sum
	}
	scope := ItemScope{}
	if scope.node, scope.held, err = node.Node(); err != nil {
		return itemState{}, ItemScope{}, false, fmt.Errorf("read the node of %s %s: %w", kind, itemID, err)
	}
	return state, scope, true, nil
}

// heldItemBlockLinks is the block mentions the item's blocks hold, read with the predicate the
// reconciling delete visits them by, so the index knows what it moves before it writes, and
// whether each row names its block's node, which a retraction of the link it stands for needs.
// For an ask the same statement reads the ask's own refs rows too, with the predicate HeldTargets
// reads them by, each with its kind and tuples_stored (own, read true): a write of an ask's parts
// alone reconciles its question's references after them (PlanAskParts), and plans that, with the
// citations those rows stand for, against these rows rather than a read of its own
// (ItemIndex.PlanOwnRefs). A row of the ask's own carries no block id, which tells the two apart,
// and its last column is its tuples_stored where a link's is whether it names its block's node.
// For a comment or a message the same statement reads the identities the store holds a mention
// of out of the item's node now (mentioned, IdentityMentionsRead), each a row with no block id
// whose far end is an identity's kind, which no refs row names (TargetOf): what the index appends
// no mention of again, and retracts the mention of where the text no longer names it. For a
// comment it reads its delivery routes too (routes, routeRows, CommentRoutesRead): the session or
// the role each of its comment_mentions rows targets, and the session that row resolved to, each a
// mention the comment's write states apart from its text (api's commentMentionFacts), which no edit
// retracts.
func heldItemBlockLinks(ctx context.Context, q Queryer, kind ItemKind, tables itemTables, itemID string) (held []itemBlockLink, own []heldCitation, read bool, mentioned, routes map[Target]struct{}, err error) {
	query, args := `
		select block_id, kind, to_kind, to_id, node_kind is not null from `+tables.refs+` where `+tables.item+` = $1::uuid
		union all
		select null, '`+graphstore.TupleMentions+`', mentioned.to_kind, mentioned.to_id, true from (
			`+IdentityMentionsRead(`$2`, `$3`, `$4`)+`
		) mentioned
	`, []any{itemID, string(kind), itemID, IdentityKinds}
	switch kind {
	case ItemComment:
		query += `
		union all
		select null, '` + routeRows + `', route.to_kind, route.to_id, true from (` + CommentRoutesRead(`$1::uuid`) + `) route`
	case ItemAsk:
		query = `
			select block_id, kind, to_kind, to_id, node_kind is not null from ask_block_refs where ask_id = $1::uuid
			union all
			select null, kind, to_kind, to_id, tuples_stored from refs where from_kind = 'ask' and from_id = $2 and to_kind = any($3)
		`
		args, read = []any{itemID, itemID, nodeKinds}, true
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, false, nil, nil, fmt.Errorf("read the block mentions of %s %s: %w", kind, itemID, err)
	}
	defer rows.Close()
	held, own, mentioned, routes = []itemBlockLink{}, []heldCitation{}, map[Target]struct{}{}, map[Target]struct{}{}
	for rows.Next() {
		var blockID *string
		var link itemBlockLink
		if err := rows.Scan(&blockID, &link.kind, &link.target.Kind, &link.target.ID, &link.named); err != nil {
			return nil, nil, false, nil, nil, fmt.Errorf("scan a block mention of %s %s: %w", kind, itemID, err)
		}
		switch {
		case blockID == nil && kind == ItemComment && link.kind == routeRows:
			routes[link.target] = struct{}{}
		case blockID == nil && kind != ItemAsk:
			mentioned[link.target] = struct{}{}
		case blockID == nil:
			own = append(own, heldCitation{target: link.target, kind: link.kind, stored: link.named})
		default:
			link.blockID = *blockID
			held = append(held, link)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, nil, nil, fmt.Errorf("read the block mentions of %s %s: %w", kind, itemID, err)
	}
	return held, own, read, mentioned, routes, nil
}

// routeRows is the kind heldItemBlockLinks' statement gives the rows it reads off a comment's
// comment_mentions, the table's own name, which no tuple kind and no block link kind is, so the
// scan tells a delivery route from a mention.
const routeRows = "comment_mentions"

// itemRefRows is the item's own refs rows by target, each with its created_at and source_seq,
// which date the tuples an item's first indexing states of the links it inserts (planItemBlocks).
func itemRefRows(ctx context.Context, q Queryer, kind ItemKind, itemID string) (map[Target]itemRefRow, error) {
	rows, err := q.Query(ctx, `
		select to_kind, to_id, created_at, source_seq from refs where from_kind = $1 and from_id = $2
	`, string(kind), itemID)
	if err != nil {
		return nil, fmt.Errorf("read the references of %s %s: %w", kind, itemID, err)
	}
	defer rows.Close()
	cited := map[Target]itemRefRow{}
	for rows.Next() {
		var target Target
		var row itemRefRow
		if err := rows.Scan(&target.Kind, &target.ID, &row.createdAt, &row.sourceSeq); err != nil {
			return nil, fmt.Errorf("scan a reference of %s %s: %w", kind, itemID, err)
		}
		cited[target] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the references of %s %s: %w", kind, itemID, err)
	}
	return cited, nil
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
// in place and, among removed blocks of equal text, the last removed; and beside them, by id, what
// else the index reads of each row (storedItemBlock).
//
// A current row whose node the store holds gone reads as naming none. Neither arm of graph_edges
// answers such a row - the old one reads only rows that name no node, and the store's only live
// nodes - so it is the index's to take over as it takes over an older build's row: the next
// revision this release writes states the node live again and names it (planItemBlocks). It is
// what a restore of a removed block leaves where nothing unmarks the row: the release before this
// one restores a block by an upsert that moves its position and leaves its node columns as they
// were, and only migration 20261116150000's trigger has that update name no node, so whatever
// write leaves a row current under a gone node, this read keeps it from being answered by neither
// arm for good.
func storedItemBlocks(ctx context.Context, q Queryer, kind ItemKind, tables itemTables, itemID string, scoped bool) ([]itemblocks.Stored, map[string]storedItemBlock, error) {
	// Only a kind whose blocks record a maker has the column; kind is this package's own
	// constant, never caller input.
	maker := "null::jsonb"
	if tables.makers {
		maker = "b.made_by"
	}
	// For an item whose node the store holds (scoped), where the store places each row's block
	// node now (storedItemBlock.placed): the newest part_of tuple from the block's node into the
	// item, read the way the store reads a current relation (graphstore.CurrentTupleRead), with
	// both ends of the relation and its kind bound, one probe of the block's address per row.
	rows, err := q.Query(ctx, `
		select b.block_id, b.type, b.text_sha256, coalesce(b.position, -1), b.excerpt, b.created_at,
		       b.node_kind is not null and not (b.position is not null and n.state = $2), `+maker+`,
		       $3::boolean and coalesce((
		           `+graphstore.CurrentTupleRead(`t.from_id`, `t.asserted`,
		`t.to_kind = $4 and t.to_id = $5 and t.kind = $6 and t.from_kind = $7 and t.from_id = $8 || b.block_id`)+`
		       ), false)
		from `+tables.blocks+` b
		left join graph_nodes n on n.kind = b.node_kind and n.id = b.node_address
		where b.`+tables.item+` = $1::uuid
		order by b.position nulls last, b.removed_at desc, length(b.block_id), b.block_id
	`, itemID, graphstore.StateGone, scoped, string(kind), itemID, graphstore.TuplePartOf, BlockNodeKind,
		itemBlockNodeAddress(kind, itemID, ""))
	if err != nil {
		return nil, nil, fmt.Errorf("read the blocks of %s %s: %w", kind, itemID, err)
	}
	defer rows.Close()
	stored := []itemblocks.Stored{}
	held := map[string]storedItemBlock{}
	for rows.Next() {
		var block itemblocks.Stored
		var row storedItemBlock
		var madeBy []byte
		if err := rows.Scan(&block.ID, &block.Type, &block.Hash, &block.Position, &row.excerpt, &row.createdAt, &row.named, &madeBy, &row.placed); err != nil {
			return nil, nil, fmt.Errorf("scan a block of %s %s: %w", kind, itemID, err)
		}
		if madeBy != nil {
			row.madeBy = &model.Actor{}
			if err := json.Unmarshal(madeBy, row.madeBy); err != nil {
				return nil, nil, fmt.Errorf("decode the maker of block %s of %s %s: %w", block.ID, kind, itemID, err)
			}
		}
		row.current = block.Position >= 0
		stored = append(stored, block)
		held[block.ID] = row
	}
	return stored, held, rows.Err()
}

// storedItemBlock is what an index reads of one block row beside what itemblocks aligns on:
// whether the block is current, its stored excerpt, which a link it holds shows, when its row
// was first written, which is the moment its part_of is dated by, as the old arm of graph_edges
// dates it, whether the row names a node the store answers it by, which an older build's row
// does not (storedItemBlocks), the maker it records, nil where the item's own author made it, and
// whether the store places its node in the item now, which an item the store holds no node of reads
// as false (storedItemBlocks).
type storedItemBlock struct {
	current   bool
	excerpt   string
	createdAt time.Time
	named     bool
	madeBy    *model.Actor
	placed    bool
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
// excerpt, text and, for a kind whose blocks record one, its maker (nil: the item's own author
// made it), which its made_by column holds as json, and whether the block is a node of the store
// (ItemScope.blockNodeOf), which its row then names.
type itemBlockRow struct {
	id       string
	position int
	nodeType string
	sum      string
	excerpt  string
	text     string
	madeBy   *model.Actor
	named    bool
}

// queueBlocks queues one new revision of the item's blocks: written (new, changed or restored
// blocks) take the revision and `at` as their changed facts, and a new one madeAt and `at` as its
// created_at, the moment its part_of is dated by (planItemBlocks), which the insert writes rather
// than leave to the column's default, the transaction's now(); a written row already stored keeps
// its created_at. Kept blocks move their position, type and excerpt; removed ones lose their
// position at `at`. A kind with revisions records each written block's text, and null for each
// removed one, at the revision. A kind whose blocks record a maker writes each written block's; a
// kept block keeps the maker it was written with, as it keeps its text.
//
// Each written and kept row names its node in the statement that writes it where the block is a
// node (itemBlockRow.named), and names none where it is not, so the row says which arm of
// graph_edges answers it (migration 20261126045003); the node is already written, queued before
// these statements (Queue), since the foreign key is checked inside the statement. A removed row
// keeps the node columns it had.
func (i ItemIndexing) queueBlocks(b *store.WriteBatch) error {
	kind, tables, itemID, revision := i.kind, i.tables, i.itemID, i.revision
	if len(i.removed) > 0 {
		b.Queue(fmt.Sprintf("remove the blocks %s %s no longer holds", kind, itemID), `
			update `+tables.blocks+` set position = null, removed_at = $2
			where `+tables.item+` = $1::uuid and block_id = any($3)
		`, itemID, i.at, i.removed)
	}
	if len(i.written) > 0 {
		ids, positions, types, sums, excerpts, named := itemBlockColumns(i.written)
		what := fmt.Sprintf("record the blocks of %s %s", kind, itemID)
		if tables.revisions() {
			// Only a kind whose blocks record a maker has the column, so the three fragments are
			// empty for the others; kind is this package's own constant, never caller input.
			maker, makerValue, makerUpdate := "", "", ""
			if tables.makers {
				maker, makerValue, makerUpdate = ", made_by", ", b.made_by::jsonb", ", made_by = excluded.made_by"
			}
			makers, err := itemBlockMakers(kind, itemID, i.written)
			if err != nil {
				return err
			}
			b.Queue(what, `
				insert into `+tables.blocks+` (`+tables.item+`, block_id, node_id, node_kind, node_address, position, type, text_sha256, excerpt, made_at, changed_revision, changed_at, created_at`+maker+`)
				select $1::uuid, b.id, '`+string(kind)+`/' || $1::uuid::text || '#' || b.id,
				       case when b.named then $12 end, case when b.named then '`+string(kind)+`/' || $1::uuid::text || '#' || b.id end,
				       b.position, b.type, b.sum, b.excerpt, $2, $3, $4, $4`+makerValue+`
				from unnest($5::text[], $6::int[], $7::text[], $8::text[], $9::text[], $10::text[], $11::boolean[]) as b (id, position, type, sum, excerpt, made_by, named)
				on conflict (`+tables.item+`, block_id) do update set
					node_kind = excluded.node_kind,
					node_address = excluded.node_address,
					position = excluded.position,
					type = excluded.type,
					text_sha256 = excluded.text_sha256,
					excerpt = excluded.excerpt,
					changed_revision = excluded.changed_revision,
					changed_at = excluded.changed_at,
					removed_at = null`+makerUpdate+`
			`, itemID, i.madeAt, revision, i.at, ids, positions, types, sums, excerpts, makers, named, BlockNodeKind)
		} else {
			// A message records no revisions, so a block it writes takes the time of the write
			// as its made_at and its changed_at: the moment the message was sent, or the edit
			// that added it (PATCH /api/v1/messages/{id}, indexed at edited_at). A block an edit
			// rewrote keeps the made_at it was written with and takes the edit as its changed_at;
			// a reindex under another parser moves only those of new text. A message stored
			// before blocks existed and edited before its first index was never indexed at the
			// moment it was sent, so every block of that first index is made at the edit, the
			// one time Dispatch holds for its text, where a comment's are made at none (null).
			b.Queue(what, `
				insert into message_blocks (message_id, block_id, node_id, node_kind, node_address, position, type, text_sha256, excerpt, made_at, changed_at, created_at)
				select $1::uuid, b.id, 'message/' || $1::uuid::text || '#' || b.id,
				       case when b.named then $9 end, case when b.named then 'message/' || $1::uuid::text || '#' || b.id end,
				       b.position, b.type, b.sum, b.excerpt, $2, $2, $2
				from unnest($3::text[], $4::int[], $5::text[], $6::text[], $7::text[], $8::boolean[]) as b (id, position, type, sum, excerpt, named)
				on conflict (message_id, block_id) do update set
					node_kind = excluded.node_kind,
					node_address = excluded.node_address,
					position = excluded.position,
					type = excluded.type,
					text_sha256 = excluded.text_sha256,
					excerpt = excluded.excerpt,
					changed_at = excluded.changed_at,
					removed_at = null
			`, itemID, i.at, ids, positions, types, sums, excerpts, named, BlockNodeKind)
		}
	}
	if len(i.kept) > 0 {
		ids, positions, types, _, excerpts, named := itemBlockColumns(i.kept)
		b.Queue(fmt.Sprintf("move the unchanged blocks of %s %s", kind, itemID), `
			update `+tables.blocks+` b set position = k.position, type = k.type, excerpt = k.excerpt,
				node_kind = case when k.named then $7 end, node_address = case when k.named then b.node_id end
			from unnest($2::text[], $3::int[], $4::text[], $5::text[], $6::boolean[]) as k (id, position, type, excerpt, named)
			where b.`+tables.item+` = $1::uuid and b.block_id = k.id
		`, itemID, ids, positions, types, excerpts, named, BlockNodeKind)
	}
	if !tables.revisions() || len(i.written)+len(i.removed) == 0 {
		return nil
	}
	textIDs := make([]string, 0, len(i.written)+len(i.removed))
	texts := make([]*string, 0, len(i.written)+len(i.removed))
	for index := range i.written {
		textIDs, texts = append(textIDs, i.written[index].id), append(texts, &i.written[index].text)
	}
	for _, id := range i.removed {
		textIDs, texts = append(textIDs, id), append(texts, nil)
	}
	// A comment's text rows leave transaction_started_at to its default, the start of this
	// transaction, which the keyword fill compares with a comment.created event's created_at to
	// tell revision 1 the comment's own transaction wrote (textversions' firstRevisionBody); the
	// update of a text on a conflict keeps it.
	b.Queue(fmt.Sprintf("record the block texts of revision %d of %s %s", revision, kind, itemID), `
		insert into `+tables.texts+` (`+tables.item+`, block_id, revision, text)
		select $1::uuid, t.id, $2, t.text from unnest($3::text[], $4::text[]) as t (id, text)
		on conflict (`+tables.item+`, block_id, revision) do update set text = excluded.text
	`, itemID, revision, textIDs, texts)
	return nil
}

// itemBlockColumns is rows as the parallel arrays an unnest reads.
func itemBlockColumns(rows []itemBlockRow) (ids []string, positions []int32, types, sums, excerpts []string, named []bool) {
	for _, row := range rows {
		ids = append(ids, row.id)
		positions = append(positions, int32(row.position))
		types = append(types, row.nodeType)
		sums = append(sums, row.sum)
		excerpts = append(excerpts, row.excerpt)
		named = append(named, row.named)
	}
	return ids, positions, types, sums, excerpts, named
}

// itemBlockMakers is each row's maker as the json its made_by column holds, nil where the item's
// own author made it, which is every row of a kind whose blocks record no maker.
func itemBlockMakers(kind ItemKind, itemID string, rows []itemBlockRow) ([]*string, error) {
	makers := make([]*string, len(rows))
	for index, row := range rows {
		if row.madeBy == nil {
			continue
		}
		encoded, err := json.Marshal(row.madeBy)
		if err != nil {
			return nil, fmt.Errorf("encode the maker of block %s of %s %s: %w", row.id, kind, itemID, err)
		}
		maker := string(encoded)
		makers[index] = &maker
	}
	return makers, nil
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
// mention, so nothing is deleted. The targets the two statements move and the tuples their rows
// stand for are already known (PlanItemIndex), so neither returns its rows. A row names its
// block's node in its own insert where the block's row names it once this index is written and
// the far end fits the store's bound (linkNamed), and names none otherwise, which is the row its
// old arm answers; the node columns are the insert's alone, so a pair an older build wrote keeps
// the nulls it was written with and its old arm keeps answering it.
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
		insert into `+tables.refs+` (`+tables.item+`, block_id, from_node, node_kind, node_address, to_kind, to_id, kind, created_at, source_seq)
		select $1::uuid, p.block_id, $6::text || '/' || $1::uuid::text || '#' || p.block_id,
		       case when p.stated then $10 end, case when p.stated then $6::text || '/' || $1::uuid::text || '#' || p.block_id end,
		       p.to_kind, p.to_id, p.kind,
		       coalesce(case when $8::boolean then r.created_at end, $7::timestamptz), case when $8::boolean then r.source_seq end
		from unnest($2::text[], $3::text[], $4::text[], $5::text[], $9::boolean[]) as p (block_id, kind, to_kind, to_id, stated)
		left join refs r on r.from_kind = $6 and r.from_id = $1::uuid::text
		                and r.to_kind = p.to_kind and r.to_id = p.to_id
		on conflict (`+tables.item+`, block_id, to_kind, to_id, kind) do nothing
	`, itemID, blockIDs, kinds, toKinds, toIDs, string(kind), i.at, i.firstIndexing, i.linkNamed, BlockNodeKind)
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
// such lock and needs none, since nothing ever rewrites a direct message's body: only an issue
// message is edited or deleted (PATCH and DELETE /api/v1/messages/{id}). It takes no item row: a
// row lock would assign the transaction a transaction id to hold through everything it reads and
// plans, and what it would exclude beyond the owner lock is a writer of the item row that takes
// no owner lock, which no writer a server runs is: api's lock table holds behind a held owner
// lock the serving writers of these rows that open their own transaction
// (TestEveryWriterOnTheLockTableWaitsForTheOwnerLockOfTheRowItWrites), those that write inside a
// transaction their caller opened rest on the lock that caller took, which that table does not
// drive, and the writers that take none are named with why they take none (LockItemOwner). Which
// writer is which is kept true by TestTheLockTablePlacesEveryWriterTheCensusNames.
// Reading and planning come first and its writes follow back to back (IndexItemIfOwed). A new
// block and a new mention take the time the item's text was written, read the same way for all
// three kinds: the row's coalesce(edited_at, created_at), a message's edited_at among them since
// PATCH /api/v1/messages/{id}. An item a write site indexed since it was listed is no longer owed
// and is left alone; one deleted since, before the lock or while it waited, took its state row
// with it (on delete cascade), so nothing is owed.
//
// It runs from the block-index reconciliation (docs.(*Service).ReconcileBlockIndex) and indexes
// through IndexItemIfOwed, which indexes nothing unless the item's index is owed, so every
// mention it records is of text an earlier write already appended an event for. A block mention
// takes the source_seq of the item's own refs row for its target at the item's first indexing
// (PlanItemIndex) and none otherwise, and no ?since= read lists a row with none until a later
// write of the item stamps it (Stamp). Its index is the item's whole text - the blocks, then the
// item's own references, an ask's question's after its parts - so it settles what the forms queue
// holds for the item after every delete it made (RefsFormsOwed), which rebuild-refs leaves owed
// until this index runs.
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

// LockItemOwner takes the lock of the owner of comment, message or ask itemID - its issue, or
// the document a comment or an ask on a document hangs on - which every writer of the item
// takes, and reports whether the item is still there: one deleted since it was listed took its
// state row with it (on delete cascade), so nothing is owed of it. A direct message, whose
// conversation is no owner, has none and needs none, since nothing rewrites a direct message's
// body: only an issue message is edited or deleted.
//
// It is the lock every writer of a comment, a message, an ask or one of their block-state rows
// takes before it writes that row, inside and outside this package: the window arm here
// (WriteItemWindows), the serving routes through their own helpers (api's lockedComment and
// lockCommentOwner, docs' lockArtifactOwner), and the anchor-block backfill, which takes it
// here, one row at a time (docs.backfillAnchorBlock). A writer that reads what it writes reads
// it in a statement after this one, never in the same one.
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
// item's owner lock first - and api holds that invariant in two halves, names and locks.
//
// The half that holds a writer behind this lock is the lock table
// (TestEveryWriterOnTheLockTableWaitsForTheOwnerLockOfTheRowItWrites,
// item_row_owner_lock_test.go): it takes an item's owner lock in a transaction of its own and
// requires each serving writer of these rows that opens its own transaction to queue behind it
// with nothing written yet, so one of them that stops taking its lock reds there. The writers
// that write inside a transaction their caller opened - docs' helpers that a version write, a
// settlement or a route calls, this package's block-index statements, the drain's settle and the
// stamp - take no lock of their own: they rest on the lock that caller took, which that table
// does not drive, and its gap map (itemRowLockNoRow, item_writer_census_test.go) names each with
// why it has no row. TestTheLockTablePlacesEveryWriterTheCensusNames holds every census name to
// one of the two.
//
// The other half is a census of names (TestEveryWriterOfAnItemRowIsOnTheOwnerLockCensus): it
// finds every write of those rows in the module and reds unless the function making it is on a
// list, so a `delete from comments` added tomorrow (dispatch://INFRA-1523) reds there before it
// ships. It reads names and no lock - the lock named beside an entry is prose, and nothing
// compares it with the code - so a writer that stopped taking its lock is still named and the
// census stays green for it, which is what the table above is for.
// TestTheLockTablePlacesEveryWriterTheCensusNames keeps the two lists one: every name on the
// census is a row of that table, or is in that table's gap map with why it has none.
//
// Fifteen of the census's names take no owner lock, in seven exceptions, and cost this one
// nothing; the lock table's gap map names the same fifteen. cmd/dispatch-seed's three item loaders
// fill a database before any server is started on it; api's queueBroadcastCopy writes direct
// messages, whose conversation is no owner (store.OwnerOf); docs' restoreWordings runs once at
// boot under the copy locks alone (RestoreDecidedWordings) and refuses its restoration when the
// rows moved under it; RebuildAll raises the owed markers of every comment, message and ask so
// the index re-derives them, and writes no item row and no other column of a state row; store's
// collisions, nameWrite and urlWrite build the statements of the boot rename pass
// (RewriteRenamedRepositories), whose tables hold stored repository fields and no item row;
// graphcensus' writeAttempts and writeMarks, two boot passes of the backfill, write delivery
// attempts and a person's read state, tables no item owns; and the migration runners' four
// writers of their version tables write no item row. Every other writer holds the owner this
// takes: those the lock table drives, the backfill among them, by taking it themselves, and those
// its gap map names as running inside a caller's transaction, through the lock that caller took.
//
// A new writer of one of those rows takes this lock before it reads what it writes:
// store.LockOwner on store.OwnerOf(the row's issue key, its artifact id), as this function,
// api's lockedComment and lockCommentOwner and docs' lockArtifactOwner each take it, and then
// reads in the statement after it. Then it goes on the census with the lock it took, and on the
// lock table that holds it behind that lock - or in that table's gap map, with why it has no row.
func LockItemOwner(ctx context.Context, tx pgx.Tx, kind ItemKind, itemID string) (bool, error) {
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
// else, in the caller's transaction: it is to an item what WriteDocumentWindows is to a
// document. Two callers run it: the block-index tick's window arm, for an item whose text was
// written, and the seed, which writes each item's windows with the item (cmd/dispatch-seed). It
// takes the item's owner lock, the lock every writer of the item takes (LockItemOwner), reads
// under it the text the item's blocks were split from - its stored body and its issue's title
// in one statement, or an ask's parts (AskParts, itemWindowText) - pairs it with the item's
// current block rows, and sends the one statement that writes the windows under those ids,
// stamping the state the blocks are in beside them and clearing the marker that listed it in
// that same statement (clearItemWindowsOwed, writeWindows). It writes no block row and no
// reference: the index of this item's blocks stands, and only its windows are missing, so
// everything it reads is read before it holds a transaction id.
//
// It is the one writer of an item's windows, and the delete of a post is the one other remover
// of them (TombstoneItem). An index of the item's blocks records the blocks and leaves the
// windows that stand for them to this arm (ItemIndexing.Queue), so every set of windows an item
// holds was written here: with the item for a seeded one, and one reconciliation tick behind the
// write of its text otherwise.
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
	found, err := LockItemOwner(ctx, tx, kind, itemID)
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
// (WriteItemWindows, LockItemOwner), and the reads come in the statements after it, so each
// sees what the lock's previous holder committed.
//
// main read the comment's or message's row `for no key update` here, which held it against
// every writer of that row, one holding no owner lock included. This read holds it against the
// writers that take the owner's lock, and what holds a serving writer of that row to taking it
// is api's lock table, which queues each of them behind a held owner lock (LockItemOwner,
// TestEveryWriterOnTheLockTableWaitsForTheOwnerLockOfTheRowItWrites); the census beside it
// names every writer of the row and reads no lock. A new writer of the row takes that lock, or
// the text read here can move under the windows written from it.
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
	stored, _, err := storedItemBlocks(ctx, tx, kind, tables, itemID, false)
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
