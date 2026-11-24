package projectaccesstest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// TestNewHandlerServesTheRowsToItsOwnBearer is the handler a process of its own serves, as the
// local stack's Dispatch stand does: the person and project routes answer from the rows it was
// given to the bearer it was given, and any other bearer is refused 403, as NewServer's are.
func TestNewHandlerServesTheRowsToItsOwnBearer(t *testing.T) {
	const token = "the-stack-own-bearer"
	handler := NewHandler(map[string]Row{
		"PRIV": {Visibility: "private", Owner: "owner@airlock.test", Listed: []string{"person@airlock.test"}, SlackChannels: []string{"C0ALLOWED1"}},
	}, map[string]string{"person@airlock.test": "full"}, token)

	ask := func(path, bearer string) (int, map[string]any) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+bearer)
		answer := httptest.NewRecorder()
		handler.ServeHTTP(answer, request)
		var body map[string]any
		if err := json.Unmarshal(answer.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s answered %d %q, no JSON object: %v", path, answer.Code, answer.Body.String(), err)
		}
		return answer.Code, body
	}

	if status, body := ask(personPath+"?person=person@airlock.test", token); status != http.StatusOK || body["member_kind"] != "full" {
		t.Errorf("the person route answered %d %v; want 200 and member_kind full", status, body)
	}
	status, body := ask(projectPath+"PRIV?person=person@airlock.test", token)
	channels, _ := body["slack_channels"].([]any)
	if status != http.StatusOK || body["visibility"] != "private" || body["visible"] != true || body["owner"] != "owner@airlock.test" ||
		!slices.Equal(channels, []any{"C0ALLOWED1"}) {
		t.Errorf("the project route answered %d %v; want PRIV private, visible to its listed person, with the owner's one channel", status, body)
	}
	if status, body := ask(projectPath+"PRIV?person=stranger@airlock.test", token); status != http.StatusOK || body["visible"] != false {
		t.Errorf("the project route answered a stranger %d %v; want 200 and visible false", status, body)
	}
	if status, body := ask(personPath+"?person=person@airlock.test", "another-bearer"); status != http.StatusForbidden || body["code"] != "ACCESS_ACCOUNT_REQUIRED" {
		t.Errorf("another bearer was answered %d %v; want 403 ACCESS_ACCOUNT_REQUIRED", status, body)
	}
}
