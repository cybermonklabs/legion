// post_history.go is what a write of a comment's or an issue message's body does to the body it
// replaces: the one write that keeps it (recordPostBodyHistory), the clear both deletes make of
// it and of the sends still carrying it (clearDeletedPostBody, settleDeletedPostDeliveries), and
// the two reads that serve the kept bodies, GET /api/v1/comments/{id}/history and GET
// /api/v1/messages/{id}/history. One table holds the kept bodies of both kinds, keyed by the one
// of comment_id and message_id its row carries (post_body_history, migration 20261110224508).
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// postHistoryEntry is one body a write replaced, with the actor that replaced it and the time
// of that write. Body is empty on a deleted post, whose read gives no part of the body
// anywhere, this one included.
type postHistoryEntry struct {
	Body     string      `json:"body"`
	EditedBy model.Actor `json:"edited_by"`
	EditedAt time.Time   `json:"edited_at"`
}

// postHistoryRead is what the two history reads answer: the post's earlier bodies, newest
// first, and the deleted mark the post carries, as every other read of it carries it. Entries
// holds at most maxEventPage rows; the page after is the same read under ?before= the oldest
// edited_at this one gave.
type postHistoryRead struct {
	DeletedAt *time.Time         `json:"deleted_at"`
	DeletedBy *model.Actor       `json:"deleted_by"`
	Entries   []postHistoryEntry `json:"entries"`
}

// postHistoryOwner is where a post of this kind is kept: the table the post itself is in and
// the post_body_history column naming it, in the order refs gives them, which owns the map of
// an item kind to its storage (refs.ItemKind.ItemTable): post_body_history keys a post by the
// same column every table of that post's blocks keys it by. Only a comment and a message keep a
// history, so an ask, which is an item kind of its own, is refused here rather than there. kind
// is the handler's own constant and never caller input, so a kind with no history is this
// server's own fault rather than a refusal a caller can provoke.
func postHistoryOwner(kind refs.ItemKind) (items, column string, err error) {
	if kind != refs.ItemComment && kind != refs.ItemMessage {
		return "", "", fmt.Errorf("read the body history of a %q: only a comment and a message keep one", string(kind))
	}
	return kind.ItemTable()
}

