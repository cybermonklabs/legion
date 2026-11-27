package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/artifacts/{slug}/versions",
	Auth:        authAny,
	Description: "Name the current version of an issue document, by slug or unambiguous filename. A retired document, which only its slug finds, is 409 DOCUMENT_RETIRED, and the issue's decisions document once it is retired 409 DECISIONS_ARE_NODES, as POST /api/v1/artifacts/{id}/versions answers them. On an issue Legion runs, a named version of its spec or Verification document by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT.",
	Handler:     (*server).createNamedVersion,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.Version{}, Request: createVersionInput{},
	},
})
