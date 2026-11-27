package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/comments",
	Auth:        authAny,
	Description: "Comment on a document or one of its quotes. A reply to a deleted comment is 409 POST_DELETED: a withdrawn post takes no new text. " + commentPartiesDescription,
	Handler:     (*server).createArtifactComment,
	Replay:      func(s *server) keyedReplay { return replayRecovered(s, s.replayComment) },
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusCreated, Response: commentWriteResponse{}, Request: commentInput{}, Advice: writeAdvice{},
	},
})
