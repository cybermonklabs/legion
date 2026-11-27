package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// commentProjectionKind reads the suggestion's kind from the live document. ctx is the handler's
// document context (Docs.Join), so the read joins the handler's transaction: it runs while that
// transaction is open, where a second pooled connection is what deadlocks the pool
// (store.ErrNestedAcquire), and it reads the document as the transaction's own writes left it.
func (s *server) commentProjectionKind(ctx context.Context, comment model.Comment) (string, error) {
	if comment.Suggestion == nil || comment.Anchor == nil {
		return "", nil
	}
	return s.deps.Docs.SuggestionKind(ctx, comment.Anchor.ArtifactID, comment.Anchor.MarkID)
}

// projectThreadMark writes the document margin mark a change to comment's body leaves behind:
// its own, when the comment carries an anchor, and otherwise the mark of the comment it
// replies to, which holds the text of each reply (commentMarkRecord) and so goes stale when a
// reply's body moves. A thread anchored nowhere projects nothing. ctx is the handler's
// document context (Docs.Join), which is the request's context with the transaction joined to
// it, so the reads below run inside that transaction as commentProjectionKind does.
//
// comment is the row as the route's queued write of it leaves it, which the table holds only
// once Commit has run that write: an anchored one is registered on ledger as that write leaves
// it (registerCommentAnchor), for the anchor refreshes ProjectMark runs, and a reply takes its
// own place in the chain the table holds (withWrittenReply), so the record names the body the
// write leaves rather than the one it replaces.
//
// A decided or orphaned suggestion's mark is gone, so it has no projection kind to carry; the
// write still lands and ProjectMark writes only the marks map, as a reply, a reopen and a
// resolve of that suggestion do.
func (s *server) projectThreadMark(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, comment model.Comment, actor model.Actor) error {
	marked := comment
	if marked.Anchor == nil {
		if comment.ReplyTo == nil {
			return nil
		}
		root, err := s.loadComment(ctx, tx, *comment.ReplyTo)
		if err != nil {
			return err
		}
		if root.Anchor == nil {
			return nil
		}
		marked = root
	} else {
		s.registerCommentAnchor(ledger, comment, actor, true)
	}
	replies, err := s.loadReplyChain(ctx, tx, everyReader, "reply_to", marked.ID)
	if err != nil {
		return err
	}
	replies = withWrittenReply(replies, comment)
	projectionKind := ""
	kind, kindErr := s.commentProjectionKind(ctx, marked)
	if kindErr != nil && !errors.Is(kindErr, docs.ErrAnchorMissing) {
		return kindErr
	}
	if kindErr == nil {
		projectionKind = kind
	}
	return s.deps.Docs.ProjectMark(ctx, marked.Anchor.ArtifactID, marked.Anchor.MarkID,
		commentMarkRecord(marked, replies, projectionKind), actor)
}

// withWrittenReply is replies, a chain loadReplyChain read, with written in place of the row of
// the same id: a reply this transaction has queued an update of, which the table holds as it
// stood before. A chain that does not hold it - the thread root's own write - is returned as
// read.
func withWrittenReply(replies []model.Comment, written model.Comment) []model.Comment {
	for index := range replies {
		if replies[index].ID == written.ID {
			replies[index] = written
			break
		}
	}
	return replies
}

func commentMarkRecord(comment model.Comment, replies []model.Comment, suggestionKind string) docs.MarkRecord {
	record := docs.MarkRecord{
		Kind:      "comment",
		By:        docs.ActorRef(comment.Author),
		CreatedAt: timestampValue(comment.CreatedAt),
		Text:      comment.Body,
		Resolved:  comment.Resolved,
		Replies:   make([]docs.MarkReply, 0, len(replies)),
	}
	for _, reply := range replies {
		record.Replies = append(record.Replies, docs.MarkReply{
			By:   docs.ActorRef(reply.Author),
			Text: reply.Body,
			At:   timestampValue(reply.CreatedAt),
		})
	}
	if comment.Suggestion != nil {
		record.Kind = suggestionKind
		record.Content = comment.Suggestion.ReplaceWith
		record.Status = "pending"
		if comment.Suggestion.Accepted != nil {
			if *comment.Suggestion.Accepted {
				record.Status = "accepted"
			} else {
				record.Status = "rejected"
			}
		}
	}
	return record
}

