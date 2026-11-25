package api

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// Comments and messages at a cut. The comment and message routes run their present statements
// under the cut, and the shadows give each post as it stood then: its body at the cut, its
// present delete mark (decision 14), and each delivery attempt with the state that its receipt or
// reply event at the cut gives. A comment's mentions are the rows its creating transaction wrote,
// which no write changes. These are the parts of a post that the records at the cut do not give.
// The comment and message routes, and the threads of the ask routes, leave them out (leaveOut).

// undatedCommentAttempt and undatedMessageAttempt are the members of a delivery attempt that no
// receipt or reply event at the cut dates, which a past-time answer leaves out (reason E,
// asof.UndatedCommentAttempt): the state, the members that a send writes with the state, and the
// delivery mode and the session that a send resolves with no event. The shadows give such an
// attempt the empty state and the defaults of those members (commentDeliveriesShadow,
// messageDeliveriesShadow in asof). Each settle of an attempt writes its receipt in its
// transaction (settleDeliveryAttempt, the delivery sweep, the outbox's abandoned mentions, a
// delete's settle), so an attempt that no receipt dates at the cut was not settled then: it held
// no error and was no duplicate, and the answer gives those two.
var (
	undatedCommentAttempt = asof.UndatedCommentAttempt()
	undatedMessageAttempt = asof.UndatedMessageAttempt()
)

// elementsOf names element index of the array at prefix in an answer: "" for an answer that is a
// list, /replies for a thread's replies.
func elementsOf(prefix string) func(index int) string {
	return func(index int) string { return prefix + "/" + strconv.Itoa(index) }
}

// deletedSinceTheCut reports whether a post whose present delete mark is deletedAt was deleted
// after cut. Such a post reads at the cut as deleted, with no part of its body (decision 14). A
// post deleted at or before the cut was deleted at the cut, and its empty body is its body then.
func deletedSinceTheCut(deletedAt *time.Time, cut asof.Cut) bool {
	return deletedAt != nil && deletedAt.After(cut.Time())
}

// leaveOutUndatedAttempts names, of each of a post's attempts whose state (state) is empty, which
// is an attempt that no event at the cut dates, the members of it at place/deliveries/<n>.
func leaveOutUndatedAttempts[Attempt any](ctx context.Context, place string, attempts []Attempt, state func(Attempt) string, members []string) {
	for index, attempt := range attempts {
		if state(attempt) != "" {
			continue
		}
		at := place + "/deliveries/" + strconv.Itoa(index) + "/"
		named := make([]string, len(members))
		for member := range members {
			named[member] = at + members[member]
		}
		leaveOut(ctx, named...)
	}
}

// idsReadAtTheCut runs statement, an asof statement over ids ($1) and the cut ($2) that answers
// ids, and answers the set it read.
func (s *server) idsReadAtTheCut(ctx context.Context, statement string, ids []string, cut asof.Cut) (map[string]bool, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, statement, ids, cut.Time())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	read := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		read[id] = true
	}
	return read, rows.Err()
}

// commentsAtTheCut names what a past-time answer leaves out of each of comments, the comment at
// the place that at(index) names in the answer:
//   - body, of a comment that its author deleted after the cut (deletedSinceTheCut), and of a
//     comment whose body at the cut no record holds (reason T,
//     asof.CommentBodiesNotGivenStatement). The shadow gives each of them the empty body;
//   - the text of the block that the comment replies to and whether it is removed, where the
//     item that holds the block changed after the cut (replyBlocksAtTheCut);
//   - the members of each delivery attempt that no event at the cut dates (undatedCommentAttempt);
//   - whether the mark of the comment's anchor was gone, where the shadow gives the anchor as not
//     orphaned and a write with no event that dates it may have orphaned the anchor before the cut
//     (reason E, asof.CommentAnchorsOrphanedNotGivenStatement).
//
// A request at no cut leaves out nothing.
func (s *server) commentsAtTheCut(ctx context.Context, comments []model.Comment, at func(index int) string) error {
	cut, atCut := asof.CutFrom(ctx)
	if !atCut || len(comments) == 0 {
		return nil
	}
	var emptied, anchored []string
	blocks := make([]*model.ReplyBlock, len(comments))
	for index, comment := range comments {
		place := at(index)
		switch {
		case deletedSinceTheCut(comment.DeletedAt, cut):
			leaveOut(ctx, place+"/body")
		case comment.DeletedAt == nil && comment.Body == "":
			// An empty body at the cut is the body then (a suggestion's comment can hold none),
			// or a body that no record holds. Only such a comment is read again.
			emptied = append(emptied, comment.ID)
		}
		// Only an anchor that the shadow gives as not orphaned can be one that a write with no
		// event orphaned before the cut: no write sets the flag false with no event.
		if comment.Anchor != nil && !comment.Anchor.Orphaned {
			anchored = append(anchored, comment.ID)
		}
		blocks[index] = comment.ReplyBlock
		leaveOutUndatedAttempts(ctx, place, comment.Deliveries, func(attempt model.CommentDelivery) string { return attempt.State }, undatedCommentAttempt)
	}
	if err := s.replyBlocksAtTheCut(ctx, cut, blocks, at); err != nil {
		return err
	}
	if len(anchored) > 0 {
		undated, err := s.idsReadAtTheCut(ctx, asof.CommentAnchorsOrphanedNotGivenStatement, anchored, cut)
		if err != nil {
			return fmt.Errorf("read which comments' anchors no record dates at the cut: %w", err)
		}
		for index, comment := range comments {
			if undated[comment.ID] {
				leaveOut(ctx, at(index)+"/anchor/orphaned")
			}
		}
	}
	if len(emptied) == 0 {
		return nil
	}
	unknown, err := s.idsReadAtTheCut(ctx, asof.CommentBodiesNotGivenStatement, emptied, cut)
	if err != nil {
		return fmt.Errorf("read which comments have no body at the cut: %w", err)
	}
	for index, comment := range comments {
		if unknown[comment.ID] {
			leaveOut(ctx, at(index)+"/body")
		}
	}
	return nil
}

