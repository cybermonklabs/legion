package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/artifacts/{id}/references", Auth: authAny, Versions: onlyV2,
		Description: "The mentions of a document, by kind and id. direction=in (the default) lists the items that cite " +
			"it, one row per citing item {kind, id, issue_key?, project?, excerpt?, ref_key?}, each ? absent where the " +
			"item has none (an empty excerpt among them), an ask citing it from an option or an answer among them; " +
			"direction=out the items it cites {kind, to_id, artifact?}, artifact the cited " +
			"document's row where the item is one. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}. " +
			"GET /api/v2/references reads every edge kind of one node, a page by position. 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.listArtifactReferencesV2,
		Resource: "references", Kind: kindCollection,
		Query:    artifactReferencesV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[artifactReferenceV2]{}},
		IDs:      v2DocumentIDs(),
		Refusals: listRefusals(documentMissingRefusal),
	}
})
