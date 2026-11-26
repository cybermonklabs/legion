// hazards_test.go holds the red team's hazards of cybermonklabs/legion#4156 closed at their source
// (plan task t-a-34): a token is never read as a browser, every access.Resolve takes the request's
// own credential, the owner a create writes is a full member, groups are bounded and paged, and
// project.access_changed carries no access list.
package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
)

// TestATokenIsNeverReadAsABrowser: carol's dsp_ token holds the person scope and lists nothing,
// so it sees the public projects carol sees and none of her private ones. Acting for carol on a
// person-scope route, actingPerson answers carol as a user carrying the token's act; that actor,
// with the token's credential, still resolves as the token, never as carol's browser. The mint
// resolves optionalActor's actor, a session, so the token cannot give another token PRIV either,
// while carol's own browser can.
func TestATokenIsNeverReadAsABrowser(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	seedCredentialProjects(t, handler, database)
	makePrivate(t, database, privateProject)
	token := mintListed(t, handler, accessCarol, "carol's agent", nil)
	if len(token.Projects) != 0 {
		t.Fatalf("carol's token lists %v, want none", token.Projects)
	}

	srv := directServer(deps)
	request := bearerOf(token.Token)
	caller, _, err := srv.optionalActor(request)
	if err != nil {
		t.Fatalf("resolve carol's token: %v", err)
	}
	acting, ok := srv.actingPerson(httptest.NewRecorder(), request)
	if !ok || acting.Kind != "user" || acting.Act == nil || acting.Act.TokenID != caller.Credential.TokenID {
		t.Fatalf("actingPerson = %+v, %t, want carol as a user carrying the token's act", acting, ok)
	}
	acting.Credential = caller.Credential
	viewer, err := access.Resolve(request.Context(), deps.Store.Pool, acting)
	if err != nil {
		t.Fatalf("resolve carol's token acting as carol: %v", err)
	}
	if want := []string{publicProject}; !slices.Equal(viewer.Sees, want) {
		t.Fatalf("carol's token acting as carol sees %v, want %v: a token that lists nothing sees its person's public projects, and %s is private",
			viewer.Sees, want, privateProject)
	}

	wantRefusal(t, agentRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", mintBody("given by the token", []string{privateProject}), token.Token),
		http.StatusBadRequest, codeTokenProjectsInput, "carol's token", "mint a token listing "+privateProject)
	mintListed(t, handler, accessCarol, "given at the browser", []string{privateProject})
}
