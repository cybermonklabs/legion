package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// Test round 6 of this change: the decisions step retires every issue's decisions.md, and a retired
// document takes no live write (docs' joinLiveWrite refuses it under the owner's lock). An issue's
// close and reopen write the state they give its asks into every document of the issue
// (docs.WriteIssueAskStates, over issueDocumentIDs), the retired one included, so once an issue's
// decisions.md is retired a person can no longer close the issue while it holds an open ask: the
// close answered 409 DOCUMENT_RETIRED, with advice to write a document of another name. Driven on
// two builds of the head (SEED-1 of the decisions corpus, after the step), and held here with any
// retired document of the issue in place of the step's: the close and the reopen are taken, and the
// open ask is withdrawn by the close and open again after the reopen.
func TestAnIssueWithARetiredDocumentClosesAndReopensWithItsOpenAsk(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n\n:::ask{#q1 urgency=\"low\" multiple=\"false\"}\nShould the census print the corpus size?\n:::\n")
	settleIssue(t, documents, issue.Key)
	askID := blockAskID(t, handler, issue.Key, "q1")
	retired := uploadIssueDocument(t, handler, issue.Key, "notes.md", "A document the step retired.\n")
	retireTestDocument(t, database, retired.ID)

	askState := func(stage string) string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: GET /api/v1/asks/%s: status=%d body=%s", stage, askID, response.Code, response.Body.String())
		}
		var read struct {
			Ask struct {
				State string `json:"state"`
			} `json:"ask"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
			t.Fatalf("%s: decode the ask: %v: %s", stage, err, response.Body.String())
		}
		return read.Ask.State
	}
	if state := askState("before the close"); state != "open" {
		t.Fatalf("before the close the ask of block q1 reads %q, want open", state)
	}
	for _, step := range []struct{ status, askState string }{{"done", "resolved"}, {"todo", "open"}} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"status": step.status}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("PATCH /api/v1/issues/%s to %s, with an open ask and the retired document %s: status=%d body=%s, want 200: a retired document of the issue must not stop its close or its reopen",
				issue.Key, step.status, retired.ID, response.Code, response.Body.String())
		}
		if state := askState("after the move to " + step.status); state != step.askState {
			t.Errorf("after the move to %s the ask of block q1 reads %q, want %s", step.status, state, step.askState)
		}
	}
}
