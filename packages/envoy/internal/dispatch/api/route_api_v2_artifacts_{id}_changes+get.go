package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/artifacts/{id}/changes", Auth: authAny, Versions: onlyV2,
		Description: "What changed between two stored versions of one document, version_from=<n>&version_to=<m>: " +
			artifactChangesText + " version_from and version_to are both required, each a version number from 1 to " +
			"2147483647 (the largest version number the store holds), and version_from below version_to (400 " +
			"INVALID_QUERY); a version the document does not have is 404 NOT_FOUND, a file or an image 400 NOT_DOCUMENT, " +
			"and 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.getArtifactChangesV2,
		Resource: "changes", Kind: kindReport,
		Query: artifactChangesV2Query{},
		Shape: routeShape{Status: http.StatusOK, Response: artifactChanges{}},
		IDs:   v2DocumentIDs(),
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, documentMissingRefusal, artifactNotDocumentRefusal,
			{Condition: apicode.QueryVersionMissing, Code: apicode.NotFound},
		},
	}
})
