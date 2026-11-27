package refs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// IndexItemIfOwed indexes comment, message or ask itemID in the caller's transaction when its
// blocks are owed: its state row is missing (an older build wrote it before the state existed),
// owed (owed_at set by an older build's write, or by rebuild-refs), or indexed from another text
// than the stored one. It indexes as ReindexOwed does: the blocks and the mentions each
// holds, and for a comment or an issue message its own refs, at the time its text was written; a
// direct message writes no mention, and an ask's own refs are its question's, which the grammar
// reconciliation owns (ReindexOwed). The block-index reconciliation drains owed items
// through it (IndexOwedItemBlocks); a reply that names a block of an owed item plans the same
// index (PlanItemIfOwed) and writes it before its own writes, so the block it names is one the
// item holds now. The caller holds the item's owner, the reconciliation included, and a reply
// holds the message row it answers; the index takes the state row after them. It reports whether
// the item was owed and so indexed. It is PlanItemIfOwed and ItemRepair.Write back to back, the
// repair's statements run at once as a batch of their own; a transaction that decides before it
// writes plans the repair and queues it in its write phase, reading the blocks it leaves from
// the plan (ItemRepair.Block).
func IndexItemIfOwed(ctx context.Context, tx pgx.Tx, kind ItemKind, itemID string, origins text.Origins) (bool, error) {
	repair, err := PlanItemIfOwed(ctx, tx, kind, itemID, origins)
	if err != nil {
		return false, err
	}
	if !repair.Owed() {
		return false, nil
	}
	if err := repair.Write(ctx, tx); err != nil {
		return false, err
	}
	return true, nil
}

// ItemRepair is the index an item is owed (IndexItemIfOwed), planned against what its block
// index holds (PlanItemIfOwed) and written by Queue, which puts its statements on a write
// phase's batch. A repair of an item that owes none writes nothing and holds no plan: its
// blocks stand as the index stored them (CurrentItemBlock).
type ItemRepair struct {
	kind     ItemKind
	itemID   string
	owed     bool
	mentions bool
	indexing ItemIndexing
	refs     Replacement
}

// PlanItemIfOwed plans the index comment, message or ask itemID is owed, read and planned from
// its stored text as IndexItemIfOwed indexes it, its short ids resolved for the author its row
// records (NewReader); an item that owes none plans nothing.
func PlanItemIfOwed(ctx context.Context, q Queryer, kind ItemKind, itemID string, origins text.Origins) (ItemRepair, error) {
	var query string
	switch kind {
	case ItemAsk:
		// An ask has no body of its own: its blocks are its parts, read from its row.
		return planAskIfOwed(ctx, q, itemID, origins)
	case ItemComment:
		query = `
			select c.body, coalesce(c.edited_at, c.created_at), true,
			       s.comment_id is not null, s.owed_at is not null, s.body_sha256, c.author
			from comments c left join comment_block_state s on s.comment_id = c.id
			where c.id = $1::uuid`
	case ItemMessage:
		query = `
			select m.body, coalesce(m.edited_at, m.created_at), m.issue_key is not null,
			       s.message_id is not null, s.owed_at is not null, s.body_sha256, m.author
			from messages m left join message_block_state s on s.message_id = m.id
			where m.id = $1::uuid`
	default:
		return ItemRepair{}, fmt.Errorf("index the blocks of %s %s: not an item kind", kind, itemID)
	}
	var body string
	var writtenAt time.Time
	var mentions, stated, owed bool
	var sum *string
	var author []byte
	if err := q.QueryRow(ctx, query, itemID).Scan(&body, &writtenAt, &mentions, &stated, &owed, &sum, &author); err != nil {
		return ItemRepair{}, fmt.Errorf("read the block state of %s %s: %w", kind, itemID, err)
	}
	if stated && !owed && sum != nil && *sum == itemblocks.BodySHA256(body) {
		return ItemRepair{kind: kind, itemID: itemID}, nil
	}
	repair := ItemRepair{kind: kind, itemID: itemID, owed: true, mentions: mentions}
	var located []text.Located
	if mentions {
		writer, err := writerOf(author)
		if err != nil {
			return ItemRepair{}, fmt.Errorf("read the author of %s %s: %w", kind, itemID, err)
		}
		located = NewReader(ctx, q, writer, string(kind), itemID, origins).Mentions(body)
	}
	index, err := ReadItemIndex(ctx, q, kind, itemID)
	if err != nil {
		return ItemRepair{}, fmt.Errorf("index the blocks of %s %s: %w", kind, itemID, err)
	}
	if repair.indexing, err = PlanItemIndex(index, kind, itemID, body, serverURL, writtenAt, located, nil); err != nil {
		return ItemRepair{}, fmt.Errorf("index the blocks of %s %s: %w", kind, itemID, err)
	}
	if !mentions {
		return repair, nil
	}
	if repair.refs, err = PlanReplace(ctx, q, string(kind), itemID, text.Refs(located), &writtenAt); err != nil {
		return ItemRepair{}, fmt.Errorf("index %s %s: %w", kind, itemID, err)
	}
	repair.refs = repair.refs.SettlingForms()
	return repair, nil
}

