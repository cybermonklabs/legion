package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/issues/{key}/nodes/{id}",
	Auth:        authAny,
	Description: "Delete the node {id} names and its part_of subtree {actor?}, in one transaction, or with ?dry_run=true answer how many nodes the delete would take that a read of the issue's tree returns to the caller, writing nothing and taking no write lock. {id} is read as the move reads it; a node the caller may not see under the issue is 404 NOT_FOUND. The delete takes no comment, message, issue node, ask, :::ask block or document of the subtree, nor what stands under one; each block it takes leaves the text of its document in one direct write of the document's tree under the room locks, a block it does not take being lifted to where the taken block's text stood, and each document so changed writes one version with its artifact.version, which moves its open approval request; each lens, finding, risk and constraint it takes goes gone, each comment whose node anchor names one is orphaned, every tuple out of what it takes is retracted, and a converted issue's queries are marked owed and drained once it has committed. Refusals, in order: an ask, an :::ask block, a comment, a message, a result, an issue node or a document as {id} is 400 NODE_NOT_DELETABLE, after a caller who holds no person scope naming the :::ask block of an answered ask, 409 ANSWERED_ASK_KEPT; on the delete, no If-Match is 428 PRECONDITION_REQUIRED and no expected_count 400 DELETE_INPUT; under the owner locks of the issue and of every issue a node of the subtree stands under, a node under an issue it did not lock at each of two attempts is 409 PARENT_CONTENDED, and a tag that is no longer the node's placement_seq 412 PRECONDITION_FAILED; on the dry run as on the delete, a subtree that holds a result is 409 NODE_HOLDS_RESULT, a requirement, criterion or check on an issue Legion runs from a writer who is no person and no architect 403 DOCUMENT_WRITER_NOT_ARCHITECT, from a caller who holds no person scope a person's decision 409 ANSWERED_ASK_KEPT and a lens, finding, risk or constraint 403 STATE_NOT_PERMITTED, each naming a node only where the caller may see it; on the delete, an expected_count that is not the count the delete takes is 409 DELETE_COUNT_MOVED, carrying the count now in count; and last, on the dry run as on the delete, a delete that would leave a callout or a list item it keeps with no block of its own 409 CONTAINER_WOULD_EMPTY, naming the container and its kind, which the dry run reads off the live document with no write lock. The dry run reads no expected_count. A refused delete stores nothing. Answers 200 {count}.",
	Handler:     (*server).deleteNodeRoute,
	WriteClass:  scopeProject,
	ID:          &idRoute{Reason: "a node's store address under the issue, which FindNodeUnderIssue decodes and no prefix shortens"},
	// The delete of a node and its subtree, and its dry run (node_delete.go): the count, and the
	// count now where the expected_count it confirms has moved.
	Shape: routeShape{
		Status: http.StatusOK, Response: nodeDeleted{}, Request: actorInput{}, OptionalRequest: true,
		Params: nodeDeleteQuery{}, Headers: []routeHeader{deleteIfMatch},
		Refusals: []routeAnswer{{Status: http.StatusConflict, Body: deleteCountMoved{}}},
	},
})
