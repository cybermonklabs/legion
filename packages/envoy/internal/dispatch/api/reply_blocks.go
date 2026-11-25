package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// replyBlockOwner is the kind of item the block a reply names belongs to, read from the node id
// replies_to_block stores (refs.ParseItemBlockNode): `ask/<id>#<part>` is a part of an ask,
// which a reply naming that ask names, and `comment/<id>#<block>` a block of the comment the
// reply answers. A node id of no shape this build knows is read as a comment's, the kind every
// reply written before ask parts named.
func replyBlockOwner(nodeID string) refs.ItemKind {
	if kind, _, _, ok := refs.ParseItemBlockNode(nodeID); ok {
		return kind
	}
	return refs.ItemComment
}

// namedBlock is the block of its parent a reply names (reply_to_block, in_reply_to_block): the
// kind of item holding it, since a comment's reply may name a block of the comment it answers or
// a part of the ask it was asked under, the node id replies_to_block stores, the item holding the
// block, the block's own id and its stored excerpt.
type namedBlock struct {
	kind    refs.ItemKind
	node    string
	item    string
	id      string
	excerpt string
}

// replyBlock is the block as a reply carries it (model.ReplyBlock), for a write whose reply row
// is queued and so has nothing for the read of a stored reply to join (refs.ReplyBlocks). The
// block is one the item holds now, so it is not removed; the item's own kind addresses it, with
// issueKey, project and slug, as that read addresses it, under the reply's owner (ownerAddress).
func (block *namedBlock) replyBlock(issueKey *string, project, slug string) *model.ReplyBlock {
	if block == nil {
		return nil
	}
	return &model.ReplyBlock{
		Node:    block.node,
		Ref:     refs.ItemBlockRef(block.kind, issueKey, project, slug, block.item, block.id),
		BlockID: block.id,
		Text:    block.excerpt,
	}
}

// ownerAddress is how refs addresses an item under owner: an issue's comments and messages by
// its key, a document's comments by its project and slug (refs.ItemBlockRef, refs.ReplyBlocks).
func (s *server) ownerAddress(ctx context.Context, tx pgx.Tx, owner owner) (issueKey *string, project, slug string, err error) {
	if owner.ArtifactID == nil {
		return owner.IssueKey, "", "", nil
	}
	if err := tx.QueryRow(ctx, `
		select coalesce(project_key, ''), slug from artifacts where id = $1
	`, *owner.ArtifactID).Scan(&project, &slug); err != nil {
		return nil, "", "", fmt.Errorf("load the address of document %s: %w", *owner.ArtifactID, err)
	}
	return owner.IssueKey, project, slug, nil
}

// deferredRefusal splits what a delivery reply's planning returned into the refusal its route
// holds back and the failure it answers at once. The two delivery replies plan the reply above
// their row locks and raise what that planning refuses where the refusal was raised before,
// after the mention and the attempt have been checked, so that no answer of either route moves
// (planCommentReply, planMessageReply). Only an *apiError is such a refusal: it is this
// request's own answer, and the transaction is sound.
//
// Anything else is a database failure, which must not be held back. A failed query aborts the
// transaction, so every statement after it answers 25P02 "current transaction is aborted", and
// a route that held the first failure back would log that one and lose the cause.
func deferredRefusal(err error) (refusal, failure error) {
	var refused *apiError
	if errors.As(err, &refused) {
		return err, nil
	}
	return nil, err
}

