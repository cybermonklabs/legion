package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/me/documents/{id}/read",
	Auth:        authAny,
	Description: "The caller's last read version of a document {artifact_id, version, read_at}; version and read_at are null when they have read none.",
	Handler:     (*server).getDocumentRead,
	Successor:   successorIs(http.MethodGet, "/me/marks/{kind}/{id}", "kind=document"),
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: model.DocumentRead{}},
})