// Owed reports whether the item owed an index, so the repair has statements to queue.
func (r ItemRepair) Owed() bool {
	return r.owed
}

// Queue queues the repair's writes on a write phase's batch: the item's blocks and their
// mentions, then, for a comment, an issue message or an ask, its own references, whose settle of
// the queues follows both deletes.
func (r ItemRepair) Queue(b *store.WriteBatch) {
	if !r.owed {
		return
	}
	r.indexing.Queue(b)
	if r.mentions {
		r.refs.Queue(b)
	}
}

// Write runs the repair's writes at once, as a batch of their own (Queue, store.RunWrite), for a
// caller with no write phase to queue them on: it is where a repair runs when nothing holds it
// until later, and the one place that decides how one runs. A repair the item owes none writes
// nothing.
func (r ItemRepair) Write(ctx context.Context, tx pgx.Tx) error {
	if !r.owed {
		return nil
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		r.Queue(b)
		return nil
	}); err != nil {
		return fmt.Errorf("index the blocks of %s %s: %w", r.kind, r.itemID, err)
	}
	return nil
}

// Block is the node id and stored excerpt of block blockID of the item as the repair leaves it,
// which a reply naming that block reads in place of the stored one (CurrentItemBlock); found is
// false when the repaired body holds no such block.
func (r ItemRepair) Block(blockID string) (nodeID, excerpt string, found bool) {
	return r.indexing.Block(blockID)
}

// CurrentItemBlock is block blockID of comment, message or ask itemID while a current revision of
// the item holds it: its node id and stored excerpt. found is false when the item holds no such
// block now, a removed one included, and when who may not see the item the block is part of,
// which is then answered as a block the item does not hold.
//
// The visibility read is of the item's own node - the comment, the message or the ask - and not
// of a node of the block's own kind: a block's text is its item's text, and the store holds a
// node for the item (graphstore.KindComment), where no writer makes a `comment_block`,
// `message_block` or `ask_block` node, so a test over one of those always passed.
func CurrentItemBlock(ctx context.Context, q Queryer, who model.Actor, kind ItemKind, itemID, blockID string) (nodeID, excerpt string, found bool, err error) {
	tables, err := kind.tables()
	if err != nil {
		return "", "", false, err
	}
	err = q.QueryRow(ctx, `
		select b.node_id, b.excerpt from `+tables.blocks+` b
		where b.`+tables.item+` = $1::uuid and b.block_id = $2 and b.position is not null
		  and `+VisibleKind(string(kind), "b."+tables.item+"::text", "$3")+`
	`, itemID, blockID, VisibilityArg(who)).Scan(&nodeID, &excerpt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("read block %s of %s %s: %w", blockID, kind, itemID, err)
	}
	return nodeID, excerpt, true, nil
}

