package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/me/issues/{key}/state",
	Auth:        authAny,
	Description: "Update the caller's read state for an issue. last_read_seq is from 0 to 2147483647 (the largest event position the store holds); any other is 400 INVALID_STATE.",
	Handler:     (*server).putUserState,
	WriteClass:  scopeSession,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: userIssueState{}, Request: userIssueStateInput{},
		Refusals: []routeAnswer{{Status: http.StatusConflict, Body: userStateStaleError{}}},
	},
})
