package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/comments/{id}/followers/{session_id}",
	Auth:        authAny,
	Description: "Leave the comment thread a comment is in, so a person's later replies there no longer reach the session, as a writer in the thread, of its document or of the issue's spec; a reply that mentions it still does, the holder of the issue's claim or route still gets every reply, and a later comment of it in the thread rejoins it (a credential holding the person scope for any session; every other for its own session alone). A comment in an ask's thread answers 409 ASK_THREAD: unfollow the ask.",
	Handler:     (*server).unfollowCommentThread,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: "NOT_FOUND"},
	Shape:       routeShape{Status: http.StatusNoContent, Request: actorInput{}, OptionalRequest: true},
})
