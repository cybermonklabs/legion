package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// createSessionIssueMessage posts an issue message whose author is the session sessionActor()
// names, which is the author every correction below is made as.
func createSessionIssueMessage(t *testing.T, handler http.Handler, issueKey, body string) model.Message {
	t.Helper()
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/messages", map[string]any{
		"actor": sessionActor(), "body": body,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeBody[model.Message](t, created)
}

// editIssueMessage edits the message as login, or as the session sessionActor() names when login
// is empty, and fails t on anything but 200.
func editIssueMessage(t *testing.T, handler http.Handler, id, body, login string) model.Message {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/messages/"+id, map[string]any{"body": body}, login)
	if login == "" {
		response = sessionRequest(t, handler, http.MethodPatch, "/api/v1/messages/"+id, map[string]any{
			"actor": sessionActor(), "body": body,
		})
	}
	if response.Code != http.StatusOK {
		t.Fatalf("edit message %s: status=%d body=%s", id, response.Code, response.Body.String())
	}
	return decodeBody[model.Message](t, response)
}

// deleteIssueMessage deletes the message as login, or as the session sessionActor() names when
// login is empty, and fails t on anything but 200.
func deleteIssueMessage(t *testing.T, handler http.Handler, id, login string) model.Message {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/messages/"+id, nil, login)
	if login == "" {
		response = sessionRequest(t, handler, http.MethodDelete, "/api/v1/messages/"+id, map[string]any{
			"actor": sessionActor(),
		})
	}
	if response.Code != http.StatusOK {
		t.Fatalf("delete message %s: status=%d body=%s", id, response.Code, response.Body.String())
	}
	return decodeBody[model.Message](t, response)
}

// seedConversationMessage stores a message of a conversation with no issue, targeted at target
// ("session:<id>"): the one kind of message these two routes refuse. It is written straight to
// the table because the route that opens such a conversation
// (POST /api/v1/agents/{session_id}/messages) sends it to a live agent listener, which what the
// routes do with it has nothing to do with.
func seedConversationMessage(t *testing.T, database *store.Store, author model.Actor, body, target string) string {
	t.Helper()
	encoded, err := json.Marshal(author)
	if err != nil {
		t.Fatalf("encode the author: %v", err)
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into messages (issue_key, author, body, target)
		values (null, $1::jsonb, $2, $3)
		returning id::text
	`, encoded, body, target).Scan(&id); err != nil {
		t.Fatalf("seed a message of a conversation: %v", err)
	}
	return id
}

// messageBlockRows is each message_blocks row as this file reads it, in the shape
// commentBlockRows reads a comment's: whether the block is still in the body, and the excerpt a
// reply read would serve as its text.
func messageBlockRows(t *testing.T, database *store.Store, messageID string) []commentBlockRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select position is not null, excerpt from message_blocks
		where message_id = $1 order by block_id
	`, messageID)
	if err != nil {
		t.Fatalf("read the blocks of %s: %v", messageID, err)
	}
	defer rows.Close()
	var blocks []commentBlockRow
	for rows.Next() {
		var block commentBlockRow
		if err := rows.Scan(&block.current, &block.excerpt); err != nil {
			t.Fatalf("scan a block of %s: %v", messageID, err)
		}
		blocks = append(blocks, block)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of %s: %v", messageID, err)
	}
	return blocks
}

