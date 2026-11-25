package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A plan is read at the version its owner gave: GET /api/v1/artifacts/{id}/versions/{n}?blocks=true,
// whose blocks are that version's own. Each block keeps its id, its type and its attributes by
// name, and its text is the body its byte range holds in the markdown of the same answer, without
// the block's opening line or its closing fence.
func TestPlanDocumentReadsTheBlocksOfThatVersion(t *testing.T) {
	const markdown = "Intro.\n\n:::plan_task{#a steps=\"implementer reviewer\"}\nDo the change.\n\nThen check it.\n:::\n\n:::plan_task{#b}\nAnother task.\n:::\n"
	first := strings.Index(markdown, ":::plan_task{#a")
	firstEnd := strings.Index(markdown, ":::\n\n:::plan_task{#b") + len(":::")
	second := strings.Index(markdown, ":::plan_task{#b")
	secondEnd := len(markdown) - 1
	blocks := []map[string]any{
		{"id": "a", "type": "plan_task", "from": first, "to": firstEnd, "token": "sha256:a", "references": map[string]int{"comments": 0, "asks": 0},
			"attributes": []map[string]string{{"name": "steps", "value": "implementer reviewer", "kind": "string"}}},
		{"id": "b", "type": "plan_task", "from": second, "to": secondEnd, "token": "sha256:b", "references": map[string]int{"comments": 0, "asks": 0}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		if r.Method != http.MethodGet {
			t.Fatalf("request = %s %s, want GET", r.Method, r.URL.Path)
		}
		switch r.URL.Path + "?" + r.URL.RawQuery {
		case "/api/v1/artifacts/plan-1/versions/3?blocks=true":
			writeJSON(t, w, map[string]any{"markdown": markdown, "number": 3, "named": false, "blocks": blocks})
		case "/api/v1/artifacts/plan-1/versions/4?blocks=true":
			// A Dispatch of the release before ignores the parameter and answers the version alone.
			writeJSON(t, w, map[string]any{"markdown": markdown, "number": 4, "named": false})
		case "/api/v1/artifacts/plan-1/versions/5?blocks=true":
			writeJSON(t, w, map[string]any{"markdown": markdown, "number": 5, "blocks": []map[string]any{
				{"id": "a", "type": "plan_task", "from": first, "to": len(markdown) + 1, "token": "sha256:a"},
			}})
		case "/api/v1/artifacts/plan-1/versions/6?blocks=true":
			writeJSON(t, w, map[string]any{"markdown": markdown, "number": 7, "blocks": blocks})
		case "/api/v1/artifacts/plan-1?":
			writeJSON(t, w, map[string]any{"id": "plan-1", "issue_key": "LEGION-12", "slug": "plan-md"})
		case "/api/v1/artifacts/plan-1/versions/9?blocks=true":
			writeJSONStatus(t, w, http.StatusNotFound, map[string]any{"code": "VERSION_NOT_FOUND", "error": "no version 9"})
		default:
			t.Fatalf("request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer server.Close()
	client := New(server.URL, BearerTransport(dispatchToken, nil), "")

	document, err := client.PlanDocument(context.Background(), "plan-1", 3)
	if err != nil {
		t.Fatalf("read the plan at version 3: %v", err)
	}
	if document.ArtifactID != "plan-1" || document.IssueKey != "LEGION-12" || document.Version != 3 || len(document.Blocks) != 2 {
		t.Fatalf("plan document = %+v, want plan-1 of LEGION-12 at version 3 with two blocks", document)
	}
	task := document.Blocks[0]
	if task.ID != "a" || task.Type != "plan_task" || task.Attributes["steps"] != "implementer reviewer" || len(task.Attributes) != 1 {
		t.Fatalf("block 0 = %+v, want plan_task a with steps implementer reviewer", task)
	}
	if task.Text != "Do the change.\n\nThen check it." {
		t.Fatalf("block 0's text = %q, want its body", task.Text)
	}
	if other := document.Blocks[1]; other.ID != "b" || other.Text != "Another task." || len(other.Attributes) != 0 {
		t.Fatalf("block 1 = %+v, want plan_task b with no attributes and its body", other)
	}

	// Control: a Dispatch that answers no blocks key gives ErrNoBlocks, never an empty plan.
	if _, err := client.PlanDocument(context.Background(), "plan-1", 4); !errors.Is(err, ErrNoBlocks) {
		t.Fatalf("a version answered with no blocks key = %v, want ErrNoBlocks", err)
	}
	// A block whose range does not index the markdown it came with is refused, not sliced.
	if _, err := client.PlanDocument(context.Background(), "plan-1", 5); err == nil || errors.Is(err, ErrNoBlocks) || !strings.Contains(err.Error(), "block a") {
		t.Fatalf("a block outside its markdown = %v, want an error naming block a", err)
	}
	// An answer for another version than the one asked for is refused.
	if _, err := client.PlanDocument(context.Background(), "plan-1", 6); err == nil || !strings.Contains(err.Error(), "version 7") {
		t.Fatalf("an answer of version 7 for version 6 = %v, want an error naming both", err)
	}
	// Dispatch's own refusal passes through, so a caller can tell a missing version.
	if _, err := client.PlanDocument(context.Background(), "plan-1", 9); !Missing(err) {
		t.Fatalf("a version Dispatch does not have = %v, want its 404", err)
	}
	// No version below 1 exists, so none is asked for.
	if _, err := client.PlanDocument(context.Background(), "plan-1", 0); err == nil {
		t.Fatal("version 0 was read, want a refusal before any request")
	}
}
