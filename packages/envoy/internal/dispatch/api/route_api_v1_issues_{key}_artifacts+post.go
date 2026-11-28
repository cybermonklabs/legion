package api

import (
	"fmt"
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/artifacts",
	Auth:        authAny,
	Description: fmt.Sprintf("Upload an issue document or file (JSON or multipart). A text/markdown upload is a document; any other is an image only when its bytes are a PNG, JPEG, GIF or WebP, stored under the type its bytes are whatever was declared, and otherwise a file under its declared type. A name no artifact has yet is 400 INVALID_ARTIFACT when it, or the reference key <owner>/<slug> it gives the new artifact, is over %d bytes; an upload under an existing artifact's name is never refused for its length. On an issue Legion runs, an upload of its spec or Verification document, a file under the name verification.md among them, by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT. The command bar's in-process caller makes an upload conditional by a condition on the request's context, never a body member: its undo's write back names the document its read found, else 409 DOCUMENT_CHANGED with nothing written (a name no document has is refused the same), beside the token that read returned as the body's precondition, else 409 PRECONDITION_FAILED, and its upload is a create alone - with force too - a name the owner already holds when the upload takes the owner's lock being 409 DOCUMENT_EXISTS with nothing written, unless the write's read before found the document or file of its name, when the upload expects that artifact at the latest version the read found, another version written since (or the artifact gone) being 409 DOCUMENT_CHANGED with nothing written; a write back carrying none of these and no precondition is 500 GOING_CONDITION_MISSING, nothing written. The answer is {artifact, version, previous_version}: previous_version is the number of the newest version the upload wrote its next version over, as the upload found it under the owner's lock (0 for an artifact that held none yet), and null where the upload made the artifact.", maxNewArtifactKeyBytes),
	Handler:     (*server).uploadArtifact,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: artifactUploadResponse{},
		Request: jsonArtifactUpload{}, Multipart: true, Advice: writeAdvice{},
	},
})
