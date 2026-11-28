package dispatch

//ci:reads packages/contracts/fixtures/dispatch-version-blocks/* versionBlocksFixture reads each answer a real Dispatch served, for PlanDocument's tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// versionBlocksFixture is one answer under packages/contracts/fixtures/dispatch-version-blocks,
// which a real Dispatch served and envoy's TestAVersionsBlocksCarryTheParentOfATaskBlock writes:
// version 1 of ACME-1's plan.md, whose task blocks t1, t2 and t3 stand under default, and a build
// of the release before refusing that version.
func versionBlocksFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "dispatch-version-blocks", name))
	if err != nil {
		t.Fatalf("read the fixture %s: %v", name, err)
	}
	return body
}

// servingVersion is a Dispatch that answers version 1 of plan-1 with status and body, as the
// fixture's Dispatch did, and plan-1 itself as plan.md of ACME-1.
func servingVersion(t *testing.T, status int, body []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		switch r.URL.Path + "?" + r.URL.RawQuery {
		case "/api/v1/artifacts/plan-1/versions/1?blocks=true":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(body)
		case "/api/v1/artifacts/plan-1?":
			writeJSON(t, w, map[string]any{"id": "plan-1", "issue_key": "ACME-1", "slug": "plan-md"})
		default:
			t.Fatalf("request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// A plan read from a real Dispatch keeps each task block's `parent`, among the block's attributes
// and whole, as Dispatch serves it with the kind block[]: t1, t2 and t3 name default, and default
// names none. The fault is a client that keeps only the attributes it knows, or cuts a reference.
// The control is a client that leaves `parent` out of a block's attributes: the three blocks then
// name no parent.
func TestPlanDocumentReadsTheParentOfATaskBlock(t *testing.T) {
	body := versionBlocksFixture(t, "plan-with-parent.json")
	const parent = "dispatch://ACME-1/artifact/plan-md#b-default"
	// The fixture is the shape the contract names: `parent` among the attributes, of kind block[].
	var served struct {
		Blocks []struct {
			ID         string `json:"id"`
			Attributes []struct{ Name, Value, Kind string }
		} `json:"blocks"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode the fixture: %v", err)
	}
	kinds := map[string]string{}
	for _, block := range served.Blocks {
		for _, attribute := range block.Attributes {
			if attribute.Name == "parent" {
				kinds[block.ID] = attribute.Kind + " " + attribute.Value
			}
		}
	}
	if want := map[string]string{"t1": "block[] " + parent, "t2": "block[] " + parent, "t3": "block[] " + parent}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("the fixture's parent attributes = %v, want %v", kinds, want)
	}

	document, err := New(servingVersion(t, http.StatusOK, body).URL, BearerTransport(dispatchToken, nil)).PlanDocument(context.Background(), "plan-1", 1)
	if err != nil {
		t.Fatalf("read the plan at version 1: %v", err)
	}
	if document.IssueKey != "ACME-1" || document.Version != 1 {
		t.Fatalf("plan document = %+v, want version 1 of ACME-1's plan", document)
	}
	got := map[string]string{}
	texts := map[string]string{}
	for _, block := range document.Blocks {
		if value, ok := block.Attributes["parent"]; ok {
			got[block.ID] = value
		}
		texts[block.ID] = block.Text
	}
	if want := map[string]string{"t1": parent, "t2": parent, "t3": parent}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the parent of each task block = %v, want %v: default names none", got, want)
	}
	if want := map[string]string{"default": "Ship the parser.", "t1": "Read the grammar.", "t2": "Write the parser.", "t3": "Test the parser."}; !reflect.DeepEqual(texts, want) {
		t.Fatalf("the text of each task block = %v, want %v", texts, want)
	}
}

// A build of the release before, which declares no `parent`, refuses the version read of a plan
// that holds one: 409 DOC_NEWER_CONTENT, the body the fixture holds. PlanDocument answers that
// refusal as an *Error with its status, code and message, never ErrNoBlocks and never Missing, so
// register_plan answers it as a failed read and records nothing. The control is a PlanDocument
// that reads any refusal of the version as ErrNoBlocks: the code is then gone.
func TestPlanDocumentAnswersAnOlderBuildsRefusal(t *testing.T) {
	body := versionBlocksFixture(t, "older-build-newer-content.json")
	var refused struct{ Code, Error string }
	if err := json.Unmarshal(body, &refused); err != nil || refused.Code != "DOC_NEWER_CONTENT" || refused.Error == "" {
		t.Fatalf("the fixture = %s (%v), want a DOC_NEWER_CONTENT refusal with its message", body, err)
	}
	_, err := New(servingVersion(t, http.StatusConflict, body).URL, BearerTransport(dispatchToken, nil)).PlanDocument(context.Background(), "plan-1", 1)
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Status != http.StatusConflict || refusal.Code != "DOC_NEWER_CONTENT" || refusal.Message != refused.Error {
		t.Fatalf("PlanDocument = %v, want Dispatch's 409 DOC_NEWER_CONTENT with its message %q", err, refused.Error)
	}
	if errors.Is(err, ErrNoBlocks) || Missing(err) {
		t.Fatalf("PlanDocument = %v, want neither ErrNoBlocks nor a missing version", err)
	}
}

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
	client := New(server.URL, BearerTransport(dispatchToken, nil))

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
