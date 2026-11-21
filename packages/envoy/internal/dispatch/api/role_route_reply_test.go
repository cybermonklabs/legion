package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// roleRouteListener stands in for the Envoy listener of one Dispatch deployment in the tests of a
// reply to an attempt sent for a route to a role: for each role, the session its claim names now
// and whether that session is live, as GET /v1/roles/<role> answers them (200 for a live holder,
// 404 holder_lapsed naming the holder for one that is not), and every session's own record. A
// send is stored for its target and answered as the listener answers one.
type roleRouteListener struct {
	mu      sync.Mutex
	holders map[string]string
	lapsed  map[string]bool
	// silent answers every role lookup 503, as a listener whose bus does not answer does.
	silent bool
	server *httptest.Server
}

func newRoleRouteListener(t *testing.T) *roleRouteListener {
	t.Helper()
	listener := &roleRouteListener{holders: map[string]string{}, lapsed: map[string]bool{}}
	listener.server = httptest.NewServer(http.HandlerFunc(listener.serve))
	t.Cleanup(listener.server.Close)
	return listener
}

// claim makes session the holder of role, live or not.
func (l *roleRouteListener) claim(role, session string, live bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holders[role] = session
	l.lapsed[session] = !live
}

func (l *roleRouteListener) serve(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	record := func(session string) map[string]any {
		state := "live"
		if l.lapsed[session] {
			state = "unresponsive"
		}
		return map[string]any{"session_id": session, "title": session, "capabilities": []string{"btw", "steer", "aside"}, "state": state}
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/roles/") && l.silent:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"envoy: the message bus did not answer: timeout"}`))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/roles/"):
		role := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
		holder, claimed := l.holders[role]
		switch {
		case !claimed:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "no holder for role " + role, "reason": "unclaimed"})
		case l.lapsed[holder]:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "role " + role + " holder " + holder + " is unresponsive", "reason": "holder_lapsed",
				"holder": holder, "holder_state": "unresponsive", "claim_released": false,
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"role": role, "holder": holder, "title": holder, "capabilities": []string{"btw", "steer", "aside"}, "holder_state": "live",
			})
		}
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
		_ = json.NewEncoder(w).Encode(record(strings.TrimPrefix(r.URL.Path, "/v1/registry/")))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
		rows := []map[string]any{}
		for session := range l.lapsed {
			rows = append(rows, record(session))
		}
		_ = json.NewEncoder(w).Encode(rows)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
		var input struct {
			TargetSession  string `json:"target_session"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"event_id": "envelope-" + input.IdempotencyKey, "recipient": input.TargetSession, "state": "connected",
		})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}
}

// roleRouteReplies is one Dispatch over a roleRouteListener, with an issue to send on.
type roleRouteReplies struct {
	listener *roleRouteListener
	handler  http.Handler
	database *store.Store
	issue    string
}

func newRoleRouteReplies(t *testing.T) *roleRouteReplies {
	t.Helper()
	return newRoleRouteRepliesTraced(t, nil)
}

// newRoleRouteRepliesTraced is newRoleRouteReplies with tracer on every connection of Dispatch's
// pool (testServerOptions.tracer); nil traces nothing.
func newRoleRouteRepliesTraced(t *testing.T, tracer pgx.QueryTracer) *roleRouteReplies {
	t.Helper()
	listener := newRoleRouteListener(t)
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.server.URL, tracer: tracer})
	issue := createInteractionIssue(t, handler, "TEST", "Replies to a role route", "before")
	return &roleRouteReplies{listener: listener, handler: handler, database: database, issue: issue.Key}
}

// message sends a targeted message to role, whose claim names holder, and returns it once its
// first attempt went to holder.
func (rig *roleRouteReplies) message(t *testing.T, role, holder, body string) model.Message {
	t.Helper()
	message := createIssueMessage(t, rig.handler, rig.issue, map[string]any{
		"body": body, "target": "role:" + role, "delivery": "steer",
	}, "alice")
	if got := message.Deliveries; len(got) != 1 || got[0].SessionID != holder || got[0].State != "sent" {
		t.Fatalf("the message to role:%s = %#v, want one attempt sent to %s", role, got, holder)
	}
	return message
}

// mention posts a comment mentioning role, whose claim names holder, and returns it once its
// mention's first attempt went to holder.
func (rig *roleRouteReplies) mention(t *testing.T, role, holder, body string) mentionedCommentRead {
	t.Helper()
	comment := decodeMentionedComment(t, postMentionedComment(t, rig.handler, rig.issue, map[string]any{
		"body": body, "delivery": "steer", "mentions": []map[string]any{{"target": "role:" + role}},
	}))
	attempts := settledCommentDeliveries(t, rig.database, comment.ID)
	if len(attempts) != 1 || attempts[0].SessionID == nil || *attempts[0].SessionID != holder || attempts[0].State != "sent" {
		t.Fatalf("the mention of role:%s = %#v, want one attempt sent to %s", role, attempts, holder)
	}
	return comment
}

// replyMessage is one reply callback on attempt 1 of message, as session's host makes it: a body
// or an error, with follow_up when asked.
func (rig *roleRouteReplies) replyMessage(t *testing.T, message model.Message, session string, reply map[string]any, followUp bool) *httptest.ResponseRecorder {
	t.Helper()
	reply["actor"] = map[string]any{"kind": "session", "id": session}
	reply["attempt"] = 1
	path := "/api/v1/messages/" + message.ID + "/reply"
	if followUp {
		path += "?follow_up=true"
	}
	return bearerRequest(t, rig.handler, http.MethodPost, path, reply)
}

// replyComment is one reply callback on attempt 1 of comment's one mention.
func (rig *roleRouteReplies) replyComment(t *testing.T, comment mentionedCommentRead, session string, reply map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	reply["actor"] = map[string]any{"kind": "session", "id": session}
	reply["attempt"] = 1
	return bearerRequest(t, rig.handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", reply)
}

// messageAttempt reads attempt 1 of message: the session it names, its state and its reply.
func (rig *roleRouteReplies) messageAttempt(t *testing.T, message model.Message) (session, state string, replyID *string) {
	t.Helper()
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select session_id, state, reply_id::text from message_deliveries where message_id = $1 and attempt = 1
	`, message.ID).Scan(&session, &state, &replyID); err != nil {
		t.Fatalf("read attempt 1 of message %s: %v", message.ID, err)
	}
	return session, state, replyID
}