// recordPostBodyHistory keeps the body a write of comment or message itemID is about to
// replace, in the caller's transaction and under the locks that caller already holds: one
// post_body_history row with the actor making the write. An edit calls it with the body it
// replaces and a delete with the body it clears; a create calls it never, so the history of a
// post nobody edited is empty.
func recordPostBodyHistory(ctx context.Context, tx pgx.Tx, kind refs.ItemKind, itemID, body string, actor model.Actor) error {
	_, column, err := postHistoryOwner(kind)
	if err != nil {
		return err
	}
	editedBy, err := encodeJSON(actor)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into post_body_history (`+column+`, body, edited_by) values ($1, $2, $3)
	`, itemID, body, editedBy); err != nil {
		return fmt.Errorf("keep the replaced body of %s %s: %w", kind, itemID, err)
	}
	return nil
}

// deletedPostDeliveryError is what a send of a deleted post still pending is settled with: the
// text that would have been sent is gone, so nothing can send it. It is written in the shape
// settleUndeliverableMention writes its two causes in (delivery_sweep.go).
const deletedPostDeliveryError = "not sent: the post was deleted"

// clearDeletedPostBody is the clear a delete of either kind makes, in the caller's transaction
// and under the locks that caller already holds: the history row keeping the body about to go
// (recordPostBodyHistory), the one update that empties the body and stamps the delete on the
// row, and the copies of the body that do not follow the cleared column — the block index, each
// block's stored excerpt and the text each revision recorded of a block — taken out of them
// (refs.TombstoneItem). It returns the time the delete carries, in UTC as every time this API
// answers one, and the reference changes the delete's event carries.
//
// A comment clears its suggestion with its body, since the suggestion's replacement text is the
// comment's text too, and a message carries no suggestion: the kind decides which, and the clear
// rides the one update, so the kind's search trigger rewrites the row's vector once from the
// cleared body and the deleted post stops matching a search.
func (s *server) clearDeletedPostBody(
	ctx context.Context,
	tx pgx.Tx,
	kind refs.ItemKind,
	itemID, previousBody string,
	actor model.Actor,
) (time.Time, model.ReferenceChanges, error) {
	items, _, err := postHistoryOwner(kind)
	if err != nil {
		return time.Time{}, model.ReferenceChanges{}, err
	}
	alsoClears := ""
	if kind == refs.ItemComment {
		alsoClears = ", suggestion = null"
	}
	if err := recordPostBodyHistory(ctx, tx, kind, itemID, previousBody, actor); err != nil {
		return time.Time{}, model.ReferenceChanges{}, err
	}
	deletedBy, err := encodeJSON(actor)
	if err != nil {
		return time.Time{}, model.ReferenceChanges{}, err
	}
	var deletedAt time.Time
	if err := tx.QueryRow(ctx, `
		update `+items+`
		set body = ''`+alsoClears+`, deleted_at = now(), deleted_by = $2
		where id = $1
		returning deleted_at
	`, itemID, deletedBy).Scan(&deletedAt); err != nil {
		return time.Time{}, model.ReferenceChanges{}, err
	}
	deletedAt = deletedAt.UTC()
	changes, err := refs.TombstoneItem(ctx, tx, kind, itemID, previousBody, s.deps.ServerURL, deletedAt)
	if err != nil {
		return time.Time{}, model.ReferenceChanges{}, err
	}
	return deletedAt, changes, nil
}

// settleDeletedPostDeliveries fails every send of the deleted post still pending and returns the
// delivery receipt each settle owes, appended but not yet published. A delivery Dispatch already
// sent keeps what it carried; one still waiting would send the text after the delete, so it is
// settled here rather than left to the delivery sweep. claimed_at moves with the settle, as
// every other settle of an attempt moves it: it is how a sender still holding the old claim
// learns the row is no longer its own.
//
// attempts is the table of the post's send attempts and owner the column naming the post in it,
// both the caller's own constants, and columns is the select list receipt reads a settled row
// by. Every settled row is read before the first receipt is appended, since that append is
// another query of this transaction.
func (s *server) settleDeletedPostDeliveries(
	ctx context.Context,
	tx pgx.Tx,
	attempts, owner, columns, itemID string,
	receipt func(row pgx.CollectableRow) (model.Event, error),
) ([]model.Event, error) {
	rows, err := tx.Query(ctx, `
		update `+attempts+` set state = 'failed', error = $2, claimed_at = now()
		where `+owner+` = $1 and state = 'pending'
		returning `+columns,
		itemID, deletedPostDeliveryError)
	if err != nil {
		return nil, err
	}
	owed, err := pgx.CollectRows(rows, receipt)
	if err != nil {
		return nil, err
	}
	events := make([]model.Event, 0, len(owed))
	for _, settled := range owed {
		appended, err := s.appendEvent(ctx, tx, settled)
		if err != nil {
			return nil, err
		}
		events = append(events, appended)
	}
	return events, nil
}

// getCommentHistory answers GET /api/v1/comments/{id}/history, read by whoever GET
// /api/v1/comments/{id} answers: any authenticated caller.
func (s *server) getCommentHistory(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !requireUUIDPath(w, r, "comment") {
		return
	}
	s.writePostHistory(w, r, refs.ItemComment)
}

// getMessageHistory answers GET /api/v1/messages/{id}/history under the read rule of GET
// /api/v1/messages/{id} (readableMessageThread), as the message's blocks read is. Only an issue
// message is edited or deleted, and that rule asks for a session on a thread with no issue
// alone, so it costs this read nothing.
func (s *server) getMessageHistory(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.readableMessageThread(w, r); !ok {
		return
	}
	s.writePostHistory(w, r, refs.ItemMessage)
}

func (s *server) writePostHistory(w http.ResponseWriter, r *http.Request, kind refs.ItemKind) {
	before, err := parseOptionalRFC3339(r.URL.Query(), "before", time.Time{})
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	read, err := s.postHistory(r.Context(), kind, r.PathValue("id"), before)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, read)
}

// postHistory is one post's kept bodies, newest first, at most maxEventPage of them and only
// those written before `before` when it is given. The order is the partial index's
// (post_body_history_comment, post_body_history_message), so the read sorts nothing, and
// edited_at is clock_timestamp(), so two writes of one transaction order by it with the
// identity breaking a tie within one reading.
//
// The post's deleted mark and its kept bodies are one statement, so both are read in one
// snapshot: as two statements a delete that committed between them served the bodies the
// first read found with no mark on them, which is the one answer this read must never give.
// The post itself drives the join, so a post that is not there answers no row at all, as the
// single read of it did (pgx.ErrNoRows, 404); the lateral is its page of kept bodies, read
// through the same partial index under the same order and limit, and a post that kept none
// answers one row with every history column null.
func (s *server) postHistory(ctx context.Context, kind refs.ItemKind, id string, before time.Time) (postHistoryRead, error) {
	items, column, err := postHistoryOwner(kind)
	if err != nil {
		return postHistoryRead{}, err
	}
	var cursor *time.Time
	if !before.IsZero() {
		cursor = &before
	}
	// A delete gives no part of the body on any read of the post, so the mark stands in place of
	// every body this list would otherwise serve and the read answers none of them.
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select p.deleted_at, p.deleted_by,
		       case when p.deleted_at is null then h.body else '' end, h.edited_by, h.edited_at
		from `+items+` p
		left join lateral (
			select body, edited_by, edited_at
			from post_body_history
			where `+column+` = p.id and ($2::timestamptz is null or edited_at < $2)
			order by edited_at desc, id desc
			limit $3
		) h on true
		where p.id = $1
	`, id, cursor, maxEventPage)
	if err != nil {
		return postHistoryRead{}, fmt.Errorf("read the body history of %s %s: %w", kind, id, err)
	}
	defer rows.Close()
	read := postHistoryRead{Entries: []postHistoryEntry{}}
	found := false
	for rows.Next() {
		var deletedAt, editedAt *time.Time
		var deletedBy, editedBy []byte
		var body *string
		if err := rows.Scan(&deletedAt, &deletedBy, &body, &editedBy, &editedAt); err != nil {
			return postHistoryRead{}, fmt.Errorf("scan a kept body of %s %s: %w", kind, id, err)
		}
		if !found {
			found = true
			if read.DeletedAt, read.DeletedBy, err = model.DeletedPostMark(deletedAt, deletedBy); err != nil {
				return postHistoryRead{}, err
			}
		}
		// The post kept no body this page holds: the left join's one row carries the mark alone.
		if editedAt == nil {
			continue
		}
		entry := postHistoryEntry{Body: *body, EditedAt: editedAt.UTC()}
		if err := json.Unmarshal(editedBy, &entry.EditedBy); err != nil {
			return postHistoryRead{}, fmt.Errorf("decode the actor of a kept body of %s %s: %w", kind, id, err)
		}
		read.Entries = append(read.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return postHistoryRead{}, fmt.Errorf("read the body history of %s %s: %w", kind, id, err)
	}
	if !found {
		return postHistoryRead{}, pgx.ErrNoRows
	}
	return read, nil
}
