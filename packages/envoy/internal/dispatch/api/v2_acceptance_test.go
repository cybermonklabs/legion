package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// PATCH /api/v2/issues/{key} runs #4884's acceptance check through the work both versions' patch
// makes (patchIssueAs): a status of done that carries refuse_open_foreign_children, as the Legion
// daemon's acceptance writes do, is refused 409 FOREIGN_CHILD_OPEN while a child of another project
// is open, naming that child, and leaves the issue as it stood; the field beside a status other
// than retro or done is 400 INVALID_ISSUE.
func TestAV2AcceptanceWriteWaitsForAnOpenChildOfAnotherProject(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	parent, child := createListedIssues(t, handler, "CORE", 1)[0], createListedIssues(t, handler, "SIDE", 1)[0]
	// The test writes the link itself rather than through the route, as main's own test of the
	// check does, so it reads the same at a build whose routes still refuse a parent of another
	// project.
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, parent, child); err != nil {
		t.Fatalf("put %s under %s: %v", child, parent, err)
	}
	refused := dispatchRequest(t, handler, http.MethodPatch, "/api/v2/issues/"+parent,
		map[string]any{"status": "done", "refuse_open_foreign_children": true}, "alice")
	if refused.Code != http.StatusConflict {
		t.Fatalf("the acceptance write over an open child answered %d %s, want 409 FOREIGN_CHILD_OPEN", refused.Code, refused.Body.String())
	}
	refusal := decodeBody[foreignChildOpenAnswer](t, refused)
	if refusal.Code != "FOREIGN_CHILD_OPEN" || len(refusal.Issues) != 1 || refusal.Issues[0].Key != child || refusal.Issues[0].Project != "SIDE" {
		t.Fatalf("the refusal is %+v, want FOREIGN_CHILD_OPEN naming %s of SIDE", refusal, child)
	}
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v2/issues/"+parent, nil, "alice")
	if status := decodeBody[model.Issue](t, read).Status; status == "done" {
		t.Fatalf("the refused write left %s done", parent)
	}

	beside := dispatchRequest(t, handler, http.MethodPatch, "/api/v2/issues/"+parent,
		map[string]any{"status": "in_progress", "refuse_open_foreign_children": true}, "alice")
	if code, _ := refusalOf(t, beside); beside.Code != http.StatusBadRequest || code != "INVALID_ISSUE" {
		t.Fatalf("the field beside status in_progress answered %d %s, want 400 INVALID_ISSUE", beside.Code, beside.Body.String())
	}
}
