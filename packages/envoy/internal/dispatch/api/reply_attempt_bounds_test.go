package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The attempt a reply names in its body is the caller's input, and comment_deliveries and
// message_deliveries keep attempts in integer columns, so an attempt above their range names no
// delivery any comment or message has. Each reply route answers it as it answers any attempt the
// item does not have, 404 COMMENT_NOT_FOUND or 404 MESSAGE_NOT_FOUND. This is no regression test
// of a 500: before the bound, each route already gave this 404, through the error branch of the
// read of its delivery, which pgx failed to encode the attempt for. It pins that answer, which the
// bound (fitsIntegerColumn) now gives before that read, as every handler of this package bounds
// such a number before the query it reaches. The largest number the column holds is no delivery
// either and is answered the same, and the control is the attempt that was delivered, which each
// route takes.
func TestAnAttemptNumberAReplyNamesAboveTheColumnsRangeIsNotFoundNotAFault(t *testing.T) {
	listener, _ := closedIssueListener(t)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Reply attempts", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Look at this.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	// The mention sender delivers attempt 1 after the comment's response (INFRA-606).
	if settled := settledCommentDeliveries(t, database, comment.ID); len(settled) != 1 || settled[0].State != "sent" {
		t.Fatalf("the mention's attempts = %#v, want attempt 1 sent", settled)
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Did this ship?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("a message to s1: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)

	routes := []struct {
		path, code string
		body       func(attempt int) map[string]any
	}{
		{
			path: "/api/v1/comments/" + comment.ID + "/reply", code: "COMMENT_NOT_FOUND",
			body: func(attempt int) map[string]any {
				return map[string]any{"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": attempt,
					"target": "session:s1", "body": "Looked."}
			},
		},
		{
			path: "/api/v1/messages/" + message.ID + "/reply", code: "MESSAGE_NOT_FOUND",
			body: func(attempt int) map[string]any {
				return map[string]any{"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": attempt, "body": "It shipped."}
			},
		},
	}
	for _, route := range routes {
		for _, attempt := range []int{2147483648, 3000000000, 2147483647} {
			response := bearerRequest(t, handler, http.MethodPost, route.path, route.body(attempt))
			if response.Code != http.StatusNotFound || responseCode(t, response) != route.code {
				t.Errorf("%s with attempt %d: status=%d body=%s, want 404 %s", route.path, attempt, response.Code, response.Body.String(), route.code)
			}
		}
		if response := bearerRequest(t, handler, http.MethodPost, route.path, route.body(1)); response.Code != http.StatusCreated {
			t.Errorf("%s with attempt 1 (the control): status=%d body=%s, want 201", route.path, response.Code, response.Body.String())
		}
	}
}