// commentAttempt reads attempt 1 of comment's one mention.
func (rig *roleRouteReplies) commentAttempt(t *testing.T, comment mentionedCommentRead) (session *string, state string, replyID *string) {
	t.Helper()
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select session_id, state, reply_id::text from comment_deliveries where comment_id = $1 and attempt = 1
	`, comment.ID).Scan(&session, &state, &replyID); err != nil {
		t.Fatalf("read attempt 1 of comment %s: %v", comment.ID, err)
	}
	return session, state, replyID
}

// replyRefused fails the test unless response is the 403 REPLY_FORBIDDEN a session that may not
// answer the attempt is told.
func replyRefused(t *testing.T, what string, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"REPLY_FORBIDDEN"`) {
		t.Fatalf("%s: status=%d body=%s, want 403 REPLY_FORBIDDEN", what, response.Code, response.Body.String())
	}
}

type roleRouteMessageReply struct {
	ID        string      `json:"id"`
	Body      string      `json:"body"`
	Author    model.Actor `json:"author"`
	InReplyTo *string     `json:"in_reply_to"`
	Duplicate bool        `json:"duplicate"`
}

type roleRouteCommentReply struct {
	ID      string      `json:"id"`
	Body    string      `json:"body"`
	Author  model.Actor `json:"author"`
	ReplyTo *string     `json:"reply_to"`
}

