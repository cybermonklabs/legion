package api

import (
	"fmt"
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/artifacts",
	Auth:        authAny,
	Description: fmt.Sprintf("Upload an issue document or file (JSON or multipart). A text/markdown upload is a document; any other is an image only when its bytes are a PNG, JPEG, GIF or WebP, stored under the type its bytes are whatever was declared, and otherwise a file under its declared type. A name no artifact has yet is 400 INVALID_ARTIFACT when it, or the reference key <owner>/<slug> it gives the new artifact, is over %d bytes; an upload under an existing artifact's name is never refused for its length. A new version that is not a person's own and leaves out or retypes the block of an answered ask is 409 ANSWERED_ASK_KEPT, as POST /api/v1/artifacts/{id}/edits answers it, and writes no version. An upload under the name or slug of a retired document, which takes no new text, is 409 DOCUMENT_RETIRED and writes nothing. A new document whose name gives the slug of a decisions document is 409 DECISIONS_ARE_NODES, and so is an upload of that name once the issue's decisions document is retired, rather than DOCUMENT_RETIRED: a decision is a `decision` block in the spec. An upload of that name while the issue's decisions document an earlier release stored still stands is that document's next version. On an issue Legion runs, an upload of its spec or Verification document, a file under the name verification.md among them, by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT.", maxNewArtifactKeyBytes),
	Handler:     (*server).uploadArtifact,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: artifactUploadResponse{},
		Request: jsonArtifactUpload{}, Multipart: true, Advice: writeAdvice{},
	},
})
