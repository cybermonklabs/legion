package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// readPostHistory is one history read as an authenticated person makes it.
func readPostHistory(t *testing.T, handler http.Handler, target string) postHistoryRead {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return decodeBody[postHistoryRead](t, response)
}

// A post nobody has edited has an empty history, not an error: the read is the record of what
// replaced a body, and nothing has replaced one.
func TestPostHistoryOfAPostNobodyEditedIsEmptyAndNotAnError(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Untouched posts", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "stands as written"}, "alice")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"actor": sessionActor(), "body": "also stands as written",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)

	for name, target := range map[string]string{
		"comment": "/api/v1/comments/" + comment.ID + "/history",
		"message": "/api/v1/messages/" + message.ID + "/history",
	} {
		t.Run(name, func(t *testing.T) {
			history := readPostHistory(t, handler, target)
			if len(history.Entries) != 0 || history.DeletedAt != nil || history.DeletedBy != nil {
				t.Fatalf("history of an untouched %s = %#v, want no entries and no deleted mark", name, history)
			}
		})
	}
}

// Two edits leave two rows, newest first, each with the body it replaced, the actor that
// replaced it and the time; ?before= reads the page after a row, and anything that is not an
// RFC 3339 time is refused.
func TestCommentHistoryListsEachReplacedBodyNewestFirst(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Edited twice", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "first body", "actor": sessionActor(),
	}, "")
	for _, body := range []string{"second body", "third body"} {
		edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{
			"body": body, "actor": sessionActor(),
		})
		if edited.Code != http.StatusOK {
			t.Fatalf("edit to %q: status=%d body=%s", body, edited.Code, edited.Body.String())
		}
	}

	history := readPostHistory(t, handler, "/api/v1/comments/"+comment.ID+"/history")
	if len(history.Entries) != 2 {
		t.Fatalf("history = %#v, want the two bodies the two edits replaced", history.Entries)
	}
	if history.Entries[0].Body != "second body" || history.Entries[1].Body != "first body" {
		t.Fatalf("history bodies = %q and %q, want \"second body\" then \"first body\": newest first",
			history.Entries[0].Body, history.Entries[1].Body)
	}
	author := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	for index, entry := range history.Entries {
		if !entry.EditedBy.SameAs(author) {
			t.Fatalf("entry %d was replaced by %#v, want the editing session %#v", index, entry.EditedBy, author)
		}
		if entry.EditedAt.IsZero() || entry.EditedAt.After(time.Now().UTC().Add(time.Second)) {
			t.Fatalf("entry %d edited_at = %s, want the time of the edit", index, entry.EditedAt)
		}
	}
	if !history.Entries[0].EditedAt.After(history.Entries[1].EditedAt) {
		t.Fatalf("edited_at %s is not after %s: the list is not newest first",
			history.Entries[0].EditedAt, history.Entries[1].EditedAt)
	}

	// The page after the first row is the rows written before it.
	page := readPostHistory(t, handler, "/api/v1/comments/"+comment.ID+"/history?before="+
		history.Entries[0].EditedAt.Format(time.RFC3339Nano))
	if len(page.Entries) != 1 || page.Entries[0].Body != "first body" {
		t.Fatalf("?before= the newest row answered %#v, want the one row under it", page.Entries)
	}
	empty := readPostHistory(t, handler, "/api/v1/comments/"+comment.ID+"/history?before="+
		history.Entries[1].EditedAt.Format(time.RFC3339Nano))
	if len(empty.Entries) != 0 {
		t.Fatalf("?before= the oldest row answered %#v, want nothing under it", empty.Entries)
	}
	refused := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/comments/"+comment.ID+"/history?before=yesterday", nil, "alice")
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("?before=yesterday: status=%d body=%s, want 400", refused.Code, refused.Body.String())
	}
}

