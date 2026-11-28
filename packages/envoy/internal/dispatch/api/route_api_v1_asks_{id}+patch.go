package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPatch,
	Pattern:     "/api/v1/asks/{id}",
	Auth:        authAny,
	Description: "Reword an open ask (its author, or a credential holding the person scope); a block ask's `:::ask` block is rewritten with it. On an issue Legion runs, a reword of a block ask in its spec or Verification document by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT.",
	Handler:     (*server).editAsk,
	Successor:   successorIs(http.MethodPatch, "/asks/{id}"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "ask", NotFound: apicode.NotFound},
	Shape:       routeShape{Status: http.StatusOK, Response: shapedAsk{}, Request: editAskInput{}},
})
