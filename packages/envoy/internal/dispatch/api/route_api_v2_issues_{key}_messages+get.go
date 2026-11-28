package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/messages", Auth: authAny, Versions: onlyV2,
		Description: "List an issue's message threads, oldest first: every root message of the issue, a targeted one " +
			"listed like any other. " + messageThreadText + " limit 1 to 200 (default 50) and offset; {items, total, " +
			"limit, offset}. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
		Handler:  s.listIssueMessagesV2,
		Resource: "messages", Kind: kindCollection, Key: "{id}",
		Query:    issueMessagesV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[messageThreadV2]{}},
		Refusals: listRefusals(issueMissingRefusal),
	}
})
