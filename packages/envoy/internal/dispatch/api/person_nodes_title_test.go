package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestAPersonPageRowAnswersItsFirstLineUnderBothNames holds GET /api/v1/people/{login}/nodes to the
// member the release before answers: each row carries its first line as first_line and again as
// title, one value under both names, so a page of the release before, which draws a row from title,
// still draws each line this build answers. It holds for a standing comment, whose line is its first
// line, and for a deleted one, whose line is the empty body the delete left. The rows are read as raw
// JSON, so a row that carries no title at all fails here rather than reading as an empty one.
func TestAPersonPageRowAnswersItsFirstLineUnderBothNames(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TITLE", "A person's lines under both names", "# Spec\n\nA passage.\n")
	const person, reader = "alice", "bob"
	standing := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Ship it on Friday.\nReview is done."}, person)
	deleted := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Hold the deploy until Monday."}, person)
	deleteThreadComment(t, handler, deleted.ID, person)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/"+person+"/nodes", nil, reader)
	if response.Code != http.StatusOK {
		t.Fatalf("%s reads %s's page: status=%d body=%s", reader, person, response.Code, response.Body.String())
	}
	page := decodeBody[struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}](t, response)
	want := map[string]string{standing.ID: "Ship it on Friday.", deleted.ID: ""}
	if len(page.Rows) != len(want) {
		t.Fatalf("%s's page = %s, want her two comments", person, response.Body.String())
	}
	for _, row := range page.Rows {
		var id, firstLine, title string
		for name, into := range map[string]*string{"id": &id, "first_line": &firstLine, "title": &title} {
			raw, carried := row[name]
			if !carried {
				t.Fatalf("a row of %s's page carries no %s: %s", person, name, response.Body.String())
			}
			if err := json.Unmarshal(raw, into); err != nil {
				t.Fatalf("a row's %s is %s, want a string: %v", name, raw, err)
			}
		}
		line, listed := want[id]
		if !listed {
			t.Fatalf("%s's page holds the row %s, which is neither of her comments: %s", person, id, response.Body.String())
		}
		if firstLine != line || title != firstLine {
			t.Errorf("the row of %s answers first_line %q and title %q, want %q under both names", id, firstLine, title, line)
		}
	}
}
