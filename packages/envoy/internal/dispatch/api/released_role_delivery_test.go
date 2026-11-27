package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/listenerwire"
)

// sjawhar/legion#2230. A Legion agent whose issue pod the daemon released because it was idle has
// stopped as any other stopped agent has, and its daemon decides what a message to it does: its
// answer loop answers a question for it and wakes it for everything else. So Dispatch resolves a
// released session, and a role whose newest entry is a released session, to that session while
// its project answers, as it resolves any stopped agent the registry names (registeredTarget,
// unheldRoleTarget): the attempt is sent there, the listener hands it to the agent's daemon on
// notifications.legion.<project>.agents.<session> and raises no wake, and nothing is published on
// the role's topic, whose keep would raise the role's wake. The control is the same agent of a
// project that does not answer, whose loop is off: the listener's own answer stands, as with no
// registry.
//
// The stand-in listener answers each entry with `released: true` (stoppedAgentListener.released),
// as the listener of 043f9c67 answered the entry its daemon wrote for a claim idle release
// suspended. Dispatch of 043f9c67 read that field: it queued a message to the session on the
// session's own subject, failed a mention of it, and kept a message and a mention of the role for
// its next holder, which is what these tests fail on with that code.

// requireHandedToTheDaemon fails unless the stand-in listener handed each of its sends to
// sessionID's daemon, count in all, and published nothing for them.
func requireHandedToTheDaemon(t *testing.T, listener *stoppedAgentListener, sessionID string, count int) {
	t.Helper()
	subject := listenerwire.LegionHandoffSubject("test", sessionID)
	if got := listener.handedOff(); len(got) != count || slices.ContainsFunc(got, func(handed string) bool { return handed != subject }) {
		t.Fatalf("the listener handed off %v, want %d hand-offs on %s", got, count, subject)
	}
	if got := listener.published(); len(got) != 0 {
		t.Fatalf("listener publishes = %#v, want none: a publish on the role's topic keeps the message and wakes the role", got)
	}
}

// A message and a mention of a released session are sent to that session, the listener hands each
// to its daemon, and the session's reply, which its daemon posts, answers the message.
func TestReleasedAgentsMessageAndMentionAreSentToItsSession(t *testing.T) {
	t.Parallel()
	listener := newStoppedAgentListener(t)
	listener.released.Store(true)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Released agent", "before")

	onIssue := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Why did you pick this?", "target": "session:" + stoppedAgent, "delivery": "btw",
	}, "alice")
	assertHandedOff(t, "a message to the released session", onIssue, stoppedAgent)
	sessionTarget := "session:" + stoppedAgent
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can you explain this?", "delivery": "btw", "mentions": []map[string]any{{"target": sessionTarget}},
	}))
	settled := settledCommentDeliveries(t, database, comment.ID)
	if len(settled) != 1 || settled[0].State != model.DeliveryStateSent || deref(settled[0].SessionID) != stoppedAgent || settled[0].Error != nil {
		t.Fatalf("the mention of the released session = %#v, want attempt 1 sent to %s", settled, stoppedAgent)
	}
	requireHandedToTheDaemon(t, listener, stoppedAgent, 2)
	assertAnswered(t, "the message to the released session", replyAs(t, handler, onIssue.ID, stoppedAgent, "Because of the spec."), onIssue.ID)

	// Control: the project does not answer, so the listener's answer stands - no live session -
	// and Dispatch queues the message on the session's own subject itself.
	control := newStoppedAgentListener(t)
	control.released.Store(true)
	control.notAnswering.Store(true)
	controlHandler, _ := newTargetedMessageHandler(t, control.URL)
	controlIssue := createInteractionIssue(t, controlHandler, "TEST", "Released agent, loop off", "before")
	queued := createIssueMessage(t, controlHandler, controlIssue.Key, map[string]any{
		"body": "Anyone?", "target": "session:" + stoppedAgent, "delivery": "btw",
	}, "alice")
	if got := queued.Deliveries; len(got) != 1 || got[0].State != model.DeliveryStateQueued || got[0].SessionID != stoppedAgent || got[0].EnvelopeID == nil {
		t.Fatalf("control: deliveries = %#v, want queued for %s by Dispatch, as with no registry", got, stoppedAgent)
	}
	if got := control.sent(); len(got) != 0 {
		t.Fatalf("control: listener sends = %v, want none", got)
	}
}