// replyRowPlan is the reply row a delivery callback's route plans above its first row lock and
// may then write, so that the transaction holds its transaction id for its writes alone
// (INFRA-636). Both routes write as they go rather than through a ledger, so each planned
// statement is run where that route makes its write. The half that differs between them - the
// thread a comment reply joins, the body and the named block a message reply carries - each
// embeds this in a plan of its own (commentReplyPlan, messageReplyPlan).
type replyRowPlan struct {
	// parentIndex is the index the item the reply answers was found to owe, written before the
	// reply's own writes (refs.ItemRepair.Write).
	parentIndex refs.ItemRepair
	// id and createdAt are the reply row's, reserved while planning because its index and its
	// references are planned for them before the insert runs.
	id        string
	createdAt time.Time
	// replyBlock is the block of its parent the reply names as the reply carries it, nil when
	// it names none. It is read while planning, because the reply's row is not inserted then
	// and the read that gives a stored reply its block would find nothing
	// (namedBlock.replyBlock).
	replyBlock *model.ReplyBlock
	// changes are the reference graph changes the reply's event counts, write is the step that
	// writes its blocks, their mentions and its references, and indexed is what that step states
	// in the node and tuple store, which the reply queues under its event (graphstore.QueueCaused).
	changes model.ReferenceChanges
	write   docs.WriteStep
	indexed graphstore.Uncaused
	// refusal is the *apiError this planning earned, held back to the branch that answers with
	// it so that no answer of the route moves. A failure that is not a refusal is returned at
	// once instead (deferredRefusal).
	refusal error
}

// planReplyBlock checks that blockID is a current block of item itemID — the comment or the
// message a reply answers, or the ask a reply names, whose blocks are the parts of its question,
// its options and its answer — and returns it with the index the item owes, which the caller
// writes where it makes its own writes: queued on its write phase (namedReplyBlock) or run at
// once beside them (refs.ItemRepair.Write, the two delivery replies). The caller holds the
// item's owner (when it has one), which assigns no transaction id (store.LockOwner), and takes
// the item row of a message with the index it writes, never with this read; an item an older
// build wrote, or wrote after its last index, is indexed by the same transaction
// (refs.PlanItemIfOwed), so the block named is one the item holds now: the repaired index is
// read from its plan, the stored one from the block rows. A block the item does not hold now
// answers 400 with code, naming field and the block, and so does one who, the reply's writer,
// may not see.
//
// The live write of this request is the reply, which its own route stamps; the item indexed here
// is the parent, whose text an earlier write already carried an event for, so a block mention
// this records takes the source_seq of the parent's own refs row for its target at the item's
// first indexing (refs.PlanItemIndex) and none after it, and either way it is no new fact of this
// request.
func (s *server) planReplyBlock(
	ctx context.Context, tx pgx.Tx, who model.Actor, kind refs.ItemKind, itemID, blockID, field, code string,
) (refs.ItemRepair, *namedBlock, error) {
	repair, err := refs.PlanItemIfOwed(ctx, tx, kind, itemID, s.deps.ServerURL)
	if err != nil {
		return refs.ItemRepair{}, nil, err
	}
	node, excerpt, found := "", "", false
	if repair.Owed() {
		node, excerpt, found = repair.Block(blockID)
	} else if node, excerpt, found, err = refs.CurrentItemBlock(ctx, tx, who, kind, itemID, blockID); err != nil {
		return refs.ItemRepair{}, nil, err
	}
	if !found {
		return refs.ItemRepair{}, nil, errorf(http.StatusBadRequest, code, "%s %s is not a block of %s %s", field, blockID, kind, itemID)
	}
	return repair, &namedBlock{kind: kind, node: node, item: itemID, id: blockID, excerpt: excerpt}, nil
}

// nodeID is the block's node id for replies_to_block, null for a reply naming no block.
func (block *namedBlock) nodeID() *string {
	if block == nil {
		return nil
	}
	return &block.node
}

// queueReplyParentMessageLock queues the lock of message id, the parent a reply names a block of,
// `for no key update` as the first write of the reply's write phase, after its issue's owner
// lock, the order recordPendingMessageDelivery takes them in, so the index of that parent the
// transaction may queue (planReplyBlock) and the reply's own writes run under it. It is not FOR
// UPDATE: the reply's insert then takes the row FOR KEY SHARE through its foreign key, which this
// lock does not block.
func queueReplyParentMessageLock(b *store.WriteBatch, id string) {
	b.Queue("", `select 1 from messages where id = $1 for no key update`, id)
}

