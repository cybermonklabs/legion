package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/artifacts/{slug}/edits",
	Auth:        authAny,
	Description: "Apply edit ops with an optional optimistic-concurrency precondition to a project document, by slug or unambiguous filename. A batch that is not a person's own and would change a person's decision of the document - take an answered ask's block or a decision a person decided out of it, retype it, or change its words - is 409 ANSWERED_ASK_KEPT, and an edit of a retired document, which only its slug finds, 409 DOCUMENT_RETIRED, as POST /api/v1/artifacts/{id}/edits answers them.",
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
