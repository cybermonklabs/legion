package api

import (
	"net/http"
	"regexp"
	"testing"
	"time"
)

// versionsAddress is the address a DOCUMENT_RETIRED refusal names for the retired document's
// versions (retiredDocumentRefusal's "its versions stay readable at ...").
var versionsAddress = regexp.MustCompile(`/api/v1/artifacts/[0-9a-f-]+/versions(?:/[0-9]+)?`)

// A write into a retired document is refused 409 DOCUMENT_RETIRED, and the refusal tells the
// caller where the document's versions stay readable. An agent that follows that advice reads the
// address it names, so the address must answer a read, not a 404 for a route that serves only a
// write.
func TestTheRetiredDocumentRefusalNamesAReadableAddressOfItsVersions(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	notes := uploadIssueDocument(t, handler, issue.Key, "notes.md", "Notes on the census.\n")
	retireTestDocument(t, database, notes.ID)

	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+notes.ID+"/versions",
		map[string]any{"summary": "Named once the document was retired."}, "alice")
	if refused.Code != http.StatusConflict || responseCode(t, refused) != "DOCUMENT_RETIRED" {
		t.Fatalf("a named version of the retired notes.md: status=%d body=%s, want 409 DOCUMENT_RETIRED", refused.Code, refused.Body.String())
	}
	named := versionsAddress.FindString(refused.Body.String())
	if named == "" {
		t.Fatalf("the DOCUMENT_RETIRED refusal names no address of the document's versions: %s", refused.Body.String())
	}
	read := dispatchRequest(t, handler, http.MethodGet, named, nil, "alice")
	if read.Code != http.StatusOK {
		t.Errorf("GET %s, the address the DOCUMENT_RETIRED refusal names for the retired document's versions: status=%d body=%s, want 200",
			named, read.Code, read.Body.String())
	}
}
