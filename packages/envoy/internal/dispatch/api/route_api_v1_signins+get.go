package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/signins",
	Auth:        authAny,
	Description: "List the caller's own sign-ins on their machines (`agent-secrets login`), one per box key they have not revoked, each naming the machine's account, the key's thumbprint, when the person signed in there and when that machine last used it. Nobody lists another person's: Dispatch names the person from its own session.",
	Handler:     (*server).listSignIns,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape: routeShape{
		// GET /v1/person-signins (cipher.Client.SignIns).
		Status: http.StatusOK, Response: wire.PersonSignInsResponse{},
	},
})
