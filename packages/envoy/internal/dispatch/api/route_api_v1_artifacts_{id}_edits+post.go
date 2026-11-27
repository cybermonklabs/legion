package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/edits",
	Auth:        authAny,
	Description: "Apply quote-anchored edit ops with an optional optimistic-concurrency precondition. An issue's tree artifact (kind tree) takes an edit as a document does, into its own fragment of the issue room. An op naming, by its store address (<ref_key>#<block id>), a node of another issue is 409 NODE_IN_OTHER_ISSUE, naming that node's issue, and writes nothing. A batch that is not a person's own (one whose credential does not hold the person scope: an agent's session, a pod's service account, the deployment's shared token) whose result would change a person's decision of the document - take an answered ask's block or a decision a person decided out of it, retype it, or change its words - is 409 ANSWERED_ASK_KEPT, quoting each such ask's question or decision's words and naming its node and its block's address, and writes nothing; a person's own, at a browser or through a token of theirs that holds the person scope, is taken. A batch whose result adds a requirement block to an issue that is a view on its ancestor's spec - an issue born converted whose spec is not its own and whose nearest ancestor holding requirements and no view itself is not done, the view_of GET /api/v1/issues/{key}/tree answers - is 409 REQUIREMENT_BELONGS_TO_ANCESTOR, naming that ancestor, and writes nothing; one taken whose result holds a requirement block on an issue that is no view makes the issue's spec its own, so it is never a view. An edit of a retired document, which takes no new text, is 409 DOCUMENT_RETIRED and writes nothing; one of an issue's decisions document once it is retired is 409 DECISIONS_ARE_NODES instead: a decision is a `decision` block in the spec. An edit of the issue's decisions query, which renders the issue's decisions and takes no text, is 409 DECISIONS_QUERY and writes nothing. On an issue Legion runs, an edit of its spec or Verification document by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT, and so is such a session's edit of the issue's tree artifact whose batch creates, changes, removes or moves a requirement, criterion or check block, naming the first such block; an edit of the tree artifact that touches none of those blocks is taken, and each refused edit writes nothing.",
	Handler:     (*server).editArtifact,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusOK, Response: editArtifactResponse{}, Request: editArtifactInput{}, Advice: writeAdvice{},
		Refusals: []routeAnswer{
			{Status: http.StatusConflict, Body: editPreconditionError{}},
			{Status: http.StatusConflict, Body: editLostError{}},
			{Status: http.StatusConflict, Body: ambiguousTargetError{}},
		},
	},
})
