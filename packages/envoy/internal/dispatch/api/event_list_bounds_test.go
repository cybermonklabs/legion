package api

import (
	"fmt"
	"net/http"
	"testing"
)

// An event list's after and before are positions in its owner's event sequence, and events keeps
// that sequence in an integer column (events.seq), so a position above its range names no event.
// Both lists that page by it, an issue's and a project document's, refuse it as they refuse any
// position that is not one, 400 INVALID_QUERY, and never answer 500, which reads as a fault of the
// server. It is the class dispatch://INFRA-175 closed for a version number and a delivery attempt
// (artifacts.go fitsIntegerColumn). The control is the largest position the column holds, which
// both lists answer 200.
func TestAnEventPositionAboveTheColumnsRangeIsRefusedNotAFault(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Event bounds", "A spec\n")
	createAdviceProject(t, handler, "NOTES")
	document := uploadDocument(t, handler, "/api/v1/projects/NOTES", "notes.md", "# Notes\n\nOne.\n")

	for _, list := range []string{
		"/api/v1/issues/" + issue.Key + "/events",
		"/api/v1/artifacts/" + document.Artifact.ID + "/events",
	} {
		for _, field := range []string{"after", "before"} {
			for _, position := range []int64{2147483648, 3000000000} {
				target := fmt.Sprintf("%s?%s=%d", list, field, position)
				response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
				if response.Code != http.StatusBadRequest || responseCode(t, response) != "INVALID_QUERY" {
					t.Errorf("GET %s: status=%d body=%s, want 400 INVALID_QUERY", target, response.Code, response.Body.String())
				}
			}
			target := fmt.Sprintf("%s?%s=%d", list, field, 2147483647)
			if response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"); response.Code != http.StatusOK {
				t.Errorf("GET %s: status=%d body=%s, want 200", target, response.Code, response.Body.String())
			}
		}
	}
}
