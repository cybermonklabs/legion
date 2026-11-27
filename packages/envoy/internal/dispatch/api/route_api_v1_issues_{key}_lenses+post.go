package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/lenses",
	Auth:        authAny,
	Description: "Invoke a lens on one node of the issue {on, kind, verdict?, findings?, triggered_by?, visibility?}: one analysis of one kind of the lens catalogue, logged under the node it looks at by its part_of tuple, with one has_kind tuple to its lens_kind. on is the dispatch:// address, or the store address /nodes/{id} takes, of a node the caller may see that stands under the issue: the issue itself, a document or one of its blocks, an ask, a comment or a message or one of their blocks, or a lens, a finding, a risk or a constraint of the issue; such a node that is gone is 409 NODE_GONE, naming it, since gone is final, and so is a stale one - a lens whose recorded document row no longer stands at its document's address, or a node under one - and any other is 404 NOT_FOUND. A lens on a document or on one of its blocks records, as its artifact_id, the id of the document's row the artifacts table holds as the write runs, and a document no row stands at by then is 409 NODE_GONE; once that row no longer stands at the address, the lens and everything under it read as gone. kind is the id of a lens_kind node (GET /api/v1/schema/blocks lists them under lens_kinds): none is 400 LENS_KIND_REQUIRED, and an id no lens_kind node holds 400 INVALID_POINTER. verdict is stored as the lens's verdict. Each of findings is one finding, a block node under the lens at a random block id, addressed dispatch://lens/<uuid>#b-<id>, ranked in the order written. triggered_by is the address of the lens of the issue whose finding asked for this one, a triggered_by tuple. visibility author keeps the lens and its findings to their author, and is 403 AUTHOR_NAMES_NOBODY on a credential that proves no author, the deployment's shared bearer. With no visibility the lens and its findings take the visibility of the node it is invoked on; visibility project on a node reserved for its author is 400 VISIBILITY_WIDER_THAN_PARENT, naming the lens and that node, and stores nothing. A refused write stores nothing, and no write appends an event or sends a delivery. Answers 201 with the lens - its address dispatch://lens/<uuid>, kind, on, verdict, triggered_by, author (the person or session its authored_by tuple names, the session the body named on the shared bearer, null where the write named only a workload), created_at, state and visibility - and its findings, each with its address, text, author, state and visibility.",
	Handler:     (*server).invokeLens,
	// The reasoning nodes a request writes under an issue - a lens, a finding of one, a risk, a
	// constraint - one pointer tuple out of a node of the issue, the state of a node of the issue,
	// written or settled, and the move of a node of the issue: each belongs to the project of the
	// issue its path names.
	WriteClass: scopeProject,
	// The reasoning nodes under an issue (reasoning_routes.go, constraint_routes.go and
	// pointer_routes.go). Each write answers the node it wrote, carrying its state and visibility,
	// so an author reads `author` on the node reserved for her in the answer to her own write.
	Shape: routeShape{
		Status: http.StatusCreated, Response: lensInvoked{}, Request: lensInvocationInput{},
	},
})
