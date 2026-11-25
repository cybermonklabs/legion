package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// heldRowRequest is one row of TestARequestBehindARowAnotherTransactionHoldsIsRefusedBusyAtItsBound:
// the server, the statement that holds the row the request waits on, and the request.
type heldRowRequest struct {
	handler  http.Handler
	database *store.Store
	// hold, with holdArgs, takes the row in the test's own transaction, as another writer would.
	hold     string
	holdArgs []any
	// method, path and body are the request, sent with the session bearer when session is set and
	// as alice otherwise; written is its answer once the row is free.
	method, path string
	body         any
	session      bool
	written      int
	// call, when set, is made in place of a request: the path a request or a sender takes to the
	// row below its handler, whose refusal at the bound is store.ErrPoolBusy, and which returns
	// nil once the row is free.
	call func(ctx context.Context) error
}

// A request whose transaction waits on a row lock another transaction holds waits no longer than a
// write waits for any lock another writer holds (the store's lockWait), and is refused 503
// DATABASE_BUSY with Retry-After at that bound, so its client sends it again; once the row is free
// the same request goes through. An admitted write has no deadline, and none of these rows is
// ordered by an owner lock the request holds against every other writer of it, so without the
// bound the request held its pool connection and its admission unit for as long as the holder held
// the row. One row per route family whose lock takes the bound: a direct message's row (the
// accept; the claim takes the same row first), a delivery attempt's row (a session's reply; the
// claim takes the same rows), a write phase's lock of the message a reply names a block of
// (store.WriteBatch.QueueWithinBound, which an item's block state takes too), a comment delivery's
// attempt (a session's reply; the claim takes the same rows), the Inbox's Mark all read and its
// dismiss (each with the row its own lock order reaches first), and an agent's read mark's prune.
// Then one row per write of such a row that waited on it with no lock taken first: the accept's
// write of its attempt, an agent's read mark's own row, an architecture source's save and removal,
// a document's removal's clear of the inbox threads that name it (whose other writers take the
// document's owner lock, where the removal holds its issue's), and, below the handlers, a mention
// sender's record of its resolution and its settle, a keyed repeat's claim of an owed mention (a
// write of its own on the background pool), and a direct message sender's settle and claim
// renewal, each refused store.ErrPoolBusy, which the routes that reach them answer 503 as above.
// The bound is the test's (store.WithLockWaitForTest) rather than production's ten seconds, and the
// holder is a transaction of the test's own that holds the row until the test lets it go.
func TestARequestBehindARowAnotherTransactionHoldsIsRefusedBusyAtItsBound(t *testing.T) {
	const bound = time.Second
	for _, row := range []struct {
		name    string
		arrange func(t *testing.T) heldRowRequest
	}{
		{
			name: "a session's accept behind its direct message's row",
			arrange: func(t *testing.T) heldRowRequest {
				handler, database, root, _, _ := directConversationFrom(t, "alice")
				return heldRowRequest{
					handler: handler, database: database,
					hold: `select 1 from messages where id = $1 for update`, holdArgs: []any{root.ID},
					method: http.MethodPost, path: "/api/v1/messages/" + root.ID + "/deliveries/1/accept",
					body:    map[string]any{"actor": map[string]any{"kind": "session", "id": "s1"}},
					session: true, written: http.StatusOK,
				}
			},
		},
		{
			name: "a session's reply behind its delivery attempt's row",
			arrange: func(t *testing.T) heldRowRequest {
				handler, database, root, _, _ := directConversationFrom(t, "alice")
				return heldRowRequest{
					handler: handler, database: database,
					hold:     `select 1 from message_deliveries where message_id = $1 and attempt = 1 for update`,
					holdArgs: []any{root.ID},
					method:   http.MethodPost, path: "/api/v1/messages/" + root.ID + "/reply?follow_up=true",
					body: map[string]any{
						"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "On it.",
					},
					session: true, written: http.StatusCreated,
				}
			},
		},
		{
			name: "a reply naming a block of a direct message behind that message's row",
			arrange: func(t *testing.T) heldRowRequest {
				live := true
				sent := []map[string]any{}
				listener := sessionListener(t, &live, &sent)
				t.Cleanup(listener.Close)
				handler, database := newTargetedMessageHandler(t, listener.URL)
				direct := decodeBody[model.Message](t, postAgentMessage(t, handler, "s1", map[string]any{
					"body": "Direct one.\n\nDirect two.", "delivery": "btw",
				}, "alice"))
				return heldRowRequest{
					handler: handler, database: database,
					hold: `select 1 from messages where id = $1 for update`, holdArgs: []any{direct.ID},
					method: http.MethodPost, path: "/api/v1/agents/s1/messages",
					body: map[string]any{
						"body": "On direct two.", "delivery": "btw", "in_reply_to": direct.ID, "in_reply_to_block": "b2",
					},
					written: http.StatusCreated,
				}
			},
		},
		{
			name: "a session's reply to a comment mention behind its delivery attempt's row",
			arrange: func(t *testing.T) heldRowRequest {
				live := true
				sent := []map[string]any{}
				listener := sessionListener(t, &live, &sent)
				t.Cleanup(listener.Close)
				handler, database := newTargetedMessageHandler(t, listener.URL)
				issue := createInteractionIssue(t, handler, "TEST", "A reply behind a held attempt", "before")
				mentioned := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
					"body": "Can this ship?", "mentions": []map[string]any{{"target": "session:s1"}},
				}))
				// The mention sender settles the attempt after the create answers; the row is held
				// once it has, so the sender is not what waits on it.
				settledCommentDeliveries(t, database, mentioned.ID)
				return heldRowRequest{
					handler: handler, database: database,
					hold: `
						select 1 from comment_deliveries
						where comment_id = $1 and target = 'session:s1' and attempt = 1 for update`,
					holdArgs: []any{mentioned.ID},
					method:   http.MethodPost, path: "/api/v1/comments/" + mentioned.ID + "/reply",
					body: map[string]any{
						"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "It ships.",
					},
					session: true, written: http.StatusCreated,
				}
			},
		},
		{
			name: "a Mark all read behind the caller's own thread row",
			arrange: func(t *testing.T) heldRowRequest {
				handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
				issue := createInteractionIssue(t, handler, "TEST", "Marked behind a held row", "A spec")
				thread := seedUnreadThread(t, handler, "/api/v1/issues/"+issue.Key+"/comments", "Ready to ship?")
				return heldRowRequest{
					handler: handler, database: database,
					hold:     `select 1 from user_threads where login = 'alice' and thread_id = $1 for no key update`,
					holdArgs: []any{thread},
					method:   http.MethodPost, path: "/api/v1/me/inbox/read",
					body:    map[string]any{"read_through": time.Now().UTC().Format(time.RFC3339Nano)},
					written: http.StatusOK,
				}
			},
		},
		{
			name: "a dismiss of an Agent thread behind its root's row",
			arrange: func(t *testing.T) heldRowRequest {
				live := true
				sent := []map[string]any{}
				listener := sessionListener(t, &live, &sent)
				t.Cleanup(listener.Close)
				handler, database := newTargetedMessageHandler(t, listener.URL)
				root, _ := agentThread(t, handler, "btw", "Deploy is tonight.", "Noted.")
				return heldRowRequest{
					handler: handler, database: database,
					hold: `select 1 from messages where id = $1 for update`, holdArgs: []any{root.ID},
					method: http.MethodPost, path: "/api/v1/me/inbox/dismiss",
					body:    dismissBody(t, database, "alice", []string{inboxDismissAgentPrefix + root.ID}),
					written: http.StatusOK,
				}
			},
		},
		{
			name: "an agent's read mark behind a reply row its prune deletes",
			arrange: func(t *testing.T) heldRowRequest {
				live := true
				sent := []map[string]any{}
				listener := sessionListener(t, &live, &sent)
				t.Cleanup(listener.Close)
				handler, database := newTargetedMessageHandler(t, listener.URL)
				_, reply := agentThread(t, handler, "btw", "Deploy is tonight.", "Noted.")
				// The reply read by id, as a view that shows it marks it: the row the read mark that
				// passes it then prunes.
				if marked := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
					"read_replies": []string{reply.ID},
				}, "alice"); marked.Code != http.StatusOK {
					t.Fatalf("mark the reply read by id: status=%d body=%s", marked.Code, marked.Body.String())
				}
				return heldRowRequest{
					handler: handler, database: database,
					hold:     `select 1 from user_agent_reply_read where login = 'alice' and reply_id = $1 for update`,
					holdArgs: []any{reply.ID},
					method:   http.MethodPut, path: "/api/v1/me/agents/s1/state",
					body:    map[string]any{"read_through": time.Now().UTC().Format(time.RFC3339Nano)},
					written: http.StatusOK,
				}
			},
		},
		{
			name: "a session's accept behind its delivery attempt's row",
			arrange: func(t *testing.T) heldRowRequest {
				handler, database, root, _, _ := directConversationFrom(t, "alice")
				return heldRowRequest{
					handler: handler, database: database,
					hold:     `select 1 from message_deliveries where message_id = $1 and attempt = 1 for update`,
					holdArgs: []any{root.ID},
					method:   http.MethodPost, path: "/api/v1/messages/" + root.ID + "/deliveries/1/accept",
					body:    map[string]any{"actor": map[string]any{"kind": "session", "id": "s1"}},
					session: true, written: http.StatusOK,
				}
			},
		},
		{
			name: "an agent's read mark behind the caller's own read row",
			arrange: func(t *testing.T) heldRowRequest {
				handler, database, _ := newTestServer(t, testServerOptions{})
				mark := map[string]any{"read_through": time.Now().UTC().Format(time.RFC3339Nano)}
				if marked := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", mark, "alice"); marked.Code != http.StatusOK {
					t.Fatalf("mark the agent read: status=%d body=%s", marked.Code, marked.Body.String())
				}
				return heldRowRequest{
					handler: handler, database: database,
					hold:   `select 1 from user_agent_read where login = 'alice' and session_id = 's1' for update`,
					method: http.MethodPut, path: "/api/v1/me/agents/s1/state",
					body:    map[string]any{"read_through": time.Now().UTC().Format(time.RFC3339Nano)},
					written: http.StatusOK,
				}
			},
		},
		{
			name: "a save of an architecture source behind its row",
			arrange: func(t *testing.T) heldRowRequest {
				handler, database := savedArchitectureSource(t)
				return heldRowRequest{
					handler: handler, database: database,
					hold:   `select 1 from architecture_sources where project_key = 'CORE' and repo = 'legion/arch' for update`,
					method: http.MethodPut, path: sourcePath("CORE", "legion/arch"),
					body:    map[string]any{"branch": "release"},
					written: http.StatusOK,
				}
			},
		},
		{
			name: "a removal of an architecture source behind its row",
			arrange: func(t *testing.T) heldRowRequest {
				handler, database := savedArchitectureSource(t)
				return heldRowRequest{
					handler: handler, database: database,
					hold:   `select 1 from architecture_sources where project_key = 'CORE' and repo = 'legion/arch' for update`,
					method: http.MethodDelete, path: sourcePath("CORE", "legion/arch"),
					body:    map[string]any{},
					written: http.StatusNoContent,
				}
			},
		},
		{
			name: "a document's removal behind an inbox thread row that names the document",
			arrange: func(t *testing.T) heldRowRequest {
				handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
				createInteractionIssue(t, handler, "TEST", "A spec of its own", "# Spec\n\nA passage.\n")
				issue := titleIssue(t, handler, "A document made by mistake")
				document := emptyDocument(t, handler, issue.Key, "Lauch notes")
				// No write of Dispatch's names an issue's document in a thread row - the thread
				// trigger records artifact_id only for a thread with no issue, a project document's -
				// so the row is planted directly: the removal's clear is the cleanup for the day that
				// rule stops holding (artifactRemovalClears).
				if _, err := database.Pool.Exec(context.Background(), `
					insert into user_threads (login, thread_id, kind, project_key, artifact_id, read_through)
					values ('alice', gen_random_uuid(), 'comment', $1, $2, now())`, issue.Project, document.ID); err != nil {
					t.Fatalf("plant an inbox thread row naming the document: %v", err)
				}
				return heldRowRequest{
					handler: handler, database: database,
					hold: `select 1 from user_threads where artifact_id = $1 for update`, holdArgs: []any{document.ID},
					method: http.MethodDelete, path: "/api/v1/artifacts/" + document.ID,
					written: http.StatusNoContent,
				}
			},
		},
		{
			name: "a mention sender's record of its resolution behind its attempt's row",
			arrange: func(t *testing.T) heldRowRequest {
				database, served, comment, claim := reopenedMentionAttempt(t, true)
				return heldRowRequest{
					database: database, hold: heldMentionAttemptRow, holdArgs: []any{comment.ID},
					call: func(ctx context.Context) error {
						_, _, held, err := served.recordCommentDeliveryResolution(ctx, comment,
							ResolvedMention{Target: "session:s1", Delivery: claim.delivery}, claim.attemptClaim, nil)
						if err == nil && !held {
							return errors.New("the record found the attempt no longer held by its claim")
						}
						return err
					},
				}
			},
		},
		{
			name: "a mention sender's settle behind its attempt's row",
			arrange: func(t *testing.T) heldRowRequest {
				database, served, comment, claim := reopenedMentionAttempt(t, true)
				return heldRowRequest{
					database: database, hold: heldMentionAttemptRow, holdArgs: []any{comment.ID},
					call: func(ctx context.Context) error {
						settled, err := served.completeCommentDelivery(ctx, comment, model.CommentDelivery{
							CommentID: comment.ID, Target: "session:s1", Attempt: 1, Delivery: claim.delivery,
						}, model.Actor{Kind: "user", ID: "alice"}, claim.attemptClaim, sendOutcome{})
						if err == nil && settled.State != model.DeliveryStateSent {
							return fmt.Errorf("the settle left the attempt %s, want sent", settled.State)
						}
						return err
					},
				}
			},
		},
		{
			name: "a keyed repeat's claim of an owed mention behind its attempt's row",
			arrange: func(t *testing.T) heldRowRequest {
				database, served, comment, _ := reopenedMentionAttempt(t, false)
				return heldRowRequest{
					database: database, hold: heldMentionAttemptRow, holdArgs: []any{comment.ID},
					call: func(ctx context.Context) error {
						_, mine, err := served.claimQueuedMention(ctx, mentionKey{comment: comment.ID, target: "session:s1", attempt: 1})
						if err == nil && !mine {
							return errors.New("the claim found the owed attempt held by another sender")
						}
						return err
					},
				}
			},
		},
		{
			name: "a direct message sender's settle behind its attempt's row",
			arrange: func(t *testing.T) heldRowRequest {
				database, served, root, pending := reopenedDirectAttempt(t)
				return heldRowRequest{
					database: database, hold: heldDirectAttemptRow, holdArgs: []any{root.ID},
					call: func(ctx context.Context) error {
						settled, err := served.completeMessageDelivery(ctx, root, model.Actor{Kind: "user", ID: "alice"}, pending, sendOutcome{})
						if err == nil && settled.State != model.DeliveryStateSent {
							return fmt.Errorf("the settle left the attempt %s, want sent", settled.State)
						}
						return err
					},
				}
			},
		},
		{
			name: "a direct message sender's claim renewal behind its attempt's row",
			arrange: func(t *testing.T) heldRowRequest {
				database, served, root, pending := reopenedDirectAttempt(t)
				return heldRowRequest{
					database: database, hold: heldDirectAttemptRow, holdArgs: []any{root.ID},
					call: func(ctx context.Context) error {
						_, held, err := served.renewMessageDeliveryClaim(ctx, root, pending, pending.resolved)
						if err == nil && !held {
							return errors.New("the renewal found the attempt no longer held by its claim")
						}
						return err
					},
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			arranged := row.arrange(t)
			// send makes the request, or the call, under the test's bound: a call's answer is its
			// error alone.
			send := func() (*httptest.ResponseRecorder, error) {
				ctx := store.WithLockWaitForTest(context.Background(), bound)
				if arranged.call != nil {
					return nil, arranged.call(ctx)
				}
				data, err := json.Marshal(arranged.body)
				if err != nil {
					t.Errorf("encode the request: %v", err)
				}
				request := httptest.NewRequest(arranged.method, arranged.path, bytes.NewReader(data)).WithContext(ctx)
				request.Header.Set("Content-Type", "application/json")
				if arranged.session {
					request.Header.Set("Authorization", "Bearer agent-token")
				} else {
					request.Header.Set("X-Dispatch-User", "alice")
				}
				response := httptest.NewRecorder()
				arranged.handler.ServeHTTP(response, request)
				return response, nil
			}

			ctx := context.Background()
			holder, err := arranged.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the holder: %v", err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			held, err := holder.Exec(ctx, arranged.hold, arranged.holdArgs...)
			if err != nil {
				t.Fatalf("hold the row: %v", err)
			}
			if held.RowsAffected() != 1 {
				t.Fatalf("the holder took %d rows, want the one row the request waits on", held.RowsAffected())
			}

			type answer struct {
				response *httptest.ResponseRecorder
				err      error
			}
			answered := make(chan answer, 1)
			started := time.Now()
			go func() {
				response, err := send()
				answered <- answer{response: response, err: err}
			}()
			if waiting := storetest.WaitForLockWaiters(t, holder, 1); waiting < 1 {
				t.Fatalf("the request never queued behind the row the holder holds")
			}
			var got answer
			select {
			case got = <-answered:
			case <-time.After(10 * time.Second):
				waited := time.Since(started)
				_ = holder.Rollback(ctx)
				<-answered
				t.Fatalf("the request was still waiting on the held row %s after it was sent, past a write's lock bound of %s: "+
					"it waits on a held row until its holder lets go", waited.Round(time.Millisecond), bound)
			}
			waited := time.Since(started)
			if response := got.response; arranged.call != nil {
				if !store.IsPoolBusy(got.err) {
					t.Fatalf("the call behind the held row: %v, want store.ErrPoolBusy", got.err)
				}
			} else if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"`+admission.CodeDatabaseBusy+`"`) ||
				response.Header().Get("Retry-After") == "" {
				t.Fatalf("the request behind the held row: status=%d Retry-After=%q body=%s, want 503 %s with Retry-After",
					response.Code, response.Header().Get("Retry-After"), response.Body.String(), admission.CodeDatabaseBusy)
			}
			if waited < bound {
				t.Fatalf("the request was refused %s after it was sent, inside its bound of %s", waited.Round(time.Millisecond), bound)
			}
			if waiting := storetest.WaitForLockWaiters(t, holder, 0); waiting != 0 {
				t.Fatalf("%d sessions still wait behind the held row after the refusal, want none", waiting)
			}

			if err := holder.Rollback(ctx); err != nil {
				t.Fatalf("let the row go: %v", err)
			}
			again, err := send()
			switch {
			case arranged.call != nil && err != nil:
				t.Fatalf("the call made again once the row was free: %v", err)
			case arranged.call == nil && again.Code != arranged.written:
				t.Fatalf("the request sent again once the row was free: status=%d body=%s, want %d",
					again.Code, again.Body.String(), arranged.written)
			}
		})
	}
}

// A statement of a request transaction that waits on a lock no bounded wait of its own covers - a
// plain advisory lock, a plain locking read - waits no longer than a write's lock bound (the
// store's lockWait), on the request pool exactly as production opens it (storetest.Open, through
// store.Open): the transaction carries the bound from its BEGIN (store.Pool.Begin), and the
// pool's connections carry it for a statement outside a transaction. Postgres refuses the wait
// lock_not_available (55P03), lock_timeout covering an advisory lock as it covers a row lock, and
// the refusal is answered as every lock wait that ran out its bound is: store.IsPoolBusy, which
// the api's error mapping (admission.WriteUnserved, writeHandlerError) answers 503 DATABASE_BUSY
// with Retry-After. Once the lock is free the same statement goes through. The bound is the
// test's (store.WithLockWaitForTest), which a request transaction's BEGIN takes from its context;
// the test gives up well before production's ten seconds, which the connection's own setting
// would end the wait at.
func TestAPlainLockWaitOfARequestTransactionIsRefusedBusyAtTheWriteLockBound(t *testing.T) {
	const bound = time.Second
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `create table plain_lock_wait (id int primary key); insert into plain_lock_wait values (1)`); err != nil {
		t.Fatalf("make a row to hold: %v", err)
	}
	for _, row := range []struct {
		name, lock string
		args       []any
	}{
		{name: "a plain advisory lock behind a key another transaction holds", lock: `select pg_advisory_xact_lock($1)`, args: []any{int64(592622)}},
		{name: "a plain locking read behind a row another transaction holds", lock: `select id from plain_lock_wait where id = $1 for update`, args: []any{1}},
	} {
		t.Run(row.name, func(t *testing.T) {
			holder, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the holder: %v", err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if _, err := holder.Exec(ctx, row.lock, row.args...); err != nil {
				t.Fatalf("hold the lock: %v", err)
			}
			request := store.WithLockWaitForTest(store.WithTransactionTracking(ctx), bound)
			lock := func() error {
				tx, err := database.Pool.Begin(request)
				if err != nil {
					return fmt.Errorf("begin the request transaction: %w", err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if _, err := tx.Exec(request, row.lock, row.args...); err != nil {
					return fmt.Errorf("take the lock: %w", err)
				}
				return tx.Commit(request)
			}

			answered := make(chan error, 1)
			started := time.Now()
			go func() { answered <- lock() }()
			if waiting := storetest.WaitForLockWaiters(t, holder, 1); waiting < 1 {
				t.Fatalf("the request transaction never queued behind the held lock")
			}
			var refused error
			select {
			case refused = <-answered:
			case <-time.After(bound + 4*time.Second):
				waited := time.Since(started)
				_ = holder.Rollback(ctx)
				<-answered
				t.Fatalf("the request transaction was still waiting on the held lock %s after it began, past a write's lock bound of %s: "+
					"the request pool bounds no plain lock wait", waited.Round(time.Millisecond), bound)
			}
			waited := time.Since(started)
			if !store.IsPoolBusy(refused) {
				t.Fatalf("the request transaction behind the held lock: %v, want a refusal store.IsPoolBusy reads", refused)
			}
			if waited < bound {
				t.Fatalf("the request transaction was refused %s after it began, inside its bound of %s", waited.Round(time.Millisecond), bound)
			}
			answer := httptest.NewRecorder()
			if !admission.WriteUnserved(answer, request, refused, true) || answer.Code != http.StatusServiceUnavailable ||
				!strings.Contains(answer.Body.String(), `"code":"`+admission.CodeDatabaseBusy+`"`) || answer.Header().Get("Retry-After") == "" {
				t.Fatalf("the refusal answered: status=%d Retry-After=%q body=%s, want 503 %s with Retry-After",
					answer.Code, answer.Header().Get("Retry-After"), answer.Body.String(), admission.CodeDatabaseBusy)
			}

			if err := holder.Rollback(ctx); err != nil {
				t.Fatalf("let the lock go: %v", err)
			}
			if err := lock(); err != nil {
				t.Fatalf("the request transaction once the lock was free: %v", err)
			}
		})
	}
}

// The rows reopenedMentionAttempt's and reopenedDirectAttempt's callers hold: attempt 1 of the
// comment's mention of s1, and of the direct message.
const (
	heldMentionAttemptRow = `
		select 1 from comment_deliveries where comment_id = $1 and target = 'session:s1' and attempt = 1 for update`
	heldDirectAttemptRow = `select 1 from message_deliveries where message_id = $1 and attempt = 1 for update`
)

// reopenedClaim is the claim a sender holds on the attempt reopenedMentionAttempt reopens, with the
// attempt's mode.
type reopenedClaim struct {
	attemptClaim
	delivery string
}

// reopenedMentionAttempt is a comment mentioning s1 on a server sending to a live s1, with its
// attempt 1 set back to pending once its sender settled it, as a sender leaves an attempt it holds
// and has not settled: under a claim taken now when claimed is set, and otherwise under none, as
// an attempt owed again is. It answers the store, the server, the comment and that claim.
func reopenedMentionAttempt(t *testing.T, claimed bool) (*store.Store, *server, model.Comment, reopenedClaim) {
	t.Helper()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createInteractionIssue(t, handler, "TEST", "A sender's write behind a held attempt", "before")
	mentioned := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, database, mentioned.ID)
	claim := "null"
	if claimed {
		claim = "now()"
	}
	var claimedAt *time.Time
	reopened := reopenedClaim{attemptClaim: attemptClaim{attempt: 1}}
	if err := database.Pool.QueryRow(context.Background(), `
		update comment_deliveries set state = 'pending', claimed_at = `+claim+`
		where comment_id = $1 and target = 'session:s1' and attempt = 1
		returning claimed_at, delivery`, mentioned.ID).Scan(&claimedAt, &reopened.delivery); err != nil {
		t.Fatalf("set the mention's attempt back to pending: %v", err)
	}
	if claimedAt != nil {
		reopened.claimedAt = *claimedAt
	}
	comment, err := served.loadComment(context.Background(), database.Pool, mentioned.ID)
	if err != nil {
		t.Fatalf("load the comment: %v", err)
	}
	return database, served, comment, reopened
}

// reopenedDirectAttempt is alice's direct message to a live s1, with its attempt 1 set back to
// pending under a claim taken now once its own request settled it, as a sender leaves an attempt
// it holds and has not settled. It answers the store, the server, the message and the attempt as
// that sender holds it.
func reopenedDirectAttempt(t *testing.T) (*store.Store, *server, model.Message, pendingMessageDelivery) {
	t.Helper()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	created := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "Where is the dashboard?", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message: status=%d body=%s", created.Code, created.Body.String())
	}
	root := decodeBody[model.Message](t, created)
	pending := pendingMessageDelivery{
		attemptClaim: attemptClaim{attempt: 1},
		resolved:     ResolvedMention{Target: "session:s1", Delivery: "aside"},
	}
	if err := database.Pool.QueryRow(context.Background(), `
		update message_deliveries set state = 'pending', claimed_at = now()
		where message_id = $1 and attempt = 1 and state <> 'pending'
		returning claimed_at`, root.ID).Scan(&pending.claimedAt); err != nil {
		t.Fatalf("set the direct message's settled attempt back to pending: %v", err)
	}
	return database, served, root, pending
}

// savedArchitectureSource is a server whose CORE project has legion/arch saved as its
// architecture source on main.
func savedArchitectureSource(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database := newArchitectureSourceServer(t, &fakeGitHubApp{
		installations: map[string]string{"legion/arch": "read"},
		repos:         map[string]*fakeArchitectureRepo{"legion/arch": {commit: shaOne, files: archFiles()}},
	})
	createTestProject(t, handler, "CORE")
	putSource(t, handler, "CORE", "legion/arch", "main")
	return handler, database
}
