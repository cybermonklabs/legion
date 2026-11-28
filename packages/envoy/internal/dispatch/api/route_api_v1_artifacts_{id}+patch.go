package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPatch,
	Pattern:     "/api/v1/artifacts/{id}",
	Auth:        authAny,
	Description: "Correct a document's title: {name}. It writes the name alone - the slug, the reference key it gives and the role the document plays do not move - and answers the document as it now is. The owner's name it already holds is 409 ARTIFACT_NAME_TAKEN, a name whose slug is one of the documents Dispatch makes itself, on a document that is not that one, 400 ARTIFACT_ROLE_NAME, the issue's spec 400 ARTIFACT_PRIMARY, an artifact that is no document 400 ARTIFACT_KIND_MISMATCH, and a name that is empty or too long 400 INVALID_ARTIFACT.",
	Handler:     (*server).renameArtifact,
	Successor:   successorIs(http.MethodPatch, "/artifacts/{id}"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.Artifact{}, Request: renameArtifactInput{},
	},
})
