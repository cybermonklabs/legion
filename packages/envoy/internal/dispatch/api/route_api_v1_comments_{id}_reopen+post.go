package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/comments/{id}/reopen",
	Auth:        authAny,
	Description: "Reopen a resolved comment thread, whatever resolution it holds. Its optional body {actor?} names the session the reopen acts for where the caller's credential proved none, as the person routes take it (403 ACTOR_SESSION_MISMATCH where the credential acts for another session); a body that does not decode is ignored, as an empty one is. The answer is the comment as the reopen left it, with previous beside it: the thread's resolution {resolved, resolved_by, resolved_at} when the reopen took the comment's lock. The command bar's undo of a resolve makes the reopen conditional by an in-process condition on the request's context, never a body member: the thread must still hold the resolution the turn left, as the comment's read answers resolved_at, when the reopen takes the comment's lock, else 409 COMMENT_CHANGED, nothing written (500 GOING_CONDITION_MISSING for such a write back carrying no condition).",
	Handler:     (*server).reopenComment,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: "NOT_FOUND"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusOK, Response: resolvedComment{}, Request: actorInput{}, OptionalRequest: true},
})
