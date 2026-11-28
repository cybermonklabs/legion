package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/artifacts/{id}/comments", Auth: authAny, Versions: onlyV2,
		Description: "List a project document's comments, every comment and reply, oldest first, each as the issue " +
			"comment list answers it. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}. The answer " +
			"carries an ETag, which moves for every write the list shows, a delivery attempt that appends no event " +
			"included, and for every page; If-None-Match answers 304. An issue's document is 400 ARTIFACT_LINKED, its " +
			"comments being its issue's, and a file of an agent's conversation 400 ARTIFACT_AGENT_OWNED; 404 " +
			"ARTIFACT_NOT_FOUND.",
		Handler:  s.listArtifactCommentsV2,
		Resource: "comments", Kind: kindCollection, Key: "{id}",
		Query: artifactCommentsV2Query{},
		Shape: routeShape{
			Status: http.StatusOK, Response: page[shapedComment]{},
			Also: []routeAnswer{{Status: http.StatusNotModified}},
		},
		IDs:      v2DocumentIDs(),
		Refusals: listRefusals(projectDocumentRefusals()...),
	}
})