// lockReplyParentMessage takes queueReplyParentMessageLock's lock at once, in a transaction that
// writes as it goes (replyMessage, a session's reply to a message delivery).
func lockReplyParentMessage(ctx context.Context, tx pgx.Tx, id string) error {
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		queueReplyParentMessageLock(b, id)
		return nil
	})
}

// hydrateCommentReplyBlocks sets each comment's ReplyBlock, the block of its parent it answers
// (null when it names none, or names one who may not see), in one query per kind of item the
// named blocks belong to: a reply under an ask names a part of that ask, every other reply a
// block of the comment it answers (replyBlockOwner), so a page of either costs one query and a
// page of both two. A comment that already carries its block is left alone: its writer read that
// block before its row was written, and its insert may still be queued, where this read would
// find nothing (namedBlock.replyBlock).
func hydrateCommentReplyBlocks(ctx context.Context, q queryer, who model.Actor, comments ...*model.Comment) error {
	ids := map[refs.ItemKind][]string{}
	for _, comment := range comments {
		if comment.RepliesToBlock != nil && comment.ReplyBlock == nil {
			owner := replyBlockOwner(*comment.RepliesToBlock)
			ids[owner] = append(ids[owner], comment.ID)
		}
	}
	blocks := make(map[string]*model.ReplyBlock, len(ids))
	for owner, owned := range ids {
		found, err := refs.ReplyBlocks(ctx, q, who, owner, owned)
		if err != nil {
			return err
		}
		maps.Copy(blocks, found)
	}
	for _, comment := range comments {
		if block, read := blocks[comment.ID]; read {
			comment.ReplyBlock = block
		}
	}
	return nil
}

// hydrateMessageReplyBlocks sets each message's ReplyBlock, the block of the message it answers
// (null when it names none, or names one who may not see), in one query over every message that
// names one (RepliesToBlock), however many there are, and none when no message does.
func hydrateMessageReplyBlocks(ctx context.Context, q queryer, who model.Actor, messages ...*model.Message) error {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.RepliesToBlock != nil {
			ids = append(ids, message.ID)
		}
	}
	blocks, err := refs.ReplyBlocks(ctx, q, who, refs.ItemMessage, ids)
	if err != nil {
		return err
	}
	for _, message := range messages {
		message.ReplyBlock = blocks[message.ID]
	}
	return nil
}

// hydrateThreadReplyBlocks sets ReplyBlock on every message of the threads roots head: each root
// (a message read by id may be a reply itself) and each reply chains holds under it, in one
// query, for who.
func hydrateThreadReplyBlocks(ctx context.Context, q queryer, who model.Actor, roots []model.Message, chains map[string][]model.Message) error {
	messages := make([]*model.Message, 0, len(roots))
	for index := range roots {
		messages = append(messages, &roots[index])
		chain := chains[roots[index].ID]
		for reply := range chain {
			messages = append(messages, &chain[reply])
		}
	}
	return hydrateMessageReplyBlocks(ctx, q, who, messages...)
}

// deleteEventReplyBlock is block, the block of its parent a reply names, as the reply's delete
// event carries it: every field that routes the event and says which block it is - its node id,
// which names the kind and the id of the item holding it, its reference, its id, whether it is
// removed and the quoted post's marks - and no text. A delete event carries no text of another
// post (dispatch://INFRA-1523, the architect's ruling of 2026-11-19): the reply's creation froze
// a preview of its parent, which the event log keeps, and the delete is a new event written after
// it, so it writes no new copy of that text. Both correction builders apply it on a delete
// (commentCorrectionPayload, messageCorrectionEvent), and a message's delete leaves out
// reply_body with it, the preview of the parent or of this block a message event carries beside
// it. The block is copied, so the reply a route answers keeps the text its read serves. A reply
// naming no block carries nil, which stays nil.
func deleteEventReplyBlock(block *model.ReplyBlock) *model.ReplyBlock {
	if block == nil {
		return nil
	}
	carried := *block
	carried.Text = ""
	return &carried
}
