package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPatch,
	Pattern:     "/api/v1/asks/{id}",
	Auth:        authAny,
	Description: "Reword an open ask (its author, or a credential holding the person scope); a block ask's `:::ask` block is rewritten with it. A block ask whose block is in a retired document, which takes no new text, is 409 DOCUMENT_RETIRED and writes nothing; in an issue's decisions document once it is retired it is 409 DECISIONS_ARE_NODES instead: a decision is a `decision` block in the spec. On an issue Legion runs, a reword of a block ask in its spec or Verification document by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT.",
	Handler:     (*server).editAsk,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "ask", NotFound: "NOT_FOUND"},
	Shape:       routeShape{Status: http.StatusOK, Response: shapedAsk{}, Request: editAskInput{}},
})
