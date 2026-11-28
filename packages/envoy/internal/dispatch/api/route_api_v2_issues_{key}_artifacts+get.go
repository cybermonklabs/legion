package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/artifacts", Auth: authAny, Versions: onlyV2,
		Description: "List an issue's documents, files and images, its spec among them, oldest first. " + artifactRowText +
			" " + artifactSlugText + " " + artifactIncludeText + " " + includedPageLimitText(v2ArtifactList, artifactListV2Query{}) +
			" {items, total, limit, offset}. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
		Handler:  s.listIssueArtifactsV2,
		Resource: "artifacts", Kind: kindCollection, Key: "{id}",
		Query:         artifactListV2Query{},
		List:          v2ArtifactList,
		Shape:         routeShape{Status: http.StatusOK, Response: page[artifactV2]{}},
		DocumentOwner: documentOwnerIssue,
		Refusals:      documentRefusals(listRefusals(issueMissingRefusal)...),
	}
})
