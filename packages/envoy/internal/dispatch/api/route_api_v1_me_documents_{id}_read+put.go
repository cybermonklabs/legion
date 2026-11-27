package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/me/documents/{id}/read",
	Auth:        authAny,
	Description: "Record the caller's last read version of a document {version}: the Spec tab marks each block that changed since it. Answers the stored read; a version that is missing, negative or above 2147483647 (the largest version number the store holds) is 400 INVALID_STATE.",
	Handler:     (*server).putDocumentRead,
	Successor:   successorIs(http.MethodPatch, "/me/marks/{kind}/{id}", "kind=document"),
	WriteClass:  scopeSession,
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.DocumentRead{}, Request: documentReadInput{},
	},
})
