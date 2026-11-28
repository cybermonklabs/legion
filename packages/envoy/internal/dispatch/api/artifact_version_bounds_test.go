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
	t.Parallel()
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md", "# Plan\n\nOne.\n", "# Plan\n\nTwo.\n")

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

// The same numbers on the two /api/v2 routes that read a named version, each answering with the
// code its row declares for that value: GET …/versions/{number} answers a number the document has
// no version of as the object its path names, 404 VERSION_NOT_FOUND, and GET …/changes holds
// version_from and version_to to the range in its decoder, 400 INVALID_QUERY. Neither answers
// 500. The control is the largest number the column holds, which the version read answers 404
// VERSION_NOT_FOUND and the change set the 404 NOT_FOUND of a version the document does not have.
func TestAVersionNumberAboveTheColumnsRangeIsRefusedNotAFaultUnderV2(t *testing.T) {
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md", "# Plan\n\nOne.\n", "# Plan\n\nTwo.\n")

	for _, read := range []struct {
		path   string
		status int
		code   string
	}{
		{fmt.Sprintf("/api/v2/artifacts/%s/changes?version_from=1&version_to=2147483648", artifactID), http.StatusBadRequest, "INVALID_QUERY"},
		{fmt.Sprintf("/api/v2/artifacts/%s/changes?version_from=2147483648&version_to=2147483649", artifactID), http.StatusBadRequest, "INVALID_QUERY"},
		{fmt.Sprintf("/api/v2/artifacts/%s/versions/2147483648", artifactID), http.StatusNotFound, "VERSION_NOT_FOUND"},
		{fmt.Sprintf("/api/v2/artifacts/%s/changes?version_from=1&version_to=2147483647", artifactID), http.StatusNotFound, "NOT_FOUND"},
		{fmt.Sprintf("/api/v2/artifacts/%s/versions/2147483647", artifactID), http.StatusNotFound, "VERSION_NOT_FOUND"},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, read.path, nil, "alice")
		if refusal := decodeBody[refusedAnswer](t, response); response.Code != read.status || refusal.Code != read.code {
			t.Errorf("GET %s: status=%d body=%s, want %d %s", read.path, response.Code, response.Body.String(), read.status, read.code)
		}
	}
}
