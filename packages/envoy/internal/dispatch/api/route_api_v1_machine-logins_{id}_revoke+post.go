package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/machine-logins/{id}/revoke",
	Auth:        authAny,
	Description: "Revoke a machine login, expired or not, ending every session it enrolled, a service's pods and a person's pod included. Anyone signed in revokes a service's login. Only the person who approved a person's machine login revokes that one. Only the person that a person's pod login names (its person: the broker's BROKER_POD_OPERATORS maps the pod's Kubernetes account to that person) revokes that one, and the revoke does not keep the pod out: the pod logs in again with its token under a new login while the broker's BROKER_SERVICE_LOGINS names its account, and that login is the person's while BROKER_POD_OPERATORS still maps the account to that person. A caller Dispatch calls a restricted member revokes their own logins alone: a service's login is 404 to them, as an id no login has, and so it is to any caller while the broker cannot ask Dispatch what kind of member the caller is, who still revokes their own.",
	Handler:     (*server).revokeMachineLogin,
	Successor:   successorIs(http.MethodDelete, "/machine-logins/{id}"),
	WriteClass:  scopeDeployment,
	ID:          &idRoute{Reason: "a machine login id"},
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	ApproveOnly: true,
	Shape: routeShape{
		// POST /v1/launcher-credentials/{id}/revoke-by-approver
		// (cipher.Client.RevokeMachineLogin).
		Status: http.StatusOK, Response: wire.StateResponse{},
	},
})
