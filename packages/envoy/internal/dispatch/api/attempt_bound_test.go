package api

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

// An attempt's number is the int4 its row holds, so an attempt past that range names no delivery
// attempt of a comment or a message. Each JSON reply refuses one as it refuses attempt 0, before
// it reads anything: the same status, the same code and the same words, never a read of the
// attempt under some other number.
func TestAReplyNamingAnAttemptPastInt4IsRefusedAsAttemptZeroIs(t *testing.T) {
	handler, _, root, _, _ := directConversationFrom(t, "alice")
	issue := createTestIssue(t, handler, "BOUND", "Attempt bounds")
	comment := createThreadComment(t, handler, issue, map[string]any{"body": "A comment a session answers."}, "alice")
	for _, reply := range []struct{ name, path string }{
		{"a comment's reply", "/api/v1/comments/" + comment.ID + "/reply"},
		{"a message's reply", "/api/v1/messages/" + root.ID + "/reply"},
	} {
		t.Run(reply.name, func(t *testing.T) {
			answer := func(attempt int) *httptest.ResponseRecorder {
				t.Helper()
				return bearerRequest(t, handler, http.MethodPost, reply.path, map[string]any{
					"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": attempt, "body": "Answered.",
				})
			}
			zero := answer(0)
			zeroStatus, zeroBody := zero.Code, zero.Body.String()
			if code, _ := refusalOf(t, zero); zeroStatus != http.StatusForbidden || code != "REPLY_FORBIDDEN" {
				t.Fatalf("attempt 0 answered %d %s, want 403 REPLY_FORBIDDEN, so this comparison proves nothing", zeroStatus, zeroBody)
			}
			if past := answer(math.MaxInt32 + 1); past.Code != zeroStatus || past.Body.String() != zeroBody {
				status, body := past.Code, past.Body.String()
				t.Errorf("attempt %d answered %d %s, want what attempt 0 answers: %d %s", math.MaxInt32+1, status, body, zeroStatus, zeroBody)
			}
		})
	}
}
