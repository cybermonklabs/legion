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
	Description: "Rebuild a document's history into one fresh update, keeping its block ids, anchors and margin records as far as its history or its rows record them: a history ygo cannot decode, from its latest saved markdown or supplied {markdown}; a history that decodes but parks more than a room's document may, 4,096 structs or 4,096 delete ranges, which no room loads, from its own text, unsettled edits included, or supplied markdown, the fresh update holding nothing the history parked; a history so near the decode cap or the edit-scan total that a write it must still take is refused (a write of its whole text, near the decode cap), from its own text or supplied markdown, closing its room in this server first, whose connected browsers drop their unsent edits - a room this server holds decides that on its own; 409 DOCUMENT_LIVE for any other document live in this server, 409 DOCUMENT_LOADS for any other history that loads.",
	Handler:     (*server).rebuildArtifact,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: docs.RebuildReport{}, Request: rebuildArtifactInput{},
	},
})
