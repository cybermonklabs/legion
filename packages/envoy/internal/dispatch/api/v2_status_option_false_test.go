package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// PATCH /api/v2/issues/{key} takes its status-change form only for a status option or dry_run set
// to true. One sent as false asks for nothing, as the tool's rule reads the options
// (issueStatusOptionsOf), so the plain PATCH takes the body: a title beside it is written and
// answered 200 with the issue, where the status-change form refuses it 400 STATUS_CHANGE_INPUT,
// and a status beside it answers the issue, not the form's {issue, changed}.
func TestAV2StatusOptionSentFalseAsksForNothing(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	options := []string{"include_children", "unlink_children", "include_parent", "dry_run"}
	keys := createListedIssues(t, handler, "OPTS", len(options))
	for index, option := range options {
		key := keys[index]
		for _, write := range []struct {
			body  map[string]any
			check func(model.Issue) bool
		}{
			{map[string]any{"title": "x", option: false}, func(issue model.Issue) bool { return issue.Title == "x" }},
			{map[string]any{"status": "in_progress", option: false}, func(issue model.Issue) bool { return issue.Status == "in_progress" }},
		} {
			response := dispatchRequest(t, handler, http.MethodPatch, "/api/v2/issues/"+key, write.body, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("PATCH /api/v2/issues/%s with %v answered %d %s, want 200 with the issue", key, write.body, response.Code, response.Body.String())
			}
			answer := decodeBody[map[string]json.RawMessage](t, response)
			_, changed := answer["changed"]
			_, wrapped := answer["issue"]
			issue := decodeBody[model.Issue](t, response)
			if changed || wrapped || issue.Key != key || !write.check(issue) {
				t.Fatalf("PATCH /api/v2/issues/%s with %v answered %s, want the issue as the write left it, not {issue, changed}", key, write.body, response.Body.String())
			}
		}
	}
}
