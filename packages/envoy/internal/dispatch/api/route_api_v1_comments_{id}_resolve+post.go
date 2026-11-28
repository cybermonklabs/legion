package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/comments/{id}/resolve",
	Auth:        authAny,
	Description: "Resolve a comment thread. The answer is the comment as the resolve left it, with previous beside it: the thread's resolution {resolved, resolved_by, resolved_at} when the resolve took the comment's lock. The command bar's undo of a reopen makes the resolve conditional by an in-process condition on the request's context, never a body member: the thread must be open when the resolve takes the comment's lock, else 409 COMMENT_CHANGED, nothing written (500 GOING_CONDITION_MISSING for such a write back carrying no condition).",
	Handler:     (*server).resolveComment,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: "NOT_FOUND"},
	Shape:       routeShape{Status: http.StatusOK, Response: resolvedComment{}, Request: actorInput{}, OptionalRequest: true},
})
