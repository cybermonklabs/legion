package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/artifacts/{slug}/text",
	Auth:        authAny,
	Description: "An issue document's canonical markdown, whole-document token, version and unsettled, by slug or unambiguous filename, as GET /api/v1/artifacts/{id}/text answers them: version is the newest version whose stored markdown is that markdown, and " + unsettledText + "; ?blocks=true carries its blocks beside them, from the one tree.",
	Handler:     (*server).getArtifactText,
	Successor:   deletedFor(http.MethodGet, "/issues/{key}/artifacts?slug={slug}&include=text"),
	Read:        readEntry{Writes: readWritesNothing, WriteNote: docsCompactionNote},
	Shape: routeShape{
		Status: http.StatusOK, Response: artifactText{}, Params: artifactTextQuery{},
	},
})
