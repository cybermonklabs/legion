// message_corrections.go is PATCH /api/v1/messages/{id} and DELETE /api/v1/messages/{id}: the
// author of an issue message replaces its body, or withdraws it. They are the message twins of
// editComment (comment_actions.go) and deleteComment (comment_delete.go), with what a message
// does not have left out: it carries no anchor and so no document margin mark, no suggestion and
// no fallback copy, and its pending sends are rows of message_deliveries.
//
// Each runs in the two phases the message create does (createStoredMessage): a read phase that
// takes the issue's owner lock and reads and plans everything the write decides on, holding no
// transaction id, and one batch that sends every write and is committed (store.RunWrite). A
// message does no document work, so neither joins a document ledger.
package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// editMessageInput is the body of PATCH /api/v1/messages/{id}.
//
// actor is who makes the edit, for a bearer caller that names its own session: the route is the
// author's, and an agent's message has an author of kind session, exactly as editCommentInput
// says of a comment. It carries no blocks and no base_revision, which the comment edit takes
// from the dashboard editor: no editor holds a message, so the block ids of an edited message
// follow its text.
type editMessageInput struct {
	Body  string       `json:"body" required:"true"`
	Actor *model.Actor `json:"actor"`
}

// lockedMessageForCorrection is the prologue both corrections take. It reads the message, takes
// its issue's owner lock without refusing a closed issue (lockIssue) — a wrong post nobody can
// correct after the issue closes is the defect these two routes exist for — reads the row again
// in a statement after that lock, and applies the refusals in the order every write of this
// change takes them: the author first, so a caller that is not the author learns nothing else
// about the post; then the kind, since only a message on an issue is edited or deleted; then a
// post already deleted, since a delete is final. verb names the write in the author's refusal,
// and past names it in the kind's: "edit" and "edited", "delete" and "deleted". One word with a
// "d" after it reads as "editd".
//
// It takes no row lock, as lockedComment takes none: a row lock would give the transaction a
// transaction id to hold through everything its read phase does, and every writer of an issue
// message holds its issue's owner lock (refs.LockItemOwner), so the row holds still until this
// transaction ends and two corrections of one message serialise on that lock. A message with no
// issue takes no lock: it is refused below, before anything is written.
//
// now is tx's now(), read with the row after the lock: the time the transaction began, which
// Postgres holds fixed for the whole of it. It is the edited_at or deleted_at the correction
// writes, read in its read phase so its index, its event and its answer carry it before the
// queued update runs, as the message create reads its created_at (createStoredMessage).
//
// ok is false once it has written the refusal.
func (s *server) lockedMessageForCorrection(
	w http.ResponseWriter, r *http.Request, tx pgx.Tx, actor model.Actor, verb, past string,
) (model.Message, time.Time, bool) {
	message, err := s.loadMessage(r.Context(), tx, "", r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return model.Message{}, time.Time{}, false
	}
	var now time.Time
	if message.IssueKey != nil {
		if _, err := s.lockIssue(r.Context(), tx, *message.IssueKey); err != nil {
			s.writeHandlerError(w, err)
			return model.Message{}, time.Time{}, false
		}
		if message, err = scanMessage(tx.QueryRow(r.Context(), `
			select `+messageColumns+`, now() from messages where id = $1
		`, message.ID), &now); err != nil {
			s.writeHandlerError(w, err)
			return model.Message{}, time.Time{}, false
		}
	}
	// The author is compared as a writer and not field by field, as the comment edit compares:
	// a person's actor carries the credential that acted beside them (model.ActorAct).
	if !message.Author.SameAs(actor) {
		writeError(w, "NOT_AUTHOR", http.StatusForbidden, "only the message author may "+verb+" it")
		return model.Message{}, time.Time{}, false
	}
	if message.IssueKey == nil {
		writeError(w, "NOT_AN_ISSUE_MESSAGE", http.StatusConflict,
			"only a message on an issue is "+past+": this one belongs to a conversation")
		return model.Message{}, time.Time{}, false
	}
	if message.DeletedAt != nil {
		s.writeHandlerError(w, deletedPostError(refs.ItemMessage))
		return model.Message{}, time.Time{}, false
	}
	return message, now.UTC(), true
}

