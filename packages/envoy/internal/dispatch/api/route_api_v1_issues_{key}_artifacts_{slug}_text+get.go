package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/artifacts/{slug}/text",
	Auth:        authAny,
	Description: "An issue document's canonical markdown, whole-document token, version and asks, by slug or unambiguous filename, as GET /api/v1/artifacts/{id}/text answers them with and without Dispatch-Asks: 1, and each ask block's state and answer in asks; ?blocks=true carries its blocks beside them, from the one tree.",
	Handler:     (*server).getArtifactText,
	Read:        readEntry{Writes: readWritesNothing, WriteNote: docsCompactionNote},
	Shape: routeShape{
		Status: http.StatusOK, Response: artifactText{}, Params: artifactTextQuery{},
	},
})
