package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/asks/{id}/resolve",
	Auth:        authAny,
	Description: "Retract or resolve an ask without answering it (kind retracted or resolved; withdrawn is written only by an issue's close, and an ask on a closed issue is 409 ISSUE_CLOSED); a block ask's block records it too. On an issue Legion runs, a retraction or a resolution of a block ask whose block, or a copy of it, is in its spec or Verification document, by a session that is no person and no architect of the issue or of an issue above it, is 403 DOCUMENT_WRITER_NOT_ARCHITECT. A person who will not answer declines instead, on a route of their own, POST /api/v1/asks/{id}/decline.",
	Handler:     (*server).resolveAsk,
	Successor:   successorIs(http.MethodPut, "/asks/{id}/state"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "ask", NotFound: apicode.NotFound},
	Shape:       routeShape{Status: http.StatusOK, Response: shapedAsk{}, Request: resolveAskInput{}},
})
