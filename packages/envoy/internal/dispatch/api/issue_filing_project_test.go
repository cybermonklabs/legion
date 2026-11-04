package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestIssueFiledFromPublicProjectConversation(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "PUB")
	artifact := createProjectDocument(t, handler, "PUB", "Planning notes", "A public project document")
	for _, kind := range []string{"comment", "ask"} {
		body := map[string]any{"body": "File the reported defect"}
		if kind == "ask" {
			body = map[string]any{"question": "Should the parser defect be filed?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}}}
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/"+kind+"s", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", kind, response.Code, response.Body.String())
		}
		item := decodeBody[struct {
			ID string `json:"id"`
		}](t, response)
		ref := "dispatch://PUB/artifact/" + artifact.Slug + "/" + kind + "/" + item.ID
		key := createSuggestionIssue(t, handler, map[string]any{"project": "PUB", "title": "Public " + kind + " filing", "filed_from": ref})
		response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/links", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("links: %d %s", response.Code, response.Body.String())
		}
		links := decodeBody[model.GraphReferences](t, response)
		if len(links.Edges) != 1 || links.Edges[0].Kind != "filed_from" || links.Edges[0].Node.Ref != ref {
			t.Fatalf("project source: %+v", links)
		}
	}
}
