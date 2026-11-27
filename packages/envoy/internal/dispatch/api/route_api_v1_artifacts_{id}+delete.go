package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/artifacts/{id}",
	Auth:        authAny,
	Description: "Remove a document of an issue that is still untouched: made by typing a title and never written in. It answers 204 and appends artifact.removed on the issue, and it leaves no row behind. Every lens invoked on the document, and all that stands under each at every depth - its findings, its risks and the lenses on those, those the caller may not see included - goes gone in the same transaction, so no read answers one of them live with nothing to stand on. A document that holds writing or a later version, an edit that has not settled, or a comment, ask, review, approval request, check result, read mark or link from anything in Dispatch is 409 ARTIFACT_NOT_UNTOUCHED, saying which of those it is; the issue's spec, a document that plays a role, a document of the issue's room and an artifact that is no document read the same refusal; and a project's document is 400 ARTIFACT_PROJECT_DOCUMENT, since a project document's events are its own and its removal event would need the row the removal deletes.",
	Handler:     (*server).removeArtifact,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusNoContent},
})
