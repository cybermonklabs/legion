package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/artifacts",
	Auth:        authAny,
	Description: "Upload a project document or file (JSON or multipart); a file's kind is read from its bytes, and a new artifact's name held to its bound, as an issue upload's are. A new version that is not a person's own and leaves out or retypes the block of an answered ask is 409 ANSWERED_ASK_KEPT, as POST /api/v1/artifacts/{id}/edits answers it, and writes no version. An upload under the name or slug of a retired document, which takes no new text, is 409 DOCUMENT_RETIRED and writes nothing.",
	Handler:     (*server).uploadProjectArtifact,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: artifactUploadResponse{},
		Request: jsonArtifactUpload{}, Multipart: true,
		// A project document has no issue state to report, so its write advice is the
		// document half alone (artifacts.go, the withRawAdvice arm).
		Advice: documentWriteAdvice{},
	},
})
