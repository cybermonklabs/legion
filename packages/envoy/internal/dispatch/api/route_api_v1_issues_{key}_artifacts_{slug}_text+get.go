package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/artifacts/{slug}/text",
	Auth:        authAny,
	Description: "An issue document's canonical markdown, whole-document token and version, by slug or unambiguous filename; ?blocks=true carries its blocks beside them, from the one tree.",
	Handler:     (*server).getArtifactText,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	Shape: routeShape{
		Status: http.StatusOK, Response: artifactText{}, Params: artifactTextQuery{},
	},
})