// messageBlockState is the message's block-index state: whether an index is owed, and the sum of
// the body the blocks it holds were split from.
func messageBlockState(t *testing.T, database *store.Store, messageID string) (owed bool, sum string) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select owed_at is not null, coalesce(body_sha256, '') from message_block_state where message_id = $1
	`, messageID).Scan(&owed, &sum); err != nil {
		t.Fatalf("read the block state of %s: %v", messageID, err)
	}
	return owed, sum
}

// The refusals are in one order on this write: the author first, so a caller that is not the
// author learns nothing else about the post; then the kind, since only a message on an issue is
// edited; then a post already deleted.
func TestEditMessageIsAuthorOnlyAndOnlyOnAnIssue(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Edit a message", "before")
	message := createSessionIssueMessage(t, handler, issue.Key, "the session's first body")

	person := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/messages/"+message.ID,
		map[string]any{"body": "a person's edit"}, "alice")
	if person.Code != http.StatusForbidden || !strings.Contains(person.Body.String(), `"code":"NOT_AUTHOR"`) {
		t.Fatalf("a person edits a session's message: status=%d body=%s, want 403 NOT_AUTHOR", person.Code, person.Body.String())
	}
	other := sessionRequest(t, handler, http.MethodPatch, "/api/v1/messages/"+message.ID, map[string]any{
		"body": "another session's edit", "actor": map[string]any{"kind": "session", "id": "session-fedcba9876543210"},
	})
	if other.Code != http.StatusForbidden || !strings.Contains(other.Body.String(), `"code":"NOT_AUTHOR"`) {
		t.Fatalf("another session edits it: status=%d body=%s, want 403 NOT_AUTHOR", other.Code, other.Body.String())
	}

	edited := editIssueMessage(t, handler, message.ID, "the session's second body", "")
	if edited.Body != "the session's second body" || edited.EditedAt == nil {
		t.Fatalf("the author's edit answered %#v, want the new body and edited_at", edited)
	}

	// A person edits their own message through the same route, with no actor in the body.
	own := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "the person's first body"}, "alice")
	if theirs := editIssueMessage(t, handler, own.ID, "the person's second body", "alice"); theirs.Body != "the person's second body" {
		t.Fatalf("the person's edit answered %#v, want their new body", theirs)
	}

	// The kind is refused after the author: a caller that is not the author of an issue-less
	// message is told NOT_AUTHOR and learns nothing about the message.
	conversation := seedConversationMessage(t, database,
		model.Actor{Kind: "user", ID: "alice"}, "a message of a conversation", "session:s1")
	stranger := sessionRequest(t, handler, http.MethodPatch, "/api/v1/messages/"+conversation, map[string]any{
		"body": "an edit of another author's message", "actor": sessionActor(),
	})
	if stranger.Code != http.StatusForbidden || !strings.Contains(stranger.Body.String(), `"code":"NOT_AUTHOR"`) {
		t.Fatalf("a session edits a person's conversation message: status=%d body=%s, want 403 NOT_AUTHOR",
			stranger.Code, stranger.Body.String())
	}
	refused := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/messages/"+conversation,
		map[string]any{"body": "an edit of a conversation message"}, "alice")
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"code":"NOT_AN_ISSUE_MESSAGE"`) {
		t.Fatalf("the author edits a conversation message: status=%d body=%s, want 409 NOT_AN_ISSUE_MESSAGE",
			refused.Code, refused.Body.String())
	}
	var held string
	if err := database.Pool.QueryRow(context.Background(),
		`select body from messages where id = $1`, conversation).Scan(&held); err != nil {
		t.Fatalf("read the conversation message: %v", err)
	}
	if held != "a message of a conversation" {
		t.Fatalf("the conversation message after the refused edits = %q, want it untouched", held)
	}
}

