package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/text",
	Auth:        authAny,
	Description: "A document's canonical markdown, its whole-document token and its version. With ?blocks=true the answer carries the document's blocks as well, each as GET /api/v1/artifacts/{id}/blocks reports it, rendered from the one tree the markdown and the token come from: a listing read this way cannot be split by another session's write.",
	Handler:     (*server).getArtifactText,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route answers the markdown and the document token of the document's newest version at the cut, and with ?blocks=true the blocks of that version.", WriteNote: docsCompactionNote},
	Shape: routeShape{
		Status: http.StatusOK, Response: artifactText{}, Params: artifactTextAsOfQuery{},
	},
})
