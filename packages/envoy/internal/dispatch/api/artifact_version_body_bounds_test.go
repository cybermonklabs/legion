package api

import (
	"math"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A document's version number reaches a write in a request body as well as in a path or a query:
// the read mark a person records (PUT /api/v1/me/documents/{id}/read) and the plan review an agent
// records (POST /api/v1/artifacts/{id}/plan-reviews). Each stores the number in an integer column,
// so a number above that column's range names no version a document can have. Each route refuses
// such a number as the caller's input, with the refusal it already gives a version it does not
// take, and never answers 500, which reads as a fault of the server. The control is the largest
// number the column holds: the read mark stores it, since a mark is the reader's own state and
// names no version the document must hold, and the plan review refuses it as a version the plan
// does not have.
func TestAVersionNumberInABodyAboveTheColumnsRangeIsRefusedNotAFault(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "PLAN", "Version bounds", "A spec\n")
	plan := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", "# Plan\n\nOne.\n")
	const pastTheColumn = int64(math.MaxInt32) + 1

	t.Run("the read mark", func(t *testing.T) {
		path := "/api/v1/me/documents/" + plan.Artifact.ID + "/read"
		over := dispatchRequest(t, handler, http.MethodPut, path, map[string]any{"version": pastTheColumn}, "alice")
		if code, _ := refusalOf(t, over); over.Code != http.StatusBadRequest || code != "INVALID_STATE" {
			t.Errorf("PUT %s version %d: status=%d body=%s, want 400 INVALID_STATE", path, pastTheColumn, over.Code, over.Body.String())
		}
		largest := dispatchRequest(t, handler, http.MethodPut, path, map[string]any{"version": math.MaxInt32}, "alice")
		if largest.Code != http.StatusOK {
			t.Fatalf("PUT %s version %d: status=%d body=%s, want 200", path, math.MaxInt32, largest.Code, largest.Body.String())
		}
		if read := decodeBody[model.DocumentRead](t, largest); read.Version == nil || *read.Version != math.MaxInt32 {
			t.Errorf("PUT %s version %d stored %v, want %d", path, math.MaxInt32, read.Version, math.MaxInt32)
		}
	})

	t.Run("the plan review", func(t *testing.T) {
		path := "/api/v1/artifacts/" + plan.Artifact.ID + "/plan-reviews"
		for _, version := range []int64{pastTheColumn, math.MaxInt32} {
			refused := sessionRequest(t, handler, http.MethodPost, path, map[string]any{"actor": sessionActor(), "version": version})
			if code, _ := refusalOf(t, refused); refused.Code != http.StatusBadRequest || code != "INVALID_REVIEW" {
				t.Errorf("POST %s version %d: status=%d body=%s, want 400 INVALID_REVIEW", path, version, refused.Code, refused.Body.String())
			}
		}
	})
}
