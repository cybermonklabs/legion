package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/artifacts/{id}/versions", Auth: authAny, Versions: onlyV2,
		Description: "List an artifact's versions: each {number, named, summary, authors, created_at, size?, mime?, " +
			"sha256?, token?}, no markdown; token is a file's or a picture's version token, which an upload replacing " +
			"that version names as its precondition, and a document's version carries none. sort=number (the default, " +
			"oldest first) or -number. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}. GET " +
			"/api/v2/artifacts/{id}/versions/{number} reads one. 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.listArtifactVersionsV2,
		Resource: "versions", Kind: kindCollection, Key: "{number}",
		Query:    artifactVersionsV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[model.Version]{}},
		IDs:      v2DocumentIDs(),
		Refusals: listRefusals(documentMissingRefusal),
	}
})
