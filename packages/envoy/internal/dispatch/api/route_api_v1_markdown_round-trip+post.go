package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/markdown/round-trip",
	Auth:        authAny,
	Description: fmt.Sprintf("The markdown Dispatch would store for {markdown}, the text an inline upload sends as its content: the checks, the parse and the render an upload of a markdown document makes of the text alone. The route writes nothing and reads no row. A caller compares the answer with the text it holds, and refuses its own write where the two differ. A body naming no markdown asks about the empty text. Text every upload of a markdown document refuses for the text alone, a replacement included (a JSON upload of blank content over a held document is refused as blank before its text is parsed), is refused as that upload refuses it, with its status, code and words: 400 NUL_CHARACTER for text holding U+0000, in the words the upload refuses its content with (content holds ...; this route refused such text before too, in the words markdown holds ...), 413 CAP_EXCEEDED for text past the bytes one document holds or making more elements than one write may, 400 INVALID_MARKDOWN for markdown the parser refuses or whose rendering no reader reads back, 400 INVALID_ASK_BLOCK for an ask that breaks its content rule in a text whose rendering no reader reads back, which that refusal names instead, and a render that fails at the status the upload answers it with. A body with {new_document} true is answered as the upload of a new document: it asks too for the refusals only a new document's seed makes, which a replacement, judged against the document it replaces, relaxes: 400 INVALID_MARKDOWN for a block id named on two blocks or an element this build cannot create yet, 400 INVALID_ASK_BLOCK for an ask that breaks its content rule, 413 CAP_EXCEEDED for text whose rendering is past the bytes or the elements one document holds, and 400 INVALID_CI_CHECK for a check proved by CI that carries a run, takes a when other than before_merge or names no negative control. A text that breaks two of the rules is refused with the first the seed makes. A body without {new_document} is answered as the most permissive replacement, one over a document that already holds what the text holds, takes the text: with the markdown that replacement stores, or with the first refusal every upload makes of the text. For a text holding a block id named on two blocks or an element this build cannot create yet, that markdown is the rendering, where this route answered the text itself before. Each refusal, whatever its status, carries the response header %s, valued %s, which marks the answer as the refusal of the text; a caller reads only that the header is there. No other answer of the route carries it; the route's own 413 CAP_EXCEEDED for a body past its bound does not. Text of whitespace alone that none of these refuses is answered with the empty markdown.", contracts.TextRefusalHeader, textRefusalMark),
	Handler:     (*server).postMarkdownRoundTrip,
	// The round trip writes nothing and reads no row: it answers what a document write would
	// store for the text it is given.
	WriteClass: scopeNoWrite,
	Shape: routeShape{
		Status: http.StatusOK, Response: markdownRoundTripResponse{}, Request: markdownRoundTripInput{},
	},
})
