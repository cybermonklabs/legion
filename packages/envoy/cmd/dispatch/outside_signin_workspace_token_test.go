package main

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A restricted member's token is admitted by exactly the rule that admits a restricted sign-in,
// not by a second copy of it: bob@second.example, an account outside the Workspace that PRIV's
// list names, signs in while DISPATCH_SIGNIN_WORKSPACE_DOMAINS lists nothing, and mints a token,
// which acts. The same deployment with second.example among the Workspace's domains would refuse
// that sign-in (identity.ErrWorkspaceAddress), so it refuses the token too, at once and while the
// list still names bob: 401 TOKEN_OWNER_NOT_ADMITTED saying the address is on a domain the
// Workspace owns, on a read and on a write.
func TestOutsideSignInTokenStopsOnceItsAddressIsOnAWorkspaceDomain(t *testing.T) {
	const bob = "bob@second.example"
	s := newOutsideSignIn(t, nil)
	s.listOn(t, "PRIV", access.PrincipalPerson, bob)
	signIn := s.signInAs(t, s.handler, bob)
	if signIn.Code != http.StatusFound || !sessionCookieSet(signIn) {
		t.Fatalf("the callback for %s with no Workspace domain listed answered %d %s, want a signed-in redirect", bob, signIn.Code, signIn.Body.String())
	}
	if kind := s.memberKind(t, bob); kind != string(model.MemberRestricted) {
		t.Fatalf("people.member_kind of %s = %q, want restricted", bob, kind)
	}
	token := mintTokenOn(t, s.handler, signIn)
	requireTokenActs(t, "GET /api/v1/projects with bob's token, no Workspace domain listed",
		bearerRequest(s.handler, http.MethodGet, "/api/v1/projects", "", token))

	listed := s.router(t, s.outsideBootWith(t, map[string]string{"DISPATCH_SIGNIN_WORKSPACE_DOMAINS": "second.example"}))
	if refused := s.signInAs(t, listed, bob); refused.Code != http.StatusForbidden || sessionCookieSet(refused) {
		t.Fatalf("the callback for %s with second.example listed answered %d %s, want the 403 page and no cookie", bob, refused.Code, refused.Body.String())
	}
	const saying = "on a domain the Google Workspace owns"
	requireTokenRefused(t, "GET /api/v1/projects with bob's token, second.example listed", saying,
		bearerRequest(listed, http.MethodGet, "/api/v1/projects", "", token))
	requireTokenRefused(t, "POST /api/v1/issues with bob's token, second.example listed", saying,
		bearerRequest(listed, http.MethodPost, "/api/v1/issues", `{"project":"PRIV","title":"Written once bob's domain is the Workspace's","actor":{"kind":"session","id":"bob-agent"}}`, token))
}
