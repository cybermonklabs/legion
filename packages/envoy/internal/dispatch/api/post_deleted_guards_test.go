// post_deleted_guards_test.go is what a deleted post refuses and what it still takes. A write
// that would add text to a post whose author withdrew it answers 409 POST_DELETED; a write that
// moves no text stands, because the post keeps its row and its place in its thread. An edit and
// a delete of a post of a closed issue answer 200: a wrong post nobody can correct after the
// issue closes is the defect these routes exist for.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// refusedAsDeleted fails t unless the response is the one refusal a deleted post makes.
func refusedAsDeleted(t *testing.T, name string, response *httptest.ResponseRecorder) {
	t.Helper()
	code, _ := refusalOf(t, response)
	if response.Code != http.StatusConflict || code != "POST_DELETED" {
		t.Fatalf("%s on a deleted post: status=%d body=%s, want 409 POST_DELETED",
			name, response.Code, response.Body.String())
	}
}

// Every write that would add text to a deleted post is refused, each in its own right: a
// suggestion's accept and reject, a reply of either kind, a new delivery of either kind, and a
// second edit of either kind. A second delete is refused by the same rule, which the author
// tests of each delete route hold.
func TestEveryWriteThatAddsToADeletedPostIsRefused(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A deleted post refuses", "before")

	suggestion := createThreadComment(t, handler, issue.Key, map[string]any{
		"body":       "Say this instead.",
		"anchor":     map[string]any{"artifact": "spec", "quote": "before"},
		"suggestion": map[string]string{"replace_with": "after"},
	}, "alice")
	deleteThreadComment(t, handler, suggestion.ID, "alice")

	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "the withdrawn comment", "actor": sessionActor(),
	}, "")
	deleteThreadComment(t, handler, comment.ID, "")

	message := createSessionIssueMessage(t, handler, issue.Key, "the withdrawn message")
	// The retry route answers for a message nobody targeted before it reads the delete, so the
	// message this one refuses is a targeted one; the target is written here because an
	// ordinary send needs a live agent listener, which this refusal has nothing to do with.
	if _, err := database.Pool.Exec(context.Background(),
		`update messages set target = 'session:s1' where id = $1`, message.ID); err != nil {
		t.Fatalf("target the message: %v", err)
	}
	deleteIssueMessage(t, handler, message.ID, "")

	t.Run("accept a suggestion", func(t *testing.T) {
		refusedAsDeleted(t, "accept", dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/comments/"+suggestion.ID+"/accept", map[string]any{}, "alice"))
	})
	t.Run("reject a suggestion", func(t *testing.T) {
		refusedAsDeleted(t, "reject", dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/comments/"+suggestion.ID+"/reject", map[string]any{}, "alice"))
	})
	t.Run("reply to a comment", func(t *testing.T) {
		// The refusal comes before the mention the reply answers is looked for, so a session
		// holding a delivery frame of the comment is told that the post is gone and not that
		// the mention is.
		refusedAsDeleted(t, "reply", sessionRequest(t, handler, http.MethodPost,
			"/api/v1/comments/"+comment.ID+"/reply", map[string]any{
				"actor": sessionActor(), "attempt": 1, "body": "answering it",
			}))
	})
	t.Run("reply to a message", func(t *testing.T) {
		refusedAsDeleted(t, "reply", sessionRequest(t, handler, http.MethodPost,
			"/api/v1/messages/"+message.ID+"/reply", map[string]any{
				"actor": sessionActor(), "attempt": 1, "body": "answering it",
			}))
	})
	t.Run("deliver a comment again", func(t *testing.T) {
		refusedAsDeleted(t, "retry the mention", dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{"delivery": "steer"}, "alice"))
	})
	t.Run("deliver a message again", func(t *testing.T) {
		refusedAsDeleted(t, "retry the send", dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/messages/"+message.ID+"/deliveries", map[string]any{"delivery": "steer"}, "alice"))
	})
	t.Run("edit a deleted comment", func(t *testing.T) {
		refusedAsDeleted(t, "edit", sessionRequest(t, handler, http.MethodPatch,
			"/api/v1/comments/"+comment.ID, map[string]any{"actor": sessionActor(), "body": "a second body"}))
	})
	t.Run("edit a deleted message", func(t *testing.T) {
		refusedAsDeleted(t, "edit", sessionRequest(t, handler, http.MethodPatch,
			"/api/v1/messages/"+message.ID, map[string]any{"actor": sessionActor(), "body": "a second body"}))
	})
	t.Run("delete a deleted comment", func(t *testing.T) {
		refusedAsDeleted(t, "delete", sessionRequest(t, handler, http.MethodDelete,
			"/api/v1/comments/"+comment.ID, map[string]any{"actor": sessionActor()}))
	})
	t.Run("delete a deleted message", func(t *testing.T) {
		refusedAsDeleted(t, "delete", sessionRequest(t, handler, http.MethodDelete,
			"/api/v1/messages/"+message.ID, map[string]any{"actor": sessionActor()}))
	})

	// Nothing the refused writes asked for was written: the posts stand as the deletes left
	// them, with no reply under either.
	if stored := readThreadComment(t, handler, comment.ID); stored.Body != "" || stored.DeletedAt == nil {
		t.Fatalf("the deleted comment after the refused writes = %#v, want the mark and no body", stored)
	}
	var replies int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from messages where in_reply_to = $1`, message.ID).Scan(&replies); err != nil {
		t.Fatalf("count the replies of the deleted message: %v", err)
	}
	if replies != 0 {
		t.Fatalf("the deleted message holds %d replies, want none: each reply was refused", replies)
	}
}

// A session that answered a delivery and then deleted its own answer has nothing left to
// follow up under: ?follow_up=true is refused 409 POST_DELETED, as every other reply to a
// deleted post is. The post a follow-up hangs under is that answer and not the message the
// attempt carried, so the route reads the answer's own mark (replyAfterAnswer,
// message_delivery.go).
func TestAFollowUpUnderADeletedAnswerIsRefused(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A follow-up under a withdrawn answer", "before")
	message := createIssueMessage(t, handler, issue.Key,
		map[string]any{"body": "the message the session answers"}, "alice")
	// The attempt the session answers is written here because an ordinary send needs a live
	// agent listener, which this refusal has nothing to do with.
	if _, err := database.Pool.Exec(context.Background(), `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state)
		values ($1, 1, 'steer', $2, 'pending')
	`, message.ID, sessionActor()["id"]); err != nil {
		t.Fatalf("seed the attempt the session answers: %v", err)
	}

	answered := sessionRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply?follow_up=true",
		map[string]any{"actor": sessionActor(), "attempt": 1, "body": "the answer it withdraws"})
	if answered.Code != http.StatusCreated {
		t.Fatalf("the session answers the delivery: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := decodeBody[replyResponse](t, answered)
	deleteIssueMessage(t, handler, answer.ID, "")

	refusedAsDeleted(t, "a follow-up under the deleted answer", sessionRequest(t, handler, http.MethodPost,
		"/api/v1/messages/"+message.ID+"/reply?follow_up=true",
		map[string]any{"actor": sessionActor(), "attempt": 1, "body": "more, under the answer it withdrew"}))

	var replies int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from messages where in_reply_to = $1`, answer.ID).Scan(&replies); err != nil {
		t.Fatalf("count the replies of the deleted answer: %v", err)
	}
	if replies != 0 {
		t.Fatalf("the deleted answer holds %d replies, want none: the follow-up was refused", replies)
	}
}

