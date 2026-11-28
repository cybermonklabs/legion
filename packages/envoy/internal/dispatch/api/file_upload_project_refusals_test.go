package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A file upload to a project under the name of a retired project document is refused as the
// project upload route's words say, 409 DOCUMENT_RETIRED ("An upload under the name or slug of a
// retired document, which takes no new text, is 409 DOCUMENT_RETIRED and writes nothing", POST
// /api/v1/projects/{key}/artifacts in api/routes_table.go), whether its bytes go to a row or to a
// file store, and the refused upload leaves no object in the store. The check before the object
// (refuseUploadBeforeItsObject) reads a project's documents as it reads an issue's, so it reads the
// retirement before the kind on this route too.
func TestAProjectFileUploadUnderARetiredDocumentsNameIsRefusedAsRetiredWithOrWithoutAFileStore(t *testing.T) {
	t.Parallel()
	forEachFileStore(t, func(t *testing.T, handler http.Handler, database *store.Store, _ string, memory *filestest.Memory) {
		design := createProjectDocument(t, handler, "CORE", "design.md", "# Design\n")
		retireTestDocument(t, database, design.ID)
		refused := multipartRequest(t, handler, "/api/v1/projects/CORE/artifacts",
			map[string]string{"name": "design.md"}, "design.md", "text/plain", []byte("plain text\n"), "alice")
		if refused.Code != http.StatusConflict || responseCode(t, refused) != "DOCUMENT_RETIRED" {
			t.Errorf("a file upload to the project under the name of the retired design.md: status=%d body=%s, want 409 DOCUMENT_RETIRED",
				refused.Code, refused.Body.String())
		}
		if memory != nil && memory.Len() != 0 {
			t.Errorf("the file store holds %d objects after the refused upload, want none", memory.Len())
		}
	})
}
