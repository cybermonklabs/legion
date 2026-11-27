package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/comments",
	Auth:        authAny,
	Description: "Comment on a document or one of its quotes. An issue's document, its decisions document included, is 400 ARTIFACT_LINKED whether or not it is retired, naming its issue's routes: comment on it through POST /api/v1/issues/{key}/comments. A reply names the comment it answers in reply_to, and one block of it in reply_to_block. " + replyQuoteDescription("reply_to_block", "INVALID_COMMENT") + " A reply to a deleted comment is 409 POST_DELETED: a withdrawn post takes no new text. A comment on a quote of a retired document, which takes no new text, is 409 DOCUMENT_RETIRED and writes nothing. A reply to a thread on a quote of a retired document is taken, and writes nothing into the document. " + commentPartiesDescription,
	Handler:     (*server).createArtifactComment,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusCreated, Response: commentWriteResponse{}, Request: commentInput{}, Advice: writeAdvice{},
	},
})
