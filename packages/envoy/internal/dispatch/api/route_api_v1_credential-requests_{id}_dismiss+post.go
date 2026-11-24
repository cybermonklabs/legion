package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/credential-requests/{id}/dismiss",
	Auth:        authAny,
	Description: "Dismiss an expired credential request as the caller, a secret request or a machine login whose deadline passed before anybody decided it: it leaves every list for good, the caller's and, for a shared request, everybody's. Only a person whose list holds it may dismiss it (403 NOT_APPROVER); a request of a private project the caller may not see is 404, as an id no request has, and 503 PROJECT_ACCESS_UNAVAILABLE while the broker cannot ask Dispatch; a request that can still be decided is 409 RECORD_PENDING, one that was decided, or cancelled before its deadline, is 409 RECORD_TERMINAL, and a second dismissal answers the first one's values. A caller Dispatch calls a restricted member dismisses only their own machine logins: a secret request or a service's machine login is 403 NOT_APPROVER to them, saying why, and while the broker cannot ask Dispatch what kind of member the caller is, its dismissal is 503 MEMBER_KIND_UNCHECKABLE, dismissing nothing.",
	Handler:     (*server).dismissCredentialRecord,
	WriteClass:  scopeDeployment,
	ID:          &idRoute{Reason: "a credential request id"},
	// Dismissing an expired credential request settles nothing: nobody decided it before its
	// deadline and nobody can now, so taking it off the Inbox needs the person scope alone, as
	// POST /api/v1/me/inbox/dismiss does, and not approve.
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		// POST /v1/credential-requests/{record}/dismiss (cipher.Client.Dismiss). Dispatch
		// reads no body: the one field the broker takes, the approver, is the caller.
		Status: http.StatusOK, Response: wire.DismissResponse{},
	},
})
