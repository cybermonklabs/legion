package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/me/answers",
	Auth:        authAny,
	Description: "The caller's own answers and replies on asks across issues and project documents, newest first: each row is an answer (the ask.answered event's answer, and whether it is still the ask's current one) or a reply, with the ask's question, state, owner and deep link; pages with limit (1-200, default 50) and offset; {rows, total, limit, offset}.",
	Handler:     (*server).listMyAnswers,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: model.MyAnswersResponse{}, Params: myAnswersQuery{}},
})