// A message and a picker mention of a role (`role:<token>`) whose newest entry is a released
// session are sent to that session, pinned to it, and the listener hands each to its daemon; that
// session's reply answers each. Nothing is kept for the role.
func TestReleasedRolesMessageAndPickerMentionAreSentToItsNewestSession(t *testing.T) {
	t.Parallel()
	listener := newStoppedAgentListener(t)
	listener.released.Store(true)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Released role", "before")
	roleTarget := "role:" + registeredRole

	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Who planned this?", "target": roleTarget, "delivery": "aside",
	}, "alice")
	assertHandedOff(t, "a message to the released role", message, stoppedNewest)
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can the planner explain this?", "delivery": "btw", "mentions": []map[string]any{{"target": roleTarget}},
	}))
	settled := settledCommentDeliveries(t, database, comment.ID)
	if len(settled) != 1 || settled[0].State != model.DeliveryStateSent || deref(settled[0].SessionID) != stoppedNewest || settled[0].Error != nil {
		t.Fatalf("the picker mention of the released role = %#v, want attempt 1 sent to %s", settled, stoppedNewest)
	}
	if row := commentAttemptRow(t, database, comment.ID, roleTarget, 1); row != "sent///"+stoppedNewest {
		t.Fatalf("the picker mention's attempt row = %q, want sent to %s", row, stoppedNewest)
	}
	requireHandedToTheDaemon(t, listener, stoppedNewest, 2)
	assertAnswered(t, "the message to the released role", replyAs(t, handler, message.ID, stoppedNewest, "I did."), message.ID)
	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": stoppedNewest}, "attempt": 1, "target": roleTarget,
		"body": "Answered by " + stoppedNewest + "'s daemon.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("%s's reply to the picker mention: status=%d body=%s, want 201", stoppedNewest, answered.Code, answered.Body.String())
	}

	// Control: the project does not answer, so the listener's answer stands - no holder - and
	// nothing is kept or sent.
	control := newStoppedAgentListener(t)
	control.released.Store(true)
	control.notAnswering.Store(true)
	controlHandler, _ := newTargetedMessageHandler(t, control.URL)
	controlIssue := createInteractionIssue(t, controlHandler, "TEST", "Released role, loop off", "before")
	failed := createIssueMessage(t, controlHandler, controlIssue.Key, map[string]any{
		"body": "Who planned this?", "target": roleTarget, "delivery": "aside",
	}, "alice")
	if got := failed.Deliveries; len(got) != 1 || got[0].State != model.DeliveryStateFailed || deref(got[0].Error) != "no holder for role "+registeredRole {
		t.Fatalf("control: deliveries = %#v, want failed with the listener's no-holder answer", got)
	}
	if sent, published := control.sent(), control.published(); len(sent) != 0 || len(published) != 0 {
		t.Fatalf("control: listener sends %v and publishes %#v, want none", sent, published)
	}
}

