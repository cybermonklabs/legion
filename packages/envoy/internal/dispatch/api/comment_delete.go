// comment_delete.go is DELETE /api/v1/comments/{id}: the author withdraws its own comment. The
// row and its place in the thread stay; the body goes, and with it every copy of the body that
// does not follow the cleared column — the block index, each block's stored excerpt, the text
// each revision recorded of a block, and the document margin mark — each rewritten in the one
// transaction that clears it. A pending suggestion's proof is in the document itself, so the
// same transaction rejects it before the clear.
package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// deletedPostDeliveryError is what a mention attempt of a deleted comment is settled with: the
// text that would have been sent is gone, so nothing can send it. It is written in the shape
// settleUndeliverableMention writes its two causes in (delivery_sweep.go).
const deletedPostDeliveryError = "not sent: the post was deleted"

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
			comment.Anchor.Orphaned = true
			anchor, err := encodeJSON(comment.Anchor)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if _, err := tx.Exec(r.Context(), `update comments set anchor = $2 where id = $1`, comment.ID, anchor); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
	}
	previousBody := comment.Body
	if err := recordPostBodyHistory(r.Context(), tx, refs.ItemComment, comment.ID, previousBody, actor); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	deletedBy, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The comments_search trigger rewrites the row's vector from the cleared body on this
	// update, so the deleted comment stops matching a search and nothing else touches it. The
	// suggestion's replacement text goes with the body: it is the comment's text too.
	var deletedAt time.Time
	if err := tx.QueryRow(r.Context(), `
		update comments
		set body = '', suggestion = null, deleted_at = now(), deleted_by = $2
		where id = $1
		returning deleted_at
	`, comment.ID, deletedBy).Scan(&deletedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	deletedAt = deletedAt.UTC()
	comment.Body = ""
	comment.Suggestion = nil
	comment.DeletedAt = &deletedAt
	comment.DeletedBy = &actor
	// The block index, each block's stored excerpt and the text each revision recorded of a
	// block do not follow the cleared column, so the whole of the comment's text is taken out
	// of them here (refs.TombstoneItem), in this transaction.
	referenceChanges, err := refs.TombstoneItem(
		r.Context(), tx, refs.ItemComment, comment.ID, previousBody, s.deps.ServerURL, deletedAt)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.projectThreadMark(documentCtx, tx, comment, actor); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events, err := s.settleDeletedCommentDeliveries(r.Context(), tx, comment)
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

// settleDeletedCommentDeliveries fails every mention attempt of the comment still pending and
// returns the comment.delivery receipt each settle owes, appended but not yet published. A
// delivery Dispatch already sent keeps what it carried; one still waiting would send the text
// after the delete, so it is settled here rather than left to the delivery sweep.
func (s *server) settleDeletedCommentDeliveries(
	ctx context.Context, tx pgx.Tx, comment model.Comment,
) ([]model.Event, error) {
	rows, err := tx.Query(ctx, `
		update comment_deliveries set state = 'failed', error = $2, claimed_at = now()
		where comment_id = $1 and state = 'pending'
		returning `+commentDeliveryColumns,
		comment.ID, deletedPostDeliveryError)
	if err != nil {
		return nil, err
	}
	failed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.CommentDelivery, error) {
		return scanCommentDelivery(row)
	})
	if err != nil {
		return nil, err
	}
	events := make([]model.Event, 0, len(failed)+1)
	for _, attempt := range failed {
		receipt, err := s.appendEvent(ctx, tx, commentDeliveryReceipt(comment, comment.Author, attempt))
		if err != nil {
			return nil, err
		}
		events = append(events, receipt)
	}
	return events, nil
}
