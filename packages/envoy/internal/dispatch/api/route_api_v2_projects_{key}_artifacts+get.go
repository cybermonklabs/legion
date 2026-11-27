package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/projects/{key}/artifacts", Auth: authAny, Versions: onlyV2,
		Description: "List a project's documents, files and images, oldest first: its own and every one of its issues' " +
			"but each issue's spec. issue=none keeps the project's own, which no issue holds, and issue=<key> one issue's. " +
			artifactRowText + " " + artifactSlugText + " " + artifactIncludeText + " " +
			includedPageLimitText(v2ArtifactList, projectArtifactsV2Query{}) + " {items, total, limit, offset}. 404 " +
			"PROJECT_NOT_FOUND for a project that does not exist.",
		Handler:  s.listProjectArtifactsV2,
		Resource: "artifacts", Kind: kindCollection, Key: "{id}",
		Query:         projectArtifactsV2Query{},
		List:          v2ArtifactList,
		Shape:         routeShape{Status: http.StatusOK, Response: page[artifactV2]{}},
		DocumentOwner: documentOwnerProject,
		Refusals:      documentRefusals(listRefusals(projectMissingRefusal)...),
	}
})
