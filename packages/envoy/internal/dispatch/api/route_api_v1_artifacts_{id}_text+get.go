package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/text",
	Auth:        authAny,
	Description: "A document's canonical markdown, its whole-document token, its version and its asks. A reader that reads asks sends Dispatch-Asks: 1 and is served markdown with no server-owned attribute on any ask block (state, answered_by, answered_at, selected, answer, invalid, copied_from, copied_from_document), whatever the live document or its stored versions hold, so the text uploads back unchanged; a request without that header is served the form kept for one release for the readers of the release before, with selected, answer and invalid taken off every ask block and state, answered_by, answered_at, copied_from and copied_from_document as the document holds them. Both forms answer the same token and the same asks, and a version's own text (GET /api/v1/artifacts/{id}/versions/{number}) is what the one renderer answers for that version, which names its source in Dispatch-Render-Source. asks answers each ask block of the markdown in document order, read from the ask that answers for it, under the names those attributes carried: {block_id, state?, answered_by?, answered_at?, selected?, answer?, copied_from?, copied_from_document?}. state is the ask's, a copy's source's (copied_from naming the source ask and copied_from_document its document's dispatch:// address), resolved for a block of a closed issue's document no settlement has indexed, and absent for any other block no ask indexes yet, a question nobody has opened; who answered, when, the options chosen and the words stand on an answered ask alone. A document with no ask block answers no asks member, as the release before answered every document. With ?blocks=true the answer carries the document's blocks as well, each as GET /api/v1/artifacts/{id}/blocks reports it, rendered from the one tree the markdown and the token come from, so their byte ranges index the markdown served: a listing read this way cannot be split by another session's write. An issue's tree artifact (kind tree) answers its own fragment of the issue room: the nodes written into the issue rather than into a document.",
	Handler:     (*server).getArtifactText,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Read:        readEntry{Writes: readWritesNothing, WriteNote: docsCompactionNote},
	Shape: routeShape{
		Status: http.StatusOK, Response: artifactText{}, Params: artifactTextQuery{},
	},
})
