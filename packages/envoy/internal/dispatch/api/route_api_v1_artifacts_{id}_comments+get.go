package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/comments",
	Auth:        authAny,
	Description: "List a document's comments. The answer carries an ETag, which moves for every write the list shows, a delivery attempt that appends no event included; If-None-Match answers 304.",
	Handler:     (*server).listArtifactComments,
	Successor:   successorIs(http.MethodGet, "/artifacts/{id}/comments"),
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []shapedComment{}},
})