// The same order on the delete, with the second delete refused: a delete is final.
func TestDeleteMessageIsAuthorOnlyAndOnlyOnAnIssueAndFinal(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete a message", "before")
	message := createSessionIssueMessage(t, handler, issue.Key, "the withdrawn claim")

	person := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/messages/"+message.ID, nil, "alice")
	if person.Code != http.StatusForbidden || !strings.Contains(person.Body.String(), `"code":"NOT_AUTHOR"`) {
		t.Fatalf("a person deletes a session's message: status=%d body=%s, want 403 NOT_AUTHOR", person.Code, person.Body.String())
	}
	other := sessionRequest(t, handler, http.MethodDelete, "/api/v1/messages/"+message.ID, map[string]any{
		"actor": map[string]any{"kind": "session", "id": "session-fedcba9876543210"},
	})
	if other.Code != http.StatusForbidden || !strings.Contains(other.Body.String(), `"code":"NOT_AUTHOR"`) {
		t.Fatalf("another session deletes it: status=%d body=%s, want 403 NOT_AUTHOR", other.Code, other.Body.String())
	}

	conversation := seedConversationMessage(t, database,
		model.Actor{Kind: "user", ID: "alice"}, "a message of a conversation", "session:s1")
	refused := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/messages/"+conversation, nil, "alice")
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"code":"NOT_AN_ISSUE_MESSAGE"`) {
		t.Fatalf("the author deletes a conversation message: status=%d body=%s, want 409 NOT_AN_ISSUE_MESSAGE",
			refused.Code, refused.Body.String())
	}

	deleted := deleteIssueMessage(t, handler, message.ID, "")
	if deleted.Body != "" || deleted.DeletedAt == nil || deleted.DeletedBy == nil || deleted.DeletedBy.ID != "session-0123456789abcdef" {
		t.Fatalf("the delete answered %#v, want an empty body with the mark", deleted)
	}
	again := sessionRequest(t, handler, http.MethodDelete, "/api/v1/messages/"+message.ID, map[string]any{
		"actor": sessionActor(),
	})
	if again.Code != http.StatusConflict || !strings.Contains(again.Body.String(), `"code":"POST_DELETED"`) {
		t.Fatalf("a second delete: status=%d body=%s, want 409 POST_DELETED", again.Code, again.Body.String())
	}
}

// A read of the message gives the new body and the time of the edit, and the event the edit
// appended carries the new body, as comment.edited does.
func TestEditMessageReadsBackTheNewBodyAndTheTimeOfTheEdit(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "An edited message reads", "before")
	message := createSessionIssueMessage(t, handler, issue.Key, "the first body")
	if message.EditedAt != nil {
		t.Fatalf("a message nobody edited carries edited_at %v", message.EditedAt)
	}

	edited := editIssueMessage(t, handler, message.ID, "the corrected body", "")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/messages/"+message.ID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the message: status=%d body=%s", response.Code, response.Body.String())
	}
	read := decodeBody[messageRead](t, response)
	if read.Message.Body != "the corrected body" {
		t.Fatalf("the message reads %q, want the corrected body", read.Message.Body)
	}
	if read.Message.EditedAt == nil || !read.Message.EditedAt.Equal(*edited.EditedAt) {
		t.Fatalf("the message reads edited_at %v, want the time the edit answered, %v", read.Message.EditedAt, edited.EditedAt)
	}
	if read.Message.EditedAt.Before(read.Message.CreatedAt) {
		t.Fatalf("edited_at %s is before created_at %s", read.Message.EditedAt, read.Message.CreatedAt)
	}

	events := issueEventsOfType(t, handler, issue.Key, "message.edited")
	if len(events) != 1 {
		t.Fatalf("the issue holds %d message.edited events, want the one the edit appended", len(events))
	}
	var payload struct {
		ID   string `json:"id"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("decode the message.edited payload: %v", err)
	}
	if payload.ID != message.ID || payload.Body != "the corrected body" {
		t.Fatalf("the message.edited payload = %#v, want the message with its new body", payload)
	}

	// The history keeps the body the edit replaced, which no read of the message gives.
	history := readPostHistory(t, handler, "/api/v1/messages/"+message.ID+"/history")
	if len(history.Entries) != 1 || history.Entries[0].Body != "the first body" {
		t.Fatalf("the message's history = %#v, want the one body the edit replaced", history.Entries)
	}
}

// The block index follows the body on both writes: an edit re-indexes from the new body, and a
// delete leaves no block in the body with no excerpt a reply read could serve.
func TestMessageBlockIndexFollowsAnEditAndADelete(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "An edited message's blocks", "before")
	message := createSessionIssueMessage(t, handler, issue.Key,
		"The first paragraph of the claim.\n\nThe second paragraph of it.")
	if blocks := messageBlockRows(t, database, message.ID); len(blocks) != 2 {
		t.Fatalf("the message indexed %d blocks, want the body's two", len(blocks))
	}
	if windows := postEmbeddingWindows(t, database, "message/"+message.ID); windows == 0 {
		t.Fatalf("the message holds no embedding window before the delete, so dropping them proves nothing")
	}

	editIssueMessage(t, handler, message.ID, "The one paragraph that replaces both.", "")

	// messages_item_blocks_owed fires on this update of the body (migration 20261110224508) and
	// the edit's own index settles what it owes, in the one transaction.
	if owed, _ := messageBlockState(t, database, message.ID); owed {
		t.Fatalf("the message is still owed a block index after its edit")
	}
	current := 0
	for _, block := range messageBlockRows(t, database, message.ID) {
		if !block.current {
			continue
		}
		current++
		if block.excerpt != "The one paragraph that replaces both." {
			t.Fatalf("the block in the edited body reads %q, want the new text", block.excerpt)
		}
	}
	if current != 1 {
		t.Fatalf("the edited body holds %d blocks, want the one it splits into", current)
	}

	deleteIssueMessage(t, handler, message.ID, "")

	if owed, _ := messageBlockState(t, database, message.ID); owed {
		t.Fatalf("the message is still owed a block index after its delete")
	}
	for index, block := range messageBlockRows(t, database, message.ID) {
		if block.current {
			t.Fatalf("block %d is still in the body after the delete", index)
		}
		if block.excerpt != "" {
			t.Fatalf("block %d keeps the excerpt %q, which a reply read would serve as its text", index, block.excerpt)
		}
	}
	if events := issueEventsOfType(t, handler, issue.Key, "message.deleted"); len(events) != 1 {
		t.Fatalf("the issue holds %d message.deleted events, want the one the delete appended", len(events))
	}
	// A deleted post is reachable by no meaning search: the re-index with the empty body leaves
	// the message with no block, and an owner left with no block loses every window it had
	// (refs/blocks.go, writeWindows).
	if windows := postEmbeddingWindows(t, database, "message/"+message.ID); windows != 0 {
		t.Fatalf("the deleted message keeps %d embedding windows, want none: a meaning search would reach it", windows)
	}
}

