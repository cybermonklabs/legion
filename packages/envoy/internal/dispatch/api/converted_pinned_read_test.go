package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAReferencePinnedBeforeTheConversionReadsTheBlockItsVersionHeld is c-redirects' pinned leg
// (R13): a reference pinned to a version of a document written before its issue converts reads,
// after the conversion, the block that version held. PIN-1 is an issue of the release before, whose
// verification.md holds the check c-target in version 1 and in version 2, which changes only the
// paragraph before it, so no text of c-target is recorded at version 2 itself. Before the
// conversion the reference dispatch://PIN-1/artifact/verification-md#b-c-target@v2 answers the
// check's text. The conversion moves the document's blocks into the issue's room and retires the
// document's own block index at version 2 (docs' retireDocuments), which must record no text of a
// block it deletes there: after it the same pinned reference answers 200 with the check's text at
// version 2, not 404 "no block".
func TestAReferencePinnedBeforeTheConversionReadsTheBlockItsVersionHeld(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "PIN")
	issue := createDigestIssue(t, handler, database, "PIN", "A pinned read across the conversion", "The spec of the issue.\n", true)
	const words = "The check holds its words."
	check := ":::check{#c-target}\n" + words + "\n:::\n"
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "verification.md", "The words of version one.\n\n"+check)
	if second := uploadDocument(t, handler, owner, "verification.md", "The words of version two.\n\n"+check); second.Version.Number != 2 {
		t.Fatalf("the second upload of verification.md to %s wrote version %d, want 2", issue.Key, second.Version.Number)
	}
	settleIssue(t, documents, issue.Key)
	settleReindexQueue(t, handler, database, deps, issue.Key)

	pinned := "dispatch://" + issue.Key + "/artifact/verification-md#b-c-target@v2"
	readPinned := func(when string) {
		t.Helper()
		answer := dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?to="+url.QueryEscape(pinned), nil, "alice")
		if answer.Code != http.StatusOK {
			t.Fatalf("%s: GET /api/v1/references?to=%s answered %d, want 200 with the block version 2 held: %s", when, pinned, answer.Code, answer.Body.String())
		}
		node := decodeBody[struct {
			Node model.GraphNode `json:"node"`
		}](t, answer).Node
		if node.Pin == nil || node.Pin.Version != 2 || !strings.Contains(node.Pin.Text, words) {
			t.Fatalf("%s: %s answers the pin %+v, want version 2 holding %q", when, pinned, node.Pin, words)
		}
	}
	readPinned("before the conversion")

	converted, err := documents.ConvertIssue(context.Background(), issue.Key, docs.IssueRoomGate, false)
	if err != nil || converted.Outcome != docs.IssueConverted {
		t.Fatalf("convert %s: %+v, %v; want it converted", issue.Key, converted, err)
	}
	readPinned("after the conversion")
}
