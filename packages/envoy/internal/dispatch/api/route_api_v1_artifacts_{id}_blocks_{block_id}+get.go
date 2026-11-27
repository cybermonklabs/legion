package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/blocks/{block_id}",
	Auth:        authAny,
	Description: "Where one block stands in a document: its path from the top-level block down (type, id, child index), and for a table block, row or cell the table's id, the row index (0 is the header), the cell's column index, the text of the header cell drawn above it and the row's cells; 404 TARGET_NOT_FOUND for an id the live document does not hold.",
	Handler:     (*server).getArtifactBlockPath,
	Successor:   successorIs(http.MethodGet, "/artifacts/{id}/blocks/{block_id}"),
	// The path names a block of the document too, and TARGET_NOT_FOUND is the answer for a block
	// id the document does not hold.
	ID:    &idRoute{Kind: "artifact", Blocks: true, NotFound: apicode.ArtifactNotFound, BlockNotFound: apicode.BlockNotFound},
	Read:  readEntry{Writes: readWritesNothing, WriteNote: docsCompactionNote},
	Shape: routeShape{Status: http.StatusOK, Response: model.BlockPath{}},
})
