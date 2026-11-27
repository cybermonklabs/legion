package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/spec/{owner}/{repo}/nodes/{id}",
	Auth:        authAny,
	Description: "One node of the approved spec of a spec source of a project - a statement, check, component or interface - with each approved text, oldest first, printed as The Spec's files hold it, and the provenance of each: the act (approval, brought_back or import), the issue, the documents and versions the person's approval named with their digests, the approval request, the review row and the person; and each removal. 404 SPEC_NODE_NOT_FOUND when the approved spec never held the id.",
	Handler:     (*server).getSpecNode,
	// A node of a repository's approved spec, whose {id} is the node's own id in that repository:
	// a statement's, check's, component's or interface's slug, which no prefix shortens. The route
	// answers SPEC_NODE_NOT_FOUND wherever the approved spec holds no node of that id.
	ID:    &idRoute{Reason: "a node id of the repository's approved spec, a slug of its own and no uuid at all"},
	Shape: routeShape{Status: http.StatusOK, Response: model.SpecNodeRead{}},
})
