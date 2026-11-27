package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/text",
	Auth:        authAny,
	Description: "A document's canonical markdown, its whole-document token, its version and unsettled: version is the newest version whose stored markdown is that markdown, and " + unsettledText + ". With ?blocks=true the answer carries the document's blocks as well, each as GET /api/v1/artifacts/{id}/blocks reports it, rendered from the one tree the markdown and the token come from: a listing read this way cannot be split by another session's write.",
	Handler:     (*server).getArtifactText,
	Successor:   deletedFor(http.MethodGet, "/artifacts/{id}?include=text"),
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Read:        readEntry{Writes: readWritesNothing, WriteNote: docsCompactionNote},
	Shape: routeShape{
		Status: http.StatusOK, Response: artifactText{}, Params: artifactTextQuery{},
	},
})
