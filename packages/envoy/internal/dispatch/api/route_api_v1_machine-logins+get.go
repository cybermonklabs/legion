package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/machine-logins",
	Auth:        authAny,
	Description: "List the machine logins the caller may revoke that can still reach a secret: the secrets broker's launcher credentials for every service's login, whoever approved it, for the caller's own machines, and for the caller's own pods, unexpired or expired with a session still running, each naming who approved it. A person's pod login is the login of a pod whose Kubernetes account the broker's BROKER_POD_OPERATORS maps to a person. Only that person's list holds it, and its person field names that person. The field is null on every other login. A caller Dispatch calls a restricted member is listed their own machines' and pods' logins alone, and so is any caller while the broker cannot ask Dispatch what kind of member the caller is.",
	Handler:     (*server).listMachineLogins,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		// GET /v1/launcher-credentials (cipher.Client.MachineLogins).
		Status: http.StatusOK, Response: wire.LauncherCredentialsResponse{},
	},
})
