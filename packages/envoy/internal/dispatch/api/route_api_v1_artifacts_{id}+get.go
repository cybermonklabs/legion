package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}",
	Auth:        authAny,
	Description: "Read a document's metadata and current version, with the room the document is edited in (room: its issue room's tree artifact, or its own id) and the fragment of that room that holds it (fragment). Each version of a document in versions carries digest, what an approval of that version covers, as GET /api/v1/artifacts/{id}/versions/{number} answers it; no other read of a list of versions carries it. An issue's tree artifact (kind tree) reads as a document does.",
	Handler:     (*server).getArtifact,
	// The {id} is an artifact, looked for among every document and file, as a whole artifact id
	// is. The read goes through loadArtifact, which answers ARTIFACT_NOT_FOUND.
	ID:    &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Read:  readEntry{Writes: readWritesNothing},
	Shape: routeShape{Status: http.StatusOK, Response: artifactRead{}},
})
