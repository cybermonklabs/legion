package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/artifacts/{slug}/edits",
	Auth:        authAny,
	Description: "Apply edit ops with an optional optimistic-concurrency precondition to a project document, by slug or unambiguous filename.",
	Handler:     (*server).editArtifact,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: editArtifactResponse{}, Request: editArtifactInput{},
		// A project document has no issue state to report (artifacts.go, the withRawAdvice arm).
		Advice: documentWriteAdvice{},
		Refusals: []routeAnswer{
			{Status: http.StatusConflict, Body: editPreconditionError{}},
			{Status: http.StatusConflict, Body: editLostError{}},
			{Status: http.StatusConflict, Body: ambiguousTargetError{}},
		},
	},
})