// replyLockWaitWindow bounds how long the probe below waits for the reply to queue on the
// issue's row. It is far longer than the reply's own first read and shorter than the package's
// test timeout, so a reply that never reaches the lock is reported rather than hanging the run.
const replyLockWaitWindow = 20 * time.Second

// waitForALockWaiter blocks until a backend of this database waits on a lock it has not been
// granted, which is the reply's transaction queuing on the issue row this test holds. The two
// backends are this test's own: nothing else writes this database while the test runs.
func waitForALockWaiter(t *testing.T, database *store.Store) {
	t.Helper()
	deadline := time.Now().Add(replyLockWaitWindow)
	for {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_locks l join pg_stat_activity a on a.pid = l.pid
			where not l.granted and a.datname = current_database()
		`).Scan(&waiting); err != nil {
			t.Fatalf("read the lock waiters: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing waited for the issue's row within %s: the reply never reached the lock", replyLockWaitWindow)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The window the delete commits in, driven rather than reasoned about. A transaction of this
// test's own takes the issue row exactly as lockIssue takes it, the reply reads the message -
// standing, since nothing is committed yet - and then queues on that row; the holder marks the
// message deleted and commits, which is the delete this route must not post behind; the reply
// takes the lock and reads the row again under it. The delete is written here rather than sent
// to DELETE /api/v1/messages/{id} because that route takes the same row lock, and a second
// waiter cannot commit inside the first one's wait.
//
// Before the re-read (message_delivery.go's replyMessage) the deleted check read the row taken
// before the lock, so this reply was judged on a message that still stood and posted under a
// deleted one.
func TestAReplyIsRefusedByADeleteThatCommitsWhileItWaitsForTheIssueRow(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A delete inside the reply's window", "before")
	message := createIssueMessage(t, handler, issue.Key,
		map[string]any{"body": "the message the delete withdraws"}, "alice")
	if _, err := database.Pool.Exec(context.Background(), `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state)
		values ($1, 1, 'steer', $2, 'pending')
	`, message.ID, sessionActor()["id"]); err != nil {
		t.Fatalf("seed the attempt the session answers: %v", err)
	}

	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction that holds the issue's row: %v", err)
	}
	defer holder.Rollback(ctx)
	// The statement lockIssue runs (issueStatus under ownerRowLock, server.go).
	if _, err := holder.Exec(ctx,
		`select status, closed_at is null from issues where key = $1 for no key update`, issue.Key); err != nil {
		t.Fatalf("hold the issue's row: %v", err)
	}

	replied := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		replied <- sessionRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply",
			map[string]any{"actor": sessionActor(), "attempt": 1, "body": "answering a message that was withdrawn"})
	}()
	waitForALockWaiter(t, database)

	if _, err := holder.Exec(ctx,
		`update messages set body = '', deleted_at = now(), deleted_by = $2::jsonb where id = $1`,
		message.ID, `{"kind":"user","id":"alice"}`); err != nil {
		t.Fatalf("delete the message inside the reply's window: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the delete: %v", err)
	}

	refusedAsDeleted(t, "a reply the delete beat to the issue's row", <-replied)
	var replies int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from messages where in_reply_to = $1`, message.ID).Scan(&replies); err != nil {
		t.Fatalf("count the replies of the deleted message: %v", err)
	}
	if replies != 0 {
		t.Fatalf("the deleted message holds %d replies, want none: the reply was refused", replies)
	}
}