// messagesAtTheCut names what a past-time answer leaves out of each of reads, the thread at the
// place that at(index) names in the answer ("" for an answer that is one thread): of its root
// (/message) and of each reply (/replies/<n>), the body of a message that its author deleted
// after the cut (deletedSinceTheCut), the text of the block that the message replies to and
// whether it is removed, where its message changed after the cut (replyBlocksAtTheCut), and the
// members of each delivery attempt that no event at the cut dates (undatedMessageAttempt). The
// records give the body of every other message at the cut (asof.CommentBodiesNotGivenStatement
// says why). A request at no cut leaves out nothing.
func (s *server) messagesAtTheCut(ctx context.Context, reads []messageRead, at func(index int) string) error {
	cut, atCut := asof.CutFrom(ctx)
	if !atCut {
		return nil
	}
	var places []string
	var blocks []*model.ReplyBlock
	visit := func(place string, message model.Message) {
		if deletedSinceTheCut(message.DeletedAt, cut) {
			leaveOut(ctx, place+"/body")
		}
		leaveOutUndatedAttempts(ctx, place, message.Deliveries, func(attempt model.MessageDelivery) string { return attempt.State }, undatedMessageAttempt)
		places, blocks = append(places, place), append(blocks, message.ReplyBlock)
	}
	for index, read := range reads {
		thread := at(index)
		visit(thread+"/message", read.Message.Message)
		replyAt := elementsOf(thread + "/replies")
		for reply, shaped := range read.Replies {
			visit(replyAt(reply), shaped.Message)
		}
	}
	return s.replyBlocksAtTheCut(ctx, cut, blocks, func(index int) string { return places[index] })
}

// replyBlocksAtTheCut names, of each post whose reply block is blocks[index] (nil for a post that
// replies to no block) at the place at(index), the block's text and whether it is removed, where
// the item that holds the block changed after the cut (asof.ItemsChangedAfterStatement): the
// change rewrote the item's blocks, and the block index keeps no state of them with its time
// (reason E). The block of an item that did not change after the cut reads as the index holds
// it now, which is how it stood at the cut. The block of a post deleted at any time reads removed
// with no text, as the post reads deleted at the cut (decision 14). The block's node, id and
// reference are the reply's own, which no write changes, and its post's edit time and delete mark
// are the shadow's.
func (s *server) replyBlocksAtTheCut(ctx context.Context, cut asof.Cut, blocks []*model.ReplyBlock, at func(index int) string) error {
	items := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block == nil || block.DeletedAt != nil {
			continue
		}
		if _, item, _, ok := refs.ParseItemBlockNode(block.Node); ok {
			items = append(items, item)
		}
	}
	var changed map[string]bool
	if len(items) > 0 {
		var err error
		if changed, err = s.idsReadAtTheCut(ctx, asof.ItemsChangedAfterStatement, items, cut); err != nil {
			return fmt.Errorf("read which items of reply blocks changed after the cut: %w", err)
		}
	}
	for index, block := range blocks {
		if block == nil || block.DeletedAt != nil {
			continue
		}
		// A node of no shape this build reads names no item it can check, so its block is left
		// out as an item's that changed.
		if _, item, _, ok := refs.ParseItemBlockNode(block.Node); !ok || changed[item] {
			leaveOut(ctx, at(index)+"/reply_block/text", at(index)+"/reply_block/removed")
		}
	}
	return nil
}

// anchorAtTheCut reports whether ctx carries a cut, at which the single read of a comment or an
// ask (getComment, getAsk) reads no document for its anchor's block: the place of the block at
// place/anchor_block is left out where the anchor names a block (reason E). The block path is
// computed from a document tree (pmdoc.BlockPathOf), and a stored version is markdown, which
// holds the id of a typed block only: the tree of the version at the cut does not hold the id of
// an untyped block, nor of the blocks that hold a typed one, so no record gives the path at the
// cut. At no cut the caller reads the place from the live document.
func anchorAtTheCut(ctx context.Context, anchor *model.Anchor, place string) bool {
	if _, atCut := asof.CutFrom(ctx); !atCut {
		return false
	}
	if anchor != nil && anchor.BlockID != nil && *anchor.BlockID != "" {
		leaveOut(ctx, place+"/anchor_block")
	}
	return true
}
