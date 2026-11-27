package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/nodes/{id}/findings",
	Auth:        authAny,
	Description: "Add one finding to a lens {text, visibility?}: a block node under the lens {id} names, at a random block id, addressed dispatch://lens/<uuid>#b-<id>, after every finding the lens holds. {id} is the lens's store address under the issue (lens/<uuid>, or its dispatch:// address), percent-encoded as one path segment; a node the caller may not see under the issue is 404 NOT_FOUND, a gone one 409 NODE_GONE, naming it, and a node that is no lens 400 INVALID_POINTER; visibility author is 403 AUTHOR_NAMES_NOBODY on a credential that proves no author. With no visibility the finding takes its lens's; visibility project under a lens reserved for its author is 400 VISIBILITY_WIDER_THAN_PARENT, naming the finding and the lens, and stores nothing. An empty text is 400 FINDING_INPUT. A refused write stores nothing, and the write appends no event. Answers 201 with the finding: its address, id, block_id, its lens's address, text, author, created_at, state and visibility.",
	Handler:     (*server).addLensFinding,
	WriteClass:  scopeProject,
	// A node's routes under an issue, whose {id} is the node's store address (`lens/<uuid>`, a
	// block's node id, or its whole `dispatch://` address), one percent-encoded path segment that
	// FindNodeUnderIssue decodes and no prefix shortens: the route answers NOT_FOUND wherever it
	// names no node the caller may see under the issue.
	ID: &idRoute{Reason: "a node's store address under the issue, which FindNodeUnderIssue decodes and no prefix shortens"},
	Shape: routeShape{
		Status: http.StatusCreated, Response: lensFinding{}, Request: lensFindingInput{},
	},
})
