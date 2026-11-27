package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/blocks",
	Auth:        authAny,
	Description: "A document's blocks with markdown ranges, tokens, reference counts and each block's embedding, where meaning search stands on it (embedded, pending, or none while no window covers it); a typed block carries attributes, its type's attributes in declaration order as its opening line writes them ({name, value, kind}, the value unescaped and unquoted and the kind the one the block schema declares it, so a reader follows a block[] value as the pointers it holds and reads every other kind as text), and a table block lists its rows and cells by the indexes delete_row and delete_column take.",
	Handler:     (*server).getArtifactBlocks,
	Successor:   deletedFor(http.MethodGet, "/artifacts/{id}?include=blocks"),
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Read:        readEntry{Writes: readWritesNothing, WriteNote: docsCompactionNote},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.ArtifactBlock{}},
})