// Dispatch delivers a message and a mention for a route to role R to the session R's claim names
// when it sends, holder A. A reply to that attempt, and an error callback on it, are accepted
// from A, and from the session R's claim names at reply time while that session is live: a
// different session, B, that took R from A answers what A never read. The attempt's first stored
// reply stays its reply, and a later one is answered as the reply routes answer a second reply
// today. Any other session, and R's holder while it is not live, is refused 403 REPLY_FORBIDDEN.
// The rows are those of the check c-dispatch-role-route that the reply routes decide: (1), (3)
// and (4). Rows (2), (5) and (6) are TestADispatchMessageForARoleReachesTheSessionThatTakesTheRole
// in cmd/listener, which runs the real listener.
func TestAReplyToARoleRouteAttemptComesFromTheRolesHolder(t *testing.T) {
	const holderA, holderB, other = "ses_role_a", "ses_role_b", "ses_role_other"

	t.Run("(1) a different session takes R: its reply and its error callback are accepted", func(t *testing.T) {
		rig := newRoleRouteReplies(t)
		const role = "reply-takeover"
		rig.listener.claim(role, holderA, true)
		answered := rig.message(t, role, holderA, "Which release carries the fix?")
		failed := rig.message(t, role, holderA, "Can you run the migration?")
		answeredMention := rig.mention(t, role, holderA, "Please review the plan.")
		failedMention := rig.mention(t, role, holderA, "Please rerun the tests.")
		rig.listener.claim(role, holderB, true)

		reply := rig.replyMessage(t, answered, holderB, map[string]any{"body": "Release 4 carries it."}, false)
		if reply.Code != http.StatusCreated {
			t.Fatalf("the new holder's reply to the message: status=%d body=%s, want 201", reply.Code, reply.Body.String())
		}
		if stored := decodeBody[roleRouteMessageReply](t, reply); stored.Author.ID != holderB || stored.Body != "Release 4 carries it." {
			t.Fatalf("the new holder's reply = %+v, want B's body authored by %s", stored, holderB)
		}
		if session, state, replyID := rig.messageAttempt(t, answered); session != holderA || state != "sent" || replyID == nil {
			t.Fatalf("the answered attempt = session %s state %s reply %v, want A's attempt, sent, with B's reply", session, state, replyID)
		}
		callback := rig.replyMessage(t, failed, holderB, map[string]any{"error": "No migration tool on this host"}, false)
		if callback.Code != http.StatusOK {
			t.Fatalf("the new holder's error callback on the message: status=%d body=%s, want 200", callback.Code, callback.Body.String())
		}
		if attempt := decodeBody[model.MessageDelivery](t, callback); attempt.State != "failed" || attempt.Error == nil || *attempt.Error != "No migration tool on this host" {
			t.Fatalf("the message attempt after the new holder's error callback = %+v, want failed with its error", attempt)
		}

		mentionReply := rig.replyComment(t, answeredMention, holderB, map[string]any{"body": "The plan holds."})
		if mentionReply.Code != http.StatusCreated {
			t.Fatalf("the new holder's reply to the mention: status=%d body=%s, want 201", mentionReply.Code, mentionReply.Body.String())
		}
		if stored := decodeBody[roleRouteCommentReply](t, mentionReply); stored.Author.ID != holderB || stored.ReplyTo == nil || *stored.ReplyTo != answeredMention.ID {
			t.Fatalf("the new holder's reply to the mention = %+v, want B's reply under %s", stored, answeredMention.ID)
		}
		mentionCallback := rig.replyComment(t, failedMention, holderB, map[string]any{"error": "The test runner is down"})
		if mentionCallback.Code != http.StatusOK {
			t.Fatalf("the new holder's error callback on the mention: status=%d body=%s, want 200", mentionCallback.Code, mentionCallback.Body.String())
		}
		if _, state, _ := rig.commentAttempt(t, failedMention); state != "failed" {
			t.Fatalf("the mention attempt after the new holder's error callback is %s, want failed", state)
		}
	})

	t.Run("(3) the previous holder replied first: the attempt keeps that reply and the second is answered as today", func(t *testing.T) {
		rig := newRoleRouteReplies(t)
		const role = "reply-first-kept"
		rig.listener.claim(role, holderA, true)
		message := rig.message(t, role, holderA, "Is the index rebuilt?")
		comment := rig.mention(t, role, holderA, "Is the backfill done?")

		first := rig.replyMessage(t, message, holderA, map[string]any{"body": "Rebuilt at noon."}, false)
		if first.Code != http.StatusCreated {
			t.Fatalf("the previous holder's reply: status=%d body=%s", first.Code, first.Body.String())
		}
		firstReply := decodeBody[roleRouteMessageReply](t, first)
		firstMention := rig.replyComment(t, comment, holderA, map[string]any{"body": "Done on Tuesday."})
		if firstMention.Code != http.StatusCreated {
			t.Fatalf("the previous holder's mention reply: status=%d body=%s", firstMention.Code, firstMention.Body.String())
		}
		firstMentionReply := decodeBody[roleRouteCommentReply](t, firstMention)
		rig.listener.claim(role, holderB, true)

		second := rig.replyMessage(t, message, holderB, map[string]any{"body": "Not rebuilt yet."}, false)
		if second.Code != http.StatusOK {
			t.Fatalf("the new holder's second reply: status=%d body=%s, want 200 with the stored reply", second.Code, second.Body.String())
		}
		if stored := decodeBody[roleRouteMessageReply](t, second); stored.ID != firstReply.ID || stored.Author.ID != holderA || stored.Body != "Rebuilt at noon." || !stored.Duplicate {
			t.Fatalf("the second reply's answer = %+v, want the first reply, A's, as a duplicate", stored)
		}
		followUp := rig.replyMessage(t, message, holderB, map[string]any{"body": "Rebuilt again after the restore."}, true)
		if followUp.Code != http.StatusCreated {
			t.Fatalf("the new holder's follow-up: status=%d body=%s, want 201", followUp.Code, followUp.Body.String())
		}
		posted := decodeBody[roleRouteMessageReply](t, followUp)
		if posted.Author.ID != holderB || posted.InReplyTo == nil || *posted.InReplyTo != firstReply.ID {
			t.Fatalf("the follow-up = %+v, want B's message under the first reply %s", posted, firstReply.ID)
		}
		resent := rig.replyMessage(t, message, holderB, map[string]any{"body": "Rebuilt again after the restore."}, true)
		if resent.Code != http.StatusOK || decodeBody[roleRouteMessageReply](t, resent).ID != posted.ID {
			t.Fatalf("a retry of the posted follow-up: status=%d body=%s, want 200 with the follow-up %s", resent.Code, resent.Body.String(), posted.ID)
		}
		if session, _, replyID := rig.messageAttempt(t, message); session != holderA || replyID == nil || *replyID != firstReply.ID {
			t.Fatalf("the attempt = session %s reply %v, want A's attempt keeping its first reply %s", session, replyID, firstReply.ID)
		}

		secondMention := rig.replyComment(t, comment, holderB, map[string]any{"body": "Not done."})
		if secondMention.Code != http.StatusOK {
			t.Fatalf("the new holder's second mention reply: status=%d body=%s, want 200 with the stored reply", secondMention.Code, secondMention.Body.String())
		}
		if stored := decodeBody[roleRouteCommentReply](t, secondMention); stored.ID != firstMentionReply.ID || stored.Author.ID != holderA || stored.Body != "Done on Tuesday." {
			t.Fatalf("the second mention reply's answer = %+v, want the first reply, A's", stored)
		}
		if session, _, replyID := rig.commentAttempt(t, comment); session == nil || *session != holderA || replyID == nil || *replyID != firstMentionReply.ID {
			t.Fatalf("the mention attempt = session %v reply %v, want A's attempt keeping its first reply %s", session, replyID, firstMentionReply.ID)
		}
	})

	t.Run("(4) a session neither the attempt nor R's claim names is refused, and so is R's holder while it is not live", func(t *testing.T) {
		rig := newRoleRouteReplies(t)
		const role = "reply-refused"
		rig.listener.claim(role, holderA, true)
		message := rig.message(t, role, holderA, "Who owns the deploy?")
		comment := rig.mention(t, role, holderA, "Who owns the rollback?")
		rig.listener.claim(role, holderB, true)

		replyRefused(t, "a reply to the message from a session nothing names", rig.replyMessage(t, message, other, map[string]any{"body": "I do."}, false))
		replyRefused(t, "an error callback on the message from a session nothing names", rig.replyMessage(t, message, other, map[string]any{"error": "no"}, false))
		replyRefused(t, "a reply to the mention from a session nothing names", rig.replyComment(t, comment, other, map[string]any{"body": "I do."}))
		replyRefused(t, "an error callback on the mention from a session nothing names", rig.replyComment(t, comment, other, map[string]any{"error": "no"}))

		rig.listener.claim(role, holderB, false)
		replyRefused(t, "a reply to the message from R's holder while it is not live", rig.replyMessage(t, message, holderB, map[string]any{"body": "I do."}, false))
		replyRefused(t, "a reply to the mention from R's holder while it is not live", rig.replyComment(t, comment, holderB, map[string]any{"body": "I do."}))
		if _, state, replyID := rig.messageAttempt(t, message); state != "sent" || replyID != nil {
			t.Fatalf("the message attempt after the refusals = %s with reply %v, want sent and unanswered", state, replyID)
		}
		if _, state, replyID := rig.commentAttempt(t, comment); state != "sent" || replyID != nil {
			t.Fatalf("the mention attempt after the refusals = %s with reply %v, want sent and unanswered", state, replyID)
		}
	})
}