// commentEventThread is what a comment payload says about the thread the comment joined:
// the ask it replies to (question, state, and whose turn it is once this comment is the
// newest reply) or the root of the comment thread. Populated on comment.created and on the
// comment.answered a delivery callback appends; an edit or a delete names the root alone
// (commentCorrectionPayload); zero for root comments and for every other comment.* event.
type commentEventThread struct {
	AskQuestion  string
	AskState     string
	AskWaitingOn string
	ThreadRootID string
}

// commentWriteResponse is what a route that posts a comment answers: the comment row, and for a
// reply to an open ask whom that ask waits on now that the reply is its newest
// (commentEventThread.AskWaitingOn, under the same name the comment's event carries it).
type commentWriteResponse struct {
	shapedComment
	AskWaitingOn string `json:"ask_waiting_on,omitempty"`
}

// commentEventPayload builds the payload of every comment.* event. It takes what the write moved
// in the reference graph — an empty ReferenceChanges on a transition that writes no body — so a
// producer whose comment text can cite something cannot append an event that stays silent. It
// reads the block the comment answers (reply_block) as the transaction sees it into comment
// itself, so a route that answers with the comment answers that block too. The payload reaches
// every subscriber, so the block is read as everyReader.
func (s *server) commentEventPayload(ctx context.Context, tx pgx.Tx, comment *model.Comment, artifactName string, thread commentEventThread, changes model.ReferenceChanges) (model.CommentEventPayload, error) {
	if err := hydrateCommentReplyBlocks(ctx, tx, everyReader, comment); err != nil {
		return model.CommentEventPayload{}, fmt.Errorf("load the block comment %s answers: %w", comment.ID, err)
	}
	payload := model.CommentEventPayload{
		Comment:                 *comment,
		ArtifactName:            artifactName,
		AskQuestion:             thread.AskQuestion,
		AskState:                thread.AskState,
		AskWaitingOn:            thread.AskWaitingOn,
		ThreadRootID:            thread.ThreadRootID,
		ReferenceChangesPayload: model.NewReferenceChangesPayload(changes),
	}
	// A body is bounded block by block, so it can be longer than one event carries: the event
	// takes its first part and says so, and a reader gets the rest from the comment itself.
	payload.Body, payload.BodyTruncated = model.EventBody(comment.Body)
	if comment.ArtifactID == nil {
		return payload, nil
	}
	if err := tx.QueryRow(ctx, `select coalesce(project_key, ''), slug from artifacts where id = $1`, *comment.ArtifactID).Scan(&payload.ProjectKey, &payload.ArtifactSlug); err != nil {
		return model.CommentEventPayload{}, fmt.Errorf("load artifact comment event owner: %w", err)
	}
	return payload, nil
}

// commentCorrectionPayload is the payload of comment.edited and comment.deleted: the event
// carries the thread_root_id its comment's comment.created carried, and the thread_logins on
// that thread as the correction is written, so a reader routes a correction as it routes a post -
// an Inbox refreshes only for the logins with a Replies row on the thread. Those are read by the
// event's insert (the logins this returns, for queueLoggedEvent), as the comment create's are,
// so they are what the transaction's queued writes leave. The ask's question, state and turn
// belong to the moment the comment was posted and stay with that event.
//
// A reply's comment.edited carries the block it names with the text that block holds as the edit
// is written, as its comment.created carried the text it held then. Its comment.deleted carries
// no text at all: the deleted comment's body and suggestion, and the anchor quote of an insert
// suggestion no accept made document text, which is the text that suggestion inserted, are
// already cleared in the comment this payload is built from, and the block it names keeps every
// field but its text, its quote and its occurrence (deleteEventReplyBlock), so the event keeps
// every field that routes it - reply_to, ask_id, the anchor a root comment carries, the block's
// node id and id, thread_root_id and thread_logins - and no copy of the post the reply answers.
// A reply naming no block carries reply_block null either way.
func (s *server) commentCorrectionPayload(ctx context.Context, tx pgx.Tx, comment *model.Comment, artifactName string, changes model.ReferenceChanges) (model.CommentEventPayload, *events.Logins, error) {
	var thread commentEventThread
	if comment.ReplyTo != nil {
		thread.ThreadRootID = *comment.ReplyTo
	}
	payload, err := s.commentEventPayload(ctx, tx, comment, artifactName, thread, changes)
	if err != nil {
		return model.CommentEventPayload{}, nil, err
	}
	if comment.DeletedAt != nil {
		payload.ReplyBlock = deleteEventReplyBlock(payload.ReplyBlock)
	}
	return payload, events.ThreadLoginsOf(commentThread(*comment)), nil
}

