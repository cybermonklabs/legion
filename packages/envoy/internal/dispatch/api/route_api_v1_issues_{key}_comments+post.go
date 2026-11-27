package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/comments",
	Auth:        authAny,
	Description: "Comment on an issue, a document quote, or an ask. A reply names the comment it answers in reply_to, and one block of it in reply_to_block; with ask_id in place of reply_to, reply_to_block names one part of that ask. A reply to a deleted comment is 409 POST_DELETED: a withdrawn post takes no new text. A comment on a quote of a retired document, which takes no new text, is 409 DOCUMENT_RETIRED and writes nothing; in an issue's decisions document once it is retired it is 409 DECISIONS_ARE_NODES instead: a decision is a `decision` block in the spec. A reply to a thread on a quote of a retired document is taken, and writes nothing into the document. " + commentPartiesDescription,
	Handler:     (*server).createComment,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: commentWriteResponse{}, Request: commentInput{}, Advice: writeAdvice{},
	},
})