// The ordinary thread reply is the commonest reply of all, and it takes the same refusal as the
// delivery callback's: POST /api/v1/issues/{key}/comments and POST /api/v1/artifacts/{id}/comments
// with reply_to naming a deleted comment, and POST /api/v1/issues/{key}/messages with in_reply_to
// naming a deleted message. The refusal is read from the parent the create path already holds, so
// it comes before the block a reply names is looked for: a reply naming a block of the withdrawn
// post is told the post is gone and not that the delete took its blocks. A reply to a post nobody
// deleted still stands, which the last two cases hold.
func TestThreadReplyToADeletedPostIsRefused(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A deleted post takes no reply", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "the withdrawn comment"}, "alice")
	deleteThreadComment(t, handler, comment.ID, "alice")

	document := createProjectDocument(t, handler, "TEST", "Design notes", "# Design notes\n")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments",
		map[string]any{"body": "the withdrawn document comment"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the document comment: status=%d body=%s", created.Code, created.Body.String())
	}
	documentComment := decodeBody[model.Comment](t, created)
	deleteThreadComment(t, handler, documentComment.ID, "alice")

	message := createSessionIssueMessage(t, handler, issue.Key, "the withdrawn message")
	deleteIssueMessage(t, handler, message.ID, "")

	t.Run("reply to a deleted comment on its issue", func(t *testing.T) {
		refusedAsDeleted(t, "thread reply", dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "answering the withdrawn comment", "reply_to": comment.ID,
			}, "bob"))
	})
	t.Run("reply naming a block of a deleted comment", func(t *testing.T) {
		refusedAsDeleted(t, "thread reply to a block", dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "answering its first block", "reply_to": comment.ID, "reply_to_block": "b1",
			}, "bob"))
	})
	t.Run("reply to a deleted comment on a document", func(t *testing.T) {
		refusedAsDeleted(t, "document thread reply", dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/artifacts/"+document.ID+"/comments", map[string]any{
				"body": "answering the withdrawn document comment", "reply_to": documentComment.ID,
			}, "bob"))
	})
	t.Run("reply to a deleted message", func(t *testing.T) {
		refusedAsDeleted(t, "thread reply", sessionRequest(t, handler, http.MethodPost,
			"/api/v1/issues/"+issue.Key+"/messages", map[string]any{
				"actor": sessionActor(), "body": "answering the withdrawn message", "in_reply_to": message.ID,
			}))
	})

	// The negative control: the guard refuses a reply to the post that was deleted and to no
	// other, so a thread reply to a live comment and to a live message still answers 201.
	t.Run("reply to a comment nobody deleted", func(t *testing.T) {
		live := createThreadComment(t, handler, issue.Key, map[string]any{"body": "the comment that stands"}, "alice")
		reply := createThreadComment(t, handler, issue.Key, map[string]any{
			"body": "answering it", "reply_to": live.ID,
		}, "bob")
		if reply.ReplyTo == nil || *reply.ReplyTo != live.ID {
			t.Fatalf("the reply to a live comment = %#v, want it under %s", reply, live.ID)
		}
	})
	t.Run("reply to a message nobody deleted", func(t *testing.T) {
		live := createSessionIssueMessage(t, handler, issue.Key, "the message that stands")
		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages",
			map[string]any{"actor": sessionActor(), "body": "answering it", "in_reply_to": live.ID})
		if response.Code != http.StatusCreated {
			t.Fatalf("reply to a live message: status=%d body=%s, want 201", response.Code, response.Body.String())
		}
		if reply := decodeBody[model.Message](t, response); reply.InReplyTo == nil || *reply.InReplyTo != live.ID {
			t.Fatalf("the reply to a live message = %#v, want it under %s", reply, live.ID)
		}
	})

	// Nothing the refused replies asked for was written: neither deleted post holds a reply.
	for _, deleted := range []model.Comment{comment, documentComment} {
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+deleted.ID, nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("read the deleted comment: status=%d body=%s", read.Code, read.Body.String())
		}
		if thread := decodeBody[commentRead](t, read); len(thread.Replies) != 0 {
			t.Fatalf("the deleted comment holds %#v, want no reply: each reply was refused", thread.Replies)
		}
	}
	var replies int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from messages where in_reply_to = $1`, message.ID).Scan(&replies); err != nil {
		t.Fatalf("count the replies of the deleted message: %v", err)
	}
	if replies != 0 {
		t.Fatalf("the deleted message holds %d replies, want none: the reply was refused", replies)
	}
}

// A write that moves no text stands on a deleted post: a resolve and a reopen of its thread, and
// the session's record that it took a deleted message as its own turn.
func TestEveryWriteThatMovesNoTextStandsOnADeletedPost(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A deleted post stands", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "the withdrawn root"}, "alice")
	deleteThreadComment(t, handler, comment.ID, "alice")

	t.Run("resolve the thread", func(t *testing.T) {
		resolved := dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/comments/"+comment.ID+"/resolve", map[string]any{}, "alice")
		if resolved.Code != http.StatusOK {
			t.Fatalf("resolve a deleted thread: status=%d body=%s, want 200", resolved.Code, resolved.Body.String())
		}
		if read := decodeBody[model.Comment](t, resolved); !read.Resolved || read.DeletedAt == nil {
			t.Fatalf("the resolved comment = %#v, want it resolved and still deleted", read)
		}
	})
	t.Run("reopen the thread", func(t *testing.T) {
		reopened := dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/comments/"+comment.ID+"/reopen", map[string]any{}, "alice")
		if reopened.Code != http.StatusOK {
			t.Fatalf("reopen a deleted thread: status=%d body=%s, want 200", reopened.Code, reopened.Body.String())
		}
		if read := decodeBody[model.Comment](t, reopened); read.Resolved || read.DeletedAt == nil {
			t.Fatalf("the reopened comment = %#v, want it open and still deleted", read)
		}
	})

	// The accept route records that a session took a message as its user's own turn, which adds
	// no text to the post. Its message is a person's direct send to the session, on no issue,
	// which this change never deletes; the delete that the accept stands after is the mark on
	// the row, written here as the one route that could write it refuses an issue-less message.
	direct := seedConversationMessage(t, database, model.Actor{Kind: "user", ID: "alice"},
		"take this as your turn", "session:session-0123456789abcdef")
	if _, err := database.Pool.Exec(context.Background(), `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, requested_by)
		values ($1, 1, 'aside', 'session-0123456789abcdef', 'sent', '{"kind":"user","id":"alice"}'::jsonb)
	`, direct); err != nil {
		t.Fatalf("seed the attempt the session accepts: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update messages set deleted_at = now(), deleted_by = '{"kind":"user","id":"alice"}'::jsonb, body = ''
		where id = $1
	`, direct); err != nil {
		t.Fatalf("mark the message deleted: %v", err)
	}
	t.Run("accept a delivery", func(t *testing.T) {
		accepted := sessionRequest(t, handler, http.MethodPost,
			"/api/v1/messages/"+direct+"/deliveries/1/accept", map[string]any{"actor": sessionActor()})
		if accepted.Code != http.StatusOK {
			t.Fatalf("accept an attempt of a deleted message: status=%d body=%s, want 200",
				accepted.Code, accepted.Body.String())
		}
	})
}

