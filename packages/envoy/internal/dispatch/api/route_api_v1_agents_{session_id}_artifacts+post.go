package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/agents/{session_id}/artifacts",
	Auth:        authAny,
	Description: "Upload a file or image to an agent's conversation (multipart, as an issue upload, its kind read from its bytes): ref_key agent/<session_id>/<slug>, addressed dispatch://agent/<session_id>/artifact/<slug>. It holds files and images only: a markdown document (JSON content or a text/markdown file) is 400 ARTIFACT_INPUT, since a document belongs to an issue or a project. No event is appended. A caller whose credential proves a session of its own uploads only into that session: any other session_id is 403 SESSION_SCOPE. A session_id that is empty or holds /, ?, #, whitespace, a control character or a character that ends a reference in text (<>\"'`[]|) is 400 INVALID_SESSION_ID on every agent artifact route.",
	Handler:     (*server).uploadAgentArtifact,
	Successor:   successorIs(http.MethodPost, "/agents/{session_id}/artifacts"),
	WriteClass:  scopeSession,
	Shape: routeShape{
		Status: http.StatusCreated, Response: artifactUploadResponse{},
		// The upload reads JSON or multipart as the issue and project forms do, but a
		// conversation holds files and images alone, and a JSON upload is always a markdown
		// document: 400 ARTIFACT_INPUT (agent_artifacts.go). No issue, so no advice.
		Request: jsonArtifactUpload{}, Multipart: true,
	},
})