func (s *server) commentArtifactName(ctx context.Context, tx pgx.Tx, comment model.Comment) (string, error) {
	if comment.Anchor == nil {
		return "", nil
	}
	var name string
	if err := tx.QueryRow(ctx, `select name from artifacts where id = $1`, comment.Anchor.ArtifactID).Scan(&name); err != nil {
		return "", err
	}
	return name, nil
}

// registerCommentAnchor records comment's anchored row on ledger as the route's queued write of it
// leaves it (docs.Ledger.RegisterPendingAnchor): the anchor refreshes the transaction runs after
// that write - ProjectMark's - read the open anchors from the table, which holds the write only
// once Commit has run it, so they take this row from the ledger, as they would have read it after
// the write. An unresolved row is refreshed, and a refresh that moves its anchor prepares its
// event (commentAnchorRefreshEvent); a resolved one no longer is. A comment without an anchor has
// nothing to register. stored says the table already holds the row (an update rather than a new
// comment's insert).
func (s *server) registerCommentAnchor(ledger *docs.Ledger, comment model.Comment, actor model.Actor, stored bool) {
	if comment.Anchor == nil {
		return
	}
	kind := docs.MarkComment
	if comment.Suggestion != nil {
		kind = docs.MarkSuggestion
	}
	row := docs.PendingAnchor{ID: comment.ID, Kind: kind, Anchor: *comment.Anchor, Open: !comment.Resolved}
	if row.Open {
		row.Event = func(ctx context.Context, tx pgx.Tx) (func(model.Anchor) (model.Event, error), error) {
			return s.commentAnchorRefreshEvent(ctx, tx, comment, actor, stored)
		}
	}
	ledger.RegisterPendingAnchor(row)
}

// commentAnchorRefreshEvent builds comment's comment.anchor_refreshed event for a refresh that
// moves its anchor (docs.PendingAnchor.Event): the row as the route's write leaves it, with the
// quote and orphaned flag the refresh writes, as the refresh reads a stored row back. What it
// reads - the anchor document's name and key, and a stored row's mention and delivery rows - it
// reads here, in the read phase, once the refresh has found the anchor moved; a new comment's
// mention rows are inserted after its projection, so the refresh read none. The builder only
// assembles the event.
func (s *server) commentAnchorRefreshEvent(ctx context.Context, tx pgx.Tx, comment model.Comment, actor model.Actor, stored bool) (func(model.Anchor) (model.Event, error), error) {
	row := comment
	row.Mentions, row.Deliveries = []model.Mention{}, []model.CommentDelivery{}
	if stored {
		hydrated, err := s.hydrateCommentSideTables(ctx, tx, everyReader, []model.Comment{row})
		if err != nil {
			return nil, fmt.Errorf("load refreshed comment %q: %w", comment.ID, err)
		}
		row = hydrated[0]
	}
	anchor := *comment.Anchor
	var payload model.CommentEventPayload
	if err := tx.QueryRow(ctx, `
		select name, coalesce(project_key, ''), slug
		from artifacts where id = $1
	`, anchor.ArtifactID).Scan(&payload.ArtifactName, &payload.ProjectKey, &payload.ArtifactSlug); err != nil {
		return nil, fmt.Errorf("load refreshed comment %q artifact: %w", comment.ID, err)
	}
	return func(refreshed model.Anchor) (model.Event, error) {
		moved := anchor
		moved.Quote, moved.Orphaned = refreshed.Quote, refreshed.Orphaned
		payload := payload
		payload.Comment = row
		payload.Comment.Anchor = &moved
		return model.Event{
			IssueKey: row.IssueKey, ArtifactID: row.ArtifactID,
			Type: "comment.anchor_refreshed", Actor: actor, Payload: payload,
		}, nil
	}, nil
}
