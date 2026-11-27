package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/nodes/{id}/pointers",
	Auth:        authAny,
	Description: "Append, retract or retarget one pointer tuple out of the node {id} names {kind, action, to, from?}. kind is has_kind (a lens to its lens_kind, to being a lens_kind id), triggered_by (a lens to a lens of the issue invoked before it, which asked for it), constrained_by (a requirement block to a constraint of the issue or of the catalogue) or prevented_by (a risk to a check block of any issue of the issue's project, to and a retarget's from alike, as the pre-mortem conversion writes one). action append asserts a tuple at to, retract retracts the one at to, and retarget moves the one at from to to, retracting the old and asserting the new in one write and touching no part_of tuple. {id} is the node's store address under the issue, percent-encoded as one path segment. The store holds each tuple to its type rule: a far end of a type the rule does not allow, a prevented_by at a check of another project or of a retired document, naming the check, a triggered_by at the lens itself or at a lens not invoked before it (by the seq of each lens's first part_of tuple), naming both lenses, and a lens left with no kind are 400 INVALID_POINTER, so no triggered_by closes a cycle; a second has_kind, and a retarget of a pre-mortem lens's has_kind away from pre-mortem, are 409 POINTER_ALREADY_ASSERTED, one body whatever hangs under the lens; a retraction or a retarget of a tuple the node does not hold is 404 NOT_FOUND. A node the caller may not see under the issue, and a far end the caller may not see under the issue (a prevented_by's in any issue), answer 404 NOT_FOUND, as one that does not exist, and so does a far end that is gone; a gone node {id} is 409 NODE_GONE, naming it, since gone is final; a has_kind at an id no lens_kind node holds is 400 INVALID_POINTER; part_of, which moves through the node's parent route, or any other kind or action, is 400 POINTER_INPUT. On an issue Legion runs, a pointer out of a requirement, criterion or check block - a requirement's constrained_by - by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT; a pointer out of a lens or a risk, a prevented_by at a check among them, is no such write. A refused write stores nothing, and the write appends no event. Answers 200 {node, kind, action, to, from}, each end with its address, kind, state and visibility, from null but for a retarget.",
	Handler:     (*server).writePointer,
	WriteClass:  scopeProject,
	ID:          &idRoute{Reason: "a node's store address under the issue, which FindNodeUnderIssue decodes and no prefix shortens"},
	Shape: routeShape{
		Status: http.StatusOK, Response: pointerWritten{}, Request: pointerInput{},
	},
})
