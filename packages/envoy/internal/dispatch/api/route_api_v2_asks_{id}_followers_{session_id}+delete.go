package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodDelete, Pattern: "/asks/{id}/followers/{session_id}", Auth: authAny, Versions: onlyV2,
		Description: "Unfollow an ask: a session's own credential for itself, and a credential holding the person scope for " +
			"any session; a bearer that holds no person scope names its session in actor, and acting on another session is " +
			"403 FOLLOWER_FORBIDDEN. 204; 404 FOLLOWER_NOT_FOUND for a session that does not follow it, 404 ASK_NOT_FOUND.",
		Handler:  s.unfollowAskV2,
		Resource: "followers", Kind: kindItem, Key: "{session_id}",
		Body:  requestBody{Kind: bodyJSON, Type: actorInput{}, Optional: true},
		Shape: routeShape{Status: http.StatusNoContent},
		Scope: scopeProject, ProjectOf: projectPathAsk, SessionPath: "session_id",
		IDs: v2AskIDs(),
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, askMissingRefusal, followerSessionBlankRefusal, followOfAnotherSessionRefusal,
			{Condition: apicode.SessionNotFollowing, Code: apicode.FollowerNotFound},
		},
	}
})
