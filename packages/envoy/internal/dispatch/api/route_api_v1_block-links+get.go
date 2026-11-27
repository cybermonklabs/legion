package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/block-links",
	Auth:        authAny,
	Description: "Links from blocks of one document to blocks of another: ?from=<document ref>&to=<document ref>; both block nodes, the edge kind, and when and in which version the source wrote each, newest first.",
	Handler:     (*server).getBlockLinks,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: model.BlockLinks{}, Params: blockLinksQuery{}},
})