// One answer holds the newest contracts.PostHistoryPageLimit rows, the page `dispatch read
// --history` takes as full (POST_HISTORY_PAGE_LIMIT, the declaration this constant is generated
// from); the one row past it is the page after, ?before= the oldest row the first page gave.
func TestPostHistoryAnswersAPageOfThePageLimitAndTheRowsBeforeItAfter(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A long history", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "the body today"}, "alice")
	// Body n was replaced n minutes ago: body 1 is the newest, and the last, one past the page,
	// is the oldest.
	kept := contracts.PostHistoryPageLimit + 1
	if _, err := database.Pool.Exec(context.Background(), `
		insert into post_body_history (comment_id, body, edited_by, edited_at)
		select $1::uuid, 'body ' || n, '{"kind":"user","id":"alice"}'::jsonb, now() - make_interval(mins => n)
		from generate_series(1, $2::int) n
	`, comment.ID, kept); err != nil {
		t.Fatalf("keep %d bodies: %v", kept, err)
	}

	page := readPostHistory(t, handler, "/api/v1/comments/"+comment.ID+"/history")
	if len(page.Entries) != contracts.PostHistoryPageLimit {
		t.Fatalf("the first page holds %d rows, want contracts.PostHistoryPageLimit, %d",
			len(page.Entries), contracts.PostHistoryPageLimit)
	}
	oldest := page.Entries[len(page.Entries)-1]
	if newest, want := page.Entries[0].Body, fmt.Sprintf("body %d", contracts.PostHistoryPageLimit); newest != "body 1" || oldest.Body != want {
		t.Fatalf("the first page runs from %q to %q, want \"body 1\" to %q: the newest rows", newest, oldest.Body, want)
	}
	after := readPostHistory(t, handler, "/api/v1/comments/"+comment.ID+"/history?before="+
		oldest.EditedAt.Format(time.RFC3339Nano))
	if want := fmt.Sprintf("body %d", kept); len(after.Entries) != 1 || after.Entries[0].Body != want {
		t.Fatalf("?before= the first page's oldest row answered %#v, want the one row %q", after.Entries, want)
	}
}

// A history is read by whoever reads the post: any authenticated caller, the author or not. An
// agent that did not write the comment reads what replaced its bodies, as a person does.
func TestCommentHistoryIsReadByASessionThatIsNotItsAuthor(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Another session reads", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "the author's first body", "actor": sessionActor(),
	}, "")
	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{
		"body": "the author's second body", "actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("the author edits: status=%d body=%s", edited.Code, edited.Body.String())
	}

	// A bearer caller that is not the author: no actor is named, because a read records none.
	read := sessionRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID+"/history", nil)
	if read.Code != http.StatusOK {
		t.Fatalf("another session reads the history: status=%d body=%s", read.Code, read.Body.String())
	}
	history := decodeBody[postHistoryRead](t, read)
	if len(history.Entries) != 1 || history.Entries[0].Body != "the author's first body" {
		t.Fatalf("history read by another session = %#v, want the one replaced body", history.Entries)
	}
	if other := readPostHistory(t, handler, "/api/v1/comments/"+comment.ID+"/history"); len(other.Entries) != 1 {
		t.Fatalf("history read by another person = %#v, want the one replaced body", other.Entries)
	}
}

// A delete gives no part of the body on any read of the post, so the history it kept is served
// as the mark and no text at all.
func TestDeletedPostHistoryGivesTheMarkAndNoBody(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Deleted history", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "the first body"}, "alice")
	edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID,
		map[string]any{"body": "the second body"}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
	deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+comment.ID, nil, "alice")
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete: status=%d body=%s", deleted.Code, deleted.Body.String())
	}

	history := readPostHistory(t, handler, "/api/v1/comments/"+comment.ID+"/history")
	if history.DeletedAt == nil || history.DeletedBy == nil || history.DeletedBy.ID != "alice" {
		t.Fatalf("deleted history mark = %#v / %#v, want the time and the deleting actor", history.DeletedAt, history.DeletedBy)
	}
	if len(history.Entries) != 2 {
		t.Fatalf("deleted history = %#v, want the edit's row and the delete's row", history.Entries)
	}
	for index, entry := range history.Entries {
		if entry.Body != "" {
			t.Fatalf("deleted history entry %d carries %q, want no body at all", index, entry.Body)
		}
	}
	// The answer is the comment as the delete left it: the body it could give is the one that
	// stood at the delete. The body the edit replaced is no longer on the comment's row, and the
	// answer carries no history.
	if body := deleted.Body.String(); strings.Contains(body, "the second body") {
		t.Fatalf("the delete answered %s, want no part of the body", body)
	}
}

