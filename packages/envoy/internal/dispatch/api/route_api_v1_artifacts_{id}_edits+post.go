package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/edits",
	Auth:        authAny,
	Description: "Apply quote-anchored edit ops with an optional optimistic-concurrency precondition. On an issue Legion runs, an edit of its spec or Verification document by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT.",
	Handler:     (*server).editArtifact,
	Successor:   successorIs(http.MethodPost, "/artifacts/{id}/edits"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Shape: routeShape{
		Status: http.StatusOK, Response: editArtifactResponse{}, Request: editArtifactInput{}, Advice: writeAdvice{},
		Refusals: []routeAnswer{
			{Status: http.StatusConflict, Body: editPreconditionError{}},
			{Status: http.StatusConflict, Body: editLostError{}},
			{Status: http.StatusConflict, Body: ambiguousTargetError{}},
		},
	},
})
