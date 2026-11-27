package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/artifacts/{slug}/versions/{number}",
	Auth:        authAny,
	Description: "One version of a project's document, file or image, by slug or unambiguous filename, as GET /api/v1/artifacts/{id}/versions/{number} answers it: a document as JSON, a file or an image as its bytes with Content-Disposition: attachment naming the artifact's name, -v<number> before the extension for an older version. With ?download=true every kind answers Content-Disposition: attachment with that name and Cache-Control: private, no-cache, and a document answers its Markdown as text/markdown. {number} is a version from 1 to 2147483647 (the largest the store holds), and any other is 400 INVALID_VERSION.",
	Handler:     (*server).getArtifactVersion,
	Successor:   deletedFor(http.MethodGet, "/artifacts/{id}/versions/{number}"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: artifactVersionText{}, Params: artifactVersionQuery{},
		Opaque:             "a file version answers that file's bytes under its own content type, named by Content-Disposition (serveFileVersion); with download=true a document version answers its Markdown as text/markdown; only a document version read without that parameter carries the JSON above",
		OpaqueContentTypes: []string{"application/octet-stream", "text/markdown"},
	},
})
