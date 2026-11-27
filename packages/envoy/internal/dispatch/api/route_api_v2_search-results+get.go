package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/search-results", Auth: authAny, Versions: onlyV2,
		Description: searchResultsV2Description,
		Handler:     s.searchResultsV2,
		Resource:    "search-results", Kind: kindCollection,
		TimeFields: []string{"written_at"}, CrossProject: true,
		Query: searchResultsV2Query{},
		Shape: routeShape{Status: http.StatusOK, Response: searchResultsV2{}},
		List:  listBounds{Default: searchDefaultLimit, Max: searchMaxLimit},
		Refusals: listRefusals(queryProjectMalformedRefusal, numberProjectMalformedRefusal, issueStatusInvalidRefusal,
			searchAuthorInvalidRefusal, valuePastBoundRefusal),
	}
})