// ItemBlockRef is the reference of block blockID of comment, message or ask itemID, the one
// ReplyBlocks reads for a reply naming it: the item addressed under its issue (issueKey), or,
// for a comment on a document, under that document (project, slug). A direct message has no
// address, and so its blocks have no reference. A write that names a block before its reply row
// exists, which that read has nothing to join, builds the reference with it.
func ItemBlockRef(kind ItemKind, issueKey *string, project, slug, itemID, blockID string) string {
	return BlockRef(itemRef(string(kind), issueKey, project, slug, itemID), blockID)
}

// ReplyBlocks reads the block each of the replies replyIDs names in replies_to_block, keyed by
// reply id, in one query; a reply that names no block has no entry. kind is the kind of item the
// block belongs to, which names the table its blocks are in and the column keying them
// (itemTables); the replies are read from the table of the kind that answers such a block
// (repliesFrom): a comment's and an ask's replies are comments, a message's are messages. A
// block belongs to an item in its reply's own thread, so its reference is addressed under the
// reply's owner (as itemBlockParent addresses the reply's parent); a message is owned by its
// issue alone, so a message reply needs neither project nor slug, and a direct message's block
// has no reference. Text is the block's stored excerpt, which a removal keeps, so a removed
// block reads the last text it held. A block of a comment or a message carries the post's
// EditedAt, read from the item row the block belongs to, so a read of the reply alone says its
// parent was edited. A block of a post its author deleted reads removed with no text, since the
// delete clears every excerpt (TombstoneItem), and carries the post's delete mark, read from the
// same row.
//
// It answers for who: a block of an item who may not see is answered as no block, so the reply
// that names it reads as one that names none, and neither the block's id nor its text nor its
// post's marks are answered. That test is of the item's own node, as CurrentItemBlock's is.
func ReplyBlocks(ctx context.Context, q Queryer, who model.Actor, kind ItemKind, replyIDs []string) (map[string]*model.ReplyBlock, error) {
	blocks := make(map[string]*model.ReplyBlock)
	if len(replyIDs) == 0 {
		return blocks, nil
	}
	tables, err := kind.tables()
	if err != nil {
		return nil, fmt.Errorf("read reply blocks of %s: not an item kind", kind)
	}
	replies, _ := repliesFrom(kind).tables()
	owner, joins := `coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, '')`, `
			left join issues i on i.key = r.issue_key
			left join artifacts a on a.id = r.artifact_id`
	if repliesFrom(kind) == ItemMessage {
		owner, joins = `'', ''`, ``
	}
	query := `
		select r.id::text, b.node_id, b.block_id, b.excerpt, b.position is null, b.` + tables.item + `::text,
		       r.issue_key, ` + owner + `, ` + kind.postMarkColumns("post") + `
		from ` + replies.items + ` r
		join ` + tables.blocks + ` b on b.node_id = r.replies_to_block
		join ` + tables.items + ` post on post.id = b.` + tables.item + joins + `
		where r.id = any($1::uuid[]) and r.replies_to_block is not null
		  and ` + VisibleKind(string(kind), "b."+tables.item+"::text", "$2")
	rows, err := q.Query(ctx, query, replyIDs, VisibilityArg(who))
	if err != nil {
		return nil, fmt.Errorf("read the blocks %s replies name: %w", kind, err)
	}
	defer rows.Close()
	for rows.Next() {
		var replyID, itemID, project, slug string
		var issueKey *string
		var marks postMarks
		block := model.ReplyBlock{}
		if err := rows.Scan(append([]any{&replyID, &block.Node, &block.BlockID, &block.Text, &block.Removed, &itemID, &issueKey, &project, &slug}, marks.targets()...)...); err != nil {
			return nil, fmt.Errorf("scan the block a %s reply names: %w", kind, err)
		}
		if block.DeletedAt, block.DeletedBy, err = marks.deleted(); err != nil {
			return nil, fmt.Errorf("the %s block %s: %w", kind, block.Node, err)
		}
		block.EditedAt = marks.editedAt
		block.Ref = BlockRef(itemRef(string(kind), issueKey, project, slug, itemID), block.BlockID)
		blocks[replyID] = &block
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the blocks %s replies name: %w", kind, err)
	}
	return blocks, nil
}
