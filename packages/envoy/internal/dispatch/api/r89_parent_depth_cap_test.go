package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestAParentPastTheDepthCapIsRefusedAndWritesNothing (plan v89): a create or a reparent that
// would put an issue more than refs.IssueDepthCap links below its root is refused, 409
// PARENT_INPUT, and writes nothing: no issue row, no parent change, no issue event. At the cap
// itself both are written. Under DEEP's Root, a chain puts Tip refs.IssueDepthCap links down,
// and Loose stands alone:
//
//   - a create under Tip (cap+1 links) is refused and DEEP holds the same issues after it;
//   - a reparent of Loose under Tip (cap+1 links) is refused, and Loose keeps no parent and gains
//     no event;
//   - a create under Tip's parent (the cap) and a reparent of Loose under it are written.
//
// Control: skip refuseParentTooDeep in validateIssueParent and issue_create.go, and the create
// under Tip answers 500 INTERNAL and the reparent 409 DEPENDENCY_CYCLE ("the dependency graph is
// deeper than 32 links"), not PARENT_INPUT; the test fails naming each.
func TestAParentPastTheDepthCapIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "DEEP", "name": "Deep"}, "alice")
	root := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "DEEP", "title": "Root"}, "alice"))
	chain := createChain(t, handler, "DEEP", root.Key, refs.IssueDepthCap) // chain[i] is i+1 links below Root
	tip, belowCap := chain[len(chain)-1], chain[len(chain)-2]
	loose := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "DEEP", "title": "Loose", "force": true}, "alice"))
	written := func() (issues int, looseParent *string, looseEvents int) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `
			select (select count(*) from issues where project_key = 'DEEP'),
			       (select parent_key from issues where key = $1),
			       (select count(*) from events where issue_key = $1)
		`, loose.Key).Scan(&issues, &looseParent, &looseEvents); err != nil {
			t.Fatalf("read what DEEP holds: %v", err)
		}
		return issues, looseParent, looseEvents
	}
	refusedPastTheCap := func(name string, response int, body string) {
		t.Helper()
		if response != http.StatusConflict || !strings.Contains(body, "PARENT_INPUT") || !strings.Contains(body, "at most 32 links") {
			t.Errorf("%s, %d links below Root: status=%d body=%s, want 409 PARENT_INPUT naming the 32-link limit", name, refs.IssueDepthCap+1, response, body)
		}
	}

	issues, parent, events := written()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "DEEP", "title": "Too deep", "parent": tip, "force": true}, "alice")
	refusedPastTheCap("a create under "+tip, created.Code, created.Body.String())
	moved := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+loose.Key, map[string]any{"parent": tip}, "alice")
	refusedPastTheCap("a reparent of "+loose.Key+" under "+tip, moved.Code, moved.Body.String())
	if afterIssues, afterParent, afterEvents := written(); afterIssues != issues || afterParent != nil || afterEvents != events {
		t.Errorf("after the two refused writes DEEP holds %d issues (was %d), %s's parent is %v and it has %d events (was %v, %d): a refused write wrote something",
			afterIssues, issues, loose.Key, afterParent, afterEvents, parent, events)
	}

	if atCap := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "DEEP", "title": "At the cap", "parent": belowCap, "force": true}, "alice"); atCap.Code != http.StatusCreated {
		t.Errorf("a create under %s, %d links below Root: status=%d body=%s, want 201", belowCap, refs.IssueDepthCap, atCap.Code, atCap.Body.String())
	}
	if atCap := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+loose.Key, map[string]any{"parent": belowCap}, "alice"); atCap.Code != http.StatusOK {
		t.Errorf("a reparent of %s under %s, %d links below Root: status=%d body=%s, want 200", loose.Key, belowCap, refs.IssueDepthCap, atCap.Code, atCap.Body.String())
	}
	if afterIssues, afterParent, _ := written(); afterIssues != issues+1 || afterParent == nil || *afterParent != belowCap {
		t.Errorf("after the two writes at the cap DEEP holds %d issues (want %d) and %s's parent is %v (want %s)", afterIssues, issues+1, loose.Key, afterParent, belowCap)
	}
}
