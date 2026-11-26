package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// TestProjectScopeIsListedOnTheProjectChecksRowsAndNotOnAProjectInBodyRow: PROJECT_SCOPE is what
// the project check answers a write it mounts, a row of class scopeProject, and every such
// operation lists it. A row of class scopeProjectInBody, which names its project in its body, is
// not mounted behind that check: it answers a project out of reach in a 404 of its own, which it
// declares, so its operation lists no PROJECT_SCOPE, and no refusal it declares names one either.
// The rows of the class are read from the table, and a walk that found none of either class fails.
func TestProjectScopeIsListedOnTheProjectChecksRowsAndNotOnAProjectInBodyRow(t *testing.T) {
	t.Parallel()
	mounted, inBody := 0, 0
	for _, route := range openAPIRoutes(apiV2) {
		_, listed := openAPIOperationCodes(route)[apicode.ProjectScope.Name()]
		switch {
		case route.Scope == scopeProjectInBody:
			inBody++
			if listed {
				t.Errorf("%s %s names its project in its body and lists %s", route.Method, route.Pattern, apicode.ProjectScope.Name())
			}
		case route.Scope == scopeProject && route.Method != http.MethodGet:
			mounted++
			if !listed {
				t.Errorf("%s %s is behind the project check and does not list %s", route.Method, route.Pattern, apicode.ProjectScope.Name())
			}
		}
	}
	if mounted == 0 || inBody == 0 {
		t.Fatalf("the walk found %d rows behind the project check and %d that name their project in a body; want both above zero", mounted, inBody)
	}
}
