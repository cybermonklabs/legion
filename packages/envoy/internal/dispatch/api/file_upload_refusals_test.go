package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// forEachFileStore runs check on a server of its own with the project CORE and an issue holding a
// spec, once with every upload's bytes in its row and once beside a file store, which is how
// production serves uploads. putUploadedFile refuses a file upload before its object is written
// (refuseUploadBeforeItsObject) only where a store stands, and its comment says that check makes
// the refusals storeArtifact's transaction makes, since nothing deletes an object and one a
// refused upload left would be named by no row.
func forEachFileStore(t *testing.T, check func(t *testing.T, handler http.Handler, database *store.Store, issueKey string, memory *filestest.Memory)) {
	t.Helper()
	for _, withStore := range []bool{false, true} {
		name := "in its row"
		if withStore {
			name = "in a file store"
		}
		t.Run(name, func(t *testing.T) {
			options := testServerOptions{settle: time.Hour}
			var memory *filestest.Memory
			if withStore {
				memory = filestest.NewMemory()
				options.files = memory
			}
			handler, database, _ := newTestServer(t, options)
			createReferenceAPIProject(t, handler, "CORE")
			issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
			check(t, handler, database, issue.Key, memory)
		})
	}
}

// A file upload under the name of a retired document is refused as the upload route's words say,
// 409 DOCUMENT_RETIRED ("An upload under the name or slug of a retired document, which takes no
// new text, is 409 DOCUMENT_RETIRED and writes nothing", POST /api/v1/issues/{key}/artifacts in
// api/routes_table.go), whether its bytes go to a row or to a file store, and the refused upload
// leaves no object in the store. storeArtifact's transaction reads the retirement before the
// kind, so with no store the upload answers DOCUMENT_RETIRED; the check before the object must too.
func TestAFileUploadUnderARetiredDocumentsNameIsRefusedAsRetiredWithOrWithoutAFileStore(t *testing.T) {
	t.Parallel()
	forEachFileStore(t, func(t *testing.T, handler http.Handler, database *store.Store, issueKey string, memory *filestest.Memory) {
		notes := uploadIssueDocument(t, handler, issueKey, "notes.md", "Notes on the census.\n")
		retireTestDocument(t, database, notes.ID)

		refused := multipartRequest(t, handler, "/api/v1/issues/"+issueKey+"/artifacts",
			map[string]string{"name": "notes.md"}, "notes.md", "text/plain", []byte("plain text\n"), "alice")
		if refused.Code != http.StatusConflict || responseCode(t, refused) != "DOCUMENT_RETIRED" {
			t.Errorf("a file upload under the name of the retired notes.md: status=%d body=%s, want 409 DOCUMENT_RETIRED, as the upload route's words say",
				refused.Code, refused.Body.String())
		}
		if memory != nil && memory.Len() != 0 {
			t.Errorf("the file store holds %d objects after the refused upload, want none", memory.Len())
		}
	})
}

// A file named decisions.md on an issue is refused 409 DECISIONS_ARE_NODES, as a document of that
// name is (decisionsAreNodes in storeArtifact's transaction), and beside a file store it is refused
// before its object is written (refuseUploadBeforeItsObject), so no object is left that no row
// names.
func TestAFileNamedDecisionsMdOnAnIssueIsRefusedBeforeItsObjectIsWritten(t *testing.T) {
	t.Parallel()
	forEachFileStore(t, func(t *testing.T, handler http.Handler, _ *store.Store, issueKey string, memory *filestest.Memory) {
		refused := multipartRequest(t, handler, "/api/v1/issues/"+issueKey+"/artifacts",
			map[string]string{"name": "decisions.md"}, "decisions.md", "text/plain", []byte("We chose Postgres.\n"), "alice")
		if refused.Code != http.StatusConflict || responseCode(t, refused) != codeDecisionsAreNodes {
			t.Errorf("a file upload named decisions.md on %s: status=%d body=%s, want 409 %s",
				issueKey, refused.Code, refused.Body.String(), codeDecisionsAreNodes)
		}
		if memory != nil && memory.Len() != 0 {
			t.Errorf("the file store holds %d objects after the refused upload of decisions.md, want none: no row names them", memory.Len())
		}
	})
}
