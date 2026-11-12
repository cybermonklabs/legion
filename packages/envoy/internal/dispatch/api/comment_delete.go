// comment_delete.go is DELETE /api/v1/comments/{id}: the author withdraws its own comment. The
// row and its place in the thread stay; the body goes, and with it every copy of the body that
// does not follow the cleared column — the block index, each block's stored excerpt, the text
// each revision recorded of a block, and the document margin mark — each rewritten in the one
// transaction that clears it. A pending suggestion's proof is in the document itself, so the
// same transaction rejects it before the clear.
package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// deleteComment answers DELETE /api/v1/comments/{id}. Its prologue is the comment twin of the
// message delete's (lockedCommentForCorrection): the owner locked without the closed refusal,
// then the author and a post already deleted.
func (s *server) deleteComment(w http.ResponseWriter, r *http.Request) {
	if !requireUUIDPath(w, r, "comment") {
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
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	comment, ok := s.lockedCommentForCorrection(w, r, tx, actor, "delete")
	if !ok {
		return
	}
	// The owner lock above decided this comment is deleted, a closed issue's included, so its
	// anchor's document writes are comment marks (docs.WithCommentMarks): the margin projection
	// below, which a closed issue takes as it takes the delete. A comment carrying a pending
	// suggestion takes the context here, where the create withholds it from one: the reject
	// below settles that mark and the clear takes the suggestion off the row, so the record the
	// projection writes names no pending suggestion, and a new one is what a closed issue
	// refuses.
	documentCtx = docs.WithCommentMarks(documentCtx)
	artifactName, err := s.commentArtifactName(r.Context(), tx, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A pending suggestion's mark is proof in the document that the suggestion stands, and an
	// insert suggestion's text is in the document's own body (docs/reject.go, rejectedInsert).
	// The margin projection below rewrites the mark record and removes neither, and the clear
	// leaves an accept and a reject with nothing to move (409 POST_DELETED), so the delete
	// rejects the suggestion here, as the reject arm of commentAction does. It rejects it as the
	// comment's own anchor writes are made (docs.RejectDeletedCommentSuggestion), under the
	// owner lock this transaction holds: a closed issue still takes the delete of its comment,
	// so its document takes this reject too, where the accept and the reject routes are refused.
	// An orphaned mark is already out of the document: the delete records that on the anchor and
	// stands, where an accept and a reject answer 409.
	if comment.Suggestion != nil && comment.Suggestion.Accepted == nil && comment.Anchor != nil {
		if err := s.deps.Docs.RejectDeletedCommentSuggestion(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, actor); err != nil {
			if !errors.Is(err, docs.ErrAnchorOrphaned) {
				s.writeHandlerError(w, err)
				return
			}
			if err := recordOrphanedCommentAnchor(r.Context(), tx, &comment); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
	}
	deletedAt, referenceChanges, err := s.clearDeletedPostBody(
		r.Context(), tx, refs.ItemComment, comment.ID, comment.Body, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comment.Body = ""
	comment.Suggestion = nil
	comment.DeletedAt = &deletedAt
	comment.DeletedBy = &actor
	if err := s.projectThreadMark(documentCtx, tx, comment, actor); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events, err := s.settleDeletedPostDeliveries(
		r.Context(), tx, "comment_deliveries", "comment_id", commentDeliveryColumns, comment.ID,
		func(row pgx.CollectableRow) (model.Event, error) {
			attempt, err := scanCommentDelivery(row)
			if err != nil {
				return model.Event{}, err
			}
			return commentDeliveryReceipt(comment, comment.Author, attempt), nil
		})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A copy of the comment the outbox held back for a mention it waited on is settled with
	// nothing published: the text that copy carries is withdrawn, and the comment.deleted event
	// below is what each subscriber is told instead (comment_fallback_copies, migration
	// 20261101180039).
	if _, err := tx.Exec(r.Context(), `
		update comment_fallback_copies set settled_at = now(), published = false
		where comment_id = $1 and settled_at is null
	`, comment.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	payload, err := s.commentEventPayload(r.Context(), tx, &comment, artifactName, commentEventThread{}, referenceChanges)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.deleted",
		actor,
		payload,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refs.Stamp(r.Context(), tx, "comment", comment.ID, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(events...)
	WriteJSON(w, http.StatusOK, comment)
}
