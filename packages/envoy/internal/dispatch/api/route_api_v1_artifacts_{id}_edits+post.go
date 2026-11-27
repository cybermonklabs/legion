package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/edits",
	Auth:        authAny,
	Description: "Apply quote-anchored edit ops with an optional optimistic-concurrency precondition. A batch that is not a person's own (one whose credential does not hold the person scope: an agent's session, a pod's service account, the deployment's shared token) whose result drops the block of an answered ask, or retypes it, is 409 ANSWERED_ASK_KEPT, quoting each such ask's question and naming its node and its block's address, and writes nothing; a person's own, at a browser or through a token of theirs that holds the person scope, is taken. An edit of a retired document, which takes no new text, is 409 DOCUMENT_RETIRED and writes nothing; one of an issue's decisions document once it is retired is 409 DECISIONS_ARE_NODES instead: a decision is a `decision` block in the spec. On an issue Legion runs, an edit of its spec or Verification document by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT.",
	Handler:     (*server).editArtifact,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusOK, Response: editArtifactResponse{}, Request: editArtifactInput{}, Advice: writeAdvice{},
		Refusals: []routeAnswer{
			{Status: http.StatusConflict, Body: editPreconditionError{}},
			{Status: http.StatusConflict, Body: editLostError{}},
			{Status: http.StatusConflict, Body: ambiguousTargetError{}},
		},
	},
})
