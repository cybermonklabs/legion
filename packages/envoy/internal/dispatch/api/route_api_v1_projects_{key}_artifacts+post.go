package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/artifacts",
	Auth:        authAny,
	Description: "Upload a project document or file (JSON or multipart); a file's kind is read from its bytes, and a new artifact's name held to its bound, as an issue upload's are.",
	Handler:     (*server).uploadProjectArtifact,
	BodyLimit:   maxUploadRequestBytes,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: artifactUploadResponse{},
		Request: jsonArtifactUpload{}, Multipart: true,
		// A project document has no issue state to report, so its write advice is the
		// document half alone (artifacts.go, the withRawAdvice arm).
		Advice: documentWriteAdvice{},
	},
})
