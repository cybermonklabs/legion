package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/artifacts/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Read one artifact, of any owner and any kind, as every list of artifacts answers it. " +
			artifactRowText + " " + artifactIncludeText + " 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.getArtifactV2,
		Resource: "artifacts", Kind: kindItem, Key: "{id}",
		Query:    artifactReadV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: artifactV2{}},
		IDs:      v2DocumentIDs(),
		Refusals: documentRefusals(undeclaredKeyRefusal, documentMissingRefusal),
	}
})
