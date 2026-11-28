package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/comments",
	Auth:        authAny,
	Description: "List a document's comments. The answer carries an ETag, which moves for every write the list shows, a delivery attempt that appends no event included; If-None-Match answers 304.",
	Handler:     (*server).listArtifactComments,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfCommentListAtACut},
	Shape:       routeShape{Status: http.StatusOK, Response: []shapedComment{}, Params: asOfQuery{}},
})
