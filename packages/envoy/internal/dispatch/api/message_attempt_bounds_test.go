package api

import (
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
