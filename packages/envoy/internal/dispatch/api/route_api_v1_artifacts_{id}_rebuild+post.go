package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/rebuild",
	Auth:        authAny,
	Description: "Rebuild a document history ygo cannot load from its latest saved markdown; a live document is 409 DOCUMENT_LIVE and one that loads is 409 DOCUMENT_LOADS, whether or not it is retired. A retired document whose history does not load, which takes no new text, is 409 DOCUMENT_RETIRED and writes nothing; it is 409 DECISIONS_ARE_NODES instead where it is an issue's decisions document: a decision is a `decision` block in the spec.",
	Handler:     (*server).rebuildArtifact,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: docs.RebuildReport{}, Request: rebuildArtifactInput{},
	},
})
