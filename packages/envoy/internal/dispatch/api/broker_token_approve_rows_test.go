package api

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestEveryRowThatSettlesWhatAPersonSettlesNeedsApprove holds each route that settles what only
// the person settles to the approve scope in the route table. TestNoBrokerTokenGetsApprove walks
// whatever rows name approve, so a row that stops naming it leaves that walk and the test still
// passes, while every broker token, which holds no approve, is then answered on it. This pins
// the rows themselves: a review, a decision's Keep, a credential's approve, deny and machine
// lookup, the revoke of a credential grant, a machine login and a sign-in, and a personal
// token's mint, change and revoke. Each is pinned at /api/v1 (routeScopes) and at the /api/v2
// route that answers it, on that row's own scopes or, for the decision a credential request's
// state sets, on the rule of that state (its Authorities): /api/v2 grants nothing /api/v1 refuses.
func TestEveryRowThatSettlesWhatAPersonSettlesNeedsApprove(t *testing.T) {
	for _, name := range []string{
		"POST /api/v1/artifacts/{id}/reviews",
		"POST /api/v1/artifacts/{id}/versions/{number}/reviews",
		"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep",
		"POST /api/v1/credential-requests/{id}/approve",
		"POST /api/v1/credential-requests/{id}/deny",
		"POST /api/v1/credential-requests/machine-lookup",
		"POST /api/v1/credential-grants/{id}/revoke",
		"POST /api/v1/machine-logins/{id}/revoke",
		"POST /api/v1/signins/{id}/revoke",
		"POST /api/v1/me/agent-tokens",
		"PATCH /api/v1/me/agent-tokens/{id}",
		"DELETE /api/v1/me/agent-tokens/{id}",
	} {
		scopes, named := routeScopes[name]
		if !named || !slices.Contains(scopes, model.ScopeApprove) {
			t.Errorf("%s names the scopes %v, want %s among them: without it a broker token, which never holds %s, is answered there",
				name, scopes, model.ScopeApprove, model.ScopeApprove)
		}
	}

	served := map[string]apiRoute{}
	for _, route := range (&server{}).routes() {
		served[route.Method+" "+route.Pattern] = route
	}
	for _, row := range []struct {
		// name is the /api/v2 route, and state the value of its body's state that needs approve,
		// empty for a route whose own scopes need it.
		name, state string
	}{
		{"POST /api/v2/artifacts/{id}/reviews", ""},
		{"PUT /api/v2/issues/{key}/decisions/{block_id}/state", ""},
		{"PUT /api/v2/credential-requests/{id}/state", "approved"},
		{"PUT /api/v2/credential-requests/{id}/state", "denied"},
		{"POST /api/v2/machine-login-lookups", ""},
		{"DELETE /api/v2/credential-grants/{id}", ""},
		{"DELETE /api/v2/machine-logins/{id}", ""},
		{"DELETE /api/v2/me/signins/{id}", ""},
		{"POST /api/v2/me/agent-tokens", ""},
		{"PATCH /api/v2/me/agent-tokens/{id}", ""},
		{"DELETE /api/v2/me/agent-tokens/{id}", ""},
	} {
		route, mounted := served[row.name]
		switch {
		case !mounted:
			t.Errorf("%s is no route the table mounts", row.name)
		case row.state == "" && !slices.Contains(route.OAuthScopes, model.ScopeApprove):
			t.Errorf("%s names the scopes %v, want %s among them: without it a broker token, which never holds %s, is answered there",
				row.name, route.OAuthScopes, model.ScopeApprove, model.ScopeApprove)
		case row.state != "" && !slices.ContainsFunc(route.Authorities, func(rule authorityRule) bool {
			return rule.Where.In == partBody && rule.Where.Name == "state" && rule.Value == row.state &&
				slices.Contains(rule.Scopes, model.ScopeApprove)
		}):
			t.Errorf("%s holds the state %s to the rules %+v, want one naming %s: without it a broker token, which never holds %s, decides there",
				row.name, row.state, route.Authorities, model.ScopeApprove, model.ScopeApprove)
		}
	}
}