// An edit and a delete of a post of a closed issue answer 200: they take the prologue that does
// not refuse a closed owner. Every other write of an existing comment keeps today's refusal.
func TestEditAndDeleteOfAPostOfAClosedIssue(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Correct a closed issue's posts", "before")
	edited := createThreadComment(t, handler, issue.Key, map[string]any{"body": "the first body"}, "alice")
	deleted := createThreadComment(t, handler, issue.Key, map[string]any{"body": "the withdrawn comment"}, "alice")
	editedMessage := createSessionIssueMessage(t, handler, issue.Key, "the message's first body")
	deletedMessage := createSessionIssueMessage(t, handler, issue.Key, "the withdrawn message")
	closeIssue(t, handler, issue.Key)

	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+edited.ID,
		map[string]any{"body": "the corrected body"}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("edit a comment of a closed issue: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if comment := decodeBody[model.Comment](t, response); comment.Body != "the corrected body" {
		t.Fatalf("the edited comment = %#v, want the corrected body", comment)
	}
	if comment := deleteThreadComment(t, handler, deleted.ID, "alice"); comment.DeletedAt == nil {
		t.Fatalf("the deleted comment of a closed issue = %#v, want the mark", comment)
	}
	if message := editIssueMessage(t, handler, editedMessage.ID, "the message's corrected body", ""); message.EditedAt == nil {
		t.Fatalf("the edited message of a closed issue = %#v, want edited_at", message)
	}
	if message := deleteIssueMessage(t, handler, deletedMessage.ID, ""); message.DeletedAt == nil {
		t.Fatalf("the deleted message of a closed issue = %#v, want the mark", message)
	}

	// Every other write of a comment of a closed issue is refused as it always was.
	resolved := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/comments/"+edited.ID+"/resolve", map[string]any{}, "alice")
	code, _ := refusalOf(t, resolved)
	if resolved.Code != http.StatusConflict || code != "ISSUE_CLOSED" {
		t.Fatalf("resolve a comment of a closed issue: status=%d body=%s, want 409 ISSUE_CLOSED",
			resolved.Code, resolved.Body.String())
	}
}

