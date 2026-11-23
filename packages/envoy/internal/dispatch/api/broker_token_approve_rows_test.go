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
// token's mint, change and revoke.
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
}
