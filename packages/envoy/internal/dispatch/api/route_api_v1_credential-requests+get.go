package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/credential-requests",
	Auth:        authAny,
	Description: "List credential requests the caller may decide (?approver=me only), and the expired ones nobody has dismissed: those naming the caller, a shared secret's and a service's machine login, which anyone signed in decides. Each row's state is pending (a decision may still settle it) or expired (its deadline, expires_at, passed before anybody decided it, and an expired row stays until somebody dismisses it); the pending rows come first and then the expired, each newest first, and a row with no state, from a secrets broker older than states, is pending. Each secret request carries approve_url, the secrets broker's page where it is decided; null without a configured secrets broker. A caller Dispatch calls a restricted member is no approver of a secret or of a service's machine login, so their list holds their own machine logins alone; while the broker cannot ask Dispatch what kind of member the caller is, it holds none of those other rows either.",
	Handler:     (*server).listCredentialPending,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	// A relay route: Dispatch writes the broker's bytes unparsed (relayBrokerResponse), so the
	// Response names the shape the broker's own handler answers with, declared in
	// internal/cipher/wire and matched through the cipher.Client method the handler calls.
	Shape: routeShape{
		// GET /v1/pending (cipher.Client.Pending).
		Status: http.StatusOK, Response: wire.PendingResponse{}, Params: credentialApproverQuery{},
		// A Dispatch with no broker answers 200 and the JSON literal `null` rather than
		// 404 FEATURE_OFF, so every page of such a deployment reads this route cleanly.
		Nullable: true,
	},
})