// A deleted reply keeps its place in the ask's thread, so it goes on deciding whose turn the ask
// waits on: the delete clears the body and keeps the row newestReply reads.
func TestADeletedNewestReplyStillDecidesTheAsksTurn(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "The turn after a delete", "before")
	askID := openTurnAsk(t, handler, issue.Key, "Which auditor?")

	replied := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Ask the second auditor.", "ask_id": askID,
	}, "alice")
	if replied.Code != http.StatusCreated {
		t.Fatalf("reply on the ask: status=%d body=%s", replied.Code, replied.Body.String())
	}
	reply := decodeBody[model.Comment](t, replied)
	if row := readInboxRow(t, handler, askID); row.WaitingOn != "agent" {
		t.Fatalf("the ask waits on %q after the human's reply, want agent", row.WaitingOn)
	}

	deleted := deleteThreadComment(t, handler, reply.ID, "alice")
	if deleted.AskID == nil || *deleted.AskID != askID || deleted.Turn == nil {
		t.Fatalf("the deleted reply = %#v, want it still the ask's reply with its turn", deleted)
	}

	row := readInboxRow(t, handler, askID)
	if row.WaitingOn != "agent" {
		t.Fatalf("the ask waits on %q after its newest reply was deleted, want agent: the row still decides", row.WaitingOn)
	}
	if row.LastReply == nil || row.LastReply.Author.Kind != "user" {
		t.Fatalf("the ask's last reply = %#v, want the deleted reply's own row", row.LastReply)
	}
	if detail := readAskDetail(t, handler, askID); detail.WaitingOn != "agent" {
		t.Fatalf("the ask reads waiting_on %q, want agent", detail.WaitingOn)
	}
}
