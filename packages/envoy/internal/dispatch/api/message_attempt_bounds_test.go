package api

import (
	"fmt"
	"net/http"
	"testing"
)

// An attempt number is the caller's input, and message_deliveries keeps attempts in an integer
// column, so a number above its range names no attempt any message has. The accept route answers
// it as it answers any attempt that does not exist, 404 MESSAGE_NOT_FOUND, and never 500, which
// reads as a fault of the server. It is the class dispatch://INFRA-175 closed for a version number
// on GET …/versions/{number}, GET …/changes and the read mark (artifacts.go fitsIntegerColumn).
// The control is the largest number the column holds, which the route already answers 404.
func TestAnAttemptNumberAboveTheColumnsRangeIsNotFoundNotAFault(t *testing.T) {
	t.Parallel()
	handler, _, root, _, _ := directConversationFrom(t, "alice")

	for _, attempt := range []int{2147483648, 3000000000, 2147483647} {
		response := acceptDelivery(t, handler, root.ID, attempt, "s1")
		if response.Code != http.StatusNotFound || responseCode(t, response) != "MESSAGE_NOT_FOUND" {
			t.Errorf("accept of attempt %d: status=%d body=%s, want 404 MESSAGE_NOT_FOUND", attempt, response.Code, response.Body.String())
		}
	}
}

// The same attempts on the accept's /api/v2 route, PUT /api/v2/messages/{id}/deliveries/{attempt}/state
// {state: accepted}, which answers an attempt the message does not have as the object its path names,
// the code its row declares for one (404 DELIVERY_NOT_FOUND), and never 500. The control is the
// largest number the column holds, which the route answers the same.
func TestAnAttemptNumberAboveTheColumnsRangeIsNotFoundNotAFaultUnderV2(t *testing.T) {
	handler, _, root, _, _ := directConversationFrom(t, "alice")

	for _, attempt := range []int{2147483648, 3000000000, 2147483647} {
		path := fmt.Sprintf("/api/v2/messages/%s/deliveries/%d/state", root.ID, attempt)
		response := bearerRequest(t, handler, http.MethodPut, path, map[string]any{
			"state": "accepted", "actor": map[string]any{"kind": "session", "id": "s1"},
		})
		if response.Code != http.StatusNotFound || responseCode(t, response) != "DELIVERY_NOT_FOUND" {
			t.Errorf("PUT %s: status=%d body=%s, want 404 DELIVERY_NOT_FOUND", path, response.Code, response.Body.String())
		}
	}
}
