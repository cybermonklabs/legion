package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/asks", Auth: authAny, Versions: onlyV2,
		Description: "List an issue's asks, oldest first, each as GET /api/v2/asks answers it. state=all (the default), " +
			"open, answered or resolved. " + askRowText + " " + includedPageLimitText(v2AskList, issueAsksV2Query{}) +
			" {items, total, limit, offset}. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
		Handler:  s.listIssueAsksV2,
		Resource: "asks", Kind: kindCollection, Key: "{id}",
		Query:       issueAsksV2Query{},
		List:        v2AskList,
		Shape:       routeShape{Status: http.StatusOK, Response: page[askV2]{}},
		Authorities: []authorityRule{askMarkAuthority},
		Refusals:    listRefusals(issueMissingRefusal, actingSessionMismatchRefusal),
	}
})
