package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/artifacts/{slug}/blocks",
	Auth:        authAny,
	Description: "A project document's blocks and tokens, by slug or unambiguous filename.",
	Handler:     (*server).getArtifactBlocks,
	Successor:   deletedFor(http.MethodGet, "/projects/{key}/artifacts?issue=none&slug={slug}&include=blocks"),
	Read:        readEntry{Writes: readWritesNothing, WriteNote: docsCompactionNote},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.ArtifactBlock{}},
})
