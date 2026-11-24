// credential_projects_test.go proves that a credential carries its list of projects: a personal
// token's agent_tokens.projects and a broker token's list reach the one rule of what a caller sees
// (internal/dispatch/access), and a mint or an edit lists only projects its caller sees.
package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// viewerOf is what request's credential sees, resolved as every read resolves its caller: the
// caller optionalActor decides, and access.Resolve over it once.
func viewerOf(t *testing.T, deps Deps, request *http.Request) model.Viewer {
	t.Helper()
	caller, _, err := directServer(deps).optionalActor(request)
	if err != nil {
		t.Fatalf("resolve the caller: %v", err)
	}
	viewer, err := access.Resolve(request.Context(), deps.Store.Pool, caller)
	if err != nil {
		t.Fatalf("resolve what the caller sees: %v", err)
	}
	return viewer
}