// The pending send the two delete-settle tests seed on a post of each kind, one that nothing has
// resolved yet, and the pin that send's own sender writes on its row holding no owner lock: a
// comment's the session its send resolved to (recordCommentDeliveryResolution), a message's the
// session its renewed try goes to (renewMessageDeliveryClaim). Each takes the post's id.
const (
	seedPendingCommentAttempt = `
		insert into comment_deliveries (comment_id, target, attempt, delivery, state)
		values ($1, 'role:reviewer', 1, 'steer', 'pending')
	`
	movePendingCommentAttempt = `update comment_deliveries set session_id = coalesce(session_id, 's1') where comment_id = $1`
	seedPendingMessageAttempt = `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state)
		values ($1, 1, 'steer', '', 'pending')
	`
	movePendingMessageAttempt = `update message_deliveries set session_id = coalesce(nullif(session_id, ''), 's1') where message_id = $1`
)

// A delete settles each send of its post still pending from what its read found of it, and an
// attempt's own sender moves a pending row holding no owner lock: a comment's pins the session
// its send resolved to (recordCommentDeliveryResolution), a message's the session its renewed
// try goes to (renewMessageDeliveryClaim). The settle therefore holds each row it settles to that
// read (queueDeletedPostSettlement). A row that stands as the read found it is settled, an
// unresolved comment attempt's null session and resolve error included. One whose session moved
// after the read refuses the settle, as one the read never found does, and the route answers
// both 409 DELETE_CONTENDED with Retry-After: 1, with nothing settled.
func TestADeletesSettleHoldsEachPendingAttemptToWhatItsReadFound(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "TEST", "Settle what the read found", "before")
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", strings.TrimSpace(sql), err)
		}
	}
	kinds := []struct {
		name string
		// open writes a post with one send pending that nothing has resolved yet.
		open func(t *testing.T) string
		// read is what the delete's read finds of the post's pending sends, and none a read
		// that found none.
		read func(t *testing.T, id string) pendingAttemptsRead
		none pendingAttemptsRead
		// move is the pin the send's own sender writes on its row, holding no owner lock.
		move, state string
	}{
		{
			name: "comment",
			open: func(t *testing.T) string {
				comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "a comment a send waits on"}, "alice")
				exec(t, seedPendingCommentAttempt, comment.ID)
				return comment.ID
			},
			read: func(t *testing.T, id string) pendingAttemptsRead {
				t.Helper()
				rows, err := database.Pool.Query(ctx, `
					select `+commentDeliveryColumns+` from comment_deliveries where comment_id = $1 and state = 'pending'
				`, id)
				if err != nil {
					t.Fatalf("read the pending attempts of comment %s: %v", id, err)
				}
				pending, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.CommentDelivery, error) {
					return scanCommentDelivery(row)
				})
				if err != nil {
					t.Fatalf("read the pending attempts of comment %s: %v", id, err)
				}
				return commentAttemptsRead(pending)
			},
			none:  commentAttemptsRead(nil),
			move:  movePendingCommentAttempt,
			state: `select state from comment_deliveries where comment_id = $1`,
		},
		{
			name: "message",
			open: func(t *testing.T) string {
				message := createSessionIssueMessage(t, handler, issue.Key, "a message a send waits on")
				exec(t, seedPendingMessageAttempt, message.ID)
				return message.ID
			},
			read: func(t *testing.T, id string) pendingAttemptsRead {
				t.Helper()
				rows, err := database.Pool.Query(ctx, `
					select `+messageDeliveryColumns+` from message_deliveries where message_id = $1 and state = 'pending'
				`, id)
				if err != nil {
					t.Fatalf("read the pending attempts of message %s: %v", id, err)
				}
				pending, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.MessageDelivery, error) {
					return scanMessageDelivery(row)
				})
				if err != nil {
					t.Fatalf("read the pending attempts of message %s: %v", id, err)
				}
				return messageAttemptsRead(pending)
			},
			none:  messageAttemptsRead(nil),
			move:  movePendingMessageAttempt,
			state: `select state from message_deliveries where message_id = $1`,
		},
	}
	for _, kind := range kinds {
		for _, shape := range []struct {
			name          string
			moved, unread bool
		}{
			{name: "a row that stands as read is settled"},
			{name: "a row its sender moved after the read refuses the settle", moved: true},
			{name: "a row the read did not find refuses the settle", unread: true},
		} {
			t.Run(kind.name+": "+shape.name, func(t *testing.T) {
				id := kind.open(t)
				read := kind.read(t, id)
				if shape.unread {
					read = kind.none
				}
				if shape.moved {
					exec(t, kind.move, id)
				}

				tx, err := database.Pool.Begin(ctx)
				if err != nil {
					t.Fatalf("begin the delete's transaction: %v", err)
				}
				defer tx.Rollback(ctx)
				settle := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
					queueDeletedPostSettlement(b, id, read)
					return nil
				})
				want := model.DeliveryStateFailed
				if shape.moved || shape.unread {
					if settle == nil {
						t.Fatalf("the settle took a row its read did not find as it stands")
					}
					recorder := httptest.NewRecorder()
					(&server{}).writeHandlerError(recorder, settle)
					if code, _ := refusalOf(t, recorder); recorder.Code != http.StatusConflict || code != deleteContended ||
						recorder.Header().Get("Retry-After") != "1" {
						t.Fatalf("the route answers the refusal %d (Retry-After %q) %s, want 409 %s with Retry-After 1",
							recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String(), deleteContended)
					}
					if err := tx.Rollback(ctx); err != nil {
						t.Fatalf("roll the refused settle back: %v", err)
					}
					want = model.DeliveryStatePending
				} else {
					if settle != nil {
						t.Fatalf("settle the row its read found as it stands: %v", settle)
					}
					if err := tx.Commit(ctx); err != nil {
						t.Fatalf("commit the settle: %v", err)
					}
				}
				var state string
				if err := database.Pool.QueryRow(ctx, kind.state, id).Scan(&state); err != nil {
					t.Fatalf("read the attempt after the settle: %v", err)
				}
				if state != want {
					t.Fatalf("the attempt reads %s after the settle, want %s", state, want)
				}
			})
		}
	}
}