// A role the registry holds no entry for, while the role read says its project answers, has a
// first holder coming: a person's message and mention of it are kept for that holder rather than
// failed. Dispatch publishes each on the role's topic through the listener's publish route, which
// keeps it on the role's kept lane and wakes the role, and records the attempt taken. A role with
// an entry goes to its newest session instead, and a role with no entry whose project does not
// answer keeps the listener's no-holder answer.
func TestRoleMessageToARoleWithNoEntryIsKeptForItsFirstHolder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		role         string
		notAnswering bool
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
			name: "no entry, so kept for its first holder", role: unregisteredRole,
			wantState: model.DeliveryStateQueued, wantEnvelope: "kept-", published: true,
		},
		{
			name: "no entry, and a live session claimed the role by the publish", role: unregisteredRole, publishAnswer: http.StatusOK,
			wantState: model.DeliveryStateSent, wantEnvelope: "delivered-", published: true,
		},
		{
			name: "no entry, and the listener kept nothing", role: unregisteredRole, publishAnswer: http.StatusNotFound,
			wantState: model.DeliveryStateFailed, wantFailure: "no holder for role " + unregisteredRole, published: true,
		},
		{
			name: "no entry, its project not answering", role: unregisteredRole, notAnswering: true,
			wantState: model.DeliveryStateFailed, wantFailure: "no holder for role " + unregisteredRole,
		},
		{
			name: "an entry, so its newest session", role: registeredRole,
			wantState: model.DeliveryStateSent, wantSession: stoppedNewest, wantEnvelope: "handed-off-",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := newStoppedAgentListener(t)
			listener.notAnswering.Store(tc.notAnswering)
			listener.publishAnswer.Store(tc.publishAnswer)
			handler, database := newTargetedMessageHandler(t, listener.URL)
			issue := createInteractionIssue(t, handler, "TEST", "Unheld role", "before")
			roleTarget := "role:" + tc.role
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
				"body": "Can the role explain this?", "delivery": "btw",
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
				for i, want := range []struct{ key, expectsReply, mode, postKind, postID string }{
					{key: message.ID + ":aside", expectsReply: "optional", mode: "aside", postKind: "message", postID: message.ID},
					{key: model.CommentMentionIdempotencyKey(comment.ID, roleTarget, "btw"), expectsReply: "required", mode: "btw", postKind: "comment", postID: comment.ID},
				} {
					got := published[i]
					if got["topic"] != "notifications.role."+tc.role || got["keep"] != true || got["source"] != "dispatch" ||
						got["idempotency_key"] != want.key || got["expects_reply"] != want.expectsReply {
						t.Fatalf("publish %d = %#v, want it on the role's topic, kept, from dispatch, under %q expecting %q",
							i, got, want.key, want.expectsReply)
					}
					if _, named := got["target_session"]; named {
						t.Fatalf("publish %d = %#v, want it to name no session", i, got)
					}
					// The post the publish delivers, which the listener keeps the copy for and names
					// back to Dispatch when the copy's hour ends.
					if post, _ := got["dispatch_post"].(map[string]any); post["kind"] != want.postKind || post["id"] != want.postID {
						t.Fatalf("publish %d names the post %#v, want the %s %s it delivers", i, got["dispatch_post"], want.postKind, want.postID)
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

// A message and a mention Dispatch kept for a role with no entry name no session, and the listener
// hands them to whichever session holds the role first. That session's reply is taken: the
// session the listener names as the role's holder when the reply comes may answer, and its reply
// pins the attempt to it, so the attempt reads answered, the message's thread carries the reply
// the card shows, and every later reply is judged by the check every reply passes. A session that
// does not hold the role is refused, and a listener that does not answer is a 503 the holder
// retries. Once the holder has answered, a later live holder of the role may still answer the
// attempt, as main's #4387 lets the live holder answer any attempt sent for its role
// (roleHolderReply): it is answered with the stored answer, and nothing it sends is stored or
// moves the attempt off the holder.
func TestTheFirstHolderOfARoleWithNoEntryMayReplyToWhatWasKeptForIt(t *testing.T) {
	t.Parallel()
	const holder, intruder, successor = "first-reviewer", "intruder", "successor-reviewer"
	listener := newStoppedAgentListener(t)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Unheld role reply", "before")
	roleTarget := "role:" + unregisteredRole

	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Who holds this?", "target": roleTarget, "delivery": "aside",
	}, "alice")
	if got := message.Deliveries; len(got) != 1 || got[0].State != model.DeliveryStateQueued || got[0].SessionID != "" {
		t.Fatalf("the role message's deliveries = %#v, want one queued attempt naming no session", got)
	}
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can the reviewer explain this?", "delivery": "btw",
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

	// Pinned and answered, the attempt keeps its holder's answer. A later live holder of the role
	// passes the reply check as #4387's role-holder rule lets it, and is answered with that stored
	// answer, 200: its own reply is stored nowhere, and the attempt still names the holder.
	listener.roleHolder.Store(new(successor))
	for name, response := range map[string]*httptest.ResponseRecorder{
		"the message": replyAs(t, handler, message.ID, successor, "Me too."),
		"the mention": replyToMention(successor),
	} {
		body := response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(body, `"id":"`+holder+`"`) ||
			strings.Contains(body, "Me too.") || strings.Contains(body, "Answered by "+successor) {
			t.Fatalf("the successor's reply to %s: status=%d body=%s, want 200 with the holder's stored answer", name, response.Code, body)
		}
	}
	if session := messageSession(); session != holder {
		t.Fatalf("the message's attempt names %q after the successor's reply, want %s", session, holder)
	}
	if row := commentAttemptRow(t, database, comment.ID, roleTarget, 1); !strings.HasSuffix(row, "/"+holder) {
		t.Fatalf("the mention's attempt row = %q after the successor's reply, want it still pinned to %s", row, holder)
	}
	for _, row := range issueMessageRows(t, handler, issue.Key) {
		for i := range row.Replies {
			if row.Replies[i].Author.ID == successor {
				t.Fatalf("the issue's messages carry a reply by %s: %#v, want none stored", successor, row.Replies[i])
			}
		}
	}
}
