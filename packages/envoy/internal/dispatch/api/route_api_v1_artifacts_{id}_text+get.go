package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/text",
	Auth:        authAny,
	Description: "A document's canonical markdown, its whole-document token and its version, with asks: one entry per ask block in document order, its state and answer read from its ask. A reader that reads asks sends Dispatch-Asks: 1 and is served markdown with none of an ask block's eight server-owned attributes (state, answered_by, answered_at, selected, answer, invalid, copied_from, copied_from_document), which it can upload back unchanged; a request without that header is served the form kept for one release for the readers of the release before, with selected, answer and invalid taken off every ask block and state, answered_by, answered_at, copied_from and copied_from_document as the document holds them. Both forms answer the same token. With ?blocks=true the answer carries the document's blocks as well, each as GET /api/v1/artifacts/{id}/blocks reports it, rendered from the one tree the markdown and the token come from: a listing read this way cannot be split by another session's write.",
	Handler:     (*server).getArtifactText,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Read:        readEntry{Writes: readWritesNothing, WriteNote: docsCompactionNote},
	Shape: routeShape{
		Status: http.StatusOK, Response: artifactText{}, Params: artifactTextQuery{},
	},
})