// The blocks of a message whose index is owed are stamped at the edit and not at the creation
// (refs.IndexItemIfOwed): a build before this one updated no message body, so a message owed an
// index at a time of its own could only have been written then; an edited one is read at
// coalesce(edited_at, created_at), as a comment is.
func TestOwedBlocksOfAnEditedMessageAreStampedAtTheEdit(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Owed blocks of an edit", "before")
	message := createSessionIssueMessage(t, handler, issue.Key, "the first body")
	editIssueMessage(t, handler, message.ID, "the corrected body", "")

	// The message is owed an index again, as it would be had the index of the edit not run:
	// the body is the stored one, and nothing about the row moves.
	if _, err := database.Pool.Exec(context.Background(), `
		update message_block_state set owed_at = now(), body_sha256 = null where message_id = $1
	`, message.ID); err != nil {
		t.Fatalf("owe the message a block index: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		delete from message_blocks where message_id = $1
	`, message.ID); err != nil {
		t.Fatalf("drop the blocks of the message: %v", err)
	}

	// A reply naming a block of the message indexes it first (api/reply_blocks.go), which is
	// how an owed message is read by the write that needs its blocks.
	reply := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"actor": sessionActor(), "body": "answering that block", "in_reply_to": message.ID, "in_reply_to_block": "b1",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply naming a block of the message: status=%d body=%s", reply.Code, reply.Body.String())
	}

	var madeAt, createdAt, editedAt time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select b.made_at, m.created_at, m.edited_at
		from message_blocks b join messages m on m.id = b.message_id
		where b.message_id = $1 and b.position is not null
	`, message.ID).Scan(&madeAt, &createdAt, &editedAt); err != nil {
		t.Fatalf("read the re-indexed block: %v", err)
	}
	if !madeAt.Equal(editedAt) {
		t.Fatalf("the block was made at %s, want the time of the edit %s (created_at is %s)", madeAt, editedAt, createdAt)
	}
}

// A send still pending would carry the text after the delete, so the delete fails it with the
// one error that says why, and appends the message.delivery receipt that settle owes.
func TestDeleteMessageFailsEachPendingSend(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete with a pending send", "before")
	message := createSessionIssueMessage(t, handler, issue.Key, "the message a send is waiting on")
	if _, err := database.Pool.Exec(context.Background(), `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state)
		values ($1, 1, 'steer', 's1', 'pending')
	`, message.ID); err != nil {
		t.Fatalf("seed a pending attempt: %v", err)
	}

	deleteIssueMessage(t, handler, message.ID, "")

	var state, failure string
	if err := database.Pool.QueryRow(context.Background(), `
		select state, coalesce(error, '') from message_deliveries where message_id = $1 and attempt = 1
	`, message.ID).Scan(&state, &failure); err != nil {
		t.Fatalf("read the attempt after the delete: %v", err)
	}
	if state != "failed" || failure != deletedPostDeliveryError {
		t.Fatalf("the attempt reads %q / %q, want failed with %q", state, failure, deletedPostDeliveryError)
	}

	receipts := issueEventsOfType(t, handler, issue.Key, "message.delivery")
	if len(receipts) != 1 {
		t.Fatalf("the issue holds %d message.delivery receipts, want the one the settle owes", len(receipts))
	}
	var payload struct {
		MessageID string `json:"message_id"`
		Attempt   int    `json:"attempt"`
		State     string `json:"state"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(receipts[0].Payload, &payload); err != nil {
		t.Fatalf("decode the message.delivery receipt: %v", err)
	}
	if payload.MessageID != message.ID || payload.Attempt != 1 ||
		payload.State != "failed" || payload.Error != deletedPostDeliveryError {
		t.Fatalf("the receipt says %#v, want the failed attempt with its error", payload)
	}
}
