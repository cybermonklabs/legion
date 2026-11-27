package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/versions",
	Auth:        authAny,
	Description: "Name the current document version. A retired document, which takes no new text, is 409 DOCUMENT_RETIRED and writes no version; an issue's decisions document once it is retired is 409 DECISIONS_ARE_NODES instead: a decision is a `decision` block in the spec. On an issue Legion runs, a named version of its spec or Verification document by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT.",
	Handler:     (*server).createNamedVersion,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.Version{}, Request: createVersionInput{},
	},
})
