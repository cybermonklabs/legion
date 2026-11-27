package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/comments", Auth: authAny, Versions: onlyV2,
		Description: "List an issue's comments, oldest first: its own, those anchored in its documents, every reply and " +
			"every ask thread's comment, each as GET /api/v2/comments/{id} answers its fields, without replies. " +
			"artifact=<document id> keeps the comments anchored in one document of the issue, which are its root comments " +
			"and suggestions alone, since a reply carries no anchor. limit 1 to 200 (default 50) and offset; {items, total, " +
			"limit, offset}. The answer carries an ETag, which moves for every write the list shows, a delivery attempt " +
			"that appends no event included, and for every page; If-None-Match answers 304. A comment's author carries " +
			"proven: true when the write's credential proved it (a browser's person, a broker-issued token's session), and " +
			"no proven when the request only named it. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
		Handler:  s.listIssueCommentsV2,
		Resource: "comments", Kind: kindCollection, Key: "{id}",
		Query: issueCommentsV2Query{},
		Shape: routeShape{
			Status: http.StatusOK, Response: page[shapedComment]{},
			Also: []routeAnswer{{Status: http.StatusNotModified}},
		},
		Refusals: listRefusals(issueMissingRefusal),
	}
})
