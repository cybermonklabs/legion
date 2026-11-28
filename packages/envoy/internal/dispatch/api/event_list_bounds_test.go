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
	t.Parallel()
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

// The same positions on the two /api/v2 logs that page by seq, an issue's and a project
// document's: each refuses an after or a before above the column's range as it refuses any
// position that is none of its positions, the code its row declares for one (400
// POSITION_INVALID), and never answers 500. The control is the largest position the column holds:
// a before there answers 200, and an after there is above the log's last seq, which the row
// refuses as it always has.
func TestAnEventPositionAboveTheColumnsRangeIsRefusedNotAFaultUnderV2(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Event bounds", "A spec\n")
	createAdviceProject(t, handler, "NOTES")
	document := uploadDocument(t, handler, "/api/v1/projects/NOTES", "notes.md", "# Notes\n\nOne.\n")

	refused := func(target string) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusBadRequest || responseCode(t, response) != "POSITION_INVALID" {
			t.Errorf("GET %s: status=%d body=%s, want 400 POSITION_INVALID", target, response.Code, response.Body.String())
		}
	}
	for _, list := range []string{
		"/api/v2/issues/" + issue.Key + "/events",
		"/api/v2/artifacts/" + document.Artifact.ID + "/events",
	} {
		for _, field := range []string{"after", "before"} {
			for _, position := range []int64{2147483648, 3000000000} {
				refused(fmt.Sprintf("%s?%s=%d", list, field, position))
			}
		}
		refused(fmt.Sprintf("%s?after=%d", list, 2147483647))
		target := fmt.Sprintf("%s?before=%d", list, 2147483647)
		if response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"); response.Code != http.StatusOK {
			t.Errorf("GET %s: status=%d body=%s, want 200", target, response.Code, response.Body.String())
		}
	}
}
