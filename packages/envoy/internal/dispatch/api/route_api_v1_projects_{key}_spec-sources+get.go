package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/spec-sources",
	Auth:        authAny,
	Description: "A project's spec sources, the repositories whose approved spec it holds, by repository; [] when it names none, 404 for a project that does not exist. An issue whose repository is an enabled spec source of its project holds a change request, which a person's approval writes into that repository's approved spec.",
	Handler:     (*server).listProjectSpecSources,
	Shape:       routeShape{Status: http.StatusOK, Response: []model.SpecSource{}},
})
