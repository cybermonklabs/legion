package api

// The reads that give one line of a post: a deleted post reaches each of them with its mark and
// no body, so a reader meets a post its author withdrew and not an empty line, and an edited post
// reaches each with the time of its edit, so a reader meets a correction as one.

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// GET /api/v1/agents/{session_id}/answers: a comment a person deleted keeps its place in the
// session's list, with no text and the mark.
func TestSessionAnswersMarksADeletedComment(t *testing.T) {
	f := newAnswersFixture(t)
	askID := f.ask(t, answersSession, "Which database backs the queue?")
	reply := f.comment(t, map[string]any{"body": "Use SQLite.", "ask_id": askID}, "alice")
	f.publishOutbox(t)
	deleteThreadComment(t, f.handler, reply, "alice")

	item := f.only(t, answersSession)
	if item.ID != reply || item.Kind != model.SessionAnswerKindComment {
		t.Fatalf("item = %+v, want the deleted comment %s in its place", item, reply)
	}
	if item.Text != "" {
		t.Fatalf("item text = %q, want no part of the deleted body", item.Text)
	}
	if item.DeletedAt == nil || item.DeletedBy == nil || item.DeletedBy.ID != "alice" {
		t.Fatalf("item mark = %v by %v, want the time of the delete and the actor that made it", item.DeletedAt, item.DeletedBy)
	}
}

// The message arm of the same read carries the time of an edit, which it did not before this
// change, and the mark of a delete.
func TestSessionAnswersCarriesAMessagesEditTimeAndItsDeletedMark(t *testing.T) {
	f := newAnswersFixture(t)
	root := f.message(t, map[string]any{"body": "The deploy is blocked on review."}, answersSession)
	reply := f.message(t, map[string]any{"body": "I will review it now.", "in_reply_to": root}, "alice")
	f.publishOutbox(t)

	editIssueMessage(t, f.handler, reply, "I am reviewing it now.", "alice")
	item := f.only(t, answersSession)
	if item.ID != reply || item.Text != "I am reviewing it now." {
		t.Fatalf("item = %+v, want the message's current body", item)
	}
	if item.EditedAt == nil || item.EditedAt.Before(item.At) {
		t.Fatalf("item edited_at = %v, want the edit's time, after %s", item.EditedAt, item.At)
	}
	if item.DeletedAt != nil {
		t.Fatalf("item deleted_at = %v on a message that stands", item.DeletedAt)
	}

	deleteIssueMessage(t, f.handler, reply, "alice")
	deleted := f.only(t, answersSession)
	if deleted.ID != reply || deleted.Text != "" {
		t.Fatalf("item = %+v, want the deleted message in its place with no part of its body", deleted)
	}
	if deleted.DeletedAt == nil || deleted.DeletedBy == nil || deleted.DeletedBy.ID != "alice" {
		t.Fatalf("item mark = %v by %v, want the time of the delete and the actor that made it", deleted.DeletedAt, deleted.DeletedBy)
	}
}

// myAnswersWithMarkRead is GET /api/v1/me/answers as this file reads it: the reply of each row
// with the time of its edit and the mark a delete leaves on it.
type myAnswersWithMarkRead struct {
	Rows []struct {
		Kind  string `json:"kind"`
		Reply *struct {
			ID        string       `json:"id"`
			Body      string       `json:"body"`
			EditedAt  *time.Time   `json:"edited_at"`
			DeletedAt *time.Time   `json:"deleted_at"`
			DeletedBy *model.Actor `json:"deleted_by"`
		} `json:"reply"`
	} `json:"rows"`
	Total int `json:"total"`
}

// editThreadComment edits the comment as login, fails t on anything but 200, and answers the
// time of the edit, as the edited comment carries it.
func editThreadComment(t *testing.T, handler http.Handler, id, body, login string) time.Time {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+id, map[string]any{"body": body}, login)
	if response.Code != http.StatusOK {
		t.Fatalf("edit comment %s: status=%d body=%s", id, response.Code, response.Body.String())
	}
	edited := decodeBody[model.Comment](t, response)
	if edited.EditedAt == nil {
		t.Fatalf("the edit of comment %s answered no edited_at: %#v", id, edited)
	}
	at, err := time.Parse(time.RFC3339Nano, *edited.EditedAt)
	if err != nil {
		t.Fatalf("parse the edit time %q of comment %s: %v", *edited.EditedAt, id, err)
	}
	return at
}

// GET /api/v1/me/answers: the caller's own reply on an ask carries no edit time while it stands
// as first written, and the time of the edit once the caller edited it.
func TestMyAnswersCarriesAnEditedReplysEditTime(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "My answers and an edited reply", "A spec")
	ask := createAnswerListAsk(t, handler, issue.Key, "Which day do we ship?")
	reply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Friday, if review is done.", "ask_id": ask.ID,
	}, "alice")

	read := func() myAnswersWithMarkRead {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/answers", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("list my answers: status=%d body=%s", response.Code, response.Body.String())
		}
		page := decodeBody[myAnswersWithMarkRead](t, response)
		if page.Total != 1 || len(page.Rows) != 1 || page.Rows[0].Reply == nil || page.Rows[0].Reply.ID != reply.ID {
			t.Fatalf("page = %+v, want the one reply row", page)
		}
		return page
	}
	if row := read().Rows[0].Reply; row.EditedAt != nil {
		t.Fatalf("the row's reply edited_at = %v on a reply never edited", row.EditedAt)
	}

	edited := editThreadComment(t, handler, reply.ID, "Thursday, review is done.", "alice")
	row := read().Rows[0].Reply
	if row.Body != "Thursday, review is done." || row.DeletedAt != nil {
		t.Fatalf("the row's reply = %+v, want the edited body standing", row)
	}
	if row.EditedAt == nil || !row.EditedAt.Equal(edited) {
		t.Fatalf("the row's reply edited_at = %v, want the edit's time %s", row.EditedAt, edited)
	}
}

