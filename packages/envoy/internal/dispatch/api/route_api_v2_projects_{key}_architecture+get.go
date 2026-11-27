package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/projects/{key}/architecture", Auth: authAny, Versions: onlyV2,
		Description: "A project's component tree with the work attached: {sources, totals, components, unassigned, " +
			"not_architectural, retired_links}, its sources' sync bookkeeping, each component with its source repository, " +
			"per-component counts and issue rows, and the issues no component counts - unassigned, declared not " +
			"architectural, or linked only to retired components. With Dispatch-Trees: 1 each component's prose carries " +
			"its display tree. A read-only token may call it. 404 SOURCE_NOT_FOUND for a project with no architecture " +
			"source, 404 PROJECT_NOT_FOUND for a project that does not exist.",
		Handler:  s.getArchitectureTreeV2,
		Resource: "architecture", Kind: kindReport,
		Shape:    routeShape{Status: http.StatusOK, Response: architectureTreeRead{}},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectMissingRefusal, projectHasNoSourceRefusal},
	}
})
