package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/asks",
	Auth:        authAny,
	Description: "Open a question ask on an issue (options optional). After the write commits, a related-item check searches the project for the question and waits at most 5 seconds; the response's advice.suggestions says whether it found related items (outcome, with the reason in missing when not made), how it searched, and the three items most like it with each ask item's answer state.",
	Handler:     (*server).createAsk,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: shapedAsk{}, Request: createAskInput{}, Advice: writeAdvice{},
	},
})
