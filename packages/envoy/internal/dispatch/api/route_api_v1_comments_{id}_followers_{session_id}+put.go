package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/comments/{id}/followers/{session_id}",
	Auth:        authAny,
	Description: "Follow the comment thread a comment is in, so a person's later replies there reach the session: rejoin a thread it left, or hear one it never wrote in (a credential holding the person scope for any session; every other for its own session alone). A comment in an ask's thread answers 409 ASK_THREAD: follow the ask.",
	Handler:     (*server).followCommentThread,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: "NOT_FOUND"},
	Shape:       routeShape{Status: http.StatusNoContent, Request: actorInput{}, OptionalRequest: true},
})
