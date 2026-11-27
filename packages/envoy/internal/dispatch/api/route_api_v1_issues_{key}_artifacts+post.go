package api

import (
	"fmt"
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:             http.MethodPost,
	Pattern:            "/api/v1/issues/{key}/artifacts",
	Auth:               authAny,
	Description:        fmt.Sprintf("Upload an issue document or file (JSON or multipart). A text/markdown upload is a document; any other is an image only when its bytes are a PNG, JPEG, GIF or WebP, stored under the type its bytes are whatever was declared, and otherwise a file under its declared type. A name no artifact has yet is 400 INVALID_ARTIFACT when it, or the reference key <owner>/<slug> it gives the new artifact, is over %d bytes; an upload under an existing artifact's name is never refused for its length. On an issue Legion runs, an upload of its spec or Verification document, a file under the name verification.md among them, by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT.", maxNewArtifactKeyBytes),
	Handler:            (*server).uploadArtifact,
	BodyLimit:          maxUploadRequestBytes,
	RefusesClosedIssue: true,
	WriteClass:         scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: artifactUploadResponse{},
		Request: jsonArtifactUpload{}, Multipart: true, Advice: writeAdvice{},
	},
})
