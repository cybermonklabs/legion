package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/comments",
	Auth:        authAny,
	Description: "List an issue's comments; ?artifact= scopes to a document. The answer carries an ETag, which moves for every write the list shows, a delivery attempt that appends no event included; If-None-Match answers 304. A comment's author carries proven: true when the write's credential proved it (a browser's person, a broker-issued token's session), and no proven when the request only named it.",
	Handler:     (*server).listComments,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: []shapedComment{}, Params: issueCommentsQuery{},
	},
})
