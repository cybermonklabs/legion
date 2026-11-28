package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/schema/blocks",
	Auth:        authPublic,
	Description: "The typed document block schema this process installed, as JSON {version, nodes, marks, types}, and beside it, read from the store on each request: store_types, every type of the store that declares no block type, by name, with the attributes its attribute nodes hold; and lens_kinds, the lens catalogue by id, each kind with its address, its words and in sources the constraint holding its primary source (standard) and that source's reference. A type or a lens kind written after the process started is answered by its next request, and neither moves the version.",
	Handler:     (*server).getBlockSchema,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: blockSchemaRead{}},
})