// GET /api/v1/me/answers: the caller's own reply on an ask, deleted, reads with the mark and no
// body.
func TestMyAnswersMarksADeletedReply(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "My answers and a deleted reply", "A spec")
	ask := createAnswerListAsk(t, handler, issue.Key, "Which day do we ship?")
	reply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Friday, if review is done.", "ask_id": ask.ID,
	}, "alice")

	deleteThreadComment(t, handler, reply.ID, "alice")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/answers", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list my answers: status=%d body=%s", response.Code, response.Body.String())
	}
	page := decodeBody[myAnswersWithMarkRead](t, response)
	if page.Total != 1 || len(page.Rows) != 1 || page.Rows[0].Reply == nil {
		t.Fatalf("page = %+v, want the one reply row", page)
	}
	row := page.Rows[0].Reply
	if row.ID != reply.ID || row.Body != "" {
		t.Fatalf("the row's reply = %+v, want the deleted reply with no part of its body", row)
	}
	if row.DeletedAt == nil || row.DeletedBy == nil || row.DeletedBy.ID != "alice" {
		t.Fatalf("the row's mark = %v by %v, want the time of the delete and the actor that made it", row.DeletedAt, row.DeletedBy)
	}
}

// GET /api/v1/me/replies: the thread's root and the newest reply each carry the mark, so a row
// whose subject or whose reply a delete cleared says that the post is deleted.
func TestMyRepliesMarksADeletedRootAndADeletedReply(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "My replies and a deleted post", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Ship it on Friday?"}, "alice")
	reply := sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Friday works.", "reply_to": root.ID,
	})

	row := replyRow(t, getMyReplies(t, handler, "alice", ""), root.ID)
	if row.DeletedAt != nil || row.Reply.DeletedAt != nil {
		t.Fatalf("row = %+v, want no mark while both posts stand", row)
	}

	deleteThreadComment(t, handler, root.ID, "alice")
	row = replyRow(t, getMyReplies(t, handler, "alice", ""), root.ID)
	if row.Subject != "" {
		t.Fatalf("subject = %q after the root was deleted, want no part of its body", row.Subject)
	}
	if row.DeletedAt == nil || row.DeletedBy == nil || row.DeletedBy.ID != "alice" {
		t.Fatalf("the row's mark = %v by %v, want the root's delete", row.DeletedAt, row.DeletedBy)
	}
	if row.Reply.DeletedAt != nil || row.Reply.FirstLine != "Friday works." {
		t.Fatalf("the row's reply = %+v, want the reply standing with its own text", row.Reply)
	}

	deleteThreadComment(t, handler, reply.ID, "")
	row = replyRow(t, getMyReplies(t, handler, "alice", ""), root.ID)
	if row.Reply.FirstLine != "" {
		t.Fatalf("the row's reply line = %q after the reply was deleted, want no part of its body", row.Reply.FirstLine)
	}
	if row.Reply.DeletedAt == nil || row.Reply.DeletedBy == nil || row.Reply.DeletedBy.Kind != "session" {
		t.Fatalf("the row's reply mark = %v by %v, want the session's delete", row.Reply.DeletedAt, row.Reply.DeletedBy)
	}
}

// GET /api/v1/me/replies: a row carries the time its thread's root post was edited, for a
// comment thread and for an issue message thread alike, and none while the root stands as first
// written.
func TestMyRepliesCarriesTheEditTimeOfAnEditedRoot(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "My replies and an edited root", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Ship it on Friday?"}, "alice")
	sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Friday works.", "reply_to": root.ID,
	})
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Status?"}, "alice")
	if answer := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Green.", "in_reply_to": message.ID, "actor": sessionActor(),
	}); answer.Code != http.StatusCreated {
		t.Fatalf("session message reply: status=%d body=%s", answer.Code, answer.Body.String())
	}

	page := getMyReplies(t, handler, "alice", "")
	for _, id := range []string{root.ID, message.ID} {
		if row := replyRow(t, page, id); row.EditedAt != nil {
			t.Fatalf("row %s edited_at = %v while its root stands as first written", id, row.EditedAt)
		}
	}

	edited := editThreadComment(t, handler, root.ID, "Ship it on Thursday?", "alice")
	editedMessage := editIssueMessage(t, handler, message.ID, "Status, please?", "alice")
	page = getMyReplies(t, handler, "alice", "")
	row := replyRow(t, page, root.ID)
	if row.Subject != "Ship it on Thursday?" || row.DeletedAt != nil {
		t.Fatalf("comment row = %+v, want the edited root standing", row)
	}
	if row.EditedAt == nil || !row.EditedAt.Equal(edited) {
		t.Fatalf("comment row edited_at = %v, want the root's edit at %s", row.EditedAt, edited)
	}
	messageRow := replyRow(t, page, message.ID)
	if messageRow.Subject != "Status, please?" {
		t.Fatalf("message row subject = %q, want the edited root's", messageRow.Subject)
	}
	if messageRow.EditedAt == nil || editedMessage.EditedAt == nil || !messageRow.EditedAt.Equal(*editedMessage.EditedAt) {
		t.Fatalf("message row edited_at = %v, want the root's edit at %v", messageRow.EditedAt, editedMessage.EditedAt)
	}
}
