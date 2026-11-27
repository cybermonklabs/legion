package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/artifacts/{slug}/edits",
	Auth:        authAny,
	Description: "Apply edit ops with an optional optimistic-concurrency precondition to an issue document, by slug or unambiguous filename. A batch that is not a person's own and would change a person's decision of the document - take an answered ask's block or a decision a person decided out of it, retype it, or change its words - is 409 ANSWERED_ASK_KEPT, one that adds a requirement block to an issue that is a view on its ancestor's spec is 409 REQUIREMENT_BELONGS_TO_ANCESTOR, naming that ancestor, an edit of a retired document, which only its slug finds, 409 DOCUMENT_RETIRED, one of the issue's decisions document once it is retired 409 DECISIONS_ARE_NODES, and an edit of the issue's decisions query is 409 DECISIONS_QUERY, as POST /api/v1/artifacts/{id}/edits answers them. On an issue Legion runs, an edit of its spec or Verification document by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT, and so is such a session's edit of the issue's tree artifact whose batch creates, changes, removes or moves a requirement, criterion or check block, as POST /api/v1/artifacts/{id}/edits answers it.",
	Handler:     (*server).editArtifact,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: editArtifactResponse{}, Request: editArtifactInput{}, Advice: writeAdvice{},
		Refusals: []routeAnswer{
			{Status: http.StatusConflict, Body: editPreconditionError{}},
			{Status: http.StatusConflict, Body: editLostError{}},
			{Status: http.StatusConflict, Body: ambiguousTargetError{}},
		},
	},
})
