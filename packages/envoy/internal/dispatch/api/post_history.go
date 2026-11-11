// post_history.go is the body an edit or a delete of a comment or an issue message replaced:
// the one write that keeps it (recordPostBodyHistory), and the two reads that serve it, GET
// /api/v1/comments/{id}/history and GET /api/v1/messages/{id}/history. One table holds both
// kinds, keyed by the one of comment_id and message_id its row carries
// (post_body_history, migration 20261110224508).
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

// postHistoryOwner is the post_body_history column naming a post of this kind, and the table
// the post itself is in. kind is the handler's own constant and never caller input, so a kind
// with no history is this server's own fault rather than a refusal a caller can provoke.
func postHistoryOwner(kind refs.ItemKind) (column, items string, err error) {
	switch kind {
	case refs.ItemComment:
		return "comment_id", "comments", nil
	case refs.ItemMessage:
		return "message_id", "messages", nil
	}
	return "", "", fmt.Errorf("read the body history of a %q: only a comment and a message keep one", string(kind))
}

// recordPostBodyHistory keeps the body a write of comment or message itemID is about to
// replace, in the caller's transaction and under the locks that caller already holds: one
// post_body_history row with the actor making the write. An edit calls it with the body it
// replaces and a delete with the body it clears; a create calls it never, so the history of a
// post nobody edited is empty.
func recordPostBodyHistory(ctx context.Context, tx pgx.Tx, kind refs.ItemKind, itemID, body string, actor model.Actor) error {
	column, _, err := postHistoryOwner(kind)
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
func (s *server) postHistory(ctx context.Context, kind refs.ItemKind, id string, before time.Time) (postHistoryRead, error) {
	column, items, err := postHistoryOwner(kind)
	if err != nil {
		return postHistoryRead{}, err
	}
	read := postHistoryRead{Entries: []postHistoryEntry{}}
	var deletedAt *time.Time
	var deletedBy []byte
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select deleted_at, deleted_by from `+items+` where id = $1
	`, id).Scan(&deletedAt, &deletedBy); err != nil {
		return postHistoryRead{}, err
	}
	read.DeletedAt, read.DeletedBy, err = deletedPostMark(deletedAt, deletedBy)
	if err != nil {
		return postHistoryRead{}, err
	}
	var cursor *time.Time
	if !before.IsZero() {
		cursor = &before
	}
	// A delete gives no part of the body on any read of the post, so the mark stands in place of
	// every body this list would otherwise serve and the read fetches none of them.
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select case when $4::boolean then '' else body end, edited_by, edited_at
		from post_body_history
		where `+column+` = $1 and ($2::timestamptz is null or edited_at < $2)
		order by edited_at desc, id desc
		limit $3
	`, id, cursor, maxEventPage, read.DeletedAt != nil)
	if err != nil {
		return postHistoryRead{}, fmt.Errorf("read the body history of %s %s: %w", kind, id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry postHistoryEntry
		var editedBy []byte
		if err := rows.Scan(&entry.Body, &editedBy, &entry.EditedAt); err != nil {
			return postHistoryRead{}, fmt.Errorf("scan a kept body of %s %s: %w", kind, id, err)
		}
		if err := json.Unmarshal(editedBy, &entry.EditedBy); err != nil {
			return postHistoryRead{}, fmt.Errorf("decode the actor of a kept body of %s %s: %w", kind, id, err)
		}
		entry.EditedAt = entry.EditedAt.UTC()
		read.Entries = append(read.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return postHistoryRead{}, fmt.Errorf("read the body history of %s %s: %w", kind, id, err)
	}
	return read, nil
}
