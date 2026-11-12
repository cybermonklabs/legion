// message_corrections.go is PATCH /api/v1/messages/{id} and DELETE /api/v1/messages/{id}: the
// author of an issue message replaces its body, or withdraws it. They are the message twins of
// editComment (comment_actions.go) and deleteComment (comment_delete.go), with what a message
// does not have left out: it carries no anchor and so no document margin mark, no suggestion and
// no fallback copy, and its pending sends are rows of message_deliveries.
package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// lockedMessageForCorrection is the prologue both corrections take. It reads the message, locks
// its issue without refusing a closed one (lockIssue) — a wrong post nobody can correct after
// the issue closes is the defect these two routes exist for — re-reads the row under its own
// lock, and applies the refusals in the order every write of this change takes them: the author
// first, so a caller that is not the author learns nothing else about the post; then the kind,
// since only a message on an issue is edited or deleted; then a post already deleted, since a
// delete is final. verb names the write in the author's refusal, and past names it in the kind's:
// "edit" and "edited", "delete" and "deleted". One word with a "d" after it reads as "editd".
//
// The row lock is `for no key update`, the lock every other writer of a messages row takes
// (lockReplyParentMessage): the body is no key column, and a reply's insert takes the row
// `for key share` through its foreign key, which this lock does not block. Two corrections of
// one message still serialise on it.
//
// ok is false once it has written the refusal.
func (s *server) lockedMessageForCorrection(
	w http.ResponseWriter, r *http.Request, tx pgx.Tx, actor model.Actor, verb, past string,
) (model.Message, bool) {
	unlocked, err := s.loadMessage(r.Context(), tx, "", r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return model.Message{}, false
	}
	if unlocked.IssueKey != nil {
		if _, err := s.lockIssue(r.Context(), tx, *unlocked.IssueKey); err != nil {
			s.writeHandlerError(w, err)
			return model.Message{}, false
		}
	}
	message, err := scanMessage(tx.QueryRow(r.Context(), `
		select `+messageColumns+` from messages where id = $1 for no key update
	`, unlocked.ID))
	if err != nil {
		s.writeHandlerError(w, err)
		return model.Message{}, false
	}
	// The author is compared as a writer and not field by field, as the comment edit compares:
	// a person's actor carries the credential that acted beside them (model.ActorAct).
	if !message.Author.SameAs(actor) {
		writeError(w, "NOT_AUTHOR", http.StatusForbidden, "only the message author may "+verb+" it")
		return model.Message{}, false
	}
	if message.IssueKey == nil {
		writeError(w, "NOT_AN_ISSUE_MESSAGE", http.StatusConflict,
			"only a message on an issue is "+past+": this one belongs to a conversation")
		return model.Message{}, false
	}
	if message.DeletedAt != nil {
		s.writeHandlerError(w, deletedPostError(refs.ItemMessage))
		return model.Message{}, false
	}
	return message, true
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
	message, ok := s.lockedMessageForCorrection(w, r, tx, actor, "edit", "edited")
	if !ok {
		return
	}
	previousBody := message.Body
	if err := recordPostBodyHistory(r.Context(), tx, refs.ItemMessage, message.ID, previousBody, actor); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var editedAt time.Time
	if err := tx.QueryRow(r.Context(), `
		update messages
		set body = $2, edited_at = now()
		where id = $1
		returning edited_at
	`, message.ID, input.Body).Scan(&editedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	editedAt = editedAt.UTC()
	message.Body = input.Body
	message.EditedAt = &editedAt
	// The blocks of the new body are indexed at the time of the edit, as an edited comment's
	// are, which settles the index messages_item_blocks_owed owes on this update (migration
	// 20261110224508). A message keeps no per-revision block text, so the body this edit
	// replaced is kept by the history row above and nowhere else.
	referenceChanges, err := refs.ReplaceCountedItemEdit(
		r.Context(), tx, refs.ItemMessage, message.ID, previousBody, message.Body, s.deps.ServerURL, editedAt, nil)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendMessageCorrection(r.Context(), tx, &message, "message.edited", actor, referenceChanges)
	if err != nil {
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
// row and its place in the thread stay; the body goes, and with it each copy of the body that
// does not follow the cleared column — the block index and each block's stored excerpt — in the
// one transaction that clears it. Each reply keeps its own text and its own place.
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
	message, ok := s.lockedMessageForCorrection(w, r, tx, actor, "delete", "deleted")
	if !ok {
		return
	}
	deletedAt, referenceChanges, err := s.clearDeletedPostBody(
		r.Context(), tx, refs.ItemMessage, message.ID, message.Body, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	message.Body = ""
	message.DeletedAt = &deletedAt
	message.DeletedBy = &actor
	events, err := s.settleDeletedPostDeliveries(
		r.Context(), tx, "message_deliveries", "message_id", messageDeliveryColumns, message.ID,
		func(row pgx.CollectableRow) (model.Event, error) {
			attempt, err := scanMessageDelivery(row)
			if err != nil {
				return model.Event{}, err
			}
			return messageDeliveryReceipt(message, message.Author, attempt, ""), nil
		})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendMessageCorrection(r.Context(), tx, &message, "message.deleted", actor, referenceChanges)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), events...)
	WriteJSON(w, http.StatusOK, message)
}

// appendMessageCorrection appends the event an edit or a delete of message owes, stamped on the
// reference graph as the message's own create event is (refs.Stamp), and returns it unpublished.
// The payload is the one every write of a message builds, so an edit's and a delete's say the
// same thing about the thread as the create's did; the deleted message's body is already
// cleared in the row this payload is built from, so the event carries none.
func (s *server) appendMessageCorrection(
	ctx context.Context,
	tx pgx.Tx,
	message *model.Message,
	eventType string,
	actor model.Actor,
	changes model.ReferenceChanges,
) (model.Event, error) {
	if err := hydrateMessageReplyBlocks(ctx, tx, message); err != nil {
		return model.Event{}, err
	}
	thread, err := loadMessageReplyThread(ctx, tx, *message)
	if err != nil {
		return model.Event{}, err
	}
	payload := thread.payload(*message, changes)
	payload.ThreadLogins, err = messageThreadLogins(ctx, tx, *message)
	if err != nil {
		return model.Event{}, err
	}
	event, err := s.appendEvent(ctx, tx, messageEvent(*message, eventType, actor, payload))
	if err != nil {
		return model.Event{}, err
	}
	if err := refs.Stamp(ctx, tx, "message", message.ID, event.ID); err != nil {
		return model.Event{}, err
	}
	return event, nil
}
