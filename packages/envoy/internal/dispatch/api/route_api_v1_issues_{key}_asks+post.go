package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/asks",
	Auth:        authAny,
	Description: "Open a question ask on an issue (options optional). After the write commits, a related-item check searches the project for the question and waits at most 5 seconds; the response's advice.suggestions says whether it found related items (outcome, with the reason in missing when not made), how it searched, and the three items most like it with each ask item's answer state. An ask anchored in a retired document, which takes no new text, is 409 DOCUMENT_RETIRED and writes nothing; in an issue's decisions document once it is retired it is 409 DECISIONS_ARE_NODES instead: a decision is a `decision` block in the spec.",
	Handler:     (*server).createAsk,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: shapedAsk{}, Request: createAskInput{}, Advice: writeAdvice{},
	},
})
