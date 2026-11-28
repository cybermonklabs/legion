package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/artifacts/{id}/reviews", Auth: authAny, Versions: onlyV2,
		Description: "List a document's reviews, newest first in the order their events committed: each {id, " +
			"artifact_id, version, state, kind (person or plan_review), actor, reason, ask_id, created_at, content_sha256, " +
			"content_sha256_source}; a file or an image has none. limit 1 to 200 (default 50) and offset; {items, total, " +
			"limit, offset}. 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.listArtifactReviewsV2,
		Resource: "reviews", Kind: kindCollection, Key: "{id}",
		Query:    artifactReviewsV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[artifactReviewRead]{}},
		IDs:      v2DocumentIDs(),
		Refusals: listRefusals(documentMissingRefusal),
	}
})
