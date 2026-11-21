package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// replyMessageV2 is replyMessage on /api/v2: one reply callback on attempt 1 of message, as
// session's host makes it, at POST /api/v2/messages/{id}/replies.
func (rig *roleRouteReplies) replyMessageV2(t *testing.T, message model.Message, session string, reply map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	reply["actor"] = map[string]any{"kind": "session", "id": session}
	reply["attempt"] = 1
	return bearerRequest(t, rig.handler, http.MethodPost, "/api/v2/messages/"+message.ID+"/replies", reply)
}

// replyCommentV2 is replyComment on /api/v2: one reply callback on attempt 1 of comment's one
// mention, at POST /api/v2/comments/{id}/replies.
func (rig *roleRouteReplies) replyCommentV2(t *testing.T, comment mentionedCommentRead, session string, reply map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	reply["actor"] = map[string]any{"kind": "session", "id": session}
	reply["attempt"] = 1
	return bearerRequest(t, rig.handler, http.MethodPost, "/api/v2/comments/"+comment.ID+"/replies", reply)
}

// The /api/v2 reply routes answer an attempt sent for a route to a role as /api/v1's do
// (TestAReplyToARoleRouteAttemptComesFromTheRolesHolder and the two tests after it): each hands
// its body to the work the /api/v1 route runs (answerMessageDelivery, answerCommentDelivery),
// which asks the listener who holds the role. A different session, B, that took role R from the
// session the attempt names, A, has its reply and its error callback accepted while it is R's live
// holder; a session nothing names, and B once it is not live, is refused 403 REPLY_FORBIDDEN; a
// role lookup the listener does not answer is 503 ENVOY_UNAVAILABLE; and a failed read of the
// attempt's route is 500, never a refusal.
func TestAV2ReplyToARoleRouteAttemptComesFromTheRolesHolder(t *testing.T) {
	const holderA, holderB, other = "ses_role_a", "ses_role_b", "ses_role_other"

	t.Run("a different session takes R: its reply and its error callback are accepted", func(t *testing.T) {
		rig := newRoleRouteReplies(t)
		const role = "v2-reply-takeover"
		rig.listener.claim(role, holderA, true)
		answered := rig.message(t, role, holderA, "Which release carries the fix?")
		failed := rig.message(t, role, holderA, "Can you run the migration?")
		answeredMention := rig.mention(t, role, holderA, "Please review the plan.")
		failedMention := rig.mention(t, role, holderA, "Please rerun the tests.")
		rig.listener.claim(role, holderB, true)

		reply := rig.replyMessageV2(t, answered, holderB, map[string]any{"body": "Release 4 carries it."})
		if reply.Code != http.StatusCreated {
			t.Fatalf("the new holder's reply to the message: status=%d body=%s, want 201", reply.Code, reply.Body.String())
		}
		if stored := decodeBody[roleRouteMessageReply](t, reply); stored.Author.ID != holderB || stored.Body != "Release 4 carries it." {
			t.Fatalf("the new holder's reply = %+v, want B's body authored by %s", stored, holderB)
		}
		if session, state, replyID := rig.messageAttempt(t, answered); session != holderA || state != "sent" || replyID == nil {
			t.Fatalf("the answered attempt = session %s state %s reply %v, want A's attempt, sent, with B's reply", session, state, replyID)
		}
		callback := rig.replyMessageV2(t, failed, holderB, map[string]any{"error": "No migration tool on this host"})
		if callback.Code != http.StatusOK {
			t.Fatalf("the new holder's error callback on the message: status=%d body=%s, want 200", callback.Code, callback.Body.String())
		}
		if attempt := decodeBody[model.MessageDelivery](t, callback); attempt.State != "failed" || attempt.Error == nil || *attempt.Error != "No migration tool on this host" {
			t.Fatalf("the message attempt after the new holder's error callback = %+v, want failed with its error", attempt)
		}

		mentionReply := rig.replyCommentV2(t, answeredMention, holderB, map[string]any{"body": "The plan holds."})
		if mentionReply.Code != http.StatusCreated {
			t.Fatalf("the new holder's reply to the mention: status=%d body=%s, want 201", mentionReply.Code, mentionReply.Body.String())
		}
		if stored := decodeBody[roleRouteCommentReply](t, mentionReply); stored.Author.ID != holderB || stored.ReplyTo == nil || *stored.ReplyTo != answeredMention.ID {
			t.Fatalf("the new holder's reply to the mention = %+v, want B's reply under %s", stored, answeredMention.ID)
		}
		mentionCallback := rig.replyCommentV2(t, failedMention, holderB, map[string]any{"error": "The test runner is down"})
		if mentionCallback.Code != http.StatusOK {
			t.Fatalf("the new holder's error callback on the mention: status=%d body=%s, want 200", mentionCallback.Code, mentionCallback.Body.String())
		}
		if _, state, _ := rig.commentAttempt(t, failedMention); state != "failed" {
			t.Fatalf("the mention attempt after the new holder's error callback is %s, want failed", state)
		}
	})

	t.Run("a session neither the attempt nor R's claim names is refused, and so is R's holder while it is not live", func(t *testing.T) {
		rig := newRoleRouteReplies(t)
		const role = "v2-reply-refused"
		rig.listener.claim(role, holderA, true)
		message := rig.message(t, role, holderA, "Who owns the deploy?")
		comment := rig.mention(t, role, holderA, "Who owns the rollback?")
		rig.listener.claim(role, holderB, true)

		replyRefused(t, "a reply to the message from a session nothing names", rig.replyMessageV2(t, message, other, map[string]any{"body": "I do."}))
		replyRefused(t, "a reply to the mention from a session nothing names", rig.replyCommentV2(t, comment, other, map[string]any{"body": "I do."}))
		rig.listener.claim(role, holderB, false)
		replyRefused(t, "a reply to the message from R's holder while it is not live", rig.replyMessageV2(t, message, holderB, map[string]any{"body": "I do."}))
		replyRefused(t, "a reply to the mention from R's holder while it is not live", rig.replyCommentV2(t, comment, holderB, map[string]any{"body": "I do."}))
		if _, state, replyID := rig.messageAttempt(t, message); state != "sent" || replyID != nil {
			t.Fatalf("the message attempt after the refusals = %s with reply %v, want sent and unanswered", state, replyID)
		}
		if _, state, replyID := rig.commentAttempt(t, comment); state != "sent" || replyID != nil {
			t.Fatalf("the mention attempt after the refusals = %s with reply %v, want sent and unanswered", state, replyID)
		}
	})

	t.Run("a role lookup the listener does not answer is 503 ENVOY_UNAVAILABLE", func(t *testing.T) {
		rig := newRoleRouteReplies(t)
		const role = "v2-reply-unanswered"
		rig.listener.claim(role, holderA, true)
		message := rig.message(t, role, holderA, "Is the cache warm?")
		comment := rig.mention(t, role, holderA, "Is the queue drained?")
		rig.listener.claim(role, holderB, true)
		rig.listener.mu.Lock()
		rig.listener.silent = true
		rig.listener.mu.Unlock()

		for what, response := range map[string]*httptest.ResponseRecorder{
			"the message": rig.replyMessageV2(t, message, holderB, map[string]any{"body": "Warm."}),
			"the mention": rig.replyCommentV2(t, comment, holderB, map[string]any{"body": "Drained."}),
		} {
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"ENVOY_UNAVAILABLE"`) {
				t.Fatalf("the role's holder's reply to %s with the lookup unanswered: status=%d body=%s, want 503 ENVOY_UNAVAILABLE", what, response.Code, response.Body.String())
			}
		}
		if answered := rig.replyMessageV2(t, message, holderA, map[string]any{"body": "Warm, says A."}); answered.Code != http.StatusCreated {
			t.Fatalf("the attempt's own session's reply with the lookup unanswered: status=%d body=%s, want 201", answered.Code, answered.Body.String())
		}
	})

	t.Run("a failed read of the attempt's route is 500, never a refusal", func(t *testing.T) {
		fault := &routeReadFault{}
		rig := newRoleRouteRepliesTraced(t, fault)
		const role = "v2-reply-read-fails"
		rig.listener.claim(role, holderA, true)
		message := rig.message(t, role, holderA, "Is the index rebuilt?")
		comment := rig.mention(t, role, holderA, "Is the backfill done?")
		rig.listener.claim(role, holderB, true)

		fault.armed.Store(true)
		for what, response := range map[string]*httptest.ResponseRecorder{
			"the message": rig.replyMessageV2(t, message, holderB, map[string]any{"body": "Rebuilt."}),
			"the mention": rig.replyCommentV2(t, comment, holderB, map[string]any{"body": "Done."}),
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
		if response := rig.replyMessageV2(t, message, holderB, map[string]any{"body": "Rebuilt."}); response.Code != http.StatusCreated {
			t.Fatalf("control: the same reply to the message once the read answers: status=%d body=%s, want 201", response.Code, response.Body.String())
		}
		if response := rig.replyCommentV2(t, comment, holderB, map[string]any{"body": "Done."}); response.Code != http.StatusCreated {
			t.Fatalf("control: the same reply to the mention once the read answers: status=%d body=%s, want 201", response.Code, response.Body.String())
		}
	})
}
