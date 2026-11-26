package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// roleRouteListener stands in for the Envoy listener of one Dispatch deployment in the tests of a
// reply to an attempt that a session the attempt does not name may answer: for each role, the
// session its claim names now and whether that session is live, as GET /v1/roles/<role> answers
// them (200 for a live holder, 404 holder_lapsed naming the holder for one that is not); for each
// session, the sessions that replaced it and the principal it names them for, as GET
// /v1/sessions/<session>/successors answers them; and every session's own record. A send is
// stored for its target and answered as the listener
// answers one.
type roleRouteListener struct {
	mu         sync.Mutex
	holders    map[string]string
	lapsed     map[string]bool
	successors map[string][]string
	principals map[string]string
	// silent answers every role lookup and every successor read 503, as a listener whose bus does
	// not answer does.
	silent bool
	server *httptest.Server
}

func newRoleRouteListener(t *testing.T) *roleRouteListener {
	t.Helper()
	listener := &roleRouteListener{holders: map[string]string{}, lapsed: map[string]bool{}, successors: map[string][]string{}, principals: map[string]string{}}
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

// replace names successor as a session that replaced previous, as a claim that named previous
// as the session it replaces has the listener's handover record name it, both sessions having
// registered on a credential that proves no principal.
func (l *roleRouteListener) replace(previous, successor string) {
	l.replaceAs(previous, successor, "")
}

// replaceAs is replace where both sessions last registered as principal.
func (l *roleRouteListener) replaceAs(previous, successor, principal string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.successors[previous] = append(l.successors[previous], successor)
	l.principals[previous] = principal
}

// silence makes the listener answer every role lookup and successor read 503.
func (l *roleRouteListener) silence() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.silent = true
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
	successorsOf, readsSuccessors := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/successors")
	readsSuccessors = readsSuccessors && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sessions/")
	switch {
	case (readsSuccessors || r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/roles/")) && l.silent:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"envoy: the message bus did not answer: timeout"}`))
	case readsSuccessors:
		successors := append([]string{}, l.successors[successorsOf]...)
		_ = json.NewEncoder(w).Encode(map[string]any{"session_id": successorsOf, "principal": l.principals[successorsOf], "successors": successors})
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
	return rig.mentionOf(t, "role:"+role, holder, body)
}

// mentionOf posts a comment mentioning target and returns it once its mention's first attempt
// went to holder.
func (rig *roleRouteReplies) mentionOf(t *testing.T, target, holder, body string) mentionedCommentRead {
	t.Helper()
	comment := decodeMentionedComment(t, postMentionedComment(t, rig.handler, rig.issue, map[string]any{
		"body": body, "delivery": "steer", "mentions": []map[string]any{{"target": target}},
	}))
	attempts := settledCommentDeliveries(t, rig.database, comment.ID)
	if len(attempts) != 1 || attempts[0].SessionID == nil || *attempts[0].SessionID != holder || attempts[0].State != "sent" {
		t.Fatalf("the mention of %s = %#v, want one attempt sent to %s", target, attempts, holder)
	}
	return comment
}

// direct sends a person's direct message to session, with no issue, as the Agents page sends one
// (POST /api/v1/agents/{session_id}/messages), and returns it once its first attempt went there.
func (rig *roleRouteReplies) direct(t *testing.T, session, body string) model.Message {
	t.Helper()
	response := postAgentMessage(t, rig.handler, session, map[string]any{"body": body, "delivery": "steer"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("a direct message to %s: status=%d body=%s", session, response.Code, response.Body.String())
	}
	message := decodeBody[model.Message](t, response)
	if got := message.Deliveries; len(got) != 1 || got[0].SessionID != session || got[0].State != "sent" {
		t.Fatalf("the direct message to %s = %#v, want one attempt sent to it", session, got)
	}
	return message
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
	t.Parallel()
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
// never a refusal that ends a reply the role's holder may well be entitled to. The session the
// attempt names is answered as ever, with no lookup at all.
func TestAReplyWhoseRoleHolderTheListenerDoesNotNameIsAskedAgain(t *testing.T) {
	t.Parallel()
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
	}
	if answered := rig.replyMessage(t, message, "ses_role_a", map[string]any{"body": "Warm, says A."}, false); answered.Code != http.StatusCreated {
		t.Fatalf("the attempt's own session's reply with the lookup unanswered: status=%d body=%s, want 201", answered.Code, answered.Body.String())
	}
}

// A person's direct message to session A, and a mention of A, go to A. A claim that named A as the
// session it replaces has the listener copy what A had not read to the claimant, B, each copy
// keeping A's attempt, so B answers that attempt: its reply and its error callback are accepted
// while the listener names B among A's successors (GET /v1/sessions/<A>/successors), and the
// attempt keeps A as its session and B's reply as its reply. A session the listener names for no
// session, and one it names as the successor of another session, are refused 403
// REPLY_FORBIDDEN, and so is B while the listener does not name it. A successor read the listener
// does not answer is a 503 ENVOY_UNAVAILABLE the reply's host can make again, and A's own reply is
// answered as ever, with no read at all.
func TestAReplyToADirectMessageComesFromTheSessionThatReplacedItsRecipient(t *testing.T) {
	t.Parallel()
	const replaced, successor, other = "ses_handover_a", "ses_handover_b", "ses_handover_other"

	t.Run("(a) the session that replaced the recipient answers its direct message and its mention", func(t *testing.T) {
		rig := newRoleRouteReplies(t)
		answered := rig.direct(t, replaced, "Which release carries the fix?")
		failed := rig.direct(t, replaced, "Can you run the migration?")
		mention := rig.mentionOf(t, "session:"+replaced, replaced, "Please review the plan.")
		rig.listener.replace(replaced, successor)

		reply := rig.replyMessage(t, answered, successor, map[string]any{"body": "Release 4 carries it."}, false)
		if reply.Code != http.StatusCreated {
			t.Fatalf("the successor's reply to the direct message: status=%d body=%s, want 201", reply.Code, reply.Body.String())
		}
		if stored := decodeBody[roleRouteMessageReply](t, reply); stored.Author.ID != successor || stored.Body != "Release 4 carries it." {
			t.Fatalf("the successor's reply = %+v, want its body authored by %s", stored, successor)
		}
		if session, state, replyID := rig.messageAttempt(t, answered); session != replaced || state != "sent" || replyID == nil {
			t.Fatalf("the answered attempt = session %s state %s reply %v, want A's attempt, sent, with B's reply", session, state, replyID)
		}
		callback := rig.replyMessage(t, failed, successor, map[string]any{"error": "No migration tool on this host"}, false)
		if callback.Code != http.StatusOK {
			t.Fatalf("the successor's error callback on the direct message: status=%d body=%s, want 200", callback.Code, callback.Body.String())
		}
		if attempt := decodeBody[model.MessageDelivery](t, callback); attempt.State != "failed" {
			t.Fatalf("the attempt after the successor's error callback = %+v, want failed", attempt)
		}

		mentionReply := rig.replyComment(t, mention, successor, map[string]any{"body": "The plan holds."})
		if mentionReply.Code != http.StatusCreated {
			t.Fatalf("the successor's reply to the mention: status=%d body=%s, want 201", mentionReply.Code, mentionReply.Body.String())
		}
		if stored := decodeBody[roleRouteCommentReply](t, mentionReply); stored.Author.ID != successor || stored.ReplyTo == nil || *stored.ReplyTo != mention.ID {
			t.Fatalf("the successor's reply to the mention = %+v, want B's reply under %s", stored, mention.ID)
		}
		if session, _, replyID := rig.commentAttempt(t, mention); session == nil || *session != replaced || replyID == nil {
			t.Fatalf("the mention's attempt = session %v reply %v, want A's attempt with B's reply", session, replyID)
		}
	})

	t.Run("(b) a session the listener does not name as the recipient's successor is refused", func(t *testing.T) {
		rig := newRoleRouteReplies(t)
		message := rig.direct(t, replaced, "Who owns the deploy?")
		mention := rig.mentionOf(t, "session:"+replaced, replaced, "Who owns the rollback?")
		replyRefused(t, "the successor's reply before the listener names it", rig.replyMessage(t, message, successor, map[string]any{"body": "I do."}, false))
		rig.listener.replace(replaced, successor)
		rig.listener.replace("ses_handover_elsewhere", other)

		replyRefused(t, "a reply to the direct message from a session nothing names", rig.replyMessage(t, message, other, map[string]any{"body": "I do."}, false))
		replyRefused(t, "an error callback on the direct message from a session nothing names", rig.replyMessage(t, message, other, map[string]any{"error": "no"}, false))
		replyRefused(t, "a reply to the mention from a session nothing names", rig.replyComment(t, mention, other, map[string]any{"body": "I do."}))
		if _, state, replyID := rig.messageAttempt(t, message); state != "sent" || replyID != nil {
			t.Fatalf("the direct message's attempt after the refusals = %s with reply %v, want sent and unanswered", state, replyID)
		}
		if _, state, replyID := rig.commentAttempt(t, mention); state != "sent" || replyID != nil {
			t.Fatalf("the mention's attempt after the refusals = %s with reply %v, want sent and unanswered", state, replyID)
		}
	})

	t.Run("(c) a successor read the listener does not answer is asked again", func(t *testing.T) {
		rig := newRoleRouteReplies(t)
		message := rig.direct(t, replaced, "Is the cache warm?")
		mention := rig.mentionOf(t, "session:"+replaced, replaced, "Is the queue drained?")
		rig.listener.replace(replaced, successor)
		rig.listener.silence()

		for what, response := range map[string]*httptest.ResponseRecorder{
			"the direct message": rig.replyMessage(t, message, successor, map[string]any{"body": "Warm."}, false),
			"the mention":        rig.replyComment(t, mention, successor, map[string]any{"body": "Drained."}),
		} {
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"ENVOY_UNAVAILABLE"`) {
				t.Fatalf("the successor's reply to %s with the read unanswered: status=%d body=%s, want 503 ENVOY_UNAVAILABLE", what, response.Code, response.Body.String())
			}
		}
		if answered := rig.replyMessage(t, message, replaced, map[string]any{"body": "Warm, says A."}, false); answered.Code != http.StatusCreated {
			t.Fatalf("the attempt's own session's reply with the read unanswered: status=%d body=%s, want 201", answered.Code, answered.Body.String())
		}
	})
}

// A successor answers the replaced session's delivery only on a credential that acts for the
// principal the listener names the successors for (actsForSuccessorPrincipal), in the form that
// names the token's kind and scope (ciphertoken.Caller.Principal): the listener names them by the
// records of the sessions' last registrations, and a session run on another principal's broker
// token before a registration records that may not answer the earlier principal's deliveries. A
// person's direct message goes to session A; the listener names B as A's successor for one
// principal; B replies under a broker token that proves B and acts for a principal of its own.
// Each case row - a token acting for another person, for a service, or for the same person on a
// project an operator's entry names, which is an agent's and not the person's own - is refused 403
// REPLY_FORBIDDEN and leaves the attempt unanswered, and fails where Dispatch takes the listener's
// list alone, or compares the token's person alone. Each control is accepted: a token acting for
// the person, the service, or the project entry the listener names, and the shared agent token,
// which proves no session and may name A itself, and so is asked nothing more.
func TestASuccessorsReplyIsHeldToThePrincipalTheListenerNamesIt(t *testing.T) {
	t.Parallel()
	listener := newRoleRouteListener(t)
	brokered := newBrokerTokenRigOn(t, testServerOptions{envoyURL: listener.server.URL})
	rig := &roleRouteReplies{listener: listener, handler: brokered.handler, database: brokered.database}
	// Each answer is the broker's for a token of the successor's session: a person-scoped token
	// of a person, an account entry's naming no person (personlessAccountToken, whose service is
	// the whole account subject: one of the person scope naming no person is a machine nobody
	// signed in on, which writes nothing), and a project entry's acting for alice on
	// brokerTestProject (liveNamedToken).
	personToken := func(person string) func(string) ciphertoken.Introspection {
		return func(jkt string) ciphertoken.Introspection {
			answer := personScopedTokenAnswer(jkt)
			answer.Person = person
			return answer
		}
	}
	const (
		alice        = "person:" + brokerTestPerson
		aliceOnEntry = "named:" + brokerTestProject + ":" + brokerTestPerson
		account      = "service:" + brokerTestAccount
	)
	for i, row := range []struct {
		name              string
		named             string
		answer            func(jkt string) ciphertoken.Introspection
		shared, forbidden bool
	}{
		{name: "case: the successor's token acts for another person", named: alice, answer: personToken("bob"), forbidden: true},
		{name: "case: the successor's token acts for a service", named: alice, answer: personlessAccountToken, forbidden: true},
		{name: "case: the successor's token is a project entry's acting for the person whose own session the listener names", named: alice, answer: liveNamedToken, forbidden: true},
		{name: "control: the successor's token acts for the person the listener names", named: alice, answer: personToken(brokerTestPerson)},
		{name: "control: the successor's token acts for the service the listener names", named: account, answer: personlessAccountToken},
		{name: "control: the successor's token is the project entry's the listener names", named: aliceOnEntry, answer: liveNamedToken},
		{name: "control: the shared agent token, which proves no session", named: alice, shared: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			replaced, successor := fmt.Sprintf("ses_principal_a%d", i), fmt.Sprintf("ses_principal_b%d", i)
			message := rig.direct(t, replaced, "Which release carries the fix?")
			listener.replaceAs(replaced, successor, row.named)
			body := map[string]any{"actor": map[string]any{"kind": "session", "id": successor}, "attempt": 1, "body": "Release 4 carries it."}
			path := "/api/v1/messages/" + message.ID + "/reply"
			var reply *httptest.ResponseRecorder
			if row.shared {
				reply = bearerRequest(t, rig.handler, http.MethodPost, path, body)
			} else {
				caller := newBrokerCaller(t)
				token := fmt.Sprintf("a-token-of-%s-row-%d", successor, i)
				answer := row.answer(caller.jkt)
				answer.Session = successor
				brokered.broker.Says(token, answer)
				reply = brokered.serve(caller.presenting(t, http.MethodPost, path, token, body))
			}
			if row.forbidden {
				replyRefused(t, "the successor's reply on a token of another principal", reply)
				if _, state, replyID := rig.messageAttempt(t, message); state != "sent" || replyID != nil {
					t.Fatalf("the attempt after the refusal = %s with reply %v, want sent and unanswered", state, replyID)
				}
				return
			}
			if reply.Code != http.StatusCreated {
				t.Fatalf("the successor's reply: status=%d body=%s, want 201", reply.Code, reply.Body.String())
			}
			if session, _, replyID := rig.messageAttempt(t, message); session != replaced || replyID == nil {
				t.Fatalf("the answered attempt = session %s reply %v, want A's attempt with B's reply", session, replyID)
			}
		})
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
	t.Parallel()
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
