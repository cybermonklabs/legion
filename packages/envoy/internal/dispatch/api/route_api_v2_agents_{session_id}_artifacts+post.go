package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/agents/{session_id}/artifacts", Auth: authAny, Versions: onlyV2,
		Description: "Upload a file or image to an agent's conversation: ref_key agent/<session_id>/<slug>, addressed " +
			"dispatch://agent/<session_id>/artifact/<slug>. " + artifactUploadText + " A conversation holds files and " +
			"images only. " + fileUploadPreconditionText + " " +
			artifactUploadAnswerText + " " +
			artifactUploadRefusals(", a markdown document, as JSON content or a text/markdown file") +
			" No event is appended. A caller whose credential proves a session of its own uploads only into that " +
			"session: any other session_id is 403 SESSION_SCOPE; a session_id that is no session id is 400 " +
			"INVALID_SESSION_ID.",
		Handler:  s.uploadAgentArtifactV2,
		Resource: "artifacts", Kind: kindCollection, Key: "{id}",
		Body: requestBody{Kind: bodyUpload, Type: jsonArtifactUpload{}},
		Shape: routeShape{
			Status: http.StatusCreated, Response: artifactUploadV2{},
			Refusals: uploadPreconditionRefusalAnswers(),
		},
		Scope: scopeSession, SessionPath: "session_id",
		Refusals: uploadBodyRefusals(sessionIDMalformedRefusal, documentPreconditionRefusal),
	}
})