// editMessage answers PATCH /api/v1/messages/{id}. It writes the body the edit replaces into
// post_body_history, re-indexes the blocks from the new body at the time of the edit, and
// appends message.edited, which carries the new body as comment.edited does.
func (s *server) editMessage(w http.ResponseWriter, r *http.Request) {
	if !requireUUIDPath(w, r, "message") {
		return
	}
	var input editMessageInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.correctionActor(w, r, input.Actor)
	if !ok {
		return
	}
	// An edit stores line feeds alone, as a message's creation does (validateCreateMessage) and as
	// the comment edit does: a body that holds a carriage return draws differently in every reader
	// of the stored text, the message.edited event and its deliveries among them.
	input.Body = pmdoc.LineFeeds(input.Body)
	if strings.TrimSpace(input.Body) == "" {
		writeError(w, "INVALID_MESSAGE", http.StatusBadRequest, "message body is required")
		return
	}
	if err := blockBoundError(itemblocks.Check("body", input.Body)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	message, editedAt, ok := s.lockedMessageForCorrection(w, r, tx, actor, "edit", "edited")
	if !ok {
		return
	}
	previousBody := message.Body
	history, err := postBodyHistoryStep(refs.ItemMessage, message.ID, previousBody, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	message.Body = input.Body
	message.EditedAt = &editedAt
	// The blocks of the new body are indexed at the time of the edit, as an edited comment's
	// are, which settles the index messages_item_blocks_owed owes on this update (migration
	// 20261110224508). A message keeps no per-revision block text, so the history row above is
	// the one copy served as the body this edit replaced; the message's earlier events and the
	// excerpt kept of a block this edit removed still hold parts of it. The update is queued
	// below, so the index is planned from the body it writes, as the comment edit's is
	// (planEditedItem).
	referenceChanges, index, err := s.planEditedItem(
		r.Context(), tx, message.Author, refs.ItemMessage, message.ID, previousBody, message.Body, editedAt, nil)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	correction, logins, err := s.messageCorrectionEvent(r.Context(), tx, &message, "message.edited", actor, referenceChanges)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The answer lists the attempts as a read of the message lists them: the edit writes none,
	// so they are read here, in the read phase, and a read that fails writes nothing.
	if err := s.hydrateWrittenMessage(r.Context(), tx, &message); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The history row, the update, the index, the event and its stamp go in one batch: the
	// event's id is the one the stamp names as the id sequence left it (refs.QueueStamp).
	var event model.Event
	if err := store.RunWrite(r.Context(), tx, func(b *store.WriteBatch) error {
		if err := history(b); err != nil {
			return err
		}
		b.Queue("", `
			update messages
			set body = $2, edited_at = $3
			where id = $1
		`, message.ID, message.Body, editedAt)
		if err := index(b); err != nil {
			return err
		}
		return s.queueLoggedEventOn(b, correction, logins, "message", message.ID, func(appended model.Event) { event = appended })
	}); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), event)
	WriteJSON(w, http.StatusOK, message)
}

