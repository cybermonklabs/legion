package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/going/capability",
	Auth:        authAny,
	Description: "Whether this deployment has a text model configured: {model_configured} and nothing else, so no endpoint, deployment name or credential can appear in it. Any caller Dispatch admits reads it, a pod's own agent token included.",
	Handler:     (*server).getGoingCapability,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: goingCapability{}},
})
