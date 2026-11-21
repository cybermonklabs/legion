package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A live tree as deep as the schema allows is a document every read serves, and one level deeper
// is one a browser cannot write. A crafted browser client writes either through the room's CRDT,
// below pmdoc.Update's own validation. At the bound, the last peer's settlement stamps the chain's
// blocks and versions it, GET /text answers 200 with its markdown and token, and GET /blocks 200
// with a token for every block, each of which hashes the subtree below it. One level deeper, the
// room's apply check refuses the update, since the document it leaves does not render where it
// rendered before (docs' renderCheck): the browser is closed 4413 DOC_UPDATE_REFUSED, the update
// is rolled back, settlement writes no version, and both reads serve the document as it stood. A
// tree stored past the bound - before the check, or by a write the check does not judge - is
// outside the schema for every route alike (TestEveryRouteAnswersAStoredTreeOutsideTheSchemaAlike).
func TestEveryReadServesATreeAtTheDepthBound(t *testing.T) {
	for _, test := range []struct {
		name     string
		depth    int
		refused  bool
		versions int
	}{
		{name: "at the bound", depth: pmdoc.MaxTreeDepth, versions: 2},
		{name: "past the bound", depth: pmdoc.MaxTreeDepth + 1, refused: true, versions: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			documentService, handler, database := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "Deep document", "before\n")
			artifactID := issue.PrimaryArtifactID
			peer := connectBrowserPeer(t, documentService, artifactID)
			peer.transact(t, func(txn *crdt.Transaction, fragment *crdt.YXmlFragment) error {
				fragment.Delete(txn, 0, len(fragment.Children()))
				docstest.WriteDeepChain(txn, fragment, test.depth, "deepest")
				return nil
			})
			if test.refused {
				select {
				case <-peer.connection().Ended:
				case <-time.After(30 * time.Second):
					t.Fatalf("the room kept the browser whose update writes a tree %d levels deep, want it closed %q", test.depth, contracts.DocumentUpdateRefusedCloseReason)
				}
				if closed := peer.connection().Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentUpdateRefusedCloseReason {
					t.Fatalf("the update writing a tree %d levels deep closed the browser with %v, want %d %q", test.depth, closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentUpdateRefusedCloseReason)
				}
			} else {
				peer.barrier(t)
			}
			peer.closeAndWait(t)

			var versions int
			if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
				t.Fatalf("count document versions: %v", err)
			}
			if versions != test.versions {
				t.Fatalf("settlement left %d versions of a tree %d levels deep, want %d", versions, test.depth, test.versions)
			}

			text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", nil, "alice")
			blocks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks", nil, "alice")
			for _, read := range []struct {
				route    string
				response *httptest.ResponseRecorder
			}{{"GET /text", text}, {"GET /blocks", blocks}} {
				if read.response.Code != http.StatusOK {
					t.Fatalf("%s after a tree %d levels deep: status=%d body=%.300s, want %d", read.route, test.depth, read.response.Code, read.response.Body.String(), http.StatusOK)
				}
			}
			if test.refused {
				document := decodeBody[struct {
					Markdown string `json:"markdown"`
					Version  int    `json:"version"`
				}](t, text)
				if document.Markdown != "before\n" || document.Version != test.versions {
					t.Fatalf("GET /text after the refused tree %d levels deep: version %d, markdown %q, want version %d of %q", test.depth, document.Version, document.Markdown, test.versions, "before\n")
				}
				if stamped := decodeBody[[]model.ArtifactBlock](t, blocks); len(stamped) != 1 || stamped[0].Type != "paragraph" {
					t.Fatalf("GET /blocks after the refused tree %d levels deep lists %+v, want the one paragraph it stood with", test.depth, stamped)
				}
				return
			}
			document := decodeBody[struct {
				Markdown string `json:"markdown"`
				Version  int    `json:"version"`
				Token    string `json:"token"`
			}](t, text)
			if !strings.Contains(document.Markdown, "deepest") || document.Version != test.versions || !strings.HasPrefix(document.Token, "sha256:") {
				t.Fatalf("GET /text of a tree %d levels deep: version %d, token %q, markdown ends %q", test.depth, document.Version, document.Token, document.Markdown[max(0, len(document.Markdown)-80):])
			}
			stamped := decodeBody[[]model.ArtifactBlock](t, blocks)
			if len(stamped) != test.depth-1 {
				t.Fatalf("GET /blocks of a tree %d levels deep lists %d blocks, want %d", test.depth, len(stamped), test.depth-1)
			}
			for _, block := range stamped {
				if !strings.HasPrefix(block.Token, "sha256:") {
					t.Fatalf("block %q (%s) has token %q", block.ID, block.Type, block.Token)
				}
			}
		})
	}
}
