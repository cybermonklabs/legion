package api

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestRelatedGraphIncludesReferencesInAskAnswers(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "PARTS")
	foundation := createSearchIssue(t, handler, "PARTS", "Reference foundation", "")
	candidate := createSearchIssue(t, handler, "PARTS", "Earlier implementation discussion", "")
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+candidate+"/asks", map[string]any{"question": "Which approach should we choose?", "options": []map[string]string{{"label": "First"}, {"label": "Second"}}}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("ask: %d %s", response.Code, response.Body.String())
	}
	ask := decodeBody[model.Ask](t, response)
	answerPartAsk(t, handler, ask, "alice", []string{"First"}, "Use dispatch://"+foundation+" as the basis.")
	params := url.Values{"title": {"A newly reported defect"}, "text": {"Relates to dispatch://" + foundation}, "keywords_only": {"true"}}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/PARTS/related?"+params.Encode(), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("related: %d %s", response.Code, response.Body.String())
	}
	result := decodeBody[model.Suggestions](t, response)
	if len(result.GraphRelated) != 1 || result.GraphRelated[0].Key != candidate {
		t.Fatalf("answer mention missing: %s", response.Body.String())
	}
	if len(result.GraphRelated[0].Shared) != 1 {
		t.Fatalf("same target counted repeatedly: %+v", result.GraphRelated)
	}
}
