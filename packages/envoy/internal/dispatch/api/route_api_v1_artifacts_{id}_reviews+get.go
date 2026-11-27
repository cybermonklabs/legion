package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/reviews",
	Auth:        authAny,
	Description: "List a document's approval reviews.",
	Handler:     (*server).listArtifactReviews,
	Successor:   successorIs(http.MethodGet, "/artifacts/{id}/reviews"),
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []artifactReviewRead{}},
})
