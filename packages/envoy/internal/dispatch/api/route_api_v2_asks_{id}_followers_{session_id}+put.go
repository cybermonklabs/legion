package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/asks/{id}/followers/{session_id}", Auth: authAny, Versions: onlyV2,
		Description: "Follow an ask, so its answer, edits, resolution and replies reach the session: a session's own " +
			"credential for itself, and a credential holding the person scope for any session; a bearer that holds no " +
			"person scope names its session in actor, and acting on another session is 403 FOLLOWER_FORBIDDEN. Answers 200 " +
			"the follower {session_id, since}; following an ask the session already follows answers the same and writes " +
			"nothing. 404 ASK_NOT_FOUND.",
		Handler:  s.followAskV2,
		Resource: "followers", Kind: kindItem, Key: "{session_id}",
		Body:  requestBody{Kind: bodyJSON, Type: actorInput{}, Optional: true},
		Shape: routeShape{Status: http.StatusOK, Response: model.AskFollower{}},
		Scope: scopeProject, ProjectOf: projectPathAsk, SessionPath: "session_id",
		IDs:      v2AskIDs(),
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, askMissingRefusal, followerSessionBlankRefusal, followOfAnotherSessionRefusal},
	}
})
