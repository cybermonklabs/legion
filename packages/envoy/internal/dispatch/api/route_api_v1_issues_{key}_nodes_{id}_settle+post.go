package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/nodes/{id}/settle",
	Auth:        authAny,
	Description: "Settle the proposed node {id} names {actor?}: one state tuple at settled, written by the caller, and no event. {id} is the node's store address under the issue, percent-encoded as one path segment; a node the caller may not see under the issue is 404 NOT_FOUND, in the words a node nobody wrote answers. A gone node is 409 NODE_GONE, naming it. A settle on the block of an :::ask block of a document is 409 ASK_HOLDS_STATE and writes nothing, naming the ask whose node holds the question's state (the block's own ask or the source ask for a copy) by its dispatch://<KEY>/ask/<uuid> address, or saying no ask holds the question yet. Any other node that is not at proposed is 409 NODE_NOT_PROPOSED. A person settles any proposal - a browser's cookie, or a token holding the person scope, whose settle is written as the person it acts for - and any other caller only a proposal whose recorded author (its made_by; for a block of a document, the writer of the version that first held the block) is the author its credential proves, by the one author rule that decides who reads a node reserved for its author: the same person for a personal token or a broker session that names a person, the same session for a broker session that names none, and the same workload for a pod's service-account token; any other is 403 SETTLE_NOT_PERMITTED, naming the node. The deployment's shared agent token proves no author, so it settles nothing. On an issue Legion runs, a settle of a requirement, criterion or check block by a session that is no person and no architect of the issue or of an issue above it, its own proposal among them, is then 403 DOCUMENT_WRITER_NOT_ARCHITECT, and nothing is stored. Where the settle changes a query of a converted issue, the issue's queries are marked owed in the write and their versions written once it has committed. Answers 200 {node: {address, kind, state, visibility}, previous}.",
	Handler:     (*server).settleNode,
	WriteClass:  scopeProject,
	ID:          &idRoute{Reason: "a node's store address under the issue, which FindNodeUnderIssue decodes and no prefix shortens"},
	Shape: routeShape{
		Status: http.StatusOK, Response: nodeStateWritten{}, Request: actorInput{}, OptionalRequest: true,
	},
})
