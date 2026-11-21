package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// sjawhar/legion#2230. A role whose holder is gone because the Legion daemon released the role's
// idle issue pod - the registry's newest entry for the role token is `released` and not
// `finished` - is coming back: the daemon resumes the same session on the next wake of the role.
// A person's message to it is kept for that next holder rather than failed, whether or not the
// project's daemon answers for its stopped agents, since no daemon answers this one: Dispatch
// publishes it on the role's topic through the listener's publish route, which keeps it on the
// role's kept lane and wakes the role, and records the attempt taken. Every other role keeps what
// it did before: the finished role's newest session takes the message while its project answers,
// and a role whose claim is not released, or is released with its issue finished, or is finished
// with its project not answering, keeps the listener's no-holder answer.
func TestRoleMessageToAReleasedRoleIsKeptForItsNextHolder(t *testing.T) {
	noHolder := "no holder for role " + registeredRole
	roleTarget := "role:" + registeredRole
	for _, tc := range []struct {
		name                             string
		finished, released, notAnswering bool
		// publishAnswer is the status the listener answers a role publish with (202 when 0).
		publishAnswer int32
		// The outcome both the message's attempt and the mention's record: the state, the
		// session pinned ("" for none), the failure ("" for none), and the prefix of the envelope
		// recorded ("" for none).
		wantState, wantSession, wantFailure, wantEnvelope string
		// published says the message and the mention were each published on the role's topic.
		published bool
	}{
		{
			name: "released, so kept for its next holder", released: true,
			wantState: model.DeliveryStateQueued, wantEnvelope: "kept-", published: true,
		},
		{
			name: "released, its project not answering, so kept all the same", released: true, notAnswering: true,
			wantState: model.DeliveryStateQueued, wantEnvelope: "kept-", published: true,
		},
		{
			name: "released, and a live session claimed the role by the publish", released: true, publishAnswer: http.StatusOK,
			wantState: model.DeliveryStateSent, wantEnvelope: "delivered-", published: true,
		},
		{
			name: "released, and the listener kept nothing", released: true, publishAnswer: http.StatusNotFound,
			wantState: model.DeliveryStateFailed, wantFailure: noHolder, published: true,
		},
		{name: "not released", wantState: model.DeliveryStateFailed, wantFailure: noHolder},
		{
			name: "finished", finished: true,
			wantState: model.DeliveryStateSent, wantSession: stoppedNewest, wantEnvelope: "handed-off-",
		},
		{name: "released and finished", finished: true, released: true, wantState: model.DeliveryStateFailed, wantFailure: noHolder},
		{
			name: "finished, its project not answering", finished: true, notAnswering: true,
			wantState: model.DeliveryStateFailed, wantFailure: noHolder,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := newStoppedAgentListener(t)
			listener.roleRuns.Store(!tc.finished)
			listener.released.Store(tc.released)
			listener.notAnswering.Store(tc.notAnswering)
			listener.publishAnswer.Store(tc.publishAnswer)
			handler, database := newTargetedMessageHandler(t, listener.URL)
			issue := createInteractionIssue(t, handler, "TEST", "Released role", "before")
			envelopeLike := func(envelope *string) bool {
				if tc.wantEnvelope == "" {
					return envelope == nil
				}
				return envelope != nil && strings.HasPrefix(*envelope, tc.wantEnvelope)
			}

			message := createIssueMessage(t, handler, issue.Key, map[string]any{
				"body": "Who holds this?", "target": roleTarget, "delivery": "aside",
			}, "alice")
			if got := message.Deliveries; len(got) != 1 || got[0].State != tc.wantState || got[0].SessionID != tc.wantSession ||
				deref(got[0].Error) != tc.wantFailure || !envelopeLike(got[0].EnvelopeID) {
				t.Fatalf("the role message's deliveries = %#v, want one %s attempt to %q failing %q with an envelope %q",
					got, tc.wantState, tc.wantSession, tc.wantFailure, tc.wantEnvelope)
			}

			comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
				"body": "Can the planner explain this?", "delivery": "btw",
				"mentions": []map[string]any{{"target": roleTarget}},
			}))
			settled := settledCommentDeliveries(t, database, comment.ID)
			if len(settled) != 1 || settled[0].State != tc.wantState || deref(settled[0].SessionID) != tc.wantSession ||
				deref(settled[0].Error) != tc.wantFailure || !envelopeLike(settled[0].EnvelopeID) {
				t.Fatalf("the role mention's attempts = %#v, want one %s attempt to %q failing %q with an envelope %q",
					settled, tc.wantState, tc.wantSession, tc.wantFailure, tc.wantEnvelope)
			}
			if row, want := commentAttemptRow(t, database, comment.ID, roleTarget, 1),
				tc.wantState+"/"+tc.wantFailure+"//"+tc.wantSession; row != want {
				t.Fatalf("the role mention's attempt row = %q, want %q", row, want)
			}

			published := listener.published()
			if !tc.published {
				if len(published) != 0 {
					t.Fatalf("listener publishes = %#v, want none", published)
				}
			} else {
				if len(published) != 2 {
					t.Fatalf("listener publishes = %#v, want the message's and the mention's", published)
				}
				for i, want := range []struct{ key, expectsReply, mode string }{
					{key: message.ID + ":aside", expectsReply: "optional", mode: "aside"},
					{key: model.CommentMentionIdempotencyKey(comment.ID, roleTarget, "btw"), expectsReply: "required", mode: "btw"},
				} {
					got := published[i]
					if got["topic"] != "notifications.role."+registeredRole || got["keep"] != true || got["source"] != "dispatch" ||
						got["idempotency_key"] != want.key || got["expects_reply"] != want.expectsReply {
						t.Fatalf("publish %d = %#v, want it on the role's topic, kept, from dispatch, under %q expecting %q",
							i, got, want.key, want.expectsReply)
					}
					if _, named := got["target_session"]; named {
						t.Fatalf("publish %d = %#v, want it to name no session", i, got)
					}
					var frame struct {
						Delivery struct {
							Attempt int    `json:"attempt"`
							Mode    string `json:"mode"`
						} `json:"delivery"`
					}
					payload, _ := got["payload"].(string)
					if err := json.Unmarshal([]byte(payload), &frame); err != nil || frame.Delivery.Attempt != 1 ||
						frame.Delivery.Mode != want.mode {
						t.Fatalf("publish %d's payload = %q (%v), want the delivery frame of attempt 1 in %s", i, payload, err, want.mode)
					}
				}
			}
			sent := listener.sent()
			if tc.wantSession == "" && len(sent) != 0 {
				t.Fatalf("listener sends = %v, want none", sent)
			}
			if tc.wantSession != "" && (len(sent) != 2 || sent[0] != tc.wantSession || sent[1] != tc.wantSession) {
				t.Fatalf("listener sends = %v, want the message and the mention sent to %s", sent, tc.wantSession)
			}
		})
	}
}