// deleteMessage answers DELETE /api/v1/messages/{id}: the author withdraws its own message. The
// row and its place in the thread stay; the body goes, and with it each copy a read serves as the
// message's own text that does not follow the cleared column — the block index and each block's
// stored excerpt — in the one transaction that clears it. The event log's payloads stay as
// written and are masked at each read (attachPostMarks), and a reply's events that quote the
// message - its creation, and an edit of the reply written before the delete - keep the preview
// they froze. Each reply keeps its own text and its own place. The delete's own message.deleted,
// of a message that is itself a reply, carries no text of the post it answers
// (messageCorrectionEvent).
func (s *server) deleteMessage(w http.ResponseWriter, r *http.Request) {
	if !requireUUIDPath(w, r, "message") {
		return
	}
	var input actorInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	actor, ok := s.correctionActor(w, r, input.Actor)
	if !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	message, deletedAt, ok := s.lockedMessageForCorrection(w, r, tx, actor, "delete", "deleted")
	if !ok {
		return
	}
	referenceChanges, clear, err := s.clearDeletedPostBody(
		r.Context(), tx, refs.ItemMessage, message.ID, message.Body, false, message.Author, actor, deletedAt)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	message.Body = ""
	message.DeletedAt = &deletedAt
	message.DeletedBy = &actor
	correction, logins, err := s.messageCorrectionEvent(r.Context(), tx, &message, "message.deleted", actor, referenceChanges)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The answer is read here, in the read phase, with the attempts as they stand: the settle is
	// queued below, so the attempts it settles are set in the answer as it leaves them, and each
	// one's receipt is built from that. The settle refuses should a row no longer stand as this
	// read found it (queueDeletedPostSettlement).
	if err := s.hydrateWrittenMessage(r.Context(), tx, &message); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var pending []model.MessageDelivery
	var receipts []model.Event
	for index := range message.Deliveries {
		attempt := &message.Deliveries[index]
		if attempt.State != model.DeliveryStatePending {
			continue
		}
		pending = append(pending, *attempt)
		attempt.State, attempt.Error = deliveryOutcome(deletedPostDeliveryError, false)
		receipts = append(receipts, messageDeliveryReceipt(message, message.Author, *attempt, ""))
	}
	// The clear, the settle, its receipts, the event and its stamp go in one batch, in the order
	// the delete makes them.
	appended := make([]model.Event, 0, len(receipts)+1)
	collect := func(event model.Event) { appended = append(appended, event) }
	if err := store.RunWrite(r.Context(), tx, func(b *store.WriteBatch) error {
		if err := clear(b); err != nil {
			return err
		}
		queueDeletedPostSettlement(b, message.ID, messageAttemptsRead(pending))
		for _, receipt := range receipts {
			if err := s.deps.Events.QueueAppend(b, receipt, collect); err != nil {
				return err
			}
		}
		return s.queueLoggedEventOn(b, correction, logins, "message", message.ID, collect)
	}); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), appended...)
	WriteJSON(w, http.StatusOK, message)
}

// messageCorrectionEvent is the event an edit or a delete of message owes, built in the
// correction's read phase: the payload every write of a message builds, so an edit's and a
// delete's say the same thing about the thread as the create's did, and the thread_logins of the
// message's thread root, which the event's insert reads (events.ThreadLoginsOf), as the message
// create's does. The message is an issue message: a correction refuses any other
// (lockedMessageForCorrection).
//
// A reply's message.edited carries the preview of the post it answers as that post reads when the
// edit is written - reply_body, and reply_block with its text - as its create carried the preview
// of the post as it read then. Its message.deleted carries no text at all: the deleted message's
// body is already cleared in the message this payload is built from, and of the post the reply
// answers it carries no reply_body and the reply_block with every field but its text
// (deleteEventReplyBlock). It keeps every field that routes it: in_reply_to, thread_target, the
// block's node id and id, and the thread_logins. The payload reaches every subscriber and every
// reader of the conversation, so the block is read as everyReader, as the create's is
// (finishPendingMessageDelivery).
func (s *server) messageCorrectionEvent(
	ctx context.Context,
	tx pgx.Tx,
	message *model.Message,
	eventType string,
	actor model.Actor,
	changes model.ReferenceChanges,
) (model.Event, *events.Logins, error) {
	if err := hydrateMessageReplyBlocks(ctx, tx, everyReader, message); err != nil {
		return model.Event{}, nil, err
	}
	thread, err := loadMessageReplyThread(ctx, tx, *message)
	if err != nil {
		return model.Event{}, nil, err
	}
	root, err := messageThreadRoot(ctx, tx, message.ID, message.InReplyTo)
	if err != nil {
		return model.Event{}, nil, err
	}
	payload := thread.payload(*message, changes)
	if message.DeletedAt != nil {
		payload.ReplyBody = ""
		payload.ReplyBlock = deleteEventReplyBlock(payload.ReplyBlock)
	}
	return messageEvent(*message, eventType, actor, payload), events.ThreadLoginsOf(root), nil
}
