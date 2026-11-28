package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}",
	Auth:        authAny,
	Description: "Download one version of an agent's conversation's file or image, as the issue form serves it: its bytes under its own content type, with Content-Disposition: attachment naming the file's name, -v<number> before the extension for an older version (report-v2.pdf), and Cache-Control: private, max-age=31536000, immutable. With ?download=true the answer carries Content-Disposition: attachment with that name and Cache-Control: private, no-cache. A conversation holds no document, so no version here answers Markdown. {number} is a version from 1 to 2147483647 (the largest the store holds), and any other is 400 INVALID_VERSION.",
	Handler:     (*server).getArtifactVersion,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape: routeShape{
		Status: http.StatusOK, Params: artifactVersionQuery{},
		Opaque:             "a conversation holds files and images alone, so a version is that file's bytes under its own content type, named by Content-Disposition with the file's name (serveFileVersion)",
		OpaqueContentTypes: []string{"application/octet-stream"},
	},
})