// A message and a mention Dispatch kept for a released role name no session, and the listener
// hands them to whichever session holds the role next. That session's reply is taken: the session
// the listener names as the role's holder when the reply comes may answer, and its reply pins the
// attempt to it, so the attempt reads answered, the message's thread carries the reply the card
// shows, and every later reply is judged by the check every reply passes. A session that does not
// hold the role is refused, a listener that does not answer is a 503 the holder retries, and once
// the attempt is pinned a later holder of the role is refused as any other session is.
func TestTheReleasedRolesNextHolderMayReplyToWhatWasKeptForIt(t *testing.T) {
	const holder, intruder, successor = "woken-planner", "intruder", "successor-planner"
	listener := newStoppedAgentListener(t)
	listener.roleRuns.Store(true)
	listener.released.Store(true)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Released role reply", "before")
	roleTarget := "role:" + registeredRole

	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Who holds this?", "target": roleTarget, "delivery": "aside",
	}, "alice")
	if got := message.Deliveries; len(got) != 1 || got[0].State != model.DeliveryStateQueued || got[0].SessionID != "" {
		t.Fatalf("the role message's deliveries = %#v, want one queued attempt naming no session", got)
	}
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can the planner explain this?", "delivery": "btw",
		"mentions": []map[string]any{{"target": roleTarget}},
	}))
	if settled := settledCommentDeliveries(t, database, comment.ID); len(settled) != 1 ||
		settled[0].State != model.DeliveryStateQueued || settled[0].SessionID != nil {
		t.Fatalf("the role mention's attempts = %#v, want one queued attempt naming no session", settled)
	}
	replyToMention := func(sessionID string) *httptest.ResponseRecorder {
		return bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
			"actor": map[string]any{"kind": "session", "id": sessionID}, "attempt": 1, "target": roleTarget,
			"body": "Answered by " + sessionID + ".",
		})
	}
	messageSession := func() string {
		var session string
		if err := database.Pool.QueryRow(t.Context(),
			`select coalesce(session_id, '') from message_deliveries where message_id = $1 and attempt = 1`, message.ID,
		).Scan(&session); err != nil {
			t.Fatalf("read the message's attempt: %v", err)
		}
		return session
	}
	forbidden := func(name string, response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"REPLY_FORBIDDEN"`) {
			t.Fatalf("%s: status=%d body=%s, want 403 REPLY_FORBIDDEN", name, response.Code, response.Body.String())
		}
	}
	listener.roleHolder.Store(new(holder))

	// A session that does not hold the role is refused, and nothing is pinned.
	forbidden("the intruder's reply to the message", replyAs(t, handler, message.ID, intruder, "Not mine."))
	forbidden("the intruder's reply to the mention", replyToMention(intruder))
	if session := messageSession(); session != "" {
		t.Fatalf("the message's attempt names %q after a refused reply, want no session", session)
	}

	// A listener that does not answer cannot say who holds the role: the holder is told to try
	// again, not refused.
	listener.rolesDown.Store(true)
	for name, response := range map[string]*httptest.ResponseRecorder{
		"the message": replyAs(t, handler, message.ID, holder, "Retry me."),
		"the mention": replyToMention(holder),
	} {
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"ENVOY_UNAVAILABLE"`) {
			t.Fatalf("the holder's reply to %s while the listener is down: status=%d body=%s, want 503 ENVOY_UNAVAILABLE",
				name, response.Code, response.Body.String())
		}
	}
	listener.rolesDown.Store(false)

	// The role's holder answers both, and each attempt is pinned to it and reads answered.
	assertAnswered(t, "the holder's reply to the message", replyAs(t, handler, message.ID, holder, "I hold it now."), message.ID)
	if answered := replyToMention(holder); answered.Code != http.StatusCreated {
		t.Fatalf("the holder's reply to the mention: status=%d body=%s, want 201", answered.Code, answered.Body.String())
	}
	if session := messageSession(); session != holder {
		t.Fatalf("the message's attempt names %q after the holder's reply, want %s", session, holder)
	}
	if row := commentAttemptRow(t, database, comment.ID, roleTarget, 1); !strings.HasPrefix(row, "sent//") ||
		!strings.HasSuffix(row, "/"+holder) || row == "sent///"+holder {
		t.Fatalf("the mention's attempt row = %q, want it sent, answered and pinned to %s", row, holder)
	}
	// The message's card is drawn from the issue's message list: its thread carries the holder's
	// reply to it, which the card reads as the message's answer.
	var answer *shapedMessage
	for _, row := range issueMessageRows(t, handler, issue.Key) {
		if row.Message.ID != message.ID {
			continue
		}
		if got := row.Message.Deliveries; len(got) != 1 || got[0].SessionID != holder || got[0].ReplyID == nil {
			t.Fatalf("the listed message's deliveries = %#v, want its attempt answered by %s", got, holder)
		}
		for i := range row.Replies {
			if reply := &row.Replies[i]; reply.InReplyTo != nil && *reply.InReplyTo == message.ID && reply.Author.ID == holder {
				answer = reply
			}
		}
	}
	if answer == nil || answer.Body != "I hold it now." {
		t.Fatalf("the listed message's thread carries %#v, want the holder's reply", answer)
	}

	// Pinned, the attempt is the holder's alone: a session that holds the role later is refused.
	listener.roleHolder.Store(new(successor))
	forbidden("the successor's reply to the message", replyAs(t, handler, message.ID, successor, "Me too."))
	forbidden("the successor's reply to the mention", replyToMention(successor))
}
