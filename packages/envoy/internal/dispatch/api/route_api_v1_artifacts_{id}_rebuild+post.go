package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/rebuild",
	Auth:        authAny,
	Description: "Rebuild a document history ygo cannot load from its latest saved markdown; a live document is 409 DOCUMENT_LIVE and one that loads is 409 DOCUMENT_LOADS.",
	Handler:     (*server).rebuildArtifact,
	Successor:   successorIs(http.MethodPost, "/artifacts/{id}/rebuilds"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: docs.RebuildReport{}, Request: rebuildArtifactInput{},
	},
})
