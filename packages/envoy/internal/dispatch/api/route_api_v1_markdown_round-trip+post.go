package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/markdown/round-trip",
	Auth:        authAny,
	Description: "The markdown Dispatch would store for the text a body gives it {markdown}: the parse and the render every write of a whole document makes. The route writes nothing and reads no row. A caller compares the answer with the text it holds, and refuses its own write where the two differ. Text the parse or the render refuses comes back as it was given, so the write that follows refuses it with its own error.",
	Handler:     (*server).postMarkdownRoundTrip,
	// The round trip writes nothing and reads no row: it answers what a document write would
	// store for the text it is given.
	WriteClass: scopeNoWrite,
	Shape: routeShape{
		Status: http.StatusOK, Response: markdownRoundTripResponse{}, Request: markdownRoundTripInput{},
	},
})
