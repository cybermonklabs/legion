package api

import (
	"fmt"
	"net/http"
	"testing"
)

// A version number is a positive integer of the caller's choosing, and the store keeps versions
// in an integer column, so a number above its range names no version a document can have. Both
// routes that read a named version answer such a number as a refusal of the caller's input: 400
// INVALID_VERSION, or the 404 NOT_FOUND of a version the document does not have. Neither answers
// it 500, which reads as a fault of the server and tells the caller nothing of its own input. The
// changes route is dispatch://INFRA-175's, and it reads a version as the version route reads one,
// so the two answer alike. The control is the largest number the column holds, which each route
// already answers 404.
func TestAVersionNumberAboveTheColumnsRangeIsRefusedNotAFault(t *testing.T) {
	handler := newTestHandler(t)
	artifactID := uploadVersions(t, handler, "plan.md", "# Plan\n\nOne.\n", "# Plan\n\nTwo.\n")

	for _, path := range []string{
		fmt.Sprintf("/api/v1/artifacts/%s/changes?from=1&to=2147483648", artifactID),
		fmt.Sprintf("/api/v1/artifacts/%s/changes?from=2147483648&to=2147483649", artifactID),
		fmt.Sprintf("/api/v1/artifacts/%s/versions/2147483648", artifactID),
		fmt.Sprintf("/api/v1/artifacts/%s/changes?from=1&to=2147483647", artifactID),
		fmt.Sprintf("/api/v1/artifacts/%s/versions/2147483647", artifactID),
	} {
		response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		refusal := decodeBody[refusedAnswer](t, response)
		switch {
		case response.Code == http.StatusBadRequest && refusal.Code == "INVALID_VERSION":
		case response.Code == http.StatusNotFound && refusal.Code == "NOT_FOUND":
		default:
			t.Errorf("GET %s: status=%d body=%s, want 400 INVALID_VERSION or 404 NOT_FOUND", path, response.Code, response.Body.String())
		}
	}
}
