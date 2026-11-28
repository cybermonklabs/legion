package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/projects/{key}/issues", Auth: authAny, Versions: onlyV2,
		Description: "List one project's issues, filtered: the rows GET /api/v2/issues?project={key} answers for the " +
			"same filters. " + issueListText + " 404 PROJECT_NOT_FOUND for a project that does not exist.",
		Handler:  s.listProjectIssuesV2,
		Resource: "issues", Kind: kindCollection, Key: "{key}",
		TimeFields:  v2IssueTimes,
		Query:       projectIssuesV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[model.IssueSummary]{}},
		List:        v2IssueList,
		Authorities: []authorityRule{issuePinnedAuthority},
		Refusals:    issueListRefusals(projectMissingRefusal),
	}
})
