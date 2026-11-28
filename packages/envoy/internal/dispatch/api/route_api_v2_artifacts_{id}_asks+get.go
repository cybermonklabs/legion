package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/artifacts/{id}/asks", Auth: authAny, Versions: onlyV2,
		Description: "List a project document's asks, oldest first, each as GET /api/v2/asks answers it. state=all " +
			"(the default), open, answered or resolved. " + askRowText + " " + includedPageLimitText(v2AskList, artifactAsksV2Query{}) +
			" {items, total, limit, offset}. An issue's document is 400 ARTIFACT_LINKED, its asks being GET " +
			"/api/v2/issues/{key}/asks, and a file of an agent's conversation 400 ARTIFACT_AGENT_OWNED; 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.listArtifactAsksV2,
		Resource: "asks", Kind: kindCollection, Key: "{id}",
		Query:       artifactAsksV2Query{},
		List:        v2AskList,
		Shape:       routeShape{Status: http.StatusOK, Response: page[askV2]{}},
		Authorities: []authorityRule{askMarkAuthority},
		IDs:         v2DocumentIDs(),
		Refusals:    listRefusals(projectDocumentRefusals(actingSessionMismatchRefusal)...),
	}
})