// Whether a session the attempt does not name holds the role is the listener's to say, so a role
// lookup the listener does not answer is a 503 ENVOY_UNAVAILABLE the reply's host can make again,
// never a refusal that ends a reply the role's holder may well be entitled to. It names the route
// and not the session replying, since a handler's error path carries it. The session the attempt
// names is answered as ever, with no lookup at all.
func TestAReplyWhoseRoleHolderTheListenerDoesNotNameIsAskedAgain(t *testing.T) {
	rig := newRoleRouteReplies(t)
	const role = "reply-unanswered"
	rig.listener.claim(role, "ses_role_a", true)
	message := rig.message(t, role, "ses_role_a", "Is the cache warm?")
	comment := rig.mention(t, role, "ses_role_a", "Is the queue drained?")
	rig.listener.claim(role, "ses_role_b", true)
	rig.listener.mu.Lock()
	rig.listener.silent = true
	rig.listener.mu.Unlock()

	for what, response := range map[string]*httptest.ResponseRecorder{
		"the message": rig.replyMessage(t, message, "ses_role_b", map[string]any{"body": "Warm."}, false),
		"the mention": rig.replyComment(t, comment, "ses_role_b", map[string]any{"body": "Drained."}),
	} {
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"ENVOY_UNAVAILABLE"`) {
			t.Fatalf("the role's holder's reply to %s with the lookup unanswered: status=%d body=%s, want 503 ENVOY_UNAVAILABLE", what, response.Code, response.Body.String())
		}
		if body := response.Body.String(); !strings.Contains(body, "role:"+role) || strings.Contains(body, "ses_role_b") {
			t.Fatalf("the 503 to the reply to %s = %s, want it naming role:%s and not the session replying", what, body, role)
		}
	}
	if answered := rig.replyMessage(t, message, "ses_role_a", map[string]any{"body": "Warm, says A."}, false); answered.Code != http.StatusCreated {
		t.Fatalf("the attempt's own session's reply with the lookup unanswered: status=%d body=%s, want 201", answered.Code, answered.Body.String())
	}
}

// routeReadFault ends the reply routes' read of the route and the attempt a reply answers
// (roleHolderReplyOf) while armed, as a statement timeout or a reset pooled connection ends it:
// that one statement is sent with a context already ended, and every other statement runs.
type routeReadFault struct{ armed atomic.Bool }

func (f *routeReadFault) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	read := strings.Contains(data.SQL, "from messages m left join message_deliveries d") ||
		strings.Contains(data.SQL, "from comment_mentions m\n\t\tleft join comment_deliveries d")
	if !f.armed.Load() || !read {
		return ctx
	}
	ended, cancel := context.WithCancel(ctx)
	cancel()
	return ended
}

func (*routeReadFault) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A reply from the role's live holder, to an attempt that names another session, needs the read of
// the attempt's route and session before the listener is asked. A read that fails says nothing of
// who may answer, so the reply is answered 500 and stores nothing, never refused 403
// REPLY_FORBIDDEN: the holder's host makes the reply again. The control is the same two replies
// once the read answers, which are accepted, so the 500 comes from the failed read alone.
func TestAReplyWhoseRouteReadFailsIsNotRefused(t *testing.T) {
	fault := &routeReadFault{}
	rig := newRoleRouteRepliesTraced(t, fault)
	const role = "reply-read-fails"
	rig.listener.claim(role, "ses_role_a", true)
	message := rig.message(t, role, "ses_role_a", "Is the index rebuilt?")
	comment := rig.mention(t, role, "ses_role_a", "Is the backfill done?")
	rig.listener.claim(role, "ses_role_b", true)

	fault.armed.Store(true)
	for what, response := range map[string]*httptest.ResponseRecorder{
		"the message": rig.replyMessage(t, message, "ses_role_b", map[string]any{"body": "Rebuilt."}, false),
		"the mention": rig.replyComment(t, comment, "ses_role_b", map[string]any{"body": "Done."}),
	} {
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "REPLY_FORBIDDEN") {
			t.Fatalf("the role's live holder's reply to %s while the route read fails: status=%d body=%s, want 500 and no refusal", what, response.Code, response.Body.String())
		}
	}
	if _, _, replyID := rig.messageAttempt(t, message); replyID != nil {
		t.Fatalf("the message's attempt has reply %s after a reply answered 500; want none", *replyID)
	}
	if _, _, replyID := rig.commentAttempt(t, comment); replyID != nil {
		t.Fatalf("the mention's attempt has reply %s after a reply answered 500; want none", *replyID)
	}

	fault.armed.Store(false)
	if response := rig.replyMessage(t, message, "ses_role_b", map[string]any{"body": "Rebuilt."}, false); response.Code != http.StatusCreated {
		t.Fatalf("control: the same reply to the message once the read answers: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	if response := rig.replyComment(t, comment, "ses_role_b", map[string]any{"body": "Done."}); response.Code != http.StatusCreated {
		t.Fatalf("control: the same reply to the mention once the read answers: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
}
