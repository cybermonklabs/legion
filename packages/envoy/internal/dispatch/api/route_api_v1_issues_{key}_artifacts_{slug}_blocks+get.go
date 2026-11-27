package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/artifacts/{slug}/blocks",
	Auth:        authAny,
	Description: "An issue document's blocks and tokens, by slug or unambiguous filename.",
	Handler:     (*server).getArtifactBlocks,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfBlocksAtACut, WriteNote: docsCompactionNote},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.ArtifactBlock{}, Params: asOfQuery{}},
})