// The settle's refusal as the delete route meets it, driven rather than reasoned about: a send of
// the post is pending, and a transaction of this test's own makes the write that send's own sender
// makes on its row, holding no owner lock - it pins the session its send resolved to - and holds
// the row until it commits. The delete reads the attempt as it stood before that write and queues
// its settle behind it; the write commits, and the settle meets a row its read did not find. The
// delete answers 409 DELETE_CONTENDED with Retry-After: 1 and writes nothing - the body stands,
// no deleted_at, no kept body, the attempt still pending - and the same delete sent again reads
// the attempt as it stands then and answers 200.
func TestADeleteWhoseSendMovesWhileItIsWrittenAnswersDeleteContended(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "TEST", "A send that moves under its delete", "before")
	for _, kind := range []struct {
		name string
		// open writes a post of alice's with one send pending that nothing has resolved yet.
		open func(t *testing.T) string
		// route is the delete's path less the post's id; posts and attempts are the post's table
		// and its attempts', and owner the column naming the post in post_body_history and in
		// attempts.
		route, posts, attempts, owner string
		// move is the pin the send's own sender writes on its row (recordCommentDeliveryResolution,
		// renewMessageDeliveryClaim).
		move string
	}{
		{
			name: "comment",
			open: func(t *testing.T) string {
				t.Helper()
				comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "a comment whose send moves"}, "alice")
				if _, err := database.Pool.Exec(ctx, seedPendingCommentAttempt, comment.ID); err != nil {
					t.Fatalf("seed the pending attempt of comment %s: %v", comment.ID, err)
				}
				return comment.ID
			},
			route: "/api/v1/comments/", posts: "comments", attempts: "comment_deliveries", owner: "comment_id",
			move: movePendingCommentAttempt,
		},
		{
			name: "message",
			open: func(t *testing.T) string {
				t.Helper()
				message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "a message whose send moves"}, "alice")
				if _, err := database.Pool.Exec(ctx, seedPendingMessageAttempt, message.ID); err != nil {
					t.Fatalf("seed the pending attempt of message %s: %v", message.ID, err)
				}
				return message.ID
			},
			route: "/api/v1/messages/", posts: "messages", attempts: "message_deliveries", owner: "message_id",
			move: movePendingMessageAttempt,
		},
	} {
		t.Run(kind.name, func(t *testing.T) {
			id := kind.open(t)
			holder, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the sender's transaction: %v", err)
			}
			defer holder.Rollback(ctx)
			if _, err := holder.Exec(ctx, kind.move, id); err != nil {
				t.Fatalf("move the pending attempt as its sender does: %v", err)
			}

			deleted := make(chan *httptest.ResponseRecorder, 1)
			go func() { deleted <- dispatchRequest(t, handler, http.MethodDelete, kind.route+id, nil, "alice") }()
			storetest.WaitForLockWaiters(t, holder, 1)
			if err := holder.Commit(ctx); err != nil {
				t.Fatalf("commit the sender's write: %v", err)
			}

			refused := testwait.Receive(t, "the delete's answer once the sender's write committed", deleted)
			if code, text := refusalOf(t, refused); refused.Code != http.StatusConflict || code != deleteContended ||
				text != "a send of this post moved while the delete was being written; nothing was written, and the same delete can be sent again" {
				t.Fatalf("a delete whose send moved under it: status=%d body=%s, want 409 DELETE_CONTENDED",
					refused.Code, refused.Body.String())
			}
			if got := refused.Header().Get("Retry-After"); got != "1" {
				t.Fatalf("the refusal's Retry-After = %q, want 1", got)
			}
			var body, state string
			var markedDeleted bool
			var kept int
			if err := database.Pool.QueryRow(ctx, `
				select p.body, p.deleted_at is not null,
				       (select count(*) from post_body_history h where h.`+kind.owner+` = p.id),
				       (select state from `+kind.attempts+` d where d.`+kind.owner+` = p.id)
				from `+kind.posts+` p where p.id = $1
			`, id).Scan(&body, &markedDeleted, &kept, &state); err != nil {
				t.Fatalf("read the post after the refused delete: %v", err)
			}
			if body == "" || markedDeleted || kept != 0 || state != model.DeliveryStatePending {
				t.Fatalf("after the refused delete the post reads body %q, deleted %v, %d kept bodies, its attempt %s; "+
					"want it standing with nothing kept and the attempt pending: the delete wrote nothing", body, markedDeleted, kept, state)
			}

			if again := dispatchRequest(t, handler, http.MethodDelete, kind.route+id, nil, "alice"); again.Code != http.StatusOK {
				t.Fatalf("the same delete sent again: status=%d body=%s, want 200", again.Code, again.Body.String())
			}
		})
	}
}
