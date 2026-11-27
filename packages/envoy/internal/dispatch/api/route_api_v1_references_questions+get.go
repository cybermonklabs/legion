package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/references/questions",
	Auth:        authAny,
	Description: "The question map of one reference source's document: ?kind= one of artifact, ask, comment or message and ?id= that source's whole uuid or a unique prefix of 8 characters or more. Answers {document, questions}: document the uuid of the document the source's text belongs to - its own for a document version, the document an ask or a comment is written on or anchored to, its issue's spec for an issue message - and questions each `:::ask{#id}` block that document's latest version declares, by block id, with the whole id of the ask settlement made for it and that ask's `dispatch://` address, which is what a `#id` in the source's text names and the address its edge is stored under (empty for an ask on an agent conversation's document, which no address names). A block no ask is settled for yet is absent, and a document declaring none answers []. A source whose text belongs to no document - a direct message, an id nothing holds - is 404 NOT_FOUND; a kind that is none of the four is 400 INVALID_KIND; a missing id is 400 INVALID_QUERY; an id prefix two sources carry is 409 ID_AMBIGUOUS.",
	Handler:     (*server).getReferenceQuestions,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: referenceQuestions{}, Params: referenceQuestionsQuery{},
		// ?id= resolves an id written short itself, as ?to= of GET /api/v1/references does.
		Refusals: []routeAnswer{{Status: http.StatusConflict, Body: idAmbiguousError{}}},
	},
})
