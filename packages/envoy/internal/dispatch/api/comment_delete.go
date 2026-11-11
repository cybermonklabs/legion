// comment_delete.go is DELETE /api/v1/comments/{id}: the author withdraws its own comment. The
// row and its place in the thread stay; the body goes, and with it every copy of the body that
// does not follow the cleared column — the block index, each block's stored excerpt, and the
// document margin mark — each rewritten in the one transaction that clears it.
package api

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

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
	var input actorInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
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
	artifactName, err := s.commentArtifactName(r.Context(), tx, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
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
	// The block index does not follow the cleared column. An empty body splits into no blocks,
	// so this re-index removes each one: its position is nulled, removed_at is stamped, a null
	// block text is written at the new revision, each block mention goes, and every embedding
	// window of the comment is dropped (refs/blocks.go).
	referenceChanges, err := refs.ReplaceCountedItemEdit(
		r.Context(), tx, refs.ItemComment, comment.ID, previousBody, "", s.deps.ServerURL, deletedAt, nil)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A removal keeps the block's stored excerpt, which is what a reply read serves as the text
	// of the block it answers (refs.ReplyBlocks), so without this clear a reply still prints the
	// deleted text. An ordinary edit keeps the excerpt of a block it removed; the clear belongs
	// to the delete alone.
	if _, err := tx.Exec(r.Context(), `
		update comment_blocks set excerpt = '' where comment_id = $1 and excerpt <> ''
	`, comment.ID); err != nil {
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
